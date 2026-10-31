package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
	"uws/pkg/browser/input"
	"uws/pkg/browser/modelbridge"
	"uws/pkg/browser/window"
	"uws/pkg/challenge"
)

// cdpSession 是真实 Chrome 页面的 CDP 会话。
type cdpSession struct {
	id       string
	cfg      Config
	headless bool

	client    *cdpClient
	sessionID string
	targetID  string
	httpBase  string // ws(s)://host:port 基底，构造页面级调试端点（真实 targetId）
	destroyed bool   // 标签页被关闭（用户手动关 Tab / Target.targetDestroyed）

	launched  bool
	ownWindow bool // 本会话跑在专属小窗里（window.go 策略；非 tab 复用模式）
	// sharedWindow 表示本会话的 Tab 开在「同指纹共享窗口」里（shared_window.go，
	// UWS_WINDOW_MODE=shared 默认）；sharedWindowID 是那扇窗口的 windowId，
	// sharedAnchorOwner 表示锚页窗是**本会话**建的（只有它负责按默认几何摆位）。
	sharedWindow      bool
	sharedWindowID    int
	sharedAnchorOwner bool
	// adopted 表示本会话是「接管一扇已经存在的窗口」而非新开的（会话续页，
	// GLOBAL #118）：接管时不重新停靠窗口，正是为了不打扰用户那扇干到一半的页面。
	adopted    bool
	chromeCmd  *exec.Cmd
	profileDir string
	// profilePersistent = 这个目录是持久身份（不删、要优雅退出落盘），
	// 而不是用完即焚的临时目录。
	profilePersistent bool
	// appWindow 表示这次启动用的是 Chrome 的 app 窗口（无标签栏/无地址栏，只有
	// 页面本身）。有头启动一律走这条路：起始窗即会话窗，不存在空白起始窗。
	appWindow bool
	// startupTargetID 是启动模式 Chrome 自己开出来的那扇 about:blank 起始窗；
	// 它不属于任何会话，专属会话窗就位后立即关闭（见 initTarget）。
	startupTargetID string
	// workerHookCancel 解除浏览器级 Worker 自动附着监听；它和 session 生命周期一致。
	workerHookCancel func()

	// 观看壳展开期保存的原位（2026-09-12）：真窗口被搬到展开几何后，用户切走时
	// 必须能搬回原处，否则屏幕上会留下一扇没人要的大窗口（见 viewer_window.go）。
	windowMu    sync.Mutex
	savedWindow ViewerWindowBounds
	windowSaved bool
	// viewerMetricsSuspended 记录展开期是否摘掉了设备指标覆盖（收回时要装回）。
	viewerMetricsSuspended bool

	mu              sync.Mutex
	closed          bool
	closedCh        chan struct{}
	navCnt          int
	cookies         []Cookie
	pushState       bool
	inflight        map[string]time.Time // XHR/Fetch requestId → 发起时间（TTL 剪枝）
	lastStatus      int
	lastHeaders     map[string]string
	challenge       bool
	challengeSignal *challenge.ChallengeSignal
	detector        *challenge.ChallengeDetector
	navFrameID      string // 最近一次 Page.navigate 的主框架 ID（loadFailed 归属判定）
	navErr          string // 主框架导航失败错误（下一次 Navigate 重置）

	newWindowTarget string        // 最近一次物理动作打开的新 Target（TDD-ENG-13，读后清零）
	lastHit         elementTarget // 最近一次定位成功的命中元素（v1.5.4 证据链回显）

	mutCount     int // 最近一次采样到的 MutationObserver 计数
	mutReturned  int // 已向引擎返回的计数
	mutSampling  bool
	mutSampleSeq int64

	// 页面候选缓存（TDD-OPT-01 多信号批量抽取）：同文档短窗口内复用最近一次
	// 探针采集结果，后续信号抽取仅做 Go 侧匹配；导航/已采样 DOM 突变/TTL
	// 到期后自动失效。
	docGen       uint64
	candCache    []PageCandidate
	candCacheGen uint64
	candCacheAt  time.Time
	candBaseMut  int
	candHits     uint64

	lastMouse pt
	mouseInit bool

	// 输入域子包（uws/pkg/browser/input）的会话级组件（复杂度治理·阶段 2·单元 2b）：
	//   inputRetry 是「物理输入必达」护栏（重试计数在内，懒建于 cdp_session_input.go）；
	//   pointer 是指针拟人化组件（落点 rng、最近一次派发位置与静默判据、漂移 ticker）。
	inputRetry *input.RetryGuard
	pointer    *input.Pointer

	probeOnce  sync.Once
	probeErr   error
	probeB64   string
	probePath  string
	probeStats ProbeStats // 最近一次采集的截断/溢出状态（mu 保护）

	chat       *modelbridge.Stream // 聊天 SSE 捕获状态机（方案 1，独立锁）
	capture    *recipeCaptureSink  // T-25 S1 原始快照抓存（独立锁；nil = 未启用）
	screencast *screencastStream   // Page.startScreencast latest-frame 画面流

	// pointerObserver 是被动观看小窗的指针观察者（nil = 无观看者）。它只被通知
	// 「刚才在哪里发生了哪种指针事件」，不参与输入决策，也不得回写页面（mu 保护）。
	pointerObserver PointerObserver

	// preDispatchSink 是引擎侧的「派发前受体回传」回调（nil = 无订阅者；mu 保护）。
	// 语义与 pointerObserver **严格相反**：它必须在受体已解析、物理事件尚未发出
	// （disp.mark() 之前）时被调用一次，供引擎把物理受体作为 is_ground_truth 的
	// 唯一锚点（spec/algorithm-kernel.md §5.0.14 决议 0）。fail-soft：实现只透传，
	// 回调既不得改变决策与派发，也不得回写页面。
	preDispatchSink func(PreDispatchReceptor)
}

