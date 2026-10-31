package viewerhub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"uws/pkg/browser"
)

// Session pump tuning. The fast interval keeps a click-to-picture delay inside
// the viewer's budget (R7: <=150ms perceived); the idle interval is the
// "nothing is happening" backoff so an idle agent costs almost no CPU/battery.
const (
	defaultActiveInterval = 100 * time.Millisecond
	defaultIdleInterval   = time.Second
	// screencastRetryDelay spaces out Page.startScreencast retries so a session
	// whose target is gone does not spam CDP.
	screencastRetryDelay = 3 * time.Second
	// viewerSessionClosedKind tells the shell to drop this session's panel.
	viewerSessionClosedKind = "sessionClosed"
	// 面板静默时长：任务不再动（无动作、无指针输入）这么久之后，观看壳收起面板。
	// **缺省不撤**（2026-09-24 用户现场口径，规范 §4.4 第 8 条）：面板存续由「真窗口你看不
	// 看得见」这条判据决定，静默只降推流频率。想回收长会话的 screencast 才显式设它。
	// 历史：2026-09-16 缺省是 5 分钟——那天用户看到测试结束后几个会话的小窗一直挂着；
	// 但 2026-09-24 的现场是反过来的：面板被静默收掉之后，用户盖住真窗口也看不到小窗。
	// idlePanelCloseEnvKey 设置静默时长（秒）；未设 / 0 / off / none = 不撤面板。
	idlePanelCloseEnvKey = "UWS_VIEWER_IDLE_CLOSE_SEC"
	// windowBoundsRefreshInterval 是「真窗口矩形」的下发节流：同一会话最多每这么久读一次
	// CDP（规范 §4.4 第 7 条）。壳只需要认一次窗，节流只为了让「用户挪了窗口」能跟上。
	windowBoundsRefreshInterval = 3 * time.Second
	// windowBoundsReadTimeout 给这类只读 CDP 往返封顶：它挂在推流循环上，不许把 pump 拖住。
	windowBoundsReadTimeout = 2 * time.Second
)

