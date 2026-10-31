package viewerhub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"uws/pkg/browser"
)

type fakeSource struct {
	mu        sync.Mutex
	startErr  error
	starts    int
	stops     int
	frame     *browser.ScreencastFrame
	startedAt []browser.ScreencastOptions
}

func (f *fakeSource) StartScreencast(_ context.Context, options browser.ScreencastOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.starts++
	f.startedAt = append(f.startedAt, options)
	return nil
}

func (f *fakeSource) StopScreencast(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

func (f *fakeSource) LatestScreencastFrame() (browser.ScreencastFrame, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frame == nil {
		return browser.ScreencastFrame{}, false
	}
	frame := *f.frame
	frame.JPEG = append([]byte(nil), f.frame.JPEG...)
	return frame, true
}

func (f *fakeSource) pushFrame(jpeg string, sequence int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frame = &browser.ScreencastFrame{
		JPEG:                []byte(jpeg),
		Metadata:            browser.ScreencastMetadata{DeviceWidth: 1280, DeviceHeight: 720},
		CDPSessionID:        "flattened-1",
		ScreencastSessionID: sequence,
		TargetID:            "target-1",
	}
}

func (f *fakeSource) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func newSessionFixture(t *testing.T) (*Hub, *fakeSource, *Session) {
	t.Helper()
	hub, _ := newTestHub(t, false)
	source := &fakeSource{}
	spec := SessionSpec{ID: "ctx-1", AgentName: "UWS task-1", PageTitle: "example.test", URL: "https://example.test/inbox"}
	session, err := NewSession(hub, spec, source, func() SessionSpec {
		return SessionSpec{ID: "ctx-1", AgentName: "UWS task-1", PageTitle: "example.test", URL: "https://example.test/inbox"}
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return hub, source, session
}

func decodeLatest(t *testing.T, hub *Hub, sessionID string) map[string]any {
	t.Helper()
	payload, ok := hub.Latest(sessionID)
	if !ok {
		t.Fatalf("no viewer payload for %s", sessionID)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not the v1 json line: %v (%s)", err, payload)
	}
	return decoded
}

func TestStepStartsTheScreencastAndPublishesTheFrameOnce(t *testing.T) {
	hub, source, session := newSessionFixture(t)
	ctx := context.Background()

	source.pushFrame("/9j/first", 1)
	if !session.Step(ctx) {
		t.Fatal("first Step should report a new frame")
	}
	decoded := decodeLatest(t, hub, "ctx-1")
	if decoded["version"] != float64(1) || decoded["sequence"] != float64(1) {
		t.Fatalf("unexpected envelope: %v", decoded)
	}
	sessionJSON, _ := decoded["session"].(map[string]any)
	if sessionJSON["id"] != "ctx-1" || sessionJSON["agentName"] != "UWS task-1" {
		t.Fatalf("session identity lost: %v", sessionJSON)
	}
	if _, ok := decoded["frame"].(map[string]any); !ok {
		t.Fatalf("frame missing from payload: %v", decoded)
	}
	if got := source.startCount(); got != 1 {
		t.Fatalf("StartScreencast calls = %d", got)
	}

	// Same frame again: no new publish, so an idle page costs no bandwidth.
	if session.Step(ctx) {
		t.Fatal("repeated frame must not be reported as a change")
	}
	source.pushFrame("/9j/second", 2)
	if !session.Step(ctx) {
		t.Fatal("new screencast frame should be published")
	}
	if decoded := decodeLatest(t, hub, "ctx-1"); decoded["sequence"] != float64(2) {
		t.Fatalf("sequence should follow the bridge, got %v", decoded["sequence"])
	}
}

func TestPointerAndActionRideTheSameStream(t *testing.T) {
	hub, source, session := newSessionFixture(t)
	ctx := context.Background()
	source.pushFrame("/9j/first", 1)
	if !session.Step(ctx) {
		t.Fatal("expected the first frame")
	}

	session.ObservePointer(browser.ViewerPointer{X: 320, Y: 180, Kind: browser.ViewerPointerPressed, Button: "left", ClickCount: 1, Buttons: 1})
	session.PublishAction(browser.ViewerAction{Kind: "click", Summary: "正在点击「登录」"})
	if !session.Step(ctx) {
		t.Fatal("pointer/action should count as a change")
	}

	decoded := decodeLatest(t, hub, "ctx-1")
	pointer, ok := decoded["pointer"].(map[string]any)
	if !ok || pointer["kind"] != "mousePressed" || pointer["x"] != float64(320) {
		t.Fatalf("pointer not forwarded: %v", decoded["pointer"])
	}
	action, ok := decoded["action"].(map[string]any)
	if !ok || action["summary"] != "正在点击「登录」" {
		t.Fatalf("action not forwarded: %v", decoded["action"])
	}
}

// 批次 #6 的静默漂移只是「指针轻轻动了一下」：它必须照常喂给小窗光标，但不能算
// Agent 活动——漂移每几秒一次，若照旧重置静默时钟，「没动作就收面板」（2026-09-16
// 现场诉求）就永远不会触发。
func TestIdleDriftPointerDoesNotResetTheQuietClock(t *testing.T) {
	hub, source, session := newSessionFixture(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	session.now = func() time.Time { return now }
	session.idleCloseAfter = 30 * time.Second
	session.lastAgentActivity = now

	source.pushFrame("/9j/first", 1)
	if !session.Step(ctx) {
		t.Fatal("第一帧应当发布出去")
	}

	// 静默第 20 秒来了一次漂移：光标照发（观看壳要看得见指针在动）……
	now = now.Add(20 * time.Second)
	session.ObservePointer(browser.ViewerPointer{X: 640, Y: 360, Kind: browser.ViewerPointerMoved, Idle: true})
	if !session.Step(ctx) {
		t.Fatal("漂移指针应当推给观看壳")
	}
	decoded := decodeLatest(t, hub, "ctx-1")
	pointer, ok := decoded["pointer"].(map[string]any)
	if !ok || pointer["kind"] != "mouseMoved" || pointer["x"] != float64(640) {
		t.Fatalf("漂移指针必须照常转发: %v", decoded["pointer"])
	}

	// ……但它不该把静默时钟顶回去：从最后一次「真动作」算起已过 31s > 30s，收面板。
	now = now.Add(11 * time.Second)
	session.Step(ctx)
	decoded = decodeLatest(t, hub, "ctx-1")
	action, _ := decoded["action"].(map[string]any)
	if action == nil || action["kind"] != viewerSessionClosedKind {
		t.Fatalf("漂移不该重置静默时钟，此时应收面板: %v", decoded)
	}
}

func TestScreencastStartFailureIsRetriedNotFatal(t *testing.T) {
	hub, source, session := newSessionFixture(t)
	source.mu.Lock()
	source.startErr = errors.New("target gone")
	source.mu.Unlock()

	if session.Step(context.Background()) {
		t.Fatal("a failed screencast must not report a frame")
	}
	if _, ok := hub.Latest("ctx-1"); ok {
		t.Fatal("nothing should be published before the stream exists")
	}
	if got := source.startCount(); got != 0 {
		t.Fatalf("failed starts should not count, got %d", got)
	}

	// The retry window expires and the next attempt succeeds.
	session.mu.Lock()
	session.screencastNext = time.Now().Add(-time.Millisecond)
	session.mu.Unlock()
	source.mu.Lock()
	source.startErr = nil
	source.mu.Unlock()
	source.pushFrame("/9j/retry", 7)
	if !session.Step(context.Background()) {
		t.Fatal("retry should start the stream and publish")
	}
	if got := source.startCount(); got != 1 {
		t.Fatalf("StartScreencast calls after retry = %d", got)
	}
}

func TestClosePublishesSessionClosedAndStopsTheStream(t *testing.T) {
	hub, source, session := newSessionFixture(t)
	ctx := context.Background()
	source.pushFrame("/9j/first", 1)
	if !session.Step(ctx) {
		t.Fatal("expected the first frame")
	}

	session.Close()
	decoded := decodeLatest(t, hub, "ctx-1")
	action, ok := decoded["action"].(map[string]any)
	if !ok || action["kind"] != "sessionClosed" {
		t.Fatalf("close should tell the shell to drop the panel: %v", decoded)
	}
	source.mu.Lock()
	stops := source.stops
	source.mu.Unlock()
	if stops != 1 {
		t.Fatalf("StopScreencast calls = %d", stops)
	}
	// Closing twice must not double-stop or panic.
	session.Close()
}

// A viewer must never be able to slow a session down: publishing goes through a
// bounded queue even when the socket peer has stopped reading.
func TestPumpKeepsPublishingWithAStuckViewer(t *testing.T) {
	hub, _ := newTestHub(t, false)
	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)

	source := &fakeSource{}
	session, err := NewSession(hub, SessionSpec{ID: "ctx-9", AgentName: "UWS"}, source, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session.interval = 5 * time.Millisecond
	done := make(chan struct{})
	go func() { session.Pump(ctx); close(done) }()

	for i := 0; i < 40; i++ {
		source.pushFrame("/9j/frame", int64(i+1))
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not stop after ctx cancel")
	}
	if _, ok := hub.Latest("ctx-9"); !ok {
		t.Fatal("pump never published with a stuck viewer")
	}
}

// 任务结束后小窗必须自己走下屏幕：2026-09-16 用户现场是「几个中台测试的小窗挂在
// 屏幕上，关不掉」。收面板不等于结束会话——真窗口、上下文与续页语义都要留着。
func TestQuietSessionDropsItsPanelButStaysAlive(t *testing.T) {
	hub, source, session := newSessionFixture(t)
	ctx := context.Background()

	// 固定时钟：静默时长 30s，先让会话正常工作一次。
	now := time.Unix(1_700_000_000, 0)
	session.now = func() time.Time { return now }
	session.idleCloseAfter = 30 * time.Second
	session.lastAgentActivity = now

	source.pushFrame("/9j/first", 1)
	if !session.Step(ctx) {
		t.Fatal("第一帧应当发布出去")
	}

	// 没到静默阈值：面板还在，画面照流。
	now = now.Add(10 * time.Second)
	source.pushFrame("/9j/second", 2)
	if !session.Step(ctx) {
		t.Fatal("阈值之内画面应当继续推")
	}
	if decoded := decodeLatest(t, hub, "ctx-1"); decoded["sequence"] != float64(2) {
		t.Fatalf("阈值之内不该收面板: %v", decoded)
	}

	// 静默超时：收面板（sessionClosed），并且不再往没有面板的会话推画面。
	now = now.Add(31 * time.Second)
	source.pushFrame("/9j/third", 3)
	session.Step(ctx)
	decoded := decodeLatest(t, hub, "ctx-1")
	action, _ := decoded["action"].(map[string]any)
	if action == nil || action["kind"] != viewerSessionClosedKind {
		t.Fatalf("静默超时应当告诉观看壳收起面板: %v", decoded)
	}
	if _, ok := decoded["frame"].(map[string]any); ok {
		t.Fatalf("收面板后不该再推画面: %v", decoded)
	}
	if session.Closed() {
		t.Fatal("静默只收面板，不结束会话（真窗口与续页都要留着）")
	}

	// Agent 一动，面板就回来：这一帧必须重新发布，哪怕画面没变。
	session.PublishAction(browser.ViewerAction{Kind: "click", Summary: "在「详情」上点击"})
	if !session.Step(ctx) {
		t.Fatal("Agent 又有动作后应当重新发布，让观看壳把面板放回来")
	}
	decoded = decodeLatest(t, hub, "ctx-1")
	if _, ok := decoded["frame"].(map[string]any); !ok {
		t.Fatalf("唤醒后应当推回最新一帧，让观看壳重建面板: %v", decoded)
	}
	action, _ = decoded["action"].(map[string]any)
	if action == nil || action["kind"] != "click" {
		t.Fatalf("唤醒后带上的是那条新动作: %v", decoded)
	}
}

// 用户正把真窗口展开看着时，静默收面板会把唯一的「收回小窗」入口也一起收走。
func TestQuietHoldKeepsThePanelWhileTheUserIsWatching(t *testing.T) {
	hub, source, session := newSessionFixture(t)
	ctx := context.Background()

	now := time.Unix(1_700_000_000, 0)
	session.now = func() time.Time { return now }
	session.idleCloseAfter = 30 * time.Second

	source.pushFrame("/9j/first", 1)
	session.Step(ctx)

	session.SetQuietHold(true)
	now = now.Add(10 * time.Minute)
	source.pushFrame("/9j/second", 2)
	session.Step(ctx)
	decoded := decodeLatest(t, hub, "ctx-1")
	if action, _ := decoded["action"].(map[string]any); action != nil && action["kind"] == viewerSessionClosedKind {
		t.Fatalf("展开看着的时候不该收面板: %v", decoded)
	}

	session.SetQuietHold(false)
	session.Step(ctx)
	decoded = decodeLatest(t, hub, "ctx-1")
	action, _ := decoded["action"].(map[string]any)
	if action == nil || action["kind"] != viewerSessionClosedKind {
		t.Fatalf("用户收回之后应当恢复静默收面板: %v", decoded)
	}
}

// 2026-09-22 现场批次 #2：用户点了小窗上的 ✕。这是**显式**的「我不想看了」，
// 必须立刻收面板——哪怕 Agent 上一毫秒还在动（现场的老毛病正是「关掉之后下一帧
// 又弹回来」）。收面板不等于结束会话：真窗口、上下文与续页语义都留着，Agent 再动
// 一下面板就回来。
func TestCloseGoesQuietImmediatelyEvenWhileTheAgentIsBusy(t *testing.T) {
	hub, source, session := newSessionFixture(t)
	ctx := context.Background()

	now := time.Unix(1_700_000_000, 0)
	session.now = func() time.Time { return now }
	session.idleCloseAfter = 30 * time.Second

	source.pushFrame("/9j/first", 1)
	if !session.Step(ctx) {
		t.Fatal("第一帧应当发布出去")
	}

	// Agent 刚刚动过：静默阈值远没到，靠超时是收不掉的——这正是要测的点。
	session.PublishAction(browser.ViewerAction{Kind: "click", Summary: "在「允许」上点击"})
	now = now.Add(time.Second)
	source.pushFrame("/9j/second", 2)
	session.Step(ctx)
	if decoded := decodeLatest(t, hub, "ctx-1"); decoded["action"] == nil {
		t.Fatalf("关掉之前面板还在推流: %v", decoded)
	}

	// ✕：立刻静默回收。
	session.GoQuiet()
	decoded := decodeLatest(t, hub, "ctx-1")
	action, _ := decoded["action"].(map[string]any)
	if action == nil || action["kind"] != viewerSessionClosedKind {
		t.Fatalf("✕ 应当立刻收面板: %v", decoded)
	}
	if _, ok := decoded["frame"].(map[string]any); ok {
		t.Fatalf("收面板后不该再推画面: %v", decoded)
	}
	if session.Closed() {
		t.Fatal("✕ 只收面板，不结束会话（真窗口与续页都要留着）")
	}

	// Agent 再动一下：面板回来（✕ 不是「永久屏蔽这个会话」）。
	session.PublishAction(browser.ViewerAction{Kind: "click", Summary: "在「下一步」上点击"})
	if !session.Step(ctx) {
		t.Fatal("Agent 又动之后应当重新发布，让观看壳把面板放回来")
	}
	if decoded := decodeLatest(t, hub, "ctx-1"); decoded["action"] == nil {
		t.Fatalf("唤醒后应当带上那条新动作: %v", decoded)
	}
}

// geometrySource 是「知道真窗口在哪儿」的假会话：同时实现 ScreencastSource 与
// browser.ViewerWindowGeometry（规范 §4.4 第 7 条的接线点）。
type geometrySource struct {
	*fakeSource
	mu     sync.Mutex
	bounds browser.ViewerWindowBounds
	ok     bool
	calls  int
}

func (g *geometrySource) RealWindowBounds(context.Context) (browser.ViewerWindowBounds, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.bounds, g.ok
}

func (g *geometrySource) setBounds(bounds browser.ViewerWindowBounds) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bounds = bounds
	g.ok = true
}

func (g *geometrySource) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func newGeometrySession(t *testing.T, source *geometrySource) (*Hub, *Session) {
	t.Helper()
	hub, _ := newTestHub(t, false)
	spec := SessionSpec{
		ID: "ctx-1", AgentName: "UWS task-1",
		PageTitle: "example.test", URL: "https://example.test/inbox",
	}
	session, err := NewSession(hub, spec, source, func() SessionSpec { return spec })
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return hub, session
}

// 附着模式（`browserPID=0`）的命门：真窗口矩形必须随**第一帧**就下发，否则壳只能在
// 用户点过一次小窗之后才认得出进程，盖住真窗口时判据退回焦点 ⇒ 小窗不出现
// （2026-09-24 用户现场；规范 §4.4 第 7 条）。
func TestPublishedFrameCarriesTheRealWindowBounds(t *testing.T) {
	source := &geometrySource{fakeSource: &fakeSource{}}
	source.setBounds(browser.ViewerWindowBounds{Left: 72, Top: 49, Width: 1440, Height: 887})
	hub, session := newGeometrySession(t, source)

	source.pushFrame("/9j/first", 1)
	if !session.Step(context.Background()) {
		t.Fatal("第一帧应当发布出去")
	}
	decoded := decodeLatest(t, hub, "ctx-1")
	bounds, ok := decoded["windowBounds"].(map[string]any)
	if !ok {
		t.Fatalf("帧里必须带 windowBounds（附着模式靠它认窗）: %v", decoded)
	}
	if bounds["left"] != float64(72) || bounds["top"] != float64(49) ||
		bounds["width"] != float64(1440) || bounds["height"] != float64(887) {
		t.Fatalf("矩形走样: %v", bounds)
	}
	if got := source.callCount(); got != 1 {
		t.Fatalf("一帧只该读一次窗口矩形, got %d", got)
	}
}

// 读不到（无头会话 / CDP 失败）时必须**省略**该字段：壳据此退回焦点判据，老行为不变。
func TestWindowBoundsOmittedWhenUnavailable(t *testing.T) {
	source := &geometrySource{fakeSource: &fakeSource{}} // ok=false：读不到
	hub, session := newGeometrySession(t, source)

	source.pushFrame("/9j/first", 1)
	if !session.Step(context.Background()) {
		t.Fatal("第一帧应当发布出去")
	}
	if _, ok := decodeLatest(t, hub, "ctx-1")["windowBounds"]; ok {
		t.Fatal("读不到矩形时不该编出 windowBounds")
	}

	// 根本不知道窗口的源（无头/模拟会话）也一样：整条链路保持原样。
	hub2, source2, session2 := newSessionFixture(t)
	source2.pushFrame("/9j/first", 1)
	if !session2.Step(context.Background()) {
		t.Fatal("第一帧应当发布出去")
	}
	if _, ok := decodeLatest(t, hub2, "ctx-1")["windowBounds"]; ok {
		t.Fatal("老的纯 ScreencastSource 不该凭空多出 windowBounds")
	}
}

// 节流与跟新：同一会话的窗口读取最多每 3s 一次，但用户挪过窗口之后矩形要能更新
// （矩形变化本身也要让这一帧算「新内容」，否则壳永远拿不到更正后的位置）。
func TestWindowBoundsRefreshIsThrottledAndFollowsTheWindow(t *testing.T) {
	source := &geometrySource{fakeSource: &fakeSource{}}
	source.setBounds(browser.ViewerWindowBounds{Left: 0, Top: 0, Width: 1200, Height: 800})
	hub, session := newGeometrySession(t, source)
	now := time.Unix(1_700_000_000, 0)
	session.now = func() time.Time { return now }

	source.pushFrame("/9j/first", 1)
	if !session.Step(context.Background()) {
		t.Fatal("第一帧应当发布出去")
	}
	if got := source.callCount(); got != 1 {
		t.Fatalf("首帧应当立刻读一次, got %d", got)
	}

	now = now.Add(time.Second)
	source.pushFrame("/9j/second", 2)
	session.Step(context.Background())
	if got := source.callCount(); got != 1 {
		t.Fatalf("节流窗口内不该再读, got %d", got)
	}

	now = now.Add(windowBoundsRefreshInterval)
	source.setBounds(browser.ViewerWindowBounds{Left: 300, Top: 180, Width: 1000, Height: 700})
	source.pushFrame("/9j/third", 3)
	if !session.Step(context.Background()) {
		t.Fatal("新帧应当发布出去")
	}
	if got := source.callCount(); got != 2 {
		t.Fatalf("过了节流窗口应当再读一次, got %d", got)
	}
	bounds, _ := decodeLatest(t, hub, "ctx-1")["windowBounds"].(map[string]any)
	if bounds == nil || bounds["left"] != float64(300) || bounds["width"] != float64(1000) {
		t.Fatalf("矩形更新没跟上去: %v", bounds)
	}
}

// 缺省不撤面板（规范 §4.4 第 8 条）：静默只降频，面板存续交给可见性判据。老行为
// （显式设 `UWS_VIEWER_IDLE_CLOSE_SEC`）仍按秒数回收。
func TestIdleSessionsKeepTheirPanelUnlessTheEnvironmentAsks(t *testing.T) {
	t.Setenv(idlePanelCloseEnvKey, "")
	hub, source, session := newSessionFixture(t)
	if session.idleCloseAfter != 0 {
		t.Fatalf("缺省不该再自动收面板, got %v", session.idleCloseAfter)
	}
	now := time.Unix(1_700_000_000, 0)
	session.now = func() time.Time { return now }
	session.lastAgentActivity = now.Add(-time.Hour)

	source.pushFrame("/9j/first", 1)
	if !session.Step(context.Background()) {
		t.Fatal("静默再久，这一帧也应当照发（面板由可见性判据决定去留）")
	}
	decoded := decodeLatest(t, hub, "ctx-1")
	if action, _ := decoded["action"].(map[string]any); action != nil && action["kind"] == viewerSessionClosedKind {
		t.Fatalf("静默不该收面板: %v", decoded)
	}
	if _, ok := decoded["frame"].(map[string]any); !ok {
		t.Fatalf("静默期仍要推画面: %v", decoded)
	}

	// 显式设了秒数就是显式要回收：老语义按原样保留。
	t.Setenv(idlePanelCloseEnvKey, "5")
	if got := idlePanelCloseAfterFromEnvironment(); got != 5*time.Second {
		t.Fatalf("显式秒数应当照旧生效, got %v", got)
	}
}
