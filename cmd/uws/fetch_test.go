package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchAnonymous(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("MOLI_BIN", "/nonexistent/moli")
	t.Setenv("WEBTOUCH_MOLI_BIN", "/nonexistent/moli")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("credentials leaked")
		}
		if r.URL.Path == "/start" {
			http.SetCookie(w, &http.Cookie{Name: "secret", Value: "value"})
			http.Redirect(w, r, "/end", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>hidden</title></head><body><h1>Hello</h1><p>Read <a href="/more">more</a>.</p><script>secret</script></body></html>`))
	}))
	defer server.Close()
	var out strings.Builder
	if err := fetchCLI([]string{"--url", server.URL + "/start"}, &out); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
		t.Fatal(err)
	}
	content, _ := result["content"].(string)
	if result["backend"] != "http" || result["url"] != server.URL+"/end" || !strings.Contains(content, "# Hello") || !strings.Contains(content, "[more]("+server.URL+"/more)") || strings.Contains(content, "secret") || strings.Contains(content, "hidden") {
		t.Fatalf("unexpected result: %s", out.String())
	}
}

func TestFetchErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) }))
	defer server.Close()
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"--url", "http://user:pass@example.com"}, "invalid_argument"},
		{[]string{"--url", server.URL, "--format", "screenshot"}, "unsupported_format"},
		{[]string{"--url", server.URL}, "http_status"},
		{[]string{"--url", server.URL, "--session", "x"}, "invalid_argument"},
	} {
		var out strings.Builder
		if fetchCLI(tc.args, &out) == nil || !strings.Contains(out.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%v: %s", tc.args, out.String())
		}
	}
}

func TestFetchResponseBoundaries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/large":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(strings.Repeat("x", fetchLimit+1)))
		case "/binary":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("image"))
		case "/redirect":
			http.Redirect(w, r, "http://user:pass@example.com/", http.StatusFound)
		}
	}))
	defer server.Close()
	for _, tc := range []struct{ path, code string }{{"/large", "output_too_large"}, {"/binary", "unsupported_content_type"}, {"/redirect", "fetch_failed"}} {
		var out strings.Builder
		if fetchCLI([]string{"--url", server.URL + tc.path}, &out) == nil || !strings.Contains(out.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%s: %s", tc.path, out.String())
		}
	}
}
