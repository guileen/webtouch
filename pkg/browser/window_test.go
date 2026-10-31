package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
	"uws/pkg/browser/input"
	"uws/pkg/browser/internal/wstest"
	"uws/pkg/browser/window"
)

func TestParseSizeAndPos(t *testing.T) {
	cases := []struct {
		in   string
		w, h int
		ok   bool
	}{
		{"460x880", 460, 880, true},
		{"460*880", 460, 880, true},
		{"460,880", 460, 880, true},
		{" 320 X 640 ", 320, 640, true},
		{"", 0, 0, false},
		{"460", 0, 0, false},
		{"axb", 0, 0, false},
		{"0x100", 0, 0, false},
	}
	for _, c := range cases {
		w, h, ok := parseSize(c.in)
		if ok != c.ok || (ok && (w != c.w || h != c.h)) {
			t.Errorf("parseSize(%q) = %d,%d,%v; want %d,%d,%v", c.in, w, h, ok, c.w, c.h, c.ok)
		}
	}
	if x, y, ok := parsePos("1200,24"); !ok || x != 1200 || y != 24 {
		t.Errorf("parsePos(1200,24) = %d,%d,%v", x, y, ok)
	}
	if _, _, ok := parsePos("none"); ok {
		t.Error("parsePos(none) 应为非法")
	}
}

func TestResolveWindowPolicyDefaultsAndOverrides(t *testing.T) {
	// 测试默认值必须隔离运行宿主的体验开关，不能因开发者环境变量而漂移。
	t.Setenv("UWS_WINDOW_MODE", "")
	t.Setenv("UWS_WINDOW_SIZE", "")
	t.Setenv("UWS_WINDOW_POS", "")
	t.Setenv("UWS_INPUT_ACTIVATE", "")
	// 默认：同指纹共享一扇窗口（2026-09-24 用户定案「同一个浏览器指纹共享窗口，就是
	// 默认作为」）+ 工作区内可预期固定位（同轮定案「找得到窗口」）+ 永不自动前台化。
	p := resolveWindowPolicy()
	if p.mode != windowModeShared || p.park || !p.autoPosition || p.inputActivate != activateNever {
		t.Fatalf("默认策略不符: %+v", p)
	}
	if p.width != defaultWindowWidth || p.height != defaultWindowHeight {
		t.Fatalf("默认尺寸不符: %+v", p)
	}

	// 显式指定每会话一扇专属小窗（零前置行为的出口）。
	t.Setenv("UWS_WINDOW_MODE", "window")
	if p = resolveWindowPolicy(); p.mode != windowModeWindow {
		t.Fatalf("UWS_WINDOW_MODE=window 应显式换回专属小窗: %+v", p)
	}

	// 显式 shared 与非法值都落在 shared（缺省即共享）。
	t.Setenv("UWS_WINDOW_MODE", "shared")
	if p = resolveWindowPolicy(); p.mode != windowModeShared {
		t.Fatalf("UWS_WINDOW_MODE=shared 应为共享窗口: %+v", p)
	}

	t.Setenv("UWS_WINDOW_MODE", "tab")
	t.Setenv("UWS_WINDOW_SIZE", "800x600")
	t.Setenv("UWS_WINDOW_POS", "10,20")
	t.Setenv("UWS_INPUT_ACTIVATE", "always")
	p = resolveWindowPolicy()
	if p.mode != windowModeTab || p.width != 800 || p.height != 600 || p.park || p.autoPosition ||
		p.left != 10 || p.top != 20 || p.inputActivate != activateAlways {
		t.Fatalf("显式覆盖不符: %+v", p)
	}

	// 非法值回落默认，不报错（窗口策略是体验开关，不该让会话失败）。
	t.Setenv("UWS_WINDOW_MODE", "banana")
	t.Setenv("UWS_WINDOW_SIZE", "WxH")
	t.Setenv("UWS_WINDOW_POS", "nope")
	t.Setenv("UWS_INPUT_ACTIVATE", "maybe")
	p = resolveWindowPolicy()
	if p.mode != windowModeShared || p.width != defaultWindowWidth ||
		p.park || !p.autoPosition || p.inputActivate != activateNever {
		t.Fatalf("非法值应回落默认: %+v", p)
	}

	t.Setenv("UWS_WINDOW_POS", "none")
	if p = resolveWindowPolicy(); p.autoPosition || p.park {
		t.Fatalf("UWS_WINDOW_POS=none 应既不自动定位也不停靠: %+v", p)
	}

	// 显式可见与默认同义（工作区内右上角）。
	t.Setenv("UWS_WINDOW_POS", "auto")
	if p = resolveWindowPolicy(); !p.autoPosition || p.park {
		t.Fatalf("UWS_WINDOW_POS=auto 应为工作区内固定位且不停靠: %+v", p)
	}

	// 屏外停靠退为显式选项（2026-09-24）：要「屏幕上看不到」必须自己写出来。
	t.Setenv("UWS_WINDOW_POS", "offscreen")
	if p = resolveWindowPolicy(); !p.park || p.autoPosition {
		t.Fatalf("UWS_WINDOW_POS=offscreen 应为停靠: %+v", p)
	}
}

