// WASM 探针注入链路测试（USER_ACTION #14）：
//   - DecodeSerializedNode：与 pkg/probe_wasm/src/probe.h 固定宽度布局对齐的
//     二进制契约单测（黄金字节夹具）；
//   - 真实 Chrome 集成：探针注入采集（候选带 SimHash）与 2 万节点截断 +
//     200ms 慢速遍历补偿；无 Chrome / 无 wasm 产物时自动 skip（同既有
//     TestCDPSessionRealChrome 纪律）。
package browser

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── 二进制契约：DecodeSerializedNode ──────────────────────────────────

func TestDecodeSerializedNode(t *testing.T) {
	b := make([]byte, probeSerializedNodeSize)
	copy(b[probeTagOffset:], "BUTTON")
	copy(b[probeRoleOffset:], "button")
	copy(b[probeSemanticTextOffset:], "登录")
	copy(b[probeParentSemanticOffset:], "form")
	copy(b[probeZonalOffset:], "nav")
	copy(b[probeZonalOffset+probeZonalKeywordBytes:], "hero")
	copy(b[probeParentTagOffset:], "form")
	copy(b[probeInputTypeOffset:], "submit")
	copy(b[probeAttributesOffset:], `data-testid="go"`)
	binary.LittleEndian.PutUint64(b[probeHashOffset:probeHashOffset+8], 0xDEADBEEF)
	copy(b[probeCompositeKeyOffset:], "button|登录")
	copy(b[probeXPathOffset:], "/html/body/form/button")
	copy(b[probeClassNamesOffset:], "primary cta")
	copy(b[probeIDOffset:], "login-btn")
	b[probeFlagsOffset] = 0x07 // in_form | is_leaf | in_viewport
	b[probeDepthOffset] = 3
	b[probeTierOffset] = 1
	binary.LittleEndian.PutUint32(b[probeNodeIDOffset:probeNodeIDOffset+4], 42)
	binary.LittleEndian.PutUint64(b[probeStructHashOffset:probeStructHashOffset+8], 0x99CE76C794A892A9)

	n, err := DecodeSerializedNode(b)
	if err != nil {
		t.Fatalf("DecodeSerializedNode: %v", err)
	}
	if n.Tag != "BUTTON" || n.Role != "button" || n.SemanticText != "登录" {
		t.Fatalf("基础字段错: %+v", n)
	}
	if n.ParentSemanticText != "form" || n.ParentTag != "form" || n.InputType != "submit" {
		t.Fatalf("父/输入字段错: %+v", n)
	}
	if n.ZonalKeywords[0] != "nav" || n.ZonalKeywords[1] != "hero" {
		t.Fatalf("zonal keywords 错: %+v", n.ZonalKeywords)
	}
	if n.AstSemanticHash != 0xDEADBEEF {
		t.Fatalf("ast hash 错: %#x", n.AstSemanticHash)
	}
	if n.CompositeKey != "button|登录" || n.XPath != "/html/body/form/button" {
		t.Fatalf("复合键/xpath 错: %q %q", n.CompositeKey, n.XPath)
	}
	if n.ClassNames != "primary cta" || n.ID != "login-btn" {
		t.Fatalf("class/id 错: %q %q", n.ClassNames, n.ID)
	}
	if n.Flags != 0x07 || n.DomDepth != 3 || n.Tier != 1 || n.NodeID != 42 {
		t.Fatalf("标志/层级/节点号错: flags=%#x depth=%d tier=%d id=%d",
			n.Flags, n.DomDepth, n.Tier, n.NodeID)
	}
	if n.StructuralHash != 0x99CE76C794A892A9 {
		t.Fatalf("结构签名错: %#x", n.StructuralHash)
	}
}

func TestDecodeSerializedNodeShort(t *testing.T) {
	if _, err := DecodeSerializedNode(make([]byte, probeSerializedNodeSize-1)); err == nil {
		t.Fatal("短缓冲区应报错")
	}
}

