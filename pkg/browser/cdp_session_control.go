package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"uws/pkg/browser/screencast"
	"uws/pkg/browser/window"
)

// ── 共享窗口（window 子包）桥接面（2026-09-29 复杂度治理·阶段 2·单元 3a）──────────
//
// `pkg/browser/window` 的 `Session` 是窄接口：宿主只经本适配器注入共享窗口簇**实测
// 用到**的 7 项 CDP 通道 ＋ 4 项会话事实（`Key`/`Headless`/`MarkShared`/`SharedWindowID`）。

// bridgeWindowSession 把 *cdpSession 适配成 window.Session。
type bridgeWindowSession struct{ s *cdpSession }

func (b bridgeWindowSession) Call(ctx context.Context, method string, params, out any) error {
	return b.s.client.Call(ctx, method, params, out)
}

func (b bridgeWindowSession) CallSession(ctx context.Context, sessionID, method string, params, out any) error {
	return b.s.client.CallSession(ctx, sessionID, method, params, out)
}

func (b bridgeWindowSession) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return b.s.client.Send(ctx, method, params)
}

func (b bridgeWindowSession) OffSession(sessionID string) int {
	return b.s.client.OffSession(sessionID)
}

func (b bridgeWindowSession) AttachTarget(ctx context.Context, targetID string) (string, error) {
	return b.s.client.attachTarget(ctx, targetID)
}

func (b bridgeWindowSession) CreateTarget(ctx context.Context, url string, spec window.TargetSpec) (string, error) {
	return b.s.client.createTarget(ctx, url, targetSpec{
		NewWindow:  spec.NewWindow,
		Background: spec.Background,
		Focus:      spec.Focus,
		Width:      spec.Width,
		Height:     spec.Height,
	})
}

func (b bridgeWindowSession) WindowIDForTarget(ctx context.Context, targetID string) (int, error) {
	return b.s.client.windowIDForTarget(ctx, targetID)
}

// Key 给出「同一个浏览器指纹」的窗口键（attach 端点 + profile id）。
func (b bridgeWindowSession) Key() (string, bool) { return sharedWindowKey(b.s.cfg) }

// Headless 报告本会话是否无头（无头下不装锚页焦点仿真护栏）。
func (b bridgeWindowSession) Headless() bool { return b.s.headless }

// MarkShared 回写会话事实：本会话的 Tab 开在 windowID 这扇共享窗口里。
func (b bridgeWindowSession) MarkShared(windowID int, anchorOwner bool) {
	b.s.sharedWindow = true
	b.s.sharedWindowID = windowID
	b.s.sharedAnchorOwner = anchorOwner
}

// SharedWindowID 返回本会话所属共享窗口 id（0 = 本会话不在共享窗口里）。
func (b bridgeWindowSession) SharedWindowID() int {
	if !b.s.sharedWindow {
		return 0
	}
	return b.s.sharedWindowID
}

// sharedWindowKey 是 window.Key 的宿主薄包装（保留给 cdp_session_core.go 的策略解析：
// 没有 attach 端点＝没有可共享的窗口，策略退化为每会话一窗）。
func sharedWindowKey(cfg Config) (string, bool) {
	return window.Key(cfg.AttachCDPURL, cfg.ProfileID)
}

// windowRegistry 是共享窗口注册表的宿主入口（生产走进程内默认注册表；测试各自
// window.NewRegistry() 隔离，不再需要「用例之间复位包级 map」）。
func windowRegistry() *window.Registry { return window.DefaultRegistry() }

// ── 真窗口/观看壳（window 子包）桥接面（2026-09-29 复杂度治理·阶段 2·单元 3c）──────
//
// `pkg/browser/window` 的 `ViewerSession` 是窄接口：宿主只经本适配器注入观看壳「展开」
// 链路**实测用到**的 8 项（6 项 CDP 通道/会话事实 ＋ 2 项救援前位置的状态进出）。
// `Bounds`/`Size`/`Controller`/`Geometry` 改由子包定义、此处别名，故 `pkg/viewerhub`、
// `pkg/engine`、`cmd/uws-viewer-demo` 与既有测试全部零改动。

