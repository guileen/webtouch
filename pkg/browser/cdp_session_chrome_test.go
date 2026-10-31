package browser

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCDPSessionRealChrome(t *testing.T) {
	pageURL, _, _ := cdpTestPage(t)
	s := newTestCDPSession(t, Config{
		UserAgent: "UWS-Test-UA/1.0",
		Timezone:  "Asia/Shanghai",
	})

	// 导航 + 探活（Emulation 覆盖后 UA/时区与配置一致）。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	pr := s.Probe()
	if pr.UserAgent != "UWS-Test-UA/1.0" {
		t.Fatalf("UA override mismatch: %q", pr.UserAgent)
	}
	if pr.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone override mismatch: %q", pr.Timezone)
	}

	// 页面候选：交互节点采集（真实 MATCH-05/06 输入通道）。
	cands := s.PageCandidates()
	texts := map[string]bool{}
	for _, c := range cands {
		texts[c.Text] = true
	}
	for _, want := range []string{"登录", "注册", "用户名"} {
		if !texts[want] {
			t.Fatalf("candidates missing %q: %+v", want, cands)
		}
	}

	// 物理点击（Fitts 轨迹）。
	ok, observed := s.ExecuteAction(ctx, "click", "", "登录")
	if !ok {
		t.Fatalf("click failed: %s", observed)
	}
	clicked, err := s.evalBool(ctx, `(window.__clicked === true)`)
	if err != nil || !clicked {
		t.Fatalf("click did not take effect: %v err=%v", clicked, err)
	}

	// 物理输入（Input.insertText + change）。
	ok, observed = s.ExecuteAction(ctx, "input", "hello-uws", "用户名")
	if !ok {
		t.Fatalf("input failed: %s", observed)
	}
	val, err := s.evalResult(ctx, `(document.getElementById('user').value)`)
	if err != nil || val != "hello-uws" {
		t.Fatalf("input value mismatch: %q err=%v", val, err)
	}

	// SPA pushState：点击后 NavigationState 应捕获软路由信号。
	ok, observed = s.ExecuteAction(ctx, "click", "", "跳转")
	if !ok {
		t.Fatalf("spa click failed: %s", observed)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		push, _, _ := s.NavigationState()
		if push {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pushState signal not captured")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestCDPSessionOcclusion(t *testing.T) {
	pageURL := cdpOverlayPage(t)
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	ok, observed := s.ExecuteAction(ctx, "click", "", "被遮按钮")
	if ok {
		t.Fatalf("遮挡目标不应执行成功")
	}
	if observed != "occluded" {
		t.Fatalf("expected occluded, got %q", observed)
	}
}

func TestCDPSessionScrollIntoView(t *testing.T) {
	pageURL := cdpScrollPage(t)
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	ok, observed := s.ExecuteAction(ctx, "click", "", "底部按钮")
	if !ok {
		t.Fatalf("视口外按钮点击失败: %s", observed)
	}
	clicked, err := s.evalBool(ctx, `(window.__clicked === true)`)
	if err != nil || !clicked {
		t.Fatalf("滚动后点击未生效: %v err=%v", clicked, err)
	}
}

// TDD-COORD-04 引擎原语：VLM 坐标原生拖拽（bypass 语义匹配，拖动 range 真实生效）。
func TestCDPSessionDrag(t *testing.T) {
	pageURL := cdpSliderPage(t)
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// 从滑块中点 (200,200) 拖到右端 (300,200)：value 50 → 100。
	ok, observed := s.ExecuteAction(ctx, "drag", "200,200,300,200", "challenge_slider")
	if !ok {
		t.Fatalf("drag failed: %s", observed)
	}
	if !strings.HasPrefix(observed, "drag:") {
		t.Fatalf("observed 应以 drag: 开头，got %q", observed)
	}

	deadline := time.Now().Add(3 * time.Second)
	val := ""
	for time.Now().Before(deadline) {
		v, err := s.evalResult(ctx, `(String(window.__dragValue))`)
		if err == nil {
			val = v
		}
		if val == "100" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if val != "100" {
		t.Fatalf("拖拽后滑块应到最大值 100，got %q", val)
	}

	// 非法坐标：不执行任何鼠标事件，返回失败。
	if ok, observed := s.ExecuteAction(ctx, "drag", "abc", "challenge_slider"); ok {
		t.Fatal("非法坐标不应执行成功")
	} else if observed != "invalid_drag_coords" {
		t.Fatalf("expected invalid_drag_coords, got %q", observed)
	}
}

// v1.5.1 CaptureScreenshot 数据源：真实 Chrome 返回 PNG 与视口尺寸。
func TestCDPSessionCaptureScreenshot(t *testing.T) {
	pageURL, _, _ := cdpTestPage(t)
	s := newTestCDPSession(t, Config{Viewport: &Viewport{Width: 800, Height: 600}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	// 模拟附着模式在导航前采集的尺寸过期；截图接口应按当前文档 CSS 视口刷新。
	s.mu.Lock()
	s.cfg.Viewport = &Viewport{Width: 1440, Height: 813, DeviceScaleFactor: 1}
	s.mu.Unlock()
	png, w, h, err := s.CaptureScreenshot(ctx)
	if err != nil {
		t.Fatalf("capture screenshot: %v", err)
	}
	if len(png) < 8 || !bytes.HasPrefix(png, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("返回内容应为 PNG（%d 字节，前缀 %x）", len(png), png[:min(8, len(png))])
	}
	if w != 800 || h != 600 {
		t.Fatalf("截图尺寸应取当前页面 CSS 视口 800x600，而非过期配置，got %dx%d", w, h)
	}
	if got := s.cfg.Viewport; got == nil || got.Width != 800 || got.Height != 600 {
		t.Fatalf("当前视口应同步回配置缓存，got %+v", got)
	}
	if _, rw, rh, err := s.CaptureScreenshotRegion(ctx, 700, 500, 200, 200); err != nil {
		t.Fatalf("capture clipped region: %v", err)
	} else if rw != 100 || rh != 100 {
		t.Fatalf("裁剪应按当前 800x600 视口钳制为 100x100，got %dx%d", rw, rh)
	}
}

func TestCDPSessionClicksPointerTextLeaf(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><body>
		<div id="row" onclick="window.__clicked=true" style="width:500px;height:60px">
		  <span id="target" style="display:inline-block;margin-left:430px;cursor:pointer">修改</span>
		</div><script>window.__clicked=false</script></body></html>`)
	}))
	t.Cleanup(srv.Close)
	s := newTestCDPSession(t, Config{Viewport: &Viewport{Width: 800, Height: 600}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if ok, observed := s.ExecuteAction(ctx, "click", "", "修改"); !ok {
		t.Fatalf("click pointer text leaf: %s", observed)
	}
	clicked, err := s.evalBool(ctx, `window.__clicked === true`)
	if err != nil || !clicked {
		t.Fatalf("pointer text leaf click did not fire: clicked=%v err=%v", clicked, err)
	}
	hit, ok := s.LastHitElement()
	if !ok || hit.Tag != "span" || hit.Text != "修改" {
		t.Fatalf("hit evidence should name the span leaf: ok=%v hit=%+v", ok, hit)
	}
}

func TestCDPSessionDisambiguatesPointerTextLeafByRowContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<!doctype html><html><body>"+
			"<div id='phone-row' class='item' style='width:500px;height:60px'><span>手机绑定</span>"+
			"<span id='phone-action' style='cursor:pointer'>修改</span></div>"+
			"<div id='password-row' class='item' style='width:500px;height:60px'><span>登录密码</span>"+
			"<span id='password-action' style='cursor:pointer'>修改</span></div>"+
			"<script>window.__hit='';"+
			"document.getElementById('password-row').addEventListener('click',()=>window.__hit='password');"+
			"document.getElementById('phone-row').addEventListener('click',()=>window.__hit='phone');"+
			"</script></body></html>")
	}))
	t.Cleanup(srv.Close)
	s := newTestCDPSession(t, Config{Viewport: &Viewport{Width: 800, Height: 600}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if ok, observed := s.ExecuteAction(ctx, "click", "", "登录密码"); !ok {
		t.Fatalf("click row-context pointer leaf: %s", observed)
	}
	var hit string
	err := s.evalValue(ctx, "window.__hit", &hit)
	if err != nil || hit != "password" {
		t.Fatalf("click should land in password row, got hit=%q err=%v", hit, err)
	}
	target, ok := s.LastHitElement()
	if !ok || target.Tag != "span" || target.Text != "修改" {
		t.Fatalf("hit evidence should name the row's pointer leaf: ok=%v target=%+v", ok, target)
	}
}

func TestCDPSessionNavFailure(t *testing.T) {
	s := newTestCDPSession(t, Config{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	deadURL := "http://" + ln.Addr().String() + "/"
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = s.Navigate(ctx, deadURL)
	if err == nil {
		t.Fatal("导航到拒绝连接的地址应返回显式错误")
	}
	if !strings.Contains(err.Error(), "navigation failed") {
		t.Fatalf("错误应含 navigation failed 语义，got: %v", err)
	}
}

func TestCDPSessionNewWindowOpened(t *testing.T) {
	pageURL := cdpNewWindowPage(t)
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// 点击 target="_blank" 链接：Target.targetCreated 事件驱动 NewWindowOpened。
	ok, observed := s.ExecuteAction(ctx, "click", "", "新标签")
	if !ok {
		t.Fatalf("new-window click failed: %s", observed)
	}
	var targetID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if id, found := s.NewWindowOpened(); found {
			targetID = id
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if targetID == "" {
		t.Fatal("未捕获新 Target（NEW_WINDOW_OPENED 事件缺失）")
	}

	// 原 Target 仍可用：后续点击真实生效。
	ok, observed = s.ExecuteAction(ctx, "click", "", "登录")
	if !ok {
		t.Fatalf("原 target 后续点击失败: %s", observed)
	}
	clicked, err := s.evalBool(ctx, `(window.__clicked === true)`)
	if err != nil || !clicked {
		t.Fatalf("原 target 点击未生效: %v err=%v", clicked, err)
	}
}

// TDD-ENG-14（新窗口接管，2026-09-22 用户现场反馈批次 P0-1）：新窗口出现后
// SwitchTarget 把作用面搬过去——新页可读可点，原 target 不关（用户仍能看见）。
func TestCDPSessionSwitchTarget(t *testing.T) {
	pageURL := cdpNewWindowPage(t)
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	orig := s.TargetID()

	ok, observed := s.ExecuteAction(ctx, "click", "", "新标签")
	if !ok {
		t.Fatalf("new-window click failed: %s", observed)
	}
	var targetID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if id, found := s.NewWindowOpened(); found {
			targetID = id
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if targetID == "" {
		t.Fatal("未捕获新 Target（NEW_WINDOW_OPENED 事件缺失）")
	}

	prev, err := s.SwitchTarget(ctx, targetID)
	if err != nil {
		t.Fatalf("SwitchTarget: %v", err)
	}
	if prev != orig {
		t.Fatalf("prev = %q, want %q", prev, orig)
	}
	if got := s.ActiveTargetID(); got != targetID {
		t.Fatalf("ActiveTargetID = %q, want %q", got, targetID)
	}
	// 新页处于网页状态：脚本与 DOM 都在（不是只剩一张图）。
	mark, err := s.evalResult(ctx, `document.getElementById('newtab') ? 'live' : 'blank'`)
	if err != nil || mark != "live" {
		t.Fatalf("新 target 页面不可用: mark=%q err=%v", mark, err)
	}
	// 原 target 未被关闭：可见、可切回。
	if targets := s.pageTargets(ctx); len(targets) < 2 {
		t.Fatalf("原 target 应保留，page targets = %d", len(targets))
	}
	// 切回原页同样可行（双向）。
	if _, err := s.SwitchTarget(ctx, orig); err != nil {
		t.Fatalf("切回原 target 失败: %v", err)
	}
	if got := s.ActiveTargetID(); got != orig {
		t.Fatalf("切回后 ActiveTargetID = %q, want %q", got, orig)
	}
}

func TestCDPSessionAttachMode(t *testing.T) {
	requireCDP(t)
	chrome, err := resolveChrome("")
	if err != nil {
		t.Skipf("无 Chrome，跳过 attach 测试: %v", err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	profile, err := os.MkdirTemp("", "uws-cdp-attach-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	cmd := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--no-first-run",
		"--disable-crash-reporter", "--disable-component-update",
		"--disable-background-networking",
		"--window-size=1280,900",
		"--remote-debugging-port="+strconv.Itoa(port),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+profile,
		"about:blank")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("launch chrome: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(profile)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := waitForEndpoint(ctx, port); err != nil {
		t.Fatalf("devtools endpoint: %v", err)
	}

	f := &CDPFactory{}
	s1, err := f.Create(ctx, Config{AttachCDPURL: "http://127.0.0.1:" + strconv.Itoa(port)})
	if err != nil {
		t.Fatalf("attach session1: %v", err)
	}
	defer s1.Close()
	s2, err := f.Create(ctx, Config{AttachCDPURL: "http://127.0.0.1:" + strconv.Itoa(port)})
	if err != nil {
		t.Fatalf("attach session2: %v", err)
	}
	defer s2.Close()
	if s1.ID() == s2.ID() {
		t.Fatalf("两个 attach 会话不应共享 target: %s", s1.ID())
	}
	// 附着模式未显式指定视口 → 跟随窗口实际尺寸（--window-size=1280,900 的真实
	// inner 尺寸，headless 下 innerHeight 可能 <900），不强设 1920x1080
	// （USER_ACTION #27 / GLOBAL #84：页面宽度=窗口宽度，不再“比窗口宽又无横向滚动条”）。
	cdp1 := s1.(*cdpSession)
	var dims struct {
		W int `json:"w"`
		H int `json:"h"`
	}
	if err := cdp1.evalValue(ctx, `({w: window.innerWidth, h: window.innerHeight})`, &dims); err != nil {
		t.Fatalf("read inner viewport: %v", err)
	}
	if dims.W != 1280 || dims.H <= 0 {
		t.Fatalf("页面宽度应=窗口宽度 1280（不得被 1920 覆盖），got %dx%d", dims.W, dims.H)
	}
	if cdp1.cfg.Viewport == nil || cdp1.cfg.Viewport.Width != dims.W || cdp1.cfg.Viewport.Height != dims.H {
		t.Fatalf("cfg.Viewport 应跟随页面实际视口 %dx%d，got %+v", dims.W, dims.H, cdp1.cfg.Viewport)
	}
	// 附着模式显式指定视口（移动模拟等任务级需求）→ 仍按配置覆盖。
	s3, err := f.Create(ctx, Config{
		AttachCDPURL:     "http://127.0.0.1:" + strconv.Itoa(port),
		Viewport:         &Viewport{Width: 390, Height: 844},
		ViewportExplicit: true,
	})
	if err != nil {
		t.Fatalf("attach session3(explicit viewport): %v", err)
	}
	defer s3.Close()
	cdp3 := s3.(*cdpSession)
	if err := cdp3.evalValue(ctx, `({w: window.innerWidth, h: window.innerHeight})`, &dims); err != nil {
		t.Fatalf("read explicit viewport: %v", err)
	}
	if dims.W != 390 || dims.H != 844 {
		t.Fatalf("显式视口 390x844 应生效，got %dx%d", dims.W, dims.H)
	}
	// attach 会话 Close 不应终止外部 Chrome（进程仍存活）。
	if cmd.ProcessState != nil {
		t.Fatal("attach Close 不应终止外部 Chrome")
	}
}
