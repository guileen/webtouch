package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 会话生命周期与收口（契约 §9「会话生命周期与收口」，2026-09-30 用户定案）。
//
// 三件事在这里收口：
//  1. 空闲兜底回收：会话空闲超过 TTL 即「先删站点会话、再关 Tab」，杜绝"用完没人关"的垃圾页；
//  2. 一次性会话（close_after）：本轮回答交付后立即收口；
//  3. 同站点低频门：同站串行 + 最小间隔，把"悠着点用"从自觉变成机制。
//
// 归属原则：网关只关自己创建的 Tab；不碰用户自己的窗口/标签页（引擎 CloseContext 幂等）。

const (
	// sessionIdleTTLEnv 空闲回收阈值（秒；<=0 关闭该能力）。
	sessionIdleTTLEnv = "UWS_SESSION_IDLE_TTL"
	// sessionReapIntervalEnv 回收协程周期（秒；<=0 关闭）。
	sessionReapIntervalEnv = "UWS_SESSION_REAP_SEC"
	// siteMinIntervalEnv 同站点两轮之间的最小间隔（秒；<=0 关闭）。未设时按站点默认值。
	siteMinIntervalEnv = "UWS_SITE_MIN_INTERVAL_SEC"
)

const (
	// defaultSessionIdleTTL 是空闲回收默认阈值：10 分钟内没人用就收口。
	defaultSessionIdleTTL = 10 * time.Minute
	// defaultSessionReapInterval 是回收协程默认周期。
	defaultSessionReapInterval = time.Minute
	// defaultDeepSeekSiteMinInterval 是 DeepSeek 站点默认最小间隔（2026-10-03 用户定案：严格限制频率，防禁言）。
	defaultDeepSeekSiteMinInterval = 60 * time.Second
	// defaultSiteMinInterval 是其它站点默认最小间隔。
	defaultSiteMinInterval = 15 * time.Second
)

