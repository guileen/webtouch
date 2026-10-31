package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"uws/pkg/browser"
	"uws/pkg/vlm"

	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── 引擎侧取消/结果未知语义（2026-09-23 审查结论 item 2）─────────────────
//
// 物理层把中断如实回报成 canceled_before/after_dispatch，引擎必须：
//   1) 中断先于 VLM 兜底处理（死 ctx 上不再做无意义物理尝试，也不盖掉「未知」）；
//   2) before → CANCELED，去重状态立即释放（同 intent 可安全重放）；
//   3) after  → CANCELED + 明确「结果未知」；indeterminateGrace 内同 intent 的重试
//      返回 ABORTED 且不再触发物理执行（不把一次点击变成两次）。
//
// 这里用脚本化桩回报中断语义（不依赖真实浏览器），并把「桩调用次数」当作
// 「物理层是否被再次打动」的硬证据。

type canceledStubSession struct {
	*SimCDPSession
	mu       sync.Mutex
	observed string // 非空：ExecuteAction 直接回报该 observed（模拟物理层脚本化结局）
	calls    int
}

func (s *canceledStubSession) setObserved(o string) {
	s.mu.Lock()
	s.observed = o
	s.mu.Unlock()
}

func (s *canceledStubSession) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *canceledStubSession) ExecuteAction(ctx context.Context, action, value, semantic string) (bool, string) {
	s.mu.Lock()
	s.calls++
	obs := s.observed
	s.mu.Unlock()
	if obs != "" {
		return false, obs
	}
	return s.SimCDPSession.ExecuteAction(ctx, action, value, semantic)
}

func (s *canceledStubSession) ExecuteActionWithSelector(ctx context.Context, action, value, semantic, selector string) (bool, string) {
	return s.ExecuteAction(ctx, action, value, semantic)
}

type canceledStubFactory struct {
	last *canceledStubSession
}

func (f *canceledStubFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s := &canceledStubSession{SimCDPSession: NewSimCDPSession("cancel-stub", cfg)}
	s.SetPageURL(vlmPageURL)
	f.last = s
	return s, nil
}

func cancelEnv(t *testing.T, fb *vlm.Fallback) (*testEnv, *canceledStubSession) {
	t.Helper()
	f := &canceledStubFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(2, f),
		VLM:  fb,
	})
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"),
		&uwspb.InitContextRequest{TaskId: "cancel-task"}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if f.last == nil {
		t.Fatal("工厂未创建会话")
	}
	return env, f.last
}

func cancelExec(t *testing.T, env *testEnv, intentID, semantic, action string) (*uwspb.ActionResultResponse, error) {
	t.Helper()
	return env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       intentID,
		TargetSemantic: semantic,
		ExpectedAction: action,
		ContextHint:    map[string]string{"context": ""},
	})
}

// 已派发但结果未知：CANCELED + 「结果未知」文案；同 intent 在观察窗口内
// 返回 ABORTED 且物理层调用次数不增加（不重放）。
func TestInterruptAfterDispatchReportsOutcomeUnknown(t *testing.T) {
	env, sess := cancelEnv(t, nil)
	sess.setObserved(browser.ObservedCanceledAfterDispatch)

	_, err := cancelExec(t, env, "i-after-1", "登录", "click")
	if status.Code(err) != codes.Canceled {
		t.Fatalf("已派发中断应 CANCELED，got %v", err)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "结果未知") || !strings.Contains(msg, "physical_event_dispatched") {
		t.Fatalf("文案应显式说明结果未知，got %q", msg)
	}
	if sess.callCount() != 1 {
		t.Fatalf("物理层应只被调用 1 次，got %d", sess.callCount())
	}

	// 同 intent 重试：观察窗口内既不能重放，也不能假装没发生。
	_, err2 := cancelExec(t, env, "i-after-1", "登录", "click")
	if status.Code(err2) != codes.Aborted {
		t.Fatalf("未知窗口内同 intent 重试应 ABORTED，got %v", err2)
	}
	if m := status.Convert(err2).Message(); !strings.Contains(m, "physical outcome unknown") {
		t.Fatalf("重试文案应点明结果未知，got %q", m)
	}
	if sess.callCount() != 1 {
		t.Fatalf("未知窗口内不得重放物理动作，调用次数 = %d", sess.callCount())
	}
}