// ── TDD-OPT-01：探针常驻预实例化 + 候选缓存 ───────────────────────────

func TestProbePreinstallExpr(t *testing.T) {
	expr := probePreinstallExpr("QUJD")
	for _, want := range []string{
		`atob("QUJD")`,
		"WebAssembly.instantiate",
		"window.__uwsProbe = r.instance.exports",
		"window.__uwsProbeReady = true",
		"__uwsProbeError",
	} {
		if !strings.Contains(expr, want) {
			t.Fatalf("预实例化脚本应包含 %q, got: %.200s...", want, expr)
		}
	}
	if strings.Contains(expr, "uws_probe_reset") {
		t.Fatal("预实例化脚本只负责驻留实例，不应触发采集")
	}
}

// TDD-PROBE-10（2026-08-31 Airwallex 实战）：采集器的 isInteractive 必须把
// 挂 onclick 的元素（如企业后台 <tr onclick> 表格行）视为可交互——否则行文本
// 不采集、视口内标记不置位，C++ 探针按 tier4+空文本丢弃，语义点击 no_match。
func TestProbeCollectExprInteractiveOnClick(t *testing.T) {
	expr := probeCollectExpr("QUJD")
	for _, want := range []string{
		"el.onclick != null",
		"el.getAttribute('onclick')",
		"el.children.length === 0",
		"getComputedStyle(el).cursor === 'pointer'",
	} {
		if !strings.Contains(expr, want) {
			t.Fatalf("采集器 isInteractive 应含 onclick 判定 %q", want)
		}
	}
}

func TestProbeCollectExprKeepsBulkChunksRecordAligned(t *testing.T) {
	expr := probeCollectExpr("QUJD")
	for _, want := range []string{
		"bytes.lastIndexOf(30, end - 1)",
		"end = recordEnd + 1",
		"record-aligned",
	} {
		if !strings.Contains(expr, want) {
			t.Fatalf("bulk collector must preserve node record boundaries; missing %q", want)
		}
	}
}

func TestCandidateCacheDecisions(t *testing.T) {
	s := &cdpSession{}
	now := time.Now()
	base := []PageCandidate{
		{Text: "登录"},
		{Text: "注册", Children: []PageCandidate{{Text: "子"}}},
	}
	seed := func() {
		s.candCache = base
		s.candCacheGen = 1
		s.candCacheAt = now
		s.candBaseMut = 3
		s.docGen = 1
		s.mutCount = 3
	}
	// 有效缓存命中，且返回深拷贝（修改副本不污染缓存）。
	seed()
	got, ok := s.cachedCandidatesLocked(now.Add(100 * time.Millisecond))
	if !ok || len(got) != 2 || got[1].Children[0].Text != "子" {
		t.Fatalf("有效缓存应命中: ok=%v got=%+v", ok, got)
	}
	got[0].Text = "改"
	if s.candCache[0].Text != "登录" {
		t.Fatal("缓存命中应返回深拷贝（修改副本不得污染缓存）")
	}
	// 导航后失效（docGen 变化）。
	seed()
	s.docGen = 2
	if _, ok := s.cachedCandidatesLocked(now.Add(time.Second)); ok {
		t.Fatal("docGen 变化后缓存应失效")
	}
	// TTL 到期后失效。
	seed()
	if _, ok := s.cachedCandidatesLocked(now.Add(candCacheTTL + time.Millisecond)); ok {
		t.Fatal("TTL 到期后缓存应失效")
	}
	// DOM 突变（采样计数变化）后失效。
	seed()
	s.mutCount = 4
	if _, ok := s.cachedCandidatesLocked(now.Add(time.Second)); ok {
		t.Fatal("mutCount 变化后缓存应失效")
	}
	// 空缓存不命中。
	s.candCache = nil
	if _, ok := s.cachedCandidatesLocked(now.Add(time.Second)); ok {
		t.Fatal("空缓存不应命中")
	}
}