// idlePanelCloseAfterFromEnvironment 解析「面板静默多久自动收起」。
func idlePanelCloseAfterFromEnvironment() time.Duration {
	raw := strings.TrimSpace(os.Getenv(idlePanelCloseEnvKey))
	if raw == "" {
		return 0
	}
	switch strings.ToLower(raw) {
	case "0", "off", "no", "false", "none", "-":
		return 0
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// SessionSpec is the human-facing identity of one UWS session. Title and URL
// may change while the session runs, which is why they are re-read every tick.
type SessionSpec struct {
	ID        string
	AgentName string
	PageTitle string
	URL       string
	// BrowserPID is the process hosting this session's real page (0 = unknown,
	// e.g. attach mode where the browser belongs to the user). The shell uses it
	// to tell "the user clicked our real page" from "the user went back to their
	// own work" when deciding to fold the expanded page back (R4b).
	BrowserPID int
}

// SessionSpecProvider returns the current labels for a session. It must be
// cheap: it is called once per pump tick.
type SessionSpecProvider func() SessionSpec

// ViewerSessionLiveness is the optional capability a screencast source may also
// implement: it reports whether the session's page still exists. A user who
// closes that window by hand makes it false, and the viewer must be told
// instead of being left with a picture that will never update again
// (2026-09-12 真机复跑：用户关掉真窗口后，壳还停在最后一张截图上）。
type ViewerSessionLiveness interface {
	ViewerAlive() bool
}

// Session binds one UWS session to the viewer transport.
//
// It owns exactly one browser.ViewerBridge (latest frame + pointer + action),
// one screencast subscription, and one publish loop. It never blocks the caller:
// every Send ends in Hub.Publish, which is a bounded, non-blocking overwrite.
type Session struct {
	hub    *Hub
	bridge *browser.ViewerBridge
	source browser.ScreencastSource
	spec   SessionSpecProvider
	// geometry 可选：会话知道真窗口在哪儿时，随帧把矩形交给壳（§4.4 第 7 条）。
	geometry browser.ViewerWindowGeometry

	interval     time.Duration
	idleInterval time.Duration
	now          func() time.Time

	mu             sync.Mutex
	lastSequence   uint64
	lastLabelsKey  string
	screencastUp   bool
	screencastNext time.Time
	idleTicks      int
	closed         bool
	// quiet = 面板已收起（Agent 静默超时），但会话仍然活着：下一帧或下一条动作
	// 就把它叫回来。quietHold = 用户正把真窗口展开看着，期间绝不收面板。
	quiet             bool
	quietHold         bool
	lastAgentActivity time.Time
	idleCloseAfter    time.Duration
	// windowBounds = 最近一次读到的真窗口矩形（nil = 还不知道：无头会话 / 读不到）。
	// 它随帧下发，附着模式的壳靠它认窗 → 补出 browserPID。
	windowBounds     *browser.ViewerWindowBounds
	windowBoundsNext time.Time
}

// NewSession validates identities and wires one passive bridge. It starts no
// screencast, opens no socket and touches no Chrome: Pump does that work.
func NewSession(hub *Hub, spec SessionSpec, source browser.ScreencastSource, provider SessionSpecProvider) (*Session, error) {
	if hub == nil {
		return nil, errNilHub
	}
	bridge, err := browser.NewViewerBridge(browser.ViewerSessionInfo{
		ID:        spec.ID,
		AgentName: spec.AgentName,
		PageTitle: spec.PageTitle,
		URL:       spec.URL,
	})
	if err != nil {
		return nil, err
	}
	if provider == nil {
		frozen := spec
		provider = func() SessionSpec { return frozen }
	}
	session := &Session{
		hub:            hub,
		bridge:         bridge,
		source:         source,
		spec:           provider,
		interval:       defaultActiveInterval,
		idleInterval:   defaultIdleInterval,
		now:            time.Now,
		idleCloseAfter: idlePanelCloseAfterFromEnvironment(),
	}
	if geometry, ok := source.(browser.ViewerWindowGeometry); ok {
		session.geometry = geometry
	}
	session.lastAgentActivity = session.now()
	return session, nil
}

// errNilHub mirrors browser.NewViewerBridge's validation style: a misconfigured
// publisher is a programming error, not a runtime condition.
var errNilHub = jsonError("viewerhub: hub is required")

type jsonError string

func (e jsonError) Error() string { return string(e) }

// ObservePointer implements browser.PointerObserver: the engine's input path
// reports where it acted, and the shell draws that with its own cursor. Nothing
// here reads or moves the system pointer.
func (s *Session) ObservePointer(pointer browser.ViewerPointer) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	s.bridge.PublishPointer(pointer)
	// 静默期漂移也只是「指针动了一下」：光标照画，但它不算 Agent 活动，
	// 否则漂移会一直把面板的静默时钟顶回去，面板就再也收不掉了。
	if !pointer.Idle {
		s.noteAgentActivity()
	}
}

// PublishAction records one redacted activity label for the shell.
func (s *Session) PublishAction(action browser.ViewerAction) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	s.bridge.PublishAction(action)
	// "idle" 只是「引擎这一轮没动作」，它出现得比真正的动作还勤；把它当成活动
	// 信号会让面板永远静不下来，所以只认真正的动作语义。
	if action.Kind != "idle" {
		s.noteAgentActivity()
	}
}

// quietLogLine 是行为可观测的一行：用户看到面板自己消失/自己回来时，能在
// ~/.uws/engine.log 里对上原因，而不是以为壳坏了。
func (s *Session) quietLogLine(format string, args ...any) {
	if s.hub == nil {
		return
	}
	s.hub.logf(format, args...)
}

