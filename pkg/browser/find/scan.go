// Package find 是「语义/选择器定位」域能力层（复杂度治理·阶段 3 第一刀）。
//
// 分层：编排层（`package browser` 的 cdpSession）→ 域能力层（本包）→ 原语
// （`input` 的可编辑判定与命中目标类型、`semantics` 的语义标签展开）。
// 本包只向下依赖；页面交互只经单项能力 `Evaluator`（Runtime.evaluate），
// 不持有会话状态——命中证据 `lastHit` 由宿主落账。
package find

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"uws/pkg/browser/input"
	"uws/pkg/browser/semantics"
)

// Evaluator 是定位域向宿主索取的唯一能力：页内求值并读回原始 JSON。
type Evaluator interface {
	EvalRaw(ctx context.Context, expr string) (json.RawMessage, error)
}

// errEvalEmpty 与宿主同文案（`window/anchor.go` 同例）：Runtime.evaluate 成功但 value 为空/缺失。
var errEvalEmpty = errors.New("cdp: evaluate returned empty value")

// quoteJS 生成 JS 字符串字面量（含引号/反斜杠转义）：与宿主 `quoteJS`（→ modelbridge.QuoteJS）
// 同实现 json.Marshal ⇒ 生成表达式逐字节不变，且本包不必反向依赖 modelbridge。
func quoteJS(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Scan 按语义/选择器定位元素，返回命中几何与身份（宿主 `findElementMode` 的逐行搬移）。
//
// 严格模式（strictTarget=true，2026-09-23 语义抽取门禁）：只认「元素自身文本 == 目标」（100）
// 与「元素文本包含目标」（80），不再接受「元素文本是目标的一个碎片」（原 60 分档），也不再用
// 行上下文给碎片加分；并把标题/段落/单元格等内容节点纳入候选池——extract 抽的是内容本身，
// 不是交互元素。根因：example.com/IANA 页上 `extract:Example Domains` 曾抽到导航栏链接
// 「Domains」并报 SUCCEEDED；h1 根本不在 click 候选池里，碎片匹配却成了赢家
// （见 docs/evidence/pilots/dogfood-live-2026-09-23.md §二/§三）。click/input 语义保持原样。
//
// editableOnly（2026-09-25，spec/algorithm-kernel.md §5.0.3 条款 1/2）：input/fill/select 的
// 文本汇支路只接受可编辑目标——赢家不可编辑时在「可编辑候选（语义分 > 0）」内重选；重选不到则
// 保留原赢家并标 editable=false，由派发层给出 input_not_editable 明确失败。
//
// selector 非空时先按规则选择器精确过滤（querySelectorAll），再在命中集合内做语义评分择优；
// 选择器命中但无语义分（如纯 SVG 图标按钮）时回退到第一个可见命中，解决「规则选择器 → 物理层」
// 接线缺口（DeepSeek chat 场景实测：输入框/发送按钮/回答区均需选择器）。preferLast=true（extract
// 动作）：无语义分回退与语义平分均取「最后一个」命中（回答块流式追加语义，取第一个在多轮下必是
// 旧回答）；selector 以 text= 前缀开头时改为按「精确文本」定位最小可见元素（DeepSeek「开启新对话」
// 是纯 div，无 role/button/onclick，常规扫描抓不到）。
func Scan(ctx context.Context, ev Evaluator, semantic, selector string, preferLast, strictTarget, editableOnly bool) (input.ElementTarget, error) {
	const js = "(() => {" + scanJSPredicates + scanJSTail + "\n\t})()"
	// §5.0.2：派发前把内部语义标签归一化为页面候选文案集合（多候选取最高分）。
	wants := semantics.Wants(semantic)
	wantJSON, merr := json.Marshal(wants)
	if merr != nil {
		wantJSON = []byte("[]")
	}
	expr := strings.ReplaceAll(js, "__WANTS__", string(wantJSON))
	expr = strings.ReplaceAll(expr, "__SELECTOR__", quoteJS(selector))
	expr = strings.ReplaceAll(expr, "__PREFER_LAST__", strconv.FormatBool(preferLast))
	expr = strings.ReplaceAll(expr, "__STRICT__", strconv.FormatBool(strictTarget))
	expr = strings.ReplaceAll(expr, "__EDITABLE_ONLY__", strconv.FormatBool(editableOnly))
	expr = strings.ReplaceAll(expr, "__IS_EDITABLE__", input.IsEditableFnJS)
	var t input.ElementTarget
	raw, err := ev.EvalRaw(ctx, expr)
	if err != nil {
		return input.ElementTarget{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return input.ElementTarget{}, errEvalEmpty
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return input.ElementTarget{}, fmt.Errorf("cdp: decode element target: %w", err)
	}
	// §5.0.2 条款 6（可观测）：只在发生了别名展开时留痕，避免日志噪声。
	// 命中证据（lastHit）由宿主落账：本域不持有会话状态。
	if t.Found && len(wants) > 1 {
		log.Printf("uws locate semantic=%q matched_want=%q tag=%s text=%q selector=%q",
			semantic, t.Matched, t.Tag, t.Text, selector)
	}
	return t, nil
}

// scanJSTail 是扫描脚本的后半段：候选池构建（选择器 / text= 精确文本 / 附加扫描）、
// 打分择优（含 strictTarget 与 editableOnly 重选）与返回。前半段见 scan_predicates.go。
const scanJSTail = `
	  const isTextSel = sel.startsWith('text=');
	  // 2026-08-31 Airwallex 实战：企业后台表格行 onclick 是 JS 属性绑定
	  // （tr.onclick 有值、无 onclick HTML 属性），[onclick] 属性选择器选不到 →
	  // 附加扫描所有元素里带 property onclick 的节点（CSS 无法表达属性绑定）。
	  const baseSel = 'button, a[href], input:not([type=hidden]), textarea, select, label, [role="button"], [role="link"], [role="menuitem"], [onclick], [data-testid]';
	  // 语义抽取（strictTarget）追加的内容节点：标题/段落/单元格/列表项等承载文本的角色，
	  // 与 AX 内容树保留的角色同源（axsnapshot.go）。它们不在 click 候选池里。
	  const contentSel = 'h1, h2, h3, h4, h5, h6, p, td, th, li, dt, dd, blockquote, figcaption, pre, caption';
	  let els;
	  if (isTextSel) {
	    const target = sel.slice(5);
	    els = Array.from(document.querySelectorAll('body *'))
	      .filter(el => norm(el.textContent) === target);
	  } else {
	    els = sel
	      ? Array.from(document.querySelectorAll(sel))
	      : Array.from(document.querySelectorAll(baseSel));
	    if (!sel) {
	      for (const el of document.querySelectorAll('*')) {
	        if ((el.onclick != null || isPointerTextLeaf(el)) && !el.matches(baseSel)) els.push(el);
	      }
	      if (strictTarget) {
	        for (const el of document.querySelectorAll(contentSel)) {
	          if (!el.matches(baseSel) && !els.includes(el)) els.push(el);
	        }
	      }
	    }
	  }
	  let best = null, bestEl = null, bestScore = -1, bestArea = Infinity;
	  if (isTextSel) {
	    // text= 精确文本：取最小可见元素（DeepSeek「开启新对话」是带嵌套的纯 div，
	    // 文本匹配会命中一串祖先，最小可见 = 叶子节点，点击坐标最准）。
	    let bestArea = Infinity;
	    for (const el of els) {
	      if (!visible(el)) continue;
	      const r = el.getBoundingClientRect();
	      const area = r.width * r.height;
	      if (area >= bestArea) continue;
	      bestArea = area;
	      bestEl = el;
	      best = {x: r.left + r.width/2, y: r.top + r.height/2, w: r.width, h: r.height,
	              tag: el.tagName.toLowerCase(), text: norm(el.textContent).slice(0, 120)};
	    }
	  } else {
	    for (const el of els) {
	      // §5.0.5 条款 1：物理可见性前置硬剪枝，未通过者零打分直接跳过
	      if (!visible(el)) continue;
	      const normVal = el.value ? norm(el.value) : '';
	      const cleanVal = (el.tagName && el.tagName.toLowerCase() === 'textarea' && /^<style/i.test(normVal)) ? '' : normVal;
	      const t = norm(el.innerText || cleanVal || el.getAttribute('aria-label') ||
	                 el.getAttribute('data-testid') || el.getAttribute('title') || el.placeholder || '');
	      if (!t) continue;
	      // 语义源全量评分（2026-08-20 续24 修复）：
	      //   - 只取「第一个非空文本源」会让 data-testid/aria-label 语义在有
	      //     文本的元素上永远匹配不到（如 <span data-testid="author_name">
	      //     作者甲</span>），extract 语义增强的基础；
	      //   - 零分（无任何语义关联）元素绝不入选，杜绝「未匹配语义误返回
	      //     第一个元素」的假阳性（实测真 bug）。
	      const hit = scoreEl(el);
	      if (hit.score <= 0) continue;
	      // §5.0.4 角色线性效用加成：文本汇支路（editableOnly=true）对真实可编辑元素赋 +20 分，
	      // 确保在同等文本匹配（如 80 分）下，input/textarea 绝对战胜装饰性 div/span 图标。
	      const isEd = isEditableEl(el);
	      const roleBonus = (editableOnly && isEd) ? 20 : 0;
	      const totalScore = Math.min(100, hit.score + roleBonus);
	      if (totalScore < bestScore) continue;
	      const area = areaEl(el);
	      if (totalScore === bestScore && bestEl) {
	        const bestEd = isEditableEl(bestEl);
	        // 跨分支且得分相同时，优先选择可编辑元素（消灭非可编辑元素抢占赢家）
	        if (editableOnly && isEd && !bestEd) {
	          // el 具有编辑角色而 bestEl 没有：el 绝对胜出
	        } else if (editableOnly && !isEd && bestEd) {
	          // bestEl 具有编辑角色而 el 没有：保持 bestEl
	          continue;
	        } else {
	          // 包含域亲缘检测：仅在严格包含关系下做 min(area) 叶子下钻；跨分支节点按第二物理特征量决胜
	          const isRelated = el.contains(bestEl) || bestEl.contains(el);
	          if (isRelated) {
	            if (area > bestArea) continue;
	            if (area === bestArea && !preferLast) continue;
	          } else {
	            // §5.0.5 条款 2：第二物理特征量决胜（视口中心欧氏距离 × 渲染面积）
	            const cx = (window.innerWidth || 0) / 2, cy = (window.innerHeight || 0) / 2;
	            const rEl = el.getBoundingClientRect();
	            const rBest = bestEl.getBoundingClientRect();
	            const distEl = Math.hypot(rEl.left + rEl.width/2 - cx, rEl.top + rEl.height/2 - cy);
	            const distBest = Math.hypot(rBest.left + rBest.width/2 - cx, rBest.top + rBest.height/2 - cy);
	            const featEl = area / (1.0 + distEl);
	            const featBest = bestArea / (1.0 + distBest);
	            if (featEl < featBest) continue;
	            if (featEl === featBest && !preferLast) continue;
	          }
	        }
	      }
	      bestScore = totalScore; bestArea = area;
	      bestEl = el;
	      const r = el.getBoundingClientRect(); // 语义评分分支必须自声明 r（2026-08-27 根因修复）
	      best = {x: r.left + r.width/2, y: r.top + r.height/2, w: r.width, h: r.height,
	              tag: el.tagName.toLowerCase(), text: t.slice(0, 120), matched: hit.matched};
	    }
	    // 命中容器下钻（2026-08-27）：bestEl 若是「结构性非交互容器」且文本为整块聚合，
	    // 则下钻到最内层可交互叶子，避免把点击/提取落在容器上。
	    if (bestEl && isContainerLike(bestEl) && !isRealInteractive(bestEl)) {
	      const leaf = leafOf(bestEl);
	      if (leaf && leaf !== bestEl) {
	        bestEl = leaf;
	        const r = leaf.getBoundingClientRect();
	        best = {x: r.left + r.width/2, y: r.top + r.height/2, w: r.width, h: r.height,
	                tag: leaf.tagName.toLowerCase(),
	                matched: best.matched,
	                text: norm(leaf.innerText || leaf.value || leaf.getAttribute('aria-label') ||
	                          leaf.getAttribute('data-testid') || leaf.getAttribute('title') ||
	                          leaf.placeholder || '').slice(0, 120)};
	      }
	    }
	    if (!best && sel) {
	      // 选择器命中但无语义分：回退可见元素（extract 取最后一个，其余取第一个）。
	      for (const el of els) {
	        if (!visible(el)) continue;
	        const r = el.getBoundingClientRect();
	        const t = norm(el.innerText || el.value || el.getAttribute('aria-label') ||
	                       el.getAttribute('data-testid') || el.getAttribute('title') || el.placeholder || '');
	        best = {x: r.left + r.width/2, y: r.top + r.height/2, w: r.width, h: r.height,
	                tag: el.tagName.toLowerCase(), text: t.slice(0, 120), matched: ""};
	        bestEl = el;
	        if (!preferLast) break;
	      }
	    }
	  }
	  // §5.0.3 条款 1/2（2026-09-25）：input/fill/select 只接受可编辑目标。赢家不可编辑时
	  // 先做 §5.0.7 Container Escalation（叶子装饰节点向承载容器反向穿透）；
	  // 穿透不到时在「可编辑候选（语义分 > 0）」内重选；重选不到不静默成功——保留原赢家并标
	  // editable=false，由派发层给出 input_not_editable 明确失败。
	  let editable = !!(bestEl && isEditableEl(bestEl));
	  if (editableOnly && !editable) {
	    // §5.0.7：Container Escalation（装饰叶子节点向承载容器反向穿透，消除 Wikipedia 等站点 VLM 延迟）
	    if (bestEl) {
	      let anc = bestEl.parentElement;
	      for (let depth = 0; anc && depth < 3; depth++, anc = anc.parentElement) {
	        if (anc.matches('body, html')) break;
	        const inputs = Array.from(anc.querySelectorAll('input, textarea, [contenteditable], [role="textbox"]'))
	          .filter(c => isEditableEl(c) && visible(c));
	        if (inputs.length === 1) {
	          bestEl = inputs[0];
	          editable = true;
	          break;
	        } else if (inputs.length > 1) {
	          let closestInput = null, minDist = Infinity;
	          const br = bestEl.getBoundingClientRect();
	          const bx = br.left + br.width/2, by = br.top + br.height/2;
	          for (const inp of inputs) {
	            const ir = inp.getBoundingClientRect();
	            const d = Math.hypot(ir.left + ir.width/2 - bx, ir.top + ir.height/2 - by);
	            if (d < minDist) { minDist = d; closestInput = inp; }
	          }
	          if (closestInput) {
	            bestEl = closestInput;
	            editable = true;
	            break;
	          }
	        }
	      }
	      if (editable) {
	        const r = bestEl.getBoundingClientRect();
	        best = {x: r.left + r.width/2, y: r.top + r.height/2, w: r.width, h: r.height,
	                tag: bestEl.tagName.toLowerCase(), matched: best.matched,
	                text: norm(bestEl.innerText || bestEl.value || bestEl.getAttribute('aria-label') ||
	                          bestEl.getAttribute('data-testid') || bestEl.getAttribute('title') ||
	                          bestEl.placeholder || '').slice(0, 120)};
	      }
	    }
	    const drillToEditable = container => {
	      if (!container) return null;
	      let leaf = null, leafArea = Infinity;
	      for (const c of Array.from(container.querySelectorAll(
	        'input, textarea, select, [contenteditable], [role="textbox"]'))) {
	        if (!isEditableEl(c) || !visible(c)) continue;
	        const a = areaEl(c);
	        if (a < leafArea) { leafArea = a; leaf = c; }
	      }
	      return leaf;
	    };
	    let alt = null, altScore = 0, altArea = Infinity, altMatched = '';
	    const considerAlt = (el, sc, matched) => {
	      if (!el || !visible(el) || sc <= 0) return;
	      const a = areaEl(el);
	      if (sc > altScore || (sc === altScore && a < altArea)) {
	        alt = el; altScore = sc; altArea = a; altMatched = matched || '';
	      }
	    };
	    if (!editable) {
	      for (const el of els) {
	        if (!el || !visible(el)) continue;
	        const hit = scoreEl(el);
	        if (hit.score <= 0) continue;
	        if (isEditableEl(el)) { considerAlt(el, hit.score, hit.matched); continue; }
	        // 容器（label/div/表单块）文本命中但自身不可编辑：下钻到其可编辑后代并继承
	        // 容器的语义分（Airwallex label→textarea 一类结构）。
	        considerAlt(drillToEditable(el), hit.score, hit.matched);
	      }
	      if (bestEl && bestScore > 0) considerAlt(drillToEditable(bestEl), bestScore, best.matched);
	      if (alt) {
	        bestEl = alt;
	        const r = alt.getBoundingClientRect();
	        best = {x: r.left + r.width/2, y: r.top + r.height/2, w: r.width, h: r.height,
	                tag: alt.tagName.toLowerCase(), matched: altMatched,
	                text: norm(alt.innerText || alt.value || alt.getAttribute('aria-label') ||
	                          alt.getAttribute('data-testid') || alt.getAttribute('title') ||
	                          alt.placeholder || '').slice(0, 120)};
	        editable = true;
	      }
	    }
	  }
	  document.querySelectorAll('[data-uws-target]').forEach(e => delete e.dataset.uwsTarget);
	  if (bestEl) {
	    try { bestEl.dataset.uwsTarget = '1'; } catch (e) {}
	    // 视口外元素：滚入视野后重测几何（长页面目标定位，P0 加固）。
	    try { bestEl.scrollIntoView({block: 'center', inline: 'center'}); } catch (e) {}
	    const r2 = bestEl.getBoundingClientRect();
	    if (r2.width >= 2 && r2.height >= 2) {
	      best = {x: r2.left + r2.width/2, y: r2.top + r2.height/2, w: r2.width, h: r2.height,
	              tag: bestEl.tagName.toLowerCase(), text: best.text, matched: best.matched};
	    }
	  }
	  return best ? {found: true, x: best.x, y: best.y, w: best.w, h: best.h,
	                 tag: best.tag, text: best.text, matched: best.matched || "",
	                 editable: editable} : {found: false};`
