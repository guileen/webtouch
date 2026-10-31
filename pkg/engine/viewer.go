package engine

import (
	"context"
	"net/url"
	"strings"
	"time"

	"uws/pkg/browser"
	"uws/pkg/viewerhub"
)

// Viewer wiring (P2, 2026-09-12).
//
// Every UWS session that can expose Page.startScreencast gets one passive
// viewer stream. Isolation rules this file exists to keep:
//
//   - one stream per BrowserContext, keyed by BrowserContext.ID => a session
//     can only ever publish its own tab/window/picture (R5);
//   - sessions share the Chrome profile (cookies, cache, logins) because that
//     is the product value, but never a tab, a window or a viewer panel;
//   - publishing is non-blocking, so a missing or slow viewer cannot stall or
//     change automation; a session with no viewer costs nothing but a tick.
//
// The viewer hub is optional: with Options.ViewerHub == nil every function here
// is a no-op and the engine behaves exactly as before.

// attachViewer wires one freshly acquired browser context into the viewer hub.
func (s *Server) attachViewer(bc *browser.BrowserContext, taskID, contextName string) {
	if s.viewerHub == nil || bc == nil {
		return
	}
	session := bc.Session()

	// Only real CDP sessions can stream a page picture; simulated sessions and
	// headless targets without a screencast stay silent instead of pretending.
	source, ok := session.(browser.ScreencastSource)
	if !ok {
		return
	}

	s.viewerMu.Lock()
	_, already := s.viewers[bc.ID]
	s.viewerMu.Unlock()
	if already {
		return
	}

	label := viewerAgentLabel(taskID, contextName)
	spec := func() viewerhub.SessionSpec {
		return viewerhub.SessionSpec{
			ID:         bc.ID,
			AgentName:  label,
			PageTitle:  viewerPageTitle(session),
			URL:        sessionPageURL(session),
			BrowserPID: viewerBrowserPID(session),
		}
	}
	publisher, err := viewerhub.NewSession(s.viewerHub, spec(), source, spec)
	if err != nil {
		return
	}
	// The engine's input path reports pointer transitions to the same stream;
	// the shell draws them with its own cursor (no DOM injection, no system
	// pointer movement).
	if sink, ok := session.(browser.PointerObserverSink); ok {
		sink.SetPointerObserver(publisher)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.viewerMu.Lock()
	if _, raced := s.viewers[bc.ID]; raced {
		s.viewerMu.Unlock()
		cancel()
		return
	}
	s.viewers[bc.ID] = publisher
	s.viewerStop[bc.ID] = cancel
	s.viewerSessions[bc.ID] = session
	s.viewerMu.Unlock()

	go func() {
		// Paint the first frame as soon as it exists, then settle into the
		// adaptive pump (active rate while the page moves, idle rate when not).
		_ = publisher.Step(ctx)
		publisher.Pump(ctx)
	}()
}

// detachViewer stops one session's stream and tells viewers to drop its panel.
func (s *Server) detachViewer(contextID string) {
	if s.viewerHub == nil || contextID == "" {
		return
	}
	s.viewerMu.Lock()
	publisher := s.viewers[contextID]
	cancel := s.viewerStop[contextID]
	delete(s.viewers, contextID)
	delete(s.viewerStop, contextID)
	delete(s.viewerSessions, contextID)
	s.viewerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if publisher != nil {
		publisher.Close()
	}
}

// publishViewerAction 向伴生壳发布当前会话的最新活动语义（如 "thinking", "guard_challenge", "idle"）。
func (s *Server) publishViewerAction(contextID string, action browser.ViewerAction) {
	if s.viewerHub == nil || contextID == "" {
		return
	}
	s.viewerMu.Lock()
	pub := s.viewers[contextID]
	s.viewerMu.Unlock()
	if pub != nil {
		pub.PublishAction(action)
	}
}

// viewerIntentTimeout bounds one window move: a viewer gesture must never hold
// the socket goroutine longer than a human would wait for the window to move.
const viewerIntentTimeout = 5 * time.Second

// handleViewerIntent is the reverse edge of the viewer transport (2026-09-12).
//
// The shell reports exactly two user gestures — "open this session full size"
// and "the user went back to their own work" — and the engine raises that
// session's own real window to the front (展开态 = 真页面：用户要能点、能滚、能过人机
// 验证，所以展开的是真窗口，不是放大的图片；2026-09-22 口径：只抬到最前，不搬位置、
// 不缩放). It never injects page input, never clicks for the user, and never
// touches another session's window (R5). An intent for a session this engine does
// not have is answered ok=false, which is what makes the shell fall back to its
// drawn expanded panel instead of waiting forever.
func (s *Server) handleViewerIntent(in viewerhub.Intent) {
	if s.viewerHub == nil {
		return
	}
	s.viewerMu.Lock()
	session := s.viewerSessions[in.Session]
	publisher := s.viewers[in.Session]
	s.viewerMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), viewerIntentTimeout)
	defer cancel()
	result := viewerhub.ApplyWindowIntent(ctx, s.viewerHub, in.Session, session, in)
	if publisher == nil || !result.OK {
		return
	}
	// 真窗口展开在屏幕上期间绝不自动收面板（2026-09-16）：那颗「收回小窗」胶囊
	// 是用户唯一的收回入口，面板一收，用户就只剩一扇不知道该往哪放的 Chrome 窗口。
	switch in.Intent {
	case viewerhub.IntentExpand:
		publisher.SetQuietHold(true)
	case viewerhub.IntentCollapse:
		publisher.SetQuietHold(false)
	case viewerhub.IntentClose:
		// 用户点了小窗上的 ✕：壳已经真丢掉了那块面板，引擎这边立刻静默回收
		// （停推流 + 一条 sessionClosed），免得继续往一个没有面板的会话推帧。
		// 不结束会话、不关真窗口、不搬动任何窗口。
		publisher.GoQuiet()
	}
}