func TestBuildProbeCandidatesTree(t *testing.T) {
	// 输入乱序（BFS 序 = node_id 升序；排序后兄弟保持 DOM 顺序）。
	nodes := []probeCandidate{
		{Text: "登录", Hash: "0x10", ID: 3, PID: 1},
		{Text: "body", ID: 1, PID: 0},
		{Text: "注册", ID: 4, PID: 1},
		{Text: "html", ID: 0, PID: -1},
		{Text: "用户名", ID: 2, PID: 1},
		{Text: "孤儿", ID: 7, PID: 99}, // 父被驱逐 → 提升为根
	}
	tree := buildProbeCandidates(nodes)
	if len(tree) != 2 || tree[0].Text != "html" || tree[1].Text != "孤儿" {
		t.Fatalf("根应按 BFS 序: %+v", tree)
	}
	html := tree[0]
	if len(html.Children) != 1 || html.Children[0].Text != "body" {
		t.Fatalf("html 子节点应为 body: %+v", html.Children)
	}
	body := html.Children[0]
	if len(body.Children) != 3 {
		t.Fatalf("body 应有 3 个子节点（BFS 序）: %+v", body.Children)
	}
	want := []string{"用户名", "登录", "注册"}
	for i, w := range want {
		if body.Children[i].Text != w {
			t.Fatalf("子节点[%d] = %q, 期望 %q", i, body.Children[i].Text, w)
		}
	}
	// 兄弟自身不携带子级副本（值语义序列化后 Children 一致）。
	if len(body.Children[1].Children) != 0 {
		t.Fatalf("登录不应有子节点: %+v", body.Children[1].Children)
	}
	if tree[1].SimHash != 0 {
		t.Fatalf("孤儿（无 ast hash）SimHash 应为 0: %#x", tree[1].SimHash)
	}
}

func TestBuildProbeCandidatesEmptyAndMissingParent(t *testing.T) {
	if tree := buildProbeCandidates(nil); tree != nil {
		t.Fatalf("空输入应返回 nil: %+v", tree)
	}
	// 仅根：pid=-1 不自我挂接。
	tree := buildProbeCandidates([]probeCandidate{{Text: "root", ID: 0, PID: 0}})
	if len(tree) != 1 || tree[0].Text != "root" || len(tree[0].Children) != 0 {
		t.Fatalf("根不应自我挂接: %+v", tree)
	}
	// 空文本父容器（真实 DOM 的 body/div）剪除后子节点上提为根：
	// html("",0) > body("",1) > 登录(2)/注册(3)；孤儿(7, pid=99) 独立根。
	tree = buildProbeCandidates([]probeCandidate{
		{Text: "", ID: 0, PID: -1},
		{Text: "", ID: 1, PID: 0},
		{Text: "登录", ID: 2, PID: 1},
		{Text: "注册", ID: 3, PID: 1},
		{Text: "孤儿", ID: 7, PID: 99},
	})
	if len(tree) != 3 || tree[0].Text != "登录" || tree[1].Text != "注册" || tree[2].Text != "孤儿" {
		t.Fatalf("空父节点应剪除、子节点上提: %+v", tree)
	}
	for _, c := range tree {
		if len(c.Children) != 0 {
			t.Fatalf("上提后不应残留子级: %+v", c)
		}
	}
}

// ── 真实 Chrome 集成（无 Chrome / 无 wasm 自动 skip） ─────────────────

func requireProbeWasm(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		filepath.Join("..", "..", "build", "probe_wasm_wasm", "probe_wasm.wasm"),
		filepath.Join("build", "probe_wasm_wasm", "probe_wasm.wasm"),
	} {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	t.Skip("probe wasm 未构建（make probe-wasm）；跳过真实探针集成")
	return ""
}

