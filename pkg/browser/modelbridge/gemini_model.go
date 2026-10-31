// Gemini 网页模型切换物理动作（2026-08-25 实测 DOM）。
//
// 站点专属能力收口：模型选择器按钮 + 菜单项（含「Extended thinking」这类
// 无 bard-mode-option testid 的档位）+ 切换后校验。语义层只需传目标显示名
// （如 "3.6 Flash" / "Flash" / "Extended thinking"），物理层负责开菜单、
// token 匹配、真实鼠标点击与短名校验。
package modelbridge

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const geminiPickerSelector = "[data-test-id='bard-mode-menu-button'], button[aria-label^='Open mode picker, currently ']"

var geminiPickerLabelJS = fmt.Sprintf(`(() => {
 const b=[...document.querySelectorAll(%q)].find(e=>{const r=e.getBoundingClientRect();return r.width>0&&r.height>0&&getComputedStyle(e).visibility!=="hidden";});
 return b ? (b.innerText || '').trim() : '';
})()`, geminiPickerSelector)

// MenuOption 是 Gemini 模型菜单项的几何 + 文本描述（页面 JS 求值返回）。
type MenuOption struct {
	Text     string  `json:"text"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	Selected bool    `json:"selected"`
}

// MenuOptionsJS 读取模型选择器内全部可见菜单项（含 Extended thinking 等
// 非 bard-mode-option 前缀的档位；scope 到 gem-mode-menu 防止串到其它菜单）。
//
// 真实页（2026-09-26 观测）菜单容器是 <gem-menu role="menu" data-test-id="gem-mode-menu">：
// 属性名带连字符，无连字符的 data-testid 写法在线上已不存在。菜单还是**懒渲染**的——
// 未打开时 DOM 里只有空的 gem-popover 占位，gem-menu / gem-menu-item 都不存在，
// 所以必须先真实点击打开再由本表达式读取。
//
// 收口纪律：只按 gem-mode-menu 的两种属性拼法认容器，**不用泛化的 role="menu" 兜底**
// （同站还有别的下拉菜单，泛化匹配会把无关项当模型项点击）。只要容器存在就只读容器，
// 容器存在但还没渲染出可见项时返回空、由调用方继续轮询——绝不回退整文档，否则会把
// 其它菜单项混进来。仅当页面上确实找不到容器时才降级到整文档（旧页面兼容）。
const MenuOptionsJS = `(() => {
  const panelSel = 'gem-menu[data-test-id="gem-mode-menu"], gem-menu[data-testid="gem-mode-menu"], [data-test-id="gem-mode-menu"], [data-testid="gem-mode-menu"]';
  const norm = s => (s == null ? '' : String(s)).replace(/\s+/g, ' ').trim();
  const visible = el => {
    const cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none') return false;
    const r = el.getBoundingClientRect();
    return r.width >= 2 && r.height >= 2;
  };
  const read = root => [...root.querySelectorAll('gem-menu-item')].filter(visible).map(el => {
    const r = el.getBoundingClientRect();
    return {text: norm(el.innerText), x: r.x + r.width / 2, y: r.y + r.height / 2,
            w: r.width, h: r.height, selected: (el.className || '').toString().includes('selected')};
  });
  const panels = [...document.querySelectorAll(panelSel)];
  const roots = panels.length ? panels : [document];
  for (const root of roots) {
    const items = read(root);
    if (items.length) return items;
  }
  return [];
})()`

// openGeminiPicker 点击模型选择器按钮并等待菜单出现（有界等待 SPA 启动）。
// 选择器按钮标签不可写死：桌面宽屏是原生 <button>，窄屏 responsive UI 实测
// 渲染为 <gem-button>（自定义元素，2026-08-26），必须按 data-test-id 匹配任意标签。
func OpenGeminiPicker(ctx context.Context, sess Session) (Target, error) {
	const pickerSel = geminiPickerSelector
	var picker Target
	var err error
	pickerDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(pickerDeadline) {
		picker, err = sess.FindElementOpt(ctx, "gemini_model_picker", pickerSel, false)
		if err == nil && picker.Found {
			break
		}
		select {
		case <-ctx.Done():
			return picker, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if err != nil || !picker.Found {
		return picker, fmt.Errorf("模型选择器按钮未找到: %v", err)
	}
	if err := sess.ClickAt(ctx, picker); err != nil {
		return picker, fmt.Errorf("打开模型选择器失败: %w", err)
	}
	return picker, nil
}

// waitGeminiMenu 等待模型菜单项出现并返回可见项（含 Extended thinking 开关）。
func WaitGeminiMenu(ctx context.Context, sess Session) ([]MenuOption, error) {
	var options []MenuOption
	menuDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(menuDeadline) {
		options = nil
		if verr := sess.EvalValue(ctx, MenuOptionsJS, &options); verr == nil && len(options) > 0 {
			return options, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("模型菜单未出现")
}

// switchGeminiModel 在 gemini.google.com 页面上把模型切换到 label：
//  1. 打开模型选择器（有界等待 SPA 启动）；
//  2. 按标签（token 全包含匹配）定位目标并真实鼠标点击；
//  3. 校验选择器按钮短名精确匹配目标模型的族名（"Flash" ≠ "Flash-Lite"）。
//
// 游客态 / 未解锁的档位（点击后按钮文本不变）会返回显式错误。
func SwitchGeminiModel(ctx context.Context, sess Session, disp Dispatch, label string) error {
	var current string
	if err := sess.EvalValue(ctx, geminiPickerLabelJS, &current); err == nil && strings.EqualFold(normalizeText(current), lastToken(normalizeText(label))) && current != "" {
		return nil
	}
	if _, err := OpenGeminiPicker(ctx, sess); err != nil {
		return err
	}
	options, err := WaitGeminiMenu(ctx, sess)
	if err != nil {
		// 菜单状态可能错乱（上次未关/被页面打断）：先关闭再重开一次。
		_ = CloseGeminiMenu(ctx, sess)
		time.Sleep(300 * time.Millisecond)
		if _, oerr := OpenGeminiPicker(ctx, sess); oerr != nil {
			return oerr
		}
		options, err = WaitGeminiMenu(ctx, sess)
		if err != nil {
			return err
		}
	}
	idx := PickGeminiOption(options, label)
	if idx < 0 {
		names := make([]string, 0, len(options))
		for _, o := range options {
			names = append(names, o.Text)
		}
		return fmt.Errorf("目标模型 %q 不在可用列表（%s）——游客账号可能被锁定，请先登录",
			label, strings.Join(names, " / "))
	}
	target := options[idx]
	if disp != nil {
		disp.Mark() // 菜单已定位、选项已选定，鼠标事件从这里开始发出（§5.0.11 条款 1）
	}
	if err := sess.ClickAt(ctx, Target{
		Found: true, X: target.X, Y: target.Y, W: target.W, H: target.H, Text: target.Text,
	}); err != nil {
		return fmt.Errorf("点击模型选项失败: %w", err)
	}

	// 校验：选择器按钮显示短名（如 "Flash-Lite"/"Flash"/"Pro"/"thinking"）应精确
	// 匹配目标模型显示名的末 token（模型族名）——菜单项全名与按钮短名允许省略
	// 版本前缀，但必须是词边界精确匹配（"Flash" ≠ "Flash-Lite"）。
	time.Sleep(800 * time.Millisecond)
	var labelText string
	_ = sess.EvalValue(ctx, geminiPickerLabelJS, &labelText)

	wantFamily := lastToken(normalizeText(label))
	if wantFamily == "" || !hasWord(normalizeText(labelText), wantFamily) {
		return fmt.Errorf("模型未切换成功（当前选择器仍显示 %q）——游客账号模型被锁定，请登录后重试",
			labelText)
	}
	return nil
}

// switchGeminiThinking 打开模型选择器，把「Extended thinking」复选框切到 wantOn
// （选中态 = 菜单项 class 含 selected）。开关与模型单选正交，可与任一模型组合；
// 切换后菜单可能保持打开，需关闭避免遮挡输入区。
func SwitchGeminiThinking(ctx context.Context, sess Session, disp Dispatch, wantOn bool) error {
	if _, err := OpenGeminiPicker(ctx, sess); err != nil {
		return err
	}
	options, err := WaitGeminiMenu(ctx, sess)
	if err != nil {
		// 菜单状态可能错乱（上次未关/被页面打断）：先关闭再重开一次。
		_ = CloseGeminiMenu(ctx, sess)
		time.Sleep(300 * time.Millisecond)
		if _, oerr := OpenGeminiPicker(ctx, sess); oerr != nil {
			return oerr
		}
		options, err = WaitGeminiMenu(ctx, sess)
		if err != nil {
			return err
		}
	}
	idx := FindThinkingOption(options)
	if idx < 0 {
		return fmt.Errorf("Extended thinking 选项不在菜单中")
	}
	target := options[idx]
	if target.Selected != wantOn {
		if disp != nil {
			disp.Mark() // 选项已定位且与期望状态不同，鼠标事件从这里开始发出（§5.0.11 条款 1）
		}
		if err := sess.ClickAt(ctx, Target{
			Found: true, X: target.X, Y: target.Y, W: target.W, H: target.H, Text: target.Text,
		}); err != nil {
			return fmt.Errorf("点击 Extended thinking 失败: %w", err)
		}
		time.Sleep(800 * time.Millisecond)
	}
	state, err := ReadThinkingState(ctx, sess)
	if err != nil {
		return err
	}
	if state != wantOn {
		return fmt.Errorf("Extended thinking 未切换到 %v（当前 %v）", wantOn, state)
	}
	return CloseGeminiMenu(ctx, sess)
}

// readThinkingState 读取 Extended thinking 开关状态（菜单已关闭时自动重开）。
func ReadThinkingState(ctx context.Context, sess Session) (bool, error) {
	options, err := WaitGeminiMenu(ctx, sess)
	if err != nil {
		// 菜单已关闭（切换后自动收起）：重新打开再读。
		if _, oerr := OpenGeminiPicker(ctx, sess); oerr != nil {
			return false, oerr
		}
		options, err = WaitGeminiMenu(ctx, sess)
		if err != nil {
			return false, err
		}
	}
	idx := FindThinkingOption(options)
	if idx < 0 {
		return false, fmt.Errorf("Extended thinking 选项不在菜单中")
	}
	return options[idx].Selected, nil
}

// closeGeminiMenu 关闭模型选择器菜单（Escape 键；失败则再点一次选择器按钮）。
func CloseGeminiMenu(ctx context.Context, sess Session) error {
	esc := map[string]any{
		"type": "keyDown", "key": "Escape", "code": "Escape",
		"windowsVirtualKeyCode": 27, "nativeVirtualKeyCode": 27,
	}
	if err := sess.Call(ctx, "Input.dispatchKeyEvent", esc, nil); err == nil {
		esc["type"] = "keyUp"
		_ = sess.Call(ctx, "Input.dispatchKeyEvent", esc, nil)
	}
	time.Sleep(300 * time.Millisecond)
	return nil
}

// FindThinkingOption 在菜单项中定位 Extended thinking 复选框（按文本区分于
// Flash/Pro 等模型单选；模型项文本不含 thinking）。
func FindThinkingOption(options []MenuOption) int {
	for i, o := range options {
		text := strings.ToLower(normalizeText(o.Text))
		if strings.Contains(text, "extended thinking") || strings.Contains(text, "thinking") {
			return i
		}
	}
	return -1
}

// geminiInputEmpty 判断 Gemini 输入区是否已清空（发送成功后 Angular 清空编辑器）。
// 判断失败时按「已清空」处理，避免误重试造成重复发送。
func GeminiInputEmpty(ctx context.Context, sess Session) bool {
	var empty bool
	if err := sess.EvalValue(ctx, `(() => {
	  const e = document.querySelector('.ql-editor, [contenteditable="true"]');
	  return e ? !((e.innerText || '').trim()) : true;
	})()`, &empty); err != nil {
		return true
	}
	return empty
}

// PickGeminiOption 在菜单项文本中定位目标模型：先精确匹配首行（"3.6 Flash"），
// 再退化为「目标 token 全部包含」（"Flash" → 匹配 "3.7 Flash"；"最新 Flash" →
// 匹配 "3.6 Flash（最新）" 这类）。
func PickGeminiOption(options []MenuOption, want string) int {
	wantNorm := normalizeText(want)
	if wantNorm == "" {
		return -1
	}
	firstLine := func(s string) string {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return s[:i]
		}
		return s
	}
	for i, o := range options {
		if strings.EqualFold(normalizeText(firstLine(o.Text)), wantNorm) {
			return i
		}
	}
	tokens := strings.Fields(wantNorm)
	for i, o := range options {
		first := normalizeText(firstLine(o.Text))
		full := normalizeText(o.Text)
		if tokenAllContain(first, tokens) || tokenAllContain(full, tokens) {
			if familyConflict(first, tokens) {
				continue // "Flash" 不得命中 "Flash-Lite"
			}
			return i
		}
	}
	return -1
}

// familyConflict 判断候选文本是否与目标 token 存在家族词冲突：
// 目标含 "Flash"（非 Flash-Lite）时，候选含 "Flash-Lite" 视为冲突。
func familyConflict(s string, tokens []string) bool {
	sLower := strings.ToLower(s)
	for _, t := range tokens {
		tl := strings.ToLower(t)
		if strings.Contains(tl, "flash") && !strings.Contains(tl, "flash-lite") &&
			strings.Contains(sLower, "flash-lite") {
			return true
		}
	}
	return false
}

func tokenAllContain(s string, tokens []string) bool {
	s = strings.ToLower(s)
	for _, t := range tokens {
		if !strings.Contains(s, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

// lastToken 返回字符串的最后一个空白分隔词（如 "3.5 Flash-Lite" → "Flash-Lite"）。
func lastToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// hasWord 判断 token 是否以词边界（整词）出现在 s 中（大小写不敏感）。
func hasWord(s, token string) bool {
	for _, w := range strings.Fields(s) {
		if strings.EqualFold(w, token) {
			return true
		}
	}
	return false
}

func normalizeText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
