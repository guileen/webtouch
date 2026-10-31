package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"uws/pkg/browser"
	"uws/pkg/viewerhub"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/protobuf/proto"
)

// viewerFakeSession is a CDP session that can stream a page picture and report
// pointer transitions, which is exactly the pair of optional capabilities the
// engine needs to feed a viewer.
type viewerFakeSession struct {
	mu       sync.Mutex
	id       string
	started  bool
	observer browser.PointerObserver
}

func (s *viewerFakeSession) ID() string { return s.id }
func (s *viewerFakeSession) Probe() browser.ProbeResult {
	return browser.ProbeResult{UserAgent: "UWS-Test", Timezone: "UTC"}
}
func (s *viewerFakeSession) Navigate(context.Context, string) error { return nil }
func (s *viewerFakeSession) SetCookie(browser.Cookie)               {}
func (s *viewerFakeSession) Cookies() []browser.Cookie              { return nil }
func (s *viewerFakeSession) ExportState(context.Context, string) (*browser.SerializedSessionState, error) {
	return &browser.SerializedSessionState{}, nil
}
func (s *viewerFakeSession) Close() error    { return nil }
func (s *viewerFakeSession) PageURL() string { return "https://example.test/viewer" }
func (s *viewerFakeSession) EvaluateScript(ctx context.Context, expr string) ([]byte, error) {
	return []byte(`{"result":"evaluated_ok"}`), nil
}

func (s *viewerFakeSession) StartScreencast(context.Context, browser.ScreencastOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	return nil
}

func (s *viewerFakeSession) StopScreencast(context.Context) error { return nil }

func (s *viewerFakeSession) LatestScreencastFrame() (browser.ScreencastFrame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return browser.ScreencastFrame{}, false
	}
	return browser.ScreencastFrame{
		JPEG:                []byte("/9j/frame"),
		Metadata:            browser.ScreencastMetadata{DeviceWidth: 1280, DeviceHeight: 720},
		CDPSessionID:        "fake-" + s.id,
		ScreencastSessionID: 1,
		TargetID:            "target-" + s.id,
	}, true
}

func (s *viewerFakeSession) SetPointerObserver(observer browser.PointerObserver) {
	s.mu.Lock()
	s.observer = observer
	s.mu.Unlock()
}

func (s *viewerFakeSession) emitPointer(pointer browser.ViewerPointer) {
	s.mu.Lock()
	observer := s.observer
	s.mu.Unlock()
	if observer != nil {
		observer.ObservePointer(pointer)
	}
}

type viewerFakeFactory struct {
	mu       sync.Mutex
	sessions []*viewerFakeSession
}

func (f *viewerFakeFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	session := &viewerFakeSession{id: fmt.Sprintf("cdp-%d", len(f.sessions)+1)}
	f.sessions = append(f.sessions, session)
	return session, nil
}

func (f *viewerFakeFactory) last(t *testing.T) *viewerFakeSession {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) == 0 {
		t.Fatal("factory never created a session")
	}
	return f.sessions[len(f.sessions)-1]
}

func waitForViewerPayload(t *testing.T, hub *viewerhub.Hub, sessionID string, check func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		if payload, ok := hub.Latest(sessionID); ok {
			var decoded map[string]any
			if err := json.Unmarshal(payload, &decoded); err == nil {
				last = decoded
				if check(decoded) {
					return decoded
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("viewer payload for %s never matched; last=%v", sessionID, last)
	return nil
}

func TestViewerStreamFollowsOneSessionAndClosesWithIt(t *testing.T) {
	hub := viewerhub.NewHub("")
	factory := &viewerFakeFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), ViewerHub: hub, SessionFactory: factory})
	ctx := mdCtx("coordinator-key")

	resp, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{TaskId: "viewer-task-1"})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	sessionID := resp.GetContextId()

	decoded := waitForViewerPayload(t, hub, sessionID, func(d map[string]any) bool {
		_, hasFrame := d["frame"].(map[string]any)
		return hasFrame && d["session"].(map[string]any)["id"] == sessionID
	})
	sessionJSON := decoded["session"].(map[string]any)
	if name, _ := sessionJSON["agentName"].(string); !strings.HasPrefix(name, "UWS ") {
		t.Fatalf("agent label should identify the session, got %q", name)
	}
	if sessionJSON["pageTitle"] != "example.test" {
		t.Fatalf("page title should follow the live URL, got %v", sessionJSON["pageTitle"])
	}

	// The engine's input path reports where it acted; the shell draws that.
	factory.last(t).emitPointer(browser.ViewerPointer{X: 120, Y: 64, Kind: browser.ViewerPointerPressed, Button: "left", ClickCount: 1, Buttons: 1})
	waitForViewerPayload(t, hub, sessionID, func(d map[string]any) bool {
		pointer, ok := d["pointer"].(map[string]any)
		return ok && pointer["kind"] == "mousePressed" && pointer["x"] == float64(120)
	})

	if _, err := env.client.CloseContext(ctx, &uwspb.CloseContextRequest{}); err != nil {
		t.Fatalf("CloseContext: %v", err)
	}
	waitForViewerPayload(t, hub, sessionID, func(d map[string]any) bool {
		action, ok := d["action"].(map[string]any)
		return ok && action["kind"] == "sessionClosed"
	})
}

