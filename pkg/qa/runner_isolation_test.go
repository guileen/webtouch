package qa

import (
	"context"
	"strings"
	"testing"
)

// TestResolveAttachURL_IsolatedNeverProbes9222 守住「Hub 体检不得碰个人 Chrome」：
// 隔离模式下即使 9222 有人监听，也不探测、不附着。
func TestResolveAttachURL_IsolatedNeverProbes9222(t *testing.T) {
	orig := isPortListeningFn
	defer func() { isPortListeningFn = orig }()

	probes := 0
	isPortListeningFn = func(string) bool { probes++; return true }

	isolated := NewRunner(RunnerOptions{URL: "http://x", Isolated: true})
	if got := isolated.resolveAttachURL(); got != "" {
		t.Fatalf("isolated 模式不得附着外部 Chrome，got %q", got)
	}
	if probes != 0 {
		t.Fatalf("isolated 模式不应探测 9222，probes=%d", probes)
	}

	// 对照：非隔离模式仍保留「9222 有监听就附着」的既有行为（人工路径不改）。
	plain := NewRunner(RunnerOptions{URL: "http://x"})
	if got := plain.resolveAttachURL(); got != "http://127.0.0.1:9222" {
		t.Fatalf("非隔离模式应附着 9222，got %q", got)
	}
	if probes != 1 {
		t.Fatalf("非隔离模式应探测一次，probes=%d", probes)
	}
}

// TestCreateSession_IsolatedRejectsExplicitAttach 守住 fail-closed：
// 隔离与显式附着同时出现说明调用方在猜身份，必须报错而不是替它选。
func TestCreateSession_IsolatedRejectsExplicitAttach(t *testing.T) {
	r := NewRunner(RunnerOptions{URL: "http://x", Isolated: true, AttachCDP: "http://127.0.0.1:9222"})
	_, err := r.createSession(context.Background())
	if err == nil {
		t.Fatal("expected error for isolated + explicit attach")
	}
	if !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("unexpected error: %v", err)
	}
}
