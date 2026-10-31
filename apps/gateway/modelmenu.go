// 模型菜单漂移自检（2026-09-26 用户定案）。
//
// 契约见 docs/gateway-contract-2026-08-26.md §6「模型菜单漂移自检」。要点：
// 站点会改版、档位会变，所以「菜单/选择器发生变化」必须内生为可重复检查，
// 而不是一次性修选择器。两条触发：
//
//  1. 失败即查：ensureModel 切换失败时自动跑一次深自检，结论拼进 model_error；
//  2. 定时 / 按需：GET /v1/diag/model-menu。
//
// 自检不发送消息、不切换模型、不改会话状态；深自检用页面自身 element.click()
// 开合一次菜单（无需物理鼠标，实测开关各一次 ~1.2s），结束后确保菜单关闭。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// 自检脚本标记：与页内登录探针（authProbeScript）同套路，便于测试夹具识别。
const modelMenuProbeMarker = "__uws_model_menu_probe"

// modelMenuPickerJS 是选择器按钮与菜单容器的取值口径，与 pkg/browser/gemini_model.go
// 的 geminiPickerSelector / geminiMenuOptionsJS 同源（两处口径必须一致，见 Bus 契约
// 「模型菜单的读取有三条硬约束」）。
const modelMenuPickerJS = `
  const __uws_model_menu_probe = true;
  const norm = s => (s == null ? '' : String(s)).replace(/\s+/g, ' ').trim();
  const visible = el => { const r = el.getBoundingClientRect(); return r.width >= 2 && r.height >= 2; };
  const pickerSel = "[data-test-id='bard-mode-menu-button'], button[aria-label^='Open mode picker, currently ']";
  const menuSel = 'gem-menu[data-test-id="gem-mode-menu"], gem-menu[data-testid="gem-mode-menu"], [data-test-id="gem-mode-menu"], [data-testid="gem-mode-menu"]';
  const findPicker = () => { const ps = [...document.querySelectorAll(pickerSel)]; return ps.filter(visible)[0] || ps[0] || null; };
  const rect = el => { const r = el.getBoundingClientRect(); return [Math.round(r.x + r.width / 2), Math.round(r.y + r.height / 2), Math.round(r.width), Math.round(r.height)]; };
  const read = () => {
    const p = findPicker();
    const menus = [...document.querySelectorAll(menuSel)];
    const openMenu = menus.find(e => e.getAttribute('data-visible') === 'true') || null;
    return {
      picker_present: !!p,
      picker_label: p ? (p.getAttribute('aria-label') || norm(p.innerText)) : '',
      picker_rect: p ? rect(p) : null,
      menu_container_present: menus.length > 0,
      menu_open: !!openMenu,
      menu_items: openMenu ? [...openMenu.querySelectorAll('gem-menu-item')].filter(visible).map(el => norm(el.innerText)).filter(t => t) : [],
      url: location.href.slice(0, 140)
    };
  };
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  // waitFor 是有界轮询：新建会话的页面还在导航/水合时，按钮与菜单都要等一会儿才有。
  const waitFor = async (fn, ms) => { const dl = Date.now() + ms; while (Date.now() < dl) { if (fn()) return true; await sleep(150); } return !!fn(); };`

// modelMenuLightScript 纯只读：不点击、无副作用（菜单懒渲染，因此读不到档位清单，
// 只判「选择器是否还在、按钮短名是否可解析、容器是否存在」）。
const modelMenuLightScript = `(() => {` + modelMenuPickerJS + `
  return read();
})()`

// modelMenuDeepScript 深自检：等按钮就绪 → 用页面自身 click 开菜单 → 等档位渲染 →
// 采样 → 确保关闭（有界重试，避免中途异常把菜单留在打开态遮挡页面）。
//
// 返回值是**对象**而非 JSON 字符串：网关经 gRPC 取回的是引擎对 JS 返回值做的
// JSON 序列化，脚本里再 stringify 会双重编码（2026-09-26 旁路实例实测踩到）。
const modelMenuDeepScript = `(async () => {` + modelMenuPickerJS + `
  const hadPicker = read().picker_present;
  const appeared = hadPicker || await waitFor(() => read().picker_present, 5000);
  const base = read();
  if (!appeared) return Object.assign({}, base, {clicked: false, reason: 'picker_missing'});
  findPicker().click();
  await waitFor(() => read().menu_open, 3000);
  const opened = read();
  for (let i = 0; i < 3 && read().menu_open; i++) { findPicker().click(); await sleep(400); }
  return Object.assign({}, opened, {clicked: true, closed_ok: !read().menu_open, picker_waited: !hadPicker});
})()`

