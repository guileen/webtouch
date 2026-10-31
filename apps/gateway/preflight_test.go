package gateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestIsPreflightReadinessFailureExcludesSubmissionUnknown 守住安全边界：
// 提交结果未知一律不重放（T-26 fail-closed）。
func TestIsPreflightReadinessFailureExcludesSubmissionUnknown(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&ChatError{Reason: "gemini_model_switch: target_destroyed", Semantic: "gemini_model_switch"}, true},
		{&ChatError{Reason: "gemini_model_switch: model_switch_failed:模型菜单未出现", Semantic: "gemini_model_switch"}, true},
		{&EngineError{Code: codeAborted, Reason: "cdp error -32001: Session with given id not found"}, true},
		{&ChatError{Reason: "submission_outcome_unknown: submission_outcome_unknown: in_page_direct: evaluation failed: engine error code=4: verification timeout: page did not stabilize"}, false},
		{&ChatError{Reason: "send_button: physical_event_dispatched_outcome_unknown"}, false},
		{&BadRequestError{Reason: "messages 缺少 user 消息"}, false},
	}
	for _, c := range cases {
		if got := isPreflightReadinessFailure(c.err); got != c.want {
			t.Fatalf("isPreflightReadinessFailure(%v)=%v, want %v", c.err, got, c.want)
		}
	}
	if isRetryableRoundFailure(&ChatError{Reason: "submission_outcome_unknown: target_destroyed", Semantic: "send_button"}) {
		t.Fatal("提交结果未知不得触发整轮恢复")
	}
}

// TestEnsureModelReadinessRetryBeforeRebuild 模型菜单未出现属就绪类：原地重试即可，
// 不必重建标签页（T-33：新会话第一句不再 500）。
func TestEnsureModelReadinessRetryBeforeRebuild(t *testing.T) {
	engine := newFakeEngine()
	engine.failOnce["gemini_model_switch"] = &EngineError{Code: codeAborted, Reason: "model_switch_failed:模型菜单未出现"}
	gw := newTestGateway(t, engine)
	if _, err := gw.Chat(context.Background(), chatReq("gemini-web", "gemini-3.6-flash",
		Message{Role: "user", Content: "你好"})); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if n := countCalls(engine.callKinds(), "gemini_model_switch"); n != 2 {
		t.Fatalf("就绪类失败应原地重试一次，切换次数=%d", n)
	}
	if len(engine.initNames) != 1 {
		t.Fatalf("原地重试成功不应重建标签页，init=%d", len(engine.initNames))
	}
}

// TestAuthProbeReadinessRetryThenSucceeds 登录态探测遇就绪类失败应原地重试：
// 一次瞬时失败不得被记账成站点级 15 分钟 unknown（T-33）。
func TestAuthProbeReadinessRetryThenSucceeds(t *testing.T) {
	engine := newFakeEngine()
	probes := 0
	engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			probes++
			if probes == 1 {
				return ActionResult{}, &EngineError{Code: codeAborted, Reason: "target_destroyed: Tab 被用户关闭"}
			}
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		return ActionResult{Success: true, ExtractedData: `{"status":"not_dispatched","error":"fake direct unavailable"}`}, nil
	}
	gw := newTestGateway(t, engine)
	if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "hi"})); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if probes != 2 {
		t.Fatalf("探测次数=%d, want 2", probes)
	}
	if len(engine.closeNames) != 0 {
		t.Fatalf("原地重试成功不应重建标签页: %v", engine.closeNames)
	}
}

// TestAuthProbeReadinessRebuildsTab 就绪类失败在重试用尽后应重建标签页再探测一次，
// 避免把可恢复的瞬时失败锁成站点级封锁。
func TestAuthProbeReadinessRebuildsTab(t *testing.T) {
	engine := newFakeEngine()
	probes := 0
	engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			probes++
			if probes <= 3 { // 首轮 1 次 + 原地重试 2 次全部失败
				return ActionResult{}, &EngineError{Code: codeAborted, Reason: "cdp error -32001: Session with given id not found"}
			}
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		return ActionResult{Success: true, ExtractedData: `{"status":"not_dispatched","error":"fake direct unavailable"}`}, nil
	}
	gw := newTestGateway(t, engine)
	if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "hi"})); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if probes != 4 {
		t.Fatalf("探测次数=%d, want 4（3 次首轮 + 重建后 1 次）", probes)
	}
	if len(engine.closeNames) != 1 {
		t.Fatalf("应重建标签页一次: %v", engine.closeNames)
	}
	if len(engine.initNames) != 2 {
		t.Fatalf("重建后 init 次数=%d, want 2", len(engine.initNames))
	}
}

