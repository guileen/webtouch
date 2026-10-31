// §5.0.9 条款 1 守卫的纯判定表（从宿主 `input_receptor_guard_test.go` 拆出，
// 复杂度治理·阶段 2·单元 2a：纯判定随包、真机 `httptest` 段留宿主）。
package input

import "testing"

// §5.0.9 条款 1 守卫的纯判定表：
// 受体角色画像 × 意图语义/值 → 是否熔断。定向钉死三类历史误判风险：
// ① 注册邮箱框被 VLM 当搜索框写入（github 假成功）；② `user-search` 一类合法搜索
// 受体不得因 `user` 关键词误伤；③ `microphone`/`author` 一类子串不得被 `phone`/`auth` 误伤。
func TestGuardReceptorProfile(t *testing.T) {
	cases := []struct {
		name        string
		profile     ReceptorProfile
		semantic    string
		value       string
		wantBlocked bool
	}{
		{
			name:        "github 形状：邮箱受体 + 搜索意图 ⇒ 熔断",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "email", ID: "hero_user_email", Name: "user[email]"},
			semantic:    "search_box",
			value:       "webtouch-probe@example.com",
			wantBlocked: true,
		},
		{
			name:        "同受体 + 凭据语义 ⇒ 放行",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "email", ID: "hero_user_email"},
			semantic:    "登录邮箱",
			value:       "webtouch-probe@example.com",
			wantBlocked: false,
		},
		{
			name:        "同受体 + vault 通道值 ⇒ 放行",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "email", ID: "hero_user_email"},
			semantic:    "search_box",
			value:       "@vault:github-email",
			wantBlocked: false,
		},
		{
			name:        "搜索受体优先：user-search 不因 user 关键词熔断",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "text", ID: "user-search", Name: "q"},
			semantic:    "search_box",
			value:       "webtouch-probe",
			wantBlocked: false,
		},
		{
			name:        "中文搜索受体不误伤",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "text", Placeholder: "搜索用户"},
			semantic:    "search_box",
			value:       "webtouch-probe",
			wantBlocked: false,
		},
		{
			name:        "microphone 不得被 phone 子串误伤",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "text", ID: "microphone"},
			semantic:    "search_box",
			value:       "webtouch-probe",
			wantBlocked: false,
		},
		{
			name:        "author 不得被 auth 子串误伤",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "text", ID: "author", Name: "author"},
			semantic:    "search_box",
			value:       "webtouch-probe",
			wantBlocked: false,
		},
		{
			name:        "password 类型受体 + 空语义 ⇒ 熔断",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "password"},
			semantic:    "",
			value:       "webtouch-probe",
			wantBlocked: true,
		},
		{
			name:        "OTP 受体 + 搜索意图 ⇒ 熔断（规范代号清单对齐）",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "text", ID: "otp-code", Autocomplete: "one-time-code"},
			semantic:    "search_box",
			value:       "123456",
			wantBlocked: true,
		},
		{
			name:        "手机号占位受体 + 搜索意图 ⇒ 熔断",
			profile:     ReceptorProfile{Found: true, Tag: "input", Type: "tel", Placeholder: "请输入手机号"},
			semantic:    "search_box",
			value:       "13900000000",
			wantBlocked: true,
		},
		{
			name:        "未标记目标（found=false）不熔断",
			profile:     ReceptorProfile{Found: false, Type: "email"},
			semantic:    "search_box",
			value:       "webtouch-probe",
			wantBlocked: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocked, detail := GuardReceptorProfile(c.profile, c.semantic, c.value)
			if blocked != c.wantBlocked {
				t.Fatalf("GuardReceptorProfile(%+v, %q, %q) blocked=%v detail=%q，want blocked=%v",
					c.profile, c.semantic, c.value, blocked, detail, c.wantBlocked)
			}
			if c.wantBlocked && detail == "" {
				t.Fatalf("熔断必须带受体画像证据（供回执与日志）")
			}
			if !c.wantBlocked && detail != "" {
				t.Fatalf("放行不应产生证据串，got %q", detail)
			}
		})
	}
}