// TestParkedOutsideWorkArea 用真机取证里的读数钉住「停靠成功」的判据：
// 位置是 macOS 夹取后的**实际**落点，不是请求值（GLOBAL #110 的实测数据）。
func TestParkedOutsideWorkArea(t *testing.T) {
	// 主屏工作区（实测：0,0 1536x960）。位置来自 offscreen_stream_e2e_test.go。
	main := screenWorkArea{Left: 0, Top: 0, Width: 1536, Height: 960}
	window := func(left, top int) ViewerWindowBounds {
		return ViewerWindowBounds{Left: left, Top: top, Width: 500, Height: 540}
	}
	cases := []struct {
		name   string
		bounds ViewerWindowBounds
		want   bool
	}{
		{"请求 (20000,60) 被夹到 (1730,-41)：两条屏都够不着", window(1730, -41), true},
		{"park_below 读回 (120,919)：仍压在主屏内", window(120, 919), false},
		{"完全在屏内", window(100, 100), false},
		{"贴住右边缘 1px 也算看得见", window(1535, 100), false},
		{"刚好越过右边缘", window(1536, 100), true},
		{"屏幕上方之外", window(100, -540), true},
	}
	for _, c := range cases {
		if got := parkedOutsideWorkArea(main, c.bounds); got != c.want {
			t.Errorf("%s: parkedOutsideWorkArea(%+v) = %v; want %v", c.name, c.bounds, got, c.want)
		}
	}

	// 判据只针对「页面报的那块屏」的工作区（引擎侧读的就是 screen.avail*，Chrome 只报
	// 窗口当前所在那块屏）。副屏的情况把副屏工作区传进来即可：
	secondary := screenWorkArea{Left: -150, Top: 960, Width: 1920, Height: 1080}
	if parkedOutsideWorkArea(secondary, window(-100, 1000)) {
		t.Error("落在副屏工作区内的窗口应判为看得见")
	}
	if !parkedOutsideWorkArea(secondary, window(1730, -41)) {
		t.Error("(1730,-41) 在两块屏之外，应判为停靠")
	}
}

func TestNeedForegroundActivation(t *testing.T) {
	cases := []struct {
		mode, vis string
		err       error
		want      bool
	}{
		{activateAlways, "visible", nil, false},
		{activateNever, "hidden", nil, false},
		{activateAuto, "visible", nil, false},
		{activateAuto, "hidden", nil, false},
		{activateAuto, "prerender", nil, false},
		{activateAuto, "", context.DeadlineExceeded, false},
	}
	for _, c := range cases {
		if got := needForegroundActivation(c.mode, c.vis, c.err); got != c.want {
			t.Errorf("needForegroundActivation(%s,%s,%v) = %v; want %v", c.mode, c.vis, c.err, got, c.want)
		}
	}
}

// TestFitRightTopInWorkArea 钉住默认几何（2026-09-24）：工作区内、右上角留边距、
// 尺寸夹进工作区且不超界。第三例的数据来自本轮真机读回（工作区 1536x935）。
func TestFitRightTopInWorkArea(t *testing.T) {
	work := screenWorkArea{Left: 0, Top: 25, Width: 1536, Height: 935}
	got, ok := fitRightTopInWorkArea(work, 1440, 900, window.DefaultMargin)
	if !ok {
		t.Fatal("工作区可用时应给出几何")
	}
	if want := (ViewerWindowBounds{Left: 72, Top: 49, Width: 1440, Height: 887}); got != want {
		t.Fatalf("默认几何 = %+v; want %+v（高应夹到工作区-两侧边距）", got, want)
	}
	// 能放下的请求尺寸原样保留（不放大也不缩小）。
	if got, _ := fitRightTopInWorkArea(work, 460, 720, window.DefaultMargin); got != (ViewerWindowBounds{Left: 1052, Top: 49, Width: 460, Height: 720}) {
		t.Fatalf("小窗口几何 = %+v", got)
	}
	// 工作区小于请求尺寸：按工作区减两侧边距夹取。
	small := screenWorkArea{Left: 0, Top: 0, Width: 800, Height: 600}
	if got, _ := fitRightTopInWorkArea(small, 1440, 900, window.DefaultMargin); got != (ViewerWindowBounds{Left: 24, Top: 24, Width: 752, Height: 552}) {
		t.Fatalf("小工作区几何 = %+v", got)
	}
	// 极端小工作区：不超界（上限优先于下限），窗口整块落在屏内。
	tiny := screenWorkArea{Left: 0, Top: 0, Width: 400, Height: 300}
	got, ok = fitRightTopInWorkArea(tiny, 1440, 900, window.DefaultMargin)
	if !ok || got.Left < 0 || got.Top < 0 || got.Left+got.Width > 400 || got.Top+got.Height > 300 {
		t.Fatalf("极端小工作区不该超界: %+v ok=%v", got, ok)
	}
	// 工作区/尺寸不可用 → 不搬窗（fail closed，保持 Chrome 默认位）。
	if _, ok := fitRightTopInWorkArea(screenWorkArea{}, 1440, 900, window.DefaultMargin); ok {
		t.Fatal("工作区不可用时应返回 ok=false")
	}
	if _, ok := fitRightTopInWorkArea(work, 0, 900, window.DefaultMargin); ok {
		t.Fatal("尺寸非法时应返回 ok=false")
	}
}

