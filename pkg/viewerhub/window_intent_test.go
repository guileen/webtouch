package viewerhub

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"uws/pkg/browser"
)

// cdpStub 满足 browser.CDPSession：单测只关心窗口搬运，其余方法是空实现。
type cdpStub struct{ id string }

func (s cdpStub) ID() string                             { return s.id }
func (s cdpStub) Probe() browser.ProbeResult             { return browser.ProbeResult{} }
func (s cdpStub) Navigate(context.Context, string) error { return nil }
func (s cdpStub) SetCookie(browser.Cookie)               {}
func (s cdpStub) Cookies() []browser.Cookie              { return nil }
func (s cdpStub) ExportState(context.Context, string) (*browser.SerializedSessionState, error) {
	return nil, nil
}
func (s cdpStub) Close() error { return nil }

// plainSession 有会话、没有可搬动的窗口（无头 / 附着）——壳必须收到 ok=false。
type plainSession struct{ cdpStub }

// movableSession 有一扇真窗口：记录被抬了几次，便于断言「只动这一个会话」。
//
// 2026-09-22 口径：展开不再收尺寸（抬高即展开），所以假窗口回报的是它**当前**的矩形，
// 而不是一个被缩放过的矩形——「展开不改变窗口几何」正是这一版要保住的性质。
type movableSession struct {
	cdpStub
	mu        sync.Mutex
	raised    int
	prepared  int
	activated int
	restored  int
	fail      error
}

// PrepareViewerWindow / ActivateViewerWindow 是 2026-09-24 拆出来的两步：壳要先飞动画、
// 末段才让窗口到前台（规范 §4.4 第 3 条）。两步各自记数，好断言「deferRaise 那一趟真的
// 没激活」。
func (s *movableSession) PrepareViewerWindow(context.Context) (browser.ViewerWindowBounds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return browser.ViewerWindowBounds{}, s.fail
	}
	s.prepared++
	return browser.ViewerWindowBounds{Left: 1496, Top: 60, Width: 1440, Height: 900}, nil
}

func (s *movableSession) ActivateViewerWindow(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.activated++
	return nil
}

func (s *movableSession) preparedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prepared
}

func (s *movableSession) activatedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activated
}

func (s *movableSession) RaiseViewerWindow(context.Context) (browser.ViewerWindowBounds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return browser.ViewerWindowBounds{}, s.fail
	}
	s.raised++
	// 真实实现里 RaiseViewerWindow = PrepareViewerWindow + ActivateViewerWindow（就地激活那条
	// 老路径），假窗口照样记账，好让「deferRaise 那一趟真的没激活」这句断言有意义。
	s.activated++
	return browser.ViewerWindowBounds{Left: 1496, Top: 60, Width: 1440, Height: 900}, nil
}

func (s *movableSession) RestoreViewerWindow(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restored++
	return nil
}

func (s *movableSession) BrowserPID() int { return 4242 }

func (s *movableSession) raiseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raised
}

func expandIntent(session string) Intent {
	return Intent{
		Version: IntentProtocolVersion, Kind: IntentKind, Intent: IntentExpand, Session: session,
		Size: &WindowSize{Width: 960, Height: 620},
	}
}

// readResult 从观看壳那一侧读一行回执。
func readResult(t *testing.T, conn net.Conn) IntentResult {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("读回执: %v", err)
	}
	var res IntentResult
	if err := json.Unmarshal(line, &res); err != nil {
		t.Fatalf("解析回执 %q: %v", line, err)
	}
	return res
}

func TestApplyWindowIntentExpandsTheSessionsOwnWindow(t *testing.T) {
	hub, _ := newTestHub(t, false)
	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)

	session := &movableSession{cdpStub: cdpStub{id: "ctx-1"}}
	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", session, expandIntent("ctx-1"))
	if !res.OK {
		t.Fatalf("展开应当成功: %+v", res)
	}
	if res.BrowserPID != 4242 {
		t.Fatalf("回执应带上承载进程 pid: %+v", res)
	}
	if session.raiseCount() != 1 {
		t.Fatalf("真窗口应当被抬到最前一次: raised=%d", session.raiseCount())
	}
	// 回执里的矩形就是真窗口**当前**的矩形（不再缩放），壳拿它当动画落点。
	if res.WindowBounds == nil || res.WindowBounds.Left != 1496 || res.WindowBounds.Width != 1440 {
		t.Fatalf("回执应带上真窗口的实际矩形: %+v", res.WindowBounds)
	}
	// 壳拿到的是它能解的一行（kind=intentAck + ok=true），也就是「可以让位了」。
	ack := readResult(t, viewerSide)
	if ack.Kind != IntentResultKind || !ack.OK || ack.Session != "ctx-1" || ack.Intent != IntentExpand {
		t.Fatalf("回执 = %+v", ack)
	}
}

