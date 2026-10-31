package browser

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestOffscreenWindowKeepsScreencastAndInput 是「真窗口不出现在屏幕上」的可行性门禁。
//
// 默认体验（2026-09-15 用户定案）：任务执行期间真 Chrome 窗口不出现在屏幕上，屏幕上
// 只有观看小窗；用户点小窗才把真窗口搬出来。这条默认成立的前提有两个：
//  1. 窗口能被摆到一个「用户看不见」的位置（macOS 会夹取越界窗口，必须先知道会被夹到哪）；
//  2. 摆过去之后 Chrome 仍然渲染：画面流仍出帧、页面动画不冻结、物理输入仍必达
//     （Chrome 对不可见窗口有节流，遮挡场景实测会让 Input.dispatch* 挂起，见 GLOBAL #92）。
//
// 用例一次跑完「基线 + 三种停靠策略 + 搬回原位」的对照矩阵，并把每个状态下的
// 帧率、rAF 计数、窗口读回位置、visibilityState 打在同一行 EVIDENCE 日志里。
//
// 门禁与 TestCDPSessionOwnWindowKeepsFocus 相同：UWS_RUN_CHROME_E2E=1 +
// UWS_E2E_ATTACH_URL。用例自建专属窗口、结束时关闭，并在任何失败路径上把窗口搬回原位，
// 不移动、不关闭用户已有窗口。
func TestOffscreenWindowKeepsScreencastAndInput(t *testing.T) {
	if os.Getenv("UWS_RUN_CHROME_E2E") != "1" {
		t.Skip("未设置 UWS_RUN_CHROME_E2E=1，跳过会触碰真实 Chrome 的回归用例")
	}
	attachURL := os.Getenv("UWS_E2E_ATTACH_URL")
	if attachURL == "" {
		t.Skip("未设置 UWS_E2E_ATTACH_URL，跳过真机停靠策略用例")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	f := &CDPFactory{}
	sess, err := f.Create(ctx, Config{AttachCDPURL: attachURL})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sess.Close() }()
	s := sess.(*cdpSession)
	if !s.ownWindow {
		t.Fatalf("用例要求会话自建专属窗口（ownWindow=true），当前为 false")
	}
	probe := func(format string, args ...any) string {
		t.Helper()
		v, err := s.evalResult(ctx, fmt.Sprintf(format, args...))
		if err != nil {
			t.Fatalf("evaluate %s: %v", fmt.Sprintf(format, args...), err)
		}
		return v
	}

	wID, err := s.client.windowIDForTarget(ctx, s.targetID)
	if err != nil {
		t.Fatalf("getWindowForTarget: %v", err)
	}
	origin := readTestWindowBox(t, ctx, s, wID)
	restore := func() {
		bg, cancelBg := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelBg()
		_ = setTestWindowState(bg, s, wID, "normal")
		_ = s.client.Call(bg, "Browser.setWindowBounds", map[string]any{
			"windowId": wID,
			"bounds":   map[string]any{"left": origin.Left, "top": origin.Top},
		}, nil)
	}
	// 无论断言是否失败，都把窗口搬回原位（用户可能正在用这台机器）。
	t.Cleanup(restore)

	// 页面上跑一个 rAF 动画 + 一个可点按钮：动画是「渲染是否还活着」的独立信号
	// （比画面流更底层），按钮是「停靠状态下物理输入是否必达」的探针。
	if _, err := s.client.SendSession(ctx, s.sessionID, "Runtime.evaluate", map[string]any{
		"expression": `document.body.innerHTML=` +
			`'<div id="m" style="position:fixed;left:0;top:0;width:160px;height:160px;background:#e11d48"></div>' +` +
			`'<button id="b" style="position:fixed;right:0;bottom:0;width:120px;height:60px">x</button>';` +
			`window.__ticks=0;window.__c=0;` +
			`document.getElementById('b').onclick=function(){window.__c=1};` +
			`(function loop(){var t=(window.__ticks%240);` +
			`document.getElementById('m').style.transform='translate('+t+'px,'+t+'px)';` +
			`window.__ticks++;requestAnimationFrame(loop);})();true`,
		"returnByValue": true,
	}); err != nil {
		t.Fatalf("evaluate page setup: %v", err)
	}
	if err := s.StartScreencast(ctx, DefaultScreencastOptions()); err != nil {
		t.Fatalf("start screencast: %v", err)
	}
	defer func() {
		bg, cancelBg := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelBg()
		_ = s.StopScreencast(bg)
	}()
	waitForFirstFrame(t, ctx, s, 5*time.Second)

	// 这台显示器工作区（窗口现在就在它上面）：判断「搬走以后还看不看得见」的基准。
	display := readTestDisplay(t, probe)
	t.Logf("EVIDENCE display avail=%d,%d %dx%d window=%d,%d %dx%d",
		display.Left, display.Top, display.Width, display.Height,
		origin.Left, origin.Top, origin.Width, origin.Height)

	arms := []struct {
		name string
		park func() error
	}{
		{"baseline", func() error { return nil }},
		{"park_right", func() error {
			return writeTestWindowPos(ctx, s, wID, 20000, 60)
		}},
		{"park_below", func() error {
			return writeTestWindowPos(ctx, s, wID, 120, 20000)
		}},
		{"minimized", func() error {
			return setTestWindowState(ctx, s, wID, "minimized")
		}},
	}

	var results []testParkingArm
	for _, arm := range arms {
		if err := arm.park(); err != nil {
			t.Fatalf("停靠策略 %s 失败: %v", arm.name, err)
		}
		time.Sleep(1200 * time.Millisecond)
		box := readTestWindowBox(t, ctx, s, wID)
		measured := measureScreenOffloadArm(t, ctx, s, probe, 4*time.Second)
		measured.name = arm.name
		measured.box = box
		measured.visibility = probe("String(document.visibilityState)")
		measured.offScreen = !box.Intersects(display)
		results = append(results, measured)
		t.Logf("EVIDENCE arm=%s readback=%d,%d %dx%d off_screen=%v frames=%d ticks=%d visibility=%s",
			measured.name, box.Left, box.Top, box.Width, box.Height, measured.offScreen,
			measured.frames, measured.ticks, measured.visibility)
	}

	// 选一条「确实看不见 + 渲染还在跑」的策略作为停靠基线。
	best := ""
	for _, r := range results {
		if r.name != "baseline" && r.offScreen && r.frames > 0 && r.ticks > 0 {
			best = r.name
			break
		}
	}
	if best == "" {
		t.Fatalf("没有任何停靠策略同时满足「看不见 + 不掉帧」：%s", formatTestParkingArms(results))
	}
	// 在选中的停靠状态下验证物理输入必达。
	for _, arm := range arms {
		if arm.name == best {
			if err := arm.park(); err != nil {
				t.Fatalf("重放停靠策略 %s 失败: %v", best, err)
			}
		}
	}
	time.Sleep(1200 * time.Millisecond)
	clickOK, observed := s.ExecuteAction(ctx, "click", "", "x")
	clicked := probe("String(window.__c)")

	// 搬回原位：画面流必须恢复，否则「点小窗搬出来」的体验是坏的。
	restore()
	time.Sleep(1200 * time.Millisecond)
	back := measureScreenOffloadArm(t, ctx, s, probe, 3*time.Second)
	backBox := readTestWindowBox(t, ctx, s, wID)

	t.Logf("EVIDENCE summary strategy=%s click_ok=%v click_observed=%q click_effect=%s "+
		"restored=%d,%d frames_after_restore=%d ticks_after_restore=%d arms=[%s]",
		best, clickOK, observed, clicked, backBox.Left, backBox.Top, back.frames, back.ticks,
		formatTestParkingArms(results))

	if clickOK != true || clicked != "1" {
		t.Errorf("停靠状态下的物理点击未生效：ok=%v observed=%q __c=%s", clickOK, observed, clicked)
	}
	if back.frames == 0 {
		t.Errorf("窗口搬回原位后画面流没有恢复（ticks=%d）", back.ticks)
	}
}

