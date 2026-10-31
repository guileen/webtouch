package fingerprint

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestLeaseExclusiveRenewAndRelease(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	store, err := OpenLeaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return now }
	first, err := store.Acquire("profile-a", "machine-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire("profile-a", "machine-b", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("concurrent acquire error = %v, want ErrLeaseHeld", err)
	}
	now = now.Add(30 * time.Second)
	renewed, err := store.Renew(first, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.ExpiresAt.Equal(now.Add(2*time.Minute)) || renewed.Token != first.Token {
		t.Fatalf("renewed lease = %#v", renewed)
	}
	if err := store.Release(first); err != nil {
		t.Fatal(err)
	}
	second, err := store.Acquire("profile-a", "machine-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.HolderID != "machine-b" {
		t.Fatalf("holder = %q", second.HolderID)
	}
	if err := store.Release(first); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old lease release = %v, want ErrLeaseLost", err)
	}
}

func TestLeaseExpiredCanBeTakenButCorruptionFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	store, err := OpenLeaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return now }
	if _, err := store.Acquire("profile-b", "machine-a", time.Second); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if _, err := store.Acquire("profile-b", "machine-b", time.Minute); err != nil {
		t.Fatalf("expired lease should be reclaimable: %v", err)
	}
	path, _, err := store.paths("profile-c")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire("profile-c", "machine-a", time.Minute); err == nil {
		t.Fatal("corrupted existing lease must fail closed")
	}
}

func TestHTTPLeaseBackendPreservesExclusiveTokenContract(t *testing.T) {
	store, err := OpenLeaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLeaseHTTPHandler(LeaseHTTPHandlerConfig{Store: store, BearerToken: "lease-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewHTTPLeaseBackend("https://lease.example", "lease-test-token", &http.Client{Transport: handlerRoundTripper{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Acquire("remote-profile", "node-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Acquire("remote-profile", "node-b", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second acquire = %v, want ErrLeaseHeld", err)
	}
	renewed, err := client.Renew(first, time.Minute)
	if err != nil || renewed.Token != first.Token {
		t.Fatalf("renew = %+v, %v", renewed, err)
	}
	if err := client.Release(renewed); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Acquire("remote-profile", "node-b", time.Minute); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "https://lease.example/v1/leases/remote-profile/acquire", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
}

// The in-memory RoundTripper above covers handler semantics. This test also
// crosses a real local TLS listener, proving the remote-client shape does not
// secretly depend on cleartext loopback allowances or an in-process transport.
func TestHTTPLeaseBackendUsesTLSRemoteEndpoint(t *testing.T) {
	store, err := OpenLeaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLeaseHTTPHandler(LeaseHTTPHandlerConfig{Store: store, BearerToken: "tls-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	client, err := NewHTTPLeaseBackend(server.URL, "tls-test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Acquire("tls-profile", "node-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Acquire("tls-profile", "node-b", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second TLS acquire = %v, want ErrLeaseHeld", err)
	}
	if err := client.Release(first); err != nil {
		t.Fatal(err)
	}
}

type handlerRoundTripper struct{ handler http.Handler }

func (t handlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, req)
	response := recorder.Result()
	response.Request = req
	return response, nil
}
