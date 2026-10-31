package find

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"uws/pkg/browser/input"
)

// resolveNodeByIdExpr 生成按 BFS 元素节点索引回查元素的表达式。
// BFS 顺序必须与 probeCollectExpr 完全一致：根 = document.documentElement ||
// document.body，仅 children（元素节点）入队，node_id == 队列索引 qi
// （C++ 探针按输入序 next_id_++ 分配，每个元素节点恰好产出一条记录）。
func resolveNodeByIdExpr(nodeID int32) string {
	const js = `(() => {
	  var norm = function(s){ return s == null ? '' : String(s).replace(/\s+/g,' ').trim(); };
	  var root = document.documentElement || document.body;
	  var queue = [root];
	  for (var qi = 0; qi < queue.length; qi++) {
	    var el = queue[qi];
	    if (!el || el.nodeType !== 1) continue;
	    if (qi === TARGET_ID) {
	      document.querySelectorAll('[data-uws-target]').forEach(function(e){ delete e.dataset.uwsTarget; });
	      try { el.dataset.uwsTarget = '1'; } catch (e) {}
	      try { el.scrollIntoView({block:'center', inline:'center'}); } catch (e) {}
	      var r = el.getBoundingClientRect();
	      var t = norm(el.innerText || el.value || el.getAttribute('aria-label') ||
	                   el.getAttribute('data-testid') || el.getAttribute('title') || el.placeholder || '');
	      return {found: r.width >= 2 && r.height >= 2,
	              x: r.left + r.width/2, y: r.top + r.height/2, w: r.width, h: r.height,
	              tag: el.tagName.toLowerCase(), text: t.slice(0,120)};
	    }
	    for (var ci = 0; ci < el.children.length; ci++) queue.push(el.children[ci]);
	  }
	  return {found: false};
	})()`
	return strings.ReplaceAll(js, "TARGET_ID", strconv.Itoa(int(nodeID)))
}

// NodeByID 按探针 node_id（BFS 元素节点索引）回查目标元素（宿主 `resolveNodeById` 的逐行搬移），
// 返回与 `Scan` 同构的 `input.ElementTarget`（marker + rect + tag/text）。
// node_id 越界 / 元素不可见返回 Found=false（不抛错，调用方按 node_not_found）。
func NodeByID(ctx context.Context, ev Evaluator, nodeID int32) (input.ElementTarget, error) {
	var t input.ElementTarget
	raw, err := ev.EvalRaw(ctx, resolveNodeByIdExpr(nodeID))
	if err != nil {
		return input.ElementTarget{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return input.ElementTarget{}, errEvalEmpty
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return input.ElementTarget{}, fmt.Errorf("cdp: decode node target: %w", err)
	}
	return t, nil
}
