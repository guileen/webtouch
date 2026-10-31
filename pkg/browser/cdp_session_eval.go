package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"uws/pkg/browser/input"
)

// inputAt 聚焦元素并输入值（input/textarea 走 Input.insertText，select 走 JS 赋值）。
// semantic 为意图语义，供 §5.0.9 条款 1 的「意图-受体角色守卫」在派发前比对；
// 这是全部 input 出口（确定性语义/选择器、node:、VLM 坐标）共用的**单点**守卫位置。
// disp 为派发标记（§5.0.11 条款 1/2）：本函数是文本汇支路唯一的派发点，标记必须落在
// 守卫/可编辑门禁/重锚全部通过之后、第一个输入事件（select 的合成点击或
// Input.insertText）发出之前——定位与校验阶段的中断不得被记成「已派发」。
func (s *cdpSession) inputAt(ctx context.Context, disp *dispatchTracker, t elementTarget, action, value, semantic string) error {
	s.activateForInput(ctx)
	// §5.0.10 条款 2：派发前对已标记受体做结构身份快照（Leased Token），失配时据此同余判定。
	lease := s.captureTargetLease(ctx)
	// §5.0.9 条款 1：派发前守卫。命中即熔断（不派发、不写 learned），错误串携带结构化代号，
	// 交调用方区分「确定性拒绝」——不得被重试循环吞成普通失败。
	if action != "select" {
		blocked, detail, gerr := s.guardInputReceptor(ctx, semantic, value)
		if gerr != nil {
			return fmt.Errorf("input_receptor_probe:%w", gerr)
		}
		if blocked {
			log.Printf("uws input guard semantic=%q blocked receptor=%q (TARGET_ROLE_MISMATCH)",
				semantic, detail)
			return fmt.Errorf("%s:%s", FaultTargetRoleMismatch, detail)
		}
	}
	// §5.0.3 条款 1（派发层二次校验）：文本汇支路的目标必须可编辑，否则明确失败——绝不
	// 「聚焦到别处 + 元素原生文案非空 = 成功」（假成功根因）。select 作用于自定义下拉
	// 触发器时目标是菜单触发器（2026-08-31 Airwallex 条款），不由本门禁裁定。
	reanchored := false
	if action != "select" || t.Tag == "select" {
		editable, err := s.evalBool(ctx, input.InputTargetEditableJS)
		if err != nil {
			return fmt.Errorf("input_not_editable:gate:%w", err)
		}
		if !editable {
			// §5.0.10 条款 2/3：标记载体常被水合重建抹掉 ⇒ 先做一次结构身份同余局域微重锚
			// （Budget = 1）；同余不成立（租约过期/指纹破坏/位移 > 32px）即硬熔断，绝不猜。
			ok, reason, rerr := s.reanchorTarget(ctx, lease)
			if rerr != nil {
				return fmt.Errorf("reanchor_probe:%w", rerr)
			}
			if !ok {
				log.Printf("uws input reanchor refused tag=%s reason=%s (%s)",
					t.Tag, reason, FaultTargetTopologyRupture)
				return fmt.Errorf("%s:%s", FaultTargetTopologyRupture, reason)
			}
			// 重锚命中的是**新**载体：守卫必须在最终受体上重跑（§5.0.9 条款 1 覆盖全部出口，
			// 微重锚不得成为绕过意图-受体守卫的水床出口）。
			blocked, detail, gerr := s.guardInputReceptor(ctx, semantic, value)
			if gerr != nil {
				return fmt.Errorf("input_receptor_probe:%w", gerr)
			}
			if blocked {
				log.Printf("uws input guard (reanchored) semantic=%q blocked receptor=%q (TARGET_ROLE_MISMATCH)",
					semantic, detail)
				return fmt.Errorf("%s:%s", FaultTargetRoleMismatch, detail)
			}
			reEditable, reErr := s.evalBool(ctx, input.InputTargetEditableJS)
			if reErr != nil || !reEditable {
				return fmt.Errorf("%s:reanchor_not_editable", FaultTargetTopologyRupture)
			}
			reanchored = true
		}
	}
	if disp != nil {
		disp.Mark() // 派发前校验全部通过，输入事件从这里开始发出
	}
	if t.Tag == "select" {
		const js = `(() => {
		  const el = document.querySelector('[data-uws-target="1"]');
		  if (!el) return false;
		  el.value = VALUE;
		  el.dispatchEvent(new Event('input', {bubbles: true}));
		  el.dispatchEvent(new Event('change', {bubbles: true}));
		  return true;
		})()`
		expr := strings.ReplaceAll(js, "VALUE", quoteJS(value))
		ok, err := s.evalBool(ctx, expr)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("cdp: select target missing")
		}
		if reanchored && !input.InputLanded(ctx, bridgeInputSession{s}, "") {
			log.Printf("uws input land check failed after reanchor (select) semantic=%q", semantic)
			return fmt.Errorf("%s:reanchor_not_landed", FaultWriteNotPersisted)
		}
		return nil
	}
	// 2026-08-31 Airwallex 实战：select 语义对自定义下拉框（react-select 类，
	// input+hidden 结构，如计费频率/货币/税务行为）＝打开菜单 → JS 点击文本
	// 匹配的 [role=option]。实测 Enter 只提交高亮首项（不可靠），选项点击是
	// 确定性提交（hidden value 正确落值）。无匹配选项时回退常规 insertText。
	if action == "select" && t.Tag != "select" {
		const js = `(async () => {
		  const el = document.querySelector('[data-uws-target="1"]');
		  if (!el) return {ok:false, reason:'no-target'};
		  el.dispatchEvent(new MouseEvent('mousedown', {bubbles:true, cancelable:true}));
		  el.focus();
		  el.click();
		  const want = VALUE;
		  const norm = s => (s == null ? '' : String(s)).replace(/\s+/g, ' ').trim();
		  const deadline = Date.now() + 2500;
		  while (Date.now() < deadline) {
		    const opts = Array.from(document.querySelectorAll('[role=option]'));
		    const hit = opts.find(o => norm(o.textContent).indexOf(want) >= 0);
		    if (hit) { hit.click(); return {ok:true, method:'option-click'}; }
		    await new Promise(r => setTimeout(r, 120));
		  }
		  return {ok:false, reason:'no-option'};
		})()`
		expr := strings.ReplaceAll(js, "VALUE", quoteJS(value))
		var res struct {
			OK     bool   `json:"ok"`
			Reason string `json:"reason"`
		}
		if err := s.evalValue(ctx, expr, &res); err != nil {
			return err
		}
		if res.OK {
			return nil
		}
	}
	const focusJS = `(() => {
	  const el = document.querySelector('[data-uws-target="1"]');
	  if (!el) return false;
	  el.focus();
	  if (typeof el.select === 'function') el.select();
	  return true;
	})()`
	focused, err := s.evalBool(ctx, focusJS)
	if err != nil {
		return err
	}
	if !focused {
		return fmt.Errorf("cdp: input target missing")
	}
	if err := s.inputGuard().Call(ctx, "Input.insertText",
		map[string]any{"text": value}); err != nil {
		return err
	}
	// 补发 change（表单语义；React/Vue 的 input 事件已由 insertText 触发）。
	_, _ = s.evalResult(ctx, `(() => {
	  const el = document.querySelector('[data-uws-target="1"]');
	  if (el) el.dispatchEvent(new Event('change', {bubbles: true}));
	  return true;
	})()`)
	// §5.0.10 条款 3：重锚击发后**无条件**落地回读（不得以「已重锚」为由免除后验）。
	if reanchored && !input.InputLanded(ctx, bridgeInputSession{s}, value) {
		log.Printf("uws input land check failed after reanchor semantic=%q want=%q", semantic, value)
		return fmt.Errorf("%s:reanchor_not_landed", FaultWriteNotPersisted)
	}
	return nil
}

