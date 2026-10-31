package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"uws/pkg/contract"
)

// AuthStatus 是网页账号可安全执行请求的判定结果。未知一律不允许发送，避免
// 未登录页面把自动输入误送入登录、注册或风控页面。
type AuthStatus string

const (
	AuthLoggedIn   AuthStatus = "logged_in"
	AuthLoggedOut  AuthStatus = "logged_out"
	AuthChallenge  AuthStatus = "challenge"
	AuthUnknown    AuthStatus = "unknown"
	AuthNotChecked AuthStatus = "not_checked"
)

type authSnapshot struct {
	Status    AuthStatus `json:"status"`
	CheckedAt time.Time  `json:"checked_at"`
	Rechecks  int        `json:"rechecks,omitempty"`
	Detail    string     `json:"detail,omitempty"`
}

// authProbeScript 只读取 DOM，不输入、不点击、不发网络请求。各提供商网页会
// 变化，因此只使用可见 composer、可见登录/挑战文本与可见挑战 iframe；无法判定时
// 保持 unknown 并 fail-closed。
const authProbeScript = `(() => {
  const __uws_auth_probe = true;
  const visible = (e) => !!e && !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
  const text = (document.body && document.body.innerText || '').slice(0, 20000).toLowerCase();
  const challengeMarker = /captcha|hcaptcha|recaptcha|arkose|turnstile|verify you are human|prove you are human|security check|security verification|robot check|验证您是人类|安全验证|人机验证|请完成验证|滑动验证|访问受限|访问过于频繁|异常流量/;
  const frameChallenge = Array.from(document.querySelectorAll('iframe')).some(e =>
    visible(e) && challengeMarker.test(((e.title || '') + ' ' + (e.getAttribute('src') || '') + ' ' + (e.getAttribute('aria-label') || '')).toLowerCase()));
  const visibleChallenge = Array.from(document.querySelectorAll('[class],[id],[role="dialog"],[aria-label]')).some(e => {
    if (!visible(e)) return false;
    const marker = ((e.getAttribute('class') || '') + ' ' + (e.id || '') + ' ' + (e.getAttribute('aria-label') || '')).toLowerCase();
    return challengeMarker.test(marker);
  });
  const composer = Array.from(document.querySelectorAll('textarea,[contenteditable="true"],input[type="text"]'))
    .some(e => visible(e) && !e.disabled && e.getAttribute('aria-disabled') !== 'true');
  const challenge = challengeMarker.test(text) || frameChallenge || visibleChallenge;
  const login = /\b(log in|sign in|continue with google|continue with email)\b|登录后|请登录|立即登录|登录以继续/.test(text);
  const status = challenge ? 'challenge' : (composer && !login ? 'logged_in' : (login ? 'logged_out' : 'unknown'));
  return {status, composer, login, challenge};
})()`

func (g *Gateway) authSnapshot(channelID string) authSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.auth[channelID]
}

func (g *Gateway) authLatchError(channelID string) error {
	snap := g.authSnapshot(channelID)
	locked := snap.Status == AuthLoggedOut || snap.Status == AuthChallenge
	unknownBlocked := snap.Status == AuthUnknown && !snap.CheckedAt.IsZero() &&
		(time.Since(snap.CheckedAt) < g.authCheckTTL || snap.Rechecks >= 1)
	if !locked && !unknownBlocked {
		return nil
	}
	if snap.Status == AuthUnknown && snap.Rechecks >= 1 && time.Since(snap.CheckedAt) >= g.authCheckTTL {
		snap.Detail = "一次到期复检后仍无法确认登录状态；请人工确认页面状态并显式清除 Gateway 锁"
	}
	ch := g.cfg.Channel(channelID)
	if ch == nil {
		return &AuthCheckError{Reason: "网页通道登录状态已锁定", Code: "authentication_check_failed"}
	}
	return authError(ch.Site, snap)
}

func (g *Gateway) setAuthSnapshot(channelID string, snap authSnapshot) {
	g.mu.Lock()
	g.auth[channelID] = snap
	g.mu.Unlock()
	if err := g.persistAuthState(); err != nil {
		g.logAuthPersistError(err)
	}
}

