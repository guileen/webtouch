// 点击/输入自愈（2026-08-25 用户实测反馈：页面加载慢时点击会点歪，
// 触发录音/弹层后无法继续，像按键精灵缺恢复能力）。
//
// 本文件给物理层加「定位稳定 → 点击 → 语义自检 → 意外状态恢复 → 重试」闭环：
//   - locateStableElement：有界等待元素可见、可交互（非 disabled/pointer-events:none）
//     且几何位置连续两次采样稳定（SPA 动画/页面未就绪时点击不再落空）；
//   - detectStrayUI / recoverStrayUI：点击点歪的副作用（语音输入激活、菜单/弹层
//     打开、aria-busy 遮罩）先恢复再重试，而不是直接失败；
//   - verifyGeminiClick / verifyInput：站点语义自检（发送=输入清空、新对话=回答区
//     重置、输入=已落盘非空），未达预期自动进入恢复/重试。
//
// 复杂度治理·阶段 2·单元 2c（2026-09-29）：整文件自宿主 `pkg/browser/click_recovery.go`
// 迁入本包；宿主经 `Session` 注入能力（见 `session.go`），宿主只留 `bridgeInputSession`
// 适配器与 4 个结构化代号的常量别名（`interfaces.go`）。
package input

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

// strayUIState 描述点击后检测到的意外界面状态（点歪副作用）。
type strayUIState struct {
	Dialog bool `json:"dialog"` // 模态/弹层（aria-modal / role=dialog）
	Menu   bool `json:"menu"`   // Gemini 模型菜单（gem-mode-menu）
	Mic    bool `json:"mic"`    // 语音输入（录音）激活
	Busy   bool `json:"busy"`   // 页面 aria-busy 加载遮罩
}

// geminiStrayUIJS 检测点击后的意外界面状态（通用弹层 + Gemini 专属菜单/语音）。
const geminiStrayUIJS = `(() => {
  const vis = el => {
    if (!el) return false;
    const cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const out = {dialog: false, menu: false, mic: false, busy: false};
  out.dialog = !!Array.from(document.querySelectorAll('[aria-modal="true"], [role="dialog"]')).find(vis);
  out.menu = !!Array.from(document.querySelectorAll('gem-menu[data-testid="gem-mode-menu"], [data-testid="gem-mode-menu"]')).find(vis);
  out.menu = out.menu || !!Array.from(document.querySelectorAll('gem-menu-item')).find(vis);
  out.mic = !!Array.from(document.querySelectorAll(
    'button[aria-label*="语音"][aria-pressed="true"], button[aria-label*="voice"][aria-pressed="true"],' +
    'button[data-testid*="voice"][aria-pressed="true"], [class*="listening"]'
  )).find(vis);
  out.busy = !!(document.body && document.body.getAttribute('aria-busy') === 'true');
  return out;
})()`

// detectStrayUI 读取当前页面意外状态（点歪副作用检测）。
func detectStrayUI(ctx context.Context, s Session) (strayUIState, error) {
	var st strayUIState
	if err := s.EvalValue(ctx, geminiStrayUIJS, &st); err != nil {
		return strayUIState{}, err
	}
	return st, nil
}

// geminiVoiceToggleSel 是 Gemini 语音输入开关按钮选择器（点歪后停录用）。
const geminiVoiceToggleSel = `button[aria-label*="语音"], button[aria-label*="voice"], button[data-testid*="voice"]`

// recoverStrayUI 恢复点歪副作用：语音激活→再点开关停录；菜单/弹层→Escape。
// 一次恢复后复查，未恢复再兜底一次（Escape 对录音态不一定生效）。
func recoverStrayUI(ctx context.Context, s Session, st strayUIState) error {
	if st.Mic {
		if t, err := findElementOpt(ctx, s, "gemini_voice_toggle", geminiVoiceToggleSel, false); err == nil && t.Found {
			_ = s.ClickAt(ctx, t)
			time.Sleep(300 * time.Millisecond)
		}
	}
	if st.Menu || st.Dialog {
		_ = s.CloseGeminiMenu(ctx)
	}
	time.Sleep(300 * time.Millisecond)
	again, err := detectStrayUI(ctx, s)
	if err != nil || !(again.Mic || again.Menu || again.Dialog) {
		return err
	}
	if again.Mic {
		if t, err := findElementOpt(ctx, s, "gemini_voice_toggle", geminiVoiceToggleSel, false); err == nil && t.Found {
			_ = s.ClickAt(ctx, t)
		}
	}
	_ = s.CloseGeminiMenu(ctx)
	time.Sleep(300 * time.Millisecond)
	return nil
}