// TestChatErrorStatusMatchesWriteChatError 守住逐次重试日志与响应写出的状态映射同源
// （runtime 日志里的 http= 必须等于调用方实际收到的状态码，防两处各写一套漂移）。
func TestChatErrorStatusMatchesWriteChatError(t *testing.T) {
	cases := []error{
		&BadRequestError{Reason: "x"},
		&NotFoundError{Reason: "x"},
		&NotImplementedError{Reason: "x"},
		&AuthRequiredError{Reason: "x", Code: "c"},
		&AuthCheckError{Reason: "x", Code: "c"},
		&ChatError{Reason: "x"},
		errors.New("boom"),
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeChatError(rec, c)
		if got, want := chatErrorStatus(c), rec.Code; got != want {
			t.Fatalf("chatErrorStatus(%T)=%d, writeChatError=%d", c, got, want)
		}
	}
}

// TestDefaultChatTimeoutCoversSlowChannel 守住 2026-10-03 派单 d1957d57b 的定性结论：
// gemini-flash 单轮实测 31–85s，默认预算必须覆盖实测最坏值（原 60s 会切掉合法慢轮）。
func TestDefaultChatTimeoutCoversSlowChannel(t *testing.T) {
	if DefaultChatTimeout < 90*time.Second {
		t.Fatalf("默认 chatTimeout=%s 应覆盖实测最坏 84.7s", DefaultChatTimeout)
	}
	gw := newTestGateway(t, newFakeEngine())
	if gw.chatTimeout != DefaultChatTimeout {
		t.Fatalf("未显式指定时 chatTimeout=%s, want %s", gw.chatTimeout, DefaultChatTimeout)
	}
	if gw.chatTimeout != 120*time.Second {
		t.Fatalf("默认 chatTimeout 定为 120s，实际 %s", gw.chatTimeout)
	}
}

// TestChatRetryOnTimeoutDefaultsOff 守住 T-26：超时不能证明写未发生，
// 默认不得对「本轮超时类失败」整轮重放；只能显式放宽。
func TestChatRetryOnTimeoutDefaultsOff(t *testing.T) {
	gw := newTestGateway(t, newFakeEngine())
	if gw.chatRetryOnTimeout {
		t.Fatal("chatRetryOnTimeout 默认必须为 false（T-26 安全默认）")
	}
	timeoutErr := &ChatError{Reason: "submission_outcome_unknown: in_page_direct: engine error code=4: context deadline exceeded", Semantic: "chat"}
	if gw.isRetryableChatFailure(timeoutErr) {
		t.Fatal("默认不得把超时类失败判为可重试（T-26 fail-closed）")
	}
	// 显式放宽后才纳入。
	gw.chatRetryOnTimeout = true
	if !gw.isRetryableChatFailure(timeoutErr) {
		t.Fatal("显式放宽后应把超时类失败判为可重试")
	}
}

// TestIsRoundTimeoutMarkers 覆盖两类超时特征串。
func TestIsRoundTimeoutMarkers(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&ChatError{Reason: "context deadline exceeded"}, true},
		{&ChatError{Reason: "超时预算内未确认新回答已完成"}, true},
		{&ChatError{Reason: "send_button: physical_event_dispatched_outcome_unknown"}, false},
		{&BadRequestError{Reason: "messages 缺少 user 消息"}, false},
	}
	for _, c := range cases {
		if got := isRoundTimeout(c.err); got != c.want {
			t.Fatalf("isRoundTimeout(%v)=%v, want %v", c.err, got, c.want)
		}
	}
}

// TestChatRetryEnvOverrides 覆盖可配常量：环境变量写入即生效，缺省回落安全默认。
func TestChatRetryEnvOverrides(t *testing.T) {
	t.Setenv("UWS_CHAT_RETRY_ON_TIMEOUT", "")
	if defaultChatRetryOnTimeout {
		t.Fatal("defaultChatRetryOnTimeout 常量应为 false")
	}
	if chatRetryOnTimeoutFromEnv() {
		t.Fatal("未设环境变量时应回落默认 false")
	}
	t.Setenv("UWS_CHAT_RETRY_ON_TIMEOUT", "1")
	if !chatRetryOnTimeoutFromEnv() {
		t.Fatal("UWS_CHAT_RETRY_ON_TIMEOUT=1 应开启")
	}
	t.Setenv("UWS_CHAT_RETRY_ON_TIMEOUT", "0")
	if chatRetryOnTimeoutFromEnv() {
		t.Fatal("UWS_CHAT_RETRY_ON_TIMEOUT=0 应关闭")
	}
	t.Setenv("UWS_CHAT_RETRY_ATTEMPTS", "3")
	if got := chatRetryAttemptsFromEnv(); got != 3 {
		t.Fatalf("UWS_CHAT_RETRY_ATTEMPTS=3 应生效，实际 %d", got)
	}
	t.Setenv("UWS_CHAT_RETRY_ATTEMPTS", "abc")
	if got := chatRetryAttemptsFromEnv(); got != defaultChatRetryAttempts {
		t.Fatalf("非法值应回落默认 %d，实际 %d", defaultChatRetryAttempts, got)
	}
	t.Setenv("UWS_CHAT_RETRY_BACKOFF_MS", "500")
	if got := chatRetryBackoffFromEnv(); got != 500*time.Millisecond {
		t.Fatalf("UWS_CHAT_RETRY_BACKOFF_MS=500 应为 500ms，实际 %s", got)
	}
}
