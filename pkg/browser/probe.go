// WASM 探针页内注入与候选解码（M2 真实链路，USER_ACTION #14）。
//
// 真实 CDP 会话经 Runtime.evaluate 注入 standalone WASM 探针（16MB 线性内存
// 硬边界）：JS BFS 遍历 DOM → 19 字段 bulk 协议（\x1e 记录 / \x1f 字段分隔）
// 分块写入 scratch → 探针按分级准入/驱逐保留高价值节点；Go 侧读取 kept 节点
// 的 semantic_text + ast_semantic_hash 生成 PageCandidate（SimHash 供 L3
// 结构校验）；截断时 JS 慢速遍历在 200ms 内补偿 BFS 前 200 溢出节点。
// 探针缺失/注入失败时优雅降级到既有 JS 文本采集（collectJSCandidates）。
package browser

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// ── SerializedNode 二进制契约（与 pkg/probe_wasm/src/probe.h 逐字段对齐） ──

const (
	probeTagOffset            = 0
	probeRoleOffset           = 16
	probeSemanticTextOffset   = 48
	probeParentSemanticOffset = 176
	probeZonalOffset          = 304 // [3][32]
	probeParentTagOffset      = 400
	probeParentRoleOffset     = 416
	probeInputTypeOffset      = 448
	probeAttributesOffset     = 464
	probeHashOffset           = 720 // uint64 LE
	probeCompositeKeyOffset   = 728
	probeXPathOffset          = 824
	probeClassNamesOffset     = 1080
	probeIDOffset             = 1208
	probeFlagsOffset          = 1272
	probeDepthOffset          = 1273
	probeTierOffset           = 1274
	probeNodeIDOffset         = 1276 // uint32 LE
	probeStructHashOffset     = 1280 // uint64 LE（Tier 1.5 WASM 内置结构签名）
	probeSerializedNodeSize   = 1288
	probeZonalKeywordBytes    = 32
)

// probeNode 是 SerializedNode 的可读解码（引擎侧消费用；当前注入路径只取
// semantic_text + ast_semantic_hash，完整解码供二进制契约单测与后续
// decision_trace 元信息消费）。
type probeNode struct {
	Tag                string
	Role               string
	SemanticText       string
	ParentSemanticText string
	ZonalKeywords      [3]string
	ParentTag          string
	ParentRole         string
	InputType          string
	Attributes         string
	AstSemanticHash    uint64
	CompositeKey       string
	XPath              string
	ClassNames         string
	ID                 string
	Flags              uint8
	DomDepth           uint8
	Tier               uint8
	NodeID             uint32
	StructuralHash     uint64
}

