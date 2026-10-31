package gateway

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxBodyBytes = 1 << 20 // 1MB

// Handler 是网关 HTTP 处理器（纯 stdlib，默认仅本机监听）。
type Handler struct {
	g   *Gateway
	log *log.Logger
}

// NewHandler 构造 HTTP 处理器。
func NewHandler(g *Gateway, logger *log.Logger) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	return &Handler{g: g, log: logger}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	switch {
	case r.Method == http.MethodGet && path == "/v1/health":
		h.handleHealth(w, r)
	case r.Method == http.MethodGet && path == "/internal/provider/models":
		h.handleModels(w, r)
	case r.Method == http.MethodGet && path == "/v1/diag/model-menu":
		h.handleDiagModelMenu(w, r)
	case r.Method == http.MethodGet && path == "/v1/diag/recipe":
		h.handleRecipeList(w, r)
	case r.Method == http.MethodPost && path == "/v1/diag/recipe/verify":
		h.handleRecipeVerify(w, r)
	case r.Method == http.MethodGet && path == "/v1/auth/status":
		h.handleAuthStatus(w, r)
	case r.Method == http.MethodPost && path == "/v1/auth/reset":
		h.handleAuthReset(w, r)
	case path == "/v1/sessions":
		h.handleSessions(w, r, "")
	case strings.HasPrefix(path, "/v1/sessions/"):
		h.handleSessions(w, r, strings.TrimPrefix(path, "/v1/sessions/"))
	case r.Method == http.MethodGet && path == "/v1/purge":
		h.handlePurgeList(w, r)
	case r.Method == http.MethodPost && path == "/v1/purge":
		h.handlePurgeCreate(w, r)
	case r.Method == http.MethodPost && path == "/v1/purge/drain":
		h.handlePurgeDrain(w, r)
	case strings.HasPrefix(path, "/v1/purge/"):
		h.handlePurgeItem(w, r, strings.TrimPrefix(path, "/v1/purge/"))
	case r.Method == http.MethodPost && path == "/internal/provider/chat":
		// Agent Hub's private browser-provider contract. Client-facing model APIs belong to Agent Hub.
		h.handleChat(w, r, "")
	case r.Method == http.MethodPost && path == "/internal/provider/responses":
		h.handleResponses(w, r, "")
	case retiredModelAPIPath(path):
		writeError(w, http.StatusGone, "客户端模型 API 已迁移到 Agent Hub，请改用 Agent Hub 的 /v1/models、/v1/text、/v1/chat/completions 或 /v1/responses", "gateway_moved", "")
	default:
		writeError(w, http.StatusNotFound, "not found", "invalid_request_error", "")
	}
}

// retiredModelAPIPath identifies former client-facing WebTouch model API routes.
// Site management and browser session endpoints under /v1 remain WebTouch-owned.
func retiredModelAPIPath(path string) bool {
	return path == "/v1/models" || path == "/v1/text" || path == "/v1/chat" || path == "/v1/chat/completions" ||
		path == "/v1/responses" || forcedChannelPath(path) != "" || forcedChannelResponsesPath(path) != ""
}