// 未派发即中断：CANCELED，且去重状态释放——同 intent 可以安全重放（次数 +1）。
func TestInterruptBeforeDispatchAllowsRetry(t *testing.T) {
	env, sess := cancelEnv(t, nil)
	sess.setObserved(browser.ObservedCanceledBeforeDispatch)

	_, err := cancelExec(t, env, "i-before-1", "登录", "click")
	if status.Code(err) != codes.Canceled {
		t.Fatalf("未派发中断应 CANCELED，got %v", err)
	}
	if m := status.Convert(err).Message(); !strings.Contains(m, "not_dispatched") {
		t.Fatalf("文案应说明没有任何副作用，got %q", m)
	}
	if sess.callCount() != 1 {
		t.Fatalf("物理层应被调用 1 次，got %d", sess.callCount())
	}

	// 物理层恢复正常：同 intent 必须能真的再打一次（没有被 indeterminate 挡住）。
	sess.setObserved("")
	resp, err2 := cancelExec(t, env, "i-before-1", "登录", "click")
	if err2 != nil {
		t.Fatalf("未派发中断后应可安全重放: %v", err2)
	}
	if !resp.GetSuccess() {
		t.Fatalf("重放应成功: %+v", resp)
	}
	if sess.callCount() != 2 {
		t.Fatalf("重放应再派发一次（共 2 次），got %d", sess.callCount())
	}
}

// 中断先于 VLM 兜底：取消时不得在死 ctx 上跑视觉兜底。
// 对照组（物理层只是 no_match，没中断）证明 VLM 在这套配置下确实会被调用，
// 避免「VLM 根本不可用」造成假绿。
func TestInterruptSkipsVLMFallback(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{{
		Found: true, Text: "登录", XRatio: 0.5, YRatio: 0.5,
	}}}
	fb := &vlm.Fallback{Enabled: true, Client: mock, Timeout: 5e9}
	env, sess := cancelEnv(t, fb)

	sess.setObserved(browser.ObservedCanceledBeforeDispatch)
	if _, err := cancelExec(t, env, "i-vlm-1", "登录", "click"); status.Code(err) != codes.Canceled {
		t.Fatalf("中断应 CANCELED，got %v", err)
	}
	if mock.Calls != 0 {
		t.Fatalf("中断时不得跑 VLM 兜底，calls = %d", mock.Calls)
	}

	sess.setObserved("no_match")
	if _, err := cancelExec(t, env, "i-vlm-2", "登录", "click"); err != nil {
		t.Fatalf("no_match + VLM 应成功: %v", err)
	}
	if mock.Calls == 0 {
		t.Fatal("对照组 VLM 未被调用——本测试的「中断跳过」断言无鉴别力")
	}
}

// ── VLM 坐标路径的中断语义（2026-09-23 审查结论 item 1）───────────────────
//
// VLM 兜底直发坐标点击（ClickAt）时同样受调用方 ctx 生命周期约束：派发后取消必须
// 如实变成「结果未知」，不能被展开成 Aborted/NOT_FOUND 后被上层重放。

type vlmCoordinateCancelStub struct {
	*canceledStubSession
	clickObs string // 非空：ClickAt 直接回报该 observed（模拟坐标原语被中断）
	clicks   int
}

func (s *vlmCoordinateCancelStub) setClickObserved(o string) {
	s.mu.Lock()
	s.clickObs = o
	s.mu.Unlock()
}

func (s *vlmCoordinateCancelStub) clickCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clicks
}

func (s *vlmCoordinateCancelStub) ClickAt(ctx context.Context, x, y int) (string, bool, string) {
	s.mu.Lock()
	s.clicks++
	obs := s.clickObs
	s.mu.Unlock()
	if obs != "" {
		return "", false, obs
	}
	return s.SimCDPSession.ClickAt(ctx, x, y)
}

type vlmCoordinateCancelFactory struct{ last *vlmCoordinateCancelStub }

func (f *vlmCoordinateCancelFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s := &vlmCoordinateCancelStub{canceledStubSession: &canceledStubSession{
		SimCDPSession: NewSimCDPSession("vlm-cancel-stub", cfg),
	}}
	s.SetPageURL(vlmPageURL)
	f.last = s
	return s, nil
}