// DecodeSerializedNode 解析探针固定宽度二进制记录（零信任输入：长度不足报错）。
// 偏移表必须与 probe.h 保持同步；C++ 侧 static_assert(sizeof<2048)，
// 实际 1280B（uint64 对齐后无 padding）。
func DecodeSerializedNode(b []byte) (probeNode, error) {
	if len(b) < probeSerializedNodeSize {
		return probeNode{}, fmt.Errorf("probe: short serialized node: %d < %d",
			len(b), probeSerializedNodeSize)
	}
	readStr := func(off, max int) string {
		end := off + max
		if end > len(b) {
			end = len(b)
		}
		if i := indexByte(b[off:end], 0); i >= 0 {
			end = off + i
		}
		return string(b[off:end])
	}
	var n probeNode
	n.Tag = readStr(probeTagOffset, 16)
	n.Role = readStr(probeRoleOffset, 32)
	n.SemanticText = readStr(probeSemanticTextOffset, 128)
	n.ParentSemanticText = readStr(probeParentSemanticOffset, 128)
	for i := 0; i < 3; i++ {
		n.ZonalKeywords[i] = readStr(probeZonalOffset+i*probeZonalKeywordBytes, probeZonalKeywordBytes)
	}
	n.ParentTag = readStr(probeParentTagOffset, 16)
	n.ParentRole = readStr(probeParentRoleOffset, 32)
	n.InputType = readStr(probeInputTypeOffset, 16)
	n.Attributes = readStr(probeAttributesOffset, 256)
	n.AstSemanticHash = binary.LittleEndian.Uint64(b[probeHashOffset : probeHashOffset+8])
	n.CompositeKey = readStr(probeCompositeKeyOffset, 96)
	n.XPath = readStr(probeXPathOffset, 256)
	n.ClassNames = readStr(probeClassNamesOffset, 128)
	n.ID = readStr(probeIDOffset, 64)
	n.Flags = b[probeFlagsOffset]
	n.DomDepth = b[probeDepthOffset]
	n.Tier = b[probeTierOffset]
	n.NodeID = binary.LittleEndian.Uint32(b[probeNodeIDOffset : probeNodeIDOffset+4])
	n.StructuralHash = binary.LittleEndian.Uint64(b[probeStructHashOffset : probeStructHashOffset+8])
	return n, nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// ── WASM 注入与采集（JS 常量；避免模板字符串/反引号，Go raw string 直接内嵌） ──

// FaultCSPWasmBlocked 是页内 WASM 探针被站点 CSP 拒绝时的结构化降级代号
// （spec/algorithm-kernel.md §5.0.8 条款 3）。
const FaultCSPWasmBlocked = "csp_wasm_blocked"

// classifyProbeDegradation 把探针失败文本分类为结构化降级代号（纯函数，便于单测）。
// 目前只识别严格 CSP 拒绝 WASM 编译/实例化这一类；其余失败返回空串（按普通错误处理）。
func classifyProbeDegradation(msg string) string {
	low := strings.ToLower(msg)
	for _, marker := range []string{
		"wasm-unsafe-eval",
		"unsafe-eval",
		"content security policy",
		"content-security-policy",
	} {
		if strings.Contains(low, marker) {
			return FaultCSPWasmBlocked
		}
	}
	return ""
}

// probeCollectExpr 构造采集表达式：注入（按需实例化）→ BFS 遍历 → bulk 分块
// 写入 → 读取 kept 节点 {text, hash}。b64 为 wasm 字节 base64。
func probeCollectExpr(b64 string) string {
	const js = `(async () => {
  var norm = function(s){ return s == null ? '' : String(s).replace(/[\s\u001e\u001f]+/g,' ').trim(); };
  var esc = function(v){ return norm(v).replace(/[\u001e\u001f]/g,' '); };
  var bounded = function(v){ return esc(v).slice(0, 1024); };
  var probe = window.__uwsProbe;
  if (!probe) {
    try {
      var bin = Uint8Array.from(atob(B64), function(c){ return c.charCodeAt(0); });
      var inst = await WebAssembly.instantiate(bin);
      probe = inst.instance.exports;
      window.__uwsProbe = probe;
    } catch (e) {
      // §5.0.8 条款 3：严格 CSP 下 WASM 无法编译/实例化 —— 显式回报原因，
      // 交 Go 侧分类为 csp_wasm_blocked，禁止静默降级。
      return {ok: false, blocked: true, err: String((e && e.message) || e)};
    }
  }
  probe.uws_probe_reset();
  var mem = new Uint8Array(probe.memory.buffer);
  var scratchPtr = probe.uws_probe_scratch() >>> 0;
  var scratchSize = probe.uws_probe_scratch_size();
  var RS = String.fromCharCode(30);
  var FS = String.fromCharCode(31);
  var chunk = '';
  var flush = function(){
    if (!chunk) return true;
    var bytes = new TextEncoder().encode(chunk);
    for (var off = 0; off < bytes.length;) {
      var end = Math.min(bytes.length, off + Math.min(scratchSize, 32768));
      if (end < bytes.length) {
        // Keep every ParseBulk call record-aligned. Splitting a multibyte or
        // oversized JS chunk at an arbitrary byte can turn its fragments into
        // separate nodes and desynchronize C++ node IDs from the DOM queue.
        var recordEnd = bytes.lastIndexOf(30, end - 1);
        if (recordEnd < off) { chunk = ''; return false; }
        end = recordEnd + 1;
      }
      var n = end - off;
      mem.set(bytes.subarray(off, off + n), scratchPtr);
      if (probe.uws_probe_nodes_bulk(0, n) !== 0) { chunk = ''; return false; }
      off += n;
    }
    chunk = '';
    return true;
  };
  // §5.0.6 条款 3：数据载体判定——style 模板容器，或可解析为 JSON 对象/数组的数据块。
  // 「先解析后丢弃」：只有 JSON.parse 成功才判为数据载体，避免误伤以花括号/方括号起始的普通查询。
  var isDataPayload = function(v){
    if (typeof v !== 'string' || v === '') return false;
    if (/^<style/i.test(norm(v))) return true;
    var s = v.trim();
    if (s.charAt(0) !== '{' && s.charAt(0) !== '[') return false;
    try { JSON.parse(s); return true; } catch (e) { return false; }
  };
  var isInteractive = function(el){
    var tag = (el.tagName || '').toLowerCase();
    if (tag === 'style' || tag === 'script' || tag === 'noscript' || tag === 'template') return false;
    // §5.0.6 条款 3：内联数据载体（style 模板容器 / 可解析的 JSON 数据块）不是输入控件。
    if (tag === 'textarea' && isDataPayload(el.value)) return false;
    if (/^(button|input|select|textarea|a)$/i.test(el.tagName)) return true;
    // 2026-08-31 Airwallex 实战：企业后台表格行是 <tr onclick>（无 role/无 href），
    // 不纳入交互集则取不到行文本、也不算视口内 → 探针直接丢弃 → 语义点击 no_match、
    // VLM 也认不出。凡挂 onclick（属性或属性值）的元素一律视为可交互。
    if (el.onclick != null || el.getAttribute('onclick')) return true;
    // 后台常用 <span class="operation"> 等纯文本叶子承载点击；没有 role/handler 时，
    // 把有短文本且呈现 pointer 光标的叶子纳入探针，供语义与 node ref 路由定位。
    if (el.children.length === 0) {
      var pointerText = norm(el.textContent);
      if (pointerText && pointerText.length <= 120 && getComputedStyle(el).cursor === 'pointer') return true;
    }
    var role = el.getAttribute('role') || '';
    return /^(button|link|menuitem|checkbox|radio|tab|combobox)$/i.test(role);
  };
  // Tier 1.5 结构签名：[State:D] WASM 内置——JS 按真实 DOM 计算最近语义祖先
  // role（跳过 generic/none 幽灵包装），随 bulk 送入探针计算 structural_hash。
  var nearestSemAncRole = function(el){
    var p = el.parentElement;
    while (p && p !== document.documentElement && p.nodeType === 1) {
      var r = p.getAttribute ? p.getAttribute('role') : null;
      if (r && r !== 'generic' && r !== 'none') return r;
      p = p.parentElement;
    }
    return '';
  };
  var ownText = function(el, interactive){
    if (interactive) {
      var raw = el.value || el.placeholder || el.getAttribute('aria-label') ||
                el.getAttribute('data-testid') || el.getAttribute('title') || el.innerText || el.textContent;
      // §5.0.6 条款 3：style 模板与 JSON 数据块整体丢弃，不得进入语义文本源。
      if (typeof raw === 'string' && (raw.indexOf('<style') >= 0 || isDataPayload(raw))) return '';
      var val = esc(raw);
      if (val.indexOf('<style') >= 0) return '';
      // The WASM field cap is far below the transport scratch size. Bound each
      // text field here so even one pathological label remains record-aligned.
      return val.slice(0, 1024);
    }
    // 非交互节点不取 textContent：大页面下 body 子文本会被每个子元素重复
    // 摊平（O(n^2) 字符串分配，2 万节点实测卡死 20s+），只保留显式标注。
    return esc(el.getAttribute('aria-label') || el.getAttribute('data-testid') ||
               el.getAttribute('title') || '').slice(0, 1024);
  };
  // BFS 队列 + 父索引：每个节点的文本只计算一次（父语义文本读缓存，
  // 避免 parentElement.textContent 在兄弟节点上被重复摊平——O(n^2) 卡死根因）。
  var queue = [document.documentElement || document.body];
  var pIdx = [0];
  var nText = [];
  var ok = true;
  for (var qi = 0; qi < queue.length; qi++) {
    var el = queue[qi];
    if (el.nodeType === 1) {
      var interactive = isInteractive(el);
      nText[qi] = ownText(el, interactive);
      var t = nText[qi];
      var pe = el.parentElement;
      var parentText = qi === 0 ? '' : (nText[pIdx[qi]] || '');
      var ancRole = nearestSemAncRole(el);
      var inview = '0';
      if (interactive) {
        var r = el.getBoundingClientRect();
        inview = (r.width > 0 && r.height > 0 && r.bottom >= 0 && r.top <= (window.innerHeight || 0) &&
                  r.right >= 0 && r.left <= (window.innerWidth || 0)) ? '1' : '0';
      }
      chunk += [
        (el.tagName || '').toLowerCase(),
        bounded(el.getAttribute('role') || ''),
        t,
        parentText,
        '', '', '',
        pe ? pe.tagName.toLowerCase() : '',
        pe ? bounded(pe.getAttribute('role') || '') : '',
        el.closest('form') ? '1' : '0',
        bounded(el.type || ''),
        '', '', '0',
        inview,
        '', '', '',
        el.className && typeof el.className === 'string' ? bounded(el.className) : '',
        bounded(ancRole)
      ].join(FS) + RS;
      if (chunk.length >= 32768) { if (!flush()) { ok = false; break; } }
    }
    for (var ci = 0; ci < el.children.length; ci++) {
      queue.push(el.children[ci]);
      pIdx.push(qi);
    }
  }
  if (ok) ok = flush();
  probe.uws_probe_finalize();
  var kept = probe.uws_probe_kept_count();
  var out = [];
  var dec = new TextDecoder();
  var cssPath = function(el){
    var parts = [];
    for (var n = el; n && n.nodeType === 1; n = n.parentElement) {
      var tag = (n.tagName || '').toLowerCase();
      if (!tag) break;
      var parent = n.parentElement;
      if (parent) {
        var index = 1;
        for (var sib of parent.children) { if (sib === n) break; if ((sib.tagName || '').toLowerCase() === tag) index++; }
        tag += ':nth-of-type(' + index + ')';
      }
      parts.unshift(tag);
    }
    return parts.join(' > ');
  };
  for (var i = 0; i < kept; i++) {
    if (probe.uws_probe_copy_node(i, 0, scratchSize) <= 0) break;
    var text = dec.decode(mem.subarray(scratchPtr + 48, scratchPtr + 176)).split('\0')[0];
    var dv = new DataView(probe.memory.buffer, scratchPtr, 1288);
    var hash = dv.getBigUint64(720, true).toString();
    var sh = dv.getBigUint64(1280, true).toString();
    var tag = dec.decode(mem.subarray(scratchPtr + 0, scratchPtr + 16)).split('\0')[0];
    var role = dec.decode(mem.subarray(scratchPtr + 16, scratchPtr + 48)).split('\0')[0];
    var ptg = dec.decode(mem.subarray(scratchPtr + 400, scratchPtr + 416)).split('\0')[0];
    var prl = dec.decode(mem.subarray(scratchPtr + 416, scratchPtr + 448)).split('\0')[0];
    // node_id = C++ 按输入（BFS）序分配；pIdx 是 JS 侧同序父队列索引，
    // 二者一一对应（每个队列元素产出一条记录）。根/未知父回退 -1。
    var id = dv.getUint32(1276, true);
    var pid = (id > 0 && pIdx[id] !== undefined) ? pIdx[id] : -1;
    var el = queue[id];
    var rect = el && el.nodeType === 1 ? el.getBoundingClientRect() : null;
    var selector = el && el.nodeType === 1 ? cssPath(el) : '';
    out.push({t: text, h: hash, sh: sh, id: id, pid: pid, tag: tag, role: role, ptag: ptg, prole: prl,
      sel: selector, x: rect ? rect.left : 0, y: rect ? rect.top : 0,
      dx: rect ? rect.left + window.scrollX : 0, dy: rect ? rect.top + window.scrollY : 0,
      w: rect ? rect.width : 0, height: rect ? rect.height : 0});
  }
  return {ok: ok, kept: kept, truncated: probe.uws_probe_truncated() === 1,
          overflow: probe.uws_probe_overflow_count(), fallback: probe.uws_probe_needs_fallback() === 1,
          nodes: out};
})()`
	return strings.ReplaceAll(js, "B64", quoteJS(b64))
}

// probePreinstallExpr 生成文档级预实例化脚本（Page.addScriptToEvaluateOnNewDocument，
// TDD-OPT-01 探针常驻注入）：每个新文档解析前异步实例化 standalone WASM 探针并
// 缓存 window.__uwsProbe，使后续采集路径零实例化开销。b64 为 wasm 字节 base64。
// 实例化失败记录 __uwsProbeError（采集路径有兜底实例化，不阻塞）。
func probePreinstallExpr(b64 string) string {
	const js = `(function(){
  if (window.__uwsProbe) return;
  try {
    var bin = Uint8Array.from(atob(B64), function(c){ return c.charCodeAt(0); });
    WebAssembly.instantiate(bin).then(function(r){
      window.__uwsProbe = r.instance.exports;
      window.__uwsProbeReady = true;
    }, function(e){ window.__uwsProbeError = String(e); });
  } catch (e) { window.__uwsProbeError = String(e); }
})();`
	return strings.ReplaceAll(js, "B64", quoteJS(b64))
}

// probeCompensateExpr 构造 200ms 慢速遍历补偿表达式：读取溢出队列 BFS 前 200
// 个 node_id，按与采集一致的 BFS 序重遍历 DOM，收集被驱逐节点的语义文本。
func probeCompensateExpr() string {
	return `(async () => {
  var probe = window.__uwsProbe;
  if (!probe) return [];
  var mem = new Uint8Array(probe.memory.buffer);
  var scratchPtr = probe.uws_probe_scratch() >>> 0;
  var n = probe.uws_probe_overflow_peek(0, 200);
  if (n <= 0) return [];
  var ids = {};
  var view = new Uint32Array(probe.memory.buffer, scratchPtr, n);
  for (var i = 0; i < n; i++) ids[view[i]] = true;
  var norm = function(s){ return s == null ? '' : String(s).replace(/\s+/g,' ').trim(); };
  // §5.0.6 条款 3：溢出补偿遍历与主采集同口径，数据载体（style 模板 / JSON）不得进入文本源。
  var isDataPayload = function(v){
    if (typeof v !== 'string' || v === '') return false;
    if (/^<style/i.test(norm(v))) return true;
    var s = v.trim();
    if (s.charAt(0) !== '{' && s.charAt(0) !== '[') return false;
    try { JSON.parse(s); return true; } catch (e) { return false; }
  };
  var text = function(el){
    var raw = el.value || el.placeholder || el.getAttribute('aria-label') ||
              el.getAttribute('data-testid') || el.getAttribute('title') || el.textContent;
    if (typeof raw === 'string' && (raw.indexOf('<style') >= 0 || isDataPayload(raw))) return '';
    return norm(raw);
  };
  var out = [];
  var queue = [document.documentElement || document.body];
  var idx = 0;
  for (var qi = 0; qi < queue.length; qi++) {
    var el = queue[qi];
    if (el.nodeType === 1) {
      if (ids[idx]) { var t = text(el); if (t) out.push(t); }
      idx++;
      for (var ci = 0; ci < el.children.length; ci++) queue.push(el.children[ci]);
    }
  }
	  return out;
	})()`
}

// probeExportVlmExpr 导出探针 kept 节点为极简 AXTree（ExportToVLMFormat 的页内
// 运行时出口，v1.5.5 ExportPageTree RPC 数据源）。前置：collectProbeCandidates
// 已把 kept 节点写入 window.__uwsProbe 的 WASM 缓冲（同一 document 内持久）。
// 返回 `[node_id] tag(role) "text" [offscreen]` 多行文本；探针未实例化/导出失败
// 返回空串（调用方按 Unimplemented/Internal 处理）。
func probeExportVlmExpr() string {
	return `(() => {
	  var probe = window.__uwsProbe;
	  if (!probe) return '';
	  var mem = new Uint8Array(probe.memory.buffer);
	  var scratchPtr = probe.uws_probe_scratch() >>> 0;
	  var scratchSize = probe.uws_probe_scratch_size();
	  var n = probe.uws_probe_export_vlm(0, scratchSize);
	  if (n <= 0) return '';
	  return new TextDecoder().decode(mem.subarray(scratchPtr, scratchPtr + n));
	})()`
}

// probeRefMetadataExpr 导出 kept 交互节点的 href 与最近行上下文。
// 与采集同 BFS 序；短标签附上 row/listitem/group 的可见摘要，辅助模型区分重复控件。
func probeRefMetadataExpr() string {
	return `(() => {
	  var probe = window.__uwsProbe;
	  var out = {hrefs:{}, contexts:{}};
	  if (!probe) return out;
	  var mem = new Uint8Array(probe.memory.buffer);
	  var scratchPtr = probe.uws_probe_scratch() >>> 0;
	  var kept = probe.uws_probe_kept_count();
	  var ids = [];
	  for (var i = 0; i < kept; i++) {
	    if (probe.uws_probe_copy_node(i, 0, 1288) <= 0) break;
	    var dv = new DataView(probe.memory.buffer, scratchPtr, 1288);
	    ids.push(dv.getUint32(1276, true));
	  }
	  if (!ids.length) return out;
	  var want = {};
	  for (var j = 0; j < ids.length; j++) want[ids[j]] = true;
	  var norm = function(v){ return v == null ? '' : String(v).replace(/[\s\u001e\u001f]+/g,' ').trim(); };
	  var contextCache = new WeakMap();
	  var shortNodes = [];
	  var rowContext = function(el){
	    var row = el.closest('tr,li,[role="row"],[role="listitem"],[role="group"],.row,.item');
	    if (!row || row === el) return null;
	    if (contextCache.has(row)) return contextCache.get(row);
	    var text = norm(row.innerText || row.textContent);
	    if (text.length > 64) text = Array.from(text).slice(0, 64).join('') + '…';
	    var value = text ? {tag:row.tagName.toLowerCase(), text:text} : null;
	    contextCache.set(row, value);
	    return value;
	  };
	  var queue = [document.documentElement || document.body];
	  for (var qi = 0; qi < queue.length; qi++) {
	    var el = queue[qi];
	    if (el.nodeType !== 1) continue;
	    if (want[qi] && /^a$/i.test(el.tagName)) {
	      var href = el.getAttribute('href');
	      if (href != null) {
	        var resolved = el.href || href;
	        if (resolved && String(resolved).trim()) out.hrefs[qi] = String(resolved);
	      }
	    }
	    if (want[qi]) {
	      var label = norm(el.value || el.placeholder || el.getAttribute('aria-label') || el.textContent);
	      if (label.length <= 48) {
	        shortNodes.push({id:qi, el:el, label:label});
	      }
	    }
	    for (var ci = 0; ci < el.children.length; ci++) queue.push(el.children[ci]);
	  }
	  var labelCounts = new Map();
	  for (var si = 0; si < shortNodes.length; si++) {
	    var current = shortNodes[si];
	    labelCounts.set(current.label, (labelCounts.get(current.label) || 0) + 1);
	  }
	  var generic = /^(查看|修改|更改|编辑|设置|详情|更多|操作|管理|打开|继续|提交|确认|删除|保存|重置|启用|禁用|view|modify|edit|settings?|details?|more|manage|open|continue|submit|confirm|delete|save|reset|enable|disable)$/i;
	  for (var sj = 0; sj < shortNodes.length; sj++) {
	    var candidate = shortNodes[sj];
	    if (!generic.test(candidate.label) && (labelCounts.get(candidate.label) || 0) < 2) continue;
	    var context = rowContext(candidate.el);
	    if (context && context.text && context.text !== candidate.label) out.contexts[candidate.id] = context;
	  }
	  return out;
	})()`
}

// RefContext 是短标签节点所属最近行/列表项/分组的可见语义摘要。
type RefContext struct {
	Tag  string
	Text string
}

// annotateRefTreeContexts 给可交互 ref 追加有界元信息；首个引号文本保持不变，
// 保证协调器对 node_id 和节点文本的解析契约稳定。
func annotateRefTreeContexts(tree string, contexts map[uint32]RefContext) string {
	if tree == "" || len(contexts) == 0 {
		return tree
	}
	lines := strings.Split(tree, "\n")
	for i, line := range lines {
		id, ok := refRowID(line)
		if !ok {
			continue
		}
		context, ok := contexts[id]
		if !ok || strings.TrimSpace(context.Text) == "" {
			continue
		}
		text := strings.Join(strings.Fields(context.Text), " ")
		runes := []rune(text)
		if len(runes) > 64 {
			text = string(runes[:64]) + "…"
		}
		clean := strings.NewReplacer("\"", "\\\"", "\\", "\\\\", "\r", " ", "\n", " ").Replace(text)
		kind := strings.TrimSpace(context.Tag)
		lines[i] = line + ` context=` + kind + ` "` + clean + `"`
	}
	return strings.Join(lines, "\n")
}

// refRowID 解析 `[node_id] ...` 行首的确定性节点号（不存在返回 false）。
func refRowID(line string) (uint32, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "[")
	if !ok {
		return 0, false
	}
	end := strings.IndexByte(rest, ']')
	if end <= 0 {
		return 0, false
	}
	id, err := strconv.ParseUint(rest[:end], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(id), true
}

// annotateRefTreeHrefs 给导出文本中每个存在 href 的 [node_id] 行追加
// ` href="..."`，保持行内首个引号文本不变（协调器 parse_tree_nodes 不受影响）。
func annotateRefTreeHrefs(tree string, hrefs map[uint32]string) string {
	if tree == "" || len(hrefs) == 0 {
		return tree
	}
	lines := strings.Split(tree, "\n")
	for i, line := range lines {
		id, ok := refRowID(line)
		if !ok {
			continue
		}
		u, ok := hrefs[id]
		if !ok || u == "" {
			continue
		}
		u = strings.NewReplacer(`"`, "%22", "\r", "", "\n", "").Replace(u)
		lines[i] = line + ` href="` + u + `"`
	}
	return strings.Join(lines, "\n")
}
