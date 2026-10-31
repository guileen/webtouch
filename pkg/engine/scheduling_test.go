package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"uws/pkg/browser"

	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ── T-10 整体调度：预算耗尽 × 派发状态 × 幂等账本状态转移 ──────────────────
//
// 这一组用例只驱动引擎自己的调度语义（fake Browser 会话，不拉真浏览器、不跑 VLM）：
// 调用方 ctx 的预算（deadline）耗尽时，物理层必须如实回报「事件是否已经派发」，
// 引擎据此把 intent 放进/移出幂等账本——「未派发」= 无副作用，同 intent 可安全重放；
// 「已派发」= 结果未知，观察窗口内同 intent 不得再打一次。
//
// 状态转移表（intent × dispatch_state → gRPC 码 → 可否重放）见
// docs/scheduling-trace-2026-09-23.md §3；这里把每一行钉成确定性证据。

type schedStubSession struct {
	*SimCDPSession
	mu sync.Mutex
	// observed 非空时：本次物理执行阻塞到 ctx 结束（= 预算耗尽），再回报该 observed，
	// 模拟真实物理层「超时后如实上报派发状态」。为空时透传模拟会话的默认行为。
	observed string
	calls    int
}

func (s *schedStubSession) setObserved(o string) {
	s.mu.Lock()
	s.observed = o
	s.mu.Unlock()
}

func (s *schedStubSession) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *schedStubSession) ExecuteAction(ctx context.Context, action, value, semantic string) (bool, string) {
	s.mu.Lock()
	s.calls++
	obs := s.observed
	s.mu.Unlock()
	if obs == "" {
		return s.SimCDPSession.ExecuteAction(ctx, action, value, semantic)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second): // 兜底：用例必须给带 deadline 的 ctx，否则视为挂死
	}
	return false, obs
}

func (s *schedStubSession) ExecuteActionWithSelector(ctx context.Context, action, value, semantic, selector string) (bool, string) {
	return s.ExecuteAction(ctx, action, value, semantic)
}

type schedStubFactory struct{ last *schedStubSession }

func (f *schedStubFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s := &schedStubSession{SimCDPSession: NewSimCDPSession("sched-stub", cfg)}
	s.SetPageURL(vlmPageURL)
	f.last = s
	return s, nil
}

// schedEnv 建一个只带 fake Browser 会话的引擎环境（VLM 关闭：本组用例不测兜底）。
func schedEnv(t *testing.T) (*testEnv, *schedStubSession) {
	t.Helper()
	f := &schedStubFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(2, f),
	})
	// 直连 Server（不经 gRPC 客户端）：用例要控制 ctx 的 deadline，同时不希望
	// 客户端侧的 deadline 抢先中断而看不到服务端的账本状态转移。
	ctx, cancel := schedCtx(2 * time.Second)
	defer cancel()
	if _, err := env.srv.InitializeContext(ctx, &uwspb.InitContextRequest{TaskId: "sched-task"}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if f.last == nil {
		t.Fatal("工厂未创建会话")
	}
	return env, f.last
}

// schedCtx 造一个带 incoming 身份元数据（ckeyFor 用）的 ctx，timeout<=0 表示不设 deadline。
func schedCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-api-key", "coordinator-key"))
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func schedExec(t *testing.T, env *testEnv, timeout time.Duration, intentID, semantic, action string) (*uwspb.ActionResultResponse, error) {
	t.Helper()
	ctx, cancel := schedCtx(timeout)
	defer cancel()
	return env.srv.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId:       intentID,
		TargetSemantic: semantic,
		ExpectedAction: action,
		ContextHint:    map[string]string{"context": ""},
	})
}

// 预算耗尽 + 未派发：DEADLINE_EXCEEDED，且账本立即释放——同 intent 可安全重放。
func TestScheduleDeadlineNotDispatchedAllowsReplay(t *testing.T) {
	env, sess := schedEnv(t)
	sess.setObserved(browser.ObservedCanceledBeforeDispatch)

	_, err := schedExec(t, env, 150*time.Millisecond, "sched-before-1", "登录", "click")
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("预算耗尽（未派发）应 DEADLINE_EXCEEDED，got %v", err)
	}
	if m := status.Convert(err).Message(); !strings.Contains(m, "not_dispatched") {
		t.Fatalf("文案应说明没有任何副作用，got %q", m)
	}
	if sess.callCount() != 1 {
		t.Fatalf("物理层应只被调用 1 次，got %d", sess.callCount())
	}

	// 物理层恢复：同 intent 必须能真的再打一次（未被 indeterminate 挡住）。
	sess.setObserved("")
	resp, err2 := schedExec(t, env, 2*time.Second, "sched-before-1", "登录", "click")
	if err2 != nil {
		t.Fatalf("未派发预算耗尽后应可安全重放: %v", err2)
	}
	if !resp.GetSuccess() {
		t.Fatalf("重放应成功: %+v", resp)
	}
	if sess.callCount() != 2 {
		t.Fatalf("重放应再派发一次（共 2 次），got %d", sess.callCount())
	}
}

