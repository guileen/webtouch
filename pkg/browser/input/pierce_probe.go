// 页内寻底探针脚本、探针返回值与 AX 取值最小解码（复杂度治理·阶段 2·单元 2c，2026-09-29）
//
// 三块内容都不直接下发 CDP 命令，故与 `pierce.go` 分开（两侧都留在代码软限内）：
//  1. `hitPiercingFnJS` / `inputStemPiercingJS`：页内几何层叠判定脚本；
//  2. `PierceStem` / `StemAt`：寻底第一步的返回值与宿主调用入口；
//  3. `axEditableNode` 等：`Accessibility.getFullAXTree` 取值的**最小本地解码**。
package input

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// ── 页内几何层叠判定（§5.0.10 条款 1 第一款）────────────────────────

// hitPiercingFnJS 是寻底助手函数集（注入时必须放在 IIFE 内，避免顶层 const 重复声明）。
const hitPiercingFnJS = `const uwsEl = el => (el && el.nodeType === 1) ? el : null;
const uwsEditableSel = 'input:not([type=hidden]), textarea, select, [contenteditable=""], [contenteditable="true"], [role="textbox"]';
const uwsIsEditable = el => {
  if (!uwsEl(el)) return false;
  const tag = el.tagName.toLowerCase();
  if (tag === 'textarea') return !el.disabled && !el.readOnly;
  if (tag === 'select') return !el.disabled;
  if (tag === 'input') {
    const type = (el.getAttribute('type') || 'text').toLowerCase();
    const nonText = ['hidden','button','submit','reset','image','checkbox','radio','file','range','color'];
    return nonText.indexOf(type) < 0 && !el.disabled && !el.readOnly;
  }
  if (el.isContentEditable) return true;
  return el.getAttribute('role') === 'textbox';
};
const uwsMaskReason = el => {
  if (!uwsEl(el)) return '';
  let cs = null;
  try { cs = window.getComputedStyle(el); } catch (e) { cs = null; }
  if (!cs) return '';
  if (el.getAttribute && el.getAttribute('aria-hidden') === 'true') return 'aria-hidden';
  if (cs.pointerEvents === 'none') return 'pointer-events';
  if (cs.opacity === '0' || cs.opacity === '0.0') return 'opacity';
  return '';
};
const uwsGestureProxy = el => {
  const why = uwsMaskReason(el);
  if (!why) return '';
  if (uwsIsEditable(el)) return '';
  try { if (el.querySelector && el.querySelector(uwsEditableSel)) return ''; } catch (e) {}
  return why;
};
const uwsComposedHostChain = el => {
  const out = [];
  let n = uwsEl(el);
  while (n) {
    let r = null;
    try { r = n.getRootNode ? n.getRootNode() : null; } catch (e) { r = null; }
    if (r && r.host) { out.push(r.host); n = r.host; continue; }
    break;
  }
  return out;
};`