// extractAt 读取目标文本/值（extract 动作）。
func (s *cdpSession) extractAt(ctx context.Context, t elementTarget) (string, error) {
	const js = `(() => {
	  const el = document.querySelector('[data-uws-target="1"]');
	  if (!el) return '';
	  return (el.innerText || el.value || el.getAttribute('aria-label') || '').trim();
	})()`
	return s.evalResult(ctx, js)
}

func (s *cdpSession) clearMarker(ctx context.Context) {
	_, _ = s.evalResult(ctx, `(() => {
	  document.querySelectorAll('[data-uws-target]').forEach(e => delete e.dataset.uwsTarget);
	  return true;
	})()`)
}

// errEvalEmpty 表示 Runtime.evaluate 成功返回但 value 为空/缺失
// （区别于「JS 抛异常」eval_error 与「探针返回 found:false」no_match；
// 2026-08-27 前置：executeAction 不再把三种根因统一 mask 成 target_not_found）。
var errEvalEmpty = errors.New("cdp: evaluate returned empty value")

// classifyFindFailure 把 findElementOpt 的失败归为可诊断原因，供 ExecuteAction
// 透传引擎：
//   - eval_error:<exceptionDetails>：Runtime.evaluate 抛异常（evalRaw 原样透传）；
//   - eval_empty：evaluate 成功但 value 为空/缺失；
//   - no_match：探针 JS 正常执行并返回 found:false。
//
// pageURL 非空时追加 |url=<location.href>，辅助区分「eval 到了错误 Tab/页面」。
func classifyFindFailure(err error, found bool, pageURL string) string {
	var reason string
	switch {
	case err == nil && !found:
		reason = "no_match"
	case errors.Is(err, errEvalEmpty):
		reason = "eval_empty"
	case err != nil:
		reason = "eval_error:" + err.Error()
	default:
		reason = "target_not_found"
	}
	if pageURL != "" {
		return reason + "|url=" + pageURL
	}
	return reason
}