func newTestCDPSessionWithProbe(t *testing.T, cfg Config) *cdpSession {
	t.Helper()
	requireCDP(t)
	f := &CDPFactory{Headless: true, ProbeWasmPath: requireProbeWasm(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := f.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("create cdp session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess.(*cdpSession)
}

func TestCDPSessionWasmProbe(t *testing.T) {
	pageURL, _, _ := cdpTestPage(t)
	s := newTestCDPSessionWithProbe(t, Config{UserAgent: "UWS-Test-UA/1.0"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	cands := s.PageCandidates()
	stats := s.ProbeStats()
	if stats.Kept <= 0 {
		t.Fatalf("探针应保留节点, stats=%+v", stats)
	}
	if stats.Truncated {
		t.Fatalf("小页面不应截断, stats=%+v", stats)
	}
	texts := map[string]bool{}
	for _, c := range cands {
		texts[c.Text] = true
	}
	for _, want := range []string{"登录", "注册", "用户名"} {
		if !texts[want] {
			t.Fatalf("探针候选缺失 %q: %+v", want, cands)
		}
	}
}

func cdpNestedPage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <form role="form" aria-label="登录表单">
    <button id="login">登录</button>
    <button id="reg">注册</button>
  </form>
  <a href="/next" id="ext">外部链接</a>
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestCDPSessionWasmProbeChildren 验证真实 DOM 候选树（MATCH-06 真实侧输入
// 通道）：嵌套交互结构（form[aria-label] > 登录/注册 按钮）按 BFS 父子链路
// 重建为 PageCandidate.Children，而非全量平铺。
func TestCDPSessionWasmProbeChildren(t *testing.T) {
	pageURL := cdpNestedPage(t)
	s := newTestCDPSessionWithProbe(t, Config{UserAgent: "UWS-Test-UA/1.0"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	cands := s.PageCandidates()
	texts := map[string]bool{}
	var collect func([]PageCandidate)
	collect = func(list []PageCandidate) {
		for _, c := range list {
			texts[c.Text] = true
			collect(c.Children)
		}
	}
	collect(cands)
	for _, want := range []string{"登录表单", "登录", "注册", "外部链接"} {
		if !texts[want] {
			t.Fatalf("探针候选缺失 %q: %+v", want, cands)
		}
	}
	// form 容器携带子按钮；子节点顺序 = DOM 顺序。
	var form *PageCandidate
	for i := range cands {
		if cands[i].Text == "登录表单" {
			form = &cands[i]
			break
		}
	}
	if form == nil {
		t.Fatalf("未找到 form 容器候选: %+v", cands)
	}
	if len(form.Children) != 2 || form.Children[0].Text != "登录" || form.Children[1].Text != "注册" {
		t.Fatalf("form 应携带子节点 登录/注册: %+v", form.Children)
	}
}

// TestCDPSessionProbeResidentAndCached 验证 TDD-OPT-01 真实侧：
//   - 探针常驻注入：Page.addScriptToEvaluateOnNewDocument 预实例化后，
//     导航完成即 window.__uwsProbe 就绪（采集零实例化开销）；
//   - 多信号批量抽取：紧随首次采集的连续抽取命中缓存（结果一致、毫秒级、
//     candHits 计数）；DOM 突变被采样后缓存自动失效、重新采集包含新节点。
func TestCDPSessionProbeResidentAndCached(t *testing.T) {
	pageURL := cdpNestedPage(t)
	s := newTestCDPSessionWithProbe(t, Config{UserAgent: "UWS-Test-UA/1.0"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// 1) 探针常驻：预脚本应在文档解析前实例化（无需显式采集即就绪）。
	ready, err := s.evalBool(ctx,
		`typeof window.__uwsProbe === 'object' && typeof window.__uwsProbe.uws_probe_reset === 'function'`)
	if err != nil {
		t.Fatalf("检查常驻探针: %v", err)
	}
	if !ready {
		t.Fatal("探针应由 addScriptToEvaluateOnNewDocument 常驻预实例化（window.__uwsProbe 未就绪）")
	}

	// 2) 首次采集（缓存未命中）：真实 BFS + bulk 注入，候选非空。
	cands1 := s.PageCandidates()
	if len(cands1) == 0 {
		t.Fatal("首次采集不应为空")
	}

	// 3) 紧随其后的多信号抽取命中缓存：结果一致、毫秒级、零重新采集。
	start := time.Now()
	cands2 := s.PageCandidates()
	elapsed := time.Since(start)
	if elapsed > 50*time.Millisecond {
		t.Fatalf("缓存命中抽取应 <50ms（CI 安全线；本地实测远低），实测 %v", elapsed)
	}
	s.mu.Lock()
	hits := s.candHits
	s.mu.Unlock()
	if hits != 1 {
		t.Fatalf("应有 1 次缓存命中, got %d", hits)
	}
	if fmt.Sprint(cands1) != fmt.Sprint(cands2) {
		t.Fatalf("缓存命中结果应与首次采集一致:\n首次 %v\n命中 %v", cands1, cands2)
	}

	// 4) DOM 突变 → 采样 → 缓存自动失效 → 重新采集包含新节点。
	if _, err := s.evalResult(ctx,
		`document.body.appendChild(Object.assign(document.createElement('button'), {textContent: '新按钮'})); 'ok'`); err != nil {
		t.Fatalf("注入 DOM 突变: %v", err)
	}
	s.sampleMutationsOnce()
	cands3 := s.PageCandidates()
	found := false
	var walk func([]PageCandidate)
	walk = func(list []PageCandidate) {
		for _, c := range list {
			if c.Text == "新按钮" {
				found = true
			}
			walk(c.Children)
		}
	}
	walk(cands3)
	if !found {
		t.Fatalf("突变后重新采集应包含新按钮: %+v", cands3)
	}
}

func cdpTruncationPage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
<button>登录</button>
<script>
  const b = document.body;
  for (let i = 0; i < 20000; i++) {
    const btn = document.createElement('button');
    btn.textContent = 'btn-' + i;
    b.appendChild(btn);
  }
</script>
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCDPSessionWasmTruncation(t *testing.T) {
	pageURL := cdpTruncationPage(t)
	s := newTestCDPSessionWithProbe(t, Config{UserAgent: "UWS-Test-UA/1.0"})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	cands := s.PageCandidates()
	stats := s.ProbeStats()
	if !stats.Truncated {
		t.Fatalf("2 万节点应触发截断, stats=%+v", stats)
	}
	if stats.Overflow <= 0 {
		t.Fatalf("截断后应有溢出队列, stats=%+v", stats)
	}
	if len(cands) > stats.Kept+200 {
		t.Fatalf("补偿最多 200 节点: %d > %d+200", len(cands), stats.Kept)
	}
	found := false
	for _, c := range cands {
		if c.Text == "登录" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("截断后候选应含高优先级节点 登录（%d 候选）", len(cands))
	}
}

// BenchmarkCandidateCacheHit 度量多信号抽取的缓存命中路径（TDD-OPT-01 目标
// <5ms）：真实探针采集一次后，同一文档内后续信号抽取仅做 Go 侧深拷贝复用，
// 无 CDP 往返。用 ~200 节点候选树模拟中型页面。
func BenchmarkCandidateCacheHit(b *testing.B) {
	var mkTree func(depth, fanout int) []PageCandidate
	mkTree = func(depth, fanout int) []PageCandidate {
		var out []PageCandidate
		for i := 0; i < fanout; i++ {
			n := PageCandidate{Text: fmt.Sprintf("节点-%d-%d", depth, i)}
			if depth > 0 {
				n.Children = mkTree(depth-1, fanout)
			}
			out = append(out, n)
		}
		return out
	}
	s := &cdpSession{}
	s.candCache = mkTree(2, 5) // 1 + 5 + 25 ≈ 31 候选（真实交互页量级）
	s.candCacheGen = 1
	s.candCacheAt = time.Now()
	s.candBaseMut = 0
	s.docGen = 1
	s.mutCount = 0

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, ok := s.cachedCandidatesLocked(time.Now())
		if !ok || len(got) == 0 {
			b.Fatal("缓存命中应成功")
		}
	}
}
