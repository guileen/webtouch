package browser

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"uws/pkg/fingerprint"
)

func TestLeaseFactoryKeepsSingleWriterAcrossSessionReplacement(t *testing.T) {
	store, err := fingerprint.OpenLeaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	factory := &LeaseFactory{Factory: &mockFactory{}, Store: store, HolderID: "node-a", TTL: 500 * time.Millisecond}
	first, err := factory.Create(context.Background(), Config{ProfileID: "lease-profile"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := factory.Create(context.Background(), Config{ProfileID: "lease-profile"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire("lease-profile", "node-b", time.Second); !errors.Is(err, fingerprint.ErrLeaseHeld) {
		t.Fatalf("second holder acquire = %v, want ErrLeaseHeld", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire("lease-profile", "node-b", time.Second); !errors.Is(err, fingerprint.ErrLeaseHeld) {
		t.Fatalf("replacement must retain lease, got %v", err)
	}
	// Heartbeat must extend the short TTL while the replacement session lives.
	time.Sleep(700 * time.Millisecond)
	if _, err := store.Acquire("lease-profile", "node-b", time.Second); !errors.Is(err, fingerprint.ErrLeaseHeld) {
		t.Fatalf("renewed lease must remain exclusive, got %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if lease, err := store.Acquire("lease-profile", "node-b", time.Second); err != nil {
		t.Fatalf("lease must release after final close: %v", err)
	} else if err := store.Release(lease); err != nil {
		t.Fatal(err)
	}
}

// This is the two-node deployment shape: each engine has its own factory and
// HTTP client, while the lease authority is shared. It verifies the runtime
// wrapper, rather than only the HTTP handler's token semantics.
func TestLeaseFactoryRejectsSecondRemoteNode(t *testing.T) {
	store, err := fingerprint.OpenLeaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := fingerprint.NewLeaseHTTPHandler(fingerprint.LeaseHTTPHandlerConfig{Store: store, BearerToken: "remote-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	backendA, err := fingerprint.NewHTTPLeaseBackend(server.URL, "remote-test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	backendB, err := fingerprint.NewHTTPLeaseBackend(server.URL, "remote-test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	nodeA := &LeaseFactory{Factory: &mockFactory{}, Store: backendA, HolderID: "node-a", TTL: time.Second}
	nodeB := &LeaseFactory{Factory: &mockFactory{}, Store: backendB, HolderID: "node-b", TTL: time.Second}
	first, err := nodeA.Create(context.Background(), Config{ProfileID: "remote-profile"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodeB.Create(context.Background(), Config{ProfileID: "remote-profile"}); !errors.Is(err, fingerprint.ErrLeaseHeld) {
		t.Fatalf("second remote node create = %v, want ErrLeaseHeld", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := nodeB.Create(context.Background(), Config{ProfileID: "remote-profile"})
	if err != nil {
		t.Fatalf("remote node must acquire after release: %v", err)
	}
	defer second.Close()
}