func vlmCoordinateCancelEnv(t *testing.T, fb *vlm.Fallback) (*testEnv, *vlmCoordinateCancelStub) {
	t.Helper()
	f := &vlmCoordinateCancelFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(2, f),
		VLM:  fb,
	})
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"),
		&uwspb.InitContextRequest{TaskId: "vlm-cancel-task"}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if f.last == nil {
		t.Fatal("工厂未创建会话")
	}
	return env, f.last
}

// VLM 坐标点击已派发后中断：CANCELED + 结果未知；观察窗口内同 intent 不得重放。
func TestVLMPathCanceledAfterDispatchReportsUnknown(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{{
		Action: vlm.ActionClick, Found: true, XRatio: 0.5, YRatio: 0.5,
	}}}
	fb := &vlm.Fallback{Enabled: true, Client: mock, Timeout: 5e9}
	env, sess := vlmCoordinateCancelEnv(t, fb)
	sess.setObserved("no_match") // 主路径 miss → 进 VLM 兜底
	sess.setClickObserved(browser.ObservedCanceledAfterDispatch)

	_, err := cancelExec(t, env, "i-vlm-coord-1", "登录", "click")
	if status.Code(err) != codes.Canceled {
		t.Fatalf("VLM 坐标路径派发后中断应 CANCELED，got %v", err)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "结果未知") || !strings.Contains(msg, "physical_event_dispatched") {
		t.Fatalf("文案应显式说明结果未知，got %q", msg)
	}
	if sess.clickCount() != 1 {
		t.Fatalf("坐标点击应只发起 1 次（不得继续试下一个候选），got %d", sess.clickCount())
	}

	// 同 intent 重试：窗口内既不能重放，也不能假装没发生。
	_, err2 := cancelExec(t, env, "i-vlm-coord-1", "登录", "click")
	if status.Code(err2) != codes.Aborted {
		t.Fatalf("未知窗口内同 intent 重试应 ABORTED，got %v", err2)
	}
	if sess.clickCount() != 1 || mock.Calls != 1 {
		t.Fatalf("未知窗口内不得重放（clicks=%d vlm=%d）", sess.clickCount(), mock.Calls)
	}
}

// VLM 坐标点击派发前中断：CANCELED + 明确「没有副作用」，同 intent 可安全重放。
func TestVLMPathCanceledBeforeDispatchAllowsRetry(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{
		{Action: vlm.ActionClick, Found: true, XRatio: 0.5, YRatio: 0.5},
		{Action: vlm.ActionClick, Found: true, XRatio: 0.5, YRatio: 0.5},
	}}
	fb := &vlm.Fallback{Enabled: true, Client: mock, Timeout: 5e9}
	env, sess := vlmCoordinateCancelEnv(t, fb)
	sess.setObserved("no_match")
	sess.setClickObserved(browser.ObservedCanceledBeforeDispatch)

	_, err := cancelExec(t, env, "i-vlm-coord-2", "登录", "click")
	if status.Code(err) != codes.Canceled {
		t.Fatalf("VLM 坐标路径派发前中断应 CANCELED，got %v", err)
	}
	if m := status.Convert(err).Message(); !strings.Contains(m, "not_dispatched") {
		t.Fatalf("文案应说明没有任何副作用，got %q", m)
	}
	if sess.clickCount() != 1 {
		t.Fatalf("中断后不得再试下一个候选，got %d", sess.clickCount())
	}

	// 物理层恢复：同 intent 必须能真的再打一次（没有被 indeterminate 挡住）。
	sess.setClickObserved("")
	resp, err2 := cancelExec(t, env, "i-vlm-coord-2", "登录", "click")
	if err2 != nil {
		t.Fatalf("派发前中断后应可安全重放: %v", err2)
	}
	if !resp.GetSuccess() {
		t.Fatalf("重放应成功: %+v", resp)
	}
	if sess.clickCount() != 2 {
		t.Fatalf("重放应再派发一次（共 2 次），got %d", sess.clickCount())
	}
}

// ── 幂等账本的有界回收（2026-09-23 审查结论 item 3）───────────────────────
//
// cache/inflight/indeterminate 过去只会在「同一个 intent 再来」时被删，长跑引擎上
// 会随不同 intent_id 的个数单调增长。这里把「过期即回收」钉成确定性证据。

