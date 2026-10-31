package matcher

import "testing"

// 结构通道对 rename（文本漂移）不敏感：结构哈希距离为 0（文本由文本通道负责）。
func TestStructHashRenameInvariant(t *testing.T) {
	ref := StructuralSimHash64(StructFeature{
		Tag: "button", Role: "button",
		Attrs:     []string{"data-testid", "aria-label"},
		Ancestors: []StructAncestor{{Tag: "section", Role: "group"}, {Tag: "main", Role: "region"}},
	})
	renamed := StructuralSimHash64(StructFeature{
		Tag: "button", Role: "button",
		Attrs:     []string{"data-testid", "aria-label"},
		Ancestors: []StructAncestor{{Tag: "section", Role: "group"}, {Tag: "main", Role: "region"}},
	})
	if d := HammingDistance(ref, renamed); d != 0 {
		t.Fatalf("rename 不改变结构：Hamming 应为 0，实际 %d", d)
	}
}

// 幽灵包装（祖先链插 div/generic）不改变阻尼结构流：距离 0。
func TestStructHashWrapInvariant(t *testing.T) {
	base := StructFeature{
		Tag: "button", Role: "button",
		Attrs:     []string{"data-testid"},
		Ancestors: []StructAncestor{{Tag: "div", Role: "list"}, {Tag: "section", Role: "group"}},
	}
	wrapped := StructFeature{
		Tag: "button", Role: "button",
		Attrs: []string{"data-testid"},
		Ancestors: []StructAncestor{
			{Tag: "div", Role: "generic"},
			{Tag: "div", Role: "generic"},
			{Tag: "div", Role: "list"},
			{Tag: "section", Role: "group"},
		},
	}
	if d := HammingDistance(StructuralSimHash64(base), StructuralSimHash64(wrapped)); d != 0 {
		t.Fatalf("幽灵包装不改变阻尼结构流：Hamming 应为 0，实际 %d", d)
	}
}

// 身份漂移（tag+role 全变）与 hard decoy（不同祖先语义容器）必须与参考结构拉开距离。
func TestStructHashDriftSeparates(t *testing.T) {
	ref := StructuralSimHash64(StructFeature{
		Tag: "button", Role: "button",
		Attrs:     []string{"data-testid"},
		Ancestors: []StructAncestor{{Tag: "section", Role: "group"}},
	})
	ghost := StructuralSimHash64(StructFeature{
		Tag: "div", Role: "generic",
		Attrs:     nil,
		Ancestors: []StructAncestor{{Tag: "section", Role: "group"}},
	})
	decoy := StructuralSimHash64(StructFeature{
		Tag: "a", Role: "link",
		Attrs:     nil,
		Ancestors: []StructAncestor{{Tag: "aside", Role: "complementary"}},
	})
	if d := HammingDistance(ref, ghost); d <= SimHashDistanceThreshold {
		t.Fatalf("ghost（tag+role 全漂移）结构距离应 > D_h=%d，实际 %d", SimHashDistanceThreshold, d)
	}
	if d := HammingDistance(ref, decoy); d <= SimHashDistanceThreshold {
		t.Fatalf("hard decoy（不同语义容器）结构距离应 > D_h=%d，实际 %d", SimHashDistanceThreshold, d)
	}
}

// Malhotra compatible：role 保留（tag 漂移）兼容；tag+role 全漂移不兼容。
func TestStructRoleCompatible(t *testing.T) {
	oracle := StructFeature{Tag: "button", Role: "button"}
	if !StructRoleCompatible(StructFeature{Tag: "div", Role: "button"}, oracle) {
		t.Fatal("role 保留、tag 漂移应兼容（Malhotra compatible）")
	}
	if StructRoleCompatible(StructFeature{Tag: "div", Role: "generic"}, oracle) {
		t.Fatal("tag+role 全漂移应不兼容")
	}
}
