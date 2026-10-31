package input

import (
	"context"
	"regexp"
	"strings"
)

// 输入受体角色守卫（spec/algorithm-kernel.md §5.0.9 条款 1）。
//
// 背景（机制事实）：github 站实测——§5.0.3 后验门禁在正门 3 次拒绝装饰性 `span "Sign in"`
// （代号正确），但控制流降级到 VLM 后，VLM 在无角色契约约束下把输入派发到**注册邮箱框**
// `input#hero_user_email`（语义目标是 `search_box`），报告层无 `hit_element`、AXTree 因 CSP
// 不可用，却仍返回 `SUCCEEDED`。即「假成功」被挤到 VLM 这一新出口，性质是越权写入。
//
// 本守卫放在**最低层输入原语** `inputAt`（单点实现），因此确定性语义/选择器路径、
// `node:` 路径与 VLM 坐标路径共用同一判定：派发前读取受体角色画像，与意图语义比对，
// 命中敏感角色而意图不含凭据词时熔断 `TARGET_ROLE_MISMATCH`——不派发、不写 learned。
const FaultTargetRoleMismatch = "TARGET_ROLE_MISMATCH"

// FaultVLMlandingUnverified 是 VLM 兜底输入缺少落地回读证据时的结构化代号（§5.0.9 条款 2）。
const FaultVLMlandingUnverified = "vlm_landing_unverified"

// inputReceptorProbeJS 读取已标记目标（`[data-uws-target="1"]`）的角色画像。
const inputReceptorProbeJS = `(() => {
  const el = document.querySelector('[data-uws-target="1"]');
  if (!el) return {found: false};
  const attr = n => (el.getAttribute && el.getAttribute(n)) || '';
  return {
    found: true,
    tag: (el.tagName || '').toLowerCase(),
    type: (attr('type') || '').toLowerCase(),
    name: attr('name'),
    id: el.id || '',
    placeholder: attr('placeholder'),
    aria: attr('aria-label'),
    autocomplete: (attr('autocomplete') || '').toLowerCase(),
  };
})()`

// ReceptorProfile 是受体的角色画像（派发前读取，供 §5.0.9 守卫比对）。
type ReceptorProfile struct {
	Found        bool   `json:"found"`
	Tag          string `json:"tag"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	ID           string `json:"id"`
	Placeholder  string `json:"placeholder"`
	Aria         string `json:"aria"`
	Autocomplete string `json:"autocomplete"`
}

// receptorSensitiveRe 匹配敏感角色的 ASCII 词（要求词边界，避免 microphone→phone、
// author→auth 一类子串误伤）。
var receptorSensitiveRe = regexp.MustCompile(
	`(^|[^a-z0-9])(password|passwd|pwd|otp|email|e-mail|tel|phone|mobile|username|user|login|signin|sign-in|signup|sign-up|register|auth)([^a-z0-9]|$)`)

// receptorSearchRe 匹配「受体自身就是搜索框」的画像（优先认定搜索受体，不熔断——
// 避免 `input#user-search`（用户搜索）一类合法搜索目标被 `user` 关键词误伤）。
var receptorSearchRe = regexp.MustCompile(`(^|[^a-z0-9])(search|query|find)([^a-z0-9]|$)`)

// receptorSensitiveCJK 是敏感角色的中文关键词（子串匹配）。
var receptorSensitiveCJK = []string{"密码", "邮箱", "手机", "账号", "帐号", "用户名", "验证码", "登录", "注册"}

// receptorSearchCJK 是搜索受体的中文关键词。
var receptorSearchCJK = []string{"搜索", "查询", "检索"}

// profileText 汇总用于关键词比对的画像文本（小写）。
func (p ReceptorProfile) ProfileText() string {
	return strings.ToLower(strings.Join([]string{
		p.Tag, p.Type, p.Name, p.ID, p.Placeholder, p.Aria, p.Autocomplete,
	}, " "))
}

// describe 给出简短画像（日志与失败证据用）。
func (p ReceptorProfile) Describe() string {
	parts := make([]string, 0, 4)
	if p.Tag != "" {
		s := p.Tag
		if p.ID != "" {
			s += "#" + p.ID
		}
		parts = append(parts, s)
	}
	if p.Type != "" && p.Type != "text" {
		parts = append(parts, "type="+p.Type)
	}
	if p.Name != "" {
		parts = append(parts, "name="+p.Name)
	}
	return strings.Join(parts, " ")
}

// isSensitiveReceptor 判定受体是否命中敏感角色（凭据类字段）。
func (p ReceptorProfile) IsSensitiveReceptor() bool {
	text := p.ProfileText()
	switch p.Type {
	case "password", "email", "tel":
		return true
	}
	if receptorSensitiveRe.MatchString(text) {
		return true
	}
	for _, kw := range receptorSensitiveCJK {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// isSearchReceptor 判定受体自身是否就是搜索框（搜索受体优先，不作凭据判定）。
func (p ReceptorProfile) IsSearchReceptor() bool {
	text := p.ProfileText()
	if receptorSearchRe.MatchString(text) {
		return true
	}
	for _, kw := range receptorSearchCJK {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// CredentialIntent 判定「意图本身就在处理凭据」：语义含凭据词，或值走 vault 通道
// （`@vault:<名>` 是本项目唯一受批准的明文凭据搬运路径）。命中即不熔断。
func CredentialIntent(semantic, value string) bool {
	if strings.Contains(value, "@vault:") {
		return true
	}
	text := strings.ToLower(semantic)
	if receptorSensitiveRe.MatchString(text) {
		return true
	}
	for _, kw := range receptorSensitiveCJK {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// GuardReceptorProfile 是守卫的纯判定：返回 (是否熔断, 证据)。
func GuardReceptorProfile(p ReceptorProfile, semantic, value string) (bool, string) {
	if !p.Found || !p.IsSensitiveReceptor() || p.IsSearchReceptor() {
		return false, ""
	}
	if CredentialIntent(semantic, value) {
		return false, ""
	}
	return true, p.Describe()
}

// GuardInputReceptor 在派发前执行守卫（§5.0.9 条款 1）。探针本身失败时 fail-closed：
// 无法确认受体角色就不派发（下游 editable 门禁另有一道，两道都不放行才是安全态）。
func GuardInputReceptor(ctx context.Context, ev Eval, semantic, value string) (bool, string, error) {
	var p ReceptorProfile
	if err := ev.EvalValue(ctx, inputReceptorProbeJS, &p); err != nil {
		return false, "", err
	}
	blocked, detail := GuardReceptorProfile(p, semantic, value)
	return blocked, detail, nil
}
