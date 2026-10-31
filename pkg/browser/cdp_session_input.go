package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"
	"uws/pkg/challenge"

	"uws/pkg/browser/input"
)

// dragAt 按 VLM 求解坐标执行原生拖拽：移动到起点 → mousePressed →
// 沿 Fitts 贝塞尔轨迹移动（按住按钮）→ 终点 mouseReleased（TDD-COORD-04 引擎原语）。
// 不依赖语义定位（challenge_slider 仅作日志语义），完全由坐标驱动。
func (s *cdpSession) dragAt(ctx context.Context, x1, y1, x2, y2 float64) error {
	s.activateForInput(ctx)
	start := s.mousePos()
	if !s.mouseInit {
		start = pt{x: x1, y: y1}
	}
	if start != (pt{x: x1, y: y1}) {
		if err := s.dispatchMouse(ctx, "mouseMoved", pt{x: x1, y: y1}, "none", 0); err != nil {
			return err
		}
	}
	if err := s.dispatchMouse(ctx, "mousePressed", pt{x: x1, y: y1}, "left", 1); err != nil {
		return err
	}
	dist := math.Hypot(x2-x1, y2-y1)
	size := math.Max(dist, 1)
	path := newBezierPath(pt{x: x1, y: y1}, pt{x: x2, y: y2}, size,
		rand.New(rand.NewSource(time.Now().UnixNano())))
	for _, p := range path {
		// 拖动过程中必须携带 buttons=1（按住左键），否则页面不识别为拖拽。
		if err := s.dispatchMouseWithButtons(ctx, "mouseMoved", p, "left", 1, 1); err != nil {
			return err
		}
	}
	if err := s.dispatchMouseWithButtons(ctx, "mouseReleased", pt{x: x2, y: y2}, "left", 1, 0); err != nil {
		return err
	}
	s.mu.Lock()
	s.lastMouse = pt{x: x2, y: y2}
	s.mouseInit = true
	s.mu.Unlock()
	s.startMutationSampling()
	return nil
}

func (s *cdpSession) mousePos() pt {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.mouseInit {
		return pt{x: 0, y: 0}
	}
	return s.lastMouse
}

func (s *cdpSession) dispatchMouse(ctx context.Context, typ string, p pt, button string, clickCount int) error {
	return s.dispatchMouseWithButtons(ctx, typ, p, button, clickCount, 0)
}

func (s *cdpSession) dispatchMouseWithButtons(ctx context.Context, typ string, p pt, button string, clickCount, buttons int) error {
	return s.dispatchPointer(ctx, typ, p, button, clickCount, buttons, false)
}

func (s *cdpSession) dispatchPointer(ctx context.Context, typ string, p pt, button string, clickCount, buttons int, idle bool) error {
	params := map[string]any{
		"type": typ, "x": p.x, "y": p.y,
		"button": button, "clickCount": clickCount, "buttons": buttons,
	}
	if err := s.inputGuard().Call(ctx, "Input.dispatchMouseEvent", params); err != nil {
		return err
	}
	s.pointerHumanizer().NoteDispatch(input.Point{X: p.x, Y: p.y}, time.Now())
	s.notifyPointer(typ, p, button, clickCount, buttons, idle)
	return nil
}

// SetPointerObserver implements PointerObserverSink. Attaching or detaching an
// observer cannot change automation behaviour: notification happens strictly
// after a dispatch already succeeded, and a nil observer costs one mutex read.
func (s *cdpSession) SetPointerObserver(observer PointerObserver) {
	s.mu.Lock()
	s.pointerObserver = observer
	s.mu.Unlock()
}

// SetPreDispatchReceptorSink implements PreDispatchReceptorSink. Attaching or
// detaching the sink cannot change automation behaviour: the callback happens
// strictly before a dispatch is issued and carries no return value; a nil sink
// costs one mutex read. 见 spec/algorithm-kernel.md §5.0.14 决议 0③。
func (s *cdpSession) SetPreDispatchReceptorSink(sink func(PreDispatchReceptor)) {
	s.mu.Lock()
	s.preDispatchSink = sink
	s.mu.Unlock()
}

