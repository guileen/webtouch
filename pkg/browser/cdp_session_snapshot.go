package browser

import (
	"context"
	"strings"
	"time"

	"uws/pkg/browser/find"
)

// pageSnapshotJS 是页面侧候选只读快照的页内脚本
// （spec/algorithm-kernel.md §5.0.13 条款 3 ＋ §5.0.14 条款 1/2：派发前采样 ＋ DOM 子树 GT）。
// 只读：不发出任何 Input.* 命令、不改 DOM、不触发导航。
// $PIVOT_SEL$ / $PIVOT_TAG$ / $PIVOT_TEXT$ 是采样枢轴占位符（JS 字符串字面量，空串=未提供）。
const pageSnapshotJS = `(() => {
  const norm = s => (s == null ? '' : String(s)).replace(/\s+/g, ' ').trim();
  const isDataPayload = v => {
    if (typeof v !== 'string' || v === '') return false;
    if (/^<style/i.test(norm(v))) return true;
    const s = v.trim();
    if (s.charAt(0) !== '{' && s.charAt(0) !== '[') return false;
    try { JSON.parse(s); return true; } catch (e) { return false; }
  };
  // 条款 3②：既有交互性选择器的加性扩列（[role="checkbox"] / [tabindex]:not([tabindex="-1"])）。
  const SEL = 'button, a[href], input:not([type=hidden]), textarea, select, ' +
    '[role="button"], [role="link"], [role="menuitem"], [role="checkbox"], ' +
    '[onclick], [data-testid], [tabindex]:not([tabindex="-1"])';
  const MASK = '[VAL_MASKED]';
  const tagOf = el => ((el.tagName || '') + '').toLowerCase();
  const roleOf = el => (el.getAttribute ? (el.getAttribute('role') || '') : '');
  const cssPath = el => {
    const parts = [];
    for (let n = el; n && n.nodeType === 1 && parts.length < 8; n = n.parentElement) {
      let p = tagOf(n);
      const par = n.parentElement;
      if (par) {
        let i = 1;
        for (const s of par.children) { if (s === n) break; if (tagOf(s) === p) i++; }
        p += ':nth-of-type(' + i + ')';
      }
      parts.unshift(p);
    }
    return parts.join(' > ');
  };
  const labelText = el => norm(el.getAttribute('aria-label') || el.getAttribute('placeholder') ||
    el.getAttribute('name') || el.getAttribute('title') || el.getAttribute('data-testid') || '');
  // 条款 4②：input/textarea 的值一律不进文本字段——无标签源时落 [VAL_MASKED]，绝不落明文。
  const textOf = el => {
    const t = tagOf(el);
    if (t === 'input' || t === 'textarea') return labelText(el) || (el.value ? MASK : '');
    const raw = el.innerText || el.getAttribute('aria-label') || el.getAttribute('title') || '';
    return isDataPayload(raw) ? '' : norm(raw);
  };
  const rectOf = el => el.getBoundingClientRect();
  const visible = el => {
    const cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none') return false;
    const r = rectOf(el);
    return r.width >= 2 && r.height >= 2;
  };
  const landmarkOf = el => {
    const lm = el.closest('main, [role="main"], article, nav, [role="navigation"], header, [role="banner"], footer, [role="contentinfo"], aside, [role="complementary"]');
    if (!lm) return 'other';
    const t = tagOf(lm), r = roleOf(lm);
    if (t === 'main' || r === 'main' || t === 'article') return 'main';
    if (t === 'nav' || r === 'navigation') return 'nav';
    if (t === 'header' || r === 'banner') return 'header';
    if (t === 'footer' || r === 'contentinfo') return 'footer';
    if (t === 'aside' || r === 'complementary') return 'aside';
    return 'other';
  };
  // §5.0.14 决议 0/3 ＋ §5.0.16 ＋ §5.0.17：pivot 来源。**物理受体派发点优先**（引擎侧经 PreDispatchReceptorSink
  // 回传的派发前受体坐标）；无受体点时退化**补充身份**——KB 规则选择器 → 「Tag ＋ Text 且**页内唯一**」；
  // 两者皆无时退化为 CDP 原生检索粗锚点（§5.0.16 条款 3）；再无时退化为标题空间路标锚点（§5.0.17 条款 2）。
  // 四者皆无 ⇒ 无 pivot（候选进入 Landmark 配额保护与深水区分层采样，GT 无法判定）。
  const hasReceptor = $RECEPTOR_HAS$, rx = $RECEPTOR_X$, ry = $RECEPTOR_Y$;
  const pSel = $PIVOT_SEL$, pTag = $PIVOT_TAG$, pText = $PIVOT_TEXT$;
  const hasCoarse = $COARSE_HAS$, coarseX = $COARSE_X$, coarseY = $COARSE_Y$;
  const pSem = $PIVOT_SEM$;
  let receptorEl = null;
  if (hasReceptor) { try { receptorEl = document.elementFromPoint(rx, ry) || null; } catch (e) { receptorEl = null; } }
  let pivot = null;
  if (!hasReceptor) {
    if (pSel) { try { pivot = document.querySelector(pSel); } catch (e) { pivot = null; } }
    if (!pivot && pText && pText.length <= 200) {
      let hits = 0, fuzzyEl = null, scanned = 0, capped = false;
      for (const n of document.querySelectorAll(pTag || '*')) {
        if (hits > 1) break;
        if (++scanned > 2000) { capped = true; break; }
        if (pTag && tagOf(n) !== pTag) continue;
        if (!visible(n)) continue;
        if (textOf(n) !== pText) continue;
        hits++; fuzzyEl = n;
      }
      if (hits === 1 && !capped) pivot = fuzzyEl;
    }
  }
  // §5.0.17 条款 2：提取 H1~H6 标题树空间索引
  const headings = [];
  for (const h of document.querySelectorAll('h1, h2, h3, h4, h5, h6, [role="heading"]')) {
    if (!visible(h)) continue;
    const ht = norm(h.innerText || h.getAttribute('aria-label') || '');
    if (!ht || ht.length > 200) continue;
    const hr = rectOf(h);
    let level = 2;
    const t = tagOf(h);
    if (/^h[1-6]$/.test(t)) { level = parseInt(t.charAt(1), 10); }
    else { const al = h.getAttribute('aria-level'); if (al) level = parseInt(al, 10) || 2; }
    headings.push({ text: ht, level: level, tag: t, x: hr.left + hr.width / 2, y: hr.top + hr.height / 2 });
  }
  // §5.0.17 条款 2②：目标语义匹配标题树自举空间路标锚点
  let hAnchorX = 0, hAnchorY = 0, hasHeading = false;
  if (!hasReceptor && !pivot && !hasCoarse && pSem) {
    const q = norm(pSem).toLowerCase();
    let bestScore = -1;
    for (const h of headings) {
      const ht = h.text.toLowerCase();
      let score = -1;
      if (ht === q) {
        score = 1000;
      } else if (ht.includes(q) || q.includes(ht)) {
        score = 500 - Math.abs(ht.length - q.length) * 2 - (h.level || 2) * 5;
      }
      if (score > bestScore) {
        bestScore = score;
        hAnchorX = h.x; hAnchorY = h.y;
        hasHeading = true;
      }
    }
  }
  // 锚点：受体点优先；无受体时退化为补充身份元素矩形中心；再无时退化为 CDP 粗锚点；再无退化为标题锚点；皆无 ⇒ 无锚点。
  const anchorEl = receptorEl || pivot;
  const anchorX = hasReceptor ? rx : (pivot ? (pivot.getBoundingClientRect().left + pivot.getBoundingClientRect().width / 2) : (hasCoarse ? coarseX : (hasHeading ? hAnchorX : 0)));
  const anchorY = hasReceptor ? ry : (pivot ? (pivot.getBoundingClientRect().top + pivot.getBoundingClientRect().height / 2) : (hasCoarse ? coarseY : (hasHeading ? hAnchorY : 0)));
  const hasAnchor = hasReceptor || !!pivot || hasCoarse || hasHeading;
  const nodes = [];
  const els = [];
  const seen = new Set();
  for (const el of document.querySelectorAll(SEL)) {
    // 条款 3⑥：按候选身份（CSS 路径）去重，不按文本去重（同文案多节点是真实候选）。
    const path = cssPath(el);
    if (seen.has(path) || !visible(el)) continue;
    const t = textOf(el);
    if (!t || t.length > 200) continue;
    seen.add(path);
    const r = rectOf(el);
    nodes.push({ tag: tagOf(el), role: roleOf(el), selector: path,
      x: r.left, y: r.top, w: r.width, h: r.height, text: t, landmark: landmarkOf(el) });
    els.push(el);
  }
  // §5.0.17 条款 1/3：有锚点时欧氏距离排序；无锚点时 Landmark 配额保护与深水区分层采样
  let order = [];
  let truncated = nodes.length > 64;
  if (hasAnchor) {
    const cx = anchorX, cy = anchorY;
    const dist = i => Math.hypot(cx - (nodes[i].x + nodes[i].w / 2), cy - (nodes[i].y + nodes[i].h / 2));
    order = nodes.map((_, i) => i).sort((a, b) => { const da = dist(a), db = dist(b); return da === db ? a - b : da - db; });
  } else {
    truncated = true;
    const mainIdx = [], otherIdx = [];
    for (let i = 0; i < nodes.length; i++) {
      if (nodes[i].landmark === 'main') mainIdx.push(i);
      else otherIdx.push(i);
    }
    if (mainIdx.length === 0) {
      if (nodes.length <= 64) {
        order = nodes.map((_, i) => i);
      } else {
        const step = (nodes.length - 1) / 63;
        const s = new Set();
        for (let k = 0; k < 64; k++) {
          const idx = Math.round(k * step);
          if (!s.has(idx)) { s.add(idx); order.push(idx); }
        }
      }
    } else {
      let kMain = Math.min(mainIdx.length, 48);
      let kOther = Math.min(otherIdx.length, 64 - kMain);
      if (kMain + kOther < 64 && mainIdx.length > kMain) {
        kMain = Math.min(mainIdx.length, 64 - kOther);
      }
      const chosenMain = [];
      if (mainIdx.length <= kMain) {
        for (let i = 0; i < mainIdx.length; i++) chosenMain.push(mainIdx[i]);
      } else {
        const step = (mainIdx.length - 1) / (kMain - 1);
        const s = new Set();
        for (let k = 0; k < kMain; k++) {
          const idx = mainIdx[Math.round(k * step)];
          if (!s.has(idx)) { s.add(idx); chosenMain.push(idx); }
        }
      }
      const chosenOther = [];
      if (otherIdx.length <= kOther) {
        for (let i = 0; i < otherIdx.length; i++) chosenOther.push(otherIdx[i]);
      } else {
        const step = (otherIdx.length - 1) / (kOther - 1);
        const s = new Set();
        for (let k = 0; k < kOther; k++) {
          const idx = otherIdx[Math.round(k * step)];
          if (!s.has(idx)) { s.add(idx); chosenOther.push(idx); }
        }
      }
      order = chosenMain.concat(chosenOther);
    }
  }
  const keptIdx = order.slice(0, 64);
  const kept = keptIdx.map(i => nodes[i]);
  const keptEls = keptIdx.map(i => els[i]);
  // ── 条款 2：GT 三级判据（恒等 → 祖先链 ≤3 阶 → 几何兜底），命中即止、恒 ≤1 条 ──
  // 「命中代理」＝受体派发点处的最深元素（决议 0：受体是唯一合法锚点）；无受体点时退化为
  // 补充身份元素矩形中心处的最深元素。派发尚未发生，用 elementFromPoint 作只读代理
  // （§5.0.14 条款 1①：GT 必须反映派发前状态，不得用真实命中点或事后回读）。
  let gt = -1;
  // §5.0.16 条款 3②：粗锚点仅用于候选排序截断，严禁参与 GT 判定（必须有受体或精准补充身份）
  if (hasReceptor || !!pivot) {
    const hx = anchorX, hy = anchorY;
    const hit = hasReceptor ? receptorEl : (document.elementFromPoint(hx, hy) || pivot);
    if (hit) {
      const hitPath = cssPath(hit);
      for (let i = 0; i < kept.length; i++) {
        if (kept[i].selector === hitPath) { gt = i; break; }   // ① 恒等（同一页内元素）
      }
      if (gt < 0) {
        // ② 祖先链 ≤3 阶（容忍 <button><svg/><span/></button> 类冒泡穿透）：
        //    阶数最小者胜（最近的可交互祖先），同阶取候选文档序（kept 序）。
        let bestHop = 0;
        for (let i = 0; i < keptEls.length; i++) {
          const anc = keptEls[i];
          if (!anc) continue;
          let hop = 0, matched = false;
          for (let n = hit.parentElement; n; n = n.parentElement) {
            hop++;
            if (hop > 3) break;
            if (n === anc) { matched = true; break; }
          }
          if (matched && (gt < 0 || hop < bestHop)) { gt = i; bestHop = hop; }
        }
      }
      if (gt < 0) {
        // ③ 几何兜底：矩形包含（受体点确实落在候选矩形内 ⇒ 可信的物理包含关系）。
        for (let i = 0; i < kept.length; i++) {
          const n = kept[i];
          if (hx >= n.x && hx <= n.x + n.w && hy >= n.y && hy <= n.y + n.h) { gt = i; break; }
        }
        // 「矩形中心最近」只在**无受体点**的补充身份口径下保留（决议 0②：受体是权威锚点，
        // 权威锚点落空时如实判 -1，绝不用几何猜测编造 GT）。
        if (gt < 0 && !hasReceptor && kept.length > 0) {
          let bd = Infinity;
          for (let i = 0; i < kept.length; i++) {
            const n = kept[i];
            const d = Math.hypot(hx - (n.x + n.w / 2), hy - (n.y + n.h / 2));
            if (d < bd) { bd = d; gt = i; }
          }
        }
      }
    }
  }
  // 条款 3④ ＋ 条款 7：骨架改**对称挂载**——每个入选候选各携带**自身**直系 1 阶
  // （parent → children → siblings），只留 tag + role（class 一律丢弃）、文本按条款 4⑤
  // 截断；每候选上限 8 条，超出按文档序截断并置 neighbors_truncated。消除「仅 GT 挂骨架
  // ⇒ 负样本骨架全空、骨架成 GT 伪特征」（CON-214 硬线）。
  const neighborsOf = el => {
    const out = [];
    const seenNb = new Set();
    let nbTrunc = false;
    if (!el || el.nodeType !== 1) return { neighbors: out, truncated: false };
    const push = (rel, n) => {
      if (!n || n.nodeType !== 1 || seenNb.has(n)) return;
      if (out.length >= 8) { nbTrunc = true; return; }
      seenNb.add(n);
      out.push({ rel: rel, tag: tagOf(n), role: roleOf(n), text: textOf(n) });
    };
    push('parent', el.parentElement);
    if (el.children) for (const c of el.children) push('child', c);
    const par = el.parentElement;
    if (par && par.children) for (const s of par.children) { if (s !== el) push('sibling', s); }
    return { neighbors: out, truncated: nbTrunc };
  };
  const outNodes = kept.map((n, i) => {
    const nb = neighborsOf(keptEls[i]);
    return { tag: n.tag, role: n.role, selector: n.selector,
      x: n.x, y: n.y, w: n.w, h: n.h, text: n.text, landmark: n.landmark,
      neighbors: nb.neighbors, neighbors_truncated: nb.truncated };
  });
  return { nodes: outNodes, headings: headings, truncated: truncated, gt_index: gt };
})()`

