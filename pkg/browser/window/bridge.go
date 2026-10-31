package window

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"uws/pkg/browser/screencast"
)

// BridgeProtocolVersion is deliberately local and append-only. The P1
// transport is a JSON line; selecting a Unix socket or local WebSocket remains
// a host concern, not a Chrome or Viewer concern.
const BridgeProtocolVersion = 1

// ViewerSessionInfo identifies the one UWS session that owns a bridge. The
// caller supplies agent-facing labels; CDP target identifiers remain attached
// to individual frames rather than being used as user-visible session IDs.
type ViewerSessionInfo struct {
	ID        string `json:"id"`
	AgentName string `json:"agentName"`
	PageTitle string `json:"pageTitle"`
	URL       string `json:"url"`
}

// ViewerPointerKind describes an observed CDP pointer transition. Coordinates
// are viewport CSS pixels. The native Viewer owns normalization and drawing so
// this bridge neither reads nor moves the system pointer.
type ViewerPointerKind string

const (
	ViewerPointerMoved    ViewerPointerKind = "mouseMoved"
	ViewerPointerPressed  ViewerPointerKind = "mousePressed"
	ViewerPointerReleased ViewerPointerKind = "mouseReleased"
)

type ViewerPointer struct {
	X          float64           `json:"x"`
	Y          float64           `json:"y"`
	Kind       ViewerPointerKind `json:"kind"`
	Button     string            `json:"button,omitempty"`
	ClickCount int               `json:"clickCount,omitempty"`
	Buttons    int               `json:"buttons,omitempty"`
	// Idle 标记这是静默期漂移（批次 #6），不是 Agent 的真动作。观看壳照旧画光标，
	// 但「Agent 是否还在动」的判定必须忽略它——否则每几秒一次的漂移会一直刷新面板
	// 的静默时钟，5 分钟自动收面板就永远不会触发。
	Idle bool `json:"idle,omitempty"`
}

// ViewerAction is deliberately descriptive only. In particular, callers must
// not put typed values, cookies, page trees, or other page content in Summary.
// The bridge has no command or input-forwarding channel.
type ViewerAction struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary,omitempty"`
}

// ViewerFrame is the page-only JPEG retained by the existing screencast source.
// Metadata and CDP identifiers survive unchanged, allowing the Viewer to map
// overlay coordinates without coupling native UI to CDP.
type ViewerFrame struct {
	JPEG                []byte              `json:"jpeg"`
	Metadata            screencast.Metadata `json:"metadata"`
	CDPSessionID        string              `json:"cdpSessionID"`
	ScreencastSessionID int64               `json:"screencastSessionID"`
	TargetID            string              `json:"targetID"`
}

// ViewerUpdate is a complete latest-state observation for one UWS session.
// Frame is absent until the first screencast frame arrives, so status/action
// telemetry can be delivered without inventing a page image.
type ViewerUpdate struct {
	Version  int               `json:"version"`
	Sequence uint64            `json:"sequence"`
	Session  ViewerSessionInfo `json:"session"`
	Frame    *ViewerFrame      `json:"frame,omitempty"`
	Pointer  *ViewerPointer    `json:"pointer,omitempty"`
	Action   *ViewerAction     `json:"action,omitempty"`
}

// PointerObserver receives CDP pointer transitions for passive viewing. It is
// an observer by contract: an implementation must never move the system
// pointer, inject page DOM, or delay the automation that reported the event.
type PointerObserver interface {
	ObservePointer(pointer ViewerPointer)
}

// PointerObserverSink is the optional session capability that lets a host
// attach a PointerObserver after the session exists. Sessions that do not
// implement it simply never report pointer transitions.
type PointerObserverSink interface {
	SetPointerObserver(observer PointerObserver)
}

// ViewerBridge turns an optional, session-scoped ScreencastSource into the
// native Viewer's passive local-frame boundary. It owns exactly one latest
// frame and latest pointer/action observations; it starts no screencast, opens
// no socket, and performs no Chrome or GUI operation.
type ViewerBridge struct {
	mu sync.RWMutex

	session  ViewerSessionInfo
	sequence uint64
	frame    *ViewerFrame
	pointer  *ViewerPointer
	action   *ViewerAction
	lastKey  viewerFrameKey
}

