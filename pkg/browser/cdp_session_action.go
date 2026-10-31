package browser

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"uws/pkg/browser/find"
	"uws/pkg/browser/input"
	"uws/pkg/browser/modelbridge"
	"uws/pkg/browser/semantics"
)

// ── browser.ActionSession ─────────────────────────────────────────────

// ExecuteAction 执行原子动作：语义定位 → 遮挡检测 → 物理点击/输入。
// 返回（是否成功, 观测描述）。
func (s *cdpSession) ExecuteAction(ctx context.Context, action, value, semantic string) (bool, string) {
	return s.executeAction(ctx, action, value, semantic, "")
}

// ExecuteActionWithSelector 是 SelectorActionSession 实现：与 ExecuteAction 相同，
// 但物理定位优先使用知识库规则的选择器（querySelectorAll），语义评分兜底。
func (s *cdpSession) ExecuteActionWithSelector(ctx context.Context, action, value, semantic, selector string) (bool, string) {
	return s.executeAction(ctx, action, value, semantic, selector)
}

// PageURL 返回当前页面 location.href（VLMProbeSession；失败返回空串）。
func (s *cdpSession) PageURL() string {
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	return s.pageURL(ctx)
}

// ClickAt 按视口 CSS 坐标物理点击（VLM 兜底坐标候选）：
// 先 elementFromPoint 解析命中元素，返回其 CSS 路径（供引擎学习）；
// 再走与语义点击一致的 Fitts 轨迹物理事件。无命中元素返回失败。
func (s *cdpSession) ClickAt(ctx context.Context, x, y int) (string, bool, string) {
	ctx, cancel := context.WithTimeout(ctx, semantics.ActionBudget)
	defer cancel()
	disp := &dispatchTracker{}
	sel, ok, observed := s.clickAtInner(ctx, disp, x, y)
	if ok {
		return sel, ok, observed
	}
	// 与 executeAction 同一套中断语义：elementPathAt 阶段取消 = 还没有任何物理事件
	// （可以安全重放）；clickPoint 已经把鼠标事件发给 Chrome 之后取消 = 结果未知
	// （不得让上层当成普通失败重试）。VLM 坐标路径过去不区分这两者，派发后的取消
	// 被吞成 click_failed，上层重试就把一次点击变成两次（2026-09-23 审查结论）。
	if obs, canceled := canceledObserved(ctx, disp.Dispatched()); canceled {
		return "", false, obs
	}
	return sel, ok, observed
}

// clickAtInner 是 ClickAt 的实体：定位用 disp.Mark() 之前的阶段表示「未派发」，
// 物理点击用其后的阶段表示「已派发」，由 ClickAt 统一翻译成中断观测串。
func (s *cdpSession) clickAtInner(ctx context.Context, disp *dispatchTracker, x, y int) (string, bool, string) {
	pt, err := s.elementPointAt(ctx, x, y)
	if err != nil {
		return "", false, "click_at:" + err.Error()
	}
	if pt.Selector == "" {
		return "", false, "click_at:no_element"
	}
	// §5.0.14 决议 0③：VLM 坐标点击在解出受体后、disp.Mark() 之前回传。
	// §5.0.9 条款 6：落点证据连同可见文本一并落账（引擎据此判「相关 / miss / 证据不足」）。
	vlmTarget := elementTarget{Found: true, X: float64(x), Y: float64(y), Sel: pt.Selector, Tag: pt.Tag, Text: pt.Text}
	s.mu.Lock()
	s.lastHit = vlmTarget
	s.mu.Unlock()
	s.notifyPreDispatchReceptor(vlmTarget)
	disp.Mark() // 鼠标轨迹从这里开始发给 Chrome，之后中断只能报「结果未知」
	if err := s.clickPoint(ctx, float64(x), float64(y)); err != nil {
		return "", false, "click_failed:" + err.Error()
	}
	s.clearMarker(ctx)
	return pt.Selector, true, fmt.Sprintf("click_at:%d,%d", x, y)
}