// 直接对账本做一次清扫：过期 cache、过了观察窗口的 indeterminate（连同它占着的
// inflight）必须被删；新鲜条目与真正在途的请求不得被误删。
func TestSweepLedgerLocked(t *testing.T) {
	now := time.Now()
	s := &Server{
		cache:         make(map[string]cachedResult),
		inflight:      make(map[string]struct{}),
		indeterminate: make(map[string]time.Time),
	}
	s.cache["cache-expired"] = cachedResult{expiresAt: now.Add(-time.Second)}
	s.cache["cache-fresh"] = cachedResult{expiresAt: now.Add(time.Minute)}
	s.indeterminate["unknown-old"] = now.Add(-2 * indeterminateGrace)
	s.indeterminate["unknown-fresh"] = now
	s.inflight["unknown-old"] = struct{}{} // 与 old 配对：请求早已返回，占位必须被回收
	s.inflight["live"] = struct{}{}        // 真正在执行的请求：不是过期项，不得被扫掉

	s.sweepLedgerLocked(now)

	if _, ok := s.cache["cache-expired"]; ok {
		t.Fatal("过期 cache 未被回收")
	}
	if _, ok := s.cache["cache-fresh"]; !ok {
		t.Fatal("新鲜 cache 被误删")
	}
	if _, ok := s.indeterminate["unknown-old"]; ok {
		t.Fatal("过了观察窗口的 indeterminate 未被回收")
	}
	if _, ok := s.indeterminate["unknown-fresh"]; !ok {
		t.Fatal("窗口内的 indeterminate 被误删（会放行重放）")
	}
	if _, ok := s.inflight["unknown-old"]; ok {
		t.Fatal("indeterminate 配对的 inflight 占位未被回收")
	}
	if _, ok := s.inflight["live"]; !ok {
		t.Fatal("在途请求的 inflight 占位被误删")
	}
}

// 清扫节流：间隔内重复调用不做第二次扫描（避免每次动作都全表扫账本）。
func TestSweepLedgerThrottled(t *testing.T) {
	now := time.Now()
	s := &Server{
		cache:         make(map[string]cachedResult),
		inflight:      make(map[string]struct{}),
		indeterminate: make(map[string]time.Time),
	}
	s.sweepLedgerLocked(now) // 第一次：更新 lastSweep
	s.cache["late"] = cachedResult{expiresAt: now.Add(-time.Second)}
	s.sweepLedgerLocked(now.Add(time.Second)) // 间隔内：不得清扫
	if _, ok := s.cache["late"]; !ok {
		t.Fatal("节流窗口内的清扫不应执行")
	}
	s.sweepLedgerLocked(now.Add(sweepInterval)) // 越过节流窗口：应清扫
	if _, ok := s.cache["late"]; ok {
		t.Fatal("越过节流窗口后过期项应被回收")
	}
}

// 端到端：一次「派发后结果未知」留下的 indeterminate/inflight 占位，会被后续动作
// 触发的清扫回收（不再依赖「同一个 intent 再来」）。
func TestExecuteTriggersLedgerSweep(t *testing.T) {
	env, sess := cancelEnv(t, nil)
	sess.setObserved(browser.ObservedCanceledAfterDispatch)
	if _, err := cancelExec(t, env, "i-sweep-1", "登录", "click"); status.Code(err) != codes.Canceled {
		t.Fatalf("前置：派发后中断应 CANCELED，got %v", err)
	}

	env.srv.mu.Lock()
	if len(env.srv.indeterminate) == 0 {
		env.srv.mu.Unlock()
		t.Fatal("前置：应留下 indeterminate 占位")
	}
	// 把观察窗口拨到过去，并放开节流，模拟「服务已经跑了很久」。
	for k := range env.srv.indeterminate {
		env.srv.indeterminate[k] = time.Now().Add(-2 * indeterminateGrace)
	}
	env.srv.lastSweep = time.Time{}
	env.srv.mu.Unlock()

	sess.setObserved("")
	if _, err := cancelExec(t, env, "i-sweep-2", "登录", "click"); err != nil {
		t.Fatalf("后续动作应成功（清扫不应影响它）: %v", err)
	}

	env.srv.mu.Lock()
	defer env.srv.mu.Unlock()
	if len(env.srv.indeterminate) != 0 {
		t.Fatalf("过期 indeterminate 应被回收，got %v", env.srv.indeterminate)
	}
	if _, ok := env.srv.inflight["i-sweep-1"]; ok {
		t.Fatal("配对的 inflight 占位应被回收")
	}
}
