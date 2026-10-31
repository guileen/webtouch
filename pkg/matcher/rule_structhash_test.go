// Tier 1.5 规则级 StructHash 全链路（TDD-PROBE-06 延伸）：
// 规则库 schema/查询/ReplaceAll 必须读写 struct_hash 列；Matcher.StructuralReference
// 必须能从规则库取到非 0 结构参考（供引擎 pageCandidatePool 双通道接线）。
package matcher

import "testing"

func TestRuleStructHashRoundTrip(t *testing.T) {
	s, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	want := StructuralSimHash64(StructFeature{Tag: "button", Role: "button"})
	if want == 0 {
		t.Fatal("参考结构哈希不应为 0")
	}
	id, err := s.InsertRule(Rule{
		RuleType: "text_pattern", TextPattern: "登录", Action: "click",
		SemanticLabel: "登录", Confidence: 0.9, Source: "component_gene",
		Status: "active", StructHash: want,
	})
	if err != nil {
		t.Fatalf("InsertRule: %v", err)
	}
	if id == 0 {
		t.Fatal("InsertRule 应返回非 0 id")
	}

	active, err := s.ActiveTextPatterns()
	if err != nil {
		t.Fatalf("ActiveTextPatterns: %v", err)
	}
	if len(active) != 1 || active[0].StructHash != want {
		t.Fatalf("ActiveTextPatterns StructHash = %#x (want %#x), len=%d",
			active[0].StructHash, want, len(active))
	}

	bySem, err := s.RulesBySemantic("登录")
	if err != nil {
		t.Fatalf("RulesBySemantic: %v", err)
	}
	if len(bySem) != 1 || bySem[0].StructHash != want {
		t.Fatalf("RulesBySemantic StructHash = %#x (want %#x)", bySem[0].StructHash, want)
	}
}

func TestReplaceAllPreservesStructHash(t *testing.T) {
	s, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	want := StructuralSimHash64(StructFeature{Tag: "a", Role: "link", Attrs: []string{"aria-label"}})
	if err := s.ReplaceAll([]Rule{{
		RuleType: "text_pattern", TextPattern: "注册", Action: "click",
		SemanticLabel: "注册", Confidence: 0.8, Source: "component_gene",
		Status: "ACTIVE", StructHash: want,
	}}, nil, nil); err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	active, err := s.ActiveTextPatterns()
	if err != nil {
		t.Fatalf("ActiveTextPatterns: %v", err)
	}
	if len(active) != 1 || active[0].StructHash != want || active[0].Status != "active" {
		t.Fatalf("ReplaceAll 后 StructHash/status 错: %+v", active[0])
	}
}

func TestMatcherStructuralReference(t *testing.T) {
	m, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	want := StructuralSimHash64(StructFeature{Tag: "button", Role: "button"})
	if _, err := m.InsertRule(Rule{
		RuleType: "text_pattern", TextPattern: "提交", Action: "click",
		SemanticLabel: "提交订单", Role: "button", Confidence: 0.95,
		Source: "component_gene", Status: "active", StructHash: want,
	}); err != nil {
		t.Fatalf("InsertRule: %v", err)
	}
	hash, role, ok := m.StructuralReference("提交订单")
	if !ok || hash != want || role != "button" {
		t.Fatalf("StructuralReference = (%#x, %q, %v), want (%#x, %q, true)",
			hash, role, ok, want, "button")
	}

	// 未携带结构参考的规则 → 返回 ok=false（引擎保持文本单通道）。
	if _, _, ok := m.StructuralReference("不存在语义"); ok {
		t.Fatal("无结构参考语义应返回 ok=false")
	}
}
