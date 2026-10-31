package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"uws/pkg/browser/modelbridge"
	"uws/pkg/contract"
)

// DefaultVerifyTimeout 是单次自证回放的等待上限（真实站点一轮回答约 10–40s）。
const DefaultVerifyTimeout = 90 * time.Second

// VerifyResult 是一次自证回放的实测读数，也是激活凭证（写入 overlay 产物）的内容。
type VerifyResult struct {
	Site       string            `json:"site"`
	Status     RecipeStatus      `json:"status"`
	HTTPStatus int               `json:"http_status"`
	Answer     string            `json:"answer"`
	Bytes      int               `json:"bytes"`
	ElapsedMs  int64             `json:"elapsed_ms"`
	Slots      map[string]string `json:"slots,omitempty"` // 打了 echo 标记的槽位实测值（如首轮新建的会话 ID）
}

// DriftReport 记录 Recipe 验证失败或线上直发异常时的漂移现场诊断。
type DriftReport struct {
	Site      string    `json:"site"`
	Timestamp time.Time `json:"timestamp"`
	Reason    string    `json:"reason"`
	Error     string    `json:"error"`
	Status    string    `json:"status"` // "not_dispatched", "http_error", "network_error", "queue_lost"
}

// Verifier 负责 Recipe 落地前的自证门禁回放与漂移自愈状态管理。
type Verifier struct {
	mu           sync.RWMutex
	driftReports []DriftReport
	Timeout      time.Duration
}

// NewVerifier 创建自证门禁与漂移管理器。
func NewVerifier() *Verifier {
	return &Verifier{
		driftReports: make([]DriftReport, 0, 32),
		Timeout:      DefaultVerifyTimeout,
	}
}

var (
	defaultVerifierOnce sync.Once
	defaultVerifier     *Verifier
)

// DefaultVerifier 返回全局自证器单例（漂移历史跨请求累计）。
func DefaultVerifier() *Verifier {
	defaultVerifierOnce.Do(func() { defaultVerifier = NewVerifier() })
	return defaultVerifier
}

