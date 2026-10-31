package find

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// HitPoint 是坐标命中元素的落点自证证据（spec/algorithm-kernel.md §5.0.9 条款 6）：
// elementFromPoint 解出的受体（CSS 路径 ＋ 标签）与它的可见文本。两者必须在**同一步**
// 求值里取出——分两次取值会留下「解析受体」与「读文本」之间的页面变化窗口，证据就不可信了。
type HitPoint struct {
	Selector string `json:"sel"`  // CSS 路径（tag + class + nth-of-type，遇带 id 祖先截断）；无命中为空
	Tag      string `json:"tag"`  // 命中元素标签（小写）
	Text     string `json:"text"` // 可见文本（页内已折叠空白；本包按 rune 截断 ≤hitTextLimit）
}

// hitTextLimit 是落点文本上限（rune）：与 `browser.HitElement.Text` 同口径（≤120）。
const hitTextLimit = 120

// ElementAtPoint 解析 (x,y) 处命中的元素（宿主 `elementPathAt` 的逐行搬移 ＋ 同一拍取证
// 可见文本）。文本口径：自身 `innerText`，为空则上溯 ≤3 层祖先，且只接受**可归因于命中
// 元素**的祖先文本（除命中链上的子节点外，兄弟子树均无文本贡献）；旁支文案归「证据不足」
// ——它是别人（兄弟/无关子树）的文案，收下就会把无关文本当成落点证据（§5.0.9 条款 6
// 「上溯可信边界」，2026-09-30 真机订正：canvas + 旁支段落曾造出假 miss 硬失败）。
// 无元素 / 命中 body / 命中文档根返回零值 HitPoint ＋ nil（调用方按空选择器走 no_element）。
func ElementAtPoint(ctx context.Context, ev Evaluator, x, y int) (HitPoint, error) {
	expr := strings.ReplaceAll(strings.ReplaceAll(elementAtPointJS, "X", strconv.Itoa(x)), "Y", strconv.Itoa(y))
	raw, err := ev.EvalRaw(ctx, expr)
	if err != nil {
		return HitPoint{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return HitPoint{}, errEvalEmpty
	}
	var p HitPoint
	if err := json.Unmarshal(raw, &p); err != nil {
		return HitPoint{}, err
	}
	p.Text = truncateRunes(p.Text, hitTextLimit)
	return p, nil
}

// truncateRunes 按 rune 截断：与 `pkg/engine` 侧同口径，不按字节或 UTF-16 码元切
// （否则 CJK/emoji 会被切成半个字符）。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// elementAtPointJS 是落点解析与文本取证的页内实现（单次求值）。X/Y 由 strconv 代入。
// 文本取证失败（如宿主元素受限）不改变选择器解析结果：文本留空即「证据不足」，
// 由引擎侧按 §5.0.9 条款 6 的三态处置（缺证据不改判出口、但不写 learned）。
// 上溯只接受可归因于命中元素的祖先：`attributable` 要求除命中链上的子节点外兄弟子树
// 均无文本；否则该祖先的文本出自旁支，留空（证据不足），不认错证据。
const elementAtPointJS = `(() => {
  const norm = function (s) { return s == null ? '' : String(s).replace(/\s+/g, ' ').trim(); };
  const el = document.elementFromPoint(X, Y);
  if (!el || el === document.body || el === document.documentElement) return { sel: '', tag: '', text: '' };
  const parts = [];
  let n = el;
  while (n && n.nodeType === 1 && n !== document.body && n !== document.documentElement) {
    let sel = n.tagName.toLowerCase();
    if (n.id) {
      sel += '#' + CSS.escape(n.id);
      parts.unshift(sel);
      break;
    }
    if (n.classList && n.classList.length) {
      sel += '.' + Array.from(n.classList).slice(0, 3).map(c => CSS.escape(c)).join('.');
    }
    let i = 1, sib = n;
    while ((sib = sib.previousElementSibling)) {
      if (sib.tagName === n.tagName) i++;
    }
    sel += ':nth-of-type(' + i + ')';
    parts.unshift(sel);
    n = n.parentElement;
  }
  const attributable = function (m, child) {
    for (const sib of m.children) {
      if (sib === child) continue;
      if (norm(sib.innerText)) return false;
    }
    return true;
  };
  let text = '';
  try {
    let m = el, child = null;
    for (let up = 0; m && up <= 3; up++) {
      const t = norm(m.innerText);
      if (t) {
        if (up === 0 || attributable(m, child)) text = t;
        break;
      }
      if (m === document.body || m === document.documentElement) break;
      child = m;
      m = m.parentElement;
    }
  } catch (e) { text = ''; }
  return { sel: parts.join(' > '), tag: el.tagName.toLowerCase(), text: text };
})()`
