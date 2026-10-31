package input

import "testing"

// §5.0.3 条款 4 的纯判据：回读值包含输入值即算落地；元素原生文案非空不算数。
func TestInputValueLanded(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
		ok   bool
	}{
		{"完全相同", "webtouch", "webtouch", true},
		{"大小写不敏感", "WebTouch", "webtouch", true},
		{"页面追加了其它文案", "webtouch 搜索", "webtouch", true},
		{"空白折叠", "web\ttouch", "web touch", true},
		{"不含输入值（元素原生文案非空）", "Sign in", "webtouch", false},
		{"空回读（值没进去）", "", "webtouch", false},
		{"空白回读", "   ", "webtouch", false},
		{"空输入值不构成落地", "whatever", "", false},
		{"格式化插入分隔符（退化档）", "130 0000 0000", "13000000000", true},
		{"格式化插入短横线（退化档）", "138-0000-0000", "13800000000", true},
		{"前缀被改写（真未落地）", "Search Wikipedia", "webtouch", false},
		{"叠写仍算落地（叠写由条款 5 幂等写入负责）", "webtouchwebtouch", "webtouch", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InputValueLanded(tc.got, tc.want); got != tc.ok {
				t.Fatalf("InputValueLanded(%q, %q) = %v，期望 %v", tc.got, tc.want, got, tc.ok)
			}
		})
	}
}

// 归一化辅助函数的边界：空白折叠 + 小写；alnumOnly 只留字母数字（Unicode 中文算字母）。
func TestInputGateNormalizers(t *testing.T) {
	if got := normalizeInputText("  Web\tTouch \n"); got != "web touch" {
		t.Fatalf("normalizeInputText = %q", got)
	}
	if got := alnumOnly("138-0000 0000"); got != "13800000000" {
		t.Fatalf("alnumOnly = %q", got)
	}
	if got := alnumOnly("搜索 webtouch"); got != "搜索webtouch" {
		t.Fatalf("alnumOnly 应保留中文（Unicode 字母），got %q", got)
	}
}
