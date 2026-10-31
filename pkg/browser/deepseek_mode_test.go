package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uws/pkg/browser/modelbridge"
)

func TestPickDeepSeekMode(t *testing.T) {
	options := []modelbridge.ModeOption{
		{Text: "快速模式", Checked: true},
		{Text: "专家模式", Checked: false},
		{Text: "识图模式", Checked: false},
	}
	cases := []struct {
		want string
		idx  int
	}{
		{"快速模式", 0},
		{"专家模式", 1},
		{"识图模式", 2},
		{"专家", 1}, // 前缀包含退化
		{"不存在的模式", -1},
		{"", -1},
	}
	for _, c := range cases {
		if got := modelbridge.PickDeepSeekMode(options, c.want); got != c.idx {
			t.Fatalf("modelbridge.PickDeepSeekMode(%q) = %d, want %d", c.want, got, c.idx)
		}
	}
}

func TestDeepSeekModeNameEqual(t *testing.T) {
	cases := []struct {
		got  string
		want string
		eq   bool
	}{
		{"快速模式", "快速模式", true},
		{" 快速模式 ", "快速模式", true},
		{"专家模式", "专家", true}, // 前缀互含退化
		{"识图模式", "快速模式", false},
		{"", "快速模式", false},
		{"专家模式", "", false},
	}
	for _, c := range cases {
		if got := modelbridge.ModeNameEqual(c.got, c.want); got != c.eq {
			t.Fatalf("modelbridge.ModeNameEqual(%q, %q) = %v, want %v", c.got, c.want, got, c.eq)
		}
	}
}

// deepseekModePage 返回一个模拟 DeepSeek 新会话落地页的本地测试页：
// div[role="radio"] 三模式 + div.ds-toggle-button 深度思考/智能搜索。
func deepseekModePage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <div role="radiogroup">
    <div role="radio" aria-checked="true" onclick="pickMode(this,'快速模式')">快速模式</div>
    <div role="radio" aria-checked="false" onclick="pickMode(this,'专家模式')">专家模式</div>
    <div role="radio" aria-checked="false" onclick="pickMode(this,'识图模式')">识图模式</div>
  </div>
  <div class="ds-toggle-button ds-toggle-button--m" onclick="this.classList.toggle('ds-toggle-button--selected')">深度思考</div>
  <div class="ds-toggle-button ds-toggle-button--m ds-toggle-button--selected" onclick="this.classList.toggle('ds-toggle-button--selected')">智能搜索</div>
  <script>
    window.__mode = '快速模式';
    function pickMode(el, name) {
      document.querySelectorAll('div[role="radio"]').forEach(r => r.setAttribute('aria-checked', 'false'));
      el.setAttribute('aria-checked', 'true');
      window.__mode = name;
    }
  </script>
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestDeepSeekModeToggleRealChrome 用真实 Chrome 验证模式切换与开关切换
// 物理动作（无 Chrome/沙箱阻断时自动 skip）。
func TestDeepSeekModeToggleRealChrome(t *testing.T) {
	pageURL := deepseekModePage(t)
	s := newTestCDPSession(t, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if err := s.switchDeepSeekMode(ctx, nil, "专家模式"); err != nil {
		t.Fatalf("switchDeepSeekMode(专家模式): %v", err)
	}
	mode, _ := s.evalResult(ctx, `(window.__mode || '')`)
	if mode != "专家模式" {
		t.Fatalf("mode = %q, want 专家模式", mode)
	}

	if err := s.toggleDeepSeekSwitch(ctx, nil, "深度思考", true); err != nil {
		t.Fatalf("toggle 深度思考 on: %v", err)
	}
	sel, _ := s.evalBool(ctx, `(() => {
	  const el = Array.from(document.querySelectorAll('div.ds-toggle-button'))
	    .find(e => (e.innerText||'').trim() === '深度思考');
	  return el ? el.classList.contains('ds-toggle-button--selected') : false;
	})()`)
	if !sel {
		t.Fatal("深度思考 未切换为选中")
	}
}