// TestCreateTargetSendsWindowSpec 断言专属小窗的建窗参数落到 CDP 线上。
func TestCreateTargetSendsWindowSpec(t *testing.T) {
	params := make(chan map[string]any, 1)
	url := wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
		for {
			op, payload, err := wstest.ReadServerFrame(br)
			if err != nil {
				return
			}
			if op != wstest.OpText {
				continue
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}
			method, _ := msg["method"].(string)
			id, _ := msg["id"].(float64)
			if method == "Target.createTarget" {
				var m map[string]any
				_ = json.Unmarshal(payload, &m)
				if p, ok := msg["params"].(map[string]any); ok {
					params <- p
				}
			}
			resp := map[string]any{"id": int64(id), "result": map[string]any{"targetId": "T1"}}
			b, _ := json.Marshal(resp)
			_ = wstest.WriteServerText(conn, b)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := newCDPClient(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	noFocus := false
	if _, err := c.createTarget(ctx, "about:blank", targetSpec{
		NewWindow: true, Background: true, Focus: &noFocus, Width: 460, Height: 880,
	}); err != nil {
		t.Fatalf("createTarget: %v", err)
	}
	select {
	case p := <-params:
		if p["newWindow"] != true || p["background"] != true || p["focus"] != false {
			t.Fatalf("专属小窗参数不符（应 newWindow+background+不聚焦）: %v", p)
		}
		if p["width"] != float64(460) || p["height"] != float64(880) {
			t.Fatalf("小窗尺寸未下发: %v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未捕获 Target.createTarget 参数")
	}

	// 旧行为（tab 模式）：不新开窗口、不传尺寸，保持既有语义。
	if _, err := c.createTarget(ctx, "about:blank", targetSpec{}); err != nil {
		t.Fatalf("createTarget(tab): %v", err)
	}
	select {
	case p := <-params:
		if p["newWindow"] != false {
			t.Fatalf("tab 模式不应新开窗口: %v", p)
		}
		if _, has := p["width"]; has {
			t.Fatalf("tab 模式不应下发尺寸: %v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未捕获 tab 模式参数")
	}
}

// TestCDPSessionOwnWindowKeepsFocus 是「不打断用户」的真实 Chrome 回归门禁。
// 它需要两道显式开关：附着端点和 UWS_RUN_CHROME_E2E=1。仅配置了端点通常
// 表示用户正在使用调试 Chrome，不能因普通 go test 而创建、移动或关闭其窗口。
func TestCDPSessionOwnWindowKeepsFocus(t *testing.T) {
	if os.Getenv("UWS_RUN_CHROME_E2E") != "1" {
		t.Skip("未设置 UWS_RUN_CHROME_E2E=1，跳过会触碰真实 Chrome 的回归用例")
	}
	attachURL := os.Getenv("UWS_E2E_ATTACH_URL")
	if attachURL == "" {
		t.Skip("未设置 UWS_E2E_ATTACH_URL，跳过专属小窗真实浏览器用例")
	}
	f := &CDPFactory{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 本用例证明的是「每会话一扇专属小窗」这条路径：显式指定 window 模式，
	// 免得被默认的 shared（同指纹一扇窗多 Tab）改写前提。
	mk := func() *cdpSession {
		sess, err := f.Create(ctx, Config{AttachCDPURL: attachURL, WindowMode: "window"})
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		return sess.(*cdpSession)
	}
	s1, s2 := mk(), mk()

	probe := func(s *cdpSession, expr string) string {
		v, err := s.evalResult(ctx, expr)
		if err != nil {
			t.Fatalf("evaluate %s: %v", expr, err)
		}
		return v
	}
	for i, s := range []*cdpSession{s1, s2} {
		if !s.ownWindow {
			t.Fatalf("会话 %d 应在专属小窗里运行", i+1)
		}
		if vis := probe(s, "(String(document.visibilityState))"); vis != "visible" {
			t.Fatalf("会话 %d 小窗应可见（否则物理输入会挂起），got %s", i+1, vis)
		}
		// 输入必达护栏 1/2（2026-09-12，GLOBAL #92）：焦点仿真必须真的生效——
		// 页面认为自己在焦点中，后台/被遮挡小窗的物理输入才有分发门控。
		// R1（不抢焦点）的系统层判据（Chrome 从未被抬到前台）由
		// scripts/macprobe.swift front 采样证明，见 docs/evidence/README.md。
		// 焦点仿真开关（UWS_FOCUS_EMULATION）决定此处的期望值：取证对照组要能跑出
		// 「护栏关闭 → 遮挡下物理输入挂起」的原始形态，所以断言随开关取反。
		wantFocused := "true"
		if !input.FocusEmulationEnabled() {
			wantFocused = "false"
		}
		if focused := probe(s, "(String(document.hasFocus()))"); focused != wantFocused {
			t.Fatalf("会话 %d 焦点仿真状态不符（hasFocus=%s，want %s）", i+1, focused, wantFocused)
		}
	}
	// 每个会话独立窗口 + 独立 Target。
	w1, err := s1.client.windowIDForTarget(ctx, s1.targetID)
	if err != nil {
		t.Fatalf("getWindowForTarget: %v", err)
	}
	if w2, err := s2.client.windowIDForTarget(ctx, s2.targetID); err != nil || w1 == w2 {
		t.Fatalf("两个会话应各自独立窗口: w1=%d w2=%d err=%v", w1, w2, err)
	}
	// 小窗尺寸生效（Chrome 外框 ≈ 请求尺寸，允许 80px 内框差）。
	var bounds struct {
		Bounds struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"bounds"`
	}
	if err := s1.client.Call(ctx, "Browser.getWindowBounds", map[string]any{"windowId": w1}, &bounds); err != nil {
		t.Fatalf("getWindowBounds: %v", err)
	}
	// 双向容差：Chrome 对窗口宽度有下限（macOS Chrome 152 实测：请求 360 得 500），
	// 单向上限断言会把浏览器下限误判为失败。
	const sizeTol = 80
	if bounds.Bounds.Width < defaultWindowWidth-sizeTol || bounds.Bounds.Width > defaultWindowWidth+sizeTol ||
		bounds.Bounds.Height < defaultWindowHeight-sizeTol || bounds.Bounds.Height > defaultWindowHeight+sizeTol {
		t.Fatalf("小窗未按默认尺寸创建: %+v（期望 %dx%d ±%d）",
			bounds.Bounds, defaultWindowWidth, defaultWindowHeight, sizeTol)
	}
	// 非前台小窗里的物理点击仍然生效（不依赖 activateTarget）。
	if _, err := s1.client.SendSession(ctx, s1.sessionID, "Runtime.evaluate", map[string]any{
		"expression":    `document.body.innerHTML='<button id=b style="position:fixed;left:0;top:0;width:240px;height:120px">x</button>';window.__c=0;document.getElementById('b').onclick=function(){window.__c=1};true`,
		"returnByValue": true,
	}); err != nil {
		t.Fatalf("evaluate setup: %v", err)
	}
	if ok, observed := s1.ExecuteAction(ctx, "click", "", "x"); !ok {
		t.Fatalf("专属小窗物理点击失败: %s", observed)
	}
	if got := probe(s1, "(String(window.__c))"); got != "1" {
		t.Fatalf("专属小窗点击未生效: __c=%s", got)
	}
	// 非前台小窗里的物理输入（Input.insertText）同样不能依赖整窗前台焦点。
	if _, err := s1.client.SendSession(ctx, s1.sessionID, "Runtime.evaluate", map[string]any{
		"expression":    `document.body.innerHTML='<input id=i aria-label="搜索" placeholder="搜索">';window.__v="";document.getElementById('i').oninput=function(){window.__v=this.value};true`,
		"returnByValue": true,
	}); err != nil {
		t.Fatalf("evaluate setup input: %v", err)
	}
	if ok, observed := s1.ExecuteAction(ctx, "input", "uws-bg-input", "搜索"); !ok {
		t.Fatalf("专属小窗物理输入失败: %s", observed)
	}
	if got := probe(s1, "(String(window.__v))"); got != "uws-bg-input" {
		t.Fatalf("专属小窗输入未生效: __v=%s", got)
	}
	// 取证用：把会话窗口压住停一会，让外部脚本能截到小窗（默认 0 = 不停）。
	if raw := os.Getenv("UWS_E2E_HOLD_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
}