// ModelMenuProbe 是页面脚本返回的原始采样。
type ModelMenuProbe struct {
	PickerPresent        bool     `json:"picker_present"`
	PickerLabel          string   `json:"picker_label"`
	PickerRect           []int    `json:"picker_rect"`
	MenuContainerPresent bool     `json:"menu_container_present"`
	MenuOpened           bool     `json:"menu_open"`
	MenuItems            []string `json:"menu_items"`
	URL                  string   `json:"url"`
	Clicked              *bool    `json:"clicked"`
	ClosedOK             *bool    `json:"closed_ok"`
	Reason               string   `json:"reason"`
}

// ModelMenuReport 是自检结论（HTTP 响应体与失败摘要共用）。
type ModelMenuReport struct {
	Channel          string    `json:"channel"`
	Site             string    `json:"site"`
	SessionID        string    `json:"session_id"`
	Deep             bool      `json:"deep"`
	CheckedAt        time.Time `json:"checked_at"`
	PickerPresent    bool      `json:"picker_present"`
	PickerLabel      string    `json:"picker_label"`
	CurrentFamily    string    `json:"current_family,omitempty"`
	MenuOpened       bool      `json:"menu_opened"`
	MenuItems        []string  `json:"menu_items"`
	ExpectedFamilies []string  `json:"expected_families"`
	FoundFamilies    []string  `json:"found_families"`
	UnexpectedItems  []string  `json:"unexpected_items,omitempty"`
	Drift            bool      `json:"drift"`
	DriftReasons     []string  `json:"drift_reasons,omitempty"`
	MenuClosedOK     bool      `json:"menu_closed_ok"`
	URL              string    `json:"url,omitempty"`
	Error            string    `json:"error,omitempty"`
}

var (
	errProbeNoSession = errors.New("该通道暂无会话")
	errProbeChannel   = errors.New("模型菜单自检仅支持 gemini 站点")
)

// classifyModelMenuDrift 按期望档位关键字判定漂移（纯函数，便于单测）。
// probe 为页面原始采样；deep 决定是否把「菜单没打开」计入漂移。
func classifyModelMenuDrift(expected []string, probe ModelMenuProbe, deep bool) (bool, []string, []string, []string) {
	var reasons, found, unexpected []string
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	if !probe.PickerPresent {
		reasons = append(reasons, "picker_missing")
	}
	matched := map[string]bool{}
	for _, item := range probe.MenuItems {
		hit := false
		for _, want := range expected {
			if want == "" || !strings.Contains(norm(item), norm(want)) {
				continue
			}
			hit = true
			if !matched[want] {
				matched[want] = true
				found = append(found, want)
			}
		}
		if !hit {
			unexpected = append(unexpected, item)
		}
	}
	if deep && !probe.MenuOpened {
		reasons = append(reasons, "menu_not_opened")
	}
	if probe.MenuOpened {
		for _, want := range expected {
			if want != "" && !matched[want] {
				reasons = append(reasons, "family_missing:"+want)
			}
		}
		if len(probe.MenuItems) > 0 && len(found) == 0 {
			reasons = append(reasons, "menu_unknown_shape")
		}
	}
	return len(reasons) > 0, reasons, found, unexpected
}

// currentFamily 从选择器按钮短名（"Open mode picker, currently Flash" / "Flash"）
// 取当前档位，用于对照报告；解析失败返回空串，不参与漂移判定。
func currentFamily(label string) string {
	s := strings.TrimSpace(label)
	if i := strings.LastIndex(s, "currently "); i >= 0 {
		s = strings.TrimSpace(s[i+len("currently "):])
	}
	s = strings.TrimSuffix(s, ".")
	if strings.Contains(strings.ToLower(s), "open mode picker") {
		return ""
	}
	return s
}

