package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientModelRoutesRetiredWithoutExecutingBrowser(t *testing.T) {
	// A nil gateway makes any accidental browser execution fail immediately.
	h := &Handler{}
	for _, path := range []string{"/v1/models", "/v1/text", "/v1/chat", "/v1/chat/completions", "/v1/responses", "/v1/gemini-web/chat", "/v1/gemini-web/chat/completions", "/v1/gemini-web/responses"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"text":"hello"}`))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "gateway_moved") {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
			}
		})
	}
}

func TestPrivateProviderBrowserHostDownNeverInvokesEngine(t *testing.T) {
	cfg := DefaultConfig("127.0.0.1:50051")
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	fake := newFakeEngine()
	g, err := NewGateway(&cfg, Options{
		EngineFactory:       func(string) (EngineClient, error) { return fake, nil },
		AttachCDP:           "http://127.0.0.1:59999",
		BrowserHostInterval: time.Minute,
		AuthStatePath:       filepath.Join(t.TempDir(), "auth.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(g, nil)
	for _, path := range []string{"/internal/provider/chat", "/internal/provider/responses"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"gemini-flash","messages":[{"role":"user","content":"hello"}],"input":"hello"}`)))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "browser_host_down") {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	if len(fake.callKinds()) != 0 {
		t.Fatalf("browser-down request reached engine: %v", fake.callKinds())
	}
}
