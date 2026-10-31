// DeepSeek 网页「快速/专家/识图」三模式 + 深度思考/智能搜索开关物理动作
// （2026-08-25 实测 DOM）。
//
// 站点专属能力收口：语义层只需传目标模式名（快速模式/专家模式/识图模式）或
// 开关名（深度思考/智能搜索）+ on/off，物理层负责定位、真实鼠标点击与状态校验。
// 与 Gemini 的 gemini_model_switch/gemini_thinking_switch 同构，但有两个关键差异：
//  1. 模式单选组只出现在「新会话」落地页（chat.deepseek.com/），一旦进入会话
//     （/a/chat/s/...）即消失，因此模式只能在新会话建立时切换；
//  2. 智能搜索开关在「专家模式」下不存在（用户 2026-08-25 明确），需显式报错。
package modelbridge

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ModeOption 是 DeepSeek 新会话落地页的模式单选项（页面 JS 求值返回）。
type ModeOption struct {
	Text    string  `json:"text"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	W       float64 `json:"w"`
	H       float64 `json:"h"`
	Checked bool    `json:"checked"`
}

// ModeOptionsJS 读取可见模式单选项（div[role="radio"]）。
// 选中态以 aria-checked="true" 为准（hash 类名随版本变化，不可依赖）。
const ModeOptionsJS = `(() => {
  const norm = s => (s == null ? '' : String(s)).replace(/\s+/g, ' ').trim();
  const visible = el => {
    const cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none') return false;
    const r = el.getBoundingClientRect();
    return r.width >= 2 && r.height >= 2;
  };
  return Array.from(document.querySelectorAll('div[role="radio"]'))
    .filter(visible)
    .map(el => {
      const r = el.getBoundingClientRect();
      return {
        text: norm(el.innerText),
        x: r.left + r.width / 2,
        y: r.top + r.height / 2,
        w: r.width,
        h: r.height,
        checked: (el.getAttribute('aria-checked') || '') === 'true',
      };
    });
})()`

// deepseekModeStateJS 读取当前选中模式文本（aria-checked=true 的 radio）。
const deepseekModeStateJS = `(() => {
  const el = Array.from(document.querySelectorAll('div[role="radio"]'))
    .find(e => (e.getAttribute('aria-checked') || '') === 'true');
  return el ? (el.innerText || '').replace(/\s+/g, ' ').trim() : '';
})()`

// Toggle 是 DeepSeek 输入栏开关（深度思考/智能搜索）状态与几何描述。
type Toggle struct {
	Found    bool    `json:"found"`
	Selected bool    `json:"selected"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	Text     string  `json:"text"`
}

// ToggleStateJS 读取指定文本的开关状态（选中态 = class 含
// ds-toggle-button--selected）。返回 null 表示开关不存在（如专家模式无智能搜索）。
const ToggleStateJS = `(() => {
  const want = WANT;
  const norm = s => (s == null ? '' : String(s)).replace(/\s+/g, ' ').trim();
  const el = Array.from(document.querySelectorAll('div.ds-toggle-button'))
    .find(e => norm(e.innerText) === want);
  if (!el) return null;
  const r = el.getBoundingClientRect();
  return {
    found: true,
    selected: (el.className || '').toString().includes('ds-toggle-button--selected'),
    x: r.left + r.width / 2,
    y: r.top + r.height / 2,
    w: r.width,
    h: r.height,
    text: norm(el.innerText),
  };
})()`

// switchDeepSeekMode 把 DeepSeek 新会话落地页的模式切到 label（快速模式/专家模式/
// 识图模式）。模式单选组只在新会话落地页出现；在现代 DeepSeek（无 radio 模式组，
// 仅有「深度思考」与「智能搜索」开关）平滑退化为深度思考开关联动。
func SwitchDeepSeekMode(ctx context.Context, sess Session, disp Dispatch, label string) error {
	options, err := ReadDeepSeekModes(ctx, sess)
	if err == nil && len(options) > 0 {
		idx := PickDeepSeekMode(options, label)
		if idx < 0 {
			names := make([]string, 0, len(options))
			for _, o := range options {
				names = append(names, o.Text)
			}
			return fmt.Errorf(
				"目标模式 %q 不在可用列表（%s）——模式选择仅在新会话落地页出现，请用 new_session 重建会话",
				label, strings.Join(names, " / "))
		}
		target := options[idx]
		if !target.Checked {
			if disp != nil {
				disp.Mark() // 模式组已读出、目标项未选中，鼠标事件从这里开始发出（§5.0.11 条款 1）
			}
			if err := sess.ClickAt(ctx, Target{
				Found: true, X: target.X, Y: target.Y, W: target.W, H: target.H, Text: target.Text,
			}); err != nil {
				return fmt.Errorf("点击模式选项失败: %w", err)
			}
			time.Sleep(500 * time.Millisecond)
		}
		state, _ := sess.EvalResult(ctx, deepseekModeStateJS)
		if !ModeNameEqual(state, label) {
			return fmt.Errorf("模式未切换成功（当前 %q）", state)
		}
		return nil
	}

	// 现代 DeepSeek UI（无 radio 模式组，仅有「深度思考」与「智能搜索」开关）：
	// 通用模式 / 快速模式 / 识图模式 → 深度思考: off
	// 深度思考 / 专家模式 → 深度思考: on
	switch label {
	case "通用模式", "快速模式", "识图模式":
		return ToggleDeepSeekSwitch(ctx, sess, disp, "深度思考", false)
	case "深度思考", "专家模式":
		return ToggleDeepSeekSwitch(ctx, sess, disp, "深度思考", true)
	default:
		return fmt.Errorf("未知 DeepSeek 模式标签 %q", label)
	}
}

