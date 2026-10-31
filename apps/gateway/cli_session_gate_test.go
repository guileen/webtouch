package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"uws/pkg/clisession"
)

func TestProviderHTTPDeprecatedHeaders(t *testing.T) {
	t.Setenv("UWS_PROVIDER_HTTP", "")
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodGet, "/internal/provider/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Deprecation") != "true" || rec.Header().Get("X-WebTouch-Contract") != "deprecated" || rec.Header().Get("X-WebTouch-Successor") != "cli" {
		t.Fatalf("headers=%v", rec.Header())
	}
}

func TestProviderHTTPRetiredDoesNotCallEngine(t *testing.T) {
	t.Setenv("UWS_PROVIDER_HTTP", "410")
	h, fake := newTestHandler(t, nil)
	for _, path := range []string{"/internal/provider/models", "/internal/provider/chat", "/internal/provider/responses"} {
		rec := doJSON(t, h, http.MethodPost, path, map[string]any{
			"model": "gemini-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "input": "hi",
		})
		if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "provider_http_retired") || !strings.Contains(rec.Body.String(), "webtouch") {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-WebTouch-Successor") != "cli" {
			t.Fatalf("%s headers=%v", path, rec.Header())
		}
	}
	if len(fake.initNames) != 0 || len(fake.callKinds()) != 0 {
		t.Fatalf("retired HTTP reached engine inits=%v calls=%v", fake.initNames, fake.callKinds())
	}

	bare := httptest.NewRecorder()
	(&Handler{}).ServeHTTP(bare, httptest.NewRequest(http.MethodGet, "/internal/provider/models", nil))
	if bare.Code != http.StatusGone {
		t.Fatalf("nil gateway: %d %s", bare.Code, bare.Body.String())
	}
}

func TestCLISessionReuseAndCloseOnProviderChat(t *testing.T) {
	t.Setenv("UWS_PROVIDER_HTTP", "")
	h, fake := newTestHandler(t, nil)
	hh := h.(*Handler)
	st, err := clisession.Open(filepath.Join(t.TempDir(), "cli-sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	hh.g.cliSessions = st
	created, err := st.Create(clisession.CreateInput{Channel: "deepseek-web"})
	if err != nil {
		t.Fatal(err)
	}

	rec := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"model": "deepseek-fast", "session_id": created.SessionID,
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reuse status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-WebTouch-Contract") != "deprecated" {
		t.Fatalf("headers=%v", rec.Header())
	}
	view, ok, err := st.Lookup(created.SessionID)
	if err != nil || !ok || view.State != clisession.StateActive {
		t.Fatalf("lease after use ok=%v view=%+v err=%v", ok, view, err)
	}

	unknown := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"model": "deepseek-fast", "session_id": "wts_notalease",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if unknown.Code != http.StatusOK {
		t.Fatalf("unknown id should stay on the legacy path: %d %s", unknown.Code, unknown.Body.String())
	}

	site := "c_from_page"
	hh.g.rememberCLISiteConversation(created.SessionID, &site)
	view, _, err = st.Lookup(created.SessionID)
	if err != nil || view.SiteConversationID == nil || *view.SiteConversationID != site || view.Hub.SiteConversationID == nil {
		t.Fatalf("site writeback=%+v err=%v", view, err)
	}

	if _, err := st.Close(created.SessionID); err != nil {
		t.Fatal(err)
	}
	beforeInit := len(fake.initNames)
	beforeCalls := len(fake.callKinds())
	closed := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"model": "deepseek-fast", "session_id": created.SessionID,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if closed.Code != http.StatusConflict || !strings.Contains(closed.Body.String(), "session_lease_closed") {
		t.Fatalf("closed lease: %d %s", closed.Code, closed.Body.String())
	}
	if len(fake.initNames) != beforeInit || len(fake.callKinds()) != beforeCalls {
		t.Fatalf("closed lease reached engine inits=%v calls=%v", fake.initNames, fake.callKinds())
	}
}
