package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestHandler(t *testing.T, engine EngineClient) (http.Handler, *fakeEngine) {
	t.Helper()
	fake, _ := engine.(*fakeEngine)
	if fake == nil {
		fake = newFakeEngine()
		engine = fake
	}
	cfg := DefaultConfig("127.0.0.1:50051")
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	stable := time.Duration(0)
	gw, err := NewGateway(&cfg, Options{
		EngineFactory: func(string) (EngineClient, error) { return engine, nil },
		AnswerStable:  &stable,
		Sleep:         func(time.Duration) {},
		AuthStatePath: filepath.Join(t.TempDir(), "gateway-auth-state.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(gw, nil), fake
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestModelsEndpoint(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodGet, "/internal/provider/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 15 {
		t.Fatalf("模型数=%d（含 qwen-chat / chatgpt 三档）", len(out.Data))
	}
	found := false
	for _, m := range out.Data {
		if m["id"] == "deepseek-chat" && m["channel"] == "deepseek-web" {
			found = true
			cap := m["capabilities"].(map[string]any)
			if cap["thinking"] != true || cap["search"] != true {
				t.Fatalf("deepseek-chat capabilities=%v", cap)
			}
		}
	}
	if !found {
		t.Fatal("模型表缺少 deepseek-fast")
	}
}

func TestHealthEndpoint(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodGet, "/v1/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "ok" {
		t.Fatalf("health=%v", out)
	}
}

func TestChatNonStreamOverHTTP(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"model":    "deepseek-fast",
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || out.Choices[0].Message.Content != "确定性应答" {
		t.Fatalf("out=%+v", out)
	}
	if out.SessionID != "default" {
		t.Fatalf("session_id=%q", out.SessionID)
	}
}

func TestChatStreamOverHTTP(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"model":    "deepseek-fast",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("SSE 帧缺失: %q", body)
	}
	if !strings.Contains(body, "确定性应答") {
		t.Fatalf("SSE 缺少内容: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q", ct)
	}
}

func TestInternalProviderChannelBypassesModelRouting(t *testing.T) {
	h, fake := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"channel_id": "gemini-web",
		"model":      "gemini-flash",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !hasCall(fake.callKinds(), "gemini_input", "input") {
		t.Fatalf("应使用 gemini 语义: %+v", fake.callKinds())
	}
}

func TestUnknownModelReturns404(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"model":    "nope-model",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "未知模型") {
		t.Fatalf("错误信息=%s", rec.Body.String())
	}
}

func TestMissingMessagesReturns400(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"model": "deepseek-fast",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestXUWSHeadersOverride(t *testing.T) {
	h, fake := newTestHandler(t, nil)
	body := `{"model":"deepseek-fast","messages":[{"role":"user","content":"hi"}],"thinking":false,"search":false}`
	req := httptest.NewRequest(http.MethodPost, "/internal/provider/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-uws-session-id", "s-abc")
	req.Header.Set("x-uws-thinking", "on")
	req.Header.Set("x-uws-search", "on")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// thinking/search 被头覆盖为 on：fake engine 会记录 deepseek_thinking_switch/deepseek_search_switch
	if !hasCall(fake.callKinds(), "deepseek_thinking_switch", "click") {
		t.Fatalf("应执行 thinking 开关: %+v", fake.callKinds())
	}
	if !hasCall(fake.callKinds(), "deepseek_search_switch", "click") {
		t.Fatalf("应执行 search 开关: %+v", fake.callKinds())
	}
	if len(fake.initNames) != 1 || fake.initNames[0] != "s-abc" {
		t.Fatalf("应使用 x-uws-session-id 建会话: %v", fake.initNames)
	}
}

func TestSessionsCRUDOverHTTP(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{
		"channel_id": "deepseek-web", "session_id": "s-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created["status"] != "created" {
		t.Fatalf("created=%v", created)
	}
	rec = doJSON(t, h, http.MethodGet, "/v1/sessions", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "s-1") {
		t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodDelete, "/v1/sessions/s-1", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "deleted") {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodGet, "/v1/sessions", nil)
	if strings.Contains(rec.Body.String(), "s-1") {
		t.Fatalf("删除后仍存在: %s", rec.Body.String())
	}
}

func TestSessionsCreateWithModelResolvesChannel(t *testing.T) {
	h, fake := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{
		"model": "gemini-flash", "session_id": "s-g1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["channel_id"] != "gemini-web" {
		t.Fatalf("channel_id=%v（应按模型解析到 gemini-web）", created["channel_id"])
	}
	if created["model_applied"] != true {
		t.Fatalf("model_applied=%v body=%s", created["model_applied"], rec.Body.String())
	}
	if !hasCall(fake.callKinds(), "gemini_model_switch", "click") {
		t.Fatalf("应执行 Gemini 模型切换: %+v", fake.callKinds())
	}
	if len(fake.initNames) != 1 || fake.initNames[0] != "s-g1" {
		t.Fatalf("应创建会话 s-g1: %v", fake.initNames)
	}
}

func TestSessionsCreateUnknownModelReturns404(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{
		"model": "nope-model", "session_id": "s-x",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "未知模型") {
		t.Fatalf("错误信息=%s", rec.Body.String())
	}
}

func TestSessionsCreateModelSwitchBestEffort(t *testing.T) {
	h, fake := newTestHandler(t, nil)
	fake.failAlways["gemini_model_switch"] = &EngineError{Code: codeInternal, Reason: "页面未就绪"}
	rec := doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{
		"model": "gemini-flash", "session_id": "s-g2",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("模式切换失败不应阻塞会话创建: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["model_applied"] != false {
		t.Fatalf("model_applied=%v", created["model_applied"])
	}
	if s, _ := created["model_error"].(string); !strings.Contains(s, "gemini_model_switch") {
		t.Fatalf("model_error=%v", created["model_error"])
	}
	if len(fake.initNames) != 1 {
		t.Fatalf("会话仍应创建: %v", fake.initNames)
	}
}

func TestAuthStatusEndpoint(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	h.(*Handler).g.setAuthSnapshot("deepseek-web", authSnapshot{
		Status: AuthUnknown, CheckedAt: time.Now(), Rechecks: 1, Detail: "一次复检后仍未知",
	})
	rec := doJSON(t, h, http.MethodGet, "/v1/auth/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	sites := out["sites"].(map[string]any)
	if sites["deepseek"] == nil || sites["gemini"] == nil {
		t.Fatalf("sites=%v", sites)
	}
	ds := sites["deepseek"].(map[string]any)
	if ds["status"] != "unknown" || ds["channel_id"] != "deepseek-web" || ds["manual_reset_required"] != true || ds["rechecks"] != float64(1) {
		t.Fatalf("deepseek status=%v", ds)
	}
}

func TestOpenAIHTTPChannelNotImplemented(t *testing.T) {
	cfg := DefaultConfig("127.0.0.1:50051")
	cfg.Channels = append(cfg.Channels, ChannelConfig{
		ID: "my-api", Type: ChannelTypeOpenAIHTTP, BaseURL: "https://api.example.com",
		Models: []ModelConfig{{ID: "my-model"}},
	})
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	fake := newFakeEngine()
	gw, err := NewGateway(&cfg, Options{
		EngineFactory: func(string) (EngineClient, error) { return fake, nil },
		Sleep:         func(time.Duration) {},
		AuthStatePath: filepath.Join(t.TempDir(), "gateway-auth-state.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(gw, nil)
	rec := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"model":    "my-model",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func hasCall(calls []fakeCall, semantic, action string) bool {
	for _, c := range calls {
		if c.semantic == semantic && c.action == action {
			return true
		}
	}
	return false
}