// pageURL 读取当前页面 location.href（诊断辅助；失败返回空串，不阻断主流程）。
func (s *cdpSession) pageURL(ctx context.Context) string {
	sub, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	u, err := s.evalResult(sub, "location.href")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u)
}

// wsBaseFromEndpoint 把 CDP 端点 URL 归一为 ws(s)://host:port 基底
// （用于构造页面级调试 URL ws://host:port/devtools/page/<targetId>；
// 解析失败返回空串）。
func wsBaseFromEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	switch u.Scheme {
	case "ws", "wss":
		return u.Scheme + "://" + u.Host
	case "http":
		return "ws://" + u.Host
	case "https":
		return "wss://" + u.Host
	}
	return ""
}

// evalRaw 执行 Runtime.evaluate 并返回原始 value（含异常检测）。
func (s *cdpSession) evalRaw(ctx context.Context, expr string) (json.RawMessage, error) {
	var raw struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Runtime.evaluate",
		map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &raw); err != nil {
		return nil, err
	}
	if len(raw.ExceptionDetails) > 0 {
		return nil, fmt.Errorf("cdp: evaluate exception: %s", string(raw.ExceptionDetails))
	}
	return raw.Result.Value, nil
}

// evalValue 执行表达式并把返回的 value 解码到 dst（对象/数组/标量皆可）。
func (s *cdpSession) evalValue(ctx context.Context, expr string, dst any) error {
	v, err := s.evalRaw(ctx, expr)
	if err != nil {
		return err
	}
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return json.Unmarshal(v, dst)
}

// evalBool 执行表达式并读取布尔结果。
func (s *cdpSession) evalBool(ctx context.Context, expr string) (bool, error) {
	var b bool
	if err := s.evalValue(ctx, expr, &b); err != nil {
		return false, err
	}
	return b, nil
}