// notifyPreDispatchReceptor 在「受体已解析、物理事件未发出」时回传受体（fail-soft：
// 无订阅者直接返回；回调不得影响调用方后续的遮挡判定与派发）。必须在 disp.mark()
// **之前**调用——mark() 之后即「已派发」（§5.0.11），那时再回读属事后取证。
func (s *cdpSession) notifyPreDispatchReceptor(t elementTarget) {
	if !t.Found {
		return
	}
	s.mu.Lock()
	sink := s.preDispatchSink
	s.mu.Unlock()
	if sink == nil {
		return
	}
	sink(PreDispatchReceptor{
		Found: true,
		X:     t.X,
		Y:     t.Y,
		Sel:   t.Sel,
		Tag:   t.Tag,
		Text:  t.Text,
	})
}

func (s *cdpSession) notifyPointer(typ string, p pt, button string, clickCount, buttons int, idle bool) {
	s.mu.Lock()
	observer := s.pointerObserver
	s.mu.Unlock()
	if observer == nil {
		return
	}
	kind := ViewerPointerMoved
	switch typ {
	case "mousePressed":
		kind = ViewerPointerPressed
	case "mouseReleased":
		kind = ViewerPointerReleased
	}
	observer.ObservePointer(ViewerPointer{
		X: p.x, Y: p.y, Kind: kind,
		Button: button, ClickCount: clickCount, Buttons: buttons,
		Idle: idle,
	})
}

// ProbeChallenge 探测当前页面是否处于人机验证或风控拦截态（战役一）。
func (s *cdpSession) ProbeChallenge(ctx context.Context) (*challenge.ChallengeSignal, bool) {
	s.mu.Lock()
	if s.detector == nil {
		s.detector = challenge.NewDetector()
	}
	d := s.detector
	status := s.lastStatus
	headers := s.lastHeaders
	s.mu.Unlock()

	url := s.PageURL()
	title := s.PageTitle()

	sig := d.Detect(ctx, s, url, title, status, headers)
	s.mu.Lock()
	defer s.mu.Unlock()
	if sig != nil {
		s.challenge = true
		s.challengeSignal = sig
		return sig, true
	}
	s.challenge = false
	s.challengeSignal = nil
	return nil, false
}

// PageTitle 返回当前页面 document.title。
func (s *cdpSession) PageTitle() string {
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	title, _ := s.evalResult(ctx, "document.title")
	return title
}

// DispatchHumanInput 转发人类在 Web 视窗 Canvas 上的逆向输入至 CDP（战役二）。
func (s *cdpSession) DispatchHumanInput(ctx context.Context, ev challenge.HumanInputEvent) error {
	switch ev.Type {
	case challenge.HumanInputClick:
		btn := ev.Button
		if btn == "" {
			btn = "left"
		}
		p := pt{x: ev.X, y: ev.Y}
		_ = s.dispatchMouse(ctx, "mouseMoved", p, "none", 0)
		if err := s.dispatchMouse(ctx, "mousePressed", p, btn, 1); err != nil {
			return err
		}
		return s.dispatchMouse(ctx, "mouseReleased", p, btn, 1)
	case challenge.HumanInputMouseMove:
		return s.dispatchMouse(ctx, "mouseMoved", pt{x: ev.X, y: ev.Y}, "none", 0)
	case challenge.HumanInputMouseDown:
		btn := ev.Button
		if btn == "" {
			btn = "left"
		}
		return s.dispatchMouse(ctx, "mousePressed", pt{x: ev.X, y: ev.Y}, btn, 1)
	case challenge.HumanInputMouseUp:
		btn := ev.Button
		if btn == "" {
			btn = "left"
		}
		return s.dispatchMouse(ctx, "mouseReleased", pt{x: ev.X, y: ev.Y}, btn, 1)
	case challenge.HumanInputInsertText:
		if ev.Text != "" {
			return s.inputGuard().Call(ctx, "Input.insertText", map[string]any{"text": ev.Text})
		}
	case challenge.HumanInputKeyDown:
		if ev.Key != "" {
			params := map[string]any{"type": "keyDown", "key": ev.Key}
			if ev.Text != "" {
				params["text"] = ev.Text
			}
			return s.inputGuard().Call(ctx, "Input.dispatchKeyEvent", params)
		}
	case challenge.HumanInputKeyUp:
		if ev.Key != "" {
			return s.inputGuard().Call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": ev.Key})
		}
	}
	return nil
}

