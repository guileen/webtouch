package engine

import (
	"context"
	"fmt"
	"sync"

	"uws/pkg/browser"
	"uws/pkg/challenge"
)

// simNavState 是模拟会话的可脚本化异步验证事件。
// pushState/mutated 为一次性事件（读取后清零），active 为持续电平（模拟活动连接数）。
type simNavState struct {
	pushState bool
	mutated   bool
	active    int
}

// SimCDPSession 是 browser.CDPSession 的模拟实现（零配置/无浏览器开发用），
// 记录导航与动作，供引擎服务端到端接线验证；真实 CDP 适配见 USER_ACTION.md。
type SimCDPSession struct {
	id      string
	cfg     browser.Config
	mu      sync.Mutex
	navCnt  int
	acts    []string
	cookies []browser.Cookie
	nav     simNavState
	pages   []browser.PageCandidate
	newWin  string
	// activeTarget/switches 模拟新窗口接管（browser.TargetSwitcher，TDD-ENG-14）：
	// 记录动作面已切到哪个 Target 以及切换轨迹，供引擎测试断言。
	activeTarget string
	switches     []string
	probe        browser.ProbeStats
	// challenge 模拟 WAF/验证码拦截（引擎 UNAUTHENTICATED 映射 + VLM 拖拽解锁流，TDD-COORD-04）。
	challenge bool
	// 聊天 SSE 快照/增量（browser.ChatStreamSource 模拟，方案 1 测试用）。
	chatSnap  browser.ChatStreamSnapshot
	chatDrain []browser.ChatStreamChunk
	// VLM 兜底模拟：当前页面 URL + 坐标点击记录（browser.VLMProbeSession）。
	pageURL     string
	lastSels    []string
	lastClip    []int              // 最近一次区域截图（x,y,w,h）
	lastHit     browser.HitElement // 最近一次命中元素证据（v1.5.4 HitElementSource）
	hitScripted bool               // lastHit 是否由测试脚本化（true 时合成值不覆盖它）
	navOnAction string             // 动作执行时自动跳转的目标 URL（模拟点击导航，测试用）
	nodeTree    string             // ExportPageTree 脚本化返回值（v1.5.5 混合指挥）
	// snapSet/snap 模拟页面侧只读快照通道（browser.PageSnapshotSource，§5.0.13 条款 3）。
	// 未脚本化时 PageSnapshot 返回 false ⇒ 引擎退回候选池口径（既有测试口径不变）。
	snapSet bool
	snap    browser.PageSnapshot
	// snapSeq 是「按调用顺序消费」的快照序列（§5.0.14 决议 9 空集重试的单测口；耗尽后回落 snap）。
	snapSeq []browser.PageSnapshot
	// snapPivot/snapCalls 记录派发前采样传入的枢轴与调用次数（§5.0.14 条款 1：测试断言用）。
	snapPivot browser.PageSnapshotPivot
	snapCalls int
	// preSink/preReceptors/preCalls/preLast 模拟「派发前受体回传」能力
	// （browser.PreDispatchReceptorSink，§5.0.14 决议 0）：模拟物理层在「受体已解析、
	// 物理事件未发出」时依次回调 sink；脚本化受体为空时回调零值受体（Found=false，
	// 引擎据此不采样，语义等同「物理层没解析出可锚受体」）。
	preSink      func(browser.PreDispatchReceptor)
	preReceptors []browser.PreDispatchReceptor
	preCalls     int
	preLast      browser.PreDispatchReceptor
}

// NewSimCDPSession 构造一个模拟会话。
func NewSimCDPSession(id string, cfg browser.Config) *SimCDPSession {
	return &SimCDPSession{id: id, cfg: cfg}
}

// ID 返回会话 ID。
func (s *SimCDPSession) ID() string { return s.id }

// ExportState 模拟导出。
func (s *SimCDPSession) ExportState(ctx context.Context, targetOrigin string) (*browser.SerializedSessionState, error) {
	return &browser.SerializedSessionState{}, nil
}

