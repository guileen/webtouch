package window

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
)

// ── 真窗口（观看壳「展开」）域 ────────────────────────────────────────────────
//
// 展开态 = 真页面（2026-09-12 用户口径「我要能点、能滚、能过人机验证，但不要浏览器
// 窗口那套 UI，只要页面载体本身」）。「展开」不是把图片放大，而是把**这个会话自己的
// 那扇真窗口**抬到最前：用户摸到的就是真页面的真渲染，输入链路一步都不少。
//
// 2026-09-22 现场口径：**不搬位置、不缩放**。位置控制在真机上是缺陷源（#113：停靠被
// 显示器布局夹回可见区，读回 left=1496 只剩 40px 边），用户明确接受「不用控制窗口位
// 置」。唯一的例外：窗口整块停在屏外（引擎默认把真窗口停在 parkWideLeft=20000）——
// 那时候「抬到最前」等于「点开什么也没有」，按工作区左上角把它搬进屏内，并记下原位。
//
// 边界（R1/R5）：这里只动这一个会话自己的窗口，不动任何别的窗口、不注入页面、不代替
// 用户点击；抬窗口的时机只能是用户点小窗这一个手势（壳发来的 expand 意图）。
//
// 状态归属（2026-09-29 复杂度治理·阶段 2·单元 3c 裁定）：**救援前的原位与「是否救援过」
// 留宿主会话**（经 ViewerSession 的 RememberWindow/TakeSavedWindow 进出，加锁语义原样），
// 生命周期事实（无头/存活/浏览器进程号）同样留宿主——本文件只承载流程与 CDP 命令。

// ErrNoViewerWindow 表示这个会话没有可搬动的真窗口（无头会话）。
var ErrNoViewerWindow = errors.New("browser: session has no movable window")

// Bounds 是一扇真窗口的位置与大小，坐标空间与 CDP
// Browser.setWindowBounds 一致：DIP，原点是主屏左上角，y 向下。
type Bounds struct {
	Left   int
	Top    int
	Width  int
	Height int
}

// Size 是壳发来的展开尺寸。**新口径下不再使用**（展开 = 抬到最前，不缩放）；
// 保留是为了不动 wire 协议：老壳仍会发，引擎忽略它。
type Size struct {
	Width  int
	Height int
}

// DefaultMargin 是小窗贴屏幕边缘的间距（DIP）；屏外救援时按「工作区左上角 + 本值」搬窗。
// 全仓唯一取值点（宿主 `window.go`/`cdp_session_core.go` 与测试都引用这里）。
const DefaultMargin = 24

// Caller 是浏览器级 CDP 通道（宿主的 *cdpClient 直接满足）；三个 `Browser.*` 窗口命令
// 由本包的 ReadBounds/SetBounds/SetState 承载，宿主与子包共用同一份命令形状。
type Caller interface {
	Call(ctx context.Context, method string, params, out any) error
}

// ViewerSession 是本域对宿主会话的**收窄索要面**：8 项，逐项对应实测出边，不发明能力。
type ViewerSession interface {
	// TargetID 是本会话在 CDP 里的页面 target。
	TargetID() string
	// Headless 报告本会话是否无头；无头会话没有真窗口可抬。
	Headless() bool
	// EvalValue 在页面里求值（读「当前显示器工作区」用）。
	EvalValue(ctx context.Context, expr string, dst any) error
	// WindowIDForTarget 读 target 所在浏览器窗口的 windowId（Browser.getWindowForTarget）。
	WindowIDForTarget(ctx context.Context) (int, error)
	// Call 发浏览器级 CDP 命令并解码回执。
	Call(ctx context.Context, method string, params, out any) error
	// Send 发浏览器级 CDP 命令，只取回执原文。
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
	// RememberWindow 记下救援前的位置；**首次生效**（已记过就是空操作）。
	RememberWindow(bounds Bounds)
	// TakeSavedWindow 取走并清空救援前位置；没救援过返回 ok=false。
	TakeSavedWindow() (Bounds, bool)
}

// Controller 由「拥有真窗口」的会话实现（有头 CDP 会话）；宿主以别名
// `ViewerWindowController` 暴露，故实现者与消费方都不必感知包边界。
type Controller interface {
	// RaiseViewerWindow 把这扇真窗口抬到最前：不搬位置、不缩放（2026-09-22 口径）。
	// 窗口整块停在屏外时按工作区左上角搬进屏内（唯一的例外，见文件头）。
	// 返回 Chrome 实际生效的矩形，壳据此把小窗「飞向它」的动画演到正确位置。
	// 老壳走这条（收到 expand 就地抬）；新壳走 Prepare + Activate 两步。
	RaiseViewerWindow(ctx context.Context) (Bounds, error)
	// PrepareViewerWindow 只把窗口摆好（含屏外救援）并回真实矩形，**不激活**
	// （2026-09-24 口径，规范 §4.4 第 3 条：壳飞完动画末段才抬）。
	PrepareViewerWindow(ctx context.Context) (Bounds, error)
	// ActivateViewerWindow 只做激活（CDP `Target.activateTarget`），不搬位置、不读矩形；
	// 由壳在动画末段发来的 IntentRaise 触发。
	ActivateViewerWindow(ctx context.Context) error
	// RestoreViewerWindow 把真窗口搬回抬前救援之前的位置；没救援过就是空操作。
	RestoreViewerWindow(ctx context.Context) error
}

