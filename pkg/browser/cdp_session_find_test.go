package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCDPSessionFindElementSemanticSources 回归（2026-08-20 续24）：
//   - findElementOpt 跨全部语义源评分：<span data-testid="author_name">作者甲</span>
//     可按 data-testid 语义精确定位（此前 innerText 优先导致永远匹配不到）；
//   - 无任何语义关联的查询必须 found=false（此前零分元素被误选为第一个元素，
//     导致 extract 语义缺失时误返回正文节点）。
func TestCDPSessionFindElementSemanticSources(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <h1>测试博客</h1>
  <article data-testid="main_article">正文：欢迎访问。<span data-testid="author_name">作者甲</span></article>
  <aside data-testid="ad_banner">广告：限时特惠</aside>
  <a href="/next" data-testid="next_link">下一页</a>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	cases := []struct {
		sem   string
		tag   string
		found bool
	}{
		{"author_name", "span", true}, // data-testid 语义 + 非空 innerText
		{"ad_banner", "aside", true},
		{"main_article", "article", true},
		{"下一页", "a", true}, // 文本语义
		{"不存在的语义", "", false},
	}
	for _, tc := range cases {
		got, err := findElementOpt(ctx, s, tc.sem, "", false)
		if err != nil {
			t.Fatalf("findElementOpt(%q): %v", tc.sem, err)
		}
		if got.Found != tc.found {
			t.Fatalf("findElementOpt(%q) found=%v, 期望 %v", tc.sem, got.Found, tc.found)
		}
		if got.Found && got.Tag != tc.tag {
			t.Fatalf("findElementOpt(%q) 命中 tag=%q, 期望 %q", tc.sem, got.Tag, tc.tag)
		}
	}
}

// TestCDPSessionFindElementLabelToControl（2026-08-31，Airwallex 表单实战）：
// label 文本命中但关联输入框无 placeholder/aria-label 时，findElementOpt 必须
// 沿 label→control（htmlFor/control 属性）下钻到输入框，而不是停在 label 上
// （否则 input 语义失败后交给 VLM 坐标，多输入框表单 VLM 会报错框）。
func TestCDPSessionFindElementLabelToControl(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <form>
    <label for="pn">产品名称</label>
    <input id="pn" name="productName" value="">
    <label for="desc">描述 (可选)</label>
    <textarea id="desc" name="description"></textarea>
  </form>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	got, err := findElementOpt(ctx, s, "描述", "", false)
	if err != nil {
		t.Fatalf("findElementOpt(描述): %v", err)
	}
	if !got.Found {
		t.Fatal("label 语义命中应下钻到关联 textarea")
	}
	if got.Tag != "textarea" {
		t.Fatalf("下钻目标 tag=%q, 期望 textarea", got.Tag)
	}
	// 点击/输入坐标应落在 textarea 中心而非 label。
	if got.X <= 0 || got.Y <= 0 {
		t.Fatalf("textarea 坐标异常: %+v", got)
	}
}

// R-A（2026-08-25）：知识库规则选择器贯通物理层——CDP 会话按 querySelectorAll
// 精确定位（纯 SVG 无文本按钮也能命中），选择器命中但无语义分时回退第一个可见元素，
// 无效选择器必须报错而非静默。
func TestCDPSessionFindElementBySelector(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <textarea id="chat_input" placeholder="给 DeepSeek 发送消息"></textarea>
  <div role="button" class="ds-button ds-button--primary ds-button--circle"><svg><path d="M8 1"/></svg></div>
  <div class="ds-markdown"><p>这是一段回答内容</p></div>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// 输入框：选择器命中（placeholder 语义与 want=chat_input 无交集，走回退）。
	got, err := findElementOpt(ctx, s, "chat_input", "textarea[placeholder*='发送消息']", false)
	if err != nil {
		t.Fatalf("findElementOpt(chat_input): %v", err)
	}
	if !got.Found || got.Tag != "textarea" {
		t.Fatalf("选择器应定位到 textarea，got %+v", got)
	}

	// 发送按钮：纯 SVG 无文本/aria，只能靠选择器。
	got2, err := findElementOpt(ctx, s, "send_button", "div[role='button'].ds-button--primary", false)
	if err != nil {
		t.Fatalf("findElementOpt(send_button): %v", err)
	}
	if !got2.Found || got2.Tag != "div" {
		t.Fatalf("选择器应定位到发送按钮 div，got %+v", got2)
	}

	// 回答区 extract：选择器定位后提取文本。
	ok, observed := s.ExecuteActionWithSelector(ctx, "extract", "", "chat_answer", ".ds-markdown")
	if !ok {
		t.Fatalf("ExecuteActionWithSelector(extract) 应成功，observed=%q", observed)
	}
	if !strings.Contains(observed, "这是一段回答内容") {
		t.Fatalf("extract 应返回回答文本，got %q", observed)
	}

	// 无效选择器：必须报错（不静默返回 found=false；Chrome 对部分畸形选择器
	// 宽容返回空集，此处用实测抛错的 '###'）。
	if _, err := findElementOpt(ctx, s, "x", "###", false); err == nil {
		t.Fatal("无效选择器应返回错误")
	}
}

