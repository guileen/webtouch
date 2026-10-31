// Package input 是「输入可靠性」子包（复杂度治理阶段 2·单元 2a）：
// 收口派发前的输入门禁（落地判据/幂等写入/受体角色守卫）、Leased Token 结构身份
// 同余与局域微重锚判定核、以及 ExecuteAction drag 的坐标解析。
//
// 边界：本包只做输入域语义，不持有 CDP 会话。宿主 `*cdpSession` 经 Eval 窄接口注入
// （实测本组只用 evalValue 一项能力，不多不少）；页面探针 JS 与判定核同处一包，
// 防两侧判据漂移。
package input

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"strings"
	"time"
)

// Leased Token 结构身份同余与局域微重锚（spec/algorithm-kernel.md §5.0.10 条款 2/3；
// WebTouch v2.0 RFC 裁决 1 / 手术 4）。
//
// 背景（机制事实）：水合（hydration）会在派发窗口内重建节点，标记载体
// （`data-uws-target=1`）随之消失，旧行为只能报 `input_not_editable`（白掉站点覆盖）；
// 而「Name 恒等」不能当判据——水合常同时改 `placeholder` / `aria-label`。
//
// 本文件把「派发前断言」升级为**租约**：结构身份指纹（`id` / `name` / `type` / DOM 路径）
// + 几何矩形 + 捕获时刻；命中失配时，仅当 Role 恒等 ∧ 指纹相等 ∧ 位移 ≤ 32px ∧ 租约
// 未过期（≤1500ms）才允许**一次**局域微重锚（Budget = 1）；重锚后强制落地回读；否则
// 硬熔断 `TARGET_TOPOLOGY_RUPTURE`（不猜、不回退 VLM 自由发挥）。
// Name 文案（`placeholder` / `aria-label` / 文本）只作弱加权，**不参与**恒等判据。

const (
	// reanchorLeaseTTL 是租约时效（§5.0.10 条款 2：≤1500ms 标称，待真机采样标定）。
	reanchorLeaseTTL = 1500 * time.Millisecond
	// reanchorMaxDriftPX 是局域微重锚允许的最大几何位移（§5.0.10 条款 2：≤32px 标称）。
	reanchorMaxDriftPX = 32.0
	// FaultTargetTopologyRupture 是同余断言失败的结构化熔断代号（§5.0.8 条款 1 / §5.0.10 条款 3）。
	FaultTargetTopologyRupture = "TARGET_TOPOLOGY_RUPTURE"
)