// EnvSeconds 解析「秒」为单位的时长环境变量；未设或非法返回 0/负值语义由调用方定。
func envSeconds(name string) (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		if d, derr := time.ParseDuration(raw); derr == nil {
			return d, true
		}
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// DefaultSessionIdleTTL 返回空闲回收阈值（UWS_SESSION_IDLE_TTL 覆盖；<=0 = 关闭回收）。
func DefaultSessionIdleTTL() time.Duration {
	if d, ok := envSeconds(sessionIdleTTLEnv); ok {
		if d <= 0 {
			return -1
		}
		return d
	}
	return defaultSessionIdleTTL
}

// DefaultSessionReapInterval 返回回收协程周期（UWS_SESSION_REAP_SEC 覆盖；<=0 = 关闭）。
func DefaultSessionReapInterval() time.Duration {
	if d, ok := envSeconds(sessionReapIntervalEnv); ok {
		if d <= 0 {
			return -1
		}
		return d
	}
	return defaultSessionReapInterval
}

// siteMinInterval 返回某站点的最小间隔（UWS_SITE_MIN_INTERVAL_SEC 统一覆盖；<=0 = 关闭）。
func siteMinInterval(site string) time.Duration {
	if d, ok := envSeconds(siteMinIntervalEnv); ok {
		if d <= 0 {
			return 0
		}
		return d
	}
	if strings.EqualFold(strings.TrimSpace(site), "deepseek") {
		return defaultDeepSeekSiteMinInterval
	}
	return defaultSiteMinInterval
}

// siteGate 是单站点的低频门与熔断器：同一站点串行，且两轮之间不小于最小间隔；禁言或频繁时自动熔断。
type siteGate struct {
	mu           sync.Mutex
	last         time.Time
	bannedUntil  time.Time
	bannedReason string
}

// siteTurn 取该站点的执行权：同站串行 + 最小间隔等待 + 禁言熔断拦截。返回的 release 必须 defer 调用。
func (g *Gateway) siteTurn(ctx context.Context, site string) (func(), error) {
	if g == nil || strings.TrimSpace(site) == "" {
		return func() {}, nil
	}
	interval := siteMinInterval(site)
	cleanSite := strings.ToLower(strings.TrimSpace(site))
	g.mu.Lock()
	if g.siteGates == nil {
		g.siteGates = make(map[string]*siteGate)
	}
	gate := g.siteGates[cleanSite]
	if gate == nil {
		gate = &siteGate{}
		g.siteGates[cleanSite] = gate
	}
	g.mu.Unlock()

	// 串行：拿到门锁即表示该站点没有在途轮次。
	gate.mu.Lock()
	release := gate.mu.Unlock

	// 检查站点安全熔断（防禁言/频繁封号）
	if !gate.bannedUntil.IsZero() && time.Now().Before(gate.bannedUntil) {
		release()
		remaining := time.Until(gate.bannedUntil).Round(time.Second)
		errCode := errCodeRateLimited
		if isAccountBanned(errors.New(gate.bannedReason)) {
			errCode = errCodeAccountBanned
		}
		return nil, &ChatError{
			Code:     errCode,
			Reason:   fmt.Sprintf("site_safety_circuit_breaker: 站点 %q 处于安全熔断保护中 (剩余冷却 %v; 触发原因: %s)。已自动阻断网页端请求以防账号永久封禁", cleanSite, remaining, gate.bannedReason),
			Semantic: "site_turn_breaker",
		}
	}

	if interval > 0 && !gate.last.IsZero() {
		if wait := interval - time.Since(gate.last); wait > 0 {
			if g.sleep != nil {
				g.sleep(wait)
			}
			select {
			case <-ctx.Done():
				release()
				return nil, ctx.Err()
			default:
			}
		}
	}
	// 注意：门锁仍在本闭包手里，这里只更新时间戳再放行（重复加锁会自锁死）。
	return func() {
		gate.last = time.Now()
		release()
	}, nil
}

// TripSiteBreaker 触发站点级安全熔断（禁言 / 429 频繁限制）
func (g *Gateway) TripSiteBreaker(site string, cooldown time.Duration, reason string) {
	if g == nil || strings.TrimSpace(site) == "" {
		return
	}
	cleanSite := strings.ToLower(strings.TrimSpace(site))
	g.mu.Lock()
	if g.siteGates == nil {
		g.siteGates = make(map[string]*siteGate)
	}
	gate := g.siteGates[cleanSite]
	if gate == nil {
		gate = &siteGate{}
		g.siteGates[cleanSite] = gate
	}
	g.mu.Unlock()

	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.bannedUntil = time.Now().Add(cooldown)
	gate.bannedReason = reason
	if g.logger != nil {
		g.logger.Printf("🚨 站点 %s 触发风控禁言安全熔断！已锁死该站点 %v，原因: %s", cleanSite, cooldown, reason)
	}
}

// touchSession 记录会话最近一次活动时间（回收判据）。
func (g *Gateway) touchSession(sid string) {
	if g == nil || sid == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if sess := g.sessions[sid]; sess != nil {
		sess.LastUsedAt = time.Now()
	}
}

// closeSessionWithPurge 收口一个会话：先删站点会话（站点支持且 purge=true），再关 Tab。
// 返回 purge 结果（nil 表示未做清账）。调用方不得持有该会话锁。
// closeSessionWithPurge 先按需清账再关会话；透出「页签是否确认已关」供
// DELETE /v1/sessions 响应区分（T-34）。
func (g *Gateway) closeSessionWithPurge(ctx context.Context, sid string, purge, keep bool) (map[string]any, bool, error) {
	var info map[string]any
	if purge {
		info = g.purgeSessionOnClose(ctx, sid, keep)
	}
	unlock := g.sessionLock(sid)
	// 先记下「内存里到底有没有这个会话」：closeSession 会把内存会话出账，之后无法再判断。
	sess := g.getSession(sid)
	hadContext := sess != nil && sess.ContextID != ""
	tabClosed, tabErr := g.closeSession(ctx, sid)
	unlock()
	// 显式删除且**内存里已经没有这个会话**时才补一次引擎落盘绑定清理（缺口③）：
	// 有内存会话时 closeSession 已经调过 eng.CloseContext；而首次建会话
	// （runChatRound 的 isNew 分支）也走 closeSession，那里必须保留绑定以支持
	// 「网关重启后续页接管原窗口」。
	if !hadContext {
		g.dropEngineBindings(ctx, sid)
	}
	return info, tabClosed, tabErr
}

// trySessionLock 尝试取会话锁：已被占用（有在途请求）返回 false，不阻塞。
func (g *Gateway) trySessionLock(id string) (func(), bool) {
	if id == "" {
		id = DefaultSessionID
	}
	g.mu.Lock()
	l := g.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		g.locks[id] = l
	}
	g.mu.Unlock()
	if !l.TryLock() {
		return nil, false
	}
	return l.Unlock, true
}