// InputAt 在视口坐标处定位「就近输入框」并填入 value（browser.VLMProbeSession，
// 2026-08-31 input 兜底）：elementFromPoint 命中元素若自身/祖先/后代含
// input/textarea/select（非 hidden）即取最近者聚焦填入；返回输入框自身 CSS 路径
// （供 learned 回写，下次同页面免 VLM）。解决 Airwallex 描述 textarea 无
// placeholder/aria-label、语义评分抓不到导致的「VLM 报坐标却填不进去」。
func (s *cdpSession) InputAt(ctx context.Context, x, y int, value, semantic string) (string, bool, string) {
	ctx, cancel := context.WithTimeout(ctx, semantics.ActionBudget)
	defer cancel()
	disp := &dispatchTracker{}
	sel, ok, observed := s.inputAtCoord(ctx, disp, x, y, value, semantic)
	if ok {
		return sel, ok, observed
	}
	// 同 ClickAt：定位就近输入框（Runtime.evaluate）阶段取消 = 未派发，可安全重放；
	// 聚焦/填值（inputAt）已经把键盘事件发出后取消 = 结果未知，不得让上层重放
	// （2026-09-23 审查结论）。
	if obs, canceled := canceledObserved(ctx, disp.Dispatched()); canceled {
		return "", false, obs
	}
	return sel, ok, observed
}

// inputAtCoord 是 InputAt 的实体（定位就近输入框 → 聚焦填值）；中断语义由 InputAt
// 依 disp 的「是否已派发」统一收口。
func (s *cdpSession) inputAtCoord(ctx context.Context, disp *dispatchTracker, x, y int, value, semantic string) (string, bool, string) {
	// §5.0.10 条款 1：寻底先做几何层叠判定（elementsFromPoint 栈剔除透明手势代理层），
	// 页内不可见（封闭宿主 / 全被遮罩 / 无输入）时交宿主层穿透。
	stem := input.StemAt(ctx, bridgeInputSession{s}, x, y)
	if !stem.OK {
		return input.InputAtPierced(ctx, bridgeInputSession{s}, disp, x, y, value, semantic)
	}
	sel := stem.Sel
	s.mu.Lock()
	s.lastHit = elementTarget{Found: true, X: float64(x), Y: float64(y), Sel: sel}
	s.mu.Unlock()
	// §5.0.11 条款 1：派发标记由 inputAt 在派发前校验全部通过后打，禁止前置到定位之前。
	if err := s.inputAt(ctx, disp, elementTarget{Found: true, Tag: "input", Sel: sel}, "input", value, semantic); err != nil {
		// 守卫熔断（TARGET_ROLE_MISMATCH）与写入失败（WRITE_NOT_PERSISTED）都要原样回传给
		// 引擎，不能在物理层被压成裸 `input_failed` 而丢掉结构化代号（§5.0.8 条款 2）。
		return "", false, err.Error()
	}
	// §5.0.9 条款 2：VLM 坐标路径的输入必须过落地回读（同一元素回读值包含输入值）。
	// 回读不过 = 无落地证据 ⇒ 硬判失败，禁止以 SUCCEEDED 出口出现。
	if !input.InputLanded(ctx, bridgeInputSession{s}, value) {
		log.Printf("uws input land check failed (vlm coord) semantic=%q selector=%q want=%q",
			semantic, sel, value)
		return "", false, FaultVLMlandingUnverified + ":回读值未包含输入值"
	}
	s.clearMarker(ctx)
	return sel, true, fmt.Sprintf("input_at:%d,%d", x, y)
}

// ExtractAt 在视口坐标处解析元素并提取可见文本 + CSS 路径（browser.VLMProbeSession，
// v1.5.5 自进化）：命中元素标记 data-uws-target=1 后读取文本，选择器经
// elementPathAt 解析（与 ClickAt 同源）。命中 body/文档根或不可提取返回 false。
func (s *cdpSession) ExtractAt(ctx context.Context, x, y int) (string, string, bool) {
	ctx, cancel := context.WithTimeout(ctx, semantics.ActionBudget)
	defer cancel()
	sel, err := s.elementPathAt(ctx, x, y)
	if err != nil || sel == "" {
		return "", "", false
	}
	const js = `(() => {
	  const el = document.elementFromPoint(X, Y);
	  if (!el || el === document.body || el === document.documentElement) return '';
	  document.querySelectorAll('[data-uws-target]').forEach(function(e){ delete e.dataset.uwsTarget; });
	  try { el.dataset.uwsTarget = '1'; } catch (e) {}
	  return (el.innerText || el.value || el.getAttribute('aria-label') || '').trim();
	})()`
	expr := strings.ReplaceAll(strings.ReplaceAll(js, "X", strconv.Itoa(x)), "Y", strconv.Itoa(y))
	text, err := s.evalResult(ctx, expr)
	if err != nil {
		return "", "", false
	}
	s.clearMarker(ctx)
	if text == "" {
		return "", "", false
	}
	return sel, text, true
}

