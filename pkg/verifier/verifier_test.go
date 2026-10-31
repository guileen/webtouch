// TDD-ENG-06/07（tdd-checklist.md §3.3，最核心的 SPA 时序机制）。
// 与 tests/integration/test_engine.py 双层验证；本文件为 Go 逻辑层实现。
package verifier

import (
	"testing"
	"time"
)

// fakeClock 提供固定/可推进时钟，保证涉时用例确定性（tdd-checklist §0.5）。
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func TestEng06SpaPushStateNavigating(t *testing.T) {
	clock := newFakeClock()
	v := NewVerifier(clock)

	if got := v.Snapshot(); got != StateStable {
		t.Fatalf("初始状态 = %s, want STABLE", got)
	}
	if got := v.FeedPushState(); got != StateNavigating {
		t.Fatalf("pushState 后状态 = %s, want NAVIGATING", got)
	}
	// 绝对禁止在 pushState 触发时立即判定完成（algorithm-kernel §6.2）。
	if got := v.Advance(); got != StateNavigating {
		t.Fatalf("pushState 后立即 Advance = %s, want NAVIGATING", got)
	}
}

func TestEng07NetworkIdleStableTransition(t *testing.T) {
	clock := newFakeClock()
	v := NewVerifier(clock)
	v.FeedPushState()
	v.FeedNetwork(0) // 活动连接 <2，开始静息

	clock.advance(499 * time.Millisecond)
	if got := v.Advance(); got != StateNavigating {
		t.Fatalf("静息 499ms 状态 = %s, want NAVIGATING", got)
	}
	clock.advance(1 * time.Millisecond)
	if got := v.Advance(); got != StateNavigated {
		t.Fatalf("静息 500ms 状态 = %s, want NAVIGATED", got)
	}
}

func TestEng07BusyConnectionsBlockStable(t *testing.T) {
	clock := newFakeClock()
	v := NewVerifier(clock)
	v.FeedPushState()
	v.FeedNetwork(3) // 未完结 XHR/Fetch，禁止转稳定态

	clock.advance(600 * time.Millisecond)
	if got := v.Advance(); got == StateNavigated {
		t.Fatal("活动连接 >=2 时不得转 NAVIGATED")
	}
	// 连接回落 <2 后重新累计静息窗口
	v.FeedNetwork(1)
	clock.advance(499 * time.Millisecond)
	if got := v.Advance(); got != StateNavigating {
		t.Fatalf("回落后 499ms 状态 = %s, want NAVIGATING", got)
	}
	clock.advance(1 * time.Millisecond)
	if got := v.Advance(); got != StateNavigated {
		t.Fatalf("回落后 500ms 状态 = %s, want NAVIGATED", got)
	}
}

func TestEng07DomMutationResetsIdleWindow(t *testing.T) {
	clock := newFakeClock()
	v := NewVerifier(clock)
	v.FeedPushState()
	v.FeedNetwork(0)
	clock.advance(300 * time.Millisecond)
	v.FeedDOMMutation() // 新增 DOM 突变：静息计时作废

	// 防抖 150ms 内不开始静息
	clock.advance(149 * time.Millisecond)
	if got := v.Advance(); got != StateNavigating {
		t.Fatalf("突变后防抖未过状态 = %s, want NAVIGATING", got)
	}
	// 防抖已过（t0+450），本次 Advance 建立静息起点，从零累计 500ms
	clock.advance(1 * time.Millisecond)
	if got := v.Advance(); got != StateNavigating {
		t.Fatalf("突变后防抖已过状态 = %s, want NAVIGATING", got)
	}
	clock.advance(499 * time.Millisecond)
	if got := v.Advance(); got != StateNavigating {
		t.Fatalf("突变后 499ms 状态 = %s, want NAVIGATING", got)
	}
	clock.advance(1 * time.Millisecond)
	if got := v.Advance(); got != StateNavigated {
		t.Fatalf("突变后 500ms 状态 = %s, want NAVIGATED", got)
	}
}

func TestEng07TimeoutExceeded(t *testing.T) {
	clock := newFakeClock()
	v := NewVerifier(clock)
	v.FeedPushState()
	v.FeedNetwork(3) // 一直忙，无法静息

	clock.advance(1499 * time.Millisecond)
	if got := v.Advance(); got != StateNavigating {
		t.Fatalf("1499ms 状态 = %s, want NAVIGATING", got)
	}
	clock.advance(1 * time.Millisecond)
	if got := v.Advance(); got != StateDeadlineExceeded {
		t.Fatalf("1500ms 状态 = %s, want DEADLINE_EXCEEDED", got)
	}
}

func TestEng06RepeatPushStateKeepsNavStart(t *testing.T) {
	clock := newFakeClock()
	v := NewVerifier(clock)
	v.FeedPushState()
	clock.advance(100 * time.Millisecond)
	v.FeedPushState() // 重复事件不重置导航起点
	v.FeedNetwork(3)  // 网络持续忙（真正在加载）→ 超时才判 DEADLINE

	clock.advance(1400 * time.Millisecond)
	if got := v.Advance(); got != StateDeadlineExceeded {
		t.Fatalf("重复 pushState 后仍按首次导航计时 = %s, want DEADLINE_EXCEEDED", got)
	}
}

// P4（2026-08-28）：CSS 过渡/轮播会让 DOM 持续突变，永不"静息"。此时若网络已空闲
// （页面加载/拉数据完成），超时后应容忍为 NAVIGATED（已加载好），而非 DEADLINE——
// 避免触发无谓重试 / VLM 兜底（省 token）。
func TestEng07TransitionTolerantRatherThanStuck(t *testing.T) {
	clock := newFakeClock()
	v := NewVerifier(clock)
	v.FeedPushState()
	v.FeedNetwork(0) // 网络空闲：页面已加载/无阻塞请求

	// 持续喂入 DOM 突变（超过防抖窗口 150ms，模拟轮播/抽屉动画）。
	mutateEvery := func(step time.Duration, times int) {
		for i := 0; i < times; i++ {
			clock.advance(step)
			v.FeedDOMMutation()
		}
	}
	mutateEvery(160*time.Millisecond, 6) // ~960ms 内持续突变
	if got := v.Advance(); got != StateNavigating {
		t.Fatalf("持续突变未到超时应 NAVIGATING = %s, want NAVIGATING", got)
	}

	// 继续突变到总超时 1500ms：网络空闲 → 容忍为 NAVIGATED。
	mutateEvery(100*time.Millisecond, 6) // 至 ~1560ms（超 NavTimeout 1500）
	if got := v.Advance(); got != StateNavigated {
		t.Fatalf("网络空闲+持续突变超时后应容忍 NAVIGATED = %s, want NAVIGATED", got)
	}
}

// P4 反例：网络仍忙（真正在加载/拉数据）→ 超时仍需保留 DEADLINE，不得误判稳定。
func TestEng07BusyNetworkStillDeadlineOnTimeout(t *testing.T) {
	clock := newFakeClock()
	v := NewVerifier(clock)
	v.FeedPushState()
	v.FeedNetwork(3) // 持续忙

	clock.advance(1500 * time.Millisecond)
	if got := v.Advance(); got != StateDeadlineExceeded {
		t.Fatalf("网络忙+超时应 DEADLINE_EXCEEDED = %s, want DEADLINE_EXCEEDED", got)
	}
}
