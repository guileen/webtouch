// 异步动作验证器（SPA 软路由时序状态机）。
//
// 语义对齐 spec/algorithm-kernel.md §5/§6.2 与 docs/tdd-checklist.md §3.3：
//
//	STABLE --pushState--> NAVIGATING --Network Idle 500ms 且无新增 DOM 突变--> NAVIGATED
//	总超时 1500ms 未稳 -> DEADLINE_EXCEEDED
//
// 2026-08-28（P4）：CSS 过渡/轮播会让 DOM 持续突变（永不"静息"），导致本应可用的页面
// 被误判 DEADLINE（进而触发无谓重试/ VLM 兜底）。修正：超时到达时只要**网络已空闲**
// （activeConns < 2，说明页面加载/拉数据已完成）即容忍为 NAVIGATED；仅当网络仍忙
// （真正在加载/拉数据）才保留 DEADLINE。
//
// 本包自包含：不依赖 CDP/浏览器实现，引擎侧只需喂入事件；
// 时钟可注入，保证涉时用例确定性（tdd-checklist §0.5）。
package verifier

import (
	"time"
)

// State 是异步验证器的上下文状态（对齐 context_state 的 STABLE/NAVIGATING/NAVIGATED 子集）。
type State int

const (
	// StateStable 表示页面稳定，可执行新动作。
	StateStable State = iota
	// StateNavigating 表示 SPA 软路由已捕获，等待 Network Idle 与 DOM 静息。
	StateNavigating
	// StateNavigated 表示导航完成，页面稳定。
	StateNavigated
	// StateDeadlineExceeded 表示等待超时（1500ms）未稳定。
	StateDeadlineExceeded
)

func (s State) String() string {
	switch s {
	case StateStable:
		return "STABLE"
	case StateNavigating:
		return "NAVIGATING"
	case StateNavigated:
		return "NAVIGATED"
	case StateDeadlineExceeded:
		return "DEADLINE_EXCEEDED"
	default:
		return "UNKNOWN"
	}
}

// 冻结阈值（spec/algorithm-kernel.md §5 / tdd-checklist 附录 A）。
const (
	// IdleWindow 是 Network Idle 的静息窗口：活动连接 <2 且无新增 DOM 突变持续该时长。
	IdleWindow = 500 * time.Millisecond
	// MutationDebounce 是 DOM 突变防抖窗口：窗口内重复突变只记录一次。
	MutationDebounce = 150 * time.Millisecond
	// NavTimeout 是 NAVIGATING 后的总超时：超时未稳返回 DEADLINE_EXCEEDED。
	NavTimeout = 1500 * time.Millisecond
)

// Clock 抽象时间源，测试注入固定时钟保证确定性（tdd-checklist §0.5）。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Verifier 是 SPA 软路由异步验证器的自包含状态机。
type Verifier struct {
	clock Clock

	state        State
	navStartedAt time.Time
	activeConns  int
	lastMutation time.Time
	idleSince    time.Time
}

// NewVerifier 构造验证器；clock 为空时使用真实时钟。
func NewVerifier(clock Clock) *Verifier {
	if clock == nil {
		clock = realClock{}
	}
	return &Verifier{clock: clock, state: StateStable}
}

// FeedPushState 上报 SPA 软路由捕获（History.navigated / window.pushState）。
// STABLE 下必须立即切换 NAVIGATING（TDD-ENG-06）；重复事件不重置导航起点。
func (v *Verifier) FeedPushState() State {
	if v.state == StateStable {
		v.state = StateNavigating
		v.navStartedAt = v.clock.Now()
		v.activeConns = 0
		v.lastMutation = time.Time{}
		v.idleSince = time.Time{}
	}
	return v.state
}

// FeedNetwork 上报活动连接数（XHR/Fetch）。
// 连接数 >=2 时静息计时作废；回落到 <2 时立即开始累计静息窗口。
func (v *Verifier) FeedNetwork(active int) State {
	if v.state != StateNavigating {
		return v.state
	}
	if active < 0 {
		active = 0
	}
	v.activeConns = active
	now := v.clock.Now()
	if active >= 2 {
		v.idleSince = time.Time{}
	} else if v.idleSince.IsZero() {
		v.idleSince = now
	}
	return v.state
}

// FeedDOMMutation 上报 DOM 突变。
// 防抖窗口（150ms）内的重复突变只记录一次；任何新突变都会作废已累计的静息窗口。
func (v *Verifier) FeedDOMMutation() State {
	if v.state != StateNavigating {
		return v.state
	}
	now := v.clock.Now()
	if v.lastMutation.IsZero() || now.Sub(v.lastMutation) >= MutationDebounce {
		v.lastMutation = now
		v.idleSince = time.Time{}
	}
	return v.state
}

// Advance 用当前时钟推进状态机并返回最新状态。
// NAVIGATING 下：活动连接 <2 且 DOM 突变防抖已过时开始累计静息窗口；
// 静息满 IdleWindow -> NAVIGATED；总超时 NavTimeout 未稳 -> DEADLINE_EXCEEDED。
func (v *Verifier) Advance() State {
	if v.state != StateNavigating {
		return v.state
	}
	now := v.clock.Now()
	if v.activeConns < 2 && v.idleSince.IsZero() {
		if v.lastMutation.IsZero() || now.Sub(v.lastMutation) >= MutationDebounce {
			v.idleSince = now
		}
	}
	if !v.idleSince.IsZero() && now.Sub(v.idleSince) >= IdleWindow {
		v.state = StateNavigated
		return v.state
	}
	if now.Sub(v.navStartedAt) >= NavTimeout {
		// P4 容忍：网络空闲 + 仍在突变（动画/轮播）→ 视为"已加载好"，转 NAVIGATED，
		// 不再报 DEADLINE（避免无谓重试/VLM 兜底）。网络仍忙才保留 DEADLINE。
		if v.activeConns < 2 {
			v.state = StateNavigated
		} else {
			v.state = StateDeadlineExceeded
		}
	}
	return v.state
}

// Snapshot 返回当前状态（不改写状态机）。
func (v *Verifier) Snapshot() State { return v.state }