// testParkingArm 是一次停靠观测：窗口实际落点 + 该状态下画面流与页面渲染的活性。
type testParkingArm struct {
	name       string
	box        testWindowBox
	offScreen  bool
	frames     int
	ticks      int64
	visibility string
}

func formatTestParkingArms(arms []testParkingArm) string {
	parts := make([]string, 0, len(arms))
	for _, a := range arms {
		parts = append(parts, fmt.Sprintf("%s{%d,%d %dx%d off=%v frames=%d ticks=%d}",
			a.name, a.box.Left, a.box.Top, a.box.Width, a.box.Height, a.offScreen, a.frames, a.ticks))
	}
	return strings.Join(parts, " ")
}

func measureScreenOffloadArm(t *testing.T, ctx context.Context, s *cdpSession, probe func(string, ...any) string, d time.Duration) testParkingArm {
	t.Helper()
	startTicks := ticksOf(t, probe)
	seen := map[float64]struct{}{}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if frame, ok := s.LatestScreencastFrame(); ok {
			seen[frame.Metadata.Timestamp] = struct{}{}
		}
		time.Sleep(50 * time.Millisecond)
	}
	endTicks := ticksOf(t, probe)
	return testParkingArm{frames: len(seen), ticks: endTicks - startTicks}
}

func ticksOf(t *testing.T, probe func(string, ...any) string) int64 {
	t.Helper()
	raw := probe("String(window.__ticks)")
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("解析 rAF 计数失败: raw=%q err=%v", raw, err)
	}
	return n
}