// 2026-09-24 口径（§4.4 第 3 条）：新壳的 expand 带 deferRaise —— 引擎这一趟只摆窗口、
// 回矩形，**不许激活**（否则窗口先到前台、动画后到，两段脱节）。激活要等动画末段的 raise。
func TestApplyWindowIntentDefersTheRaiseUntilTheAnimationEnds(t *testing.T) {
	hub, _ := newTestHub(t, false)
	session := &movableSession{cdpStub: cdpStub{id: "ctx-1"}}

	deferred := expandIntent("ctx-1")
	deferred.DeferRaise = true
	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", session, deferred)
	if !res.OK || res.WindowBounds == nil || res.WindowBounds.Width != 1440 {
		t.Fatalf("deferRaise 的 expand 仍应回矩形: %+v", res)
	}
	if session.preparedCount() != 1 || session.raiseCount() != 0 || session.activatedCount() != 0 {
		t.Fatalf("只该摆窗口、不该激活: prepared=%d raised=%d activated=%d",
			session.preparedCount(), session.raiseCount(), session.activatedCount())
	}

	raised := ApplyWindowIntent(context.Background(), hub, "ctx-1", session, Intent{
		Version: IntentProtocolVersion, Kind: IntentKind, Intent: IntentRaise, Session: "ctx-1",
	})
	if !raised.OK {
		t.Fatalf("末段 raise 应当成功: %+v", raised)
	}
	if session.activatedCount() != 1 || session.preparedCount() != 1 {
		t.Fatalf("raise 只该激活一次、不该再摆一次: prepared=%d activated=%d",
			session.preparedCount(), session.activatedCount())
	}
}

// 老壳不带 deferRaise：引擎保持「收到 expand 就地抬」的老行为，不许把老壳的真窗口留在后台。
func TestApplyWindowIntentWithoutDeferRaiseStillRaisesImmediately(t *testing.T) {
	hub, _ := newTestHub(t, false)
	session := &movableSession{cdpStub: cdpStub{id: "ctx-1"}}
	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", session, expandIntent("ctx-1"))
	if !res.OK || session.raiseCount() != 1 || session.activatedCount() != 1 {
		t.Fatalf("老壳路径应当就地抬: ok=%v raised=%d activated=%d", res.OK, session.raiseCount(), session.activatedCount())
	}
}

func TestDecodeIntentAcceptsRaiseAndKeepsRejectingJunk(t *testing.T) {
	line := []byte(`{"version":1,"kind":"intent","intent":"raise","session":"ctx-1"}`)
	if in, ok := DecodeIntent(line); !ok || in.Intent != IntentRaise {
		t.Fatalf("raise 意图应当被接受: ok=%v in=%+v", ok, in)
	}
	if _, ok := DecodeIntent([]byte(`{"version":1,"intent":"fly","session":"ctx-1"}`)); ok {
		t.Fatal("未知意图仍然要拒绝")
	}
}

func TestApplyWindowIntentCollapseRestoresTheOriginalSlot(t *testing.T) {
	hub, _ := newTestHub(t, false)
	session := &movableSession{cdpStub: cdpStub{id: "ctx-1"}}
	ApplyWindowIntent(context.Background(), hub, "ctx-1", session, expandIntent("ctx-1"))
	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", session, Intent{
		Version: IntentProtocolVersion, Kind: IntentKind, Intent: IntentCollapse, Session: "ctx-1",
	})
	if !res.OK || session.restored != 1 {
		t.Fatalf("收回应当搬回原位: res=%+v restored=%d", res, session.restored)
	}
}

// 无头/附着会话搬不动窗口：壳必须收到 ok=false，才会回落到自绘放大面板。
func TestApplyWindowIntentWithoutMovableWindowAnswersNotOk(t *testing.T) {
	hub, _ := newTestHub(t, false)
	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)

	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", plainSession{cdpStub{"ctx-1"}}, expandIntent("ctx-1"))
	if res.OK || res.Reason == "" {
		t.Fatalf("没有可搬动窗口时应当 ok=false 且给出原因: %+v", res)
	}
	if ack := readResult(t, viewerSide); ack.OK {
		t.Fatalf("壳不该收到 ok=true: %+v", ack)
	}
}

