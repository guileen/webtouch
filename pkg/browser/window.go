// uws 窗口与焦点策略（2026-09-11；同指纹共享窗口作默认：2026-09-24）
//
// 问题：默认 `Target.createTarget{newWindow:false}` 会把新标签插进现有 Chrome
// 窗口并抢走系统焦点（Chrome 152 实测：新 Target 所在页 hasFocus=true），
// 每次物理输入前的 `Target.activateTarget` 又把窗口反复抬到前台——用户在其他
// 窗口打字会被打断（现场描述「我正在打字，他跳出个页面」）。
//
// 方案（Chrome 152 实测数据）：附着模式每个会话在自己的小窗里执行
//
//	Target.createTarget{newWindow:true, background:true, focus:false, width, height}
//
// 实测该小窗 visibilityState=visible、hasFocus=false、物理点击 12ms 正常
// （对照组 newWindow:false 的标签 hasFocus=true 抢焦点）；
// 即使 Tab 不可见（Input.dispatch* 可能挂起到超时），也绝不以
// `Target.activateTarget` 作为兜底；R1 要求后台自动化不能打断用户的前台工作。
//
// 环境变量（均可选，默认即推荐值；非法值回落默认，不因拼错让会话失败）：
//
//	UWS_WINDOW_MODE=shared|window|tab  默认 shared（同指纹一扇受管窗口，见下）
//	                    window=每会话一扇专属小窗（零前置）；tab=旧行为（复用现有窗口，禁止作默认）
//	UWS_WINDOW_SIZE=WxH            默认 1440x900（正常网页尺寸；超出工作区时夹取）
//	UWS_WINDOW_POS=auto            默认：工作区内可预期固定位（右上角留 24 DIP 边距）
//	               offscreen|park  显式要求停到可见屏幕之外（注意 macOS 会夹取，见下）
//	               X,Y             指定落点；none|keep=不动位置
//	UWS_INPUT_ACTIVATE=auto|always|never  兼容保留；所有取值均不允许自动前台化
//
// 默认几何（2026-09-24 用户定案）：默认把专属窗口摆在工作区**右上角**、尺寸夹进工作区
// （min(1440,宽-2*边距) × min(900,高-2*边距)），并**读回实际矩形**写日志——读回值才是
// 生效值；工作区读不到时不搬窗（保持 Chrome 给的默认位）并告警。
// 旧默认是「停到屏外、屏幕只留观看小窗」（2026-09-15）；实测 macOS 会把越界请求夹成
// 角落一条边（请求 (20000,60) 落到 (1730,-41)），用户既看不见也找不到，故退为显式选项
// `UWS_WINDOW_POS=offscreen`，判据与取证见 pkg/browser/offscreen_stream_e2e_test.go 与
// docs/evidence/pilots/window-tab-governance-2026-09-24.md。
//
// 焦点事实（Chrome 153 实测，2026-09-24）：建窗、搬窗、页面内派发输入都**不会**把 Chrome
// 抬到前台（前台 App 不变，只有页面 hasFocus 变化）；唯一会抢一次焦点的是「往某个窗口里
// 新建 Tab」。故本文件的策略里没有任何 activateTarget 调用。
//
// 默认模式（2026-09-24 用户定案）：`shared` —— 同一个浏览器指纹（attach 端点 + profile）
// 只开**一扇**受管窗口，后续会话的 Tab 都开进那扇窗（实现见 shared_window.go）。代价是
// 「开 Tab 会把该窗口前置一次」，这是 ①「一指纹一窗」优先于「零前置」的已定取舍；
// 要换回「每会话一扇专属小窗、零前置」用 `UWS_WINDOW_MODE=window`。
package browser

import (
	"os"
	"strconv"
	"strings"
)