// StructuralIdentity 是派发前断言的结构身份；`WeakName` 单列，故意不进指纹。
type StructuralIdentity struct {
	Role     string  `json:"role"`
	Tag      string  `json:"tag"`
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Path     string  `json:"path"`
	WeakName string  `json:"weakName"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
}

// fingerprint 是结构身份指纹（§5.0.10 条款 2 的恒等判据：`id`/`name`/`type`/DOM 路径）。
// Role 单列（RoleCongruent 单独判定）；Name 文案（WeakName）**不入指纹**。
func (id StructuralIdentity) Fingerprint() string {
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(id.Tag)),
		strings.TrimSpace(id.ID),
		strings.TrimSpace(id.Name),
		strings.ToLower(strings.TrimSpace(id.Type)),
		strings.TrimSpace(id.Path),
	}, "\x1f")
}

// Lease 是派发前持有的租约（结构身份 + 捕获时刻 + 微重锚预算）。
type Lease struct {
	identity StructuralIdentity
	at       time.Time
	budget   int
}

// valid 判定租约是否仍在时效内。
func (l *Lease) Valid(now time.Time) bool {
	return l != nil && !l.at.IsZero() && now.Sub(l.at) <= reanchorLeaseTTL
}

// RoleCongruent 判定 Role 恒等：两侧都可得时必须相等；一侧缺失（页面 AX 无角色证据）
// 时退化为结构指纹判定——指纹相等仍是必要条件，不引入新敞口。
func RoleCongruent(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return true
	}
	return a == b
}

// ReanchorVerdict 是 §5.0.10 条款 2/3 的纯判定：返回 (是否放行局域微重锚, 原因)。
// 放行条件：租约有效 ∧ 预算未耗尽 ∧ Role 恒等 ∧ 结构身份指纹相等 ∧ 位移 ≤ 32px。
func ReanchorVerdict(lease *Lease, cand StructuralIdentity, now time.Time) (bool, string) {
	if lease == nil || strings.TrimSpace(lease.identity.Tag) == "" {
		return false, "no_lease"
	}
	if !lease.Valid(now) {
		return false, "lease_expired"
	}
	if lease.budget <= 0 {
		return false, "budget_exhausted"
	}
	if !RoleCongruent(lease.identity.Role, cand.Role) {
		return false, "role_changed"
	}
	if lease.identity.Fingerprint() != cand.Fingerprint() {
		return false, "identity_changed"
	}
	if math.Hypot(cand.X-lease.identity.X, cand.Y-lease.identity.Y) > reanchorMaxDriftPX {
		return false, "displacement_exceeded"
	}
	return true, "ok"
}

// StructuralIdentityFnJS 是页内结构身份抽取助手（★ 组合祖先只用于路径生成，
// 封闭根内的元素页内不可见，见 §5.0.10 条款 1 第二款）。
const StructuralIdentityFnJS = `const uwsAttr = (el, n) => (el && el.getAttribute ? (el.getAttribute(n) || '') : '');
const uwsIdentityPath = el => {
  const parts = [];
  let n = el;
  while (n && n.nodeType === 1 && n !== document.body && n !== document.documentElement) {
    let sel = n.tagName.toLowerCase();
    if (n.id) { sel += '#' + CSS.escape(n.id); parts.unshift(sel); break; }
    let i = 1, sib = n;
    while ((sib = sib.previousElementSibling)) { if (sib.tagName === n.tagName) i++; }
    sel += ':nth-of-type(' + i + ')';
    parts.unshift(sel);
    n = n.parentElement;
  }
  return parts.join(' > ');
};
const uwsDeriveRole = el => {
  const attr = (uwsAttr(el, 'role') || '').toLowerCase();
  if (attr) return attr;
  const tag = (el.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea') return 'textbox';
  if (tag === 'select') return 'combobox';
  return '';
};
const uwsIdentityOf = el => {
  let r = {x: 0, y: 0, w: 0, h: 0};
  try {
    const b = el.getBoundingClientRect();
    r = {x: b.left + b.width / 2, y: b.top + b.height / 2, w: b.width, h: b.height};
  } catch (e) {}
  return {
    role: uwsDeriveRole(el),
    tag: (el.tagName || '').toLowerCase(),
    id: el.id || '',
    name: uwsAttr(el, 'name'),
    type: (uwsAttr(el, 'type') || '').toLowerCase(),
    path: uwsIdentityPath(el),
    weakName: (uwsAttr(el, 'placeholder') || uwsAttr(el, 'aria-label') || ''),
    x: r.x, y: r.y, w: r.w, h: r.h,
  };
};`

// IdentityProbeJS 读取已标记受体的结构身份（派发前断言/租约捕获）。
const IdentityProbeJS = `(() => {
  ` + StructuralIdentityFnJS + `
  const el = document.querySelector('[data-uws-target="1"]');
  if (!el) return {found: false};
  return {found: true, identity: uwsIdentityOf(el)};
})()`

// ReanchorCandidateJS 按结构身份指纹找回被水合重建的受体并重新标记（唯一一次）。
// 候选必须**逐字段**满足指纹（tag/id/name/type/path），几何位移由 Go 侧纯判定再裁；
// 多处命中时取位移最小者（局域微调，不做全局搜索）。
const ReanchorCandidateJS = `(() => {
  ` + StructuralIdentityFnJS + `
  const want = __UWS_WANT__;
  const sel = 'input, textarea, select, [contenteditable=""], [contenteditable="true"], [role="textbox"]';
  const cands = Array.from(document.querySelectorAll(sel));
  let best = null, bestEl = null, bestDrift = Infinity;
  for (let i = 0; i < cands.length; i++) {
    const id = uwsIdentityOf(cands[i]);
    if (want.tag && id.tag !== want.tag) continue;
    if (want.id && id.id !== want.id) continue;
    if (want.name && id.name !== want.name) continue;
    if (want.type && id.type !== want.type) continue;
    if (want.path && id.path !== want.path) continue;
    const dx = id.x - want.x, dy = id.y - want.y;
    const drift = Math.sqrt(dx * dx + dy * dy);
    if (drift < bestDrift) { best = id; bestEl = cands[i]; bestDrift = drift; }
  }
  if (!bestEl) return {found: false};
  document.querySelectorAll('[data-uws-target]').forEach(function(e){ delete e.dataset.uwsTarget; });
  try { bestEl.dataset.uwsTarget = '1'; } catch (e) {}
  return {found: true, identity: best, drift: bestDrift};
})()`

// Eval 是宿主向本包暴露的最小能力面（实测：本组仅需页面内求值一项）。
type Eval interface {
	EvalValue(ctx context.Context, expr string, dst any) error
}

// CaptureLease 在派发前对已标记受体做一次结构身份快照（§5.0.10 条款 2）。
// 无标记载体（已被水合抹掉）返回 nil ⇒ 后续无法同余判定，只能熔断（不猜）。
func CaptureLease(ctx context.Context, ev Eval) *Lease {
	var res struct {
		Found    bool               `json:"found"`
		Identity StructuralIdentity `json:"identity"`
	}
	if err := ev.EvalValue(ctx, IdentityProbeJS, &res); err != nil || !res.Found {
		return nil
	}
	return &Lease{identity: res.Identity, at: time.Now(), budget: 1}
}

// Reanchor 在标记载体失效时执行一次结构身份同余局域微重锚（§5.0.10 条款 2/3）。
// 返回 (是否已重锚, 熔断原因, 错误)。任何不成立一律熔断，不换出口、不重试。
func Reanchor(ctx context.Context, ev Eval, lease *Lease) (bool, string, error) {
	if lease == nil {
		return false, "no_lease", nil
	}
	want, err := json.Marshal(lease.identity)
	if err != nil {
		return false, "encode_failed", err
	}
	var res struct {
		Found    bool               `json:"found"`
		Identity StructuralIdentity `json:"identity"`
		Drift    float64            `json:"drift"`
	}
	expr := strings.Replace(ReanchorCandidateJS, "__UWS_WANT__", string(want), 1)
	if err := ev.EvalValue(ctx, expr, &res); err != nil {
		return false, "probe_failed", err
	}
	if !res.Found {
		return false, "no_candidate", nil
	}
	ok, reason := ReanchorVerdict(lease, res.Identity, time.Now())
	if !ok {
		return false, reason, nil
	}
	lease.budget--
	log.Printf("uws input reanchor tag=%s path=%q drift=%.1fpx role=%s (Budget 已用尽)",
		res.Identity.Tag, res.Identity.Path, res.Drift, res.Identity.Role)
	return true, "ok", nil
}
