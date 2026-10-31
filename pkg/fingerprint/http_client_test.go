package fingerprint

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIdentityHTTPClientCarriesProfileHeadersAndBundleCookies(t *testing.T) {
	profile := mustParse(t, validProfileJSON)
	profile.Network.ProxyURL = "" // local HTTP fixture must not require the profile's example SOCKS5 relay
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("User-Agent"), profile.Emulation.UserAgent; got != want {
			t.Errorf("User-Agent = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Accept-Language"), "zh-CN,zh,en"; got != want {
			t.Errorf("Accept-Language = %q, want %q", got, want)
		}
		if got := r.Header.Get("Sec-CH-UA"); !strings.Contains(got, `"Google Chrome";v="152"`) {
			t.Errorf("Sec-CH-UA = %q", got)
		}
		if got, want := r.Header.Get("Sec-CH-UA-Platform"), `"macOS"`; got != want {
			t.Errorf("Sec-CH-UA-Platform = %q, want %q", got, want)
		}
		if got := r.Header.Get("Sec-CH-UA-Full-Version"); got != "" {
			t.Errorf("low-entropy mode unexpectedly sent high-entropy hint %q", got)
		}
		if cookie, err := r.Cookie("sid"); err != nil || cookie.Value != "bundle-value" {
			t.Errorf("sid cookie = %v, err=%v, want bundle-value", cookie, err)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	bundle, err := NewStateBundle(profile.ProfileID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bundle.Cookies, _ = json.Marshal([]map[string]any{{"name": "sid", "value": "bundle-value", "url": server.URL, "path": "/"}})
	client, err := NewIdentityHTTPClient(profile, bundle, HTTPClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.NewRequest(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Public Go callers may set context headers directly, but the identity fields
	// must still be repaired immediately before the request is sent.
	req.Header.Set("User-Agent", "other identity")
	req.Header.Set("Cookie", "sid=other-session")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if body, _ := io.ReadAll(response.Body); string(body) != "ok" {
		t.Fatalf("response = %q", body)
	}
}

func TestIdentityHTTPClientRejectsUnsafeOrInconsistentState(t *testing.T) {
	profile := mustParse(t, validProfileJSON)
	bundle, err := NewStateBundle("another-profile", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewIdentityHTTPClient(profile, bundle, HTTPClientOptions{}); err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("mismatched profile error = %v", err)
	}

	bundle, err = NewStateBundle(profile.ProfileID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bundle.Cookies = json.RawMessage(`[{"name":"partitioned","value":"x","domain":"example.test","partitionKey":{"topLevelSite":"https://example.test"}}]`)
	if _, err := NewIdentityHTTPClient(profile, bundle, HTTPClientOptions{}); err == nil || !strings.Contains(err.Error(), "partitioned") {
		t.Fatalf("partitioned cookie error = %v", err)
	}

	socks4 := mustParse(t, strings.Replace(validProfileJSON, "socks5://127.0.0.1:10808", "socks4://127.0.0.1:10808", 1))
	if _, err := NewIdentityHTTPClient(socks4, nil, HTTPClientOptions{}); err == nil || !strings.Contains(err.Error(), "SOCKS4") {
		t.Fatalf("SOCKS4 error = %v", err)
	}
}

func TestIdentityHTTPClientFullHintsAndRedirectDefault(t *testing.T) {
	profile := mustParse(t, validProfileJSON)
	profile.Network.ProxyURL = ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		if got, want := r.Header.Get("Sec-CH-UA-Full-Version"), `"152.0.7933.0"`; got != want {
			t.Errorf("full version hint = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte("final"))
	}))
	defer server.Close()

	client, err := NewIdentityHTTPClient(profile, nil, HTTPClientOptions{ClientHints: HTTPClientHintsFull})
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.NewRequest(context.Background(), http.MethodGet, server.URL+"/redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("default redirect status = %d, want 302", response.StatusCode)
	}
}

func TestIdentityHTTPClientFollowSameOriginRedirectRepairsIdentityHeaders(t *testing.T) {
	profile := mustParse(t, validProfileJSON)
	profile.Network.ProxyURL = ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("User-Agent"), profile.Emulation.UserAgent; got != want {
			t.Errorf("%s User-Agent = %q, want %q", r.URL.Path, got, want)
		}
		if got, want := r.Header.Get("Accept-Language"), "zh-CN,zh,en"; got != want {
			t.Errorf("%s Accept-Language = %q, want %q", r.URL.Path, got, want)
		}
		if got, want := r.Header.Get("Sec-CH-UA-Platform"), `"macOS"`; got != want {
			t.Errorf("%s Sec-CH-UA-Platform = %q, want %q", r.URL.Path, got, want)
		}
		switch r.URL.Path {
		case "/start":
			http.SetCookie(w, &http.Cookie{Name: "redirect-sid", Value: "after", Path: "/"})
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			for _, name := range []string{"bundle-sid", "redirect-sid"} {
				if cookie, err := r.Cookie(name); err != nil || cookie.Value == "" {
					t.Errorf("redirected request %s = %v, err=%v", name, cookie, err)
				}
			}
			_, _ = w.Write([]byte("followed"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	bundle, err := NewStateBundle(profile.ProfileID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bundle.Cookies, _ = json.Marshal([]map[string]any{{"name": "bundle-sid", "value": "before", "url": server.URL, "path": "/", "sameSite": "Lax"}})
	client, err := NewIdentityHTTPClient(profile, bundle, HTTPClientOptions{FollowRedirects: true})
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.NewRequest(context.Background(), http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "wrong-profile")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("followed response status = %d, want 200", response.StatusCode)
	}
	if body, _ := io.ReadAll(response.Body); string(body) != "followed" {
		t.Fatalf("response = %q", body)
	}
}

func TestIdentityHTTPClientRejectsCrossOriginRedirect(t *testing.T) {
	profile := mustParse(t, validProfileJSON)
	profile.Network.ProxyURL = ""
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("cross-origin target"))
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusFound)
	}))
	defer first.Close()
	client, err := NewIdentityHTTPClient(profile, nil, HTTPClientOptions{FollowRedirects: true})
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.NewRequest(context.Background(), http.MethodGet, first.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "跨 origin") {
		t.Fatalf("cross-origin redirect error = %v", err)
	}
}

func TestSameHTTPOriginNormalizesDefaultPorts(t *testing.T) {
	tests := []struct {
		name        string
		left, right string
		want        bool
	}{
		{name: "https default port", left: "https://EXAMPLE.test", right: "https://example.test:443", want: true},
		{name: "http default port", left: "http://example.test", right: "http://example.test:80", want: true},
		{name: "different explicit port", left: "https://example.test", right: "https://example.test:444", want: false},
		{name: "different scheme", left: "http://example.test", right: "https://example.test", want: false},
		{name: "different host", left: "https://example.test", right: "https://api.example.test", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			left, err := url.Parse(test.left)
			if err != nil {
				t.Fatal(err)
			}
			right, err := url.Parse(test.right)
			if err != nil {
				t.Fatal(err)
			}
			if got := sameHTTPOrigin(left, right); got != test.want {
				t.Fatalf("sameHTTPOrigin(%q, %q) = %v, want %v", test.left, test.right, got, test.want)
			}
		})
	}
}
