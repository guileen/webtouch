package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uws/pkg/browser/modelbridge"
)

// geminiPickerPage 返回模拟 Gemini 模型选择器的本地测试页。
//
// 2026-09-26 按真实页 DOM 重做夹具（旧夹具把菜单写成 data-testid，与代码同错，
// 于是永远放行、掩盖了线上差异）。与线上对齐的三点：
//   - 菜单容器属性是 data-test-id="gem-mode-menu"（带连字符）；
//   - 菜单**懒渲染**：初始 DOM 只有空的 gem-popover 占位，点开后才插入 gem-menu
//     与 gem-menu-item（未打开时 querySelectorAll('gem-menu-item') 为空）；
//   - 菜单项是「标题 + 描述」两行（线上 3.5 Flash-Lite / 3.8 Flash / 3.1 Pro /
//     Extended thinking），第一项 Flash-Lite 与目标 Flash 构成家族冲突，
//     当前模型 3.8 Flash 带 selected。
//
// shape=narrow 时选择器按钮为 <gem-button>（Gemini 窄屏 responsive UI 实测形态，
// 2026-08-26；桌面宽屏才是原生 <button>）；shape=modern 用当前 aria-label 按钮；
// shape=scoped 额外放一个常驻可见的无关 gem-menu（线上同站还有别的下拉菜单），
// 用于验证菜单项读取确实收口到模型菜单容器、而不是整文档兜底撞上它。
// 点击菜单项会把选择器按钮文本更新为 "Gemini <family>"，供 switchGeminiModel
// 的「按钮短名校验」使用。
func geminiPickerPage(t *testing.T, shape string) string {
	t.Helper()
	picker := `<button data-test-id="bard-mode-menu-button" onclick="openMenu()">Gemini Flash</button>`
	if shape == "narrow" {
		picker = `<gem-button data-test-id="bard-mode-menu-button" onclick="openMenu()">Gemini Flash</gem-button>`
	}
	if shape == "modern" {
		picker = `<button aria-label="Open mode picker, currently Flash" onclick="openMenu()">Flash</button>`
	}
	picker = strings.Replace(picker, "onclick=", `id="test-picker" onclick=`, 1)

	decoy := ""
	if shape == "scoped" {
		decoy = `<gem-menu role="menu" id="decoy-menu"><gem-menu-item onclick="pick(this)" data-family="Decoy">3.1 Pro (decoy)</gem-menu-item></gem-menu>`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
`+picker+`
`+decoy+`
<gem-popover id="popover"><!----></gem-popover>
<script>
  const MENU = '<gem-menu role="menu" data-test-id="gem-mode-menu" data-visible="true">'
    + '<gem-menu-item role="menuitem" onclick="pick(this)" data-family="Flash-Lite" data-active="true"><div>3.5 Flash-Lite</div><div>Fastest answers</div></gem-menu-item>'
    + '<gem-menu-item role="menuitem" onclick="pick(this)" data-family="Flash" class="selected"><div>3.8 Flash</div><div>All-around help</div></gem-menu-item>'
    + '<gem-menu-item role="menuitem" onclick="pick(this)" data-family="Pro"><div>3.1 Pro</div><div>Advanced reasoning</div></gem-menu-item>'
    + '<gem-menu-item role="menuitem" onclick="pick(this)" data-family="thinking"><div>Extended thinking</div><div>Complex problems</div></gem-menu-item>'
    + '</gem-menu>';
  const picker = document.getElementById("test-picker");
  window.pickerOpens = 0;
  window.openMenu = function () {
    window.pickerOpens++;
    if (!document.querySelector('gem-menu[data-test-id="gem-mode-menu"]')) {
      document.getElementById('popover').innerHTML = MENU;
    }
  };
  window.pick = function (el) {
    document.getElementById('popover').innerHTML = '<!---->';
    picker.innerText = 'Gemini ' + el.dataset.family;
  };
  window.menuItemCount = function () { return document.querySelectorAll('gem-menu-item').length; };
</script>
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestGeminiPickerRealChrome 用真实 Chrome 验证模型切换物理动作在四种页面形态下都
// 可用：旧桌面 button、窄屏 gem-button、当前 aria-label button、以及带无关下拉菜单
// 的 scoped 页（回归：菜单项读取必须收口到 gem-mode-menu，不能整文档兜底）。
// 无 Chrome/沙箱阻断时自动 skip（cdpProbe 门控）。
func TestGeminiPickerRealChrome(t *testing.T) {
	for _, name := range []string{"desktop", "narrow", "modern", "scoped"} {
		t.Run(name, func(t *testing.T) {
			pageURL := geminiPickerPage(t, name)
			s := newTestCDPSession(t, Config{})

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := s.Navigate(ctx, pageURL); err != nil {
				t.Fatalf("navigate: %v", err)
			}
			time.Sleep(200 * time.Millisecond)

			// 懒渲染事实：未打开菜单时页面不该有模型菜单项（scoped 页只剩无关菜单那一项）。
			var before int
			if err := s.evalValue(ctx, "window.menuItemCount()", &before); err != nil {
				t.Fatalf("读未打开时的菜单项数: %v", err)
			}
			wantBefore := 0
			if name == "scoped" {
				wantBefore = 1
			}
			if before != wantBefore {
				t.Fatalf("未打开菜单时的菜单项数 = %d, want %d", before, wantBefore)
			}

			if name == "modern" {
				if err := s.switchGeminiModel(ctx, nil, "Flash"); err != nil {
					t.Fatal(err)
				}
				var opens int
				if err := s.evalValue(ctx, "window.pickerOpens", &opens); err != nil || opens != 0 {
					t.Fatalf("already selected: opens=%d err=%v", opens, err)
				}
			}
			if err := s.switchGeminiModel(ctx, nil, "3.1 Pro"); err != nil {
				t.Fatalf("switchGeminiModel(3.1 Pro): %v", err)
			}
			label, _ := s.evalResult(ctx, `(() => {
			  const b = document.getElementById("test-picker");
			  return b ? (b.innerText || '').trim() : '';
			})()`)
			if label != "Gemini Pro" {
				t.Fatalf("picker label = %q, want %q", label, "Gemini Pro")
			}
			if name == "scoped" {
				// 无关菜单必须没被点过（旧实现会命中它：整文档兜底取到 "3.1 Pro (decoy)"）。
				decoy, _ := s.evalResult(ctx, `(() => {
				  const el = document.querySelector('#decoy-menu gem-menu-item');
				  return el ? (el.innerText || '').trim() : '';
				})()`)
				if decoy != "3.1 Pro (decoy)" {
					t.Fatalf("无关菜单项被改动 = %q", decoy)
				}
			}
		})
	}
}

// TestPickGeminiOptionRealMenu 用线上真实菜单文本（2026-09-26 观测）验证选择逻辑：
// 家族冲突（Flash 不得命中 Flash-Lite）、标题+描述两行、Extended thinking 归类。
func TestPickGeminiOptionRealMenu(t *testing.T) {
	real := []modelbridge.MenuOption{
		{Text: "3.5 Flash-Lite Fastest answers"},
		{Text: "3.8 Flash All-around help", Selected: true},
		{Text: "3.1 Pro Advanced reasoning"},
		{Text: "Extended thinking Complex problems"},
	}
	for _, tc := range []struct {
		want string
		idx  int
	}{
		{"Flash", 1},
		{"3.8 Flash", 1},
		{"3.1 Pro", 2},
		{"Pro", 2},
		{"Flash-Lite", 0},
		{"3.5 Flash-Lite", 0},
	} {
		if got := modelbridge.PickGeminiOption(real, tc.want); got != tc.idx {
			t.Errorf("modelbridge.PickGeminiOption(%q) = %d, want %d", tc.want, got, tc.idx)
		}
	}
	if got := modelbridge.FindThinkingOption(real); got != 3 {
		t.Errorf("findThinkingOption = %d, want 3", got)
	}
	// 反面：整文档里混进无关项时 "3.1 Pro (decoy)" 会被 token 命中 —— 这正是
	// geminiMenuOptionsJS 必须把读取收口到 gem-mode-menu 容器的原因。
	if got := modelbridge.PickGeminiOption([]modelbridge.MenuOption{{Text: "3.1 Pro (decoy)"}}, "3.1 Pro"); got != 0 {
		t.Errorf("无关项 token 命中行为变化: %d", got)
	}
}
