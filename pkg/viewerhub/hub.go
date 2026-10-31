// Package viewerhub carries live page frames and agent activity from the UWS
// engine to the native macOS viewer.
//
// Topology (P2, 2026-09-12):
//
//	engine process (listener) --unix socket, JSON lines--> viewer app (client)
//
// One socket carries every session. Each line names its own session, and the
// viewer routes lines to one panel per session. That is what keeps concurrent
// agents from fighting: they share the browser profile (cookies, cache, logins)
// but never a tab, a window or a viewer panel, and a slow viewer can never push
// back on the automation — publishing is a non-blocking overwrite of a bounded
// queue, never a blocking write on the control path.
package viewerhub

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// subscriberQueue is how many updates may wait for one viewer connection.
	// Beyond that the oldest unread update is dropped: the viewer is meant to
	// show the latest state, not a replay.
	subscriberQueue = 8
	// subscriberWriteTimeout bounds one blocking write to a viewer.
	subscriberWriteTimeout = 2 * time.Second
)

// Hub is the engine-side end of the viewer transport: a Unix-domain socket
// listener that fans session updates out to every connected viewer, and keeps
// the latest update per session so a viewer started later paints immediately.
type Hub struct {
	path string
	logf func(string, ...any)

	mu          sync.Mutex
	listener    net.Listener
	subscribers map[*subscriber]struct{}
	latest      map[string][]byte
	order       []string
	closed      bool

	// intentHandler 是反向边（壳 → 引擎）。nil = 只推流、不接收意图。
	intentHandler IntentHandler
}

type Option func(*Hub)

// WithLogger routes hub diagnostics (dropped subscribers, bind problems) to a
// host-provided logger. Viewer transport problems must never be fatal to the
// automation engine.
func WithLogger(logf func(string, ...any)) Option {
	return func(h *Hub) {
		if logf != nil {
			h.logf = logf
		}
	}
}

// IntentHandler receives one decoded viewer intent. It runs on the goroutine
// that reads that viewer's socket, so an implementation must not block
// forever: a slow handler delays only the viewer that sent it, never the
// automation publishing frames.
type IntentHandler func(Intent)

// WithIntentHandler wires the reverse edge (viewer gesture -> engine) at
// construction time. Without it the hub is exactly what it was before: one
// direction, frames only.
func WithIntentHandler(handler IntentHandler) Option {
	return func(h *Hub) { h.SetIntentHandler(handler) }
}

// SetIntentHandler installs or removes (nil) the reverse-edge handler.
func (h *Hub) SetIntentHandler(handler IntentHandler) {
	h.mu.Lock()
	h.intentHandler = handler
	h.mu.Unlock()
}

func (h *Hub) intentHandlerFunc() IntentHandler {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.intentHandler
}

// NewHub creates a hub bound to a socket path. The socket is not created until
// Listen or Serve is called, so an engine that never wires a viewer pays
// nothing.
func NewHub(path string, opts ...Option) *Hub {
	h := &Hub{
		path:        path,
		logf:        func(string, ...any) {},
		subscribers: make(map[*subscriber]struct{}),
		latest:      make(map[string][]byte),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Path returns the socket path this hub owns.
func (h *Hub) Path() string { return h.path }

// Listen binds the Unix-domain socket. A stale socket file left by a crashed
// engine is replaced; a live one is reported instead of being stolen, because
// two engines sharing one socket would silently interleave two automations.
func (h *Hub) Listen() error {
	if h.path == "" {
		return errors.New("viewerhub: socket path is required")
	}
	// Listen then Serve is a supported sequence: a host may bind early to surface
	// bind errors (tests do exactly that) and still hand the hub to Serve. Without
	// this check the second Listen dials its own socket and reports a bogus
	// "another viewer hub is already listening" — which is what the late-viewer
	// socket test hit (2026-09-12). The foreign-hub guard below is untouched.
	h.mu.Lock()
	bound := h.listener != nil && !h.closed
	h.mu.Unlock()
	if bound {
		return nil
	}
	if dir := filepath.Dir(h.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if _, err := os.Stat(h.path); err == nil {
		if conn, dialErr := net.DialTimeout("unix", h.path, 300*time.Millisecond); dialErr == nil {
			_ = conn.Close()
			return errors.New("viewerhub: another viewer hub is already listening on " + h.path)
		}
		if err := os.Remove(h.path); err != nil {
			return err
		}
	}
	listener, err := net.Listen("unix", h.path)
	if err != nil {
		return err
	}
	// The bridge carries only redacted activity labels and page thumbnails; it
	// is nobody else's business on a shared machine.
	if err := os.Chmod(h.path, 0o600); err != nil {
		_ = listener.Close()
		return err
	}
	h.mu.Lock()
	h.listener = listener
	h.mu.Unlock()
	return nil
}

// Serve accepts viewer connections until ctx is cancelled.
func (h *Hub) Serve(ctx context.Context) error {
	if err := h.Listen(); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = h.Close()
	}()
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			if h.isClosed() {
				return nil
			}
			return err
		}
		h.addSubscriber(conn)
	}
}

