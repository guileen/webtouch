package input

import (
	"strings"
	"testing"
	"time"
)

// §5.0.10 条款 2/3 的纯判定：同余三要素（Role 恒等 ∧ 结构指纹相等 ∧ 位移 ≤ 32px）
// + 租约时效（≤1500ms）+ 预算（Budget=1）。先红后绿：去掉任一要素都必须熔断。
func TestReanchorVerdict(t *testing.T) {
	base := StructuralIdentity{
		Role: "textbox", Tag: "input", ID: "kw", Name: "q", Type: "text",
		Path: "form > input#kw", WeakName: "搜索", X: 100, Y: 200, W: 300, H: 40,
	}
	now := time.Now()
	lease := func() *Lease {
		return &Lease{identity: base, at: now, budget: 1}
	}
	cases := []struct {
		name   string
		lease  *Lease
		cand   StructuralIdentity
		at     time.Time
		ok     bool
		reason string
	}{
		{"同余（位移 10px）", lease(), func() StructuralIdentity {
			c := base
			c.X, c.Y = 110, 206
			return c
		}(), now, true, "ok"},
		{"位移超限（40px）", lease(), func() StructuralIdentity {
			c := base
			c.X = 140
			return c
		}(), now, false, "displacement_exceeded"},
		{"位移刚好 32px 放行", lease(), func() StructuralIdentity {
			c := base
			c.X = 132
			return c
		}(), now, true, "ok"},
		{"水合改了 placeholder（WeakName 变）仍放行", lease(), func() StructuralIdentity {
			c := base
			c.WeakName = "搜点什么"
			c.Y = 205
			return c
		}(), now, true, "ok"},
		{"结构指纹破坏（id 变）", lease(), func() StructuralIdentity {
			c := base
			c.ID = "kw2"
			return c
		}(), now, false, "identity_changed"},
		{"结构指纹破坏（DOM 路径变）", lease(), func() StructuralIdentity {
			c := base
			c.Path = "form:nth-of-type(1) > input:nth-of-type(2)"
			return c
		}(), now, false, "identity_changed"},
		{"Role 掉成 generic", lease(), func() StructuralIdentity {
			c := base
			c.Role = "generic"
			return c
		}(), now, false, "role_changed"},
		{"租约过期（>1500ms）", lease(), base, now.Add(1501 * time.Millisecond), false, "lease_expired"},
		{"租约边界（=1500ms 仍有效）", lease(), base, now.Add(1500 * time.Millisecond), true, "ok"},
		{"预算耗尽（Budget=0）", &Lease{identity: base, at: now, budget: 0}, base, now, false, "budget_exhausted"},
		{"无租约（标记载体已被水合抹掉）", nil, base, now, false, "no_lease"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := ReanchorVerdict(tc.lease, tc.cand, tc.at)
			if ok != tc.ok || reason != tc.reason {
				t.Fatalf("ReanchorVerdict = (%v, %q)，期望 (%v, %q)", ok, reason, tc.ok, tc.reason)
			}
		})
	}
}

// 指纹必须忽略 Name 文案（§5.0.10 条款 2：Name 只作弱加权，不得作为恒等判据）。
func TestStructuralIdentityFingerprintIgnoresWeakName(t *testing.T) {
	a := StructuralIdentity{Role: "textbox", Tag: "input", ID: "kw", Name: "q", Type: "text", Path: "input#kw", WeakName: "搜索"}
	b := a
	b.WeakName = "搜索框（已水合重建）"
	b.Role = "generic" // Role 不进指纹（单独判定），此处只验证指纹
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("WeakName/Role 不应影响结构身份指纹：%q vs %q", a.Fingerprint(), b.Fingerprint())
	}
	c := a
	c.Name = "query"
	if a.Fingerprint() == c.Fingerprint() {
		t.Fatalf("name 属于结构指纹，改了必须不等")
	}
}

// Role 缺失侧退化为指纹判定（不引入新敞口：指纹相等仍是必要条件）。
func TestRoleCongruent(t *testing.T) {
	if !RoleCongruent("textbox", "TEXTBOX") {
		t.Fatalf("大小写不敏感应相等")
	}
	if !RoleCongruent("", "textbox") || !RoleCongruent("generic", "") {
		t.Fatalf("一侧缺失（页面无角色证据）应退化为指纹判定")
	}
	if RoleCongruent("generic", "textbox") {
		t.Fatalf("两侧都有且不等必须判不等")
	}
}

// 契约测试：结构身份抽取不得依赖 Element 上的 composedPath()（§5.0.10 条款 1）。
func TestStructuralIdentityJSContract(t *testing.T) {
	if strings.Contains(StructuralIdentityFnJS, "composedPath") {
		t.Fatalf("composedPath() 是 Event 的方法，Element 上不存在，禁止使用")
	}
	for _, want := range []string{"getBoundingClientRect", "uwsIdentityPath", "uwsDeriveRole"} {
		if !strings.Contains(StructuralIdentityFnJS, want) {
			t.Fatalf("结构身份抽取缺少 %q", want)
		}
	}
	if !strings.Contains(ReanchorCandidateJS, "__UWS_WANT__") {
		t.Fatalf("重锚候选探针必须保留 WANT 占位符（注入时替换）")
	}
	if !strings.Contains(ReanchorCandidateJS, "data-uws-target") {
		t.Fatalf("重锚候选必须重新标记载体，否则后续门禁与回读全部失效")
	}
}
