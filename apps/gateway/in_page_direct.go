// Package gateway 中的页内原生直发传输（In-Page Direct Transport）。
//
// 架构依据（2026-09-02 R1 与 2026-09-14 战略校准）：
// 告别依赖 DOM 选择器/点击模拟的脆性交互。利用已登录浏览器的合法安全上下文
// （真实 TLS 协商、Cookie、JWT、Origin、Referer、Sec-Fetch-Site），
// 通过 CDP Runtime.evaluate 在目标站点的 window 上下文内直接发起 fetch 请求，
// 并挂载 ReadableStream 监听 SSE 流式响应。
// 优势：
// 1. 免疫前端 UI/CSS 类名重构；
// 2. 毫秒级流式响应，零 DOM 渲染与布局开销；
// 3. 避免前台焦点抢占与 Chrome 后台节流。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"uws/apps/gateway/recipe"
	"uws/pkg/contract"
)

// DirectTransportRequest 封装页内直发请求参数。
type DirectTransportRequest struct {
	Site      string            // deepseek, gemini, chatgpt, claude 等
	Endpoint  string            // 相对路径或绝对 API 路径
	Method    string            // POST / GET
	Headers   map[string]string // 额外自定义 Header（如 Authorization 或 Content-Type）
	Payload   any               // 请求体对象（将序列化为 JSON）
	TimeoutMs int64             // 毫秒超时
	Recipe    *recipe.Recipe    // 声明式 Recipe（若非空，由 Recipe 执行器驱动）
	Prompt    string            // 用户提示词（用于 Recipe 渲染）
	Model     string            // 模型参数
}

// DirectStreamChunk 是从页内流式抽回的增量数据块。
type DirectStreamChunk struct {
	Index int    `json:"index"`
	Delta string `json:"delta"`
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
	// Slots 是页内执行器在启动响应里回显的槽位值（recipe `echo: true`；契约 §9 取值来源③）。
	// 只在流启动时随首个数据块出现一次，此后为空——它是「本轮请求体实际写进去的值」的原文。
	Slots map[string]string `json:"slots,omitempty"`
}

// CanDirectTransport 判定指定站点是否支持页内直发传输。
func CanDirectTransport(site string) bool {
	site = strings.ToLower(strings.TrimSpace(site))
	r, ok := recipe.DefaultRegistry().Get(site)
	return ok && r.Status == recipe.StatusActive
}

// BuildDirectTransportRequest 依据站点与用户输入构造页内直发请求。
func BuildDirectTransportRequest(site, content, model string) (DirectTransportRequest, bool) {
	site = strings.ToLower(strings.TrimSpace(site))
	if r, ok := recipe.DefaultRegistry().Get(site); ok && r.Status == recipe.StatusActive {
		// A recipe may bypass physical UI interaction only after verification.
		// Non-active recipes fail closed; legacy per-site request builders must
		// not bypass this lifecycle gate.
		headers := make(map[string]string)
		for k, v := range r.Request.Headers {
			headers[k] = v
		}
		return DirectTransportRequest{
			Site:      site,
			Endpoint:  r.Request.URLTemplate,
			Method:    r.Request.Method,
			Headers:   headers,
			TimeoutMs: r.Request.TimeoutMs,
			Recipe:    r,
			Prompt:    content,
			Model:     model,
		}, true
	}

	return DirectTransportRequest{}, false
}