// Probe 返回 UA/时区探活结果（TDD-ENG-01 语义）。
func (s *SimCDPSession) Probe() browser.ProbeResult {
	return browser.ProbeResult{UserAgent: s.cfg.UserAgent, Timezone: s.cfg.Timezone}
}

// Navigate 记录一次导航并返回成功。
func (s *SimCDPSession) Navigate(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.navCnt++
	return nil
}

// GoBack 模拟浏览器后退（browser.HistoryNavigator；VLM 主控 back 原语）。
// 记录一条动作便于测试断言，导航次数 +1（模拟历史前进/后退）。
func (s *SimCDPSession) GoBack(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.navCnt++
	s.acts = append(s.acts, "back")
	return nil
}

// SetCookie 写入/覆盖 Cookie。
func (s *SimCDPSession) SetCookie(c browser.Cookie) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cookies {
		if s.cookies[i].Name == c.Name {
			s.cookies[i] = c
			return
		}
	}
	s.cookies = append(s.cookies, c)
}

// Cookies 返回当前 Cookie 副本。
func (s *SimCDPSession) Cookies() []browser.Cookie {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]browser.Cookie(nil), s.cookies...)
}

// Close 无资源可释放。
func (s *SimCDPSession) Close() error { return nil }

// ExecuteAction 记录一次动作并返回成功（模拟 DOM 恒可用）。
// challenge=true 时非 drag 动作返回 challenge_intercepted（引擎映射 UNAUTHENTICATED）；
// drag 为挑战解锁原语，绕过挑战门禁（与真实 CDP 会话语义一致，v1.5.1）。
func (s *SimCDPSession) ExecuteAction(ctx context.Context, action, value, semantic string) (bool, string) {
	ok, observed := s.executeActionLocked(ctx, action, value, semantic)
	if ok {
		// §5.0.14 决议 0：模拟物理层在「受体已解析、事件未发出」时回传受体。必须在**锁外**
		// 回调——回调内会重入会话读锁做页面侧只读快照（相似地，真实会话也不持锁回调）。
		s.notifyPreDispatchReceptor()
	}
	return ok, observed
}

// executeActionLocked 是 executeAction 的持锁实体（模拟物理派发）。
func (s *SimCDPSession) executeActionLocked(ctx context.Context, action, value, semantic string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 模拟会话同样尊重调用方 ctx：中断时如实回报「未派发」（与真实 CDP 会话语义对齐，
	// 引擎的取消/结果未知语义才能在无浏览器环境下被测试）。
	if ctx.Err() != nil {
		return false, browser.ObservedCanceledBeforeDispatch
	}
	if s.challenge && action != "drag" {
		return false, "challenge_intercepted"
	}
	if action == "drag" {
		// 与真实 CDP 会话同语义：坐标非法不执行任何鼠标事件（v1.5.1）。
		if _, _, _, _, err := browser.ParseDragCoords(value); err != nil {
			return false, "invalid_drag_coords"
		}
	}
	s.acts = append(s.acts, fmt.Sprintf("%s|%s|%s", action, semantic, value))
	// v1.5.4 证据链：模拟会话默认回显合成命中元素（selector=sim:<action>:<semantic>），
	// 契约/引擎测试可直接断言 hit_element；测试可先 SetLastHit 脚本化「容器命中」场景，
	// 因此仅在未脚本化时写入合成值。URL 变化用 navOnAction 脚本化。
	// 合成命中元素每次动作都刷新（真实物理层每条命中都会重写 lastHit）；测试用
	// SetLastHit 脚本化的证据保持不覆盖，直到测试再次改写——否则第二次动作会沿用
	// 上一条语义的陈旧文本，落点自证（§5.0.9 条款 6/7）会据此误判。
	if !s.hitScripted {
		s.lastHit = browser.HitElement{
			Selector: fmt.Sprintf("sim:%s:%s", action, semantic),
			Tag:      "div",
			Text:     semantic,
		}
	}
	if s.navOnAction != "" {
		s.pageURL = s.navOnAction
	}
	return true, fmt.Sprintf("%s:%s", action, semantic)
}

