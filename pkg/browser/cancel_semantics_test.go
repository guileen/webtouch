package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
	"uws/pkg/browser/internal/wstest"
	"uws/pkg/browser/modelbridge"
)

// ── 物理层取消语义（2026-09-23 审查结论 item 2）──────────────────────────
//
// 调用方 ctx 是定位/等待/派发的同一 deadline：中断时物理层必须如实回报
// 「中断前是否已经把鼠标/键盘事件派发出去」——canceled_before_dispatch 可以
// 安全重放，canceled_after_dispatch 的结果未知、不得盲目重放。
// 本文件用「可挂起伪 CDP 服务器」把这两个分支各自钉成确定性证据。

// cancelFakeCDP 记录全部收到的 CDP 命令；stall 集合里的方法一律不响应
// （模拟物理层卡在浏览器里），其余方法立即返回空结果。
type cancelFakeCDP struct {
	mu    sync.Mutex
	calls []string
	stall map[string]bool
	// evalSeq/evalValue 脚本化 Runtime.evaluate 的返回值（JSON 原文）：evalSeq 按
	// 调用顺序逐条放行（定位返回选择器 → 聚焦判定返回 true 这类多步 JS 判定），
	// 取尽后退回 evalValue，再退回空 result（未配置时与旧行为完全一致）。
	evalSeq   []string
	evalValue string
}

// nextEvalValue 取本轮 Runtime.evaluate 应返回的 JSON 原文。
func (f *cancelFakeCDP) nextEvalValue() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.evalSeq) > 0 {
		v := f.evalSeq[0]
		f.evalSeq = f.evalSeq[1:]
		return v
	}
	return f.evalValue
}

func (f *cancelFakeCDP) record(method string) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
}

func (f *cancelFakeCDP) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.calls {
		if m == method {
			n++
		}
	}
	return n
}

func (f *cancelFakeCDP) shouldStall(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stall[method]
}

func (f *cancelFakeCDP) serve(t *testing.T) string {
	t.Helper()
	return wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
		for {
			op, payload, err := wstest.ReadServerFrame(br)
			if err != nil {
				return
			}
			if op != wstest.OpText {
				continue
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}
			id, _ := msg["id"].(float64)
			method, _ := msg["method"].(string)
			f.record(method)
			if f.shouldStall(method) {
				continue // 挂起：不响应，等调用方 ctx 到期/取消
			}
			resp := map[string]any{"id": int64(id), "result": map[string]any{}}
			if method == "Runtime.evaluate" {
				if v := f.nextEvalValue(); v != "" {
					resp = map[string]any{"id": int64(id), "result": map[string]any{
						"result": map[string]any{"type": "string", "value": json.RawMessage(v)},
					}}
				}
			}
			b, _ := json.Marshal(resp)
			_ = wstest.WriteServerText(conn, b)
		}
	})
}

// newCancelTestSession 构造一个只连着伪服务器的会话（不动真实 Chrome）。
func newCancelTestSession(t *testing.T, url string) *cdpSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := newCDPClient(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &cdpSession{
		id:        "cancel-test",
		client:    c,
		sessionID: "sess-1",
		closedCh:  make(chan struct{}),
		inflight:  make(map[string]time.Time),
		chat:      modelbridge.NewStream(),
	}
}

// awaitCall 阻塞到伪服务器收到第 n 条指定命令（或超时失败）。
func awaitCall(t *testing.T, fake *cancelFakeCDP, method string, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fake.count(method) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("伪 CDP 服务器未在 3s 内收到 %d 条 %s", n, method)
}

