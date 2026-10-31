package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newAuthStateTestGateway(t *testing.T, engine *fakeEngine, path string) *Gateway {
	t.Helper()
	cfg := DefaultConfig("127.0.0.1:50051")
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	stable := time.Duration(0)
	gw, err := NewGateway(&cfg, Options{
		EngineFactory: func(string) (EngineClient, error) { return engine, nil },
		AnswerStable:  &stable,
		Sleep:         func(time.Duration) {},
		AuthCheckTTL:  time.Minute,
		AuthStatePath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

func TestAuthNegativeStatePersistsAndStaysLatched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "state.json")
	first := newAuthStateTestGateway(t, newFakeEngine(), path)
	first.setAuthSnapshot("deepseek-web", authSnapshot{
		Status: AuthChallenge, CheckedAt: time.Now(), Detail: "人工挑战待处理",
	})

	engine := newFakeEngine()
	checks := 0
	engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			checks++
		}
		return ActionResult{Success: true, ExtractedData: `{"status":"logged_in","composer":true}`}, nil
	}
	second := newAuthStateTestGateway(t, engine, path)
	ch := &second.cfg.Channels[0]
	if err := second.ensureAuthenticated(context.Background(), ch, DefaultSessionID, ""); err == nil {
		t.Fatal("挑战状态恢复后不得放行")
	} else if authErr, ok := err.(*AuthRequiredError); !ok || authErr.Code != "human_verification_required" {
		t.Fatalf("error=%T %v, want human_verification_required", err, err)
	}
	if checks != 0 {
		t.Fatalf("挑战锁定期间不应重新探测页面，checks=%d", checks)
	}
}

func TestAuthProbeOnlyTreatsVisibleChallengeFramesAsChallenge(t *testing.T) {
	if !strings.Contains(authProbeScript, "visible(e) && challengeMarker.test") {
		t.Fatal("challenge iframe detection must require a visible frame to avoid invisible SDK false positives")
	}
}

func TestLoggedOutStatePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "state.json")
	first := newAuthStateTestGateway(t, newFakeEngine(), path)
	first.setAuthSnapshot("deepseek-web", authSnapshot{Status: AuthLoggedOut, CheckedAt: time.Now()})

	engine := newFakeEngine()
	second := newAuthStateTestGateway(t, engine, path)
	if err := second.authLatchError("deepseek-web"); err == nil {
		t.Fatal("未登录锁应跨重启保持")
	} else if authErr, ok := err.(*AuthRequiredError); !ok || authErr.Code != "login_required" {
		t.Fatalf("error=%T %v, want login_required", err, err)
	}
	if len(engine.initNames) != 0 || len(engine.callKinds()) != 0 {
		t.Fatalf("恢复未登录锁不得访问页面: init=%v calls=%+v", engine.initNames, engine.callKinds())
	}
}

func TestChatLockStopsBeforeSessionOrPageActions(t *testing.T) {
	engine := newFakeEngine()
	gw := newAuthStateTestGateway(t, engine, filepath.Join(t.TempDir(), "auth-state.json"))
	gw.setAuthSnapshot("deepseek-web", authSnapshot{Status: AuthLoggedOut, CheckedAt: time.Now()})

	if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "不要提交"})); err == nil {
		t.Fatal("未登录锁定状态应拒绝聊天")
	}
	stream, err := gw.ChatStream(context.Background(), chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "不要提交"}))
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for event := range stream {
		if event.Err != nil {
			streamErr = event.Err
		}
	}
	if streamErr == nil {
		t.Fatal("未登录锁定状态应拒绝流式聊天")
	}
	if len(engine.initNames) != 0 || len(engine.callKinds()) != 0 {
		t.Fatalf("锁定状态不得创建标签页或执行页面动作: init=%v calls=%+v", engine.initNames, engine.callKinds())
	}
}

func TestUnknownAuthCooldownStopsPageActions(t *testing.T) {
	engine := newFakeEngine()
	gw := newAuthStateTestGateway(t, engine, filepath.Join(t.TempDir(), "auth-state.json"))
	gw.setAuthSnapshot("deepseek-web", authSnapshot{
		Status: AuthUnknown, CheckedAt: time.Now(), Detail: "CDP 检查暂不可用",
	})
	_, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "暂不重试"}))
	if err == nil {
		t.Fatal("最近一次认证状态未知时应在冷却期 fail-closed")
	}
	if len(engine.initNames) != 0 || len(engine.callKinds()) != 0 {
		t.Fatalf("未知状态冷却期不得创建标签页或执行页面动作: init=%v calls=%+v", engine.initNames, engine.callKinds())
	}
}

