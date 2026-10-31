package gateway

import (
	"net/http"
	"os"
	"strings"

	"uws/pkg/clisession"
)

// allowDeprecatedProviderHTTP 标记 /internal/provider/* 已不是对外契约。
// UWS_PROVIDER_HTTP=410|retired|gone 时直接 410，且调用方不得再进入引擎。
func (h *Handler) allowDeprecatedProviderHTTP(w http.ResponseWriter, _ *http.Request) bool {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("X-WebTouch-Contract", "deprecated")
	w.Header().Set("X-WebTouch-Successor", "cli")
	w.Header().Set("Link", `<urn:webtouch:cli:session>; rel="successor-version"`)
	if providerHTTPRetired() {
		writeError(w, http.StatusGone,
			"WebTouch /internal/provider HTTP 已退役。对外契约是本机 CLI（webtouch / uws session，以及带 --session 的后续命令）。Agent Hub 应 exec CLI，不要再调用 :8000。",
			"provider_http_retired", "")
		return false
	}
	return true
}

func providerHTTPRetired() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("UWS_PROVIDER_HTTP"))) {
	case "410", "retired", "gone":
		return true
	default:
		return false
	}
}

// admitCLISession 在迁移期把已知 CLI lease 接到 provider 请求上。
// 未知 id（含隐式 default）保持旧网关行为；已关闭的 lease 返回 409，不进引擎。
func (h *Handler) admitCLISession(w http.ResponseWriter, sessionID string) bool {
	if h == nil || h.g == nil || h.g.cliSessions == nil {
		return true
	}
	if sessionID == "" || sessionID == DefaultSessionID {
		return true
	}
	_, ok, err := h.g.cliSessions.Lookup(sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CLI session lease 读取失败", "internal_error", "")
		return false
	}
	if !ok {
		return true
	}
	if _, err := h.g.cliSessions.Use(sessionID, clisession.UseInput{}); err != nil {
		writeError(w, http.StatusConflict, "CLI session 已关闭或不可复用，请 webtouch session create 重新分配句柄", "session_lease_closed", "")
		return false
	}
	return true
}

// rememberCLISiteConversation 把非流式轮次学到的站点对话 id 写回 CLI lease。
func (g *Gateway) rememberCLISiteConversation(sessionID string, siteID *string) {
	if g == nil || g.cliSessions == nil || siteID == nil || sessionID == "" || sessionID == DefaultSessionID {
		return
	}
	_ = g.cliSessions.SetSiteConversation(sessionID, *siteID)
}