// PageSnapshot 执行一次页面侧候选只读快照（§5.0.13 条款 3；采样时点见 §5.0.14 条款 1：
// 必须由调用方在**物理派发之前**触发）。pivot 是决策目标的派发前只读身份（Phase A）。
// 第二个返回值为 false 表示本会话/本页取不到快照（调用方须退回既有路径并如实标记来源）。
func (s *cdpSession) PageSnapshot(pivot PageSnapshotPivot) (PageSnapshot, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// §5.0.16：无物理受体派发点与补充身份时，若有意图语义或文本，调用 CDP 原生检索自举粗锚点
	if !pivot.HasPoint && !pivot.HasCoarsePoint && (pivot.TargetSemantic != "" || pivot.Text != "") {
		query := pivot.TargetSemantic
		if query == "" {
			query = pivot.Text
		}
		if match, err := find.SearchSemantic(ctx, bridgeCaller{s}, query); err == nil && match != nil {
			pivot.HasCoarsePoint = true
			pivot.CoarseX = match.X
			pivot.CoarseY = match.Y
		}
	}

	semQuery := pivot.TargetSemantic
	if semQuery == "" {
		semQuery = pivot.Text
	}

	js := strings.ReplaceAll(pageSnapshotJS, "$RECEPTOR_HAS$", boolJS(pivot.HasPoint))
	js = strings.ReplaceAll(js, "$RECEPTOR_X$", floatJS(pivot.PointX))
	js = strings.ReplaceAll(js, "$RECEPTOR_Y$", floatJS(pivot.PointY))
	js = strings.ReplaceAll(js, "$PIVOT_SEL$", quoteJS(pivot.Selector))
	js = strings.ReplaceAll(js, "$PIVOT_TAG$", quoteJS(pivot.Tag))
	js = strings.ReplaceAll(js, "$PIVOT_TEXT$", quoteJS(pivot.Text))
	js = strings.ReplaceAll(js, "$COARSE_HAS$", boolJS(pivot.HasCoarsePoint))
	js = strings.ReplaceAll(js, "$COARSE_X$", floatJS(pivot.CoarseX))
	js = strings.ReplaceAll(js, "$COARSE_Y$", floatJS(pivot.CoarseY))
	js = strings.ReplaceAll(js, "$PIVOT_SEM$", quoteJS(semQuery))
	var raw struct {
		Nodes []struct {
			Tag      string  `json:"tag"`
			Role     string  `json:"role"`
			Selector string  `json:"selector"`
			X        float64 `json:"x"`
			Y        float64 `json:"y"`
			W        float64 `json:"w"`
			H        float64 `json:"h"`
			Text     string  `json:"text"`
			Landmark string  `json:"landmark"`
			// §5.0.13 条款 7：每个候选**自身**的 1 阶骨架（对称挂载）。
			Neighbors []struct {
				Rel  string `json:"rel"`
				Tag  string `json:"tag"`
				Role string `json:"role"`
				Text string `json:"text"`
			} `json:"neighbors"`
			NeighborsTruncated bool `json:"neighbors_truncated"`
		} `json:"nodes"`
		Headings []struct {
			Text  string  `json:"text"`
			Level int     `json:"level"`
			Tag   string  `json:"tag"`
			X     float64 `json:"x"`
			Y     float64 `json:"y"`
		} `json:"headings"`
		Truncated bool `json:"truncated"`
		// gt_index 是页内三级判据（§5.0.14 条款 2）的结果；缺省/越界一律视为无法判定。
		GTIndex *int `json:"gt_index"`
	}
	if err := s.evalValue(ctx, js, &raw); err != nil {
		return PageSnapshot{}, false
	}
	snap := PageSnapshot{Truncated: raw.Truncated, GroundTruthIndex: PageSnapshotGroundTruthUnknown}
	for _, n := range raw.Nodes {
		node := PageSnapshotNode{
			Tag: n.Tag, Role: n.Role, Selector: n.Selector,
			X: n.X, Y: n.Y, W: n.W, H: n.H, Text: n.Text,
			NeighborsTruncated: n.NeighborsTruncated,
			Landmark:           n.Landmark,
		}
		for _, nb := range n.Neighbors {
			node.Neighbors = append(node.Neighbors, PageSnapshotNeighbor{Rel: nb.Rel, Tag: nb.Tag, Role: nb.Role, Text: nb.Text})
		}
		snap.Nodes = append(snap.Nodes, node)
	}
	for _, h := range raw.Headings {
		snap.Headings = append(snap.Headings, PageHeading{
			Text:  h.Text,
			Level: h.Level,
			Tag:   h.Tag,
			X:     h.X,
			Y:     h.Y,
		})
	}
	// 防御：越界/负数一律回落为「无法判定」（不得用非法下标去锚 GT）。
	if raw.GTIndex != nil && *raw.GTIndex >= 0 && *raw.GTIndex < len(snap.Nodes) {
		snap.GroundTruthIndex = *raw.GTIndex
	}
	// §5.0.13 条款 7④：顶层 Skeleton 保留为 **GT 候选 neighbors 的别名**（兼容旧读取路径）。
	if idx := snap.GroundTruthIndex; idx >= 0 && idx < len(snap.Nodes) {
		snap.Skeleton = snap.Nodes[idx].Neighbors
	}
	return snap, true
}
