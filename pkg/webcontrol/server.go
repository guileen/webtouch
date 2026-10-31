package webcontrol

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"uws/pkg/browser"
	"uws/pkg/challenge"
	"uws/pkg/qa"
	"uws/pkg/viewerhub"

	"golang.org/x/net/websocket"
)

//go:embed assets/index.html
var indexHTML []byte

// CircuitInfo 表示单个熔断器的对外状态视图。
type CircuitInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	TripReason string `json:"trip_reason,omitempty"`
}

// StatusResponse 是 /api/status 的输出结构。
type StatusResponse struct {
	Status          string        `json:"status"`
	EngineConnected bool          `json:"engine_connected"`
	ActiveSessions  int           `json:"active_sessions"`
	Circuits        []CircuitInfo `json:"circuits"`
	Timestamp       int64         `json:"timestamp"`
}

// QARunRequest 是 /api/qa/run 的请求载荷。
type QARunRequest struct {
	URL       string   `json:"url"`
	Viewports []string `json:"viewports,omitempty"`
}

// ViewerInputRequest 是 /api/viewer/input 的请求载荷。
type ViewerInputRequest struct {
	SessionID string  `json:"session_id,omitempty"`
	Type      string  `json:"type"` // click | input | key
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	Text      string  `json:"text,omitempty"`
	Key       string  `json:"key,omitempty"`
}

// CircuitResetRequest 是 /api/circuits/reset 的请求载荷。
type CircuitResetRequest struct {
	CircuitID string `json:"circuit_id,omitempty"`
}

// Options 是 WebControl 服务的配置选项。
type Options struct {
	Addr             string
	EngineConnected  bool
	Hub              *viewerhub.Hub
	Forwarder        browser.HumanInputForwarder
	BreakersProvider func() map[string]*challenge.CircuitBreaker
	QARunner         func(ctx context.Context, url string) (*qa.QAReport, error)
	Logf             func(string, ...any)
}

// Server 是云端 Web 交互控制面与流式视窗服务端。
type Server struct {
	addr             string
	engineConnected  bool
	hub              *viewerhub.Hub
	forwarder        browser.HumanInputForwarder
	breakersProvider func() map[string]*challenge.CircuitBreaker
	qaRunner         func(ctx context.Context, url string) (*qa.QAReport, error)
	logf             func(string, ...any)

	mux        *http.ServeMux
	httpServer *http.Server
	listener   net.Listener

	mu      sync.Mutex
	wsConns map[*websocket.Conn]struct{}
	closed  bool
}

// NewServer 创建 WebControl 服务。
func NewServer(opts Options) (*Server, error) {
	addr := opts.Addr
	if addr == "" {
		addr = ":8080"
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	bp := opts.BreakersProvider
	if bp == nil {
		bp = func() map[string]*challenge.CircuitBreaker { return nil }
	}

	s := &Server{
		addr:             addr,
		engineConnected:  opts.EngineConnected,
		hub:              opts.Hub,
		forwarder:        opts.Forwarder,
		breakersProvider: bp,
		qaRunner:         opts.QARunner,
		logf:             logf,
		mux:              http.NewServeMux(),
		wsConns:          make(map[*websocket.Conn]struct{}),
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	// 首页：嵌入式单页应用
	s.mux.HandleFunc("/", s.handleIndex)

	// API 接口
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/qa/run", s.handleQARun)
	s.mux.HandleFunc("/api/viewer/input", s.handleViewerInput)
	s.mux.HandleFunc("/api/circuits/reset", s.handleCircuitReset)

	// WebSocket 流式伴生视窗
	s.mux.Handle("/api/viewer/stream", websocket.Handler(s.handleWebSocket))
}

// Handler 返回标准的 http.Handler。
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Addr 返回服务监听地址。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// ListenAndServe 启动 HTTP 服务。
func (s *Server) ListenAndServe() error {
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	return s.Serve(l)
}

// Serve 在指定 Listener 上运行服务。
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	s.listener = l
	s.httpServer = &http.Server{Handler: s.mux}
	s.mu.Unlock()
	return s.httpServer.Serve(l)
}

// Shutdown 优雅关闭服务。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	for ws := range s.wsConns {
		_ = ws.Close()
	}
	s.mu.Unlock()

	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// handleIndex 输出内嵌 WebTouch Studio 前端页面。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