// ModelMenuReport 执行一次模型菜单自检（deep=false 只读、deep=true 开菜单采样）。
func (g *Gateway) ModelMenuReport(ctx context.Context, channelID, sid string, deep bool) (ModelMenuReport, error) {
	ch := g.cfg.Channel(channelID)
	if ch == nil {
		return ModelMenuReport{}, fmt.Errorf("未知通道 %q", channelID)
	}
	if ch.Site != "gemini" {
		return ModelMenuReport{}, fmt.Errorf("%w（通道 %s 站点 %s）", errProbeChannel, ch.ID, ch.Site)
	}
	p := g.Provider(ch.ID)
	if p == nil {
		return ModelMenuReport{}, fmt.Errorf("通道 %q 无站点 Provider", ch.ID)
	}
	if sid == "" {
		sid = g.firstSessionOfChannel(ch.ID)
		if sid == "" {
			return ModelMenuReport{}, fmt.Errorf("%w（先 POST /v1/sessions 建会话，或显式传 session=）", errProbeNoSession)
		}
	}
	sess := g.getSession(sid)
	if sess == nil {
		return ModelMenuReport{}, fmt.Errorf("会话 %q 不存在", sid)
	}
	report := ModelMenuReport{
		Channel: ch.ID, Site: ch.Site, SessionID: sid, Deep: deep,
		CheckedAt: time.Now(), ExpectedFamilies: p.MenuFamilies,
	}
	eval := g.EvaluatorForSession(sid)
	if eval == nil {
		report.Error = "页内脚本执行器不可用（会话未绑定标签页）"
		return report, nil
	}
	script := modelMenuLightScript
	timeout := 5 * time.Second
	if deep {
		script = modelMenuDeepScript
		timeout = 20 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := eval.EvaluateScript(probeCtx, script)
	if err != nil {
		report.Error = "自检脚本执行失败: " + err.Error()
		return report, nil
	}
	var probe ModelMenuProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		report.Error = "自检结果格式错误: " + err.Error()
		return report, nil
	}
	report.PickerPresent = probe.PickerPresent
	report.PickerLabel = probe.PickerLabel
	report.CurrentFamily = currentFamily(probe.PickerLabel)
	report.MenuOpened = probe.MenuOpened
	report.MenuItems = probe.MenuItems
	if report.MenuItems == nil {
		report.MenuItems = []string{}
	}
	report.URL = probe.URL
	// 菜单闭合状态：深自检以脚本回报的 closed_ok 为准；脚本没回报时按「未确认」
	// 处理（不得假定已关闭）；轻自检不点击，沿用页面当前开合状态。
	switch {
	case probe.ClosedOK != nil:
		report.MenuClosedOK = *probe.ClosedOK
	case !deep:
		report.MenuClosedOK = !probe.MenuOpened
	}
	report.Drift, report.DriftReasons, report.FoundFamilies, report.UnexpectedItems =
		classifyModelMenuDrift(p.MenuFamilies, probe, deep)
	return report, nil
}

// firstSessionOfChannel 取该通道的任一会话（优先 current），供不指定 session 的自检用；
// 不创建会话、不建标签页。
func (g *Gateway) firstSessionOfChannel(channelID string) string {
	snap := g.ListSessions()
	if id := snap.Current; id != "" && snap.Channels[id] == channelID {
		return id
	}
	for _, id := range snap.Sessions {
		if snap.Channels[id] == channelID {
			return id
		}
	}
	return ""
}

// modelMenuDigest 是「失败即查」的一行摘要（切换失败时拼进 model_error）。
// 单行、有界（≤260 字），只呈现结论与当时的菜单内容，不塞整份 JSON。
func (g *Gateway) modelMenuDigest(ctx context.Context, sid string) string {
	sess := g.getSession(sid)
	if sess == nil {
		return "模型菜单自检: 会话不存在"
	}
	rep, err := g.ModelMenuReport(ctx, sess.ChannelID, sid, true)
	if err != nil {
		return "模型菜单自检: " + err.Error()
	}
	if rep.Error != "" {
		return "模型菜单自检: " + rep.Error
	}
	verdict := "正常"
	if rep.Drift {
		verdict = "疑似漂移(" + strings.Join(rep.DriftReasons, ",") + ")"
	}
	line := fmt.Sprintf("模型菜单自检: 判定=%s 选择器=%q 当前档位=%q 菜单项=[%s]",
		verdict, rep.PickerLabel, rep.CurrentFamily, strings.Join(rep.MenuItems, " / "))
	return truncateRunes(line, 260)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// handleDiagModelMenu 是 GET /v1/diag/model-menu（定时 / 按需自检入口）。
func (h *Handler) handleDiagModelMenu(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	deep := q.Get("deep") != "0" // 默认深自检（真实开菜单采样）；deep=0 = 纯只读
	rep, err := h.g.ModelMenuReport(r.Context(), q.Get("channel"), q.Get("session"), deep)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errProbeNoSession) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error(), "invalid_request_error", "")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
