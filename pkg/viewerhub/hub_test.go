package viewerhub

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testSocketSeq int64

func newTestHub(t *testing.T, listen bool) (*Hub, string) {
	t.Helper()
	// Keep the path short: sockaddr_un.sun_path is capped at ~104 bytes and
	// t.TempDir() embeds the (long) test name.
	seq := atomic.AddInt64(&testSocketSeq, 1)
	path := filepath.Join(os.TempDir(), fmt.Sprintf("vh-%d-%d.sock", os.Getpid(), seq))
	hub := NewHub(path, WithLogger(t.Logf))
	if listen {
		// Some sandboxes refuse unix-socket bind (EPERM); the socket cases then
		// skip and are exercised for real in an unsandboxed run.
		if err := hub.Listen(); err != nil {
			t.Skipf("unix sockets unavailable in this environment: %v", err)
		}
	}
	t.Cleanup(func() { _ = hub.Close() })
	return hub, path
}

func readLines(t *testing.T, conn net.Conn, count int) []string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(conn)
	lines := make([]string, 0, count)
	for len(lines) < count {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read %d/%d lines: %v (got %v)", len(lines), count, err, lines)
		}
		lines = append(lines, strings.TrimRight(line, "\n"))
	}
	return lines
}

// One viewer must see every session, and the hub must remember the newest state
// per session so a viewer started later still paints immediately.
func TestPublishFansOutToEveryViewerAndRemembersLatest(t *testing.T) {
	hub, _ := newTestHub(t, false)
	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)

	hub.Publish("s-1", []byte("one\n"))
	hub.Publish("s-1", []byte("two\n"))
	hub.Publish("s-2", []byte("other\n"))

	if got := readLines(t, viewerSide, 3); got[0] != "one" || got[1] != "two" || got[2] != "other" {
		t.Fatalf("unexpected fan-out order: %v", got)
	}
	latest, ok := hub.Latest("s-1")
	if !ok || string(latest) != "two\n" {
		t.Fatalf("latest for s-1 = %q (ok=%v)", latest, ok)
	}
	if got := hub.SubscriberCount(); got != 1 {
		t.Fatalf("subscriber count = %d", got)
	}
}

// A viewer that stops reading must never slow the automation down: the queue is
// bounded and drops the oldest entries, keeping the newest frame.
func TestSlowViewerNeverBlocksPublish(t *testing.T) {
	hub, _ := newTestHub(t, false)
	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)

	published := make(chan struct{})
	go func() {
		defer close(published)
		for i := 0; i < 100; i++ {
			hub.Publish("s-1", []byte(fmt.Sprintf("frame-%d\n", i)))
		}
	}()
	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a viewer that stopped reading")
	}

	_ = viewerSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(viewerSide)
	seen := make([]string, 0, 16)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		seen = append(seen, strings.TrimRight(line, "\n"))
		if len(seen) > subscriberQueue+2 {
			break
		}
	}
	if len(seen) == 0 || len(seen) > subscriberQueue+1 {
		t.Fatalf("expected a bounded catch-up window, got %d lines: %v", len(seen), seen)
	}
	if last := seen[len(seen)-1]; last != "frame-99" {
		t.Fatalf("newest frame should survive the drop-oldest policy, got %q", last)
	}
}

func TestLateViewerReceivesTheLatestSnapshotOverASocket(t *testing.T) {
	hub, path := newTestHub(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Serve(ctx) }()

	hub.Publish("s-1", []byte("frame-a\n"))
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if got := readLines(t, conn, 1); got[0] != "frame-a" {
		t.Fatalf("late viewer did not receive the latest snapshot: %v", got)
	}
	hub.Publish("s-1", []byte("frame-b\n"))
	if got := readLines(t, conn, 1); got[0] != "frame-b" {
		t.Fatalf("viewer missed the live update: %v", got)
	}
}

// Two engines must not silently share one socket: the second one reports the
// conflict instead of stealing the path from the first.
func TestSecondHubRefusesToStealALiveSocket(t *testing.T) {
	hub, path := newTestHub(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Serve(ctx) }()
	// Serve binds asynchronously; wait until the path is live before probing.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub never accepted connections on %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}

	other := NewHub(path, WithLogger(t.Logf))
	if err := other.Listen(); err == nil {
		_ = other.Close()
		t.Fatal("second hub should refuse to bind a live socket")
	}
}

func TestCloseDropsViewersAndRemovesTheSocketFile(t *testing.T) {
	hub, path := newTestHub(t, true)
	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)

	if err := hub.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := hub.SubscriberCount(); got != 0 {
		t.Fatalf("subscribers after close = %d", got)
	}
	if _, err := net.Dial("unix", path); err == nil {
		t.Fatal("socket file should be gone after close")
	}
	// Publishing after close is a no-op, not a panic.
	hub.Publish("s-1", []byte("ignored\n"))
}