// handleStatus 返回引擎与熔断器当前健康度。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	circuits := make([]CircuitInfo, 0)
	bps := s.breakersProvider()
	for id, cb := range bps {
		if cb != nil {
			reason := ""
			if sig := cb.LastSignal(); sig != nil {
				reason = sig.Reason
			}
			circuits = append(circuits, CircuitInfo{
				ID:         id,
				State:      string(cb.State()),
				TripReason: reason,
			})
		}
	}

	res := StatusResponse{
		Status:          "ok",
		EngineConnected: s.engineConnected,
		ActiveSessions:  len(circuits),
		Circuits:        circuits,
		Timestamp:       time.Now().UnixMilli(),
	}
	writeJSON(w, http.StatusOK, res)
}

// handleQARun 在线触发多视口验收并返回报告。
func (s *Server) handleQARun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req QARunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, qa.QAFailureResponse{
			ExecutionStatus: qa.ExecutionStatusNotRun,
			Verdict:         qa.VerdictBlocked,
			ReasonCode:      "INVALID_JSON",
			Error:           "Invalid JSON: " + err.Error(),
		})
		return
	}
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, qa.QAFailureResponse{
			ExecutionStatus: qa.ExecutionStatusNotRun,
			Verdict:         qa.VerdictBlocked,
			ReasonCode:      "URL_REQUIRED",
			Error:           "url is required",
		})
		return
	}

	if s.qaRunner != nil {
		rep, err := s.qaRunner(r.Context(), req.URL)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, qa.QAFailureResponse{
				ExecutionStatus: qa.ExecutionStatusNotRun,
				Verdict:         qa.VerdictBlocked,
				ReasonCode:      "EXECUTION_FAILED",
				Error:           "QA Run failed: " + err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, rep)
		return
	}

	// Fail-closed: 当 Runner 未初始化时，坚决返回 503 Service Unavailable 与 NOT_RUN + BLOCKED
	writeJSON(w, http.StatusServiceUnavailable, qa.QAFailureResponse{
		ExecutionStatus: qa.ExecutionStatusNotRun,
		Verdict:         qa.VerdictBlocked,
		ReasonCode:      "RUNNER_UNAVAILABLE",
		Error:           "QA Engine Runner not initialized or configured",
	})
}

// handleViewerInput 接收人类逆向交互事件并派发给物理浏览器。
func (s *Server) handleViewerInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ViewerInputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	ev := challenge.HumanInputEvent{
		Type: challenge.HumanInputType(req.Type),
		X:    req.X,
		Y:    req.Y,
		Text: req.Text,
		Key:  req.Key,
	}

	if s.forwarder != nil {
		if err := s.forwarder.DispatchHumanInput(r.Context(), ev); err != nil {
			http.Error(w, "Dispatch failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "event": ev})
}

// handleCircuitReset 处理熔断器重置请求。
func (s *Server) handleCircuitReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req CircuitResetRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	bps := s.breakersProvider()
	resetCount := 0
	for id, cb := range bps {
		if req.CircuitID == "" || req.CircuitID == id {
			if cb != nil {
				cb.Reset()
				resetCount++
			}
		}
	}

	s.BroadcastJSON(map[string]any{
		"type":    "circuit",
		"message": "circuit reset by human intervention",
	})

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reset_count": resetCount})
}

// handleWebSocket 处理伴生流式 WebSocket 客户端连接与双向交互。
func (s *Server) handleWebSocket(ws *websocket.Conn) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ws.Close()
		return
	}
	s.wsConns[ws] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.wsConns, ws)
		s.mu.Unlock()
		_ = ws.Close()
	}()

	// 循环读取客户端发送的逆向输入事件
	for {
		var msg string
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			break
		}
		var req ViewerInputRequest
		if err := json.Unmarshal([]byte(msg), &req); err == nil {
			ev := challenge.HumanInputEvent{
				Type: challenge.HumanInputType(req.Type),
				X:    req.X,
				Y:    req.Y,
				Text: req.Text,
				Key:  req.Key,
			}
			if s.forwarder != nil {
				_ = s.forwarder.DispatchHumanInput(context.Background(), ev)
			}
		}
	}
}

// BroadcastFrame 向所有已连接的伴生视窗 WebSocket 广播新帧。
func (s *Server) BroadcastFrame(sessionID string, pngBytes []byte) {
	if len(pngBytes) == 0 {
		return
	}
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	s.BroadcastJSON(map[string]any{
		"type":       "frame",
		"session_id": sessionID,
		"data":       b64,
	})
}

// BroadcastJSON 向所有已连接的 WebSocket 广播任意 JSON 消息。
func (s *Server) BroadcastJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := string(data)

	s.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.wsConns))
	for ws := range s.wsConns {
		conns = append(conns, ws)
	}
	s.mu.Unlock()

	for _, ws := range conns {
		_ = websocket.Message.Send(ws, msg)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