// IsCanceledObserved 是两个层之间的契约桥：中断观测串必须能被唯一解成
// 「已取消 + 是否已派发」。
func TestIsCanceledObservedMapping(t *testing.T) {
	cases := []struct {
		observed   string
		canceled   bool
		dispatched bool
	}{
		{ObservedCanceledBeforeDispatch, true, false},
		{ObservedCanceledAfterDispatch, true, true},
		{"no_match", false, false},
		{"target_destroyed", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		canceled, dispatched := IsCanceledObserved(c.observed)
		if canceled != c.canceled || dispatched != c.dispatched {
			t.Fatalf("IsCanceledObserved(%q) = (%v,%v)，want (%v,%v)",
				c.observed, canceled, dispatched, c.canceled, c.dispatched)
		}
	}
}

// 未中断的 ctx 不得被误判成取消语义。
func TestCanceledObservedLiveCtx(t *testing.T) {
	if obs, canceled := canceledObserved(context.Background(), false); canceled || obs != "" {
		t.Fatalf("活 ctx 不应回报中断，got (%q,%v)", obs, canceled)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if obs, canceled := canceledObserved(ctx, false); !canceled || obs != ObservedCanceledBeforeDispatch {
		t.Fatalf("取消且未派发应为 before_dispatch，got (%q,%v)", obs, canceled)
	}
	if obs, canceled := canceledObserved(ctx, true); !canceled || obs != ObservedCanceledAfterDispatch {
		t.Fatalf("取消且已派发应为 after_dispatch，got (%q,%v)", obs, canceled)
	}
}

// 定位阶段（Runtime.evaluate 挂起）被取消：没有任何物理事件派发 →
// canceled_before_dispatch，调用方可以安全重放。
func TestExecuteActionCanceledBeforeDispatch(t *testing.T) {
	fake := &cancelFakeCDP{stall: map[string]bool{"Runtime.evaluate": true}}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		ok       bool
		observed string
	}
	done := make(chan result, 1)
	go func() {
		ok, observed := s.ExecuteAction(ctx, "click", "", "登录")
		done <- result{ok, observed}
	}()

	// 等物理层真的把定位命令发出去了（证明动作已进入执行阶段），再取消。
	awaitCall(t, fake, "Runtime.evaluate", 1)
	cancel()

	select {
	case r := <-done:
		if r.ok {
			t.Fatalf("已取消的动作不得报成功: %q", r.observed)
		}
		if r.observed != ObservedCanceledBeforeDispatch {
			t.Fatalf("observed = %q，want %s", r.observed, ObservedCanceledBeforeDispatch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 executeAction 未及时返回")
	}
	if n := fake.count("Input.dispatchMouseEvent"); n != 0 {
		t.Fatalf("定位阶段取消不得派发鼠标事件，got %d 条", n)
	}
}

// 派发阶段（Input.dispatchMouseEvent 挂起）被取消：事件已经出去 →
// canceled_after_dispatch（结果未知，不得盲目重试）。
func TestExecuteActionCanceledAfterDispatch(t *testing.T) {
	fake := &cancelFakeCDP{stall: map[string]bool{"Input.dispatchMouseEvent": true}}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		ok       bool
		observed string
	}
	done := make(chan result, 1)
	go func() {
		ok, observed := s.ExecuteAction(ctx, "drag", "200,200,300,200", "challenge_slider")
		done <- result{ok, observed}
	}()

	// 鼠标事件已经写给浏览器了，此时才取消：物理结果不可撤销。
	awaitCall(t, fake, "Input.dispatchMouseEvent", 1)
	cancel()

	select {
	case r := <-done:
		if r.ok {
			t.Fatalf("已取消的动作不得报成功: %q", r.observed)
		}
		if r.observed != ObservedCanceledAfterDispatch {
			t.Fatalf("observed = %q，want %s", r.observed, ObservedCanceledAfterDispatch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 executeAction 未及时返回")
	}
	if n := fake.count("Input.dispatchMouseEvent"); n < 1 {
		t.Fatalf("after_dispatch 分支必须先有过真实派发，got %d 条", n)
	}
}

// ── VLM 坐标原语的中断语义（ClickAt/InputAt，2026-09-23 审查结论 item 1）──────
//
// VLM 主控路径直发坐标点击/填值，过去用普通失败串（click_failed/input_failed）
// 掩盖了「中断前是否已派发」——上层会把「可能已经点过」当普通失败重放。
// 这两个原语必须与 executeAction 共用同一套 dispatchTracker 语义。

// 坐标点击在定位阶段（elementPathAt 的 Runtime.evaluate）被取消：没有任何鼠标事件派发。
func TestClickAtCanceledBeforeDispatch(t *testing.T) {
	fake := &cancelFakeCDP{stall: map[string]bool{"Runtime.evaluate": true}}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		ok       bool
		observed string
	}
	done := make(chan result, 1)
	go func() {
		_, ok, observed := s.ClickAt(ctx, 10, 20)
		done <- result{ok, observed}
	}()

	awaitCall(t, fake, "Runtime.evaluate", 1)
	cancel()

	select {
	case r := <-done:
		if r.ok {
			t.Fatalf("已取消的坐标点击不得报成功: %q", r.observed)
		}
		if r.observed != ObservedCanceledBeforeDispatch {
			t.Fatalf("observed = %q，want %s", r.observed, ObservedCanceledBeforeDispatch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 ClickAt 未及时返回")
	}
	if n := fake.count("Input.dispatchMouseEvent"); n != 0 {
		t.Fatalf("定位阶段取消不得派发鼠标事件，got %d 条", n)
	}
}

// 坐标已经解析、鼠标事件挂起时被取消：结果未知，不得让上层当成「点失败了」重放。
func TestClickAtCanceledAfterDispatch(t *testing.T) {
	fake := &cancelFakeCDP{
		stall:     map[string]bool{"Input.dispatchMouseEvent": true},
		evalValue: `{"sel":"div#login","tag":"div","text":"登录"}`, // elementPointAt 命中元素 ＋ 落点文本
	}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		ok       bool
		observed string
	}
	done := make(chan result, 1)
	go func() {
		_, ok, observed := s.ClickAt(ctx, 10, 20)
		done <- result{ok, observed}
	}()

	awaitCall(t, fake, "Input.dispatchMouseEvent", 1)
	cancel()

	select {
	case r := <-done:
		if r.ok {
			t.Fatalf("已取消的坐标点击不得报成功: %q", r.observed)
		}
		if r.observed != ObservedCanceledAfterDispatch {
			t.Fatalf("observed = %q，want %s", r.observed, ObservedCanceledAfterDispatch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 ClickAt 未及时返回")
	}
	if n := fake.count("Input.dispatchMouseEvent"); n < 1 {
		t.Fatalf("after_dispatch 分支必须先有过真实派发，got %d 条", n)
	}
}

// 坐标点击的落点证据必须连同命中元素可见文本一起落账（§5.0.9 条款 6 点击落点自证）：
// 物理层在 elementFromPoint 解析受体的同一步取证文本，引擎据此判「相关 / miss / 证据不足」。
func TestClickAtRecordsLandingText(t *testing.T) {
	fake := &cancelFakeCDP{evalValue: `{"sel":"div#app > button:nth-of-type(2)","tag":"button","text":"立即购买"}`}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sel, ok, observed := s.ClickAt(ctx, 10, 20)
	if !ok {
		t.Fatalf("坐标点击应成功: %q", observed)
	}
	if sel != "div#app > button:nth-of-type(2)" {
		t.Fatalf("selector = %q", sel)
	}
	h, has := s.LastHitElement()
	if !has {
		t.Fatal("坐标点击后必须有命中元素证据")
	}
	if h.Tag != "button" || h.Text != "立即购买" {
		t.Fatalf("落点证据缺文本/标签: %+v", h)
	}
}

// 坐标就近填值在定位输入框阶段被取消：没有键盘事件派发。
func TestInputAtCanceledBeforeDispatch(t *testing.T) {
	fake := &cancelFakeCDP{stall: map[string]bool{"Runtime.evaluate": true}}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		ok       bool
		observed string
	}
	done := make(chan result, 1)
	go func() {
		_, ok, observed := s.InputAt(ctx, 10, 20, "文本", "search_box")
		done <- result{ok, observed}
	}()

	awaitCall(t, fake, "Runtime.evaluate", 1)
	cancel()

	select {
	case r := <-done:
		if r.ok {
			t.Fatalf("已取消的坐标填值不得报成功: %q", r.observed)
		}
		if r.observed != ObservedCanceledBeforeDispatch {
			t.Fatalf("observed = %q，want %s", r.observed, ObservedCanceledBeforeDispatch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 InputAt 未及时返回")
	}
	if n := fake.count("Input.insertText"); n != 0 {
		t.Fatalf("定位阶段取消不得派发键盘事件，got %d 条", n)
	}
}

// 输入框已解析、聚焦判定通过、insertText 挂起时被取消：结果未知。
func TestInputAtCanceledAfterDispatch(t *testing.T) {
	fake := &cancelFakeCDP{
		stall: map[string]bool{"Input.insertText": true},
		// Runtime.evaluate 调用序：① 穿透寻底（§5.0.10 条款 1，返回 {ok,sel}）；
		// ② 派发前租约快照（§5.0.10 条款 2；无标记载体 ⇒ found=false、lease=nil）；
		// ③ §5.0.9 受体角色探针（未标记目标 ⇒ found=false，不熔断）；④ §5.0.3 可编辑性
		// 门禁（命中 input ⇒ true）；⑤ 聚焦判定（true）。
		evalSeq: []string{
			`{"ok":true,"sel":"input#email","tag":"input"}`,
			`{"found":false}`,
			`{"found":false}`,
			`true`,
			`true`,
		},
	}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		ok       bool
		observed string
	}
	done := make(chan result, 1)
	go func() {
		_, ok, observed := s.InputAt(ctx, 10, 20, "文本", "search_box")
		done <- result{ok, observed}
	}()

	awaitCall(t, fake, "Input.insertText", 1)
	cancel()

	select {
	case r := <-done:
		if r.ok {
			t.Fatalf("已取消的坐标填值不得报成功: %q", r.observed)
		}
		if r.observed != ObservedCanceledAfterDispatch {
			t.Fatalf("observed = %q，want %s", r.observed, ObservedCanceledAfterDispatch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 InputAt 未及时返回")
	}
}

// ── 确定性定位族的派发标记时点（§5.0.11 条款 1/3，2026-09-25）──────────────
//
// input/fill/select 与站点特化 click 族过去在**定位之前**就 disp.mark()：定位阶段
// 超时/取消时真因（input_failed:context deadline exceeded）被覆盖成
// physical_event_dispatched_outcome_unknown —— 调用方据此放弃重试，而动作从未派发。
// 标记必须落在「定位与派发前校验全部通过、物理事件即将发出」的时点。

// input 动作在定位阶段被取消：没有键盘事件派发 → canceled_before_dispatch（可安全重放）。
func TestExecuteActionInputCanceledBeforeDispatch(t *testing.T) {
	fake := &cancelFakeCDP{stall: map[string]bool{"Runtime.evaluate": true}}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		ok       bool
		observed string
	}
	done := make(chan result, 1)
	go func() {
		ok, observed := s.ExecuteAction(ctx, "input", "文本", "搜索")
		done <- result{ok, observed}
	}()

	awaitCall(t, fake, "Runtime.evaluate", 1)
	cancel()

	select {
	case r := <-done:
		if r.ok {
			t.Fatalf("已取消的动作不得报成功: %q", r.observed)
		}
		if r.observed != ObservedCanceledBeforeDispatch {
			t.Fatalf("定位阶段取消应报 %s，got %q（真因被「已派发」覆盖）",
				ObservedCanceledBeforeDispatch, r.observed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 executeAction 未及时返回")
	}
	if n := fake.count("Input.insertText"); n != 0 {
		t.Fatalf("定位阶段取消不得派发键盘事件，got %d 条", n)
	}
	if n := fake.count("Input.dispatchMouseEvent"); n != 0 {
		t.Fatalf("定位阶段取消不得派发鼠标事件，got %d 条", n)
	}
}

// 站点特化 click 族（gemini_* 走 clickWithRecovery）在定位阶段被取消：同样必须报
// canceled_before_dispatch，而不是被前置标记报成「结果未知」。
func TestExecuteActionGeminiClickCanceledBeforeDispatch(t *testing.T) {
	fake := &cancelFakeCDP{stall: map[string]bool{"Runtime.evaluate": true}}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		ok       bool
		observed string
	}
	done := make(chan result, 1)
	go func() {
		ok, observed := s.ExecuteAction(ctx, "click", "", "gemini_send")
		done <- result{ok, observed}
	}()

	awaitCall(t, fake, "Runtime.evaluate", 1)
	cancel()

	select {
	case r := <-done:
		if r.ok {
			t.Fatalf("已取消的动作不得报成功: %q", r.observed)
		}
		if r.observed != ObservedCanceledBeforeDispatch {
			t.Fatalf("定位阶段取消应报 %s，got %q（真因被「已派发」覆盖）",
				ObservedCanceledBeforeDispatch, r.observed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 executeAction 未及时返回")
	}
	if n := fake.count("Input.dispatchMouseEvent"); n != 0 {
		t.Fatalf("定位阶段取消不得派发鼠标事件，got %d 条", n)
	}
}
