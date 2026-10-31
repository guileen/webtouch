package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClassifyChatErrorWrappedAndUpstream 守住 2026-10-03 真机发现的两条分类缺口：
// ① 包装后（fmt.Errorf %w）仍要认出内层真实类型，不得退化成 internal_error/500；
// ② 网关→引擎这一段不可用（gRPC code=14 UNVAILABLE）要归 engine_unavailable/503 可重试，
// 而不是 provider_error/502 不可重试（请求到了网关，是上游接入面不可用）。
func TestClassifyChatErrorWrappedAndUpstream(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		code      string
		status    int
		retryable bool
		action    string
	}{
		{
			"引擎不可达（构造点只留原文）",
			&ChatError{Reason: `initialize_context(s): engine error code=14: connection error: desc = "transport: Error while dialing: dial tcp 127.0.0.1:59999: connect: connection refused"`},
			errCodeEngineUnavailable, http.StatusServiceUnavailable, true, actionRetry,
		},
		{
			"引擎不可达（结构码）",
			fmt.Errorf("initialize_context(s): %w", &EngineError{Code: codeUnavailable, Reason: "connection error"}),
			errCodeEngineUnavailable, http.StatusServiceUnavailable, true, actionRetry,
		},
		{
			"就绪类被包装后仍可判",
			fmt.Errorf("%w；模型菜单自检: 现场", &ChatError{Reason: "gemini_model_switch: target_destroyed", Semantic: "gemini_model_switch"}),
			errCodePageNotReady, http.StatusServiceUnavailable, true, actionRetry,
		},
		{
			"提交未知被包装后也不重放",
			fmt.Errorf("wrap: %w", &ChatError{Reason: "submission_outcome_unknown: in_page_direct: evaluation failed", Code: errCodeSubmissionUnknown}),
			errCodeSubmissionUnknown, http.StatusBadGateway, false, actionAbort,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyChatError(c.err)
			if got.Code != c.code || got.Status != c.status || got.Retryable != c.retryable || got.Action != c.action {
				t.Fatalf("classify=%+v; want code=%s status=%d retryable=%v action=%s", got, c.code, c.status, c.retryable, c.action)
			}
		})
	}
}

// TestClassifyChatError 守住契约 §5「/v1/chat 失败分类」：机器可判的 code/HTTP/retryable/action。
func TestClassifyChatError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		code      string
		status    int
		retryable bool
		action    string
	}{
		{"请求非法", &BadRequestError{Reason: "x"}, "invalid_request", http.StatusBadRequest, false, actionAbort},
		{"模型未知", &NotFoundError{Reason: "x"}, "unknown_model", http.StatusNotFound, false, actionAbort},
		{"未实现", &NotImplementedError{Reason: "x"}, "not_implemented", http.StatusNotImplemented, false, actionAbort},
		{"未登录", &AuthRequiredError{Reason: "x", Code: "login_required"}, "login_required", http.StatusUnauthorized, false, actionRelogin},
		{"登录态未知", &AuthCheckError{Reason: "x"}, errCodeAuthCheckFailed, http.StatusServiceUnavailable, false, actionRelogin},
		{"绑定失活", &ChatError{Reason: "initialize_context(s): engine error code=13: acquire target: cdp: bound session target t cannot be adopted; 目标 t 已不存在"},
			errCodeSessionBindingDead, http.StatusConflict, true, actionResetSession},
		{"就绪类", &ChatError{Reason: "chat_input: target_destroyed: Tab 被关闭"},
			errCodePageNotReady, http.StatusServiceUnavailable, true, actionRetry},
		{"引擎不可达", &ChatError{Reason: "chat_input: 引擎不可用: dial tcp: connection refused"},
			errCodeEngineUnavailable, http.StatusServiceUnavailable, true, actionRetry},
		{"本轮超时", &ChatError{Reason: "chat_answer: 超时预算内未确认新回答已完成", Code: errCodeUpstreamTimeout},
			errCodeUpstreamTimeout, http.StatusGatewayTimeout, false, actionAbort},
		{"提交结果未知", &ChatError{Reason: "submission_outcome_unknown: x", Semantic: "chat_send"},
			errCodeSubmissionUnknown, http.StatusBadGateway, false, actionAbort},
		{"站点业务失败", &ChatError{Reason: "chat_answer: 空回答", Semantic: "chat_answer"},
			errCodeProviderError, http.StatusBadGateway, false, actionAbort},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyChatError(c.err)
			if got.Code != c.code || got.Status != c.status || got.Retryable != c.retryable || got.Action != c.action {
				t.Fatalf("classify=%+v; want code=%s status=%d retryable=%v action=%s", got, c.code, c.status, c.retryable, c.action)
			}
		})
	}
}

// TestChatErrorStatusMatchesWriteChatError 的同源断言在 preflight_test.go；本例补错误体字段。
func TestWriteChatErrorBodyCarriesClassification(t *testing.T) {
	rec := httptest.NewRecorder()
	writeChatError(rec, &ChatError{Reason: "bound session target t cannot be adopted; 目标 t 已不存在", Semantic: "chat_input"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409", rec.Code)
	}
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v body=%s", err, rec.Body.String())
	}
	if body.Error["code"] != errCodeSessionBindingDead || body.Error["retryable"] != true || body.Error["action"] != actionResetSession {
		t.Fatalf("错误体缺分类: %s", rec.Body.String())
	}
	if body.Error["semantic"] != "chat_input" {
		t.Fatalf("应保留步骤 semantic: %s", rec.Body.String())
	}
}

// TestChatSessionBindingDeadSelfHeals 守住「绑定失活 → 内部清死绑定并重放一次 → 成功」，
// 即用户定案「甚至重试一次，你内部至少要做一次」在 502 类失败上的落地。
func TestChatSessionBindingDeadSelfHeals(t *testing.T) {
	engine := newFakeEngine()
	engine.incrementAnswers = true // 重建后基线需与新回答不同
	engine.initFailOnce = &EngineError{Code: codeInternal,
		Reason: "acquire target: cdp: bound session target tgt-x cannot be adopted; refusing a different target: 目标 tgt-x 已不存在"}
	gw := newTestGateway(t, engine)
	if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "hi"})); err != nil {
		t.Fatalf("绑定失活应自愈而非抛 502: %v", err)
	}
	if len(engine.initNames) != 2 {
		t.Fatalf("应清死绑定后重建一次（init=%d，want 2）", len(engine.initNames))
	}
	if len(engine.closeNames) == 0 {
		t.Fatalf("重建前应清一次死绑定（closeNames=%v）", engine.closeNames)
	}
}