const (
	// windowModeShared 同指纹共享一扇受管窗口（默认，见文件头）。
	windowModeShared = "shared"
	windowModeWindow = "window"
	windowModeTab    = "tab"

	activateAuto   = "auto"
	activateAlways = "always"
	activateNever  = "never"

	// 调试执行窗口是页面的**排版尺寸**：窗口多宽，页面就按多宽排版。默认取正常网页
	// 尺寸（1440x900），否则移动端/响应式页面按小窗宽度重排、展示不全（2026-09-22 用户
	// 现场：AutoDL 控制台页面展示不全）。
	//
	// 窗口默认停在可见屏幕之外（见 UWS_WINDOW_POS 与 parkOwnWindow），所以放大这个默认值
	// 不会在用户工作时占屏幕；用户点开小窗时引擎把这扇窗搬到展开几何再置前，尺寸由壳决定。
	//
	// 历史：2026-09-11 曾用 360x540（后因 macOS Chrome 宽度下限实测 500 调整为 500x540），
	// 当时假设「小窗=临时载体、越小越不打扰」；实测表明小窗宽度直接决定页面排版，故改回正常尺寸。
	defaultWindowWidth  = 1440
	defaultWindowHeight = 900

	// 停靠目标：往右外侧请求一个远超任何显示器宽度的位置。窗口仍保持
	// windowState=normal（不最小化）：最小化会改变渲染/输入语义，停靠只挪位置。
	parkWideLeft = 20000
	parkWideTop  = 60
)

// windowPolicy 是一次会话的窗口/焦点执行策略（env 解析结果）。
type windowPolicy struct {
	mode          string // shared | window | tab
	width, height int
	left, top     int    // 仅在 autoPosition=false 且显式指定时有效
	autoPosition  bool   // true = 贴屏幕右上角（显式可见）
	park          bool   // true = 停到可见屏幕之外（默认）
	inputActivate string // auto | always | never
}

// resolveWindowPolicy 从环境变量解析窗口策略。
func resolveWindowPolicy() windowPolicy {
	p := windowPolicy{
		mode:          windowModeShared,
		width:         defaultWindowWidth,
		height:        defaultWindowHeight,
		left:          -1,
		top:           -1,
		autoPosition:  true, // 默认：工作区内可预期固定位（2026-09-24）
		park:          false,
		inputActivate: activateNever,
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("UWS_WINDOW_MODE"))) {
	case windowModeWindow:
		p.mode = windowModeWindow
	case windowModeTab, "off", "0", "false":
		p.mode = windowModeTab
	default:
		// 缺省即 shared（同指纹一窗多 Tab）；也含显式 "shared" 与非法值的回落。
		p.mode = windowModeShared
	}
	if w, h, ok := parseSize(os.Getenv("UWS_WINDOW_SIZE")); ok {
		p.width, p.height = w, h
	}
	switch pos := strings.ToLower(strings.TrimSpace(os.Getenv("UWS_WINDOW_POS"))); pos {
	case "", "auto", "visible", "corner":
		p.park, p.autoPosition = false, true // 默认：工作区内可预期固定位
	case "offscreen", "park", "hidden":
		p.park, p.autoPosition = true, false // 显式要求：停到屏幕外（macOS 可能夹取，见文件头）
	case "none", "keep":
		p.park, p.autoPosition = false, false
	default:
		if x, y, ok := parsePos(pos); ok {
			p.left, p.top, p.park, p.autoPosition = x, y, false, false
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("UWS_INPUT_ACTIVATE"))) {
	case activateAlways, "1", "true", "on":
		p.inputActivate = activateAlways
	case activateAuto:
		p.inputActivate = activateAuto
	case activateNever, "0", "false", "off":
		p.inputActivate = activateNever
	default:
		// 保持 fail-closed 的 never 默认；auto/always 仅为显式兼容输入，
		// 也不会改变 activateForInput 的 R1 no-op 语义。
	}
	return p
}

