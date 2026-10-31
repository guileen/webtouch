package input

import (
	"strings"
	"unicode"
)

// 输入后验门禁（spec/algorithm-kernel.md §5.0.3）。
//
// 背景：`input` 的落地自检曾只判「目标元素自身文本非空」，命中 `span`/`button`/`div`
// 这类非可编辑元素时其原生文案本身就非空 ⇒ 自检必然通过、回执报 `SUCCEEDED`，而输入值
// 只是打进了当时恰好持有焦点的别处或直接丢弃（12 站实采 github `span` «Sign in»）。
// 本文件提供三块判据：
//   - IsEditableFnJS：可编辑元素判定（注入定位层 JS 与派发前二次校验，单一定义防漂移）；
//   - InputTargetSnapshotJS：从**同一元素**回读可编辑内容；
//   - InputValueLanded / SelectInputContentJS：落地判据与重试幂等写入。

// IsEditableFnJS 是「可编辑元素」判定函数（§5.0.3 条款 1）：文本类 input（排除
// hidden/button/submit/reset/image/checkbox/radio/file/range/color 与非 disabled/readOnly）、
// textarea、select、contenteditable、role=textbox。
// 注入时必须放在 IIFE 内部（顶层 const 在同一 execution context 重复声明会抛 SyntaxError）。
const IsEditableFnJS = `const isEditableEl = el => {
  if (!el) return false;
  const tag = el.tagName.toLowerCase();
  if (tag === 'textarea') return !el.disabled && !el.readOnly;
  if (tag === 'select') return !el.disabled;
  if (tag === 'input') {
    const type = (el.getAttribute('type') || 'text').toLowerCase();
    const nonText = ['hidden','button','submit','reset','image','checkbox','radio','file','range','color'];
    return nonText.indexOf(type) < 0 && !el.disabled && !el.readOnly;
  }
  if (el.isContentEditable) return true;
  return el.getAttribute('role') === 'textbox';
};`

// InputTargetEditableJS 是派发前二次校验（§5.0.3 归属：派发层）：
// 已标记元素（data-uws-target=1）是否可编辑；false 时物理层必须明确失败而非静默成功。
const InputTargetEditableJS = `(() => {
  ` + IsEditableFnJS + `
  return isEditableEl(document.querySelector('[data-uws-target="1"]'));
})()`

// InputTargetSnapshotJS 回读已标记元素的可编辑内容（§5.0.3 条款 4：只认同一元素）。
// input/textarea/select 用 value，contenteditable 用 innerText/textContent。
const InputTargetSnapshotJS = `(() => {
  ` + IsEditableFnJS + `
  const el = document.querySelector('[data-uws-target="1"]');
  if (!el) return {found: false};
  return {
    found: true,
    tag: el.tagName.toLowerCase(),
    value: (typeof el.value === 'string') ? el.value : '',
    text: (el.innerText || el.textContent || ''),
    editable: isEditableEl(el),
  };
})()`

// SelectInputContentJS 让目标元素的已有内容落入选区（§5.0.3 条款 5：重试前调用，
// 使 insertText 覆盖原值而不是追加，杜绝 ddg 实测的 `webtouchwebtouch` 叠写）。
const SelectInputContentJS = `(() => {
  const el = document.querySelector('[data-uws-target="1"]');
  if (!el) return false;
  if (el.isContentEditable) {
    const range = document.createRange();
    range.selectNodeContents(el);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    return true;
  }
  if (typeof el.select === 'function') { el.select(); return true; }
  return false;
})()`

// InputValueLanded 判定「输入值是否真的落进目标元素」（§5.0.3 条款 4）：回读值**包含**
// 输入值即算落地；空白折叠 + 大小写不敏感；再退化到「忽略非字母数字字符」比较，兼容
// `13000000000 → 130 0000 0000` 一类页面侧格式化。叠写（`webtouchwebtouch`）仍会通过
// 本判据——叠写由条款 5 的幂等写入负责，不由本判据负责。
func InputValueLanded(got, want string) bool {
	w := normalizeInputText(want)
	if w == "" {
		return false
	}
	if strings.Contains(normalizeInputText(got), w) {
		return true
	}
	gw := alnumOnly(w)
	if gw == "" {
		return false
	}
	return strings.Contains(alnumOnly(got), gw)
}

// normalizeInputText 折叠空白并转小写（页面文案的空格/大小写不可控）。
func normalizeInputText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// alnumOnly 只保留字母与数字（Unicode），用于兼容页面插入的分隔符与格式化。
func alnumOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
