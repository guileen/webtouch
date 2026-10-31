package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uws/pkg/browser/internal/wstest"
	"uws/pkg/browser/window"
)

// viewerWindowFake 专门伺候「展开」这条链路：它像真 Chrome 一样记住最后一次被设置的
// 窗口矩形并在 Browser.getWindowBounds 里回放，这样「返回 Chrome 实际生效的矩形」也被
// 覆盖，而不是只测发出去的命令；可选地回答页面报的显示器工作区。
type viewerWindowFake struct {
	mu       sync.Mutex
	calls    []string
	bounds   map[string]any
	workArea map[string]any
}

func newViewerWindowFake() *viewerWindowFake {
	return &viewerWindowFake{bounds: map[string]any{
		"left": 1400, "top": 24, "width": 500, "height": 540, "windowState": "normal",
	}}
}

func (f *viewerWindowFake) record(method string) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
}

func (f *viewerWindowFake) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.calls {
		if m == method {
			n++
		}
	}
	return n
}

func (f *viewerWindowFake) window() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]any, len(f.bounds))
	for k, v := range f.bounds {
		out[k] = v
	}
	return out
}

func (f *viewerWindowFake) serve(t *testing.T) string {
	t.Helper()
	return wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
		for {
			op, payload, err := wstest.ReadServerFrame(br)
			if err != nil {
				return
			}
			if op != wstest.OpText {
				continue
			}
			var msg map[string]any
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			id, _ := msg["id"].(float64)
			method, _ := msg["method"].(string)
			params, _ := msg["params"].(map[string]any)
			f.record(method)

			result := map[string]any{}
			switch method {
			case "Browser.getWindowForTarget":
				result = map[string]any{"windowId": 7}
			case "Browser.getWindowBounds":
				result = map[string]any{"bounds": f.window()}
			case "Browser.setWindowBounds":
				if sent, ok := params["bounds"].(map[string]any); ok {
					f.mu.Lock()
					for key, value := range sent {
						f.bounds[key] = value
					}
					f.mu.Unlock()
				}
			case "Runtime.evaluate":
				f.mu.Lock()
				work := f.workArea
				f.mu.Unlock()
				expr, _ := params["expression"].(string)
				if work != nil && strings.Contains(expr, "availWidth") {
					result = map[string]any{"result": map[string]any{"type": "object", "value": work}}
				}
			}
			encoded, _ := json.Marshal(map[string]any{"id": int64(id), "result": result})
			_ = wstest.WriteServerText(conn, encoded)
		}
	})
}