func waitForFirstFrame(t *testing.T, ctx context.Context, s *cdpSession, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := s.LatestScreencastFrame(); ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("等待首帧时上下文结束: %v", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("等待 %s 仍无画面流首帧", timeout)
}

type testWindowBox struct{ Left, Top, Width, Height int }

// Intersects 判断窗口矩形与显示器工作区是否有交叠。停靠后必须为 false：
// 只要还压在显示器上，用户就看得见那扇真窗口。
func (b testWindowBox) Intersects(screen testWindowBox) bool {
	return b.Left < screen.Left+screen.Width && screen.Left < b.Left+b.Width &&
		b.Top < screen.Top+screen.Height && screen.Top < b.Top+b.Height
}

func readTestWindowBox(t *testing.T, ctx context.Context, s *cdpSession, windowID int) testWindowBox {
	t.Helper()
	var out struct {
		Bounds struct {
			Left   int `json:"left"`
			Top    int `json:"top"`
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"bounds"`
	}
	if err := s.client.Call(ctx, "Browser.getWindowBounds", map[string]any{"windowId": windowID}, &out); err != nil {
		t.Fatalf("getWindowBounds: %v", err)
	}
	return testWindowBox{Left: out.Bounds.Left, Top: out.Bounds.Top, Width: out.Bounds.Width, Height: out.Bounds.Height}
}

// readTestDisplay 读窗口所在显示器的工作区（页面自报，坐标空间与 CDP 窗口 bounds 一致）。
func readTestDisplay(t *testing.T, probe func(string, ...any) string) testWindowBox {
	t.Helper()
	raw := probe("[screen.availLeft,screen.availTop,screen.availWidth,screen.availHeight].join(',')")
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		t.Fatalf("解析显示器工作区失败: raw=%q", raw)
	}
	var nums [4]int
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			t.Fatalf("解析显示器工作区失败: raw=%q err=%v", raw, err)
		}
		nums[i] = n
	}
	return testWindowBox{Left: nums[0], Top: nums[1], Width: nums[2], Height: nums[3]}
}

func writeTestWindowPos(ctx context.Context, s *cdpSession, windowID, left, top int) error {
	return s.client.Call(ctx, "Browser.setWindowBounds", map[string]any{
		"windowId": windowID,
		"bounds":   map[string]any{"left": left, "top": top},
	}, nil)
}

func setTestWindowState(ctx context.Context, s *cdpSession, windowID int, state string) error {
	return s.client.Call(ctx, "Browser.setWindowBounds", map[string]any{
		"windowId": windowID,
		"bounds":   map[string]any{"windowState": state},
	}, nil)
}