// VerifyRecipe 执行自证门禁：在目标页面真实上下文发起测试直发，
// **只有 HTTP 200 且解出非空回答文本**才把 Recipe 标记为 ACTIVE（契约 §13 激活门禁）。
func (v *Verifier) VerifyRecipe(ctx context.Context, eval contract.DirectScriptEvaluator, r *Recipe, testPrompt string) (VerifyResult, error) {
	began := time.Now()
	res := VerifyResult{Status: StatusDrifted}
	if r == nil {
		return res, errors.New("verifier: recipe 不能为空")
	}
	res.Site = r.Site
	if testPrompt == "" {
		testPrompt = "2+3=? 只回一个数字。"
	}

	queueKey := fmt.Sprintf("__uws_recipe_verify_%d", time.Now().UnixNano())
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = eval.EvaluateScript(cleanupCtx, fmt.Sprintf(`(() => {
			const q = window[%q];
			if (q) {
				q.controller?.abort();
				q.reader?.cancel().catch(() => {});
				delete window[%q];
			}
		})()`, queueKey, queueKey))
	}
	defer cleanup()

	script, err := BuildFetchScript(r, testPrompt, "", queueKey)
	if err != nil {
		v.RecordDrift(r.Site, "build_script_failed", err.Error(), "internal_error")
		r.Status = StatusDrifted
		return res, fmt.Errorf("verifier: 脚本构造失败: %w", err)
	}

	evalRes, err := eval.EvaluateScript(ctx, script)
	if err != nil {
		v.RecordDrift(r.Site, "eval_failed", err.Error(), "eval_error")
		r.Status = StatusDrifted
		return res, fmt.Errorf("verifier: 页面脚本执行失败: %w", err)
	}

	var hello struct {
		Status     string            `json:"status"`
		Error      string            `json:"error"`
		OK         bool              `json:"ok"`
		HTTPStatus int               `json:"httpStatus"`
		Slots      map[string]string `json:"slots"`
	}
	if err := json.Unmarshal(evalRes, &hello); err != nil {
		v.RecordDrift(r.Site, "invalid_start_json", string(evalRes), "eval_error")
		r.Status = StatusDrifted
		return res, fmt.Errorf("verifier: 无法解析自证启动响应: %w", err)
	}
	res.HTTPStatus = hello.HTTPStatus
	res.Slots = hello.Slots

	if hello.Status != "streaming_started" || !hello.OK {
		errMsg := hello.Error
		if errMsg == "" {
			errMsg = fmt.Sprintf("status=%s http=%d", hello.Status, hello.HTTPStatus)
		}
		v.RecordDrift(r.Site, "start_unhealthy", errMsg, hello.Status)
		r.Status = StatusDrifted
		return res, fmt.Errorf("verifier: 自证未通过 (%s): %s", hello.Status, errMsg)
	}

	// 轮询抽回增量，喂给 modelbridge 解码器；判据是**解出非空回答文本**而不是「有 chunk」。
	pollScript := fmt.Sprintf(`(() => {
		const q = window[%q];
		if (!q) return { chunks: [], done: true, error: "queue_lost" };
		const res = { chunks: q.chunks.slice(), done: q.done, error: q.error };
		q.chunks.length = 0;
		return res;
	})()`, queueKey)

	wait := v.Timeout
	if wait <= 0 {
		wait = DefaultVerifyTimeout
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	sink := modelbridge.NewSink(1)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			v.RecordDrift(r.Site, "context_timeout", ctx.Err().Error(), "timeout")
			r.Status = StatusDrifted
			return res, ctx.Err()
		}
		if time.Now().After(deadline) {
			v.RecordDrift(r.Site, "verify_timeout", fmt.Sprintf("超时未解出回答文本（已收 %d 字节）", res.Bytes), "timeout")
			r.Status = StatusDrifted
			return res, fmt.Errorf("verifier: 自证超时未解出回答文本（已收 %d 字节）", res.Bytes)
		}
		<-ticker.C
		resBytes, err := eval.EvaluateScript(ctx, pollScript)
		if err != nil {
			continue
		}
		var pollRes struct {
			Chunks []string `json:"chunks"`
			Done   bool     `json:"done"`
			Error  string   `json:"error"`
		}
		if err := json.Unmarshal(resBytes, &pollRes); err != nil {
			continue
		}
		if pollRes.Error != "" {
			v.RecordDrift(r.Site, "stream_error", pollRes.Error, "stream_error")
			r.Status = StatusDrifted
			return res, fmt.Errorf("verifier: 流式抽回报错: %s", pollRes.Error)
		}
		for _, raw := range pollRes.Chunks {
			res.Bytes += len(raw)
			if IsPollResponse(r) {
				// 正文走「轮询会话对象」的站点（ChatGPT）：主流只是前奏，
				// 页内执行器把轮询到的正文以单条 JSON 行回传。
				if perr, isErr := PollErrorFromChunk(raw); isErr {
					v.RecordDrift(r.Site, "answer_poll_failed", perr, "answer_poll_failed")
					r.Status = StatusDrifted
					return res, fmt.Errorf("verifier: 正文轮询失败: %s", perr)
				}
				if answer, ok := PollAnswerFromChunk(raw); ok {
					res.Answer = answer
					res.Status = StatusActive
					res.ElapsedMs = time.Since(began).Milliseconds()
					r.Status = StatusActive
					return res, nil
				}
				continue
			}
			sink.Feed([]byte(raw))
		}
		if answer := strings.TrimSpace(sink.Snapshot().Content); answer != "" {
			res.Answer = answer
			res.Status = StatusActive
			res.ElapsedMs = time.Since(began).Milliseconds()
			r.Status = StatusActive
			return res, nil
		}
		if pollRes.Done {
			v.RecordDrift(r.Site, "no_answer_text",
				fmt.Sprintf("流已结束但未解出回答文本（已收 %d 字节）", res.Bytes), "empty_answer")
			r.Status = StatusDrifted
			return res, fmt.Errorf("verifier: 流已结束但未解出回答文本（已收 %d 字节）", res.Bytes)
		}
	}
}

// RecordDrift 记录一次漂移事件。
func (v *Verifier) RecordDrift(site, reason, errText, status string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	report := DriftReport{
		Site:      site,
		Timestamp: time.Now(),
		Reason:    reason,
		Error:     errText,
		Status:    status,
	}
	v.driftReports = append(v.driftReports, report)
	if len(v.driftReports) > 50 {
		v.driftReports = v.driftReports[len(v.driftReports)-50:]
	}
}

// RecentDrifts 获取最近的漂移报告列表。
func (v *Verifier) RecentDrifts() []DriftReport {
	v.mu.RLock()
	defer v.mu.RUnlock()

	out := make([]DriftReport, len(v.driftReports))
	copy(out, v.driftReports)
	return out
}
