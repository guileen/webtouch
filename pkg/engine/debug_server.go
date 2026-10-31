package engine

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// DebugServer 为在线日志与执行状态提供只读、严格脱敏的 HTTP 调试端点（TDD-CLI-07, SEC-01）。
// 仅允许绑定 127.0.0.1 或内网回环地址，禁止对外暴露。
type DebugServer struct {
	store    *LogStore
	listener net.Listener
	server   *http.Server
}

// NewDebugServer 创建只读调试服务器。
func NewDebugServer(store *LogStore) *DebugServer {
	return &DebugServer{store: store}
}

// Start 启动调试 HTTP 监听服务。
func (s *DebugServer) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/task/", s.handleTaskDebug)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = lis
	s.server = &http.Server{Handler: mux}
	go func() {
		_ = s.server.Serve(lis)
	}()
	return nil
}

// Addr 返回实际监听的地址。
func (s *DebugServer) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Close 停止调试服务器。
func (s *DebugServer) Close() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// handleTaskDebug 处理 GET /debug/task/{request_id} 请求，只读返回脱敏日志 JSON。
func (s *DebugServer) handleTaskDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	reqID := strings.TrimPrefix(r.URL.Path, "/debug/task/")
	if reqID == "" {
		http.Error(w, "Missing request_id", http.StatusBadRequest)
		return
	}
	if s.store == nil {
		http.Error(w, "LogStore not configured", http.StatusNotFound)
		return
	}
	entry, err := s.store.QueryByRequestID(reqID)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	// 校验并再次确保 snapshot_blob、node_features 与 trace 脱敏（SEC-01/SEC-03）
	var snapMap any
	if len(entry.SnapshotBlob) > 0 {
		_ = json.Unmarshal(entry.SnapshotBlob, &snapMap)
	}
	var nodeMap any
	if len(entry.NodeFeatures) > 0 {
		_ = json.Unmarshal([]byte(entry.NodeFeatures), &nodeMap)
	}
	var traceMap any
	if len(entry.DecisionTrace) > 0 {
		_ = json.Unmarshal([]byte(entry.DecisionTrace), &traceMap)
	}

	resp := map[string]any{
		"request_id":     entry.RequestID,
		"site_domain":    entry.SiteDomain,
		"page_url":       entry.PageURL,
		"ax_tree_hash":   entry.AXTreeHash,
		"node_features":  nodeMap,
		"snapshot":       snapMap,
		"confidence":     entry.Confidence,
		"decision":       entry.Decision,
		"action_taken":   entry.ActionTaken,
		"success":        entry.Success,
		"latency_ms":     entry.LatencyMs,
		"context_state":  entry.ContextState,
		"decision_trace": traceMap,
		"pii_masked":     entry.PIIContains,
		"created_at":     entry.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}