// Geometry 由「知道真窗口在哪儿」的会话实现（有头 CDP 会话）。观看壳在**第一次点击
// 之前**就要拿这扇窗的矩形去屏上认窗 → 认进程，否则附着模式（`--attach`，
// `BrowserPID()==0`）只能退回焦点判据：盖住真窗口但 Chrome 仍在前台时会被判成「看得
// 见」，小窗不出现（2026-09-24 用户现场，规范 §4.4 第 7 条）。只读。
type Geometry interface {
	// RealWindowBounds 返回目标那扇真窗口此刻的矩形；拿不到（无头会话 / 没有窗口 /
	// CDP 失败）时 false，调用方保持「不知道」，由壳退回焦点判据。
	RealWindowBounds(ctx context.Context) (Bounds, bool)
}

// RealBounds 实现 Geometry：读 `Browser.getWindowForTarget` + `Browser.getWindowBounds`，
// 只读、不动窗口。无头会话没有窗口，返回 false。
func RealBounds(ctx context.Context, s ViewerSession) (Bounds, bool) {
	if s.Headless() {
		return Bounds{}, false
	}
	windowID, err := s.WindowIDForTarget(ctx)
	if err != nil {
		return Bounds{}, false
	}
	bounds, _, err := ReadBounds(ctx, s, windowID)
	if err != nil {
		return Bounds{}, false
	}
	return bounds, true
}

// Raise 实现 Controller.RaiseViewerWindow：把这扇真窗口抬到最前（不搬位置、不缩放，
// 2026-09-22 口径）。
//
// 只有一种情况会动窗口：它整块停在屏外（引擎默认停靠 parkWideLeft=20000，真机读回
// 只剩「夹回可见区」留下的 40px 边，与看不见没有区别）。那时候按工作区左上角把它搬
// 进屏内，并记下原位供收回时还原——「点开一定要看得见真页面」优先于「绝不动窗口」。
func Raise(ctx context.Context, s ViewerSession) (Bounds, error) {
	applied, err := Prepare(ctx, s)
	if err != nil {
		return applied, err
	}
	// 抬不起来不算失败：窗口本身就在，用户点一下 Dock 里的 Chrome 也能用。
	_ = Activate(ctx, s)
	log.Printf("browser: 观看壳抬到最前（就地激活·老壳路径）生效=%+v", applied)
	return applied, nil
}

// Prepare 实现 Controller.PrepareViewerWindow：把窗口摆到「点开就能用」的状态——必要时
// 先把最小化/全屏状态归位、把整块停在屏外的窗口救援进工作区——然后回真实矩形。**不激活**：
// 2026-09-24 现场口径是「动画末段才让窗口到前台」，激活那一步留给 Activate。
func Prepare(ctx context.Context, s ViewerSession) (Bounds, error) {
	if s.Headless() {
		return Bounds{}, ErrNoViewerWindow
	}
	windowID, err := s.WindowIDForTarget(ctx)
	if err != nil {
		return Bounds{}, fmt.Errorf("browser: viewer window id: %w", err)
	}
	current, state, err := ReadBounds(ctx, s, windowID)
	if err != nil {
		return Bounds{}, fmt.Errorf("browser: read window bounds: %w", err)
	}
	if state != "" && state != "normal" {
		// 最小化/全屏的窗口抬不到前面：activateTarget 对它没作用，用户还是看不见。
		if err := SetState(ctx, s, windowID, "normal"); err != nil {
			return Bounds{}, fmt.Errorf("browser: normalise window state: %w", err)
		}
	}
	applied := current
	if rescued, ok := rescue(ctx, s, windowID, current); ok {
		applied = rescued
	}
	if readBack, _, err := ReadBounds(ctx, s, windowID); err == nil {
		applied = readBack
	}
	log.Printf("browser: 观看壳摆好真窗口（未激活，等壳末段通知）window=%d 原位=%+v 生效=%+v", windowID, current, applied)
	return applied, nil
}

// Activate 实现 Controller.ActivateViewerWindow：把这扇窗口里的目标 Tab/窗口设为活动。
// 台前化本身由壳按 browserPID 那一步负责（§4.4 第 3 条），这里只管 Chrome 内部激活。
func Activate(ctx context.Context, s ViewerSession) error {
	if s.Headless() {
		return ErrNoViewerWindow
	}
	if _, err := s.Send(ctx, "Target.activateTarget", map[string]any{"targetId": s.TargetID()}); err != nil {
		log.Printf("browser: viewer raise activate: %v", err)
		return fmt.Errorf("browser: activate viewer target: %w", err)
	}
	log.Printf("browser: 观看壳末段激活 target=%s", s.TargetID())
	return nil
}

