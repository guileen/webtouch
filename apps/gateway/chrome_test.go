package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeChromeReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"webSocketDebuggerUrl": "ws://127.0.0.1:9222/devtools/browser/x",
		})
	}))
	defer srv.Close()
	if err := probeChrome(context.Background(), srv.URL); err != nil {
		t.Fatalf("就绪端点应探测成功: %v", err)
	}
}

func TestProbeChromeNotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if err := probeChrome(context.Background(), srv.URL); err == nil {
		t.Fatal("非就绪端点应探测失败")
	}
}

func TestChromeLaunchArgs(t *testing.T) {
	args := chromeLaunchArgs("9222", "/tmp/uws-profile", "")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--remote-debugging-port=9222",
		"--remote-allow-origins=*",
		"--user-data-dir=/tmp/uws-profile",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
		"about:blank",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q 缺 %q", joined, want)
		}
	}
	// 冷启动小窗（2026-09-11）：默认小窗尺寸；UWS_WINDOW_POS 显式时带上坐标。
	args = chromeLaunchArgs("9223", "/tmp/p2", "https://gemini.google.com")
	if !strings.Contains(strings.Join(args, " "), "--window-size=1440,900") {
		t.Fatalf("默认应带小窗尺寸: %v", args)
	}
	t.Setenv("UWS_WINDOW_SIZE", "390x844")
	t.Setenv("UWS_WINDOW_POS", "1200,24")
	if joined := strings.Join(chromeLaunchArgs("9224", "/tmp/p3", ""), " "); !strings.Contains(joined, "--window-size=390,844") ||
		!strings.Contains(joined, "--window-position=1200,24") {
		t.Fatalf("小窗参数未生效: %s", joined)
	}
	if !strings.Contains(strings.Join(args, " "), "https://gemini.google.com") {
		t.Fatalf("应保留初始 URL: %v", args)
	}
}

func TestEnsureChromeReuse(t *testing.T) {
	// 端点已就绪 → 直接复用，不尝试拉起新进程。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"webSocketDebuggerUrl": "ws://x",
		})
	}))
	defer srv.Close()
	if err := EnsureChrome(context.Background(), srv.URL,
		filepath.Join(t.TempDir(), "profile"), nil); err != nil {
		t.Fatalf("EnsureChrome(reuse): %v", err)
	}
}

func TestEnsureChromeUnreachableFailsClosedWithoutLaunchPermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv(ChromeLaunchPermissionEnv, "")

	called := false
	oldStart := startChromeProcess
	startChromeProcess = func(string, []string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { startChromeProcess = oldStart })

	err := EnsureChrome(context.Background(), srv.URL, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), ChromeLaunchPermissionEnv+"=1") {
		t.Fatalf("未授权的不可达端点应给明确门禁错误，got %v", err)
	}
	if called {
		t.Fatal("未授权路径不得尝试启动 Chrome")
	}
}

func TestEnsureChromeAuthorizedAttemptsExistingLaunchPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv(ChromeLaunchPermissionEnv, "true")
	t.Setenv("UWS_CHROME_BIN", "/test/chrome")

	called := false
	oldStart := startChromeProcess
	startChromeProcess = func(bin string, args []string) error {
		called = true
		if bin != "/test/chrome" || !strings.Contains(strings.Join(args, " "), "--remote-debugging-port=") {
			t.Fatalf("未使用既有 Chrome 启动参数: bin=%q args=%v", bin, args)
		}
		return errors.New("stub launch")
	}
	t.Cleanup(func() { startChromeProcess = oldStart })

	err := EnsureChrome(context.Background(), srv.URL, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "启动调试 Chrome 失败") {
		t.Fatalf("显式授权应走原有启动分支，got %v", err)
	}
	if !called {
		t.Fatal("显式授权应尝试启动 Chrome")
	}
}

func TestEnsureChromeMissingPort(t *testing.T) {
	if err := EnsureChrome(context.Background(), "http://127.0.0.1", "", nil); err == nil {
		t.Fatal("缺调试端口应报错")
	}
}

func TestEnsureChromeEmptyAttach(t *testing.T) {
	if err := EnsureChrome(context.Background(), "", "", nil); err != nil {
		t.Fatalf("空 attach-cdp 应直接返回: %v", err)
	}
}