// EvaluateScript 实现 DirectScriptEvaluator 接口，供 In-Page Direct Transport 执行原生 JS 与 fetch。
func (s *cdpSession) EvaluateScript(ctx context.Context, expr string) ([]byte, error) {
	raw, err := s.evalRaw(ctx, expr)
	if err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

// evalResult 执行表达式并读取字符串结果。
func (s *cdpSession) evalResult(ctx context.Context, expr string) (string, error) {
	var str string
	if err := s.evalValue(ctx, expr, &str); err != nil {
		return "", err
	}
	return str, nil
}

// ── browser.VerifierEvents ────────────────────────────────────────────

// NavigationState 返回 SPA 时序事件快照：pushState 位（读后清零）、
// XHR/Fetch 在途连接数、DOM 突变信号（基于采样计数增量，读后对齐）。
func (s *cdpSession) NavigationState() (pushState bool, activeConns int, domMutated bool) {
	s.mu.Lock()
	pushState = s.pushState
	s.pushState = false
	activeConns = freshInflight(s.inflight, time.Now(), maxInflightAge)
	s.mu.Unlock()

	if pushState {
		s.startMutationSampling()
	}
	s.sampleMutationsOnce()
	s.mu.Lock()
	domMutated = s.mutCount > s.mutReturned
	s.mutReturned = s.mutCount
	s.mu.Unlock()
	return
}

// ActiveTargetID 返回当前实际作用的 Target ID（browser.TargetSwitcher）。
// 未发生新窗口接管时等于 TargetID()。
func (s *cdpSession) ActiveTargetID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targetID
}

// SwitchTarget 把会话「当前作用的 Target」切到同一 Chrome 内的另一个页面
// （新窗口/新标签页接管，2026-09-22 用户现场反馈批次 P0-1：微信快捷登录弹出的
// 授权弹窗必须能被自动点到「允许」，而不是停下等人）。
//
// 与 initTarget 的差别：不动窗口、不关起始白板、不重新定位——只把「作用面」搬过去：
//  1. attach 新 target（flatten）+ 打开 Runtime/Page/Network 域与焦点仿真护栏；
//  2. 换 sessionID/targetID 后**重放身份**（UA/Client Hints、语言、时区、指纹 hook、
//     视口、探针预置）——新页不带同一份身份就是「同一会话两套指纹」的当场穿帮；
//  3. 换页即换页面时序（pushState/navErr/challenge/候选缓存一律不带过来），摘掉旧
//     session 的事件订阅、重挂新 session 的（旧页仍活着，它的导航/挑战/聊天流事件
//     不许再写本会话状态机）；
//  4. 迁移观看流（Page.startScreencast 绑在 sessionID 上，不迁移就会「动作打新页、
//     面板还演旧页」）；
//  5. detach 旧 session——只解除绑定，**不关标签**：原页对用户仍可见、可切回。
//
// R1（Focus Invariant）：全程不调用 Target.activateTarget / Page.bringToFront，
// 新页在后台被操作，用户前台焦点不受影响。
func (s *cdpSession) SwitchTarget(ctx context.Context, targetID string) (string, error) {
	s.mu.Lock()
	prev, oldSession, stream := s.targetID, s.sessionID, s.screencast
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return prev, errors.New("cdp: session closed")
	}
	if targetID == "" || targetID == prev {
		return prev, nil
	}
	var streamOpts ScreencastOptions
	streamRunning := false
	if stream != nil {
		streamOpts, streamRunning = stream.OptionsSnapshot()
	}

	sessionID, err := s.client.attachTarget(ctx, targetID)
	if err != nil {
		return prev, fmt.Errorf("cdp: attach target %s: %w", targetID, err)
	}
	if err := s.enableTargetDomains(ctx, sessionID); err != nil {
		_, _ = s.client.Send(ctx, "Target.detachFromTarget", map[string]any{"sessionId": sessionID})
		return prev, err
	}
	// 先换身份指针再重放身份：inputGuard().EnableFocusEmulation / installWorkerFingerprintHooks /
	// installPreProbeScript 读的都是 s.sessionID。
	s.mu.Lock()
	s.sessionID, s.targetID = sessionID, targetID
	s.mu.Unlock()
	if err := s.applyTargetIdentity(ctx, sessionID); err != nil {
		// 回滚：身份没落地就不能停在新页上（半套身份比不切更危险）。
		s.mu.Lock()
		s.sessionID, s.targetID = oldSession, prev
		s.mu.Unlock()
		_, _ = s.client.Send(ctx, "Target.detachFromTarget", map[string]any{"sessionId": sessionID})
		s.subscribe(ctx)
		return prev, err
	}
	s.clearPageState()
	s.mu.Lock()
	s.pushState = false
	s.navErr = ""
	s.mu.Unlock()
	if oldSession != "" {
		s.client.OffSession(oldSession)
	}
	s.subscribe(ctx)
	// 观看流迁移：先在新页上重开同一组参数，再断旧绑定（顺序反了会闪一帧空画面）。
	if stream != nil {
		_ = stream.Stop(ctx)
		s.mu.Lock()
		if s.screencast == stream {
			s.screencast = nil
		}
		s.mu.Unlock()
		if streamRunning {
			if next, err := s.getScreencastStream(); err == nil {
				_ = next.Start(ctx, streamOpts)
			}
		}
	}
	if oldSession != "" {
		_, _ = s.client.Send(ctx, "Target.detachFromTarget", map[string]any{"sessionId": oldSession})
	}
	return prev, nil
}