// GoBack 触发浏览器后退（browser.HistoryNavigator；VLM 主控 back 原语）。
// 用 Runtime.evaluate("history.back()") 走宿主浏览器的历史栈（SPA/传统导航均适用）。
func (s *cdpSession) GoBack(ctx context.Context) error {
	_, err := s.evalRaw(ctx, "history.back()")
	return err
}

// elementPointAt 解析 (x,y) 处命中元素的选择器与可见文本（落点自证证据：§5.0.9 条款 6）。
// 域能力在 `find.ElementAtPoint`：文本取证与受体解析在**同一步**求值内完成，
// 否则两次取值之间的页面变化会让「点中谁」与「读到的文本是谁的」错位。
func (s *cdpSession) elementPointAt(ctx context.Context, x, y int) (find.HitPoint, error) {
	return find.ElementAtPoint(ctx, findBridge{s}, x, y)
}

// elementPathAt 解析 (x,y) 处命中的元素 CSS 路径（tag + class + nth-of-type，
// 遇带 id 祖先截断），无元素/命中 body 返回空串；只要选择器的调用方沿用此面。
func (s *cdpSession) elementPathAt(ctx context.Context, x, y int) (string, error) {
	p, err := s.elementPointAt(ctx, x, y)
	if err != nil {
		return "", err
	}
	return p.Selector, nil
}

// dispatchTracker 是「物理事件是否已经派发出去」的标记面（ctx 在派发后中断时，事件可能
// 已经落地——点歪、导航中——调用方不能当没发生过，2026-09-23 审查结论）。实体已随输入簇
// 迁入 `pkg/browser/input`（`input.DispatchTracker`，复杂度治理·阶段 2·单元 2c），此处以
// 别名引用 ⇒ 调用点把 `mark()`/`dispatched` 换成导出的 `Mark()`/`Dispatched()`。
type dispatchTracker = input.DispatchTracker

// canceledObserved 把 ctx 中断翻译成物理语义；未中断返回 ok=false。
func canceledObserved(ctx context.Context, dispatched bool) (string, bool) {
	if ctx.Err() == nil {
		return "", false
	}
	if dispatched {
		return ObservedCanceledAfterDispatch, true
	}
	return ObservedCanceledBeforeDispatch, true
}

// executeAction 是物理动作的对外入口：预算来自调用方 ctx（上限 semantics.BudgetFor(semantic)；
// 多阶段切换语义上界更宽，见 action_budget.go），中断时把「是否已派发」如实回报给引擎
// （见 canceledObserved）。
func (s *cdpSession) executeAction(ctx context.Context, action, value, semantic, selector string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, semantics.BudgetFor(semantic))
	defer cancel()
	disp := &dispatchTracker{}
	ok, observed := s.executeActionInner(ctx, disp, action, value, semantic, selector)
	if ok {
		return ok, observed
	}
	if obs, canceled := canceledObserved(ctx, disp.Dispatched()); canceled {
		return false, obs
	}
	return ok, observed
}

