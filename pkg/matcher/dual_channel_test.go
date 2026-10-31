// TDD-PROBE-07（Tier 1.5 接线）：双通道 L3 与 A* 拓扑剪枝门。
//
// [State:D]：必须将 ast_semantic_hash 作为第一阶拓扑剪枝门——当且仅当拓扑哈希的
// Hamming 距离 D_h ≤ 软阈值时节点方可进入 ONNX 张量重排区。双通道 L3 把文本正交
// （rename）但结构一致的候选保留下来，同时对身份漂移（ghost）执行 Malhotra 兼容门禁。
package matcher

import "testing"

func structRule(hash uint64, role string, conf float64) Candidate {
	return Candidate{
		StructHash: hash,
		Tag:        "button",
		Role:       role,
		Confidence: conf,
		Matches: []Match{{Rule: Rule{
			TextPattern:   "登录",
			Role:          role,
			Confidence:    conf,
			StructHash:    hash,
			SemanticLabel: "登录",
		}}},
	}
}

func TestRefineByStructureDualRenameRescue(t *testing.T) {
	// 参考结构：按钮 + role=button。rename 候选文本正交但结构一致 → 双通道保留。
	refStruct := StructuralSimHash64(StructFeature{Tag: "button", Role: "button"})
	refText := SimHash64("登录")
	renameCand := Candidate{
		NodeText:   "立即登录",
		StructHash: StructuralSimHash64(StructFeature{Tag: "button", Role: "button"}),
		Tag:        "button",
		Role:       "button",
		Confidence: 0.9,
	}
	kept, rejected := RefineByStructureDual([]Candidate{renameCand}, refText, refStruct, "button", "button", 0)
	if len(kept) != 1 {
		t.Fatalf("双通道应保留 rename 候选（结构证据），kept=%d rejected=%d", len(kept), len(rejected))
	}
}

func TestRefineByStructureDualGhostRejected(t *testing.T) {
	// ghost：tag+role 全漂移 → Malhotra 不兼容，结构通道拒绝；除非文本通道命中。
	refStruct := StructuralSimHash64(StructFeature{Tag: "button", Role: "button"})
	refText := SimHash64("登录")
	ghostCand := Candidate{
		NodeText:   "登录", // 文本恰好命中 → 文本通道保留
		StructHash: StructuralSimHash64(StructFeature{Tag: "div", Role: "generic"}),
		Tag:        "div",
		Role:       "generic",
		Confidence: 0.9,
	}
	kept, _ := RefineByStructureDual([]Candidate{ghostCand}, refText, refStruct, "button", "button", 0)
	if len(kept) != 1 {
		t.Fatalf("ghost 文本命中应由文本通道保留，kept=%d", len(kept))
	}
	// 文本也漂移 + 身份漂移 → 双通道全拒。
	ghost2 := ghostCand
	ghost2.NodeText = "关闭"
	kept, rejected := RefineByStructureDual([]Candidate{ghost2}, refText, refStruct, "button", "button", 0)
	if len(kept) != 0 || len(rejected) != 1 {
		t.Fatalf("ghost（文本+身份双漂移）应被双通道剔除，kept=%d rejected=%d", len(kept), len(rejected))
	}
}

func TestRefineByStructureAutoDualFromRule(t *testing.T) {
	// 规则携带 StructHash 时，RefineByStructure 自动走双通道（无需调用方传参考哈希）。
	hash := StructuralSimHash64(StructFeature{Tag: "button", Role: "button"})
	cand := structRule(hash, "button", 0.9)
	cand.NodeText = "立即登录" // 文本与目标「登录」正交
	kept, _ := RefineByStructure([]Candidate{cand}, SimHash64("登录"), 0)
	if len(kept) != 1 {
		t.Fatalf("规则携带 StructHash 时应自动双通道保留 rename 候选，kept=%d", len(kept))
	}
}

func TestRefineByStructureTextOnlyFallback(t *testing.T) {
	// 规则不带 StructHash → 退化为文本单通道，rename 候选被剔除（既有语义）。
	cand := Candidate{NodeText: "立即登录", Confidence: 0.9,
		Matches: []Match{{Rule: Rule{TextPattern: "登录", Confidence: 0.9, SemanticLabel: "登录"}}}}
	kept, _ := RefineByStructure([]Candidate{cand}, SimHash64("登录"), 0)
	if len(kept) != 0 {
		t.Fatalf("无结构参考时应走文本单通道，rename 候选应被剔除，kept=%d", len(kept))
	}
}

func TestTopologicalPruneGate(t *testing.T) {
	refStruct := StructuralSimHash64(StructFeature{Tag: "button", Role: "button"})
	close := Candidate{StructHash: StructuralSimHash64(StructFeature{Tag: "button", Role: "button"})}
	far := Candidate{StructHash: StructuralSimHash64(StructFeature{Tag: "div", Role: "generic", Attrs: []string{"aria-hidden"}})}
	noStruct := Candidate{StructHash: 0, NodeText: "登录"}

	// 软阈值放行结构近邻；剔除结构远隔与无拓扑签名（StructHash==0）候选。
	out := TopologicalPruneGate([]Candidate{close, far, noStruct}, refStruct, TopologicalPruneSoftThreshold)
	if len(out) != 1 {
		t.Fatalf("拓扑剪枝门应仅保留结构近邻(1)，剔除远隔与无签名，实际 kept=%d", len(out))
	}
	if out[0].StructHash != refStruct {
		t.Fatalf("拓扑剪枝门保留的应是结构近邻候选")
	}
	// 无结构参考 → 门禁 no-op（全量放行）。
	out = TopologicalPruneGate([]Candidate{close, far}, 0, TopologicalPruneSoftThreshold)
	if len(out) != 2 {
		t.Fatalf("无参考结构时门禁应 no-op 全量放行，实际 %d", len(out))
	}
}

func TestTopologicalPruneGateStrictThreshold(t *testing.T) {
	refStruct := StructuralSimHash64(StructFeature{Tag: "button", Role: "button"})
	close := Candidate{StructHash: refStruct}
	out := TopologicalPruneGate([]Candidate{close}, refStruct, 0) // 0 → 默认软阈值
	if len(out) != 1 {
		t.Fatalf("默认软阈值应放行同结构候选，实际 %d", len(out))
	}
}
