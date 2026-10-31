package gateway

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestHost 构造可注入的宿主守护者（生产 probe/launch 换成假实现）。
func newTestHost(probeErr error, launchErr error) (*BrowserHost, *int32, *int32) {
	var probeCalls, launchCalls int32
	h := NewBrowserHost("http://127.0.0.1:9222", "/tmp/test-profile", time.Millisecond, log.New(&strings.Builder{}, "", 0))
	h.probe = func(context.Context, string) error {
		atomic.AddInt32(&probeCalls, 1)
		return probeErr
	}
	h.launch = func(context.Context, string, string, *log.Logger) error {
		atomic.AddInt32(&launchCalls, 1)
		return launchErr
	}
	return h, &probeCalls, &launchCalls
}

// 宿主失联 → 自动拉起一次，状态转为已附着（T-「关窗 ≠ 断链路」核心用例）。
func TestBrowserHostRelaunchOnProbeFailure(t *testing.T) {
	t.Setenv(ChromeLaunchPermissionEnv, "1")
	var up atomic.Bool
	h, _, launches := newTestHost(errors.New("connection refused"), nil)
	h.probe = func(context.Context, string) error {
		if up.Load() {
			return nil
		}
		return errors.New("connection refused")
	}
	h.launch = func(context.Context, string, string, *log.Logger) error {
		atomic.AddInt32(launches, 1)
		up.Store(true)
		return nil
	}
	h.tick(context.Background())
	if got := atomic.LoadInt32(launches); got != 1 {
		t.Fatalf("拉起次数 = %d, want 1", got)
	}
	st := h.Snapshot()
	if !st.Attached || st.Relaunches != 1 {
		t.Fatalf("状态 = %+v, want attached=true relaunches=1", st)
	}
	if st.DownSince != "" || st.LastError != "" {
		t.Fatalf("恢复后应清空失联标记: %+v", st)
	}
	// 恢复后继续探活不再拉起。
	h.tick(context.Background())
	if got := atomic.LoadInt32(launches); got != 1 {
		t.Fatalf("恢复后不应再拉起，拉起次数 = %d", got)
	}
}

// 未授权时 fail-closed：记录失联但不启动任何 Chrome 进程。
func TestBrowserHostNoLaunchWithoutPermission(t *testing.T) {
	t.Setenv(ChromeLaunchPermissionEnv, "")
	h, _, launches := newTestHost(errors.New("boom"), nil)
	h.tick(context.Background())
	if got := atomic.LoadInt32(launches); got != 0 {
		t.Fatalf("未授权不应拉起，拉起次数 = %d", got)
	}
	st := h.Snapshot()
	if st.Attached || st.DownSince == "" || st.LastError == "" {
		t.Fatalf("应记录失联态: %+v", st)
	}
}

// 拉起失败 → 记录错误、保持失联；冷却期内不重复拉起。
func TestBrowserHostRelaunchFailureAndCooldown(t *testing.T) {
	t.Setenv(ChromeLaunchPermissionEnv, "1")
	h, _, launches := newTestHost(errors.New("boom"), errors.New("chrome 启动失败"))
	h.tick(context.Background())
	if got := atomic.LoadInt32(launches); got != 1 {
		t.Fatalf("首个 tick 应尝试拉起一次，期望 1，实际 %d", got)
	}
	h.tick(context.Background())
	if got := atomic.LoadInt32(launches); got != 1 {
		t.Fatalf("冷却期内不应重复拉起，拉起次数 = %d", got)
	}
	h.mu.Lock()
	st := h.st
	h.mu.Unlock()
	if !strings.Contains(st.LaunchError, "chrome 启动失败") {
		t.Fatalf("应记录拉起失败原因: %+v", st)
	}
}

// 未纳管（attach-cdp 为空）= 空操作：既不探活也不拉起。
func TestBrowserHostUnmanagedIsNoop(t *testing.T) {
	h := NewBrowserHost("", "", time.Millisecond, log.Default())
	called := false
	h.probe = func(context.Context, string) error { called = true; return nil }
	if h.Managed() {
		t.Fatal("空端点不应纳管")
	}
	h.tick(context.Background())
	if called {
		t.Fatal("未纳管不应探活")
	}
	if st := h.Snapshot(); st.Managed {
		t.Fatalf("未纳管快照 managed 应为 false: %+v", st)
	}
}

// 探活周期环境变量：非法/缺省回落默认，正整数生效。
func TestBrowserHostIntervalFromEnv(t *testing.T) {
	t.Setenv("UWS_BROWSER_HOST_INTERVAL", "")
	if got := BrowserHostIntervalFromEnv(); got != DefaultBrowserHostInterval {
		t.Fatalf("默认周期 = %v", got)
	}
	t.Setenv("UWS_BROWSER_HOST_INTERVAL", "7")
	if got := BrowserHostIntervalFromEnv(); got != 7*time.Second {
		t.Fatalf("周期 = %v, want 7s", got)
	}
	t.Setenv("UWS_BROWSER_HOST_INTERVAL", "abc")
	if got := BrowserHostIntervalFromEnv(); got != DefaultBrowserHostInterval {
		t.Fatalf("非法值应回落默认，got %v", got)
	}
}

// 端到端就绪：无成功推理记录 ⇒ ready=false；记录成功后 ⇒ ready=true。
func TestReadinessRequiresRealInference(t *testing.T) {
	g := &Gateway{}
	ready, lastOK, lastErr, _ := g.Readiness(time.Minute)
	if ready || lastOK != "" || lastErr != "" {
		t.Fatalf("初始应不可用: ready=%v lastOK=%q lastErr=%q", ready, lastOK, lastErr)
	}
	g.RecordInferenceOK(1200 * time.Millisecond)
	ready, lastOK, _, ms := g.Readiness(time.Minute)
	if !ready || lastOK == "" || ms != 1200 {
		t.Fatalf("成功推理后应可用: ready=%v lastOK=%q ms=%d", ready, lastOK, ms)
	}
	g.RecordInferenceErr(errors.New("500 游客账号模型被锁定"))
	ready, _, lastErr, _ = g.Readiness(time.Minute)
	if ready || !strings.Contains(lastErr, "游客账号") {
		t.Fatalf("失败后应转不可用并留因: ready=%v lastErr=%q", ready, lastErr)
	}
}