// bridgeViewerSession 把 *cdpSession 适配成 window.ViewerSession。
// 「救援前位置」的加锁语义留在这里（**首次救援优先** ＋ 取走清零），不进子包。
type bridgeViewerSession struct{ s *cdpSession }

func (b bridgeViewerSession) TargetID() string { return b.s.targetID }

func (b bridgeViewerSession) Headless() bool { return b.s.headless }

func (b bridgeViewerSession) EvalValue(ctx context.Context, expr string, dst any) error {
	return b.s.evalValue(ctx, expr, dst)
}

func (b bridgeViewerSession) WindowIDForTarget(ctx context.Context) (int, error) {
	return b.s.client.windowIDForTarget(ctx, b.s.targetID)
}

func (b bridgeViewerSession) Call(ctx context.Context, method string, params, out any) error {
	return b.s.client.Call(ctx, method, params, out)
}

func (b bridgeViewerSession) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return b.s.client.Send(ctx, method, params)
}

// RememberWindow 记下救援前的位置：**首次优先**（已经记过就不再覆盖）。
func (b bridgeViewerSession) RememberWindow(bounds window.Bounds) {
	b.s.windowMu.Lock()
	defer b.s.windowMu.Unlock()
	if !b.s.windowSaved {
		b.s.savedWindow = bounds
		b.s.windowSaved = true
	}
}

// TakeSavedWindow 取走并清空救援前的位置；没救援过返回 ok=false。
func (b bridgeViewerSession) TakeSavedWindow() (window.Bounds, bool) {
	b.s.windowMu.Lock()
	defer b.s.windowMu.Unlock()
	saved, ok := b.s.savedWindow, b.s.windowSaved
	b.s.windowSaved = false
	return saved, ok
}

var (
	_ window.ViewerSession = bridgeViewerSession{}
	_ window.Controller    = (*cdpSession)(nil)
	_ window.Geometry      = (*cdpSession)(nil)
)

type (
	// ViewerWindowBounds 是一扇真窗口的位置与大小（见 window.Bounds）。
	ViewerWindowBounds = window.Bounds
	// ViewerWindowSize 是壳发来的展开尺寸（**新口径下不再使用**；保留以免动 wire 协议）。
	ViewerWindowSize = window.Size
	// ViewerWindowController 由「拥有真窗口」的会话实现（见 window.Controller）。
	ViewerWindowController = window.Controller
	// ViewerWindowGeometry 由「知道真窗口在哪儿」的会话实现（见 window.Geometry）。
	ViewerWindowGeometry = window.Geometry
)

// ErrNoViewerWindow 表示这个会话没有可搬动的真窗口（无头会话）。
var ErrNoViewerWindow = window.ErrNoViewerWindow

// BrowserProcessInfo 由知道「谁在承载这个会话」的会话实现；壳用它判断焦点去了哪里
// （是自己那扇真页面，还是用户自己的应用）。附着模式下浏览器属于用户，返回 0。
type BrowserProcessInfo interface {
	BrowserPID() int
}

// BrowserPID 返回本会话启动的浏览器进程；附着模式下浏览器属于用户，返回 0。
func (s *cdpSession) BrowserPID() int {
	if s.chromeCmd != nil && s.chromeCmd.Process != nil {
		return s.chromeCmd.Process.Pid
	}
	return 0
}

// ViewerAlive 实现 viewerhub.ViewerSessionLiveness：这个会话的页面还在不在。
// 用户手动关掉真窗口时它变 false，引擎据此让观看壳收起面板，而不是停在一张再也
// 不会更新的截图上（2026-09-12 真机复跑现场）。
func (s *cdpSession) ViewerAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.destroyed && !s.closed
}