// forcedChannelPath 解析 /v1/{channel}/chat/completions（及 /v1/{channel}/chat）前缀。
func forcedChannelPath(path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) == 4 && segs[0] == "v1" && segs[2] == "chat" && segs[3] == "completions" {
		return segs[1]
	}
	if len(segs) == 3 && segs[0] == "v1" && segs[2] == "chat" {
		return segs[1]
	}
	return ""
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	type engineHealth struct {
		Addr    string `json:"addr"`
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Error   string `json:"error,omitempty"`
	}
	seen := map[string]bool{}
	var engines []engineHealth
	for i := range h.g.cfg.Channels {
		ch := &h.g.cfg.Channels[i]
		if ch.Type != ChannelTypeWeb || seen[ch.EngineAddr] {
			continue
		}
		seen[ch.EngineAddr] = true
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		eh := engineHealth{Addr: ch.EngineAddr}
		if eng, err := h.g.engineFor(ch); err != nil {
			eh.Error = err.Error()
		} else if hs, err := eng.HealthCheck(ctx); err != nil {
			eh.Error = err.Error()
		} else {
			eh.OK = hs.OK
			eh.Version = hs.Version
		}
		cancel()
		engines = append(engines, eh)
	}
	sort.Slice(engines, func(i, j int) bool { return engines[i].Addr < engines[j].Addr })
	channels := make([]string, 0, len(h.g.cfg.Channels))
	for i := range h.g.cfg.Channels {
		channels = append(channels, h.g.cfg.Channels[i].ID)
	}
	// 端到端就绪（2026-10-01）：「进程活着」不是可用；宿主 + 一次真实推理才算。
	browser := h.g.BrowserHostStatus()
	engineOK := true
	for _, e := range engines {
		if !e.OK {
			engineOK = false
			break
		}
	}
	hostOK := !browser.Managed || browser.Attached
	ready, lastInferOK, inferErr, inferMs := h.g.Readiness(ReadinessTTLFromEnv())
	if !engineOK || !hostOK {
		ready = false
	}
	payload := map[string]any{
		"status":            "ok",
		"target":            "llm_chat",
		"version":           BuildVersion,
		"channels":          channels,
		"engines":           engines,
		"browser":           browser,
		"host_ok":           hostOK,
		"engine_ok":         engineOK,
		"ready":             ready,
		"last_inference":    lastInferOK,
		"last_inference_ms": inferMs,
	}
	if inferErr != "" {
		payload["last_inference_error"] = inferErr
	}
	// ?deep=1 追加一次**真实推理**（进程 + 宿主 + 一次真实推理＝端到端可用，2026-10-01 口径）。
	if v := strings.TrimSpace(r.URL.Query().Get("deep")); v == "1" || v == "true" {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		text, err := h.g.DeepCheck(ctx, strings.TrimSpace(r.URL.Query().Get("model")), "")
		deep := map[string]any{"ok": err == nil}
		if err != nil {
			deep["error"] = err.Error()
		} else {
			deep["text"] = text
		}
		payload["deep_check"] = deep
		if err != nil {
			payload["ready"] = false
		} else {
			readyNow, lastOKNow, _, msNow := h.g.Readiness(ReadinessTTLFromEnv())
			payload["ready"] = readyNow && payload["host_ok"].(bool) && payload["engine_ok"].(bool)
			payload["last_inference"] = lastOKNow
			payload["last_inference_ms"] = msNow
			delete(payload, "last_inference_error")
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (h *Handler) handleModels(w http.ResponseWriter, _ *http.Request) {
	type channelMeta struct {
		Site   string `json:"site"`
		Models []any  `json:"models"`
	}
	data := make([]any, 0)
	channels := map[string]channelMeta{}
	for _, m := range h.g.cfg.ModelTable() {
		data = append(data, map[string]any{
			"id":          m.ID,
			"channel":     m.Channel,
			"label":       m.Label,
			"description": m.Description,
			"object":      "model",
			"capabilities": map[string]bool{
				"thinking": m.Thinking,
				"search":   m.Search,
			},
		})
	}
	for i := range h.g.cfg.Channels {
		ch := &h.g.cfg.Channels[i]
		models := make([]any, 0, len(ch.Models))
		for _, m := range ch.Models {
			models = append(models, map[string]any{
				"id": m.ID, "family": m.Family, "label": m.Label,
				"description": m.Description,
				"thinking":    m.Thinking != nil && *m.Thinking,
				"search":      m.Search != nil && *m.Search,
				"aliases":     m.Aliases,
			})
		}
		channels[ch.ID] = channelMeta{Site: ch.Site, Models: models}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":   "list",
		"data":     data,
		"channels": channels,
	})
}

// ---------- 会话清账端点（GET/POST /v1/purge、POST /v1/purge/drain、DELETE|POST /v1/purge/{cid}） ----------

func (h *Handler) handlePurgeList(w http.ResponseWriter, r *http.Request) {
	if h.g.purge == nil {
		writeError(w, http.StatusNotImplemented, "purge: 待删除列表未启用", "not_implemented_error", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":         h.g.purge.List(),
		"pending_count": len(h.g.purge.Pending()),
	})
}

type purgeRequestBody struct {
	ChannelID      string `json:"channel_id"`
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"`
	Reason         string `json:"reason"`
	Async          bool   `json:"async"`
}

func (h *Handler) handlePurgeCreate(w http.ResponseWriter, r *http.Request) {
	if h.g.purge == nil {
		writeError(w, http.StatusNotImplemented, "purge: 待删除列表未启用", "not_implemented_error", "")
		return
	}
	var body purgeRequestBody
	if err := readJSONInto(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}
	item := PurgeItem{
		ChannelID:      strings.TrimSpace(body.ChannelID),
		SessionID:      strings.TrimSpace(body.SessionID),
		ConversationID: strings.TrimSpace(body.ConversationID),
		Reason:         strings.TrimSpace(body.Reason),
	}
	if item.ConversationID == "" && item.SessionID != "" {
		cid, err := h.g.ConversationIDForSession(r.Context(), item.SessionID)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error(), "invalid_request_error", "")
			return
		}
		item.ConversationID = cid
	}
	if item.ConversationID == "" {
		writeError(w, http.StatusBadRequest, "缺少 conversation_id 或 session_id", "invalid_request_error", "")
		return
	}
	cid, err := NormalizeConversationID(item.ConversationID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}
	item.ConversationID = cid
	if body.Async {
		queued, qerr := h.g.purge.Enqueue(item)
		if qerr != nil {
			writeError(w, http.StatusBadRequest, qerr.Error(), "invalid_request_error", "")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "conversation_id": queued.ConversationID, "item": queued})
		return
	}
	done, res, derr := h.g.PurgeConversation(r.Context(), item)
	if derr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "conversation_id": done.ConversationID, "verified": res.Verified, "result": res})
		return
	}
	// 立即删不掉 ⇒ 落「待删除列表」异步重试（用户口径）；入列本身失败才是真错误。
	queued, qerr := h.g.purge.Enqueue(item)
	if qerr != nil {
		writeError(w, http.StatusBadRequest, derr.Error(), "invalid_request_error", "")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "queued", "conversation_id": queued.ConversationID, "error": derr.Error(), "result": res,
	})
}

