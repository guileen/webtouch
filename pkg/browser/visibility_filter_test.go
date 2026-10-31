package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// §5.0.5 几何有效性硬剪枝：
// 页面前置隐藏 textarea（如 Google 首页的无渲染 offscreen textarea），
// 即使同样匹配关键词且是 textarea，必须在打分前被物理可见性硬剪枝剔除，
// 确保输入值落入视口内真实可见的搜索框。
func TestCDPSessionHiddenTextareaPrunedByPhysicalVisibility(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <!-- 离屏/无渲染隐藏 textarea（模拟 Google [28] textarea） -->
  <textarea id="hidden-ta" style="display:none;" title="搜索" placeholder="搜索"></textarea>
  <div style="margin-top: 50px;">
    <!-- 真实居中可见搜索框（模拟 Google [365] textarea(combobox)） -->
    <textarea id="real-search" title="搜索" placeholder="搜索" style="width:400px;height:40px;"></textarea>
  </div>
</body></html>`)
	}))
	defer srv.Close()

	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	ok, observed := s.ExecuteAction(ctx, "input", "webtouch-vis-pass", "search_box")
	if !ok {
		t.Fatalf("可见搜索框应战胜隐藏textarea，observed=%q", observed)
	}

	val, err := s.evalResult(ctx, `(document.getElementById('real-search') || {}).value || ''`)
	if err != nil {
		t.Fatalf("eval real-search.value: %v", err)
	}
	if val != "webtouch-vis-pass" {
		t.Fatalf("值必须落入真实可见输入框，got %q", val)
	}

	hiddenVal, _ := s.evalResult(ctx, `(document.getElementById('hidden-ta') || {}).value || ''`)
	if hiddenVal != "" {
		t.Fatalf("隐藏 textarea 不应接收到任何输入，got %q", hiddenVal)
	}
}

// §5.0.7 Container Escalation（叶子装饰节点向承载容器反向穿透）：
// 模拟 Wikipedia 场景——放大镜图标 <i title="Search"> 与搜索框 <input id="searchInput"> 在同一容器中。
// 匹配 "Search" 命中 <i> 时，自动纠偏上浮至宿主输入框并直接落盘，消除 3 次门禁重试与 VLM 救场。
func TestCDPSessionContainerEscalationForLeafIcon(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <form id="searchform" style="display:flex;align-items:center;">
    <i class="search-icon" title="Search" style="cursor:pointer;width:20px;height:20px;display:inline-block;">Search</i>
    <input id="searchInput" name="search" style="width:300px;height:30px;">
  </form>
</body></html>`)
	}))
	defer srv.Close()

	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	ok, observed := s.ExecuteAction(ctx, "input", "webtouch-wiki-pass", "search_box")
	if !ok {
		t.Fatalf("Container Escalation 应自动穿透命中宿主输入框，observed=%q", observed)
	}

	val, err := s.evalResult(ctx, `(document.getElementById('searchInput') || {}).value || ''`)
	if err != nil {
		t.Fatalf("eval searchInput.value: %v", err)
	}
	if val != "webtouch-wiki-pass" {
		t.Fatalf("值必须成功落入宿主输入框，got %q", val)
	}
}