func (s *cdpSession) executeActionInner(ctx context.Context, disp *dispatchTracker, action, value, semantic, selector string) (bool, string) {

	// 标签页已被关闭（用户手动关 Tab）：显式报 target_destroyed，
	// API 层据此自动重建新标签页，而不是泛化成定位失败（2026-08-25 用户反馈）。
	s.mu.Lock()
	dead := s.destroyed
	s.mu.Unlock()
	if dead {
		return false, "target_destroyed"
	}

	// drag 是挑战解锁原语（VLM 坐标直发，bypass 语义匹配与挑战门禁）：
	// 挑战页上必须允许拖拽执行，否则求解动作永远被 challenge 门禁拦死（TDD-COORD-04）。
	if action != "drag" {
		s.mu.Lock()
		challenge := s.challenge
		s.mu.Unlock()
		if challenge {
			return false, "challenge_intercepted"
		}
	}

	// gemini_model_switch：Gemini 网页 4 档模型切换（2026-08-25）。
	// 语义层只需把「目标模型显示名」（如 "3.6 Flash"）作为 value 传入，
	// 物理层负责：打开模型选择器 → 按标签在菜单中定位 → 真实鼠标点击 →
	// 校验选择器按钮文本已更新（游客态模型被锁定会在此显式失败）。
	if semantic == "gemini_model_switch" {
		if action != "click" {
			return false, "unsupported_action"
		}
		if value == "" {
			return false, "model_switch:missing_label"
		}
		// §5.0.11 条款 1：标记由 switchGeminiModel 在菜单定位成功、点击前打。
		if err := s.switchGeminiModel(ctx, disp, value); err != nil {
			return false, "model_switch_failed:" + err.Error()
		}
		s.clearMarker(ctx)
		return true, "model_switch:" + value
	}

	// gemini_thinking_switch：Extended thinking 复选框（与模型单选正交）。
	// value ∈ {"on","1","true"} / {"off","0","false"}。
	if semantic == "gemini_thinking_switch" {
		if action != "click" {
			return false, "unsupported_action"
		}
		wantOn := strings.EqualFold(value, "on") || value == "1" || strings.EqualFold(value, "true")
		if err := s.switchGeminiThinking(ctx, disp, wantOn); err != nil {
			return false, "thinking_switch_failed:" + err.Error()
		}
		s.clearMarker(ctx)
		if wantOn {
			return true, "thinking:on"
		}
		return true, "thinking:off"
	}

	// deepseek_mode_switch：DeepSeek 新会话落地页三模式切换（快速/专家/识图）。
	// 与 gemini_model_switch 同构；模式单选组仅新会话落地页出现，进入会话后
	// 调用会显式报错（调用方应只在新会话建立时切换）。
	if semantic == "deepseek_mode_switch" {
		if action != "click" {
			return false, "unsupported_action"
		}
		if value == "" {
			return false, "mode_switch:missing_label"
		}
		if err := s.switchDeepSeekMode(ctx, disp, value); err != nil {
			return false, "mode_switch_failed:" + err.Error()
		}
		s.clearMarker(ctx)
		return true, "mode_switch:" + value
	}

	// deepseek_thinking_switch：深度思考开关（与模式正交，落地页与进行中会话均存在）。
	if semantic == "deepseek_thinking_switch" {
		if action != "click" {
			return false, "unsupported_action"
		}
		wantOn := strings.EqualFold(value, "on") || value == "1" || strings.EqualFold(value, "true")
		if err := s.toggleDeepSeekSwitch(ctx, disp, "深度思考", wantOn); err != nil {
			return false, "thinking_switch_failed:" + err.Error()
		}
		s.clearMarker(ctx)
		if wantOn {
			return true, "thinking:on"
		}
		return true, "thinking:off"
	}

	// deepseek_search_switch：智能搜索开关（专家模式无此开关，物理层显式报错）。
	if semantic == "deepseek_search_switch" {
		if action != "click" {
			return false, "unsupported_action"
		}
		wantOn := strings.EqualFold(value, "on") || value == "1" || strings.EqualFold(value, "true")
		if err := s.toggleDeepSeekSwitch(ctx, disp, "智能搜索", wantOn); err != nil {
			return false, "search_switch_failed:" + err.Error()
		}
		s.clearMarker(ctx)
		if wantOn {
			return true, "search:on"
		}
		return true, "search:off"
	}

	switch action {
	case "drag":
		x1, y1, x2, y2, perr := ParseDragCoords(value)
		if perr != nil {
			return false, "invalid_drag_coords"
		}
		disp.Mark()
		if derr := s.dragAt(ctx, x1, y1, x2, y2); derr != nil {
			return false, "drag_failed:" + derr.Error()
		}
		s.clearMarker(ctx)
		return true, fmt.Sprintf("drag:%.0f,%.0f→%.0f,%.0f", x1, y1, x2, y2)
	case "click":
		// 2026-08-25 自愈：定位稳定 + 语义自检 + 点歪副作用恢复（录音/弹层），
		// 页面未就绪不再「点歪就失败」（click_recovery.go）。
		if input.IsGeminiClick(semantic) {
			// §5.0.11 条款 1：标记由 clickWithRecovery 在定位之后打。
			if err := input.ClickWithRecovery(ctx, bridgeInputSession{s}, disp, semantic, selector); err != nil {
				return false, "click_failed:" + err.Error()
			}
			s.enrichHitSelector(ctx)
		} else {
			target, err := findElementOpt(ctx, s, semantic, selector, false)
			if err != nil || !target.Found {
				reason := classifyFindFailure(err, target.Found, s.pageURL(ctx))
				log.Printf("uws action click semantic=%q selector=%q failed: %s", semantic, selector, reason)
				return false, reason
			}
			s.enrichHitSelector(ctx)
			occluded, oerr := s.checkOcclusion(ctx, target)
			if oerr == nil && occluded {
				return false, "occluded"
			}
			// §5.0.14 决议 0③：受体已解析、事件未发出——在 disp.Mark() 之前回传受体。
			s.notifyPreDispatchReceptor(target)
			disp.Mark()
			if cerr := s.clickAt(ctx, target); cerr != nil {
				return false, "click_failed:" + cerr.Error()
			}
		}
		s.clearMarker(ctx)
		return true, "click:" + semantic
	case "input", "fill", "select":
		// 2026-08-25 自愈：定位稳定编辑器 + 输入落盘自检（click_recovery.go）。
		// §5.0.11 条款 1：标记由 inputAt 在派发前校验通过后打，禁止前置到定位之前。
		if err := input.InputWithRecovery(ctx, bridgeInputSession{s}, disp, semantic, selector, action, value); err != nil {
			return false, "input_failed:" + err.Error()
		}
		s.enrichHitSelector(ctx)
		s.clearMarker(ctx)
		return true, action + ":" + semantic
	case "extract":
		// 语义抽取走严格模式（2026-09-23）：内容节点入池 + 只认目标文本本身，
		// 见 findElementMode。抽不到就诚实 no_match，不再用碎片命中冒充成功。
		target, err := findElementMode(ctx, s, semantic, selector, true, true, false)
		if err != nil || !target.Found {
			reason := classifyFindFailure(err, target.Found, s.pageURL(ctx))
			log.Printf("uws action extract semantic=%q selector=%q failed: %s", semantic, selector, reason)
			return false, reason
		}
		s.enrichHitSelector(ctx)
		occluded, err := s.checkOcclusion(ctx, target)
		if err == nil && occluded {
			return false, "occluded"
		}
		// §5.0.14 决议 0③：extract 同样在派发（extractAt）之前回传受体。
		s.notifyPreDispatchReceptor(target)
		text, err := s.extractAt(ctx, target)
		if err != nil {
			return false, "extract_failed"
		}
		s.clearMarker(ctx)
		return true, text
	default:
		s.clearMarker(ctx)
		return false, "unsupported_action"
	}
}