func (h *Handler) handlePurgeDrain(w http.ResponseWriter, r *http.Request) {
	if h.g.purge == nil {
		writeError(w, http.StatusNotImplemented, "purge: 待删除列表未启用", "not_implemented_error", "")
		return
	}
	max := 0
	if v := strings.TrimSpace(r.URL.Query().Get("max")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			max = n
		}
	}
	writeJSON(w, http.StatusOK, h.g.DrainPurge(r.Context(), max))
}

func (h *Handler) handlePurgeItem(w http.ResponseWriter, r *http.Request, cid string) {
	if h.g.purge == nil {
		writeError(w, http.StatusNotImplemented, "purge: 待删除列表未启用", "not_implemented_error", "")
		return
	}
	normalized, err := NormalizeConversationID(cid)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		ok, ferr := h.g.purge.Forget(normalized)
		if ferr != nil {
			writeError(w, http.StatusInternalServerError, ferr.Error(), "internal_error", "")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "待删除列表无此会话", "invalid_request_error", "")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"conversation_id": normalized, "status": "forgotten"})
	case http.MethodPost:
		item := PurgeItem{ConversationID: normalized, Reason: "manual_retry"}
		done, res, derr := h.g.PurgeConversation(r.Context(), item)
		if derr != nil {
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "conversation_id": normalized, "error": derr.Error(), "result": res})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "conversation_id": done.ConversationID, "verified": res.Verified, "result": res})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
	}
}