// CaptureScreenshot 经 CDP Page.captureScreenshot 抓取当前页面截图（PNG，v1.5.1 RPC 数据源）。
func (s *cdpSession) CaptureScreenshot(ctx context.Context) ([]byte, int, int, error) {
	w, h, err := s.refreshLayoutViewport(ctx)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("cdp: read screenshot viewport: %w", err)
	}
	var raw struct {
		Data string `json:"data"`
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Page.captureScreenshot",
		map[string]any{"format": "png", "fromSurface": true}, &raw); err != nil {
		return nil, 0, 0, err
	}
	png, err := base64.StdEncoding.DecodeString(raw.Data)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("cdp: decode screenshot: %w", err)
	}
	return png, w, h, nil
}

// refreshLayoutViewport 读取当前文档的 CSS 布局视口，并同步配置缓存。
// 附着页可能在导航后应用站点缩放；初始化时采集的窗口尺寸会因此过期。
// VLM 比例和 ClickAt 都使用 CSS px，所以截图时必须取当前 innerWidth/innerHeight。
func (s *cdpSession) refreshLayoutViewport(ctx context.Context) (int, int, error) {
	var layout struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	if err := s.evalValue(ctx, `({width: window.innerWidth, height: window.innerHeight})`, &layout); err != nil {
		return 0, 0, err
	}
	if layout.Width <= 0 || layout.Height <= 0 {
		return 0, 0, fmt.Errorf("invalid layout viewport %dx%d", layout.Width, layout.Height)
	}
	s.mu.Lock()
	if s.cfg.Viewport == nil {
		s.cfg.Viewport = &Viewport{DeviceScaleFactor: 1}
	}
	s.cfg.Viewport.Width = layout.Width
	s.cfg.Viewport.Height = layout.Height
	s.mu.Unlock()
	return layout.Width, layout.Height, nil
}

// CaptureScreenshotRegion 按视口 CSS 坐标裁剪区域截图（browser.ScreenshotterRegion）。
// clip 坐标钳制到视口内；返回实际裁剪尺寸（供 VLM 换算坐标）。
func (s *cdpSession) CaptureScreenshotRegion(ctx context.Context, x, y, w, h int) ([]byte, int, int, error) {
	vw, vh, err := s.refreshLayoutViewport(ctx)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("cdp: read region viewport: %w", err)
	}
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x+w > vw {
		w = vw - x
	}
	if y+h > vh {
		h = vh - y
	}
	if w < 1 || h < 1 {
		return nil, 0, 0, fmt.Errorf("cdp: clip region empty: %d,%d %dx%d", x, y, w, h)
	}
	var raw struct {
		Data string `json:"data"`
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Page.captureScreenshot",
		map[string]any{
			"format":      "png",
			"fromSurface": true,
			"clip": map[string]any{
				"x": x, "y": y, "width": w, "height": h, "scale": 1,
			},
		}, &raw); err != nil {
		return nil, 0, 0, err
	}
	png, err := base64.StdEncoding.DecodeString(raw.Data)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("cdp: decode region screenshot: %w", err)
	}
	return png, w, h, nil
}