// loadAuthState 恢复需要人工处理的状态与短期未知状态。已登录状态不跨进程信任，
// 重启后必须先重新做一次只读 DOM 检查。
func (g *Gateway) loadAuthState() {
	if g.authStatePath == "" {
		return
	}
	data, err := os.ReadFile(g.authStatePath)
	if err != nil {
		return
	}
	var state map[string]authSnapshot
	if err := json.Unmarshal(data, &state); err != nil {
		if g.logger != nil {
			g.logger.Printf("gateway: 登录状态文件无效，忽略: %v", err)
		}
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, snap := range state {
		if strings.TrimSpace(id) == "" || snap.CheckedAt.IsZero() {
			continue
		}
		switch snap.Status {
		case AuthLoggedOut, AuthChallenge:
			// 未登录与挑战状态跨重启保持锁定，直到用户完成处理并显式清除。
		case AuthUnknown:
			// 冷却期后最多允许一次只读复检；第二次仍未知则跨重启保持锁定。
		default:
			continue
		}
		g.auth[id] = snap
	}
}

// persistAuthState 只落盘状态元数据，不保存浏览器会话、Cookie 或凭据。
func (g *Gateway) persistAuthState() error {
	g.authPersistMu.Lock()
	defer g.authPersistMu.Unlock()
	return g.persistAuthStateLocked()
}

func (g *Gateway) persistAuthStateLocked() error {
	if g.authStatePath == "" {
		return nil
	}
	g.mu.Lock()
	state := make(map[string]authSnapshot, len(g.auth))
	for id, snap := range g.auth {
		if snap.Status != AuthLoggedIn && snap.Status != AuthNotChecked && snap.Status != "" {
			state[id] = snap
		}
	}
	g.mu.Unlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(g.authStatePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".gateway-auth-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, g.authStatePath); err != nil {
		return err
	}
	return nil
}

func (g *Gateway) logAuthPersistError(err error) {
	if g.logger != nil {
		g.logger.Printf("gateway: 无法保存登录状态元数据: %v", err)
	}
}

// clearAuthSnapshot 是用户完成手动登录/验证后显式调用的本地状态清除；不接触网页。
func (g *Gateway) clearAuthSnapshot(channelID string) error {
	g.authPersistMu.Lock()
	defer g.authPersistMu.Unlock()
	g.mu.Lock()
	previous, hadPrevious := g.auth[channelID]
	delete(g.auth, channelID)
	g.mu.Unlock()
	if err := g.persistAuthStateLocked(); err != nil {
		g.mu.Lock()
		if _, exists := g.auth[channelID]; !exists && hadPrevious {
			g.auth[channelID] = previous
		}
		g.mu.Unlock()
		return err
	}
	return nil
}

// ensureAuthenticated 在首次使用及 TTL 到期后检查当前页面。只有明确 logged_in
// 会放行；缓存中的 logged_out/challenge/unknown 同样阻断，直到下次复检。
func (g *Gateway) ensureAuthenticated(ctx context.Context, ch *ChannelConfig, sid, siteConvID string) error {
	cached := g.authSnapshot(ch.ID)
	if cached.Status == AuthLoggedOut || cached.Status == AuthChallenge {
		return authError(ch.Site, cached)
	}
	if !cached.CheckedAt.IsZero() && time.Since(cached.CheckedAt) < g.authCheckTTL {
		return authError(ch.Site, cached)
	}
	if cached.Status == AuthUnknown && !cached.CheckedAt.IsZero() &&
		cached.Rechecks >= 1 {
		cached.Detail = "一次到期复检后仍无法确认登录状态；请人工确认页面状态并显式清除 Gateway 锁"
		return authError(ch.Site, cached)
	}
	rechecks := cached.Rechecks
	if cached.Status == AuthUnknown && !cached.CheckedAt.IsZero() && time.Since(cached.CheckedAt) >= g.authCheckTTL {
		rechecks++
	}

	eval := g.EvaluatorForSession(sid)
	if eval == nil {
		snap := authSnapshot{Status: AuthUnknown, CheckedAt: time.Now(), Rechecks: rechecks, Detail: "登录检查器不可用"}
		g.setAuthSnapshot(ch.ID, snap)
		return authError(ch.Site, snap)
	}
	raw, err := g.probeAuth(ctx, ch, sid, siteConvID, eval)
	if err != nil {
		if isPreflightReadinessFailure(err) {
			// 就绪类失败（标签页刚被重建 / CDP 会话未就绪）：不锁站点，交给外层整轮恢复
			// 重建标签页后重试一次（T-33）。仍 fail-closed：恢复轮同样先探测登录态，
			// 探测不过就不会输入/发送。
			return &ChatError{
				Reason:   fmt.Sprintf("%s: 登录态探测未就绪: %v", ch.Site, err),
				Semantic: "authentication_probe",
				Code:     errCodePageNotReady,
			}
		}
		snap := authSnapshot{Status: AuthUnknown, CheckedAt: time.Now(), Rechecks: rechecks, Detail: authProbeFailureDetail(err)}
		g.setAuthSnapshot(ch.ID, snap)
		return authError(ch.Site, snap)
	}
	var result struct {
		Status    AuthStatus `json:"status"`
		Composer  bool       `json:"composer"`
		Login     bool       `json:"login"`
		Challenge bool       `json:"challenge"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Status == "" {
		result.Status = AuthUnknown
		if err != nil {
			snap := authSnapshot{Status: AuthUnknown, CheckedAt: time.Now(), Rechecks: rechecks, Detail: "登录检查结果格式错误"}
			g.setAuthSnapshot(ch.ID, snap)
			return authError(ch.Site, snap)
		}
	}
	switch result.Status {
	case AuthLoggedIn, AuthLoggedOut, AuthChallenge:
	default:
		result.Status = AuthUnknown
	}
	detail := ""
	if result.Status == AuthChallenge || result.Challenge {
		result.Status = AuthChallenge
		detail = "检测到人机验证或安全挑战；自动请求已锁定，需人工处理并显式清除状态"
	} else if result.Status == AuthLoggedOut || result.Login {
		result.Status = AuthLoggedOut
		detail = "检测到未登录状态；自动请求已锁定，需人工登录并显式清除状态"
	}
	snap := authSnapshot{Status: result.Status, CheckedAt: time.Now(), Rechecks: rechecks, Detail: detail}
	g.setAuthSnapshot(ch.ID, snap)
	return authError(ch.Site, snap)
}

// probeAuth 执行只读登录态探测；就绪类失败（标签页刚被重建、CDP 会话未就绪）先原地
// 重试，仍失败则重建标签页再探测一轮——避免一次瞬时失败被记账成站点级 15 分钟
// unknown（T-33）。两轮都失败时返回原始错误，由调用方决定封锁（fail-closed 不变）。
func (g *Gateway) probeAuth(ctx context.Context, ch *ChannelConfig, sid, siteConvID string,
	eval contract.DirectScriptEvaluator) ([]byte, error) {
	raw, err := g.evalAuthProbe(ctx, eval)
	if err == nil || !isPreflightReadinessFailure(err) {
		return raw, err
	}
	if g.logger != nil {
		g.logger.Printf("登录态探测未就绪（%s），重建标签页后重试: %v", ch.Site, err)
	}
	g.closeSession(ctx, sid)
	if _, e := g.ensureSession(ctx, ch, sid, siteConvID); e != nil {
		return raw, err
	}
	g.sleep(authSettleAfterRebuild)
	if next := g.EvaluatorForSession(sid); next != nil {
		eval = next
	}
	return g.evalAuthProbe(ctx, eval)
}

// authProbeFailureDetail preserves only the local transport failure class. Raw engine
// messages may contain page text, URLs, or other session details and are not persisted.
func authProbeFailureDetail(err error) string {
	if engineErr, ok := err.(*EngineError); ok {
		return fmt.Sprintf("登录状态检查失败（引擎 gRPC %s）", engineErr.Code.String())
	}
	return fmt.Sprintf("登录状态检查失败（%T）", err)
}

func authError(site string, snap authSnapshot) error {
	switch snap.Status {
	case AuthLoggedIn:
		return nil
	case AuthLoggedOut:
		return &AuthRequiredError{Reason: fmt.Sprintf("%s 未登录；已阻止输入和发送，请由用户在对应浏览器 Profile 中完成登录后重试", site), Code: "login_required"}
	case AuthChallenge:
		return &AuthRequiredError{Reason: fmt.Sprintf("%s 出现人机验证或安全挑战；已阻止输入和发送，请由用户手动处理后重试", site), Code: "human_verification_required"}
	default:
		detail := strings.TrimSpace(snap.Detail)
		if detail == "" {
			detail = "页面未呈现可确认的登录态"
		}
		return &AuthCheckError{Reason: fmt.Sprintf("%s 登录状态未知（%s）；为保护账号已阻止输入和发送", site, detail), Code: "authentication_check_failed"}
	}
}

// handleAuthStatus 返回缓存状态，不创建标签页、不导航，也不会触发网页检查。
func (h *Handler) handleAuthStatus(w http.ResponseWriter, _ *http.Request) {
	sites := map[string]map[string]any{}
	seen := map[string]bool{}
	for i := range h.g.cfg.Channels {
		ch := &h.g.cfg.Channels[i]
		if ch.Type != ChannelTypeWeb || seen[ch.Site] {
			continue
		}
		seen[ch.Site] = true
		snap := h.g.authSnapshot(ch.ID)
		status := snap.Status
		if status == "" {
			status = AuthNotChecked
		}
		manualReset := snap.Status == AuthLoggedOut || snap.Status == AuthChallenge ||
			(snap.Status == AuthUnknown && snap.Rechecks >= 1)
		sites[ch.Site] = map[string]any{
			"channel_id": ch.ID, "status": status, "checked_at": snap.CheckedAt,
			"detail": snap.Detail, "rechecks": snap.Rechecks,
			"manual_reset_required": manualReset,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sites":    sites,
		"hint":     "未登录/挑战状态跨重启锁定；未知状态冷却后只复检一次，仍未知则需人工确认并显式清除。登录态不会跨重启信任。",
		"endpoint": "/v1/auth/status",
	})
}

func (h *Handler) handleAuthReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil || strings.TrimSpace(req.ChannelID) == "" {
		writeError(w, http.StatusBadRequest, "需要 channel_id", "invalid_request_error", "")
		return
	}
	found := false
	for i := range h.g.cfg.Channels {
		ch := &h.g.cfg.Channels[i]
		if ch.ID == req.ChannelID && ch.Type == ChannelTypeWeb {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "未知网页通道", "invalid_request_error", "")
		return
	}
	if err := h.g.clearAuthSnapshot(req.ChannelID); err != nil {
		h.g.logAuthPersistError(err)
		writeError(w, http.StatusInternalServerError, "登录状态文件更新失败；安全锁仍会在重启后恢复", "server_error", "auth_state_write_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channel_id": req.ChannelID,
		"status":     AuthNotChecked,
		"hint":       "状态已清除；下次聊天请求会进行一次只读登录检查，不会自动登录、点击或发送。",
	})
}
