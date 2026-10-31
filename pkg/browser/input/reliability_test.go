package input

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// 单元 2b（2026-09-29）：重试语义自宿主 `pkg/browser/input_reliability_test.go` 迁入。
// 宿主的伪 CDP 服务器换成这里的最小 `Caller` 假体：重试语义只依赖「Caller 会不会
// 挂起/报错」，不需要真 WebSocket 往返，也因此能直接断言「重试前后重放了什么」。

// withFastRetry 缩短本包的重试预算，避免单测被真实退避拖慢。
func withFastRetry(t *testing.T) {
	t.Helper()
	oldTimeout, oldAttempts := retryAttemptTimeout, retryMaxAttempts
	oldBackoff, oldReady := retryBackoff, retryReadinessTimeout
	retryAttemptTimeout = 150 * time.Millisecond
	retryMaxAttempts = 3
	retryBackoff = []time.Duration{10 * time.Millisecond}
	retryReadinessTimeout = 100 * time.Millisecond
	t.Setenv("UWS_INPUT_RETRY_ATTEMPTS", "") // 隔离宿主环境变量，单测只认包内变量
	t.Cleanup(func() {
		retryAttemptTimeout, retryMaxAttempts = oldTimeout, oldAttempts
		retryBackoff, retryReadinessTimeout = oldBackoff, oldReady
	})
}

// fakeCaller 是 `Caller` 的最小假体：可让输入命令「挂起不响应」（模拟被遮挡页门控）
// 或立即返回协议错误，其余命令一律成功。ready 模拟宿主会话是否已附着（未附着时
// 「输入就绪」重放应整体跳过）。
type fakeCaller struct {
	mu      sync.Mutex
	calls   []string
	params  []map[string]any
	hang    int    // 剩余「挂起不响应」次数（仅 Input.* 命令）
	failMsg string // 非空则对输入命令立即返回该错误
	ready   bool
}

func (f *fakeCaller) Ready() bool { return f.ready }

func (f *fakeCaller) Call(ctx context.Context, method string, params map[string]any, _ any) error {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.params = append(f.params, params)
	var hang bool
	if method == "Input.dispatchMouseEvent" || method == "Input.insertText" {
		if f.hang > 0 {
			f.hang--
			hang = true
		}
	}
	failMsg := f.failMsg
	f.mu.Unlock()

	if hang {
		<-ctx.Done() // 模拟被遮挡时的挂起：不响应，等客户端超时
		return ctx.Err()
	}
	if failMsg != "" && (method == "Input.dispatchMouseEvent" || method == "Input.insertText") {
		return errors.New(failMsg)
	}
	return nil
}

func (f *fakeCaller) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeCaller) paramsFor(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for i, m := range f.calls {
		if m == method {
			out = append(out, f.params[i])
		}
	}
	return out
}

func (f *fakeCaller) count(method string) int {
	n := 0
	for _, m := range f.snapshot() {
		if m == method {
			n++
		}
	}
	return n
}

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

// TestInputRetryRecoversFromOcclusionHang 覆盖 GLOBAL #92 主场景：命令挂起
// （被遮挡页不响应）→ 重放输入就绪（焦点仿真 + lifecycle active）→ 重试成功。
func TestInputRetryRecoversFromOcclusionHang(t *testing.T) {
	withFastRetry(t)
	fake := &fakeCaller{hang: 1, ready: true}
	guard := NewRetryGuard(fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := guard.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed"}); err != nil {
		t.Fatalf("挂起后应重试成功，got %v", err)
	}
	if got := guard.Retries(); got != 1 {
		t.Fatalf("重试计数 = %d，want 1", got)
	}
	calls := fake.snapshot()
	if fake.count("Input.dispatchMouseEvent") != 2 {
		t.Fatalf("应派发两次（首挂 + 重试成功）: %v", calls)
	}
	// 重试前必须先重放「输入就绪」，否则等于裸重试。
	if fake.count("Emulation.setFocusEmulationEnabled") == 0 ||
		fake.count("Page.setWebLifecycleState") == 0 {
		t.Fatalf("重试前应重放输入就绪: %v", calls)
	}
	idxHang := indexOf(calls, "Input.dispatchMouseEvent")
	idxEmul := indexOf(calls, "Emulation.setFocusEmulationEnabled")
	if idxEmul < idxHang {
		t.Fatalf("焦点仿真应在挂起之后重放: %v", calls)
	}
	for _, p := range fake.paramsFor("Emulation.setFocusEmulationEnabled") {
		if p["enabled"] != true {
			t.Fatalf("焦点仿真应显式 enabled=true: %v", p)
		}
	}
	// R1：整个恢复过程不得出现任何抬窗口命令。
	for _, bad := range []string{"Target.activateTarget", "Page.bringToFront"} {
		if fake.count(bad) != 0 {
			t.Fatalf("恢复流程不得调用 %s: %v", bad, calls)
		}
	}
}