// ExportPageTree 实现 browser.PageTreeExporter：先采集 kept 节点（复用
// collectProbeCandidates，把 BFS 节点写入 window.__uwsProbe 的 WASM 缓冲），
// 再导出 ExportToVLMFormat 极简 AXTree 文本（v1.5.5 ExportPageTree RPC 数据源）。
func (s *cdpSession) ExportPageTree(ctx context.Context) (string, error) {
	if _, _, err := s.collectProbeCandidates(ctx); err != nil {
		return "", err
	}
	interactive, err := s.evalResult(ctx, probeExportVlmExpr())
	if err != nil {
		return "", err
	}
	// ref 行补 href 与最近行上下文（同文链接、同名按钮可区分）。
	// 增强信息采集失败时静默降级，不阻断两段式快照。
	var rawMeta struct {
		Hrefs    map[string]string `json:"hrefs"`
		Contexts map[string]struct {
			Tag  string `json:"tag"`
			Text string `json:"text"`
		} `json:"contexts"`
	}
	hrefs := map[uint32]string{}
	contexts := map[uint32]RefContext{}
	if err := s.evalValue(ctx, probeRefMetadataExpr(), &rawMeta); err == nil {
		for k, v := range rawMeta.Hrefs {
			id64, perr := strconv.ParseUint(k, 10, 32)
			if perr == nil && v != "" {
				hrefs[uint32(id64)] = v
			}
		}
		for k, v := range rawMeta.Contexts {
			id64, perr := strconv.ParseUint(k, 10, 32)
			if perr == nil && v.Text != "" {
				contexts[uint32(id64)] = RefContext{Tag: v.Tag, Text: v.Text}
			}
		}
	}
	interactive = annotateRefTreeHrefs(interactive, hrefs)
	interactive = annotateRefTreeContexts(interactive, contexts)
	// v1.5.6：页面语义内容（Accessibility 完整树，供 VLM 阅读理解——标题/正文/
	// 链接/状态都在这里，Google 结果这类正文页不再“看不见”）+ 可交互 ref 树
	// （[node_id] 与探针同源，click:node/input:node 确定性引用用）。
	content, axErr := s.exportAXContentTree(ctx)
	if axErr != nil {
		content = "（本页 AX 内容树不可用：" + axErr.Error() + "）"
	}
	var b strings.Builder
	b.WriteString("# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）\n")
	b.WriteString(content)
	b.WriteString("\n\n# 可交互节点（[ref] 行；click/input 用 node:<ref> 确定性引用）\n")
	b.WriteString(interactive)
	return b.String(), nil
}

// —— 输入域子包（`uws/pkg/browser/input`）适配器与薄包装（复杂度治理·阶段 2·单元 2a）——
// 落点纪律：折叠进既有宿主文件（本文件＝输入域宿主侧），不新开转发文件（条目收益归零）。
// bridgeEval 把宿主的 evalValue 暴露给 input 子包（该组实测只用这一项能力）；
// 下面三个薄包装让本包调用点与 `pkg/engine` 消费的符号保持不变。
// ParseDragCoords 保持包级导出（`pkg/engine/session.go` 直接消费），实现已下沉子包。

type bridgeEval struct{ s *cdpSession }

func (b bridgeEval) EvalValue(ctx context.Context, expr string, dst any) error {
	return b.s.evalValue(ctx, expr, dst)
}

func (s *cdpSession) captureTargetLease(ctx context.Context) *input.Lease {
	return input.CaptureLease(ctx, bridgeEval{s})
}

func (s *cdpSession) reanchorTarget(ctx context.Context, lease *input.Lease) (bool, string, error) {
	return input.Reanchor(ctx, bridgeEval{s}, lease)
}

func (s *cdpSession) guardInputReceptor(ctx context.Context, semantic, value string) (bool, string, error) {
	return input.GuardInputReceptor(ctx, bridgeEval{s}, semantic, value)
}