// noteAgentActivity 记录一次「Agent 真的在动」，并在需要时把已收起的面板叫回来。
func (s *Session) noteAgentActivity() {
	s.mu.Lock()
	s.lastAgentActivity = s.now()
	woke := s.quiet
	s.quiet = false
	if woke {
		// 面板已收起：强制下一帧重新发布，观看壳据此重建这个会话的面板。
		s.lastSequence = 0
		s.lastLabelsKey = ""
	}
	s.mu.Unlock()
	if woke {
		s.quietLogLine("viewerhub: 会话 %s 又有新动作，面板已放回", s.currentSpec().ID)
	}
}

// SetQuietHold 标记「用户正把真窗口展开看着」。展开期间绝不自动收面板：屏幕上
// 那扇真窗口是引擎搬过去的，壳把面板收掉 = 用户失去唯一的收回入口。
func (s *Session) SetQuietHold(hold bool) {
	s.mu.Lock()
	s.quietHold = hold
	if hold {
		s.lastAgentActivity = s.now()
		woke := s.quiet
		s.quiet = false
		if woke {
			s.lastSequence = 0
			s.lastLabelsKey = ""
		}
	}
	s.mu.Unlock()
}

// quietExpired 判断是否该收起面板：静默超时且用户没在看着这扇真窗口。
func (s *Session) shouldGoQuiet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.quiet || s.quietHold || s.idleCloseAfter <= 0 {
		return false
	}
	return s.now().Sub(s.lastAgentActivity) > s.idleCloseAfter
}

// GoQuiet 是用户显式要求「我不想再看了」（小窗上的 ✕）：立刻收面板，不等静默超时。
// 它与静默收面板走同一条回收路径（停推流、发一条 sessionClosed），语义也一样——
// 只收面板：会话、真窗口、上下文与续页语义全部保留，Agent 再动一下面板就回来。
func (s *Session) GoQuiet() {
	// 用户已经不想看了，任何「正展开着」的保留态都该让位，否则 close 会被静默忽略。
	s.SetQuietHold(false)
	s.publishQuietNotice()
}

// publishQuietNotice 收起面板：发一条 sessionClosed（壳的既有契约就是「丢掉这个
// 会话的面板」），但**不**结束会话——真窗口、上下文与续页语义都原样保留，
// Agent 再动一下面板就回来。
func (s *Session) publishQuietNotice() {
	s.mu.Lock()
	if s.closed || s.quiet {
		s.mu.Unlock()
		return
	}
	s.quiet = true
	sessionID := s.currentSpec().ID
	silent := s.idleCloseAfter
	s.mu.Unlock()
	s.quietLogLine("viewerhub: 会话 %s 静默 %s，面板已收起（会话与真窗口保留，Agent 一动就回来）",
		sessionID, silent)

	payload, err := json.Marshal(wireUpdate{
		Version: 1,
		Session: browser.ViewerSessionInfo{ID: s.currentSpec().ID},
		Action:  &browser.ViewerAction{Kind: viewerSessionClosedKind, Summary: "会话静默，面板已收起"},
	})
	if err == nil {
		s.hub.Publish(s.currentSpec().ID, append(payload, '\n'))
	}
}

// Step runs one pump iteration and reports whether the page picture changed.
// It is exported so tests can drive the loop deterministically.
func (s *Session) Step(ctx context.Context) bool {
	if !s.alive() {
		s.Close()
		return false
	}
	if s.shouldGoQuiet() {
		s.publishQuietNotice()
	}
	if !s.ensureScreencast(ctx) {
		return false
	}
	changed := s.bridge.RefreshScreencast(s.source)
	return s.publish(ctx) || changed
}