// NewWindowOpened 读取并清零新窗口标记（TDD-ENG-13，一次性语义）。
func (s *cdpSession) NewWindowOpened() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.newWindowTarget == "" {
		return "", false
	}
	id := s.newWindowTarget
	s.newWindowTarget = ""
	return id, true
}

// ── browser.ChatStreamSource（方案 1：聊天 SSE 网络层捕获） ─────────────

// ChatStreamSnapshot 返回最近一轮聊天 SSE 流的累计快照（干净 Markdown）。
func (s *cdpSession) ChatStreamSnapshot() ChatStreamSnapshot {
	return s.chat.Snapshot()
}

// ChatStreamDrain 返回并清空聊天 SSE 增量块（读后清零，供真流式推送）。
func (s *cdpSession) ChatStreamDrain() []ChatStreamChunk {
	return s.chat.Drain()
}

// startMutationSampling 启动限频 DOM 突变采样（约 50ms 一次，2s 后自动停止）。
func (s *cdpSession) startMutationSampling() {
	s.mu.Lock()
	if s.mutSampling {
		s.mu.Unlock()
		return
	}
	s.mutSampling = true
	seq := s.mutSampleSeq + 1
	s.mutSampleSeq = seq
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			if s.mutSampleSeq == seq {
				s.mutSampling = false
			}
			s.mu.Unlock()
		}()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.Now().Add(2 * time.Second)
		for {
			select {
			case <-s.closedCh:
				return
			case <-ticker.C:
			}
			if time.Now().After(deadline) {
				return
			}
			s.sampleMutationsOnce()
		}
	}()
}

// sampleMutationsOnce 读取一次 MutationObserver 计数（失败保留旧值）。
func (s *cdpSession) sampleMutationsOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var n int
	// 注意：计数器是数字，必须用 evalValue 解码 int；evalResult（string 解码）
	// 会对数字报 json 类型错误并静默丢弃——真实 CDP 的 DOM 突变信号曾因此失效
	// （TDD-OPT-01 真实侧实测暴露，2026-08-20）。
	if err := s.evalValue(ctx, `(window.__uwsMutCount || 0)`, &n); err != nil {
		return
	}
	s.mu.Lock()
	if n > s.mutCount {
		s.mutCount = n
	}
	if len(s.candCache) > 0 && n != s.candBaseMut {
		// DOM 已突变：候选快照过期（下次抽取重新采集，SPA/动态内容不失真）。
		s.candCache = nil
	}
	s.mu.Unlock()
}

// ── browser.CandidateSource ───────────────────────────────────────────

