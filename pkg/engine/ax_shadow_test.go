package engine

import "testing"

// TestAXTreeStats 守住影子观测的统计口径：行数只算有效行（跳过标题/空行），
// 命中不区分大小写（英文 ASCII 折叠），ref_hit 只在可交互 ref 行命中时为真。
func TestAXTreeStats(t *testing.T) {
	tree := `# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）
- link "登录 AutoDL" [href=https://www.autodl.com/login]
- heading "账号安全"

# 可交互节点（[ref] 行；click/input 用 node:<ref> 确定性引用）
[12] button(button) "修改密码"
[13] button(button) "退出登录"
`
	st := axTreeStats(tree, "修改密码")
	if st.lines != 4 {
		t.Fatalf("lines=%d, want 4（1 link + 1 heading + 2 ref 行；# 标题行与空行不计）", st.lines)
	}
	if st.refLines != 2 {
		t.Fatalf("refLines=%d, want 2（可交互 ref 行数独立于命中）", st.refLines)
	}
	if st.hitLines != 1 || !st.refHit {
		t.Fatalf("hitLines=%d refHit=%t, want 1/true（命中可交互 ref 行）", st.hitLines, st.refHit)
	}

	st = axTreeStats(tree, "autodl")
	if st.hitLines != 1 || st.refHit {
		t.Fatalf("hitLines=%d refHit=%t, want 1/false（大小写不敏感命中内容行）", st.hitLines, st.refHit)
	}

	st = axTreeStats(tree, "根本不存在的目标")
	if st.lines != 4 || st.hitLines != 0 || st.refLines != 2 {
		t.Fatalf("lines=%d hitLines=%d refLines=%d, want 4/0/2", st.lines, st.hitLines, st.refLines)
	}
}