// BuildInPageFetchScript 生成在页面上下文内执行的 JavaScript 异步自执行闭包。
// 它通过 window.fetch 发起请求，并通过挂载在 window.__uws_stream_<reqId> 上的队列流式传输。
func BuildInPageFetchScript(req DirectTransportRequest, streamQueueKey string) (string, error) {
	if req.Recipe != nil {
		return recipe.BuildFetchScript(req.Recipe, req.Prompt, req.Model, streamQueueKey)
	}

	bodyBytes, err := json.Marshal(req.Payload)
	if err != nil {
		return "", fmt.Errorf("in_page_direct: 无法序列化请求体: %w", err)
	}

	headersBytes, err := json.Marshal(req.Headers)
	if err != nil {
		return "", fmt.Errorf("in_page_direct: 无法序列化请求头: %w", err)
	}

	// 注入原生 JS 异步闭包：
	// - 检查并调用 window.fetch
	// - 捕获 response.body.getReader()
	// - 将流式 chunk 放入 window[streamQueueKey]
	script := fmt.Sprintf(`(async () => {
		const queueKey = %q;
		const queue = window[queueKey] = { chunks: [], done: false, error: null };
		
        let dispatched = false;
        const controller = new AbortController();
        queue.controller = controller;
		try {
			const headers = %s;
			headers["Content-Type"] = headers["Content-Type"] || "application/json";
			
            dispatched = true;
			const resp = await window.fetch(%q, {
				method: %q,
                signal: controller.signal,
				headers: headers,
				body: %s
			});
			
			if (!resp.ok) {
				const text = await resp.text().catch(() => "");
				queue.error = "HTTP " + resp.status + ": " + text;
				queue.done = true;
				return { status: "error", httpStatus: resp.status };
			}
			
			const reader = resp.body.getReader();
            queue.reader = reader;
			const decoder = new TextDecoder("utf-8");
			
			(async () => {
				try {
					while (true) {
						const { value, done } = await reader.read();
						if (done) {
							queue.done = true;
							break;
						}
						const text = decoder.decode(value, { stream: true });
						queue.chunks.push(text);
					}
				} catch (err) {
					queue.error = String(err);
					queue.done = true;
				}
			})();
			
			return { status: "streaming_started", ok: true };
		} catch (err) {
			queue.error = String(err);
			queue.done = true;
			return { status: dispatched ? "network_error" : "not_dispatched", error: String(err) };
		}
	})()`, streamQueueKey, string(headersBytes), req.Endpoint, req.Method, string(bodyBytes))

	return script, nil
}

var errDirectNotDispatched = errors.New("in_page_direct_not_dispatched")

// ExecuteInPageDirectStream preserves both completion and failure, never just EOF.
func ExecuteInPageDirectStream(ctx context.Context, eval contract.DirectScriptEvaluator, req DirectTransportRequest) (<-chan DirectStreamChunk, error) {
	queueKey := fmt.Sprintf("__uws_stream_req_%d", time.Now().UnixNano())
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = eval.EvaluateScript(cleanupCtx, fmt.Sprintf(`(() => {const q=window[%q]; if(q){q.controller?.abort();q.reader?.cancel().catch(()=>{});delete window[%q];}})()`, queueKey, queueKey))
	}
	script, err := BuildInPageFetchScript(req, queueKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errDirectNotDispatched, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	initRes, err := eval.EvaluateScript(ctx, script)
	if err != nil {
		cleanup()
		return nil, submissionError(fmt.Errorf("in_page_direct: evaluation failed: %w", err))
	}
	var start struct {
		Status string            `json:"status"`
		Error  string            `json:"error"`
		OK     bool              `json:"ok"`
		Slots  map[string]string `json:"slots"`
	}
	if err := json.Unmarshal(initRes, &start); err != nil {
		cleanup()
		return nil, submissionError(fmt.Errorf("in_page_direct: invalid start: %w", err))
	}
	if start.Status == "not_dispatched" {
		cleanup()
		return nil, fmt.Errorf("%w: %s", errDirectNotDispatched, start.Error)
	}
	if start.Status != "streaming_started" || !start.OK {
		cleanup()
		return nil, submissionError(fmt.Errorf("in_page_direct: start failed: %s %s", start.Status, start.Error))
	}
	out := make(chan DirectStreamChunk, 64)
	// 槽位回显先于任何增量下发：调用方即使中途失败也能拿到本轮实际使用的会话槽位值。
	if len(start.Slots) > 0 {
		out <- DirectStreamChunk{Slots: start.Slots}
	}
	go func() {
		defer close(out)
		defer cleanup()
		pull := fmt.Sprintf(`(() => {
   const q=window[%q];
   if(!q)return {chunks:[],done:true,error:"queue_lost"};
   const res={chunks:q.chunks.slice(),done:q.done,error:q.error};
   q.chunks.length=0;
   return res;
  })()`, queueKey)
		send := func(chunk DirectStreamChunk) bool {
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		fail := func(err error) { send(DirectStreamChunk{Error: err.Error()}) }
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			raw, err := eval.EvaluateScript(ctx, pull)
			if err != nil {
				fail(err)
				return
			}
			var result struct {
				Chunks []string `json:"chunks"`
				Done   *bool    `json:"done"`
				Error  string   `json:"error"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				fail(fmt.Errorf("invalid stream result: %w", err))
				return
			}
			if result.Done == nil || result.Chunks == nil {
				fail(errors.New("invalid stream shape"))
				return
			}
			if result.Error != "" {
				fail(errors.New(result.Error))
				return
			}
			for _, delta := range result.Chunks {
				if delta != "" && !send(DirectStreamChunk{Delta: delta}) {
					return
				}
			}
			if *result.Done {
				send(DirectStreamChunk{Done: true})
				return
			}
		}
	}()
	return out, nil
}