type viewerFrameKey struct {
	cdpSessionID        string
	screencastSessionID int64
	targetID            string
}

// NewViewerBridge creates a bridge for exactly one named UWS session.
func NewViewerBridge(session ViewerSessionInfo) (*ViewerBridge, error) {
	if strings.TrimSpace(session.ID) == "" {
		return nil, errors.New("viewer bridge: session ID is required")
	}
	return &ViewerBridge{session: session}, nil
}

// RefreshScreencast pulls the existing one-slot screencast state. It publishes
// only a new Chrome screencast frame identity, so a slow Viewer never makes the
// browser retain or replay a frame queue.
func (b *ViewerBridge) RefreshScreencast(source interface {
	LatestScreencastFrame() (screencast.Frame, bool)
}) bool {
	if source == nil {
		return false
	}
	frame, ok := source.LatestScreencastFrame()
	if !ok {
		return false
	}
	return b.PublishFrame(frame)
}

// PublishFrame copies one page JPEG into the bridge's latest-state slot.
// Repeated publication of the same CDP screencast frame is a no-op.
func (b *ViewerBridge) PublishFrame(frame screencast.Frame) bool {
	key := viewerFrameKey{frame.CDPSessionID, frame.ScreencastSessionID, frame.TargetID}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.frame != nil && key == b.lastKey {
		return false
	}
	b.frame = &ViewerFrame{
		JPEG:                append([]byte(nil), frame.JPEG...),
		Metadata:            frame.Metadata,
		CDPSessionID:        frame.CDPSessionID,
		ScreencastSessionID: frame.ScreencastSessionID,
		TargetID:            frame.TargetID,
	}
	b.lastKey = key
	b.sequence++
	return true
}

// PublishPointer records the latest observed virtual pointer state. It is an
// observation only; no native input is emitted or replayed.
func (b *ViewerBridge) PublishPointer(pointer ViewerPointer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pointer = cloneViewerPointer(pointer)
	b.sequence++
}

// PublishAction records a redacted action label for the compact Viewer.
func (b *ViewerBridge) PublishAction(action ViewerAction) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.action = cloneViewerAction(action)
	b.sequence++
}

// LatestViewerUpdate returns a defensive latest-state snapshot. The bool is
// false only before any frame, pointer, or action has been observed.
func (b *ViewerBridge) LatestViewerUpdate() (ViewerUpdate, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.sequence == 0 {
		return ViewerUpdate{}, false
	}
	return ViewerUpdate{
		Version: BridgeProtocolVersion, Sequence: b.sequence, Session: b.session,
		Frame: cloneViewerFrame(b.frame), Pointer: cloneViewerPointerPtr(b.pointer), Action: cloneViewerActionPtr(b.action),
	}, true
}

// WriteJSONLine serializes one complete latest-state update for an already
// approved local transport. It deliberately accepts io.Writer instead of
// choosing a socket, listener, endpoint, or network policy in P1.
func (b *ViewerBridge) WriteJSONLine(w io.Writer) error {
	update, ok := b.LatestViewerUpdate()
	if !ok {
		return errors.New("viewer bridge: no update available")
	}
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	n, err := w.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}

func cloneViewerFrame(frame *ViewerFrame) *ViewerFrame {
	if frame == nil {
		return nil
	}
	copy := *frame
	copy.JPEG = append([]byte(nil), frame.JPEG...)
	return &copy
}

func cloneViewerPointer(pointer ViewerPointer) *ViewerPointer {
	copy := pointer
	return &copy
}

func cloneViewerPointerPtr(pointer *ViewerPointer) *ViewerPointer {
	if pointer == nil {
		return nil
	}
	return cloneViewerPointer(*pointer)
}

func cloneViewerAction(action ViewerAction) *ViewerAction {
	copy := action
	return &copy
}

func cloneViewerActionPtr(action *ViewerAction) *ViewerAction {
	if action == nil {
		return nil
	}
	return cloneViewerAction(*action)
}
