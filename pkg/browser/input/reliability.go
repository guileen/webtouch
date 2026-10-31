// 物理输入「必达」护栏（2026-09-12；对应 GLOBAL #92 输入挂起盲区）
//
// 现象（真实 Chrome 实测，同日 4 跑 1 过 3 败）：专属小窗
// visibilityState=visible、hasFocus=false，但只要它被用户的前台窗口完全遮挡，
// Chrome 就把它当后台/被遮挡页处理，物理输入会挂起到调用超时
// （`cdp: Input.dispatchMouseEvent: context deadline exceeded`）。
//
// R1（自动化绝不打断用户前台工作）禁止用 `Target.activateTarget` /
// `Page.bringToFront` 这类「抬窗口」手段恢复，因此这里只用两条**不改窗口层级、
// 不动用户前台应用**的护栏：
//
//  1. 焦点仿真：`Emulation.setFocusEmulationEnabled{enabled:true}` —— 让渲染进程
//     认为页面处于焦点中（只影响页面内的 hasFocus 判定与输入分发门控）；
//  2. 超时重试：每次物理输入调用带独立预算，超时后重放一次「输入就绪」
//     （焦点仿真 + 尽力把页面生命周期拉回 active）再退避重试。
//
// 失败语义不变：重试耗尽后仍返回原错误（含方法名），由既有 ExecuteAction
// 错误串上报，绝不静默吞掉、也绝不以抢焦点换成功。
//
// 复杂度治理·阶段 2·单元 2b（2026-09-29）：整文件自宿主 `pkg/browser/input_reliability.go`
// 迁入，落为 `RetryGuard`（宿主经 `Caller` 注入）；重试计数随本包走，宿主只留
// `inputGuard()` 访问器与 `bridgeCaller` 适配器。`AttemptTimeout`/`AttemptsFromEnv`/
// `BackoffFor`/`IsTimeoutLike` 四项原语导出，供宿主 `shared_window.go` 的锚页中键
// 重试复用（该簇搬迁时随其一起走）。
package input

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// retryAttemptTimeout 是单次物理输入调用的独立预算。取小于调用方动作预算
	// （点击/输入动作 8s）的值，给重试留出空间，同时让「挂起」比原来更快暴露。
	retryAttemptTimeout = 2 * time.Second
	// retryMaxAttempts 是单条输入命令的总尝试次数（含首次）。
	retryMaxAttempts = 3
	// retryBackoff 是重试之间的退避，长度不足时按最后一档兜底。
	retryBackoff = []time.Duration{150 * time.Millisecond, 400 * time.Millisecond}
	// retryReadinessTimeout 是「输入就绪」重放命令的独立预算，不占输入主预算。
	retryReadinessTimeout = 800 * time.Millisecond
)

// Caller 是宿主会话向本护栏暴露的最小能力面：把一条 CDP 命令发到本会话的 target
// session。Ready 报告会话是否已具备下发条件（未附着/尚无 session 时跳过「输入就绪」重放，
// 与原 `sessionID == ""` 早退等价）。
type Caller interface {
	Ready() bool
	Call(ctx context.Context, method string, params map[string]any, out any) error
}

// RetryGuard 持有会话级重试计数并执行「必达」重试（每个会话一个）。
type RetryGuard struct {
	caller  Caller
	mu      sync.Mutex
	retries int
}

// NewRetryGuard 用宿主注入的能力面建一个护栏。
func NewRetryGuard(c Caller) *RetryGuard { return &RetryGuard{caller: c} }

// FocusEmulationEnabled 读 `UWS_FOCUS_EMULATION`（默认开）：=0 关闭护栏 1，
// 仅用于取证对照（复现「遮挡即挂起」的原始形态），不是给日常使用调的开关。
func FocusEmulationEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("UWS_FOCUS_EMULATION"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// AttemptsFromEnv 允许用 `UWS_INPUT_RETRY_ATTEMPTS=1` 关掉重试：
// 取证/排障时用来复现「遮挡即挂起」的原始形态（见 docs/evidence/README.md）。
func AttemptsFromEnv() int {
	n := retryMaxAttempts
	if raw := strings.TrimSpace(os.Getenv("UWS_INPUT_RETRY_ATTEMPTS")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 1 {
			n = v
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

// IsTimeoutLike 判断错误是否属于「输入被后台/遮挡挂起」形态（超时类）。
// 其它错误（协议错误、target 关闭等）立即上报，不做无意义重试。
func IsTimeoutLike(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return strings.Contains(err.Error(), "context deadline exceeded")
}

// AttemptTimeout 是单次物理输入调用的独立预算（本包重试与宿主锚页重试共用同一口径）。
func AttemptTimeout() time.Duration { return retryAttemptTimeout }

// ReadinessTimeout 是「输入就绪」重放的独立预算（宿主锚页重放护栏复用同一口径）。
func ReadinessTimeout() time.Duration { return retryReadinessTimeout }

// BackoffFor 取第 i 档退避（越界时用最后一档）。
func BackoffFor(i int) time.Duration {
	if len(retryBackoff) == 0 {
		return 0
	}
	if i >= len(retryBackoff) {
		i = len(retryBackoff) - 1
	}
	return retryBackoff[i]
}

// EnableFocusEmulation 打开/重放焦点仿真（幂等命令，可重复下发）。
func (g *RetryGuard) EnableFocusEmulation(ctx context.Context) error {
	return g.caller.Call(ctx, "Emulation.setFocusEmulationEnabled",
		map[string]any{"enabled": true}, nil)
}

// EnsureInputReady 在重试前重放「输入就绪」状态：焦点仿真 + 尽力把页面生命周期
// 拉回 active（后者失败不影响主流程）。全部为尽力而为，不返回错误。
func (g *RetryGuard) EnsureInputReady() {
	if !g.caller.Ready() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), retryReadinessTimeout)
	defer cancel()
	_ = g.EnableFocusEmulation(ctx)
	_ = g.caller.Call(ctx, "Page.setWebLifecycleState", map[string]any{"state": "active"}, nil)
}

// Retries 返回本会话累计的输入重试次数（供测试与证据核对）。
func (g *RetryGuard) Retries() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.retries
}

// Call 执行一条物理输入命令；超时（页面被后台/遮挡挂起）时重放输入就绪并退避重试，
// 其它错误立即返回。参数 params 只读，可在重试间复用。
func (g *RetryGuard) Call(ctx context.Context, method string, params map[string]any) error {
	attempts := AttemptsFromEnv()
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			g.EnsureInputReady()
			if d := BackoffFor(attempt - 1); d > 0 {
				timer := time.NewTimer(d)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return lastErr
				}
			}
			g.bumpRetry()
		}
		callCtx, cancel := context.WithTimeout(ctx, retryAttemptTimeout)
		err := g.caller.Call(callCtx, method, params, nil)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if !IsTimeoutLike(err) {
			return err
		}
		if ctx.Err() != nil { // 外层预算已耗尽，重试没有空间
			return lastErr
		}
	}
	return lastErr
}

// bumpRetry 记录一次重试（供测试与证据核对）。
func (g *RetryGuard) bumpRetry() {
	g.mu.Lock()
	g.retries++
	g.mu.Unlock()
}