// 意图点名了一个引擎没有的会话（例如会话已经结束）：同样 ok=false，不许乱搬。
func TestApplyWindowIntentForUnknownSessionAnswersNotOk(t *testing.T) {
	hub, _ := newTestHub(t, false)
	session := &movableSession{cdpStub: cdpStub{id: "ctx-1"}}
	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", session, expandIntent("ctx-9"))
	if res.OK || res.Reason == "" {
		t.Fatalf("未知会话应当 ok=false: %+v", res)
	}
	if session.raiseCount() != 0 {
		t.Fatal("未知会话不得抬任何窗口")
	}
}

// 搬窗口失败（比如 target 已经没了）也要如实回执，壳才能回落。
func TestApplyWindowIntentReportsMoveFailure(t *testing.T) {
	hub, _ := newTestHub(t, false)
	session := &movableSession{cdpStub: cdpStub{id: "ctx-1"}, fail: browser.ErrNoViewerWindow}
	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", session, expandIntent("ctx-1"))
	if res.OK || res.Reason == "" {
		t.Fatalf("搬运失败应当 ok=false: %+v", res)
	}
}

// 壳发来的尺寸不再参与决策（2026-09-22 口径：展开 = 抬到最前，不缩放）：老壳就算发
// 一个畸形尺寸，也照抬不误——尺寸已经不影响引擎做任何事。
func TestApplyWindowIntentIgnoresTheShellSuppliedSize(t *testing.T) {
	hub, _ := newTestHub(t, false)
	session := &movableSession{cdpStub: cdpStub{id: "ctx-1"}}
	intent := expandIntent("ctx-1")
	intent.Size = &WindowSize{Width: 10, Height: 10}
	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", session, intent)
	if !res.OK || session.raiseCount() != 1 {
		t.Fatalf("尺寸不该影响抬窗: res=%+v raised=%d", res, session.raiseCount())
	}
}

// close 不搬任何窗口：它只说「这个人不想再看这个会话了」。所以即使这个会话根本没有
// 可搬动的真窗口（无头 / 附着），也必须 ok=true —— 壳侧的 ✕ 已经生效，不该因为
// 引擎搬不了窗口而回滚成「关不掉」。
func TestApplyWindowIntentCloseNeedsNoMovableWindow(t *testing.T) {
	hub, _ := newTestHub(t, false)
	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)

	closeIntent := func() Intent {
		return Intent{Version: IntentProtocolVersion, Kind: IntentKind, Intent: IntentClose, Session: "ctx-1"}
	}

	movable := &movableSession{cdpStub: cdpStub{id: "ctx-1"}}
	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", movable, closeIntent())
	if !res.OK {
		t.Fatalf("close 不需要可搬动的窗口也得成功: %+v", res)
	}
	if movable.raiseCount() != 0 || movable.restored != 0 {
		t.Fatalf("close 不得动任何窗口: raised=%d restored=%d", movable.raiseCount(), movable.restored)
	}
	ack := readResult(t, viewerSide)
	if !ack.OK || ack.Intent != IntentClose || ack.Session != "ctx-1" {
		t.Fatalf("回执 = %+v", ack)
	}

	headless := ApplyWindowIntent(context.Background(), hub, "ctx-1", plainSession{cdpStub{"ctx-1"}}, closeIntent())
	if !headless.OK {
		t.Fatalf("无头会话的 close 也应当成功: %+v", headless)
	}
}

// close 仍然点名会话：引擎没有的会话照样 ok=false，壳据此知道这条意图其实没落到任何地方。
func TestApplyWindowIntentCloseForUnknownSessionAnswersNotOk(t *testing.T) {
	hub, _ := newTestHub(t, false)
	session := &movableSession{cdpStub: cdpStub{id: "ctx-1"}}
	res := ApplyWindowIntent(context.Background(), hub, "ctx-1", session, Intent{
		Version: IntentProtocolVersion, Kind: IntentKind, Intent: IntentClose, Session: "ctx-9",
	})
	if res.OK || res.Reason == "" {
		t.Fatalf("未知会话的 close 应当 ok=false: %+v", res)
	}
}