// Two concurrent processes may share the Chrome profile (cookies/cache) but must
// never share a tab, a window or a viewer panel.
func TestConcurrentSessionsKeepSeparateTargetsAndStreams(t *testing.T) {
	hub := viewerhub.NewHub("")
	factory := &viewerFakeFactory{}
	keys := []APIKey{
		{Key: "coordinator-key", Role: RoleCoordinator},
		{Key: "coordinator-key-2", Role: RoleCoordinator},
	}
	env := newTestEnvWithOptions(t, Options{Keys: keys, ViewerHub: hub, SessionFactory: factory})

	first, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{TaskId: "task-aaaa1111"})
	if err != nil {
		t.Fatalf("first InitializeContext: %v", err)
	}
	second, err := env.client.InitializeContext(mdCtx("coordinator-key-2"), &uwspb.InitContextRequest{TaskId: "task-bbbb2222"})
	if err != nil {
		t.Fatalf("second InitializeContext: %v", err)
	}
	if first.GetContextId() == second.GetContextId() || first.GetTargetId() == second.GetTargetId() {
		t.Fatalf("concurrent sessions shared a target: %s/%s vs %s/%s",
			first.GetContextId(), first.GetTargetId(), second.GetContextId(), second.GetTargetId())
	}

	for _, resp := range []*uwspb.InitContextResponse{first, second} {
		waitForViewerPayload(t, hub, resp.GetContextId(), func(d map[string]any) bool {
			_, hasFrame := d["frame"].(map[string]any)
			return hasFrame
		})
	}
	firstPayload := waitForViewerPayload(t, hub, first.GetContextId(), func(map[string]any) bool { return true })
	secondPayload := waitForViewerPayload(t, hub, second.GetContextId(), func(map[string]any) bool { return true })
	firstName := firstPayload["session"].(map[string]any)["agentName"]
	secondName := secondPayload["session"].(map[string]any)["agentName"]
	if firstName == secondName {
		t.Fatalf("each session needs its own label, both were %v", firstName)
	}
	if firstPayload["session"].(map[string]any)["id"] == secondPayload["session"].(map[string]any)["id"] {
		t.Fatalf("viewer streams leaked into each other: %v", firstPayload)
	}
}

// Without a hub, or on a session that cannot stream a picture, nothing changes.
func TestViewerWiringIsOptionalAndSkipsNonScreencastSessions(t *testing.T) {
	simEnv := newTestEnvWithOptions(t, Options{Keys: testKeys(), ViewerHub: viewerhub.NewHub("")})
	sim, err := simEnv.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{TaskId: "task-sim"})
	if err != nil {
		t.Fatalf("InitializeContext(sim): %v", err)
	}
	if _, ok := simEnv.srv.viewerHub.Latest(sim.GetContextId()); ok {
		t.Fatal("a simulated session has no page picture and must not publish one")
	}

	plain := newTestEnv(t)
	if _, err := plain.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{TaskId: "task-plain"}); err != nil {
		t.Fatalf("InitializeContext without a viewer hub: %v", err)
	}
}

func TestEvaluateDirectScriptAndPublishViewerAction(t *testing.T) {
	factory := &viewerFakeFactory{}
	hub := viewerhub.NewHub("")
	env := newTestEnvWithOptions(t, Options{
		Keys:           testKeys(),
		SessionFactory: factory,
		ViewerHub:      hub,
	})

	initResp, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId:      "task-eval-1",
		ContextName: proto.String("eval-test"),
	})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}

	execResp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "intent-eval-1",
		TargetSemantic: "direct_evaluate",
		ExpectedAction: "evaluate",
		Value:          proto.String("(async () => ({ result: 'evaluated_ok' }))()"),
		ContextHint:    map[string]string{"context": "eval-test"},
	})
	if err != nil {
		t.Fatalf("ExecuteAction evaluate: %v", err)
	}
	if !execResp.GetSuccess() {
		t.Fatalf("ExecuteAction evaluate failed: %s", execResp.GetErrorReason())
	}
	if execResp.GetExtractedData() != `{"result":"evaluated_ok"}` {
		t.Fatalf("unexpected extracted data: %s", execResp.GetExtractedData())
	}

	// Verify action was published to viewer stream
	waitForViewerPayload(t, hub, initResp.GetContextId(), func(d map[string]any) bool {
		action, ok := d["action"].(map[string]any)
		if !ok {
			return false
		}
		return action["kind"] == "idle"
	})
}

// 2026-09-22 现场批次 #2：壳把一条 close 意图送到引擎（用户点了小窗上的 ✕）。
// 引擎必须把这个会话立刻静默回收——即使 Agent 刚刚还在动——但**不**结束会话、
// **不**关那扇真窗口，也**不**搬动任何窗口。
func TestViewerCloseIntentQuietsTheSessionWithoutEndingIt(t *testing.T) {
	hub := viewerhub.NewHub("")
	factory := &viewerFakeFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), ViewerHub: hub, SessionFactory: factory})
	ctx := mdCtx("coordinator-key")

	resp, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{TaskId: "viewer-close-1"})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	sessionID := resp.GetContextId()
	waitForViewerPayload(t, hub, sessionID, func(d map[string]any) bool {
		_, hasFrame := d["frame"].(map[string]any)
		return hasFrame
	})

	env.srv.handleViewerIntent(viewerhub.Intent{
		Version: viewerhub.IntentProtocolVersion,
		Kind:    viewerhub.IntentKind,
		Intent:  viewerhub.IntentClose,
		Session: sessionID,
	})

	waitForViewerPayload(t, hub, sessionID, func(d map[string]any) bool {
		action, ok := d["action"].(map[string]any)
		return ok && action["kind"] == "sessionClosed"
	})

	env.srv.viewerMu.Lock()
	publisher := env.srv.viewers[sessionID]
	env.srv.viewerMu.Unlock()
	if publisher == nil {
		t.Fatal("close 之后这个会话的推流器不该被摘掉（静默 ≠ 结束）")
	}
	if publisher.Closed() {
		t.Fatal("close 只收面板，不结束会话（真窗口与续页语义都要留着）")
	}
}