// ParseDragCoords 解析 ExecuteAction drag 的 value（实现见 input.ParseDragCoords）。
func ParseDragCoords(value string) (x1, y1, x2, y2 float64, err error) {
	return input.ParseDragCoords(value)
}

// —— 单元 2b：物理输入必达护栏 + 指针拟人化的接入点（2026-09-29）——
// 逻辑已入 `pkg/browser/input`（`RetryGuard`/`Pointer`）；本文件只留两样东西：
// ① 把宿主能力面暴露给子包的适配器；② 每个会话一份组件实例的访问器。

// bridgeCaller 把宿主的会话下发能力暴露给 input.RetryGuard（重试与「输入就绪」重放）。
type bridgeCaller struct{ s *cdpSession }

func (b bridgeCaller) Ready() bool {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	return b.s.sessionID != ""
}

func (b bridgeCaller) Call(ctx context.Context, method string, params map[string]any, out any) error {
	b.s.mu.Lock()
	client, sid := b.s.client, b.s.sessionID
	b.s.mu.Unlock()
	return client.CallSession(ctx, sid, method, params, out)
}

// inputGuard 返回本会话的「必达」护栏（懒建，每个会话一份 ⇒ 重试计数按会话累计）。
func (s *cdpSession) inputGuard() *input.RetryGuard {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inputRetry == nil {
		s.inputRetry = input.NewRetryGuard(bridgeCaller{s})
	}
	return s.inputRetry
}

// 指针拟人化的静默漂移节奏（默认值取自子包，见 input.DefaultDriftTuning）。
// 取证/真机用例在**建会话之前**调小周期（cdp_session_factory.go 在此时把值注入组件）。
var (
	pointerDriftIdleMin     = input.DefaultDriftIdleMin
	pointerDriftInterval    = input.DefaultDriftInterval
	pointerDriftProbability = input.DefaultDriftProbability
)

// pointerHumanizer 返回本会话的指针拟人化组件（懒建，每个会话一份 ⇒ 落点 rng 与
// 静默判据按会话隔离）。
func (s *cdpSession) pointerHumanizer() *input.Pointer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pointer == nil {
		s.pointer = input.NewPointer(s.closedCh, func(ctx context.Context, p input.Point) error {
			// 静默期漂移：页面侧与真输入完全一样（同样是 mouseMoved，同样喂给观看小窗的
			// 光标），唯一区别是它带上 Idle 标记，因而不算「Agent 还在动」——漂移每几秒
			// 就一次，若照旧刷新面板的静默时钟，「没动作就收面板」（2026-09-16 现场诉求）
			// 就永远不会触发。
			return s.dispatchPointer(ctx, "mouseMoved", pt{x: p.X, y: p.Y}, "none", 0, 0, true)
		}, input.DriftTuning{
			IdleMin:     pointerDriftIdleMin,
			Interval:    pointerDriftInterval,
			Probability: pointerDriftProbability,
		})
	}
	return s.pointer
}

// —— 单元 2c：穿透寻底 + 点击/输入自愈的接入点（2026-09-29）——
// 逻辑已入 `pkg/browser/input`（`pierce.go` / `recovery.go`）；本文件只留适配器
// `bridgeInputSession`（方法集＝`input.Session` 的 13 项），与 `bridgeCaller`/`bridgeEval`
// 并列，不再留任何 `*cdpSession` 薄包装方法。

// bridgeInputSession 把宿主会话能力面暴露给输入簇（穿透寻底与自愈闭环）。
type bridgeInputSession struct{ s *cdpSession }

func (b bridgeInputSession) EvalValue(ctx context.Context, expr string, dst any) error {
	return b.s.evalValue(ctx, expr, dst)
}

// Call 复用 2b `bridgeCaller` 的口径（锁内取 client/sessionID），语义与旧
// `s.client.CallSession(ctx, s.sessionID, …)` 等价。
func (b bridgeInputSession) Call(ctx context.Context, method string, params map[string]any, out any) error {
	return bridgeCaller{b.s}.Call(ctx, method, params, out)
}