// SetLastHit 脚本化最近一次命中元素（引擎证据回显测试用）。
func (s *SimCDPSession) SetLastHit(h browser.HitElement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastHit = h
	s.hitScripted = true
}

// LastHitElement 返回最近一次命中元素证据（browser.HitElementSource）。
func (s *SimCDPSession) LastHitElement() (browser.HitElement, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastHit, s.lastHit.Selector != ""
}

// SetNavOnAction 脚本化「动作执行时页面跳转到指定 URL」（模拟点击导航，测试用）。
func (s *SimCDPSession) SetNavOnAction(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.navOnAction = u
}

// ExecuteActionWithSelector 记录选择器并执行（browser.SelectorActionSession 模拟；
// 真实 CDP 会话由 findElementOpt 优先 querySelectorAll(selector)）。
func (s *SimCDPSession) ExecuteActionWithSelector(ctx context.Context, action, value, semantic, selector string) (bool, string) {
	ok, observed := s.executeActionWithSelectorLocked(ctx, action, value, semantic, selector)
	if ok {
		// §5.0.14 决议 0：与 ExecuteAction 同语义（锁外回传，供回调内只读快照重入）。
		s.notifyPreDispatchReceptor()
	}
	return ok, observed
}

// executeActionWithSelectorLocked 是 ExecuteActionWithSelector 的持锁实体。
func (s *SimCDPSession) executeActionWithSelectorLocked(ctx context.Context, action, value, semantic, selector string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.challenge && action != "drag" {
		return false, "challenge_intercepted"
	}
	s.acts = append(s.acts, fmt.Sprintf("%s|%s|%s|sel=%s", action, semantic, value, selector))
	return true, fmt.Sprintf("%s:%s", action, semantic)
}

// ExecuteActionByNode 记录 node_id 确定性动作（browser.NodeActionSession 模拟，
// v1.5.5 混合指挥）；语义与 ExecuteAction 一致：challenge 门禁 + 合成命中证据。
func (s *SimCDPSession) ExecuteActionByNode(ctx context.Context, action, value string, nodeID int32) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.challenge && action != "drag" {
		return false, "challenge_intercepted"
	}
	s.acts = append(s.acts, fmt.Sprintf("node|%s|%d|%s", action, nodeID, value))
	if !s.hitScripted {
		s.lastHit = browser.HitElement{
			Selector: fmt.Sprintf("sim:node:%d", nodeID),
			Tag:      "div",
			Text:     fmt.Sprintf("node:%d", nodeID),
		}
	}
	if s.navOnAction != "" {
		s.pageURL = s.navOnAction
	}
	return true, fmt.Sprintf("%s:node:%d", action, nodeID)
}

// SetNodeTree 脚本化 ExportPageTree 返回的极简 AXTree（v1.5.5 测试用）。
func (s *SimCDPSession) SetNodeTree(tree string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodeTree = tree
}

// ExportPageTree 返回脚本化的极简 AXTree（browser.PageTreeExporter 模拟；
// 未脚本化时返回默认样例，供契约/引擎测试断言 RPC 通路）。
func (s *SimCDPSession) ExportPageTree(_ context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodeTree != "" {
		return s.nodeTree, nil
	}
	return "[1] button(button) \"立即购买\"\n[2] input(textbox) \"验证码\"\n", nil
}

// EvaluateScript 模拟 In-Page Direct 执行返回成功 JSON。
func (s *SimCDPSession) EvaluateScript(_ context.Context, _ string) ([]byte, error) {
	return []byte(`{"status":"ok","direct_transport":true}`), nil
}

// SetPageURL 脚本化当前页面 URL（VLM 学习存储键，测试用）。
func (s *SimCDPSession) SetPageURL(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageURL = u
}

// PageURL 返回当前页面 URL（browser.VLMProbeSession）。
func (s *SimCDPSession) PageURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pageURL
}

// Executed 返回已物理执行的动作次数（测试/审计用）。
func (s *SimCDPSession) Executed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.acts)
}