// 以下 5 个方法是 window 子包流程的一行门面（方法体已下沉，见 window/viewer.go）。

func (s *cdpSession) RealWindowBounds(ctx context.Context) (ViewerWindowBounds, bool) {
	return window.RealBounds(ctx, bridgeViewerSession{s})
}

func (s *cdpSession) RaiseViewerWindow(ctx context.Context) (ViewerWindowBounds, error) {
	return window.Raise(ctx, bridgeViewerSession{s})
}

func (s *cdpSession) PrepareViewerWindow(ctx context.Context) (ViewerWindowBounds, error) {
	return window.Prepare(ctx, bridgeViewerSession{s})
}

func (s *cdpSession) ActivateViewerWindow(ctx context.Context) error {
	return window.Activate(ctx, bridgeViewerSession{s})
}

func (s *cdpSession) RestoreViewerWindow(ctx context.Context) error {
	return window.Restore(ctx, bridgeViewerSession{s})
}

// 三个 `Browser.*` 窗口命令的宿主别名：命令形状（请求字段与回执解码）只在
// `window` 子包定义一处，宿主调用点（`cdp_session_core.go` 的停靠/几何读回）不变。

func (c *cdpClient) windowBounds(ctx context.Context, windowID int) (ViewerWindowBounds, string, error) {
	return window.ReadBounds(ctx, c, windowID)
}

func (c *cdpClient) setWindowBounds(ctx context.Context, windowID int, b ViewerWindowBounds) error {
	return window.SetBounds(ctx, c, windowID, b)
}

func (c *cdpClient) setWindowState(ctx context.Context, windowID int, state string) error {
	return window.SetState(ctx, c, windowID, state)
}

// Navigate 导航到 url 并安装 MutationObserver（TDD-ENG-01 接线）。
func (s *cdpSession) Navigate(ctx context.Context, url string) error {
	var out struct {
		FrameID string `json:"frameId"`
	}
	s.mu.Lock()
	s.navErr = ""
	s.navFrameID = ""
	s.mu.Unlock()
	if err := s.client.CallSession(ctx, s.sessionID, "Page.navigate",
		map[string]any{"url": url}, &out); err != nil {
		return fmt.Errorf("cdp: navigate: %w", err)
	}
	s.mu.Lock()
	s.navFrameID = out.FrameID
	s.mu.Unlock()

	// 等待主框架 load 或导航失败；SPA/数据页可能不触发 load，超时不视为失败。
	// 主框架 loadFailed（DNS/连接/TLS 错误）显式上报，避免「页面无此元素」误导排障。
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	loaded := make(chan struct{}, 1)
	go func() {
		_, _ = s.client.waitEvent(waitCtx, s.sessionID, "Page.loadEventFired")
		select {
		case loaded <- struct{}{}:
		default:
		}
	}()
waitLoop:
	for {
		s.mu.Lock()
		navErr := s.navErr
		s.mu.Unlock()
		if navErr != "" {
			s.clearPageState()
			return fmt.Errorf("cdp: navigation failed: %s", navErr)
		}
		select {
		case <-loaded:
			break waitLoop
		case <-waitCtx.Done():
			break waitLoop
		case <-time.After(50 * time.Millisecond):
		}
	}
	s.installObserver(ctx)
	// 兜底二次校验：chrome-error 判定先于 loadEventFired 到达，但 select 可能
	// 先命中 loaded 分支，离开循环后仍要确认没有已记录的导航失败。
	s.mu.Lock()
	navErr := s.navErr
	s.mu.Unlock()
	if navErr != "" {
		s.clearPageState()
		return fmt.Errorf("cdp: navigation failed: %s", navErr)
	}
	s.mu.Lock()
	s.navCnt++
	s.inflight = make(map[string]time.Time)
	s.docGen++
	s.candCache = nil
	s.mu.Unlock()
	return nil
}