// readDeepSeekModes 有界等待模式单选组出现并返回可见项。
// 若页面已出现现代开关（ds-toggle-button）但无 radio 组，则快速跳过等待。
func ReadDeepSeekModes(ctx context.Context, sess Session) ([]ModeOption, error) {
	var options []ModeOption
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		options = nil
		if err := sess.EvalValue(ctx, ModeOptionsJS, &options); err == nil && len(options) > 0 {
			return options, nil
		}
		// 若已出现现代输入栏开关且无 radio，立即退出等待
		hasToggle, _ := sess.EvalBool(ctx, `Boolean(document.querySelector('div.ds-toggle-button'))`)
		if hasToggle {
			return nil, fmt.Errorf("已是现代 DeepSeek UI（开关形式，无 radio 模式组）")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("DeepSeek 模式选择未出现（可能已进入会话，模式仅在新会话落地页可选）")
}

// toggleDeepSeekSwitch 把指定文本（深度思考/智能搜索）开关切到 wantOn。
// 开关在落地页与进行中的会话输入栏均存在；专家模式无智能搜索时显式报错。
func ToggleDeepSeekSwitch(ctx context.Context, sess Session, disp Dispatch, text string, wantOn bool) error {
	read := func() (Toggle, error) {
		expr := strings.ReplaceAll(ToggleStateJS, "WANT", QuoteJS(text))
		var t Toggle
		if err := sess.EvalValue(ctx, expr, &t); err != nil {
			return t, err
		}
		return t, nil
	}
	t, err := read()
	if err != nil {
		return err
	}
	if !t.Found {
		// 开关不存在：想「关掉」时视为已满足（专家模式无智能搜索 = 天然关闭）；
		// 想「打开」时显式报错（专家模式无法开启智能搜索）。
		if wantOn {
			return fmt.Errorf("%s 开关在当前模式下不可用", text)
		}
		return nil
	}
	if t.Selected != wantOn {
		if disp != nil {
			disp.Mark() // 开关状态已读出且与期望不同，鼠标事件从这里开始发出（§5.0.11 条款 1）
		}
		if err := sess.ClickAt(ctx, Target{
			Found: true, X: t.X, Y: t.Y, W: t.W, H: t.H, Text: t.Text,
		}); err != nil {
			return fmt.Errorf("点击 %s 开关失败: %w", text, err)
		}
		time.Sleep(500 * time.Millisecond)
		t2, rerr := read()
		if rerr != nil {
			return rerr
		}
		if t2.Found && t2.Selected != wantOn {
			return fmt.Errorf("%s 开关未切换到 %v（当前 %v）", text, wantOn, t2.Selected)
		}
	}
	return nil
}

// PickDeepSeekMode 在模式单选项中定位目标：先精确归一匹配，再退化前缀包含。
func PickDeepSeekMode(options []ModeOption, want string) int {
	wantNorm := normalizeText(want)
	if wantNorm == "" {
		return -1
	}
	for i, o := range options {
		t := normalizeText(o.Text)
		if t == wantNorm || strings.HasPrefix(t, wantNorm) || strings.HasPrefix(wantNorm, t) {
			return i
		}
	}
	return -1
}

// ModeNameEqual 判断页面当前模式文本与目标模式是否归一化一致。
func ModeNameEqual(got, want string) bool {
	got = normalizeText(got)
	want = normalizeText(want)
	if got == "" || want == "" {
		return false
	}
	return got == want || strings.HasPrefix(got, want) || strings.HasPrefix(want, got)
}