// ClickAt 记录坐标点击并返回模拟 CSS 路径（browser.VLMProbeSession）。
func (s *SimCDPSession) ClickAt(ctx context.Context, x, y int) (string, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.challenge {
		return "", false, "challenge_intercepted"
	}
	s.acts = append(s.acts, fmt.Sprintf("click_at|%d,%d", x, y))
	sel := fmt.Sprintf("sim:click:%d,%d", x, y)
	s.lastSels = append(s.lastSels, sel)
	return sel, true, "click_at"
}

// InputAt 记录坐标就近输入动作并返回模拟 CSS 路径（browser.VLMProbeSession，
// 2026-08-31 input 兜底：VLM 报输入框坐标 → 引擎就近聚焦填值）。
func (s *SimCDPSession) InputAt(ctx context.Context, x, y int, value, semantic string) (string, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.challenge {
		return "", false, "challenge_intercepted"
	}
	s.acts = append(s.acts, fmt.Sprintf("input_at|%d,%d|%s", x, y, value))
	sel := fmt.Sprintf("sim:input:%d,%d", x, y)
	s.lastSels = append(s.lastSels, sel)
	return sel, true, "input_at"
}

// ExtractAt 返回坐标处命中元素的确定性选择器 + 模拟文本（browser.VLMProbeSession，
// v1.5.5 自进化：修复 extract 只写 NodeText 的假闭环，测试用）。
func (s *SimCDPSession) ExtractAt(ctx context.Context, x, y int) (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.challenge {
		return "", "", false
	}
	sel := fmt.Sprintf("sim:extract:%d,%d", x, y)
	s.acts = append(s.acts, fmt.Sprintf("extract_at|%d,%d", x, y))
	return sel, "模拟提取文本", true
}

// LastClickSelectors 返回坐标点击返回过的 CSS 路径（测试断言用）。
func (s *SimCDPSession) LastClickSelectors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lastSels...)
}

// SetChallenge 脚本化 WAF/验证码拦截状态（测试用；真实会话由 Document 响应重算）。
func (s *SimCDPSession) SetChallenge(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.challenge = v
}

// CaptureScreenshot 返回确定性模拟截图字节（browser.Screenshotter 接口；
// 真实会话经 CDP Page.captureScreenshot 抓取 PNG）。
func (s *SimCDPSession) CaptureScreenshot(_ context.Context) ([]byte, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, h := 1920, 1080
	if s.cfg.Viewport != nil && s.cfg.Viewport.Width > 0 && s.cfg.Viewport.Height > 0 {
		w, h = s.cfg.Viewport.Width, s.cfg.Viewport.Height
	}
	return []byte("sim-png:" + s.id), w, h, nil
}

// CaptureScreenshotRegion 记录区域并返回确定性裁剪截图（browser.ScreenshotterRegion）。
func (s *SimCDPSession) CaptureScreenshotRegion(_ context.Context, x, y, w, h int) ([]byte, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastClip = []int{x, y, w, h}
	return []byte("sim-region:" + s.id), w, h, nil
}

// LastClipRegion 返回最近一次区域截图（测试断言用）。
func (s *SimCDPSession) LastClipRegion() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.lastClip...)
}

// SetNavEvents 脚本化后续导航/网络/DOM 事件（模拟 CDP 事件流，测试用）。
// pushState=true 表示本次动作触发 SPA 软路由；activeConns 为活动连接电平；
// domMutated=true 注入一次 DOM 突变（防抖窗口语义见 pkg/verifier）。
func (s *SimCDPSession) SetNavEvents(pushState bool, activeConns int, domMutated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pushState {
		s.nav.pushState = true
	}
	if domMutated {
		s.nav.mutated = true
	}
	s.nav.active = activeConns
}

// NavigationState 返回当前事件快照并清除一次性事件位（pushState/mutated）。
func (s *SimCDPSession) NavigationState() (pushState bool, activeConns int, domMutated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pushState = s.nav.pushState
	domMutated = s.nav.mutated
	s.nav.pushState = false
	s.nav.mutated = false
	activeConns = s.nav.active
	return
}