// TargetID 返回**当前作用**的真实 CDP targetId（browser.TargetDebugInfo；供引擎
// 暴露给客户端调试/对拍，替代 Pool 内部 ID ctx-N）。新窗口接管（SwitchTarget）后
// 它会前移到新页——客户端手里的原钉扎值由 BrowserContext.PinnedTarget 记住。
func (s *cdpSession) TargetID() string { return s.targetID }

// OwnWindow 报告本会话是否跑在专属小窗里（own-window 策略）。引擎持久化
// 「会话名 → Target」绑定时记下这个值，续页时按同样策略接管（v1.5.7）。
func (s *cdpSession) OwnWindow() bool { return s.ownWindow }

// adoptTarget 校验并接管一扇已经存在的标签页（会话续页）：目标必须仍然是
// page 且不是空壳，attach 失败即视为不可用（引擎侧回退为新开窗口）。
// 不在这里导航——InitialURL 由既有流程统一处理，语义与内存态续页一致。
func (s *cdpSession) adoptTarget(ctx context.Context, targetID string) error {
	for _, info := range s.pageTargets(ctx) {
		if info.TargetID != targetID {
			continue
		}
		if strings.TrimSpace(info.URL) == "" {
			return fmt.Errorf("目标 %s 没有页面内容", targetID)
		}
		if _, err := s.client.attachTarget(ctx, targetID); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("目标 %s 已不存在", targetID)
}

// DebugPageURL 返回页面级 CDP 调试 WebSocket 端点
// （ws://host:port/devtools/page/<真实 targetId>；不可用时返回空串）。
func (s *cdpSession) DebugPageURL() string {
	if s.httpBase == "" || s.targetID == "" {
		return ""
	}
	return s.httpBase + "/devtools/page/" + s.targetID
}

// candCacheTTL 是页面候选缓存的最长有效窗口：爬虫/协调器在同一文档上连续
// 抽取多个信号时共享一次探针采集（信号间无交互、DOM 稳定）；超过该窗口
// 或采样到 DOM 突变即重新采集，避免 SPA/动态内容读到陈旧候选。
const candCacheTTL = 2 * time.Second

type pt struct{ x, y float64 }

// windowPolicy 返回本会话生效的窗口策略。R1 下任何有头会话一律使用
// background/focus=false 的窗口——或同指纹共享的那一扇（默认 shared，2026-09-24）；
// 旧 tab 模式会把 target 插入用户 Chrome 窗口，无法证明不抢焦点，因而不再作为
// 有头自动化的可选回退。无头浏览器没有窗口。
//
// Config.WindowMode（非空）优先于环境变量：给「同一次运行里两种模式并存」留口子
// （测试与验收用例必须显式指定才不会互相干扰）。shared 还需要一个可共享的浏览器
// 端点（attach）：没有就退化为每会话一扇专属小窗。
func (s *cdpSession) windowPolicy() windowPolicy {
	p := resolveWindowPolicy()
	switch mode := strings.ToLower(strings.TrimSpace(s.cfg.WindowMode)); mode {
	case windowModeShared, windowModeWindow, windowModeTab:
		p.mode = mode
	}
	if s.headless {
		p.mode = windowModeTab
		return p
	}
	if p.mode == windowModeTab {
		// 有头 + tab = 把标签塞进用户窗口（实测会落进用户自己的窗口），不接受。
		p.mode = windowModeWindow
	}
	if p.mode == windowModeShared {
		if _, ok := sharedWindowKey(s.cfg); !ok {
			p.mode = windowModeWindow
		}
	}
	return p
}

// placeOwnWindow 决定专属窗口落在哪里：
//
//	默认（policy.park）       → 停到可见屏幕之外，屏幕上只留观看小窗
//	UWS_WINDOW_POS=auto       → 贴当前屏幕右上角（显式可见）
//	UWS_WINDOW_POS=X,Y/none   → 指定落点 / 不动位置
func (s *cdpSession) placeOwnWindow(ctx context.Context, policy windowPolicy) {
	label := "专属窗口"
	if s.sharedWindow {
		label = "共享窗口"
	}
	if policy.park {
		s.parkOwnWindow(ctx)
		return
	}
	var bounds ViewerWindowBounds
	switch {
	case policy.autoPosition:
		work, ok := s.mainScreenWorkArea(ctx)
		if !ok {
			log.Printf("browser: 读不到工作区，%s保持 Chrome 默认位（不下发默认几何）", label)
			return
		}
		fit, ok := fitRightTopInWorkArea(work, policy.width, policy.height, window.DefaultMargin)
		if !ok {
			log.Printf("browser: 工作区 %+v 无法容纳默认几何，%s保持原位", work, label)
			return
		}
		bounds = fit
	default:
		if policy.left < 0 || policy.top < 0 {
			return
		}
		bounds = ViewerWindowBounds{
			Left: policy.left, Top: policy.top, Width: policy.width, Height: policy.height,
		}
	}
	windowID, err := s.client.windowIDForTarget(ctx, s.targetID)
	if err != nil {
		return
	}
	if err := s.client.setWindowBounds(ctx, windowID, bounds); err != nil {
		log.Printf("browser: 摆放%s失败（%v），窗口保持原位", label, err)
		return
	}
	// 读回才是生效值：macOS 会夹取越界请求，请求值不等于落点（GLOBAL #110）。
	got, state, err := s.client.windowBounds(ctx, windowID)
	if err != nil {
		log.Printf("browser: 默认几何已下发但读回失败（%v）：请求 %+v", err, bounds)
		return
	}
	log.Printf("browser: %s落位 请求 left=%d top=%d %dx%d → 读回 left=%d top=%d %dx%d state=%s",
		label, bounds.Left, bounds.Top, bounds.Width, bounds.Height,
		got.Left, got.Top, got.Width, got.Height, state)
}

// parkOwnWindow 把专属窗口停到可见屏幕之外，并**读回实际落点**再判定是否真的停住了。
//
// macOS 会夹取越界请求（实测请求 (20000,60) 落到 (1730,-41)，见 GLOBAL #110 与
// offscreen_stream_e2e_test.go），所以「请求值生效」不能当前提：读回的矩形若仍与
// 显示器工作区相交，就承认停靠失败——把真窗口留在原地（宁可看得见，也不能让用户
// 面对一扇找不到的窗口），并把事实写进日志，便于按显示器布局重新判定。
// 停靠只挪位置、不最小化：最小化会改变渲染/输入语义（未取证），位置不会。
func (s *cdpSession) parkOwnWindow(ctx context.Context) {
	windowID, err := s.client.windowIDForTarget(ctx, s.targetID)
	if err != nil {
		return
	}
	work, hasWork := s.mainScreenWorkArea(ctx)
	if err := s.client.moveWindow(ctx, windowID, parkWideLeft, parkWideTop); err != nil {
		log.Printf("browser: 停靠屏外失败（setWindowBounds: %v），窗口保持原位", err)
		return
	}
	bounds, state, err := s.client.windowBounds(ctx, windowID)
	if err != nil {
		log.Printf("browser: 停靠屏外后读回窗口位置失败（%v）：无法证实是否真的看不见", err)
		return
	}
	if hasWork && !parkedOutsideWorkArea(work, bounds) {
		log.Printf("browser: ⚠️ 停靠被显示器布局夹回可见区（读回 left=%d top=%d %dx%d state=%s，工作区 %+v）："+
			"真窗口仍在屏幕上；UWS_WINDOW_POS=auto 可显式改为贴右上角",
			bounds.Left, bounds.Top, bounds.Width, bounds.Height, state, work)
		return
	}
	log.Printf("browser: 真窗口已停靠屏外（请求 (%d,%d) → 读回 left=%d top=%d %dx%d state=%s）",
		parkWideLeft, parkWideTop, bounds.Left, bounds.Top, bounds.Width, bounds.Height, state)
}

// mainScreenWorkArea 读页面报的「当前显示器工作区」（坐标空间与 CDP 窗口矩形一致）；
// 读不到返回 ok=false，调用方按「无法证实」处理。
func (s *cdpSession) mainScreenWorkArea(ctx context.Context) (screenWorkArea, bool) {
	var v struct {
		Left   int `json:"left"`
		Top    int `json:"top"`
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	const expr = `({left: screen.availLeft, top: screen.availTop, width: screen.availWidth, height: screen.availHeight})`
	if err := s.evalValue(ctx, expr, &v); err != nil || v.Width <= 0 || v.Height <= 0 {
		return screenWorkArea{}, false
	}
	return screenWorkArea{Left: v.Left, Top: v.Top, Width: v.Width, Height: v.Height}, true
}

// activateForInput 保留调用点兼容性，但 R1 下是硬性 no-op：自动化不能通过
// Target.activateTarget 抬起 Chrome。隐藏页输入失败应由既有动作错误语义上报，
// 不能以打断用户前台工作作为恢复手段。
func (s *cdpSession) activateForInput(_ context.Context) {
}

// appWindowURL 返回 app 窗口打开的地址。空地址用 about:blank 占位，随后由
// Navigate 驱动真实的导航；app 窗口没有标签栏与地址栏，用户看到的就是页面本身。
func appWindowURL(initial string) string {
	if trimmed := strings.TrimSpace(initial); trimmed != "" {
		return trimmed
	}
	return "about:blank"
}

// appWindowTarget 取启动模式 app 窗口的页面 target。Chrome 是先起窗口再开页面，
// 刚连上调试端口时可能还看不到，所以短轮询一下；拿不到就返回空串，由调用方
// fail closed，不能偷偷降级成带标签栏/地址栏的普通窗口。
func (s *cdpSession) appWindowTarget(ctx context.Context) string {
	for attempt := 0; attempt < 60; attempt++ {
		if s.startupTargetID != "" {
			for _, info := range s.pageTargets(ctx) {
				if info.TargetID == s.startupTargetID {
					return info.TargetID
				}
			}
		}
		if id := s.startupPageTarget(ctx); id != "" {
			return id
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(50 * time.Millisecond):
		}
	}
	return ""
}

// pageTarget 是 Target.getTargets 里我们关心的字段（id / 类型 / 当前 URL）。
type pageTarget struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
}

// pageTargets 读取浏览器当前的页面 target。
func (s *cdpSession) pageTargets(ctx context.Context) []pageTarget {
	var res struct {
		TargetInfos []pageTarget `json:"targetInfos"`
	}
	if err := s.client.Call(ctx, "Target.getTargets", nil, &res); err != nil {
		return nil
	}
	out := make([]pageTarget, 0, len(res.TargetInfos))
	for _, info := range res.TargetInfos {
		if info.Type == "page" {
			out = append(out, info)
		}
	}
	return out
}

// startupPageTarget 读取启动模式 Chrome 自带的页面 target。优先接管与本次
// InitialURL 精确匹配的页面，其次接管 about:blank；多个不明页面时宁可失败，也
// 不把用户已有窗口误当成这次 app 窗口。附着模式没有起始窗，返回空串。
func (s *cdpSession) startupPageTarget(ctx context.Context) string {
	targets := s.pageTargets(ctx)
	if len(targets) == 0 {
		return ""
	}
	expected := strings.TrimSpace(s.cfg.InitialURL)
	if expected != "" {
		for _, info := range targets {
			if info.URL == expected {
				return info.TargetID
			}
		}
	}
	if expected == "" {
		blank := make([]string, 0, len(targets))
		for _, info := range targets {
			if info.URL == "" || strings.HasPrefix(info.URL, "about:") {
				blank = append(blank, info.TargetID)
			}
		}
		if len(blank) == 1 {
			return blank[0]
		}
	}
	if len(targets) == 1 {
		return targets[0].TargetID
	}
	return ""
}

// blankStartupTarget 只在起始窗确实是一块没人用的白板（空 URL / about:*）时才返回
// 它。2026-09-12 现场症状「一扇 Example.com 窗口闪了一下就消失」的成因就在这类误关：
// app 起始窗偶尔比调试端口晚一步出现，回落路径已经把会话开在另一扇窗里，于是那扇
// **正在显示真页面**的起始窗被当成了空白白板关掉。已经在显示真页面的窗口一律不碰。
func (s *cdpSession) blankStartupTarget(ctx context.Context) string {
	targets := s.pageTargets(ctx)
	urlOf := func(id string) (string, bool) {
		for _, info := range targets {
			if info.TargetID == id {
				return info.URL, true
			}
		}
		return "", false
	}
	candidates := make([]string, 0, len(targets)+1)
	if s.startupTargetID != "" {
		candidates = append(candidates, s.startupTargetID)
	}
	for _, info := range targets {
		if info.TargetID != s.targetID {
			candidates = append(candidates, info.TargetID)
		}
	}
	for _, id := range candidates {
		if id == s.targetID {
			continue
		}
		url, present := urlOf(id)
		if !present {
			continue // 已经不存在了（比如会话自己接管的就是它）
		}
		if url == "" || strings.HasPrefix(url, "about:") {
			return id
		}
	}
	return ""
}

// initTarget 创建并附着新标签页，启用所需域，应用 UA/时区/Cookie 覆盖。
func (s *cdpSession) initTarget(ctx context.Context) error {
	policy := s.windowPolicy()
	spec := targetSpec{}
	targetID := ""
	if s.launched && s.appWindow {
		// app 起始窗就是本会话的专属窗口：直接接管它，不再另开一扇普通窗口，也就
		// 没有多余的空白窗需要事后关闭。
		targetID = s.appWindowTarget(ctx)
		if targetID != "" {
			s.startupTargetID = ""
			log.Printf("browser: 接管 app 起始窗 target=%s（无标签栏/无地址栏）", targetID)
		} else {
			return fmt.Errorf("cdp: app 起始窗未在超时内出现，拒绝降级为普通浏览器窗口")
		}
	}
	if targetID == "" {
		// 会话续页（2026-09-22 用户现场：同一个任务反复开新窗口、占资源）：
		// 引擎把「会话名 → Target」落盘，重启失忆后仍指定原来的那扇窗口，
		// 这里接管它就等于「在已经干到一半的页面上接着干」。
		// 目标不可用时，普通可选复用允许开新页；持久 session 绑定则必须失败关闭，
		// 防止无 URL 的续页请求落到 Chrome 中另一张现存页面。
		if reuse := strings.TrimSpace(s.cfg.ReuseTargetID); reuse != "" {
			if err := s.adoptTarget(ctx, reuse); err != nil {
				// 持久绑定失效（页签被关 / Chrome 重启过）时默认失败关闭，避免续页
				// 请求悄悄落到浏览器里另一张现存页面。例外：调用方**显式给了
				// initial_url**，说明它接受「从这个地址重新开始」——此时回退新开窗口
				// 比整条链路 502 更合理；没给 initial_url 时行为一律不变。
				if s.cfg.RequireReuseTarget && strings.TrimSpace(s.cfg.InitialURL) == "" {
					return fmt.Errorf("cdp: bound session target %s cannot be adopted; refusing a different target: %w", reuse, err)
				}
				log.Printf("browser: 会话续页目标 %s 不可用（%v），改为新开窗口", reuse, err)
			} else {
				targetID = reuse
				s.adopted = true
				log.Printf("browser: 会话续页接管原窗口 target=%s（不新开窗口）", reuse)
			}
		}
	}
	if targetID == "" {
		switch policy.mode {
		case windowModeShared:
			// 同指纹共享窗口（默认）：把本会话的 Tab 开进那扇已注册的窗口，而不是每会话
			// 一扇新窗。共享路径的任何故障都**不阻塞会话**——记日志后退化为专属小窗。
			created, err := windowRegistry().OpenTab(ctx, bridgeWindowSession{s}, policy.width, policy.height)
			if err != nil {
				log.Printf("browser: 共享窗口开 Tab 失败（%v），本会话退化为专属小窗", err)
			} else {
				targetID = created
			}
		}
	}
	if targetID == "" {
		// 专属小窗：shared 退化路径与显式 window 模式都走这里（tab 模式不进分支，
		// 由 createTarget 的零值 targetSpec 表达「在现有窗口里开标签」）。
		if policy.mode == windowModeWindow || policy.mode == windowModeShared {
			noFocus := false
			spec = targetSpec{
				NewWindow:  true,
				Background: true,
				Focus:      &noFocus,
				Width:      policy.width,
				Height:     policy.height,
			}
		}
		created, err := s.client.createTarget(ctx, "about:blank", spec)
		if err != nil {
			return fmt.Errorf("cdp: create target: %w", err)
		}
		targetID = created
	}
	s.targetID = targetID
	s.ownWindow = spec.NewWindow || s.sharedWindow || s.appWindow || (s.adopted && s.cfg.ReuseOwnWindow)
	sessionID, err := s.client.attachTarget(ctx, targetID)
	if err != nil {
		return fmt.Errorf("cdp: attach target: %w", err)
	}
	s.sessionID = sessionID

	// 关掉启动模式自带的空白起始窗（2026-09-12 用户现场反馈：一扇原尺寸的
	// about:blank 空窗一直挡在眼前）。只有在我们确实另开了专属窗口、且起始窗
	// 不是会话窗本体时才动它——无头模式 / tab 复用模式下那可能是唯一窗口。
	if s.launched && s.ownWindow {
		// 只关「确实还是白板」的那一扇：已经在显示真页面的窗口一律留着（见
		// blankStartupTarget 的说明）。
		if blank := s.blankStartupTarget(ctx); blank != "" {
			_, _ = s.client.Send(ctx, "Target.closeTarget", map[string]any{"targetId": blank})
			log.Printf("browser: 关掉没人用的起始白板 target=%s", blank)
		}
		s.startupTargetID = ""
	}

	if err := s.enableTargetDomains(ctx, sessionID); err != nil {
		return err
	}
	// 专属小窗定位（2026-09-11）：贴屏幕右上角，尽量不压住用户正在看的窗口；
	// 定位失败不阻塞会话（窗口留在 Chrome 给的默认位置）。
	// 续页接管来的窗口不重新停靠：用户可能已经把它挪到自己顺手的位置
	// （2026-09-22），再搬一次等于打断他正在看的那个页面。
	// 共享窗口（shared）只在**建窗的那一次**摆位：后续会话共用同一扇窗，再搬一次
	// 等于把用户挪过的窗口又拽回默认位（shared_window.go）。
	if s.ownWindow && !s.adopted && (!s.sharedWindow || s.sharedAnchorOwner) {
		s.placeOwnWindow(ctx, policy)
	}
	// 发现新 Target（target="_blank" / window.open），供 NEW_WINDOW_OPENED 判定。
	if _, err := s.client.Send(ctx, "Target.setDiscoverTargets",
		map[string]any{"discover": true}); err != nil {
		return fmt.Errorf("cdp: set discover targets: %w", err)
	}
	s.client.On("", "Target.targetCreated", func(ev cdpEvent) {
		var p struct {
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfo"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.TargetInfo.Type != "page" {
			return
		}
		s.mu.Lock()
		if p.TargetInfo.TargetID != s.targetID && s.newWindowTarget == "" {
			s.newWindowTarget = p.TargetInfo.TargetID
		}
		s.mu.Unlock()
	})
	// 标签页被关闭（用户手动关 Tab / 页面自身关闭）：标记 destroyed，
	// ExecuteAction 显式返回 target_destroyed，API 层可自动重建新 Tab。
	s.client.On("", "Target.targetDestroyed", func(ev cdpEvent) {
		var p struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		s.markTargetDestroyed(p.TargetID)
	})
	if err := s.applyTargetIdentity(ctx, sessionID); err != nil {
		return err
	}
	s.subscribe(ctx)
	return nil
}
