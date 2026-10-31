package browser

import (
	"testing"

	"uws/pkg/browser/window"
)

// ---- 纯逻辑：指纹键 / 新 target 识别 / 白板判定 ----

func TestSharedWindowKeyNeedsAttachEndpoint(t *testing.T) {
	if _, ok := sharedWindowKey(Config{}); ok {
		t.Fatal("没有 attach 端点时不该给出共享键：那种情况没有可共享的窗口")
	}
	if _, ok := sharedWindowKey(Config{AttachCDPURL: "   "}); ok {
		t.Fatal("空白 attach 端点等同于没有")
	}
	k1, ok := sharedWindowKey(Config{AttachCDPURL: "http://127.0.0.1:9222"})
	if !ok {
		t.Fatal("有 attach 端点就该给出共享键")
	}
	k2, _ := sharedWindowKey(Config{AttachCDPURL: "http://127.0.0.1:9222", ProfileID: "p1"})
	k3, _ := sharedWindowKey(Config{AttachCDPURL: "http://127.0.0.1:9222", ProfileID: "p2"})
	k4, _ := sharedWindowKey(Config{AttachCDPURL: " http://127.0.0.1:9223 "})
	if k1 == k2 {
		t.Fatal("空 profile 与具名 profile 不是同一个指纹，不该共用一扇窗口")
	}
	if k2 == k3 {
		t.Fatal("不同 profile 必须给出不同键（同端点不等于同指纹）")
	}
	if k3 == k4 {
		t.Fatal("不同 attach 端点必须给出不同键")
	}
}

func TestSharedNewPageTargetsAndBlankURL(t *testing.T) {
	before := map[string]bool{"T0": true}
	infos := []window.TargetInfo{
		{TargetID: "T0", Type: "page", URL: "about:blank"},
		{TargetID: "T1", Type: "page", URL: "about:blank"},
	}
	fresh := window.NewPageTargets(before, infos)
	if len(fresh) != 1 || fresh[0].TargetID != "T1" {
		t.Fatalf("应只认领新出现的那个 target: %+v", fresh)
	}
	if got := window.NewPageTargets(before, []window.TargetInfo{{TargetID: "T0"}}); len(got) != 0 {
		t.Fatalf("没有新 target 时应为空: %+v", got)
	}
	for _, url := range []string{"", "about:blank", "about:blank#mid", " about:blank "} {
		if !window.BlankURL(url) {
			t.Fatalf("%q 应判为白板", url)
		}
	}
	for _, url := range []string{"https://example.com/", "chrome://newtab/", "data:text/html,x"} {
		if window.BlankURL(url) {
			t.Fatalf("%q 不该判为白板（不能认领别人的页面）", url)
		}
	}
}

// ---- 策略解析：默认 shared / 显式覆盖 / 退化 ----

func TestWindowPolicySharedDefaultAndDegradation(t *testing.T) {
	t.Setenv("UWS_WINDOW_MODE", "")
	attach := Config{AttachCDPURL: "http://127.0.0.1:9222"}

	if got := (&cdpSession{cfg: attach}).windowPolicy(); got.mode != windowModeShared {
		t.Fatalf("有 attach 端点时有头会话默认应是 shared，实际 %q", got.mode)
	}
	if got := (&cdpSession{cfg: Config{}}).windowPolicy(); got.mode != windowModeWindow {
		t.Fatalf("没有 attach 端点（自带 Chrome 进程）应退化为 window，实际 %q", got.mode)
	}
	if got := (&cdpSession{cfg: Config{AttachCDPURL: attach.AttachCDPURL, WindowMode: "window"}}).windowPolicy(); got.mode != windowModeWindow {
		t.Fatalf("Config.WindowMode=window 应覆盖默认，实际 %q", got.mode)
	}
	if got := (&cdpSession{cfg: attach, headless: true}).windowPolicy(); got.mode != windowModeTab {
		t.Fatalf("无头浏览器没有窗口，应保持 tab 语义，实际 %q", got.mode)
	}
	if got := (&cdpSession{cfg: Config{AttachCDPURL: attach.AttachCDPURL, WindowMode: "tab"}}).windowPolicy(); got.mode != windowModeWindow {
		t.Fatalf("有头 + tab 会把标签塞进用户窗口，必须回落 window，实际 %q", got.mode)
	}
	if got := (&cdpSession{cfg: Config{AttachCDPURL: attach.AttachCDPURL, WindowMode: "banana"}}).windowPolicy(); got.mode != windowModeShared {
		t.Fatalf("非法覆盖值应回落默认（shared），实际 %q", got.mode)
	}
}