// SubscriberCount reports how many viewers are attached (0 is normal: the
// engine keeps running whether or not anyone is watching).
func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}

// Latest returns the most recent line published for one session.
func (h *Hub) Latest(sessionID string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	payload, ok := h.latest[sessionID]
	return payload, ok
}

// Publish fans one already-encoded JSON line out to every viewer and remembers
// it as the session's latest state. It never blocks on a viewer.
func (h *Hub) Publish(sessionID string, payload []byte) {
	if sessionID == "" || len(payload) == 0 {
		return
	}
	stored := append([]byte(nil), payload...)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if _, seen := h.latest[sessionID]; !seen {
		h.order = append(h.order, sessionID)
	}
	h.latest[sessionID] = stored
	subs := make([]*subscriber, 0, len(h.subscribers))
	for sub := range h.subscribers {
		subs = append(subs, sub)
	}
	h.mu.Unlock()

	for _, sub := range subs {
		sub.enqueue(stored)
	}
}

// PublishIntentResult answers one viewer intent to every connected viewer. It
// deliberately does not touch the latest-state slot: an ack answers a specific
// gesture, and a viewer that starts later must not be handed an old answer (nor
// have its snapshot overwritten by one).
func (h *Hub) PublishIntentResult(res IntentResult) {
	payload, err := EncodeIntentResult(res)
	if err != nil {
		h.logf("viewerhub: encode intent result: %v", err)
		return
	}
	h.broadcast(payload)
}