// rescue 处理「真窗口整块停在屏外」这一种例外：按工作区左上角把它搬进屏内（尺寸不动），
// 并记下原位。返回搬完的矩形与是否真的搬了。
func rescue(ctx context.Context, s ViewerSession, windowID int, current Bounds) (Bounds, bool) {
	area, ok := workArea(ctx, s)
	if !ok || VisibleEnough(area, current) {
		return current, false
	}
	target := Bounds{
		Left:   area.Left + DefaultMargin,
		Top:    area.Top + DefaultMargin,
		Width:  min(current.Width, area.Width),
		Height: min(current.Height, area.Height),
	}
	if target.Width <= 0 || target.Height <= 0 {
		return current, false
	}
	if err := SetBounds(ctx, s, windowID, target); err != nil {
		// 搬不动就照原样抬：至少不比不试更糟（窗口本来就在屏外）。
		log.Printf("browser: 观看壳抬前救援搬窗失败: %v", err)
		return current, false
	}
	s.RememberWindow(current)
	log.Printf("browser: 真窗口停在屏外（%+v，工作区 %+v），已搬进屏内 %+v", current, area, target)
	return target, true
}

// VisibleEnough 报告这扇窗口是否有「够用户用的那一块」落在工作区里：可见面积不到一半
// 就算停在屏外。默认停靠（left=20000）被夹回可见区后只剩 40px 的边，那种「技术上相交」
// 不该算看得见（2026-09-22 现场批次 #2）。
func VisibleEnough(area, b Bounds) bool {
	if b.Width <= 0 || b.Height <= 0 {
		return false
	}
	left := max(area.Left, b.Left)
	top := max(area.Top, b.Top)
	right := min(area.Left+area.Width, b.Left+b.Width)
	bottom := min(area.Top+area.Height, b.Top+b.Height)
	if right <= left || bottom <= top {
		return false
	}
	return float64((right-left)*(bottom-top)) >= 0.5*float64(b.Width*b.Height)
}

// workArea 读页面报的「当前显示器工作区」（Chrome 窗口坐标空间，含多屏偏移）。
// 读不到就返回 ok=false，调用方按「不夹」处理。
func workArea(ctx context.Context, s ViewerSession) (Bounds, bool) {
	var v struct {
		Left   int `json:"left"`
		Top    int `json:"top"`
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	const expr = `({left: window.screen.availLeft, top: window.screen.availTop, width: window.screen.availWidth, height: window.screen.availHeight})`
	if err := s.EvalValue(ctx, expr, &v); err != nil || v.Width <= 0 || v.Height <= 0 {
		return Bounds{}, false
	}
	return Bounds{Left: v.Left, Top: v.Top, Width: v.Width, Height: v.Height}, true
}

// clampCoord 把坐标夹进 [low, high]；区间反了就取 low（窗口比工作区还大）。
// 当前无调用点（随 3c 原样搬运，不在本单元顺手删——另案清理）。
func clampCoord(value, low, high int) int {
	if high < low {
		high = low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// Restore 实现 Controller.RestoreViewerWindow：把「抬到最前」时为救援屏外位置而搬过的
// 窗口放回原位。抬前没搬过窗口（新口径下的常态）就是空操作——展开已经不再改变窗口的
// 位置与大小，所以收回本来也没有窗口要动。
func Restore(ctx context.Context, s ViewerSession) error {
	saved, ok := s.TakeSavedWindow()
	if !ok || s.Headless() {
		return nil
	}
	windowID, err := s.WindowIDForTarget(ctx)
	if err != nil {
		return fmt.Errorf("browser: viewer window id: %w", err)
	}
	if err := SetBounds(ctx, s, windowID, saved); err != nil {
		return fmt.Errorf("browser: restore window after viewer: %w", err)
	}
	return nil
}

// ReadBounds 读回一个窗口的矩形与状态（Browser.getWindowBounds）。
func ReadBounds(ctx context.Context, c Caller, windowID int) (Bounds, string, error) {
	var res struct {
		Bounds struct {
			Left        int    `json:"left"`
			Top         int    `json:"top"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			WindowState string `json:"windowState"`
		} `json:"bounds"`
	}
	if err := c.Call(ctx, "Browser.getWindowBounds", map[string]any{"windowId": windowID}, &res); err != nil {
		return Bounds{}, "", err
	}
	return Bounds{
		Left:   res.Bounds.Left,
		Top:    res.Bounds.Top,
		Width:  res.Bounds.Width,
		Height: res.Bounds.Height,
	}, res.Bounds.WindowState, nil
}

// SetBounds 设置窗口矩形（只带位置与大小，状态保持 normal）。
func SetBounds(ctx context.Context, c Caller, windowID int, b Bounds) error {
	return c.Call(ctx, "Browser.setWindowBounds", map[string]any{
		"windowId": windowID,
		"bounds": map[string]any{
			"left":   b.Left,
			"top":    b.Top,
			"width":  b.Width,
			"height": b.Height,
		},
	}, nil)
}

// SetState 单独设置窗口状态（normal/minimized/maximized/fullscreen）。
func SetState(ctx context.Context, c Caller, windowID int, state string) error {
	return c.Call(ctx, "Browser.setWindowBounds", map[string]any{
		"windowId": windowID,
		"bounds":   map[string]any{"windowState": state},
	}, nil)
}