// StartSessionReaper 起空闲回收协程；阈值取自 Options.SessionIdleTTL / UWS_SESSION_IDLE_TTL（<=0 不启动）。
func (g *Gateway) StartSessionReaper(interval time.Duration) {
	if g == nil || interval <= 0 || g.sessionIdleTTL <= 0 {
		return
	}
	ttl := g.sessionIdleTTL
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			for _, sid := range g.reapIdleSessions(time.Now()) {
				g.logger.Printf("lifecycle: 空闲回收会话 %q（超过 %s 未活动）", sid, ttl)
			}
		}
	}()
}

// channelSite 返回通道所属站点（低频门按站点分组；未知通道返回空 = 不设门）。
func (g *Gateway) channelSite(channelID string) string {
	if g == nil || g.cfg == nil {
		return ""
	}
	if ch := g.cfg.Channel(channelID); ch != nil {
		return ch.Site
	}
	return ""
}

// finishOneShot 收口一次性会话（close_after）：只在轮次成功时收口，失败轮留给重试与空闲兜底。
func (g *Gateway) finishOneShot(ctx context.Context, sid string, roundErr error) {
	if g == nil || roundErr != nil {
		return
	}
	info, tabClosed, tabErr := g.closeSessionWithPurge(ctx, sid, true, g.keepSessions)
	if info != nil {
		g.logger.Printf("lifecycle: 一次性会话 %q 已收口 status=%v", sid, info["status"])
	}
	if !tabClosed && tabErr != nil {
		// 页签没关成：账本条目保留，启动回收会补关（T-34）。
		g.logger.Printf("lifecycle: 一次性会话 %q 的页签未确认关闭（账本保留待回收）: %v", sid, tabErr)
	}
}

// reapIdleSessions 收口所有「空闲超过 TTL 且无在途请求」的会话，返回被收口的会话 id（按序）。
// 判据取 max(CreatedAt, LastUsedAt)：新会话从创建时刻起算，老会话从最后一次活动起算。
// 有在途请求（会话锁被占）的会话跳过，下一轮再试——绝不打断进行中的轮次。
func (g *Gateway) reapIdleSessions(now time.Time) []string {
	if g == nil || g.sessionIdleTTL <= 0 {
		return nil
	}
	g.mu.Lock()
	cands := make([]string, 0, len(g.sessions))
	for sid, sess := range g.sessions {
		if now.Sub(sessionIdleSince(sess)) >= g.sessionIdleTTL {
			cands = append(cands, sid)
		}
	}
	g.mu.Unlock()

	reaped := make([]string, 0, len(cands))
	for _, sid := range cands {
		unlock, ok := g.trySessionLock(sid)
		if !ok {
			continue
		}
		if sess := g.getSession(sid); sess == nil || now.Sub(sessionIdleSince(sess)) < g.sessionIdleTTL {
			unlock() // 取锁期间已被显式关闭或被新一轮活动刷新
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*purgeTimeout)
		g.purgeSessionOnClose(ctx, sid, g.keepSessions)
		g.closeSession(ctx, sid)
		cancel()
		unlock()
		reaped = append(reaped, sid)
	}
	return reaped
}

// sessionIdleSince 返回会话的"最近活动时刻"（创建与最后使用取较晚者）。
func sessionIdleSince(sess *Session) time.Time {
	if sess == nil {
		return time.Time{}
	}
	if sess.LastUsedAt.After(sess.CreatedAt) {
		return sess.LastUsedAt
	}
	return sess.CreatedAt
}
