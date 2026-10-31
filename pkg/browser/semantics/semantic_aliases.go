package semantics

import "strings"

// 内部语义标签 → 页面候选文案集合（spec/algorithm-kernel.md §5.0.2）。
//
// 背景：`target_semantic` 是内部控制面标识（如协调器 DSL 宏展开的 `search_box`），
// 页面上不存在这个字符串；物理层定位若直接拿它做字面比对，等于「用内部编号搜
// 页面文本」，12 站实采命中率 0/12（docs/evidence/pilots/site-coverage-2026-09-24.md）。
//
// 纪律（§5.0.2）：
//   - 只放**跨站点通用**文案，不放站点专有文案（站点知识归 §4.2 规则 / §4.3 同义词）；
//   - 有序，index 0 为最通用的页面文案（评分是取最高分，顺序只作可读性与回显口径）；
//   - 未覆盖的标签由 semanticWants 原样单候选返回：fail-closed，不做启发式猜测。
var (
	aliasSearchBox  = []string{"搜索", "搜尋", "search", "站内搜索", "关键字", "关键词"}
	aliasSearchBtn  = []string{"搜索", "搜尋", "search", "查询"}
	aliasLoginBtn   = []string{"登录", "登入", "登陆", "sign in", "log in", "login", "signin"}
	aliasUsername   = []string{"用户名", "账号", "帐号", "邮箱", "手机号", "username", "email", "e-mail"}
	aliasPassword   = []string{"密码", "密碼", "password", "passwd"}
	aliasSubmitBtn  = []string{"提交", "submit", "确认", "确定"}
	semanticAliases = map[string][]string{
		"search_box":     aliasSearchBox,
		"search_button":  aliasSearchBtn,
		"login_button":   aliasLoginBtn,
		"login_username": aliasUsername,
		"username_input": aliasUsername,
		"login_password": aliasPassword,
		"password_input": aliasPassword,
		"submit_btn":     aliasSubmitBtn,
	}
)

// Wants 把内部语义标签归一化为候选文案集合：命中别名表时返回
// 「通用候选… + 原始标签（末位，保证历史行为零回归）」；未覆盖时返回
// 单元素集合 [语义原串]（行为与归一化前完全一致）。
func Wants(semantic string) []string {
	s := strings.TrimSpace(semantic)
	aliases := semanticAliases[strings.ToLower(s)]
	wants := make([]string, 0, len(aliases)+1)
	for _, a := range aliases {
		if a != "" {
			wants = append(wants, a)
		}
	}
	if s == "" {
		return wants
	}
	// 原始标签保留为末位候选：别名表覆盖后行为只增不减（零回归）。
	for _, w := range wants {
		if strings.EqualFold(w, s) {
			return wants
		}
	}
	return append(wants, s)
}