func (h *Handler) handleSessions(w http.ResponseWriter, r *http.Request, id string) {
	// `/v1/sessions/{id}/conversations`（契约 §9 S3）：站点侧栏对话列表，只读。
	if sid, suffix, ok := strings.Cut(id, "/"); ok && suffix == "conversations" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
			return
		}
		h.handleSiteConversations(w, r, sid)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.g.ListSessions())
	case http.MethodPost:
		var body map[string]any
		if err := readJSONBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
			return
		}
		channelID, _ := body["channel_id"].(string)
		model, _ := body["model"].(string)
		sid, _ := body["session_id"].(string)
		if sid == "" {
			sid, _ = body["conversation_id"].(string)
		}
		// 站点会话身份（契约 §9 S2）：body `site_conversation_id` 或头 `x-uws-site-conversation-id`；
		// 不得复用 conversation_id（那是「本网关会话键」别名）。
		siteConvID, _ := body["site_conversation_id"].(string)
		if v := r.Header.Get("x-uws-site-conversation-id"); strings.TrimSpace(v) != "" {
			siteConvID = v
		}
		if sid == "" {
			sid = fmt.Sprintf("s-%d", time.Now().UnixNano())
		}
		// 支持按 model 解析通道（桌面端「选模型即打开对应 ds/gemini 页面」，2026-08-26）。
		if channelID == "" && model != "" {
			ch, _, err := h.g.cfg.ChannelByModel(model)
			if err != nil {
				writeError(w, http.StatusNotFound, err.Error()+"（请从 Agent Hub GET /v1/models 查看可用模型）", "invalid_request_error", "")
				return
			}
			channelID = ch.ID
		}
		if channelID == "" {
			for i := range h.g.cfg.Channels {
				if h.g.cfg.Channels[i].Type == ChannelTypeWeb {
					channelID = h.g.cfg.Channels[i].ID
					break
				}
			}
		}
		if channelID == "" {
			writeError(w, http.StatusBadRequest, "无可用 web 通道", "invalid_request_error", "")
			return
		}
		ch := h.g.cfg.Channel(channelID)
		if ch == nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("通道 %q 不存在", channelID), "invalid_request_error", "")
			return
		}
		if ch.Type != ChannelTypeWeb {
			writeError(w, http.StatusNotImplemented, fmt.Sprintf("通道 %q 类型 %q 尚未实现", ch.ID, ch.Type), "not_implemented_error", "")
			return
		}
		unlock := h.g.sessionLock(sid)
		h.g.closeSession(r.Context(), sid)
		sess, err := h.g.ensureSession(r.Context(), ch, sid, siteConvID)
		// 尽力应用模型模式（DeepSeek 三模式 / Gemini 模型切换）：失败不阻塞会话创建，
		// 页面已打开；下一次 chat 由 modelChangedRequiresNewSession/ensureModel 兜底兑现。
		modelApplied := false
		var modelErr string
		if err == nil && model != "" {
			if p := h.g.Provider(ch.ID); p != nil {
				if e := h.g.ensureModel(r.Context(), p, model, sid); e != nil {
					modelErr = e.Error()
				} else {
					modelApplied = true
				}
			}
		}
		unlock()
		if err != nil {
			writeChatError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": sid, "status": "created", "context_id": sess.ContextID,
			"target_id":  sess.TargetID,
			"channel_id": ch.ID, "model": model,
			"model_applied": modelApplied,
			"model_error":   modelErr,
		})
	case http.MethodDelete:
		if id == "" || id == "sessions" {
			writeError(w, http.StatusBadRequest, "缺少 session_id", "invalid_request_error", "")
			return
		}
		// 2026-10-03 派单 d27954eeb 收紧默认：删除必须**显式** `?purge=1`，不写参数一律只解绑。
		// 起因＝默认清账误删一条不可恢复的真实对话（目标取自页签当前地址，被站点 SPA 跳转带偏）。
		// 即便显式 purge=1，目标仍要过「会话绑定对话 == 页签当前对话」的 fail-closed 核对，
		// 核对不过只解绑并在响应的 purge.reason 里说明（契约 §9 / §14）。
		purge := false
		if v := strings.TrimSpace(r.URL.Query().Get("purge")); v != "" {
			if b, ok := parseBoolString(v); ok {
				purge = b
			}
		}
		keep := h.g.keepSessionsEnabled(r)
		purgeInfo, tabClosed, tabErr := h.g.closeSessionWithPurge(r.Context(), id, purge, keep)
		// status=deleted 指「本网关会话已出账」（幂等）；页签是否真的关掉单独报
		// （T-34）：tab_closed=false 时账本条目保留，启动回收会补关，调用方不必重试删除。
		resp := map[string]any{"session_id": id, "status": "deleted", "tab_closed": tabClosed}
		if tabErr != nil {
			resp["tab_error"] = tabErr.Error()
			resp["tab_retained"] = true
		}
		if purgeInfo != nil {
			resp["purge"] = purgeInfo
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
	}
}

// keepSessionsEnabled 判断「调试保留」是否生效（契约 §14）：请求级 `?keep=1|0` 与头
// `x-uws-keep-sessions: 1|0` 显式优先，未给则回落进程级 `UWS_KEEP_SESSIONS`。
// 语义＝「少删」方向：只有显式 1/true 才算开，默认仍是删（不得反转默认）。
func (g *Gateway) keepSessionsEnabled(r *http.Request) bool {
	if r != nil {
		if v := strings.TrimSpace(r.URL.Query().Get("keep")); v != "" {
			if b, ok := parseBoolString(v); ok {
				return b
			}
		}
		if v := strings.TrimSpace(r.Header.Get(KeepSessionsHeader)); v != "" {
			if b, ok := parseBoolString(v); ok {
				return b
			}
		}
	}
	return g.keepSessions
}

// handleSiteConversations 处理 /v1/sessions/{id}/conversations（契约 §9 S3）。

// 读不到站点侧栏时回 200 + 空数组 + error（不虚构条目）。
func (h *Handler) handleSiteConversations(w http.ResponseWriter, r *http.Request, sid string) {
	out, err := h.g.ListSiteConversations(r.Context(), sid)
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