// ExecuteActionByNode 实现 browser.NodeActionSession：按探针 node_id 确定性引用
// 执行（v1.5.5 混合指挥）。node_id == 元素节点 BFS 队列索引（与 probeCollectExpr
// 的 children 遍历同序、C++ next_id_++ 一一对应）。跳过语义/选择器派发，先
// resolveNodeById 回查定位器（marker + rect），再走与语义点击一致的物理执行。
func (s *cdpSession) ExecuteActionByNode(ctx context.Context, action, value string, nodeID int32) (bool, string) {
	return s.executeActionByNode(ctx, action, value, nodeID)
}

// executeActionByNode 是 node_id 确定性执行的物理实现（click/input/select/extract）。
func (s *cdpSession) executeActionByNode(ctx context.Context, action, value string, nodeID int32) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, semantics.ActionBudget)
	defer cancel()
	disp := &dispatchTracker{}
	ok, observed := s.executeActionByNodeInner(ctx, disp, action, value, nodeID)
	if ok {
		return ok, observed
	}
	if obs, canceled := canceledObserved(ctx, disp.Dispatched()); canceled {
		return false, obs
	}
	return ok, observed
}

// executeActionByNodeInner 是 node_id 路径的实体：物理派发点用 disp.Mark() 记下
// 「事件已经出去」，供中断时区分可重放与结果未知。
func (s *cdpSession) executeActionByNodeInner(ctx context.Context, disp *dispatchTracker, action, value string, nodeID int32) (bool, string) {
	s.mu.Lock()
	dead := s.destroyed
	s.mu.Unlock()
	if dead {
		return false, "target_destroyed"
	}
	if action != "drag" {
		s.mu.Lock()
		challenge := s.challenge
		s.mu.Unlock()
		if challenge {
			return false, "challenge_intercepted"
		}
	}

	target, err := resolveNodeById(ctx, s, nodeID)
	if err != nil || !target.Found {
		if err != nil {
			return false, "node_resolve_failed:" + err.Error()
		}
		return false, fmt.Sprintf("node_not_found:%d", nodeID)
	}

	switch action {
	case "click":
		s.enrichHitSelector(ctx)
		occluded, oerr := s.checkOcclusion(ctx, target)
		if oerr == nil && occluded {
			return false, "occluded"
		}
		disp.Mark()
		if cerr := s.clickAt(ctx, target); cerr != nil {
			return false, "click_failed:" + cerr.Error()
		}
		s.clearMarker(ctx)
		return true, fmt.Sprintf("click:node:%d", nodeID)
	case "input", "fill", "select":
		// §5.0.11 条款 1：标记由 inputAt 在派发前校验通过后打。
		if ierr := s.inputAt(ctx, disp, target, action, value, ""); ierr != nil {
			return false, "input_failed:" + ierr.Error()
		}
		s.enrichHitSelector(ctx)
		s.clearMarker(ctx)
		return true, fmt.Sprintf("%s:node:%d", action, nodeID)
	case "extract":
		s.enrichHitSelector(ctx)
		occluded, oerr := s.checkOcclusion(ctx, target)
		if oerr == nil && occluded {
			return false, "occluded"
		}
		text, terr := s.extractAt(ctx, target)
		if terr != nil {
			return false, "extract_failed"
		}
		s.clearMarker(ctx)
		return true, text
	default:
		s.clearMarker(ctx)
		return false, "unsupported_action"
	}
}