// parseSize 解析 "WxH"（兼容 "W*H" / "W,H" / "W H"），非法返回 ok=false。
func parseSize(s string) (int, int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, 0, false
	}
	s = strings.NewReplacer("*", "x", ",", "x", " ", "").Replace(s)
	parts := strings.Split(s, "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// parsePos 解析 "X,Y"（兼容 "XxY"），非法返回 ok=false。
func parsePos(s string) (int, int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, 0, false
	}
	s = strings.NewReplacer("x", ",", " ", "").Replace(s)
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	x, err1 := strconv.Atoi(parts[0])
	y, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || x < 0 || y < 0 {
		return 0, 0, false
	}
	return x, y, true
}

// fitRightTopInWorkArea 把「想要的窗口尺寸」夹进工作区，并给出右上角（留 margin 边距）落点。
// 纯函数：几何判据不经真机也能测。工作区不可用或尺寸非法时返回 ok=false（调用方不搬窗）。
//
// 夹取顺序（2026-09-24）：只把**超出工作区**的尺寸收窄/收矮（工作区减两侧边距），再兜底
// 保证整块窗口落在工作区内——宁可靠边也不能有一角在屏幕外（那正是用户「找不到窗口」的成因）。
// 显式请求的尺寸（UWS_WINDOW_SIZE）不被改大改小；低于 Chrome 自身下限时由浏览器自己夹。
func fitRightTopInWorkArea(work screenWorkArea, wantW, wantH, margin int) (ViewerWindowBounds, bool) {
	if work.Width <= 0 || work.Height <= 0 || wantW <= 0 || wantH <= 0 {
		return ViewerWindowBounds{}, false
	}
	if margin < 0 {
		margin = 0
	}
	w, h := wantW, wantH
	if maxW := work.Width - 2*margin; w > maxW {
		w = maxW
	}
	if maxH := work.Height - 2*margin; h > maxH {
		h = maxH
	}
	if w > work.Width {
		w = work.Width
	}
	if h > work.Height {
		h = work.Height
	}
	left := work.Left + work.Width - w - margin
	if left < work.Left {
		left = work.Left
	}
	top := work.Top + margin
	if top+h > work.Top+work.Height {
		top = work.Top + work.Height - h
	}
	if top < work.Top {
		top = work.Top
	}
	return ViewerWindowBounds{Left: left, Top: top, Width: w, Height: h}, true
}

// needForegroundActivation 永远返回 false。它保留为纯函数以锁住 R1：页面隐藏、
// 可见性探测失败，或遗留环境变量请求 always，都不能让自动化抬起 Chrome 窗口。
func needForegroundActivation(_, _ string, _ error) bool {
	return false
}

// screenWorkArea 是一块显示器的工作区（Chrome/CDP 窗口坐标空间：DIP，原点在主屏左上角，
// y 向下，含多屏偏移）。
type screenWorkArea struct {
	Left, Top, Width, Height int
}

// intersects 判断窗口矩形是否与工作区相交。贴边也算相交：压住 1px 用户就看得见。
// 读回的尺寸缺失时按 1x1 处理，宁严不松。
func (a screenWorkArea) intersects(left, top, width, height int) bool {
	if width <= 0 {
		width = 1
	}
	if height <= 0 {
		height = 1
	}
	return left < a.Left+a.Width && left+width > a.Left &&
		top < a.Top+a.Height && top+height > a.Top
}

// parkedOutsideWorkArea 是「停靠成功」的判据：读回的窗口矩形与工作区完全不相交。
func parkedOutsideWorkArea(work screenWorkArea, b ViewerWindowBounds) bool {
	return !work.intersects(b.Left, b.Top, b.Width, b.Height)
}

// visibleInWorkArea 判断窗口矩形是否与工作区相交（相交 = 用户看得见）。
func visibleInWorkArea(area ViewerWindowBounds, b ViewerWindowBounds) bool {
	work := screenWorkArea{Left: area.Left, Top: area.Top, Width: area.Width, Height: area.Height}
	return work.intersects(b.Left, b.Top, b.Width, b.Height)
}