// Pump publishes until ctx is cancelled, at the active rate while frames keep
// changing and at the idle rate once the page goes quiet.
func (s *Session) Pump(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	idleEvery := int(s.idleInterval / s.interval)
	if idleEvery < 1 {
		idleEvery = 1
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !s.alive() {
			// 页面 target 已经没了（用户手动关掉了那扇真窗口）：告诉观看壳收起面板，
			// 而不是让它停在一张永远不会再更新的图上。
			s.Close()
			return
		}
		if s.shouldGoQuiet() {
			// 任务早已结束（无动作、无指针），页面也不再有人看：收面板。
			// 这与 Close 不同——会话还活着，Agent 一动面板就回来。
			s.publishQuietNotice()
		}
		if s.idleTicks >= idleEvery {
			s.idleTicks++
			if s.idleTicks%idleEvery != 0 {
				continue
			}
		}
		changed := false
		if s.ensureScreencast(ctx) {
			changed = s.bridge.RefreshScreencast(s.source)
			changed = s.publish(ctx) || changed
		}
		if changed {
			s.idleTicks = 0
		} else {
			s.idleTicks++
		}
	}
}

// alive 询问画面源页面是否还在；不支持活性判定的源一律按「还在」处理。
func (s *Session) alive() bool {
	if live, ok := s.source.(ViewerSessionLiveness); ok {
		return live.ViewerAlive()
	}
	return true
}

func (s *Session) ensureScreencast(ctx context.Context) bool {
	if s.source == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.screencastUp {
		return s.screencastUp
	}
	if s.now().Before(s.screencastNext) {
		return false
	}
	if err := s.source.StartScreencast(ctx, browser.DefaultScreencastOptions()); err != nil {
		// A missing target is normal (tab closed); retry later without failing.
		s.screencastNext = s.now().Add(screencastRetryDelay)
		return false
	}
	s.screencastUp = true
	return true
}

// refreshWindowBounds 定期把「这扇真窗口在哪儿」读出来，供随帧下发（规范 §4.4 第 7 条）。
//
// 为什么必须在用户点小窗之前就有：附着模式（`--attach` 到用户自己的 Chrome）拿不到
// browserPID，壳只能按矩形在屏上认窗 → 认进程；以前矩形只随「展开回执」回来，于是
// 「盖住真窗口但没点过小窗」时判据退回焦点 ⇒ 判「看得见」⇒ 小窗不出现（2026-09-24
// 用户现场）。只读、节流、且失败保留上一次的值（窗口一时读不到不该让壳丢掉已认出的窗）。
func (s *Session) refreshWindowBounds(ctx context.Context) {
	if s.geometry == nil {
		return
	}
	now := s.now()
	s.mu.Lock()
	if s.closed || now.Before(s.windowBoundsNext) {
		s.mu.Unlock()
		return
	}
	s.windowBoundsNext = now.Add(windowBoundsRefreshInterval)
	s.mu.Unlock()

	readCtx, cancel := context.WithTimeout(ctx, windowBoundsReadTimeout)
	defer cancel()
	bounds, ok := s.geometry.RealWindowBounds(readCtx)
	if !ok || bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	s.mu.Lock()
	s.windowBounds = &bounds
	s.mu.Unlock()
}

// currentWindowBounds 是最近一次读到的真窗口矩形（nil = 还不知道）。
func (s *Session) currentWindowBounds() *browser.ViewerWindowBounds {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windowBounds
}

// encodeViewerBounds 把窗口矩形编成 wire 形态；nil / 空矩形 = 省略该字段（「拿不到」与
// 老壳都走这里，行为不变）。
func encodeViewerBounds(bounds *browser.ViewerWindowBounds) *ViewerBoundsJSON {
	if bounds == nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return nil
	}
	return &ViewerBoundsJSON{
		Left:   bounds.Left,
		Top:    bounds.Top,
		Width:  bounds.Width,
		Height: bounds.Height,
	}
}

// windowBoundsKey 把矩形并进「这一帧算不算新内容」的比较里：矩形变了就值得重发一次，
// 否则壳可能永远拿不到更正后的窗口位置。
func windowBoundsKey(bounds *browser.ViewerWindowBounds) string {
	if bounds == nil {
		return ""
	}
	return fmt.Sprintf("%d,%d,%d,%d", bounds.Left, bounds.Top, bounds.Width, bounds.Height)
}

