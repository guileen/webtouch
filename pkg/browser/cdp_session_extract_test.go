package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 方案 1 真实侧（2026-08-25）：页面 fetch 本地 SSE 聊天端点（text/event-stream），
// CDP Network 层捕获必须拿到干净 delta 文本（Content 逐字一致 + Done）。
// 与 DeepSeek /api/v0/chat/completion 同构；无 Chrome/沙箱阻断时自动 skip。
func TestCDPSessionSSECaptureRealChrome(t *testing.T) {
	requireCDP(t)
	var mu sync.Mutex
	sseHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<!DOCTYPE html><html><body><script>
			  fetch('/api/v0/chat/completion', {method: 'POST'}).then(r => r.text());
			</script></body></html>`)
		case "/api/v0/chat/completion":
			mu.Lock()
			sseHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			fl, _ := w.(http.Flusher)
			writeSSE := func(s string) {
				_, _ = w.Write([]byte(s))
				if fl != nil {
					fl.Flush()
				}
			}
			writeSSE("data: {\"delta\":{\"content\":\"你\"}}\n\n")
			time.Sleep(30 * time.Millisecond)
			writeSSE("data: {\"delta\":{\"content\":\"好\"}}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL+"/"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		snap := s.ChatStreamSnapshot()
		if snap.Epoch > 0 && snap.Done {
			if snap.Content != "你好" {
				t.Fatalf("SSE 内容不符: %q (reasoning=%q)", snap.Content, snap.Reasoning)
			}
			mu.Lock()
			got := sseHits
			mu.Unlock()
			if got != 1 {
				t.Fatalf("SSE 端点应命中 1 次，got %d", got)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	snap := s.ChatStreamSnapshot()
	t.Fatalf("SSE 捕获超时: %+v", snap)
}

// TestResolveNodeByIdAndExportPageTree 验证 v1.5.5 混合指挥两个引擎原语：
// ExportPageTree 导出极简 AXTree（含 BFS node_id），resolveNodeById 按同一
// node_id 回查元素（BFS 元素节点序，与探针 collector 一致）。真实 Chrome +
// WASM 探针；无 Chrome / 未构建探针时跳过。
func TestResolveNodeByIdAndExportPageTree(t *testing.T) {
	requireCDP(t)
	pageURL, _, _ := cdpTestPage(t)
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	tree, err := s.ExportPageTree(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "wasm 未找到") {
			t.Skipf("probe wasm 未构建，跳过: %v", err)
		}
		t.Fatalf("export page tree: %v", err)
	}
	if tree == "" || !strings.Contains(tree, "登录") {
		t.Fatalf("极简 AXTree 应含登录按钮: %q", tree)
	}

	id := nodeIDForText(t, tree, "登录")
	target, err := resolveNodeById(ctx, s, int32(id))
	if err != nil {
		t.Fatalf("resolveNodeById(%d): %v", id, err)
	}
	if !target.Found || target.Tag != "button" || !strings.Contains(target.Text, "登录") {
		t.Fatalf("resolve 结果不符（id=%d）: %+v", id, target)
	}
}

// nodeIDForText 从极简 AXTree 中解析含指定文本行的 BFS node_id。
func nodeIDForText(t *testing.T, tree, text string) int {
	t.Helper()
	for _, line := range strings.Split(tree, "\n") {
		if !strings.Contains(line, text) {
			continue
		}
		open := strings.Index(line, "[")
		close := strings.Index(line, "]")
		if open < 0 || close <= open {
			continue
		}
		n, err := strconv.Atoi(line[open+1 : close])
		if err != nil {
			t.Fatalf("解析 node_id: %q", line)
		}
		return n
	}
	t.Fatalf("tree 中未找到含 %q 的行: %q", text, tree)
	return 0
}

// 语义抽取门禁（2026-09-23 live dogfood 假阳性根因）：
// `extract:Example Domains` 曾在 IANA 页抽到导航栏链接「Domains」并报 SUCCEEDED
// —— 内容节点（h1）根本不在 click 候选池里，碎片匹配（元素文本 ⊂ 目标）成了赢家。
// 修复后：extract 走严格模式（内容节点入池 + 只认目标文本本身/包含目标），
// click 的宽泛语义（含 60 分档）保持原样。
func TestCDPSessionExtractPrefersContentExactMatch(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <nav><ul>
    <li><a href="/domains">Domains</a></li>
    <li><a href="/protocols">Protocols</a></li>
  </ul></nav>
  <main>
    <h1>Example Domains</h1>
    <p>IANA-managed example domains。</p>
  </main>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	ok, observed := s.ExecuteActionWithSelector(ctx, "extract", "", "Example Domains", "")
	if !ok {
		t.Fatalf("extract 应命中 h1，observed=%q", observed)
	}
	if observed != "Example Domains" {
		t.Fatalf("extract 必须返回目标本身，不得返回碎片节点文本，got %q", observed)
	}
	// 命中元素证据：extract 也要回显真正的命中节点（tag/text）。
	if h, okh := s.LastHitElement(); !okh || h.Tag != "h1" || !strings.Contains(h.Text, "Example Domains") {
		t.Fatalf("extract 命中证据应为 h1，got %+v ok=%v", h, okh)
	}
	// click 侧候选池仍是交互元素（宽泛语义未收紧）：同一页同一目标仍 found。
	loose, err := findElementOpt(ctx, s, "Example Domains", "", false)
	if err != nil || !loose.Found {
		t.Fatalf("click 宽泛语义不应回归：found=%v err=%v", loose.Found, err)
	}
}

// 语义抽取门禁的 fail-closed 面：页面上只有「Example Domains」的碎片
// （导航链接「Domains」）时，extract 必须诚实 no_match，不得再用碎片冒充成功。
func TestCDPSessionExtractRejectsFragmentOnlyPage(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <nav><ul>
    <li><a href="/domains">Domains</a></li>
    <li><a href="/protocols">Protocols</a></li>
  </ul></nav>
  <p>Nothing about that phrase here。</p>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	ok, observed := s.ExecuteActionWithSelector(ctx, "extract", "", "Example Domains", "")
	if ok {
		t.Fatalf("碎片命中不得报成功，observed=%q", observed)
	}
	if !strings.Contains(observed, "no_match") {
		t.Fatalf("失败原因应为 no_match，got %q", observed)
	}
}

// §5.0.2（2026-09-24）语义标签归一化：内部标签（协调器 DSL 的 `search_box`）
// 在真实页面里不存在对应文案，必须**先展开为页面候选文案集合**再比对。
// 旧行为直接把 `search_box` 当字面量 ⇒ 真实站点 12 站 0/12（案卷 §4/§6）。
// 本测试用两个真实站点形态（英文 placeholder / 中文 aria-label）+ 干扰项钉住。
func TestCDPSessionSearchBoxAliasExpansion(t *testing.T) {
	requireCDP(t)
	cases := []struct {
		name string
		html string
		want string // 期望回显的命中候选
	}{
		{
			name: "english_placeholder",
			html: `<nav><a href="/signin">Sign in</a></nav>
  <form action="/s"><input id="q" name="q" placeholder="Search Amazon"></form>`,
			want: "search", // 「Search Amazon」包含候选 search（大小写不敏感）
		},
		{
			name: "chinese_aria_label",
			html: `<header><a href="/login">登录</a></header>
  <form action="/s"><input id="kw" aria-label="搜索商品"></form>`,
			want: "搜索",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  `+tc.html+`
</body></html>`)
			}))
			defer srv.Close()
			s := newTestCDPSession(t, Config{})
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			if err := s.Navigate(ctx, srv.URL); err != nil {
				t.Fatalf("navigate: %v", err)
			}
			got, err := findElementOpt(ctx, s, "search_box", "", false)
			if err != nil {
				t.Fatalf("findElementOpt(search_box): %v", err)
			}
			if !got.Found {
				t.Fatal("search_box 经别名归一化后必须命中搜索框（旧行为在此必红）")
			}
			if got.Tag != "input" {
				t.Fatalf("应命中 input 搜索框，实际 tag=%q text=%q", got.Tag, got.Text)
			}
			if !strings.EqualFold(got.Matched, tc.want) {
				t.Fatalf("命中候选应回显 %q（可观测性条款 6），got %q", tc.want, got.Matched)
			}
		})
	}
}

// §5.0.2 条款 5（fail-closed）：页面只有登录/注册文案时，别名展开不得让
// `search_box` 误命中（「找不到」优于「找错还报 SUCCEEDED」）。
func TestCDPSessionSearchBoxAliasNoFalseHit(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <nav><a href="/signin">Sign in</a><a href="/signup">注册</a></nav>
  <h1>欢迎回来</h1>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	got, err := findElementOpt(ctx, s, "search_box", "", false)
	if err != nil {
		t.Fatalf("findElementOpt(search_box): %v", err)
	}
	if got.Found {
		t.Fatalf("无搜索框的页面不得误命中，got tag=%q text=%q matched=%q", got.Tag, got.Text, got.Matched)
	}
}