// clearPageState 清除页面级状态（导航失败路径：无 Document 响应可依赖时，
// 避免上次页面的 challenge/lastStatus 残留误导判定）。
func (s *cdpSession) clearPageState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastStatus = 0
	s.challenge = false
	s.inflight = make(map[string]time.Time)
	s.docGen++
	s.candCache = nil
}

// installObserver 注入 MutationObserver 计数器（SPA 时序 DOM 突变信号）。
func (s *cdpSession) installObserver(ctx context.Context) {
	const js = `(() => {
	  if (window.__uwsMutObs) { try { window.__uwsMutObs.disconnect(); } catch (e) {} }
	  window.__uwsMutCount = 0;
	  window.__uwsMutObs = new MutationObserver(function () { window.__uwsMutCount++; });
	  try {
	    window.__uwsMutObs.observe(document.documentElement, {
	      childList: true, subtree: true, characterData: true, attributes: false
	    });
	  } catch (e) {}
	  return true;
	})()`
	_, _ = s.evalBool(ctx, js)
}

// SetCookie 通过 Network.setCookie 注入 Cookie。
func (s *cdpSession) SetCookie(c Cookie) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s.setCookie(ctx, c)
}

func (s *cdpSession) setCookie(ctx context.Context, c Cookie) {
	params := map[string]any{
		"name": c.Name, "value": c.Value,
		"path": orDefault(c.Path, "/"),
	}
	if c.Domain != "" {
		params["domain"] = c.Domain
	}
	if c.HTTPOnly {
		params["httpOnly"] = true
	}
	if c.Secure {
		params["secure"] = true
	}
	if c.Expires > 0 {
		params["expires"] = c.Expires
	}
	_ = s.client.CallSession(ctx, s.sessionID, "Network.setCookie", params, nil)
	s.mu.Lock()
	found := false
	for i := range s.cookies {
		if s.cookies[i].Name == c.Name {
			s.cookies[i] = c
			found = true
		}
	}
	if !found {
		s.cookies = append(s.cookies, c)
	}
	s.mu.Unlock()
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Cookies 返回当前页面可见 Cookie（Network.getCookies）。
func (s *cdpSession) Cookies() []Cookie {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out struct {
		Cookies []struct {
			Name     string  `json:"name"`
			Value    string  `json:"value"`
			Domain   string  `json:"domain"`
			Path     string  `json:"path"`
			HTTPOnly bool    `json:"httpOnly"`
			Secure   bool    `json:"secure"`
			Expires  float64 `json:"expires"`
		} `json:"cookies"`
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Network.getCookies", nil, &out); err == nil {
		cookies := make([]Cookie, 0, len(out.Cookies))
		for _, c := range out.Cookies {
			cookies = append(cookies, Cookie{
				Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
				HTTPOnly: c.HTTPOnly, Secure: c.Secure, Expires: int64(c.Expires),
			})
		}
		return cookies
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Cookie(nil), s.cookies...)
}

// Close 关闭会话：附着模式 detach，启动模式终止 Chrome 并清理临时 profile。
func (s *cdpSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stream := s.screencast
	workerHookCancel := s.workerHookCancel
	s.workerHookCancel = nil
	close(s.closedCh)
	s.mu.Unlock()
	if workerHookCancel != nil {
		workerHookCancel()
	}
	if stream != nil {
		stream.Release()
	}
	if s.client != nil {
		// 附着模式（接管用户 Chrome）：关闭本会话对应的标签页而不是整个浏览器；
		// 启动模式由 teardownChrome 杀进程，无需单独 closeTarget。
		if !s.launched && s.targetID != "" {
			_, _ = s.client.Send(context.Background(), "Target.closeTarget",
				map[string]any{"targetId": s.targetID})
		}
		// 共享窗口回收：窗内只剩锚页时关掉锚页（窗口随之消失）；窗内还有别的页面
		// 就一律不动（pkg/browser/window）。
		windowRegistry().Release(bridgeWindowSession{s})
		// 持久身份：先请 Chrome 自己退出。SIGKILL 掉的话最后一段 Cookie/LocalStorage
		// 不会落盘——而持久身份的全部意义就是保住那些数据。失败无所谓，
		// teardownChrome 会兜底补刀。
		if s.launched && s.profilePersistent {
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = s.client.Send(closeCtx, "Browser.close", nil)
			cancel()
		}
		_ = s.client.Close()
	}
	s.teardownChrome()
	return nil
}

// ── Page.startScreencast 画面流的兼容面（2026-09-27 复杂度治理·阶段 1，单元 5）──────
//
// 流的实现已出包到 uws/pkg/browser/screencast；这里保留原名，让 pkg/engine、pkg/viewerhub、
// cmd/uws-viewer-demo 等既有消费方零改动。新代码请直接用 screencast 包。

type (
	// ScreencastOptions 是 Page.startScreencast 的参数（见 screencast.Options）。
	ScreencastOptions = screencast.Options
	// ScreencastMetadata 是 Chrome Page.screencastFrame 的页面坐标元数据。
	ScreencastMetadata = screencast.Metadata
	// ScreencastFrame 是一帧 JPEG 及其 Chrome 元数据。
	ScreencastFrame = screencast.Frame
	// ScreencastSource 是 cdpSession 可选提供的画面流能力。
	ScreencastSource = screencast.Source

	screencastStream = screencast.Stream
)

// ErrScreencastClosed 表示宿主 CDP 会话或其 target 已关闭，不能再开始画面流。
var ErrScreencastClosed = screencast.ErrClosed

// DefaultScreencastOptions 返回 P1 Viewer 的保守缩略图参数。
func DefaultScreencastOptions() ScreencastOptions { return screencast.DefaultOptions() }

// newScreencastStream 建一条画面流：命令与事件订阅由本会话注入。
func newScreencastStream(sessionID, targetID string, command screencast.Command, ack screencast.Ack) *screencastStream {
	return screencast.New(sessionID, targetID, command, ack)
}

// ── 观看壳桥（window 子包）的兼容面（2026-09-29 复杂度治理·阶段 2·单元 3c）──────────
//
// 被动观看总线随「窗口/观看域」出包到 uws/pkg/browser/window；这里保留原名，让
// pkg/viewerhub、pkg/engine、cmd/uws-viewer-demo 与既有测试零改动。

type (
	// ViewerSessionInfo 标识拥有这条桥的一个 UWS 会话（见 window.ViewerSessionInfo）。
	ViewerSessionInfo = window.ViewerSessionInfo
	// ViewerPointerKind 描述一次观测到的 CDP 指针跃迁种类。
	ViewerPointerKind = window.ViewerPointerKind
	// ViewerPointer 是一次观测到的指针状态（画面坐标，CSS 像素）。
	ViewerPointer = window.ViewerPointer
	// ViewerAction 是一条脱敏的动作标签（不含页面内容）。
	ViewerAction = window.ViewerAction
	// ViewerFrame 是一帧页面 JPEG 及其元数据。
	ViewerFrame = window.ViewerFrame
	// ViewerUpdate 是一个会话的最新完整观测。
	ViewerUpdate = window.ViewerUpdate
	// PointerObserver 被动接收 CDP 指针跃迁（实现者必须只观察、不回放输入）。
	PointerObserver = window.PointerObserver
	// PointerObserverSink 是「会话可选提供、宿主后可挂观察者」的能力面。
	PointerObserverSink = window.PointerObserverSink
	// ViewerBridge 把一个会话的可选画面流转成原生壳的被动本地帧边界。
	ViewerBridge = window.ViewerBridge
)

const (
	// 指针跃迁种类的取值（见 window.ViewerPointerKind）。
	ViewerPointerMoved    = window.ViewerPointerMoved
	ViewerPointerPressed  = window.ViewerPointerPressed
	ViewerPointerReleased = window.ViewerPointerReleased
)

// NewViewerBridge 为**唯一一个**具名 UWS 会话建桥（window.NewViewerBridge 的宿主薄包装）。
func NewViewerBridge(session ViewerSessionInfo) (*ViewerBridge, error) {
	return window.NewViewerBridge(session)
}

// StartScreencast starts a session-scoped Page.startScreencast stream. It is
// intentionally an optional capability rather than part of CDPSession, so
// existing engine mocks and non-viewer callers remain unchanged.
func (s *cdpSession) StartScreencast(ctx context.Context, options ScreencastOptions) error {
	stream, err := s.getScreencastStream()
	if err != nil {
		return err
	}
	return stream.Start(ctx, options)
}

// StopScreencast stops the protocol stream and releases the retained latest
// frame. Repeating it is safe and does not issue duplicate stop commands.
func (s *cdpSession) StopScreencast(ctx context.Context) error {
	s.mu.Lock()
	stream := s.screencast
	s.mu.Unlock()
	if stream == nil {
		return nil
	}
	return stream.Stop(ctx)
}

func (s *cdpSession) markTargetDestroyed(targetID string) {
	s.mu.Lock()
	if targetID != s.targetID {
		s.mu.Unlock()
		return
	}
	s.destroyed = true
	stream := s.screencast
	s.mu.Unlock()
	if stream != nil {
		stream.Release()
	}
}

// LatestScreencastFrame returns a defensive copy of the one retained JPEG
// frame. Polling this method cannot mutate the producer's frame slot.
func (s *cdpSession) LatestScreencastFrame() (ScreencastFrame, bool) {
	s.mu.Lock()
	stream := s.screencast
	s.mu.Unlock()
	if stream == nil {
		return ScreencastFrame{}, false
	}
	return stream.LatestFrame()
}

func (s *cdpSession) getScreencastStream() (*screencastStream, error) {
	s.mu.Lock()
	if s.closed || s.destroyed || s.client == nil || s.sessionID == "" {
		s.mu.Unlock()
		return nil, ErrScreencastClosed
	}
	if s.screencast != nil {
		stream := s.screencast
		s.mu.Unlock()
		return stream, nil
	}
	client, sessionID, targetID := s.client, s.sessionID, s.targetID
	stream := newScreencastStream(sessionID, targetID,
		func(ctx context.Context, method string, params any) error {
			return client.CallSession(ctx, sessionID, method, params, nil)
		},
		func(method string, params any) error {
			return client.SendSessionNoWait(sessionID, method, params)
		})
	s.screencast = stream
	s.mu.Unlock()
	// cdpClient dispatches handlers synchronously from its reader goroutine.
	// The stream ACK path is therefore deliberately response-free (see
	// SendSessionNoWait), and has no dependency on a Viewer consumer.
	client.On(sessionID, "Page.screencastFrame", func(ev cdpEvent) {
		stream.HandleFrame(ev.SessionID, ev.Params)
	})
	return stream, nil
}

func (s *cdpSession) teardownChrome() {
	if s.chromeCmd != nil && s.chromeCmd.Process != nil {
		if s.profilePersistent {
			// 持久身份：给 Chrome 两秒把自己退出（它会在退出路上把 Cookie/Storage
			// 落盘），超时才补刀。reap 只交给那一个 Wait goroutine，避免并发 Wait。
			done := make(chan struct{})
			go func() { _, _ = s.chromeCmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = s.chromeCmd.Process.Kill()
				<-done
			}
		} else {
			_ = s.chromeCmd.Process.Kill()
			_, _ = s.chromeCmd.Process.Wait()
		}
	}
	// 临时目录用完即焚；持久身份是用户的资产，绝不删。
	if s.profileDir != "" && !s.profilePersistent {
		_ = os.RemoveAll(s.profileDir)
	}
}