// 展开态 = 真页面：把这扇真窗口抬到最前，**不动它的位置与大小**（2026-09-22 口径）。
// 窗口本来就在用户眼前时，引擎一次 setWindowBounds 都不该发。
func TestRaiseViewerWindowActivatesWithoutTouchingTheGeometry(t *testing.T) {
	fake := newViewerWindowFake()
	s := newInputTestSession(t, fake.serve(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	applied, err := s.RaiseViewerWindow(ctx)
	if err != nil {
		t.Fatalf("抬到最前: %v", err)
	}
	// 回执里是真窗口当前的矩形——壳拿它当「小窗飞过去」的动画落点。
	want := ViewerWindowBounds{Left: 1400, Top: 24, Width: 500, Height: 540}
	if applied != want {
		t.Fatalf("生效矩形 = %+v, want %+v", applied, want)
	}
	if n := fake.count("Browser.setWindowBounds"); n != 0 {
		t.Fatalf("窗口已在屏内时不该动几何（setWindowBounds 次数 = %d）: %v", n, fake.calls)
	}
	if fake.count("Target.activateTarget") != 1 {
		t.Fatal("展开后必须把真窗口抬到前面，否则页面压在别人窗口底下等于没展开")
	}
	// 抬前没动过窗口，收回自然也没窗口要还回去。
	if err := s.RestoreViewerWindow(ctx); err != nil {
		t.Fatalf("收回: %v", err)
	}
	if n := fake.count("Browser.setWindowBounds"); n != 0 {
		t.Fatalf("没搬过窗口的收回不该动窗口（setWindowBounds 次数 = %d）: %v", n, fake.calls)
	}
}

// 真窗口整块停在屏外（引擎默认停靠 left=20000）时「抬到最前」等于「点开什么也没有」：
// 这是新口径下唯一允许动窗口的情形——按工作区左上角搬进屏内，尺寸不变，并记下原位。
func TestRaiseViewerWindowRescuesAWindowParkedOffscreen(t *testing.T) {
	fake := newViewerWindowFake()
	fake.bounds = map[string]any{"left": 20000, "top": 60, "width": 1440, "height": 900, "windowState": "normal"}
	fake.workArea = map[string]any{"left": 0, "top": 25, "width": 1280, "height": 775}
	s := newInputTestSession(t, fake.serve(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applied, err := s.RaiseViewerWindow(ctx)
	if err != nil {
		t.Fatalf("抬到最前: %v", err)
	}
	// 工作区左上角 + margin 24；尺寸取窗口原尺寸与工作区的较小者（1440>1280、900>775）。
	want := ViewerWindowBounds{Left: 24, Top: 49, Width: 1280, Height: 775}
	if applied != want {
		t.Fatalf("救援矩形 = %+v, want %+v", applied, want)
	}
	if err := s.RestoreViewerWindow(ctx); err != nil {
		t.Fatalf("收回: %v", err)
	}
	if got := fake.window(); got["left"] != float64(20000) || got["top"] != float64(60) ||
		got["width"] != float64(1440) || got["height"] != float64(900) {
		t.Fatalf("收回应把救援搬过的窗口放回原位: %+v", got)
	}
}

// 只看得见一条边不算看得见：#113 现场读回 left=1496（1440 宽的窗口只剩 40px 在屏内），
// 与看不见没有区别——这正是「展开必须真落在眼前」这条判断存在的理由。
func TestVisibleEnoughForViewerRejectsAFortyPixelSliver(t *testing.T) {
	work := ViewerWindowBounds{Left: 0, Top: 25, Width: 1536, Height: 935}
	sliver := ViewerWindowBounds{Left: 1496, Top: 60, Width: 1440, Height: 900}
	if window.VisibleEnough(work, sliver) {
		t.Fatal("只剩 40px 在屏内的窗口不该算看得见")
	}
	onscreen := ViewerWindowBounds{Left: 24, Top: 49, Width: 1440, Height: 900}
	if !window.VisibleEnough(work, onscreen) {
		t.Fatal("整块在屏内的窗口必须算看得见")
	}
	mostlyOutside := ViewerWindowBounds{Left: 940, Top: 60, Width: 1440, Height: 900}
	if window.VisibleEnough(work, mostlyOutside) {
		t.Fatal("可见面积不到一半（596/1440）不该算看得见")
	}
	offScreen := ViewerWindowBounds{Left: 20000, Top: 60, Width: 1440, Height: 900}
	if window.VisibleEnough(work, offScreen) {
		t.Fatal("停靠位 20000 显然不算看得见")
	}
}

// 最小化/全屏的窗口抬不到前面：抬之前必须先回 normal（activateTarget 对最小化的窗口
// 不起作用），否则用户点开小窗什么也看不到。
func TestRaiseViewerWindowNormalisesAWindowStateFirst(t *testing.T) {
	fake := newViewerWindowFake()
	fake.bounds["windowState"] = "minimized"
	s := newInputTestSession(t, fake.serve(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.RaiseViewerWindow(ctx); err != nil {
		t.Fatalf("抬到最前: %v", err)
	}
	if fake.count("Browser.setWindowBounds") != 1 {
		t.Fatalf("只该发一次 setWindowBounds（把状态回 normal）: %v", fake.calls)
	}
	if got := fake.window()["windowState"]; got != "normal" {
		t.Fatalf("窗口状态 = %v", got)
	}
}

// 无头会话没有窗口可抬：必须明确报错，壳才能回落到自绘面板。
func TestRaiseViewerWindowWithoutAWindowFailsLoudly(t *testing.T) {
	s := &cdpSession{headless: true}
	if _, err := s.RaiseViewerWindow(context.Background()); !errors.Is(err, ErrNoViewerWindow) {
		t.Fatalf("无头会话应当返回 ErrNoViewerWindow, got %v", err)
	}
	if err := s.RestoreViewerWindow(context.Background()); err != nil {
		t.Fatalf("无头会话收回应当空操作: %v", err)
	}
}

// 附着模式（浏览器属于用户）没有自己的进程号，壳据此退回几何判定。
func TestBrowserPIDIsUnknownForAttachedSessions(t *testing.T) {
	s := &cdpSession{}
	if got := s.BrowserPID(); got != 0 {
		t.Fatalf("BrowserPID = %d, want 0", got)
	}
}

// 用户手动关掉那扇真窗口后，会话必须自报「页面没了」，观看壳才不会停在旧截图上。
func TestViewerAliveFollowsTargetDestruction(t *testing.T) {
	s := &cdpSession{id: "cdp-1", targetID: "T1"}
	if !s.ViewerAlive() {
		t.Fatal("target 还在时会话应当是活的")
	}
	s.markTargetDestroyed("T1")
	if s.ViewerAlive() {
		t.Fatal("用户关掉真窗口后会话必须自报已结束")
	}
}

// 有头启动会在临时 profile 里预置「关掉翻译」的偏好（启动参数在 Chrome 152 上没能
// 完全关掉那个飘在页面左上角的气泡，2026-09-12 现场）。
func TestHeadedLaunchSeedsTranslatePreference(t *testing.T) {
	dir := t.TempDir()
	seedChromePreferences(dir)
	payload, err := os.ReadFile(filepath.Join(dir, "Default", "Preferences"))
	if err != nil {
		t.Fatalf("Preferences 未写入: %v", err)
	}
	var prefs struct {
		Translate struct {
			Enabled bool `json:"enabled"`
		} `json:"translate"`
	}
	if err := json.Unmarshal(payload, &prefs); err != nil {
		t.Fatalf("Preferences 不是合法 JSON: %v", err)
	}
	if prefs.Translate.Enabled {
		t.Fatalf("翻译应当被关掉: %s", payload)
	}
}

// 有头启动模式会带着抑制浏览器气泡的参数（2026-09-12 现场：页面左上角飘着翻译气泡）。
func TestHeadedLaunchSuppressesBrowserBubbles(t *testing.T) {
	found := map[string]bool{}
	for _, arg := range chromeBubbleSuppressionArgs {
		found[arg] = true
	}
	for _, want := range []string{"--disable-features=Translate,TranslateUI", "--disable-translate", "--no-default-browser-check", "--disable-session-crashed-bubble"} {
		if !found[want] {
			t.Fatalf("缺少抑制参数 %s（现有 %v）", want, chromeBubbleSuppressionArgs)
		}
	}
}