// Actions 返回已执行动作列表（测试断言用）。
func (s *SimCDPSession) Actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acts...)
}

// SetPageCandidates 脚本化页面候选节点（模拟 WASM 探针 AXTree 输出，测试用）。
func (s *SimCDPSession) SetPageCandidates(cands []browser.PageCandidate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages = cands
}

// SetPageSnapshot 脚本化页面侧只读快照（§5.0.13 条款 3；测试用）。
func (s *SimCDPSession) SetPageSnapshot(snap browser.PageSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap, s.snapSet = snap, true
}

// SetPageSnapshotSequence 脚本化「按调用顺序消费」的快照序列（§5.0.14 决议 9；测试用）：
// 第 n 次 PageSnapshot 调用返回第 n 个元素，队列耗尽后回落到 SetPageSnapshot 的固定值
// （未设固定值则回 ok=false）。用于断言空集重试的**次数**与**最终取值**。
func (s *SimCDPSession) SetPageSnapshotSequence(snaps ...browser.PageSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapSeq = append([]browser.PageSnapshot(nil), snaps...)
}

// PageSnapshot 返回脚本化的页面侧只读快照（browser.PageSnapshotSource 接口），
// 并记录本次枢轴与调用次数（§5.0.14 条款 1：断言采样发生在派发之前、且每动作恰一次）。
func (s *SimCDPSession) PageSnapshot(pivot browser.PageSnapshotPivot) (browser.PageSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapPivot = pivot
	s.snapCalls++
	if len(s.snapSeq) > 0 {
		snap := s.snapSeq[0]
		s.snapSeq = s.snapSeq[1:]
		return snap, true
	}
	if !s.snapSet {
		return browser.PageSnapshot{}, false
	}
	return s.snap, true
}

// LastPageSnapshotPivot 返回最近一次派发前采样收到的枢轴（测试断言用）。
func (s *SimCDPSession) LastPageSnapshotPivot() browser.PageSnapshotPivot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapPivot
}

// SetPreDispatchReceptor 脚本化「派发前受体回传」序列（§5.0.14 决议 0；测试用）：
// 之后每次 ExecuteAction/ExecuteActionWithSelector 依次回调一个脚本化受体（每次动作从头开始）。
func (s *SimCDPSession) SetPreDispatchReceptor(recs ...browser.PreDispatchReceptor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.preReceptors = append([]browser.PreDispatchReceptor(nil), recs...)
}

// SetPreDispatchReceptorSink 实现 browser.PreDispatchReceptorSink（模拟会话恒声明具备该能力；
// 是否真的回传受体由 SetPreDispatchReceptor 脚本化，缺省回调零值受体 ⇒ 引擎不采样）。
func (s *SimCDPSession) SetPreDispatchReceptorSink(sink func(browser.PreDispatchReceptor)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.preSink = sink
}

// notifyPreDispatchReceptor 在锁外依次回调脚本化受体（回调内会重入读锁做只读快照，
// 故不得持锁调用；fail-soft：回调 panic 不影响模拟派发结果）。
func (s *SimCDPSession) notifyPreDispatchReceptor() {
	s.mu.Lock()
	sink := s.preSink
	recs := append([]browser.PreDispatchReceptor(nil), s.preReceptors...)
	s.mu.Unlock()
	if sink == nil {
		return
	}
	if len(recs) == 0 {
		recs = []browser.PreDispatchReceptor{{}}
	}
	for _, rec := range recs {
		s.mu.Lock()
		s.preCalls++
		if rec.Found {
			s.preLast = rec
		}
		s.mu.Unlock()
		sink(rec)
	}
}

// PreDispatchReceptorCalls 返回已回传受体的次数（测试断言幂等性用）。
func (s *SimCDPSession) PreDispatchReceptorCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preCalls
}

// LastPreDispatchReceptor 返回最近一次**有效**（Found）回传的受体（测试断言用）。
func (s *SimCDPSession) LastPreDispatchReceptor() browser.PreDispatchReceptor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preLast
}

// PageSnapshotCalls 返回页面侧只读快照被调用的次数（测试断言用：每动作恰一次采样）。
func (s *SimCDPSession) PageSnapshotCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapCalls
}