// PageCandidates 采集页面候选节点（真实 MATCH-05/06 输入通道）：
//   - WASM 探针路径（USER_ACTION #14）：JS BFS → bulk 协议喂入探针 →
//     读取 kept 节点（语义文本 + AST SimHash，供 L3 结构校验）；
//     截断时 200ms 内 JS 慢速遍历补偿 BFS 前 200 溢出节点；
//   - 探针缺失/注入失败时降级到 JS 文本采集（既有路径）。
//
// 同一文档内的连续调用命中候选缓存（TDD-OPT-01 多信号批量抽取）：第一次
// 采集后，后续信号抽取直接复用候选树，仅做 Go 侧匹配（单次 < 5ms）。
func (s *cdpSession) PageCandidates() []PageCandidate {
	s.mu.Lock()
	if got, ok := s.cachedCandidatesLocked(time.Now()); ok {
		s.candHits++
		s.mu.Unlock()
		return got
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var cands []PageCandidate
	var stats ProbeStats
	if c, st, err := s.collectProbeCandidates(ctx); err == nil {
		cands = c
		stats = st
	} else {
		// 探针不可用（未构建/注入失败）：优雅降级到 JS 文本采集。
		// §5.0.8 条款 3：失败路径仍保留降级代号（如 csp_wasm_blocked），
		// 否则「静默降级」正是本条款要根治的缺陷。
		stats = st
		cands = s.collectJSCandidates(ctx)
	}
	s.mu.Lock()
	pageStatus := s.lastStatus
	s.mu.Unlock()
	if pageStatus >= 400 {
		// 主文档 4xx/5xx：整页视为物理湮灭候选（A* 黑名单/深度惩罚的页面级输入）。
		for i := range cands {
			cands[i].HTTPStatus = pageStatus
		}
	}
	s.mu.Lock()
	s.candCache = cloneCandidates(cands)
	s.candCacheGen = s.docGen
	s.candCacheAt = time.Now()
	s.candBaseMut = s.mutCount
	s.probeStats = stats
	s.mu.Unlock()
	return cands
}

// cachedCandidatesLocked 返回仍有效的候选缓存快照（调用方持 mu）。
// 有效条件：非空 + 同一文档代（未导航）+ 采样计数未变（未 DOM 突变）+
// 未超过 TTL。命中返回深拷贝，调用方修改不污染缓存。
func (s *cdpSession) cachedCandidatesLocked(now time.Time) ([]PageCandidate, bool) {
	if len(s.candCache) == 0 || s.candCacheGen != s.docGen ||
		now.Sub(s.candCacheAt) >= candCacheTTL || s.mutCount != s.candBaseMut {
		return nil, false
	}
	return cloneCandidates(s.candCache), true
}

// cloneCandidates 深拷贝候选树（缓存快照隔离：引擎/调用方只读，后续采集
// 替换缓存不产生别名共享）。
func cloneCandidates(in []PageCandidate) []PageCandidate {
	if len(in) == 0 {
		return nil
	}
	out := make([]PageCandidate, len(in))
	for i := range in {
		out[i] = in[i]
		if len(in[i].Children) > 0 {
			out[i].Children = cloneCandidates(in[i].Children)
		}
	}
	return out
}

// collectJSCandidates 是既有 JS 文本采集降级路径（无 WASM 探针时兜底）。
func (s *cdpSession) collectJSCandidates(ctx context.Context) []PageCandidate {
	const js = `(() => {
	  const norm = s => (s == null ? '' : String(s)).replace(/\s+/g, ' ').trim();
	  // §5.0.6 条款 3：内联数据载体（style 模板 / 可解析的 JSON 数据块）不得进入语义文本源。
	  const isDataPayload = v => {
	    if (typeof v !== 'string' || v === '') return false;
	    if (/^<style/i.test(norm(v))) return true;
	    const s = v.trim();
	    if (s.charAt(0) !== '{' && s.charAt(0) !== '[') return false;
	    try { JSON.parse(s); return true; } catch (e) { return false; }
	  };
	  const out = [];
	  const seen = new Set();
	  const els = Array.from(document.querySelectorAll(
	    'button, a[href], input:not([type=hidden]), textarea, select, [role="button"], [role="link"], [role="menuitem"], [onclick], [data-testid]'));
	  const pointerTextLeaf = el => {
	    if (el.children.length !== 0) return false;
	    const text = norm(el.textContent);
	    return text.length > 0 && text.length <= 120 && getComputedStyle(el).cursor === 'pointer';
	  };
	  // 2026-08-31 Airwallex 实战：onclick 属性绑定（property handler）无 HTML 属性，
	  // [onclick] 选择器选不到 → 附加扫描（探针缺失降级路径与 findElementOpt 同口径）。
	  const baseSel = 'button, a[href], input:not([type=hidden]), textarea, select, [role="button"], [role="link"], [role="menuitem"], [onclick], [data-testid]';
	  const seenElements = new Set(els);
	  for (const el of document.querySelectorAll('*')) {
	    if ((el.onclick != null || pointerTextLeaf(el)) && !seenElements.has(el) && !el.matches(baseSel)) {
	      els.push(el);
	    }
	  }
	  const findLandmark = el => {
	    if (el.closest('main, [role="main"], article')) return 'main';
	    if (el.closest('nav, [role="navigation"]')) return 'nav';
	    if (el.closest('header, [role="banner"]')) return 'header';
	    if (el.closest('footer, [role="contentinfo"]')) return 'footer';
	    if (el.closest('aside, [role="complementary"]')) return 'aside';
	    return 'other';
	  };
	  const cssPath = el => {
	    const parts = [];
	    for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
	      let tag = (n.tagName || '').toLowerCase();
	      if (!tag) break;
	      const parent = n.parentElement;
	      if (parent) {
	        let index = 1;
	        for (const sibling of parent.children) {
	          if (sibling === n) break;
	          if ((sibling.tagName || '').toLowerCase() === tag) index++;
	        }
	        tag += ':nth-of-type(' + index + ')';
	      }
	      parts.unshift(tag);
	    }
	    return parts.join(' > ');
	  };
	  for (const el of els) {
	    const raw = el.innerText || el.value || el.placeholder || el.getAttribute('aria-label') ||
	               el.getAttribute('data-testid') || el.getAttribute('title') || '';
	    if (isDataPayload(raw)) continue;
	    const t = norm(raw);
	    if (!t || t.length > 200 || seen.has(t)) continue;
	    const cs = getComputedStyle(el);
	    if (cs.visibility === 'hidden' || cs.display === 'none') continue;
	    const r = el.getBoundingClientRect();
	    if (r.width < 2 || r.height < 2) continue;
	    seen.add(t);
	    const pText = el.parentElement ? norm(el.parentElement.innerText || '').slice(0, 100) : '';
	    let sel = '';
	    if (el.id) { sel = '#' + el.id; }
	    else if (el.getAttribute('data-testid')) { sel = '[data-testid="' + el.getAttribute('data-testid') + '"]'; }
	    else if (el.name) { sel = el.tagName.toLowerCase() + '[name="' + el.name + '"]'; }
	    if (!sel) sel = cssPath(el);
	    out.push({
	      t: t,
	      tag: (el.tagName || '').toLowerCase(),
	      role: el.getAttribute('role') || '',
	      sel: sel,
	      x: Math.round(r.left),
	      y: Math.round(r.top),
	      dx: Math.round(r.left + window.scrollX),
	      dy: Math.round(r.top + window.scrollY),
	      w: Math.round(r.width),
	      h: Math.round(r.height),
	      lm: findLandmark(el),
	      ctx: pText
	    });
	  }
	  return out;
	})()`
	var raw []struct {
		Text      string  `json:"t"`
		Tag       string  `json:"tag"`
		Role      string  `json:"role"`
		Selector  string  `json:"sel"`
		X         float64 `json:"x"`
		Y         float64 `json:"y"`
		DocumentX float64 `json:"dx"`
		DocumentY float64 `json:"dy"`
		W         float64 `json:"w"`
		H         float64 `json:"h"`
		Landmark  string  `json:"lm"`
		Context   string  `json:"ctx"`
	}
	if err := s.evalValue(ctx, js, &raw); err != nil {
		return nil
	}
	cands := make([]PageCandidate, 0, len(raw))
	for _, r := range raw {
		cands = append(cands, PageCandidate{
			Text:      r.Text,
			Tag:       r.Tag,
			Role:      r.Role,
			Selector:  r.Selector,
			X:         r.X,
			Y:         r.Y,
			DocumentX: r.DocumentX,
			DocumentY: r.DocumentY,
			W:         r.W,
			H:         r.H,
			Landmark:  r.Landmark,
			Context:   r.Context,
		})
	}
	return cands
}