// inputStemPiercingJS 是坐标就近输入框的寻底探针（§5.0.10 条款 1 第一款）。
// 相对原实现（裸 elementFromPoint）的差别：命中栈先剔除透明手势代理层，并对
// 「命中封闭宿主」显式上报（页内不可区分「无 shadow root 的自定义元素」与封闭宿主，
// 故一律交宿主层穿透判定）。
const inputStemPiercingJS = `(() => {
  ` + hitPiercingFnJS + `
  const x = X, y = Y;
  const stack = (() => { try { return document.elementsFromPoint(x, y) || []; } catch (e) { return []; } })();
  const masks = [];
  let at = null;
  for (let i = 0; i < stack.length; i++) {
    const el = uwsEl(stack[i]);
    if (!el) continue;
    const why = uwsGestureProxy(el);
    if (why) { masks.push(why); continue; }
    at = el;
    break;
  }
  if (!at) return {ok: false, reason: 'all_masked', masks: masks};
  const isInput = el => uwsEl(el) &&
    (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT') &&
    (el.type !== 'hidden');
  let target = isInput(at) ? at : null;
  if (!target && at.tagName === 'LABEL') {
    const ctrl = at.control || (at.htmlFor ? document.getElementById(at.htmlFor) : null);
    if (ctrl && isInput(ctrl)) target = ctrl;
    if (!target) target = at.querySelector('input:not([type=hidden]), textarea, select');
  }
  if (!target) target = at.querySelector('input:not([type=hidden]), textarea, select');
  // §5.0.10 条款 1 第二款：命中元素本身是封闭宿主（自定义元素且 shadowRoot 恒为 null）时，
  // 页内**不得**再向兄弟子树搜索「附近输入框」——真机实测（2026-09-25 E2E 场景 A/E）该启发式
  // 会把值写进 DOM 里另一个无关 input（假成功 + 越权写入）。此处直接上报交宿主层穿透。
  if (!target) {
    const tag = (at.tagName || '').toLowerCase();
    if (at.shadowRoot === null && tag.indexOf('-') >= 0) {
      return {ok: false, reason: 'closed_shadow_host', host: tag, masks: masks};
    }
  }
  if (!target && at.parentElement) target = at.parentElement.querySelector('input:not([type=hidden]), textarea, select');
  if (!target) {
    let p = at.parentElement;
    while (p && p !== document.body) { if (isInput(p)) { target = p; break; } p = p.parentElement; }
  }
  if (!target) return {ok: false, reason: 'no_input', masks: masks};
  const parts = [];
  let n = target;
  while (n && n.nodeType === 1 && n !== document.body && n !== document.documentElement) {
    let sel = n.tagName.toLowerCase();
    if (n.id) { sel += '#' + CSS.escape(n.id); parts.unshift(sel); break; }
    if (n.classList && n.classList.length) sel += '.' + Array.from(n.classList).slice(0, 3).map(c => CSS.escape(c)).join('.');
    let i = 1, sib = n;
    while ((sib = sib.previousElementSibling)) { if (sib.tagName === n.tagName) i++; }
    sel += ':nth-of-type(' + i + ')';
    parts.unshift(sel);
    n = n.parentElement;
  }
  document.querySelectorAll('[data-uws-target]').forEach(function(e){ delete e.dataset.uwsTarget; });
  try { target.dataset.uwsTarget = '1'; } catch (e) {}
  return {ok: true, sel: parts.join(' > '), tag: target.tagName.toLowerCase(), masks: masks,
          hostDepth: uwsComposedHostChain(target).length};
})()`

// PierceStem 是寻底探针的返回值。
type PierceStem struct {
	OK        bool     `json:"ok"`
	Reason    string   `json:"reason"`
	Sel       string   `json:"sel"`
	Tag       string   `json:"tag"`
	Masks     []string `json:"masks"`
	Host      string   `json:"host"`
	HostDepth int      `json:"hostDepth"`
}

// StemAt 执行页内几何层叠判定，取出坐标就近输入框的候选（宿主 `inputAtCoord` 的第一步）。
// 与旧实现等价：求值失败时返回 Reason="eval_error"、OK=false，由调用方转宿主层穿透。
func StemAt(ctx context.Context, s Session, x, y int) PierceStem {
	expr := strings.ReplaceAll(strings.ReplaceAll(inputStemPiercingJS, "X", strconv.Itoa(x)), "Y", strconv.Itoa(y))
	var stem PierceStem
	if err := s.EvalValue(ctx, expr, &stem); err != nil {
		stem = PierceStem{Reason: "eval_error"}
	}
	return stem
}

// ── AX 取值的最小本地解码（§5.0.10 条款 1 第二款）──────────────────
//
// 宿主 `pkg/browser/axsnapshot.go` 已有全量 AX 节点类型（`axNodeRaw`/`axStr`/`axProp`），
// 但那是「AX 快照导出」域、且被宿主 3 个文件引用：把它搬进 input 域会污染域边界，为一个
// 穿透簇给宿主加能力方法又会撑大 `Session`。故此处只保留穿透判定真正用到的字段，取值
// 解码与 `axStr` 同语义（字符串/数字/布尔/原样四种形态）。
type axValueRef struct {
	Value json.RawMessage `json:"value"`
}

func (v axValueRef) str() string {
	if len(v.Value) == 0 || string(v.Value) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(v.Value, &s); err == nil {
		return s
	}
	var f float64
	if err := json.Unmarshal(v.Value, &f); err == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	var b bool
	if err := json.Unmarshal(v.Value, &b); err == nil {
		if b {
			return "true"
		}
		return "false"
	}
	return string(v.Value)
}

type axPropRef struct {
	Name  string     `json:"name"`
	Value axValueRef `json:"value"`
}

// axEditableNode 是穿透判定从 AX 节点上真正用到的字段子集。
type axEditableNode struct {
	Ignored          bool        `json:"ignored"`
	BackendDOMNodeID int64       `json:"backendDOMNodeId"`
	Role             axValueRef  `json:"role"`
	Name             axValueRef  `json:"name"`
	Properties       []axPropRef `json:"properties"`
}

// prop 读取 AX 节点属性值的字符串形态（缺失返回空串），等价宿主 `axProp`。
func (n axEditableNode) prop(name string) string {
	for _, p := range n.Properties {
		if p.Name == name {
			return p.Value.str()
		}
	}
	return ""
}