// ── 站点 LLM 模型桥的宿主接线（复杂度治理阶段 2：实体在 pkg/browser/modelbridge） ──
// bridgeSession 把 *cdpSession 的六个能力暴露给子包；bridgeDispatch 把
// dispatchTracker（可空）暴露为标记面。下面六个薄包装让本包调用点保持不变。

type bridgeSession struct{ s *cdpSession }

func (b bridgeSession) EvalValue(ctx context.Context, expr string, dst any) error {
	return b.s.evalValue(ctx, expr, dst)
}
func (b bridgeSession) EvalBool(ctx context.Context, expr string) (bool, error) {
	return b.s.evalBool(ctx, expr)
}
func (b bridgeSession) EvalResult(ctx context.Context, expr string) (string, error) {
	return b.s.evalResult(ctx, expr)
}
func (b bridgeSession) ClickAt(ctx context.Context, t modelbridge.Target) error {
	return b.s.clickAt(ctx, elementTarget{Found: t.Found, X: t.X, Y: t.Y, W: t.W, H: t.H, Text: t.Text})
}
func (b bridgeSession) FindElementOpt(ctx context.Context, semantic, selector string, preferLast bool) (modelbridge.Target, error) {
	t, err := findElementOpt(ctx, b.s, semantic, selector, preferLast)
	return modelbridge.Target{Found: t.Found, X: t.X, Y: t.Y, W: t.W, H: t.H, Text: t.Text}, err
}
func (b bridgeSession) Call(ctx context.Context, method string, params any, dst any) error {
	return b.s.client.CallSession(ctx, b.s.sessionID, method, params, dst)
}

// bridgeDispatch 是 modelbridge.Dispatch 的宿主实现（nil 指针安全）。
type bridgeDispatch struct{ d *dispatchTracker }

func (d bridgeDispatch) Mark() {
	if d.d != nil {
		d.d.Mark()
	}
}

func (s *cdpSession) switchGeminiModel(ctx context.Context, disp *dispatchTracker, label string) error {
	return modelbridge.SwitchGeminiModel(ctx, bridgeSession{s}, bridgeDispatch{d: disp}, label)
}

func (s *cdpSession) switchGeminiThinking(ctx context.Context, disp *dispatchTracker, wantOn bool) error {
	return modelbridge.SwitchGeminiThinking(ctx, bridgeSession{s}, bridgeDispatch{d: disp}, wantOn)
}

func (s *cdpSession) switchDeepSeekMode(ctx context.Context, disp *dispatchTracker, label string) error {
	return modelbridge.SwitchDeepSeekMode(ctx, bridgeSession{s}, bridgeDispatch{d: disp}, label)
}

func (s *cdpSession) toggleDeepSeekSwitch(ctx context.Context, disp *dispatchTracker, text string, wantOn bool) error {
	return modelbridge.ToggleDeepSeekSwitch(ctx, bridgeSession{s}, bridgeDispatch{d: disp}, text, wantOn)
}

func (s *cdpSession) closeGeminiMenu(ctx context.Context) error {
	return modelbridge.CloseGeminiMenu(ctx, bridgeSession{s})
}

func (s *cdpSession) geminiInputEmpty(ctx context.Context) bool {
	return modelbridge.GeminiInputEmpty(ctx, bridgeSession{s})
}