// PageCandidates 返回当前页面候选节点（browser.CandidateSource 接口）。
func (s *SimCDPSession) PageCandidates() []browser.PageCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]browser.PageCandidate(nil), s.pages...)
}

// SetProbeStats 脚本化探针截断/溢出状态（模拟 WASM 探针通道，引擎计数测试用）。
func (s *SimCDPSession) SetProbeStats(st browser.ProbeStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probe = st
}

// ProbeStats 返回模拟探针状态（browser.ProbeStatsSource 接口）。
func (s *SimCDPSession) ProbeStats() browser.ProbeStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probe
}

// SetNewWindow 脚本化一次「新窗口打开」事件（模拟 target="_blank" 点击，
// TDD-ENG-13 引擎映射用；真实 CDP 会话由 Target.targetCreated 事件驱动）。
func (s *SimCDPSession) SetNewWindow(targetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newWin = targetID
}

// NewWindowOpened 读取并清零新窗口标记（browser.WindowEvents 接口）。
func (s *SimCDPSession) NewWindowOpened() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.newWin == "" {
		return "", false
	}
	id := s.newWin
	s.newWin = ""
	return id, true
}

// SwitchTarget 模拟把会话作用面切到新 Target（browser.TargetSwitcher，TDD-ENG-14）。
// 真实 CDP 会话见 cdpSession.SwitchTarget；这里只记录轨迹供引擎接线断言。
func (s *SimCDPSession) SwitchTarget(_ context.Context, targetID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.activeTarget
	if prev == "" {
		prev = s.id
	}
	if targetID == "" || targetID == prev {
		return prev, nil
	}
	s.activeTarget = targetID
	s.switches = append(s.switches, prev+"->"+targetID)
	return prev, nil
}

// ActiveTargetID 返回当前实际作用的 Target（未接管时 = 会话 ID）。
func (s *SimCDPSession) ActiveTargetID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTarget == "" {
		return s.id
	}
	return s.activeTarget
}

// Switches 返回目标切换轨迹（测试/审计用）。
func (s *SimCDPSession) Switches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.switches...)
}

// SetChatStream 脚本化聊天 SSE 快照与增量块（模拟 Network 层捕获，测试用）。
func (s *SimCDPSession) SetChatStream(snap browser.ChatStreamSnapshot, chunks []browser.ChatStreamChunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatSnap = snap
	s.chatDrain = append([]browser.ChatStreamChunk(nil), chunks...)
}

// ChatStreamSnapshot 返回模拟 SSE 快照（browser.ChatStreamSource 接口）。
func (s *SimCDPSession) ChatStreamSnapshot() browser.ChatStreamSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chatSnap
}

// ChatStreamDrain 返回并清空模拟增量块（browser.ChatStreamSource 接口）。
func (s *SimCDPSession) ChatStreamDrain() []browser.ChatStreamChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]browser.ChatStreamChunk(nil), s.chatDrain...)
	s.chatDrain = nil
	return out
}

// ProbeChallenge 实现 browser.ChallengeProber。
func (s *SimCDPSession) ProbeChallenge(ctx context.Context) (*challenge.ChallengeSignal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.challenge {
		return &challenge.ChallengeSignal{
			Type:       challenge.ChallengeTypePayPal,
			Provider:   "Simulated",
			Confidence: 0.99,
			Reason:     "模拟环境激活挑战态",
		}, true
	}
	return nil, false
}

// DispatchHumanInput 实现 browser.HumanInputForwarder。
func (s *SimCDPSession) DispatchHumanInput(ctx context.Context, ev challenge.HumanInputEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Type == challenge.HumanInputClick {
		s.challenge = false
	}
	return nil
}

// SimFactory 为每个 BrowserContext 创建独立模拟会话。
type SimFactory struct {
	next int64
}

// Create 实现 browser.SessionFactory。
func (f *SimFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	f.next++
	return NewSimCDPSession(fmt.Sprintf("sim-%d", f.next), cfg), nil
}
