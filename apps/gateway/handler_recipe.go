package gateway

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"uws/apps/gateway/recipe"
)

// 站点 recipe 自证与激活（契约 §13「激活门禁与验证产物」）。
//
//	GET  /v1/diag/recipe          列出各站点 recipe 的状态（含内嵌 DRAFT 与已激活产物）
//	POST /v1/diag/recipe/verify   在**真实页面上下文**跑一次自证回放；通过才置 ACTIVE 并落盘
//
// 存在的理由：recipe 是「一次真实交互的编译产物」，模板存在 ≠ 站点可用；本入口是
// 「什么被证明过」的唯一可核验通道，也是 fail-closed 回退 DOM 之前的那道门。

// DefaultRecipeVerifyTimeout 是自证回放的默认等待上限（真实站点一轮回答 10–40s）。
const DefaultRecipeVerifyTimeout = 120 * time.Second

type recipeVerifyRequest struct {
	Site      string `json:"site"`
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
	TimeoutMs int64  `json:"timeout_ms"`
	Persist   *bool  `json:"persist"`
}

func (h *Handler) handleRecipeList(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]any, 0, 8)
	for _, rec := range recipe.DefaultRegistry().List() {
		items = append(items, map[string]any{
			"site":   rec.Site,
			"status": string(rec.Status),
			"origin": rec.Match.Origin,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["site"].(string) < items[j]["site"].(string) })
	drifts := recipe.DefaultVerifier().RecentDrifts()
	writeJSON(w, http.StatusOK, map[string]any{
		"recipes":        items,
		"active_dir":     recipe.ActiveRecipeDir(),
		"recent_drifts":  drifts,
		"direct_enabled": directEnabledSites(),
	})
}

func (h *Handler) handleRecipeVerify(w http.ResponseWriter, r *http.Request) {
	var req recipeVerifyRequest
	if err := readJSONInto(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}
	site := strings.ToLower(strings.TrimSpace(req.Site))
	if site == "" {
		writeError(w, http.StatusBadRequest, "缺少 site", "invalid_request_error", "")
		return
	}
	sid := strings.TrimSpace(req.SessionID)
	if sid == "" {
		writeError(w, http.StatusBadRequest, "缺少 session_id（自证必须绑定一个真实页面会话）", "invalid_request_error", "")
		return
	}
	rec, ok := recipe.DefaultRegistry().Get(site)
	if !ok {
		writeError(w, http.StatusNotFound, "未知站点 recipe: "+site, "invalid_request_error", "unknown_recipe")
		return
	}
	eval := h.g.EvaluatorForSession(sid)
	if eval == nil {
		writeError(w, http.StatusBadRequest, "会话不存在或没有页面上下文: "+sid, "invalid_request_error", "unknown_session")
		return
	}

	wait := DefaultRecipeVerifyTimeout
	if req.TimeoutMs > 0 {
		wait = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()

	res, err := recipe.DefaultVerifier().VerifyRecipe(ctx, eval, rec, strings.TrimSpace(req.Prompt))
	payload := map[string]any{
		"site":        site,
		"session_id":  sid,
		"status":      string(res.Status),
		"http_status": res.HTTPStatus,
		"bytes":       res.Bytes,
		"elapsed_ms":  res.ElapsedMs,
	}
	if err != nil {
		payload["error"] = err.Error()
		payload["recent_drifts"] = driftForSite(site)
		h.log.Printf("recipe 自证未通过：site=%s session=%s err=%v", site, sid, err)
		writeJSON(w, http.StatusOK, payload)
		return
	}
	payload["answer"] = res.Answer
	payload["recipe_status"] = string(rec.Status)
	if len(res.Slots) > 0 {
		payload["slots"] = res.Slots
	}

	persist := true
	if req.Persist != nil {
		persist = *req.Persist
	}
	if persist {
		path, perr := recipe.SaveActive(recipe.ActiveRecipeDir(), rec, recipe.ActiveEvidence{
			Prompt:     strings.TrimSpace(req.Prompt),
			HTTPStatus: res.HTTPStatus,
			Answer:     res.Answer,
			Bytes:      res.Bytes,
			ElapsedMs:  res.ElapsedMs,
			SessionID:  sid,
		})
		if perr != nil {
			payload["persist_error"] = perr.Error()
			h.log.Printf("recipe 激活产物落盘失败：site=%s err=%v", site, perr)
		} else {
			payload["artifact"] = path
		}
	}
	h.log.Printf("recipe 自证通过并激活：site=%s session=%s http=%d answer=%q elapsed=%dms",
		site, sid, res.HTTPStatus, truncateForLog(res.Answer, 80), res.ElapsedMs)
	writeJSON(w, http.StatusOK, payload)
}

// driftForSite 取该站点最近的漂移记录（失败现场，便于「失败即查」）。
func driftForSite(site string) []recipe.DriftReport {
	out := make([]recipe.DriftReport, 0, 4)
	for _, d := range recipe.DefaultVerifier().RecentDrifts() {
		if d.Site == site {
			out = append(out, d)
		}
	}
	if len(out) > 4 {
		out = out[len(out)-4:]
	}
	return out
}

// directEnabledSites 返回当前**真的会走直发**（ACTIVE）的站点。
func directEnabledSites() []string {
	out := make([]string, 0, 4)
	for _, rec := range recipe.DefaultRegistry().List() {
		if rec.Status == recipe.StatusActive && CanDirectTransport(rec.Site) {
			out = append(out, rec.Site)
		}
	}
	sort.Strings(out)
	return out
}

func truncateForLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