// 多轮对话修复（2026-08-25）：extract 带选择器且无语义分时，回退必须取
// 「最后一个」可见命中（回答块流式追加语义），而不是第一个（多轮必取旧回答）。
func TestCDPSessionExtractPrefersLastMatch(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <div class="ds-markdown"><p>第一轮回答</p></div>
  <div class="ds-markdown"><p>第二轮回答</p></div>
  <div class="ds-markdown"><p>第三轮回答</p></div>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	ok, observed := s.ExecuteActionWithSelector(ctx, "extract", "", "chat_answer", ".ds-markdown")
	if !ok {
		t.Fatalf("extract 应成功，observed=%q", observed)
	}
	if !strings.Contains(observed, "第三轮回答") {
		t.Fatalf("extract 应取最后一个回答块，got %q", observed)
	}
	if strings.Contains(observed, "第一轮回答") {
		t.Fatalf("extract 不得取第一个回答块，got %q", observed)
	}
}

// text= 选择器前缀（2026-08-25）：按精确文本定位「最小可见」元素，
// 用于 DeepSeek「开启新对话」这类纯 div（无 role/button/onclick 属性）。
func TestCDPSessionFindElementByTextSelector(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <div style="padding:20px"><div><span>开启新对话</span></div></div>
  <button>其他按钮</button>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	got, err := findElementOpt(ctx, s, "new_chat_button", "text=开启新对话", false)
	if err != nil {
		t.Fatalf("findElementOpt(text=): %v", err)
	}
	if !got.Found || got.Tag != "span" {
		t.Fatalf("text= 应定位到最小可见 span，got %+v", got)
	}
}

// 前置/批次 1（2026-08-27）：纯语义定位（无选择器）命中文本后不得抛
// ReferenceError: r is not defined——语义评分分支漏声明几何变量 r，导致
// 「More ›」这类有语义分元素永远 eval_error（旧代码再把它 mask 成
// target_not_found，黑盒实验误判为「会话路由 Bug」；诊断通道实证根因）。
func TestCDPSessionFindElementSemanticMatchNoScopeBug(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <button onclick="this.dataset.clicked='1'">More ›</button>
  <div>Banner</div>
  <a href="#section">Top Trending</a>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	got, err := findElementOpt(ctx, s, "More", "", false)
	if err != nil {
		t.Fatalf("findElementOpt(More) 不得抛异常: %v", err)
	}
	if !got.Found || got.Tag != "button" {
		t.Fatalf("应命中 More 按钮，got %+v", got)
	}
	if !strings.Contains(got.Text, "More ›") {
		t.Fatalf("文本应为 More ›，got %q", got.Text)
	}
	// 同一 JS 内其他有语义分元素也必须不抛（回归：分支内 r 作用域）。
	got2, err := findElementOpt(ctx, s, "Top Trending", "", false)
	if err != nil {
		t.Fatalf("findElementOpt(Top Trending) 不得抛异常: %v", err)
	}
	if !got2.Found {
		t.Fatalf("应命中 Top Trending 链接，got %+v", got2)
	}
}

// 2026-08-27：drama-h5「第 4 集」点中 chapter-grid 容器假阳性根因回归。
// 章节网格容器（div[data-testid=chapter-grid]）与精确按钮（button[data-testid=chapter-item]）
// 的 innerText 都含「第 4 集」，此前按 DOM 序平分取先会选中容器；修复后必须优先
// 面积更小的叶子（button）——命中 tag=button，而非 tag=div（容器）。
func TestCDPSessionFindElementPrefersLeafOverContainer(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <div data-testid="chapter-grid" style="display:flex;flex-direction:column;gap:4px;padding:8px">
    <button data-testid="chapter-item" style="height:60px">1 第 1 集 107 秒 免费</button>
    <button data-testid="chapter-item" style="height:60px">2 第 2 集 124 秒 免费</button>
    <button data-testid="chapter-item" style="height:60px">3 第 3 集 96 秒 免费</button>
    <button data-testid="chapter-item" style="height:60px">4 第 4 集 113 秒 150金币</button>
    <button data-testid="chapter-item" style="height:60px">5 第 5 集 130 秒 150金币</button>
  </div>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	got, err := findElementOpt(ctx, s, "第 4 集", "", false)
	if err != nil {
		t.Fatalf("findElementOpt: %v", err)
	}
	if !got.Found {
		t.Fatal("应命中第 4 集")
	}
	if got.Tag != "button" {
		t.Fatalf("应命中精确 button（叶子），实际命中 tag=%q（容器）", got.Tag)
	}
	if !strings.Contains(got.Text, "第 4 集") {
		t.Fatalf("命中文本应为第 4 集，got %q", got.Text)
	}
}
