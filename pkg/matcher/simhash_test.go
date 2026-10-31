// TDD-MATCH-05（P1/M4/单元）：L3 SimHash 物理校验逻辑层。
// v1.4 冻结契约无 node_features 输入 RPC（USER_ACTION.md #6），
// 本层以「候选节点文本 → 64-bit SimHash」作为结构签名的降级代理，
// 与 pipeline/cc_fetcher/topology.py 的 simhash64 对齐（跨语言同构）；
// 真实 DOM/AXTree 特征通道落地后，将 NodeText 替换为结构特征即可，无需改调用方。
package matcher

import "testing"

// TDD-MATCH-05（单元/确定性）：同一文本 SimHash 一致；不同语义文本哈希不同。
func TestMatch05SimHashDeterministic(t *testing.T) {
	h1 := SimHash64("登录 用户名 密码 提交")
	h2 := SimHash64("登录 用户名 密码 提交")
	if h1 != h2 {
		t.Fatalf("同一文本的 SimHash 应一致：%016x != %016x", h1, h2)
	}
	h3 := SimHash64("advertisement banner cookie popup tracking")
	if h1 == h3 {
		t.Fatalf("不同语义文本的 SimHash 不应相同：%016x", h1)
	}
}

// TDD-MATCH-05（单元/黄金夹具）：结构同构（同 Token 词袋，仅词序不同）的文本
// 汉明距离必须 ≤ D_h=5（SimHash 词序不敏感，重排距离为 0），不得被 L3 剔除。
func TestMatch05SimilarTextsWithinThreshold(t *testing.T) {
	ref := SimHash64("登录 用户名 密码 提交")
	near := SimHash64("提交 密码 用户名 登录")
	if d := HammingDistance(ref, near); d > SimHashDistanceThreshold {
		t.Fatalf("相似文本汉明距离应 ≤ %d，实际 %d（ref=%016x near=%016x）",
			SimHashDistanceThreshold, d, ref, near)
	}
}

// TDD-MATCH-05（单元）：汉明距离与重叠度数学正确（含 A* 惩罚公式输入）。
func TestMatch05HammingAndOverlap(t *testing.T) {
	a := uint64(0xFF00FF00FF00FF00)
	if d := HammingDistance(a, a); d != 0 {
		t.Fatalf("相同哈希汉明距离应为 0，实际 %d", d)
	}
	b := uint64(0x00FF00FF00FF00FF)
	if d := HammingDistance(a, b); d != 64 {
		t.Fatalf("互补哈希汉明距离应为 64，实际 %d", d)
	}
	if o := SimHashOverlap(a, a); o != 1.0 {
		t.Fatalf("相同哈希重叠度应为 1.0，实际 %v", o)
	}
	if o := SimHashOverlap(a, b); o != 0.0 {
		t.Fatalf("互补哈希重叠度应为 0.0，实际 %v", o)
	}
}

// TDD-MATCH-05（单元/黄金夹具）：L3 仅对 Top-K 做结构比对，
// 汉明距离 > D_h=5 的候选被剔除；Top-K 之外的低置信候选不参与校验。
func TestMatch05RefineByStructureTopK(t *testing.T) {
	ref := SimHash64("登录 用户名 密码 提交")
	cands := []Candidate{
		{NodeText: "登录 用户名 密码 提交", Confidence: 0.90, Stage: "L1"},
		{NodeText: "提交 密码 用户名 登录", Confidence: 0.80, Stage: "L1"},
		{NodeText: "random unrelated tokens totally different page", Confidence: 0.70, Stage: "L2"},
		{NodeText: "登录 用户名 密码 提交", Confidence: 0.60, Stage: "L2"},
	}

	kept, rejected := RefineByStructure(cands, ref, 3)

	// Top-3 = 前 3 个候选（按置信度降序）；第 3 个语义完全不同 → 剔除。
	if len(kept) != 2 {
		t.Fatalf("应保留 2 个结构同构候选，实际 %d", len(kept))
	}
	if len(rejected) != 1 || rejected[0].NodeText != cands[2].NodeText {
		t.Fatalf("应剔除 1 个远距候选（%q），实际 %d 个", cands[2].NodeText, len(rejected))
	}
	// 保持输入顺序输出。
	if kept[0].NodeText != cands[0].NodeText || kept[1].NodeText != cands[1].NodeText {
		t.Fatalf("保留候选应保持输入顺序：got %q, %q", kept[0].NodeText, kept[1].NodeText)
	}
	// Top-K 之外的第 4 个候选（L2/0.60）既不保留也不剔除。
	for _, c := range append(append([]Candidate{}, kept...), rejected...) {
		if c.Stage == "L2" && c.Confidence == 0.60 {
			t.Fatalf("Top-K 之外的候选不应参与 L3 校验：NodeText=%q", c.NodeText)
		}
	}
}

// TDD-MATCH-05（单元）：topK ≤ 0 视为全量校验；空候选池安全返回。
func TestMatch05RefineByStructureAllAndEmpty(t *testing.T) {
	ref := SimHash64("登录 用户名 密码 提交")
	cands := []Candidate{
		{NodeText: "登录 用户名 密码 提交", Confidence: 0.9, Stage: "L1"},
		{NodeText: "advertisement banner cookie popup tracking", Confidence: 0.5, Stage: "L2"},
	}
	kept, rejected := RefineByStructure(cands, ref, 0)
	if len(kept) != 1 || len(rejected) != 1 {
		t.Fatalf("topK=0 应全量校验：kept=%d rejected=%d", len(kept), len(rejected))
	}
	kept, rejected = RefineByStructure(nil, ref, 5)
	if len(kept) != 0 || len(rejected) != 0 {
		t.Fatalf("空候选池应安全返回空：kept=%d rejected=%d", len(kept), len(rejected))
	}
}