// 预算耗尽 + 已派发：DEADLINE_EXCEEDED 且结果未知；观察窗口内同 intent 不得重放。
func TestScheduleDeadlineAfterDispatchBlocksReplay(t *testing.T) {
	env, sess := schedEnv(t)
	sess.setObserved(browser.ObservedCanceledAfterDispatch)

	_, err := schedExec(t, env, 150*time.Millisecond, "sched-after-1", "登录", "click")
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("预算耗尽（已派发）应 DEADLINE_EXCEEDED，got %v", err)
	}
	m := status.Convert(err).Message()
	if !strings.Contains(m, "结果未知") || !strings.Contains(m, "physical_event_dispatched") {
		t.Fatalf("文案应显式说明结果未知，got %q", m)
	}
	if sess.callCount() != 1 {
		t.Fatalf("物理层应只被调用 1 次，got %d", sess.callCount())
	}

	// 同 intent 重试（调用方预算充足）：窗口内既不能重放，也不能假装没发生。
	_, err2 := schedExec(t, env, 2*time.Second, "sched-after-1", "登录", "click")
	if status.Code(err2) != codes.Aborted {
		t.Fatalf("未知窗口内同 intent 重试应 ABORTED，got %v", err2)
	}
	if m2 := status.Convert(err2).Message(); !strings.Contains(m2, "physical outcome unknown") {
		t.Fatalf("重试文案应点明结果未知，got %q", m2)
	}
	if sess.callCount() != 1 {
		t.Fatalf("未知窗口内不得重放物理动作，调用次数 = %d", sess.callCount())
	}
}

// 跨 intent：结果未知只按 intent_id 记账。换新 intent_id（= 协调器的新 attempt）
// 是一次新的决策，引擎放行；「同一步骤的重试复用同一 intent」由协调器保证
// （coordinator/executor/executor.py::_execute_step）。
func TestScheduleNewIntentDispatchesIndependentlyOfUnknownWindow(t *testing.T) {
	env, sess := schedEnv(t)
	sess.setObserved(browser.ObservedCanceledAfterDispatch)

	if _, err := schedExec(t, env, 150*time.Millisecond, "sched-iA", "登录", "click"); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("第一次（新 intent A）应 DEADLINE_EXCEEDED，got %v", err)
	}
	if sess.callCount() != 1 {
		t.Fatalf("物理层应只被调用 1 次，got %d", sess.callCount())
	}

	// 新 intent_id：即便 A 的结果仍未知，B 也是一次独立派发（不是重放 A）。
	sess.setObserved("")
	resp, err := schedExec(t, env, 2*time.Second, "sched-iB", "登录", "click")
	if err != nil {
		t.Fatalf("新 intent 应可独立派发: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("新 intent 应成功: %+v", resp)
	}
	if sess.callCount() != 2 {
		t.Fatalf("新 intent 应产生第 2 次物理派发，got %d", sess.callCount())
	}

	// A 的未知窗口不因 B 的执行而消失：A 仍然拿「未知」，不会被补记成成功。
	if _, err := schedExec(t, env, 2*time.Second, "sched-iA", "登录", "click"); status.Code(err) != codes.Aborted {
		t.Fatalf("A 应仍在未知窗口内 ABORTED，got %v", err)
	}
	if sess.callCount() != 2 {
		t.Fatalf("A 的重试不得再派发物理动作，got %d", sess.callCount())
	}
}

// 成功后的幂等账本：同 intent 重发命中缓存副本，不再次物理派发（cache 行）。
func TestScheduleSuccessCacheHitDoesNotRedispatch(t *testing.T) {
	env, sess := schedEnv(t)
	if _, err := schedExec(t, env, 2*time.Second, "sched-cache-1", "登录", "click"); err != nil {
		t.Fatalf("首次执行应成功: %v", err)
	}
	resp, err := schedExec(t, env, 2*time.Second, "sched-cache-1", "登录", "click")
	if err != nil {
		t.Fatalf("同 intent 重发应命中缓存: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("缓存副本应仍是成功结果: %+v", resp)
	}
	if sess.callCount() != 1 {
		t.Fatalf("缓存命中不得再次物理派发，调用次数 = %d", sess.callCount())
	}
}