// broadcast fans one already-encoded line out to every viewer without remembering
// it. Like Publish it never blocks on a viewer.
func (h *Hub) broadcast(payload []byte) {
	if len(payload) == 0 {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	subs := make([]*subscriber, 0, len(h.subscribers))
	for sub := range h.subscribers {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	for _, sub := range subs {
		sub.enqueue(payload)
	}
}

func (h *Hub) addSubscriber(conn net.Conn) {
	sub := newSubscriber(conn)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return
	}
	h.subscribers[sub] = struct{}{}
	snapshot := make([][]byte, 0, len(h.order))
	for _, sessionID := range h.order {
		if payload, ok := h.latest[sessionID]; ok {
			snapshot = append(snapshot, append([]byte(nil), payload...))
		}
	}
	count := len(h.subscribers)
	h.mu.Unlock()

	h.logf("viewerhub: viewer attached (%d connected)", count)
	go sub.run(h)
	go sub.readIntents(h)
	for _, payload := range snapshot {
		sub.enqueue(payload)
	}
}

func (h *Hub) removeSubscriber(sub *subscriber) {
	h.mu.Lock()
	_, present := h.subscribers[sub]
	delete(h.subscribers, sub)
	count := len(h.subscribers)
	h.mu.Unlock()
	if present {
		h.logf("viewerhub: viewer detached (%d connected)", count)
	}
}

func (h *Hub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// Close drops every viewer, closes the listener, and removes the socket file.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	listener := h.listener
	subs := make([]*subscriber, 0, len(h.subscribers))
	for sub := range h.subscribers {
		subs = append(subs, sub)
	}
	h.subscribers = make(map[*subscriber]struct{})
	h.mu.Unlock()

	for _, sub := range subs {
		sub.close()
	}
	if listener != nil {
		_ = listener.Close()
	}
	if h.path != "" {
		_ = os.Remove(h.path)
	}
	return nil
}

// subscriber owns one viewer connection. Its queue is bounded and drops the
// oldest entry rather than growing: showing the newest frame late is useful,
// showing a backlog is not.
type subscriber struct {
	conn net.Conn

	mu      sync.Mutex
	queue   [][]byte
	closed  bool
	signal  chan struct{}
	done    chan struct{}
	closeMu sync.Once

	// intentMu guards expanded: the sessions this viewer asked to see full size.
	// A viewer that vanishes must not leave a full-size Chrome window behind, so
	// the hub remembers and folds them back on disconnect.
	intentMu sync.Mutex
	expanded map[string]struct{}
}

func newSubscriber(conn net.Conn) *subscriber {
	return &subscriber{
		conn:   conn,
		signal: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

func (s *subscriber) enqueue(payload []byte) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if len(s.queue) >= subscriberQueue {
		s.queue = s.queue[1:]
	}
	s.queue = append(s.queue, payload)
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *subscriber) run(h *Hub) {
	defer func() {
		s.close()
		h.removeSubscriber(s)
	}()
	for {
		select {
		case <-s.done:
			return
		case <-s.signal:
		}
		for {
			s.mu.Lock()
			if len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			payload := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			_ = s.conn.SetWriteDeadline(time.Now().Add(subscriberWriteTimeout))
			if _, err := s.conn.Write(payload); err != nil {
				return
			}
		}
	}
}

// trackIntent remembers which sessions this viewer has expanded. It is local
// bookkeeping, not policy: the engine still decides whether a window can move.
func (s *subscriber) trackIntent(in Intent) {
	s.intentMu.Lock()
	defer s.intentMu.Unlock()
	switch in.Intent {
	case IntentExpand:
		if in.Session == "" {
			return
		}
		if s.expanded == nil {
			s.expanded = make(map[string]struct{})
		}
		s.expanded[in.Session] = struct{}{}
	case IntentCollapse:
		delete(s.expanded, in.Session)
	}
}

// takeExpanded returns and forgets every session this viewer had expanded.
func (s *subscriber) takeExpanded() []string {
	s.intentMu.Lock()
	defer s.intentMu.Unlock()
	if len(s.expanded) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.expanded))
	for sessionID := range s.expanded {
		out = append(out, sessionID)
	}
	s.expanded = nil
	return out
}

// readIntents is the reverse edge: it reads viewer gestures until the viewer
// goes away, then folds back everything that viewer had expanded. Malformed
// lines are skipped silently — a viewer is a local client, but the engine still
// must not act on data it cannot parse.
func (s *subscriber) readIntents(h *Hub) {
	scanner := bufio.NewScanner(s.conn)
	scanner.Buffer(make([]byte, 0, 512), 64<<10)
	for scanner.Scan() {
		intent, ok := DecodeIntent(scanner.Bytes())
		if !ok {
			continue
		}
		s.trackIntent(intent)
		if handler := h.intentHandlerFunc(); handler != nil {
			handler(intent)
		}
	}
	handler := h.intentHandlerFunc()
	if handler == nil {
		return
	}
	for _, sessionID := range s.takeExpanded() {
		handler(Intent{
			Version: IntentProtocolVersion,
			Kind:    IntentKind,
			Intent:  IntentCollapse,
			Session: sessionID,
			Reason:  IntentReasonViewerGone,
		})
	}
}

func (s *subscriber) close() {
	s.closeMu.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.queue = nil
		s.mu.Unlock()
		close(s.done)
		_ = s.conn.Close()
	})
}