func TestAuthProbeFailureDetailKeepsOnlyLocalErrorClass(t *testing.T) {
	detail := authProbeFailureDetail(&EngineError{Code: codeUnavailable, Reason: "page text and target URL must not persist"})
	if !strings.Contains(detail, "gRPC Unavailable") {
		t.Fatalf("detail should identify the local transport class: %q", detail)
	}
	if strings.Contains(detail, "page text") || strings.Contains(detail, "target URL") {
		t.Fatalf("detail must not persist raw engine messages: %q", detail)
	}
}

func TestUnknownAuthAllowsOneExpiredRecheckThenLocksAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth-state.json")
	engine := newFakeEngine()
	checks := 0
	engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			checks++
		}
		return ActionResult{Success: true, ExtractedData: `{"status":"unknown","composer":false}`}, nil
	}
	first := newAuthStateTestGateway(t, engine, path)
	first.setAuthSnapshot("deepseek-web", authSnapshot{
		Status: AuthUnknown, CheckedAt: time.Now().Add(-2 * time.Minute), Detail: "上次 CDP 检查失败",
	})
	ch := &first.cfg.Channels[0]
	if _, err := first.ensureSession(context.Background(), ch, DefaultSessionID, ""); err != nil {
		t.Fatalf("prepare existing page session: %v", err)
	}
	if err := first.ensureAuthenticated(context.Background(), ch, DefaultSessionID, ""); err == nil {
		t.Fatal("复检仍无法确认登录态时必须 fail-closed")
	}
	if checks != 1 || first.authSnapshot("deepseek-web").Rechecks != 1 {
		t.Fatalf("到期后应恰好复检一次: checks=%d snapshot=%+v", checks, first.authSnapshot("deepseek-web"))
	}

	// Simulate another TTL elapsing and a process restart. The one-recheck budget
	// must remain latched and stop before creating another browser session.
	snap := first.authSnapshot("deepseek-web")
	snap.CheckedAt = time.Now().Add(-2 * time.Minute)
	first.setAuthSnapshot("deepseek-web", snap)
	restartedEngine := newFakeEngine()
	restartedEngine.evaluateFunc = engine.evaluateFunc
	second := newAuthStateTestGateway(t, restartedEngine, path)
	if _, err := second.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "不要提交"})); err == nil {
		t.Fatal("一次复检用尽后跨重启仍应锁定")
	}
	if checks != 1 || len(restartedEngine.initNames) != 0 || len(restartedEngine.callKinds()) != 0 {
		t.Fatalf("复检预算用尽后不得再探测或创建会话: checks=%d init=%v calls=%+v", checks, restartedEngine.initNames, restartedEngine.callKinds())
	}
}

func TestAuthResetClearsLatchWithoutTouchingPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "state.json")
	engine := newFakeEngine()
	checks := 0
	engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			checks++
		}
		return ActionResult{Success: true, ExtractedData: `{"status":"logged_in","composer":true}`}, nil
	}
	gw := newAuthStateTestGateway(t, engine, path)
	gw.setAuthSnapshot("deepseek-web", authSnapshot{Status: AuthLoggedOut, CheckedAt: time.Now()})
	h := NewHandler(gw, nil)
	rec := doJSON(t, h, http.MethodPost, "/v1/auth/reset", map[string]any{"channel_id": "deepseek-web"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if checks != 0 {
		t.Fatalf("reset endpoint must not touch page, checks=%d", checks)
	}
	if got := gw.authSnapshot("deepseek-web").Status; got != "" {
		t.Fatalf("state after reset=%q", got)
	}

	var persisted map[string]authSnapshot
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if _, exists := persisted["deepseek-web"]; exists {
		t.Fatalf("reset state still persisted: %v", persisted)
	}
}

func TestLoggedInStateIsNotPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "state.json")
	gw := newAuthStateTestGateway(t, newFakeEngine(), path)
	gw.setAuthSnapshot("deepseek-web", authSnapshot{Status: AuthLoggedIn, CheckedAt: time.Now()})
	second := newAuthStateTestGateway(t, newFakeEngine(), path)
	if got := second.authSnapshot("deepseek-web").Status; got != "" {
		t.Fatalf("logged-in state must be rechecked after restart, got %q", got)
	}
}

func TestAuthResetRejectsUnknownChannel(t *testing.T) {
	h := NewHandler(newTestGateway(t, newFakeEngine()), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/reset", strings.NewReader(`{"channel_id":"missing"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