func (b bridgeInputSession) InsertText(ctx context.Context, text string) error {
	return b.s.inputGuard().Call(ctx, "Input.insertText", map[string]any{"text": text})
}

func (b bridgeInputSession) EvalBool(ctx context.Context, expr string) (bool, error) {
	return b.s.evalBool(ctx, expr)
}

func (b bridgeInputSession) FindElementMode(ctx context.Context, semantic, selector string, preferLast, strictTarget, editableOnly bool) (input.ElementTarget, error) {
	return findElementMode(ctx, b.s, semantic, selector, preferLast, strictTarget, editableOnly)
}

func (b bridgeInputSession) CheckOcclusion(ctx context.Context, t input.ElementTarget) (bool, error) {
	return b.s.checkOcclusion(ctx, t)
}

func (b bridgeInputSession) ClickAt(ctx context.Context, t input.ElementTarget) error {
	return b.s.clickAt(ctx, t)
}

func (b bridgeInputSession) InputAt(ctx context.Context, disp *input.DispatchTracker, t input.ElementTarget, action, value, semantic string) error {
	return b.s.inputAt(ctx, disp, t, action, value, semantic)
}

func (b bridgeInputSession) NotifyPreDispatchReceptor(t input.ElementTarget) {
	b.s.notifyPreDispatchReceptor(t)
}

func (b bridgeInputSession) CloseGeminiMenu(ctx context.Context) error {
	return b.s.closeGeminiMenu(ctx)
}

func (b bridgeInputSession) GeminiInputEmpty(ctx context.Context) bool {
	return b.s.geminiInputEmpty(ctx)
}

func (b bridgeInputSession) TargetDestroyed() bool {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	return b.s.destroyed
}

func (b bridgeInputSession) RecordHit(t input.ElementTarget) {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	b.s.lastHit = t
}

// clickAt 按 Fitts 贝塞尔轨迹物理点击目标（TDD-ENG-03）。落点不是元素正中心，
// 而是元素内随机采样（2026-09-22 批次 #6 指针拟人化，见 pointer_humanize.go）。
func (s *cdpSession) clickAt(ctx context.Context, t elementTarget) error {
	target := s.pointerHumanizer().ClickTarget(t)
	return s.clickPoint(ctx, target.X, target.Y)
}

// clickPoint 按 Fitts 贝塞尔轨迹物理点击视口坐标（TDD-ENG-03；ClickAt 与语义点击共用）。
func (s *cdpSession) clickPoint(ctx context.Context, x, y float64) error {
	s.activateForInput(ctx)
	start := s.mousePos()
	size := 12.0
	if size < 1 {
		size = 1
	}
	end := pt{x: x, y: y}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	// 批次 #6：轨迹末尾加一段「轻微过冲 + 回拉」，按下前不再精确刹车。
	path := overshootPath(newBezierPath(start, end, size, rng), end, rng)
	for i, p := range path {
		if err := s.dispatchMouse(ctx, "mouseMoved", p, "none", 0); err != nil {
			return err
		}
		// 逐步抖动的间隔 ⇒ 速度非匀速；也避免 Chrome 把「同时到达」的移动合并成几跳。
		if i != len(path)-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pointerStepDelay(rng)):
			}
		}
	}
	if err := s.dispatchMouse(ctx, "mousePressed", pt{x: x, y: y}, "left", 1); err != nil {
		return err
	}
	if err := s.dispatchMouse(ctx, "mouseReleased", pt{x: x, y: y}, "left", 1); err != nil {
		return err
	}
	s.mu.Lock()
	s.lastMouse = pt{x: x, y: y}
	s.mouseInit = true
	s.mu.Unlock()
	// 动作后重启 DOM 突变采样窗口（引擎在 NAVIGATING 下读取）。
	s.startMutationSampling()
	return nil
}
