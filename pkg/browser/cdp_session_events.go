package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
	"uws/pkg/browser/modelbridge"
)

// subscribe 订阅页面/网络事件。
func (s *cdpSession) subscribe(ctx context.Context) {
	s.client.On(s.sessionID, "Page.navigatedWithinDocument", func(ev cdpEvent) {
		s.mu.Lock()
		s.pushState = true
		s.mu.Unlock()
	})
	s.client.On(s.sessionID, "Page.loadFailed", func(ev cdpEvent) {
		var p struct {
			FrameID   string `json:"frameId"`
			ErrorText string `json:"errorText"`
			Canceled  bool   `json:"canceled"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.Canceled || p.ErrorText == "" {
			return
		}
		if strings.Contains(p.ErrorText, "ERR_ABORTED") {
			return // 被新导航/用户打断，非真实失败
		}
		s.mu.Lock()
		if s.navFrameID != "" && p.FrameID == s.navFrameID {
			s.navErr = p.ErrorText
		}
		s.mu.Unlock()
	})
	s.client.On(s.sessionID, "Page.frameNavigated", func(ev cdpEvent) {
		var p struct {
			Frame struct {
				ID          string `json:"id"`
				ParentID    string `json:"parentId"`
				URL         string `json:"url"`
				Unreachable string `json:"unreachableUrl"`
			} `json:"frame"`
			Type string `json:"type"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.Frame.ParentID != "" {
			return // 仅主框架
		}
		if p.Type != "" && p.Type != "Navigation" {
			return
		}
		// 连接拒绝/DNS/TLS 失败时 headless=new 不保证发 Page.loadFailed，
		// 而是导航到 chrome-error:// 错误页（实测 151）；unreachableUrl 带原目标。
		if strings.HasPrefix(p.Frame.URL, "chrome-error://") || p.Frame.Unreachable != "" {
			s.mu.Lock()
			if s.navErr == "" {
				if p.Frame.Unreachable != "" {
					s.navErr = "unreachable: " + p.Frame.Unreachable
				} else {
					s.navErr = "navigation failed (chrome-error page)"
				}
			}
			s.mu.Unlock()
		}
	})
	// 同一事件两处订阅：这里只做「在途请求」簿记 ＋ T-25 S1 原始快照抓存；
	// 聊天 SSE 的武装在下方第二条 handler 里（判定条件与抓存不同，不合并）。
	s.client.On(s.sessionID, "Network.requestWillBeSent", func(ev cdpEvent) {
		rp, ok := parseCaptureRequest(ev.Params)
		if !ok {
			return
		}
		s.mu.Lock()
		s.inflight[rp.RequestID] = time.Now()
		s.mu.Unlock()
		s.captureRequest(rp.RequestID, rp.Type, rp.Method, rp.URL, rp.Headers, rp.PostData, rp.Entries)
	})
	s.client.On(s.sessionID, "Network.loadingFinished", func(ev cdpEvent) {
		s.settleRequest(ev.Params)
		s.captureSettle(ev.Params, "")
	})
	s.client.On(s.sessionID, "Network.loadingFailed", func(ev cdpEvent) {
		s.settleRequest(ev.Params)
		s.captureSettle(ev.Params, captureFailureText(ev.Params))
	})
	s.client.On(s.sessionID, "Network.responseReceived", func(ev cdpEvent) {
		var p struct {
			RequestID string `json:"requestId"`
			Type      string `json:"type"`
			Response  struct {
				Status   int               `json:"status"`
				MimeType string            `json:"mimeType"`
				Headers  map[string]string `json:"headers"`
			} `json:"response"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		if p.Type == "Document" {
			s.mu.Lock()
			s.lastStatus = p.Response.Status
			s.lastHeaders = p.Response.Headers
			// challenge 状态随最新 Document 响应重算：403+挑战头 = 拦截；
			// 页面重载为 200 后自动清除（VLM 拖拽解锁 → reload 200 场景，TDD-COORD-04 真实 E2E）。
			s.challenge = p.Response.Status == http.StatusForbidden && hasChallengeHeader(p.Response.Headers)
			s.mu.Unlock()
		}
		s.captureResponse(p.RequestID, p.Response.Status, p.Response.MimeType, p.Response.Headers)
	})
	// 聊天 SSE 捕获（方案 1）：命中聊天补全端点 → 武装 streamResourceContent
	// 并消费 dataReceived 增量；loadingFinished/Failed 收尾。纯网络层被动监听，
	// 不注入页面 JS、不暂停请求（区别于 Fetch 域拦截与探针 fetch Hook）。
	s.client.On(s.sessionID, "Network.requestWillBeSent", func(ev cdpEvent) {
		var p struct {
			RequestID string `json:"requestId"`
			Type      string `json:"type"`
			Request   struct {
				URL string `json:"url"`
			} `json:"request"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		if p.Type != "XHR" && p.Type != "Fetch" {
			return
		}
		if !modelbridge.IsChatCompletionURL(p.Request.URL) && !modelbridge.IsGeminiStreamURL(p.Request.URL) {
			return
		}
		s.chat.OnRequestStart(p.RequestID)
		s.armChatStream(p.RequestID)
	})
	s.client.On(s.sessionID, "Network.dataReceived", func(ev cdpEvent) {
		var p struct {
			RequestID string `json:"requestId"`
			Data      string `json:"data"` // base64 增量（新版 Chrome；旧版缺字段则忽略）
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.Data == "" {
			return
		}
		s.chat.OnData(p.RequestID, p.Data)
	})
	s.client.On(s.sessionID, "Network.loadingFinished", func(ev cdpEvent) {
		var p struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		if s.chat.NeedsBody(p.RequestID) {
			// Gemini batchexecute NDJSON：流式增量通道不携带数据，收尾后
			// 异步拉取响应体（getResponseBody 在 reader goroutine 上调用会
			// 死锁，必须独立 goroutine）。
			go s.fetchChatBody(p.RequestID)
		} else {
			s.chat.OnFinish(p.RequestID)
		}
	})
	s.client.On(s.sessionID, "Network.loadingFailed", func(ev cdpEvent) {
		var p struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		s.chat.OnFail(p.RequestID)
	})
}

// armChatStream 在独立 goroutine 中武装 Network.streamResourceContent：
// 事件处理器运行在 CDP reader goroutine 上，若同步 CallSession 等待响应会
// 死锁（响应仍由同一 reader 读取）。武装失败（旧版 Chrome / 请求已流式化）
// 时静默降级——dataReceived.data 仍可能携带增量，preFeed 队列保证不乱序。
func (s *cdpSession) armChatStream(reqID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var out struct {
			BufferedData  string `json:"bufferedData"`
			Base64Encoded bool   `json:"base64Encoded"`
		}
		if err := s.client.CallSession(ctx, s.sessionID, "Network.streamResourceContent",
			map[string]any{"requestId": reqID}, &out); err != nil {
			s.chat.OnArmFailed(reqID)
			return
		}
		data := []byte(out.BufferedData)
		if out.Base64Encoded {
			if dec, derr := base64.StdEncoding.DecodeString(out.BufferedData); derr == nil {
				data = dec
			}
		}
		s.chat.OnBuffered(reqID, data)
	}()
}

func (s *cdpSession) settleRequest(params json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	if _, ok := s.inflight[p.RequestID]; ok {
		delete(s.inflight, p.RequestID)
	}
	s.mu.Unlock()
}

// freshInflight 剪除超过 ttl 的在途请求（长轮询/挂起请求不再阻塞稳定判定），
// 返回仍计入「网络忙碌」的连接数。ttl <= 0 时不剪枝。
func freshInflight(m map[string]time.Time, now time.Time, ttl time.Duration) int {
	if ttl <= 0 {
		return len(m)
	}
	n := 0
	for id, started := range m {
		if now.Sub(started) > ttl {
			delete(m, id)
			continue
		}
		n++
	}
	return n
}

func hasChallengeHeader(h map[string]string) bool {
	for k, v := range h {
		kv := strings.ToLower(k + " " + v)
		if strings.Contains(kv, "turnstile") || strings.Contains(kv, "cf-chl") ||
			strings.Contains(kv, "cf-challenge") || strings.Contains(kv, "challenge-platform") || strings.Contains(kv, "cf-mitigated") || strings.Contains(kv, "recaptcha") {
			return true
		}
	}
	return false
}

// fetchChatBody 在收尾后拉取聊天流完整响应体（Gemini NDJSON 路径）并喂给
// Stream；失败时静默放弃本轮（快照为空，API 回退 DOM extract）。
func (s *cdpSession) fetchChatBody(reqID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Network.getResponseBody",
		map[string]any{"requestId": reqID}, &out); err != nil {
		s.chat.OnFail(reqID)
		return
	}
	body := []byte(out.Body)
	if out.Base64Encoded {
		if dec, err := base64.StdEncoding.DecodeString(out.Body); err == nil {
			body = dec
		}
	} else {
		// Chrome Network.getResponseBody 会在 Content-Type 缺少 charset 时
		// 将原始字节按 ISO-8859-1 (Latin-1) 解码为 UTF-16 字符串。若每个字符都在
		// 0..255 范围内且字节拼回后构成合法的 UTF-8，则自动恢复为原始 UTF-8。
		runes := []rune(out.Body)
		latin1 := make([]byte, len(runes))
		allLatin1 := true
		for i, r := range runes {
			if r > 255 {
				allLatin1 = false
				break
			}
			latin1[i] = byte(r)
		}
		if allLatin1 && utf8.Valid(latin1) {
			body = latin1
		}
	}
	s.chat.OnBody(reqID, body)
}