// closeViewers stops every pump; called from Server.Close.
func (s *Server) closeViewers() {
	if s.viewerHub == nil {
		return
	}
	s.viewerMu.Lock()
	ids := make([]string, 0, len(s.viewers))
	for id := range s.viewers {
		ids = append(ids, id)
	}
	s.viewerMu.Unlock()
	for _, id := range ids {
		s.detachViewer(id)
	}
}

// viewerAgentLabel is the user-facing "which agent is operating" label. A named
// session (v1.5.6 Session ID) is the friendliest identity; a default context
// falls back to the task ID, so two concurrent CLI processes stay tellable
// apart instead of both reading "UWS".
func viewerAgentLabel(taskID, contextName string) string {
	if name := strings.TrimSpace(contextName); name != "" {
		return "UWS " + name
	}
	compact := strings.ReplaceAll(strings.TrimSpace(taskID), "-", "")
	if compact == "" {
		return "UWS"
	}
	if len(compact) > 8 {
		compact = compact[:8]
	}
	return "UWS " + compact
}

// viewerBrowserPID 报告承载这个会话真页面的浏览器进程（附着模式 = 0 = 未知）。
// 壳从第一帧起就拿到 pid 时，第 5 条的遮挡判据不必等用户先点一次小窗（规范 §4.4 第 7 条）。
func viewerBrowserPID(session browser.CDPSession) int {
	if info, ok := session.(browser.BrowserProcessInfo); ok {
		return info.BrowserPID()
	}
	return 0
}

// viewerPageTitle labels a session by its current host: it is cheap to compute
// every tick and changes exactly when the agent navigates.
func viewerPageTitle(session browser.CDPSession) string {
	raw := sessionPageURL(session)
	if strings.TrimSpace(raw) == "" {
		return "about:blank"
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return raw
}