// publish encodes and ships the current state. It reports whether anything new
// actually went out, which is what drives the idle backoff.
func (s *Session) publish(ctx context.Context) bool {
	update, ok := s.bridge.LatestViewerUpdate()
	if !ok {
		return false
	}
	// 面板已收起（Agent 静默）：不再往一个没有面板的会话推画面，等
	// noteAgentActivity/SetQuietHold 把它唤醒（醒来时会强制重发一次）。
	s.mu.Lock()
	quiet := s.quiet
	s.mu.Unlock()
	if quiet {
		return false
	}
	spec := s.spec()
	// 真窗口矩形要跟着这一帧一起走（§4.4 第 7 条）：先刷新，再算去重键。
	s.refreshWindowBounds(ctx)
	bounds := s.currentWindowBounds()
	key := strings.Join(
		[]string{spec.AgentName, spec.PageTitle, spec.URL, windowBoundsKey(bounds)}, "\x00")

	s.mu.Lock()
	if s.closed || (update.Sequence == s.lastSequence && key == s.lastLabelsKey) {
		s.mu.Unlock()
		return false
	}
	s.lastSequence = update.Sequence
	s.lastLabelsKey = key
	s.mu.Unlock()

	payload, err := json.Marshal(wireUpdate{
		Version:  update.Version,
		Sequence: update.Sequence,
		Session: browser.ViewerSessionInfo{
			ID:        spec.ID,
			AgentName: spec.AgentName,
			PageTitle: spec.PageTitle,
			URL:       spec.URL,
		},
		Frame:      update.Frame,
		Pointer:    update.Pointer,
		Action:     update.Action,
		BrowserPID: spec.BrowserPID,
		// WindowBounds 是那扇真窗口此刻的矩形（Chrome 坐标空间）：附着模式的壳靠它在
		// 屏上认窗 → 认进程，第 5 条的遮挡判据从第一帧起就生效（§4.4 第 7 条）。
		WindowBounds: encodeViewerBounds(bounds),
	})
	if err != nil {
		return false
	}
	s.hub.Publish(spec.ID, append(payload, '\n'))
	return true
}

// Closed reports whether this session already ended (page gone or engine
// shutting down). A quiet session is *not* closed: it only has no panel.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close stops the screencast and tells every viewer to drop this session's
// panel, so a finished agent does not leave a stale window on screen.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	screencastUp := s.screencastUp
	s.screencastUp = false
	s.mu.Unlock()

	payload, err := json.Marshal(wireUpdate{
		Version: 1,
		Session: browser.ViewerSessionInfo{ID: s.currentSpec().ID},
		Action:  &browser.ViewerAction{Kind: viewerSessionClosedKind, Summary: "会话已结束"},
	})
	if err == nil {
		s.hub.Publish(s.currentSpec().ID, append(payload, '\n'))
	}
	if screencastUp && s.source != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.source.StopScreencast(ctx)
	}
}

func (s *Session) currentSpec() SessionSpec {
	if s.spec == nil {
		return SessionSpec{}
	}
	return s.spec()
}

// wireUpdate is browser.ViewerUpdate with session labels supplied fresh by the
// engine; the JSON shape is the frozen v1 contract the native shell decodes.
type wireUpdate struct {
	Version    int                       `json:"version"`
	Sequence   uint64                    `json:"sequence"`
	Session    browser.ViewerSessionInfo `json:"session"`
	Frame      *browser.ViewerFrame      `json:"frame,omitempty"`
	Pointer    *browser.ViewerPointer    `json:"pointer,omitempty"`
	Action     *browser.ViewerAction     `json:"action,omitempty"`
	BrowserPID int                       `json:"browserPID,omitempty"`
	// WindowBounds = 真窗口此刻的矩形（Chrome 坐标空间，与 `CGWindowList` 同一套左上原点）。
	// 老壳忽略它；新壳用它补出附着模式的 browserPID（§4.4 第 7 条）。
	WindowBounds *ViewerBoundsJSON `json:"windowBounds,omitempty"`
}