// elementReadyJS 判断已标记（data-uws-target）元素是否「可交互」：
// 未 disabled / aria-disabled / pointer-events:none，且可见。
const elementReadyJS = `(() => {
  const el = document.querySelector('[data-uws-target="1"]');
  if (!el) return false;
  if (el.disabled === true || el.getAttribute('aria-disabled') === 'true') return false;
  const cs = getComputedStyle(el);
  if (cs.pointerEvents === 'none') return false;
  const r = el.getBoundingClientRect();
  return r.width >= 2 && r.height >= 2;
})()`

// elementReady 检查已标记元素是否可交互（false = 页面还在渲染/按钮禁用）。
func elementReady(ctx context.Context, s Session) bool {
	ok, err := s.EvalBool(ctx, elementReadyJS)
	return err == nil && ok
}

// locateStableElement 有界等待目标元素「可见 + 可交互 + 几何稳定」：
// 每次定位后采样几何，间隔 200ms 再定位一次；中心位移 <3px 视为稳定。
// 页面未就绪（元素不存在/动画中）时持续重试；超时返回最后一次命中
// （至少不再误点「页面加载中的占位坐标」）。
// editableOnly（2026-09-25，§5.0.3 条款 1/2）：input/fill/select 文本汇支路只接受
// 可编辑目标，定位层在可编辑候选内择优（click 通道必须传 false，行为不变）。
func locateStableElement(ctx context.Context, s Session, semantic, selector string, preferLast, needEnabled, editableOnly bool) (ElementTarget, error) {
	deadline := time.Now().Add(3500 * time.Millisecond)
	var last ElementTarget
	for {
		t, err := s.FindElementMode(ctx, semantic, selector, preferLast, false, editableOnly)
		found := err == nil && t.Found
		if found {
			if !needEnabled || elementReady(ctx, s) {
				if last.Found && math.Abs(last.X-t.X) < 3 && math.Abs(last.Y-t.Y) < 3 {
					return t, nil
				}
				last = t
			}
		}
		if !time.Now().Before(deadline) {
			if t.Found {
				return t, nil
			}
			return ElementTarget{}, fmt.Errorf("element %q not ready", semantic)
		}
		select {
		case <-ctx.Done():
			return ElementTarget{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// needSendEnabled 发送类语义要求按钮已启用（输入后 Angular 才启用发送）。
func needSendEnabled(semantic string) bool {
	return semantic == "gemini_send" || semantic == "send_button"
}

// IsGeminiClick 判断语义是否属于 Gemini 页面点击（启用自愈恢复）。
func IsGeminiClick(semantic string) bool {
	return strings.HasPrefix(semantic, "gemini_")
}

// clickWithRecovery 是带自愈的物理点击：
// 定位稳定元素 → 遮挡检测 → 点击 → 语义自检 → 意外状态恢复 → 重试（≤3 次）。
// 解决「页面未就绪时点歪 → 录音/弹层 → 无法继续」的按键精灵式失败。
// disp 为派发标记（§5.0.11 条款 1）：只能在定位与遮挡/恢复判定**通过之后**、
// clickAt 即将派发鼠标事件之前标记；nil 表示调用方不关心中断分类。
func ClickWithRecovery(ctx context.Context, s Session, disp *DispatchTracker, semantic, selector string) error {
	const maxAttempts = 3
	needEnabled := needSendEnabled(semantic)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if s.TargetDestroyed() {
			return fmt.Errorf("target_destroyed")
		}
		target, err := locateStableElement(ctx, s, semantic, selector, false, needEnabled, false)
		if err != nil || !target.Found {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if semantic != "gemini_new_chat" {
			occluded, oerr := s.CheckOcclusion(ctx, target)
			if oerr == nil && occluded {
				// 弹层/菜单遮挡：先恢复界面（Escape/停录）再重试。
				if st, serr := detectStrayUI(ctx, s); serr == nil && (st.Dialog || st.Menu || st.Mic) {
					_ = recoverStrayUI(ctx, s, st)
				}
				time.Sleep(300 * time.Millisecond)
				continue
			}
		}
		// §5.0.14 决议 0③：受体（target）已解析、事件未发出——先回传再打派发标记。
		s.NotifyPreDispatchReceptor(target)
		if disp != nil {
			disp.Mark() // 鼠标轨迹从这里开始发给 Chrome，之后中断只能报「结果未知」
		}
		if err := s.ClickAt(ctx, target); err != nil {
			return fmt.Errorf("click failed: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
		if verifyGeminiClick(ctx, s, semantic) {
			return nil
		}
		if st, serr := detectStrayUI(ctx, s); serr == nil && (st.Dialog || st.Menu || st.Mic) {
			_ = recoverStrayUI(ctx, s, st)
			continue
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("click %q failed after %d attempts", semantic, maxAttempts)
}

// verifyGeminiClick 语义自检：点击是否真的生效（而非只派发了鼠标事件）。
func verifyGeminiClick(ctx context.Context, s Session, semantic string) bool {
	switch semantic {
	case "gemini_send":
		// 发送成功 = Angular 提交后输入区清空（原有自愈语义，有界等待）。
		for i := 0; i < 4; i++ {
			if s.GeminiInputEmpty(ctx) {
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		return false
	case "gemini_new_chat":
		// 新对话成功 = 回答区清空 + 输入区清空（SPA 路由完成）。
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var ok bool
			if err := s.EvalValue(ctx, `(() => {
			  const hasAnswers = !!document.querySelector('.model-response-text, structured-content-container, message-content .markdown');
			  const e = document.querySelector('.ql-editor, [contenteditable="true"]');
			  const emptyInput = !e || !((e.innerText || '').trim());
			  return !hasAnswers && emptyInput;
			})()`, &ok); err == nil && ok {
				return true
			}
			time.Sleep(300 * time.Millisecond)
		}
		return false
	default:
		// 通用点击：没有意外弹层/录音态即视为成功（页面微调可接受）。
		if st, err := detectStrayUI(ctx, s); err == nil {
			return !st.Mic && !st.Dialog
		}
		return true
	}
}

// 结构化遥测故障代号（spec/algorithm-kernel.md §5.0.8）
const (
	FaultInputOccluded     = "INPUT_OCCLUDED"
	FaultTargetNotEditable = "TARGET_NOT_EDITABLE"
	FaultWriteNotPersisted = "WRITE_NOT_PERSISTED"
	FaultLocateNoCandidate = "LOCATE_NO_CANDIDATE"
)

// inputWithRecovery 带自愈的输入：定位稳定编辑器 → 可编辑性门禁 → 遮挡检测 → 输入 →
// 自检「输入值真的落进该元素」；未落地重试（spec/algorithm-kernel.md §5.0.3/§5.0.8 条款）。
// disp 透传给 inputAt：标记落在「守卫/可编辑/重锚全部通过、输入事件即将发出」的时点
// （§5.0.11 条款 1/2）——定位阶段的失败与取消不得被记成「已派发」。
func InputWithRecovery(ctx context.Context, s Session, disp *DispatchTracker, semantic, selector, action, value string) error {
	const maxAttempts = 3
	// 文本汇支路：input/fill 恒为文本汇；select 只有落到原生 <select> 才算——select 作用于
	// 自定义下拉触发器时目标是菜单触发器、成功判据是选项点击（2026-08-31 条款），保持原行为。
	textSink := action != "select"
	reason, evidence := FaultLocateNoCandidate, ""
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if s.TargetDestroyed() {
			return fmt.Errorf("target_destroyed")
		}
		target, err := locateStableElement(ctx, s, semantic, selector, false, false, textSink)
		if err != nil || !target.Found {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			reason = FaultLocateNoCandidate
			evidence = fmt.Sprintf("locateStableElement 未定位到候选: %v", err)
			log.Printf("uws input recovery semantic=%q attempt=%d: locate failed: %v", semantic, attempt, err)
			time.Sleep(250 * time.Millisecond)
			continue
		}
		sink := textSink || target.Tag == "select"
		if sink && !target.Editable {
			// §5.0.3 条款 1/3：非可编辑命中不得派发（旧行为靠元素原生文案非空误报成功）。
			reason = FaultTargetNotEditable
			evidence = fmt.Sprintf("%s %q", target.Tag, target.Text)
			log.Printf("uws input gate semantic=%q rejected tag=%s text=%q matched=%q selector=%q",
				semantic, target.Tag, target.Text, target.Matched, selector)
			time.Sleep(250 * time.Millisecond)
			continue
		}
		occluded, oerr := s.CheckOcclusion(ctx, target)
		if oerr == nil && occluded {
			reason = FaultInputOccluded
			evidence = fmt.Sprintf("target %s at (%.1f, %.1f) occluded", target.Tag, target.X, target.Y)
			log.Printf("uws input occluded semantic=%q attempt=%d: tag=%s center=(%.1f, %.1f) occluded",
				semantic, attempt, target.Tag, target.X, target.Y)
			if st, serr := detectStrayUI(ctx, s); serr == nil && (st.Dialog || st.Menu || st.Mic) {
				_ = recoverStrayUI(ctx, s, st)
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if attempt > 0 {
			// §5.0.3 条款 5：重试前让选区覆盖已有内容，使写入幂等（杜绝 ddg 的 webtouchwebtouch）。
			if _, serr := s.EvalBool(ctx, SelectInputContentJS); serr != nil {
				log.Printf("uws input retry select-all failed semantic=%q: %v", semantic, serr)
			}
		}
		// §5.0.14 决议 0③：可编辑门禁与遮挡判定皆已通过、输入事件未发出——先回传受体。
		s.NotifyPreDispatchReceptor(target)
		if err := s.InputAt(ctx, disp, target, action, value, semantic); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			evidence = err.Error()
			if strings.HasPrefix(evidence, FaultTargetRoleMismatch) {
				// §5.0.9 条款 1：守卫熔断是确定性拒绝（受体角色不会因重试而改变），
				// 立即返回、不重试、不叠写。
				log.Printf("uws input guard semantic=%q blocked (no retry): %s", semantic, evidence)
				return fmt.Errorf("%s", evidence)
			}
			if strings.HasPrefix(evidence, FaultTargetTopologyRupture) {
				// §5.0.10 条款 3：拓扑断裂是确定性熔断（租约过期/指纹破坏/位移超限已定），
				// 立即返回、不重试、不换出口——重试只会把一次派发变成多次。
				log.Printf("uws input reanchor refused semantic=%q (no retry): %s", semantic, evidence)
				return fmt.Errorf("%s", evidence)
			}
			if strings.HasPrefix(evidence, "input_not_editable") {
				reason = FaultTargetNotEditable
			} else {
				reason = FaultWriteNotPersisted
			}
			log.Printf("uws input dispatch failed semantic=%q attempt=%d: %v", semantic, attempt, err)
			time.Sleep(300 * time.Millisecond)
			continue
		}
		landWant := value
		if !sink {
			landWant = "" // 自定义下拉：成功判据是选项点击，不由文本落地裁定
		}
		if InputLanded(ctx, s, landWant) {
			return nil
		}
		reason = FaultWriteNotPersisted
		evidence = "回读值未包含输入值"
		log.Printf("uws input land check failed semantic=%q attempt=%d: want=%q tag=%s",
			semantic, attempt, landWant, target.Tag)
		time.Sleep(300 * time.Millisecond)
	}
	if evidence == "" {
		return fmt.Errorf("%s after %d attempts", reason, maxAttempts)
	}
	return fmt.Errorf("%s: %s (after %d attempts)", reason, evidence, maxAttempts)
}

// inputLanded 自检输入是否真的落进目标元素（spec/algorithm-kernel.md §5.0.3 条款 4）：
// 从**同一元素**回读可编辑内容，want 非空时要求「回读值包含输入值」——元素原生文案非空
// 不再算落地（旧行为在 span/button 上必然误判成功）。want == "" 是自定义下拉支路
// （选项点击已是确定性提交），仍按「回读非空」判定。
func InputLanded(ctx context.Context, s Session, want string) bool {
	var snap struct {
		Found    bool   `json:"found"`
		Tag      string `json:"tag"`
		Value    string `json:"value"`
		Text     string `json:"text"`
		Editable bool   `json:"editable"`
	}
	if err := s.EvalValue(ctx, InputTargetSnapshotJS, &snap); err != nil || !snap.Found {
		return false
	}
	if want == "" {
		return snap.Editable || strings.TrimSpace(snap.Value) != "" || strings.TrimSpace(snap.Text) != ""
	}
	if !snap.Editable {
		return false
	}
	return InputValueLanded(snap.Value, want) || InputValueLanded(snap.Text, want)
}

// findElementOpt 是本包对宿主 `findElementOpt` 的等价入口（非严格、非仅可编辑候选），
// 由 `Session.FindElementMode` 现取——不额外占用接口方法。
func findElementOpt(ctx context.Context, s Session, semantic, selector string, preferLast bool) (ElementTarget, error) {
	return s.FindElementMode(ctx, semantic, selector, preferLast, false, false)
}