// TestInputEnsuresReadinessOnlyWhenSessionAttached 未附着（无 session）时不做任何
// 「输入就绪」重放：这也是宿主 `bridgeCaller.Ready()` 这层能力面存在的理由。
func TestInputEnsuresReadinessOnlyWhenSessionAttached(t *testing.T) {
	withFastRetry(t)
	fake := &fakeCaller{ready: false} // 未附着
	guard := NewRetryGuard(fake)

	guard.EnsureInputReady()
	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("未附着时不应重放任何命令: %v", calls)
	}
}

// TestInputNonTimeoutErrorFailsFast 非超时错误（协议错误）不重试，原样上报。
func TestInputNonTimeoutErrorFailsFast(t *testing.T) {
	withFastRetry(t)
	fake := &fakeCaller{failMsg: "boom", ready: true}
	guard := NewRetryGuard(fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := guard.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed"})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("非超时错误应原样上报，got %v", err)
	}
	if guard.Retries() != 0 || fake.count("Input.dispatchMouseEvent") != 1 {
		t.Fatalf("非超时错误不应重试: retries=%d calls=%d", guard.Retries(), fake.count("Input.dispatchMouseEvent"))
	}
}

// TestInputRetryExhaustedFailsLoud 重试耗尽仍失败：错误带方法名、不静默。
func TestInputRetryExhaustedFailsLoud(t *testing.T) {
	withFastRetry(t)
	fake := &fakeCaller{hang: 99, ready: true}
	guard := NewRetryGuard(fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := guard.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed"})
	if err == nil {
		t.Fatal("全文挂起时必须上报失败，不能静默")
	}
	if !IsTimeoutLike(err) {
		t.Fatalf("错误应保留超时语义，got %v", err)
	}
	if got := fake.count("Input.dispatchMouseEvent"); got != retryMaxAttempts {
		t.Fatalf("尝试次数 = %d，want %d", got, retryMaxAttempts)
	}
	if got := guard.Retries(); got != retryMaxAttempts-1 {
		t.Fatalf("重试计数 = %d，want %d", got, retryMaxAttempts-1)
	}
}

// TestInputRetryDisabledBySingleAttempt 尝试次数=1 时退化为原行为（不重试）。
func TestInputRetryDisabledBySingleAttempt(t *testing.T) {
	withFastRetry(t)
	retryMaxAttempts = 1
	fake := &fakeCaller{hang: 1, ready: true}
	guard := NewRetryGuard(fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := guard.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed"}); err == nil {
		t.Fatal("单次尝试挂起应失败")
	}
	if guard.Retries() != 0 {
		t.Fatalf("单次尝试不应有重试计数: %d", guard.Retries())
	}
}

// TestAttemptsFromEnv 环境变量只认 >=1 的整数，其余回落包内默认。
func TestAttemptsFromEnv(t *testing.T) {
	t.Setenv("UWS_INPUT_RETRY_ATTEMPTS", "")
	if got := AttemptsFromEnv(); got != retryMaxAttempts {
		t.Fatalf("未设置时 = %d，want %d", got, retryMaxAttempts)
	}
	t.Setenv("UWS_INPUT_RETRY_ATTEMPTS", "5")
	if got := AttemptsFromEnv(); got != 5 {
		t.Fatalf("显式设置时 = %d，want 5", got)
	}
	for _, bad := range []string{"0", "-3", "abc"} {
		t.Setenv("UWS_INPUT_RETRY_ATTEMPTS", bad)
		if got := AttemptsFromEnv(); got != retryMaxAttempts {
			t.Fatalf("非法值 %q 应回落默认，got %d", bad, got)
		}
	}
}
