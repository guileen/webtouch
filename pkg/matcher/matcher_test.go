// TDD-MATCH-01（L1 AC 命中）、TDD-MATCH-02（L2 同义词扰动）、
// TDD-MATCH-03（SQL 注入防护）与 TDD-MATCH-04（negative_keywords 黑名单）逻辑层测试。
// 集成用例（02/04/05）→ tests/integration/test_matcher_classifier.py；
// 安全用例（03）安全层 → tests/security/test_input_sanitization.py（引擎接线后点亮）。
package matcher

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// TDD-MATCH-01（P0/M4/单元）：页面节点文本 "登录" 且规则库含 text_pattern="登录" 时，
// L1 AC 自动机必须直接命中该节点入围候选池。
func TestMatch01L1AcHit(t *testing.T) {
	m := newTestMatcher(t, []Rule{{
		RuleType:    "text_pattern",
		TextPattern: "登录",
		Action:      "click",
		Confidence:  0.9,
		Status:      "active",
	}})
	defer m.Close()

	cands, err := m.MatchNode("登录")
	if err != nil {
		t.Fatalf("MatchNode 不应报错：%v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("候选池应含 1 个候选，实际 %d", len(cands))
	}
	if cands[0].NodeText != "登录" {
		t.Fatalf("候选节点文本应为 '登录'，实际 %q", cands[0].NodeText)
	}
	if len(cands[0].Matches) != 1 {
		t.Fatalf("应命中 1 条规则，实际 %d", len(cands[0].Matches))
	}
	hit := cands[0].Matches[0].Rule
	if hit.TextPattern != "登录" || hit.Action != "click" {
		t.Fatalf("命中规则不符：pattern=%q action=%q", hit.TextPattern, hit.Action)
	}
}

// TDD-MATCH-03（P0/M4/安全）逻辑层：恶意 target_semantic 必须被参数化查询安全处理，
// 数据库不抛错、uws_rules 表不被破坏。
func TestMatch03SqlInjectionSafe(t *testing.T) {
	m := newTestMatcher(t, []Rule{{
		RuleType:    "text_pattern",
		TextPattern: "登录",
		Action:      "click",
		Confidence:  0.9,
		Status:      "active",
	}})
	defer m.Close()

	payload := "'; DROP TABLE uws_rules;--"

	// 1) 恶意输入作为语义检索参数：不报错、返回空结果（参数化，不拼 SQL）。
	got, err := m.store.RulesBySemantic(payload)
	if err != nil {
		t.Fatalf("参数化查询不应报错：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("恶意输入不应命中任何规则，实际 %d 条", len(got))
	}

	// 2) 恶意输入作为节点文本：不 panic、不命中。
	cands, err := m.MatchNode(payload)
	if err != nil {
		t.Fatalf("MatchNode 恶意输入不应报错：%v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("恶意文本不应入候选池，实际 %d 个候选", len(cands))
	}

	// 3) 表与数据完好：uws_rules 仍存在且仅含原始 1 条规则。
	var n int
	if err := m.store.db.QueryRow("SELECT COUNT(*) FROM uws_rules").Scan(&n); err != nil {
		t.Fatalf("uws_rules 表应仍可查询：%v", err)
	}
	if n != 1 {
		t.Fatalf("uws_rules 行数应为 1，实际 %d（表被破坏）", n)
	}
}

// R-A（2026-08-25）：知识库规则携带 Selector，RulesBySemantic 必须原样返回，
// 供引擎 executePhysical 贯通到 CDP 物理层（DeepSeek chat 场景选择器接线）。
func TestRulesBySemanticCarriesSelector(t *testing.T) {
	m := newTestMatcher(t, []Rule{{
		RuleType:      "text_pattern",
		TextPattern:   "chat_input",
		SemanticLabel: "chat_input",
		Selector:      "textarea[placeholder*='发送消息'], textarea",
		Action:        "input",
		Confidence:    0.95,
		Source:        "component_gene",
		Status:        "active",
	}})
	defer m.Close()

	rules, err := m.RulesBySemantic("chat_input")
	if err != nil {
		t.Fatalf("RulesBySemantic 不应报错：%v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("应命中 1 条规则，实际 %d", len(rules))
	}
	if rules[0].Selector == "" {
		t.Fatal("规则 Selector 不应为空（物理层依赖它定位元素）")
	}
	if rules[0].Selector != "textarea[placeholder*='发送消息'], textarea" {
		t.Fatalf("选择器不符：%q", rules[0].Selector)
	}
}

// TDD-MATCH-02（P0/M4/单元）：页面包含文本 "Sign in"（不在 L1 规则内），
// 但 site_synonyms 含 TENTATIVE 同义词 "Sign in" 时，L2 以 0.2 权重扰动
// （0.5 语义召回先验 + 0.2）将其推入候选池（L3）。
func TestMatch02L2TentativeSynonym(t *testing.T) {
	m := newTestMatcher(t, []Rule{{
		RuleType:    "text_pattern",
		TextPattern: "登录",
		Action:      "click",
		Confidence:  0.95,
		Status:      "active",
	}})
	defer m.Close()

	if _, err := m.InsertSynonym(Synonym{Term: "Sign in", Status: StatusTentative, BaseWeight: 0.2}); err != nil {
		t.Fatalf("InsertSynonym 失败：%v", err)
	}

	cands, err := m.MatchQuery("Sign in", nil)
	if err != nil {
		t.Fatalf("MatchQuery 不应报错：%v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("L2 同义词应产生 1 个候选，实际 %d", len(cands))
	}
	c := cands[0]
	if c.Stage != "L2" {
		t.Errorf("候选来源应为 L2，实际 %q", c.Stage)
	}
	want := 0.5 + 0.2 // 语义召回先验 0.5 + TENTATIVE 增益 0.2
	if c.Confidence != want {
		t.Errorf("TENTATIVE 同义词置信度应为 %v，实际 %v", want, c.Confidence)
	}
}

// TDD-MATCH-02（逻辑层补充）：ACTIVE 同义词权重升至 1.0（C=1.0）；
// POISONED 同义词物理隔离，不再参与召回。
func TestMatch02ActiveAndPoisonedSynonym(t *testing.T) {
	m := newTestMatcher(t, nil)
	defer m.Close()

	if _, err := m.InsertSynonym(Synonym{Term: "Sign in", Status: StatusActive, BaseWeight: 1.0}); err != nil {
		t.Fatalf("InsertSynonym(ACTIVE) 失败：%v", err)
	}
	cands, err := m.MatchQuery("Sign in", nil)
	if err != nil || len(cands) != 1 {
		t.Fatalf("ACTIVE 同义词应召回 1 个候选：cands=%d err=%v", len(cands), err)
	}
	if cands[0].Confidence != 1.0 {
		t.Errorf("ACTIVE 同义词置信度应为 1.0，实际 %v", cands[0].Confidence)
	}

	if _, err := m.InsertSynonym(Synonym{Term: "Poisoned term", Status: StatusPoisoned}); err != nil {
		t.Fatalf("InsertSynonym(POISONED) 失败：%v", err)
	}
	if cands, err := m.MatchQuery("Poisoned term", nil); err != nil || len(cands) != 0 {
		t.Fatalf("POISONED 同义词不得召回：cands=%d err=%v", len(cands), err)
	}
}

// TDD-MATCH-04（P1/M4/单元）：节点上下文命中 negative_keywords（广告/弹窗容器）
// 时，即使文本命中 L1 规则也不得进入候选池，并显式返回黑名单拦截错误。
func TestMatch04NegativeKeywordFilter(t *testing.T) {
	m := newTestMatcher(t, []Rule{{
		RuleType:    "text_pattern",
		TextPattern: "登录",
		Action:      "click",
		Confidence:  0.95,
		Status:      "active",
	}})
	defer m.Close()

	if _, err := m.InsertNegativeKeyword("广告"); err != nil {
		t.Fatalf("InsertNegativeKeyword 失败：%v", err)
	}

	// 无黑名单上下文：L1 正常入围候选池。
	cands, err := m.MatchQuery("登录", nil)
	if err != nil {
		t.Fatalf("MatchQuery 基线不应报错：%v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("基线应命中 1 个候选，实际 %d", len(cands))
	}

	// 容器 zonal_keyword 命中黑名单 → 拦截。
	_, err = m.MatchQuery("登录", map[string]string{"zonal_keyword": "广告"})
	if !errors.Is(err, ErrBlockedByNegativeKeyword) {
		t.Fatalf("应返回 ErrBlockedByNegativeKeyword，实际 %v", err)
	}

	// 语义本身含黑名单 token → 同样拦截。
	if _, err := m.InsertNegativeKeyword("banner_zone"); err != nil {
		t.Fatalf("InsertNegativeKeyword(banner_zone) 失败：%v", err)
	}
	_, err = m.MatchQuery("banner_zone_login", nil)
	if !errors.Is(err, ErrBlockedByNegativeKeyword) {
		t.Fatalf("语义命中黑名单应返回 ErrBlockedByNegativeKeyword，实际 %v", err)
	}
}

// ── TDD-MATCH-06（P1/M4）：中间置信度 A* 多步搜索逻辑层 ──
// spec/algorithm-kernel.md §5：θ_low≤C<θ_high 触发 ε=0.02 探索或 A* 搜索；
// D_base=3（流程标志 +2）/D_max=5 硬边界；HTTP 4XX/5XX 物理湮灭（+∞ 入 AC 黑名单）；
// 逻辑偏移 Penalty = C_base × [1 + (1/SimHash_Overlap)]²，祖父回溯连坐。
// 集成层（引擎 runAction 消费）待 DOM/候选特征通道后接线（见 USER_ACTION.md）。

// 决策带：C ≥ θ_high 直接执行；C < θ_low LLM 兜底；中间区间 ε 探索或 A*。
func TestMatch06DecisionBands(t *testing.T) {
	if got := Decide(0.9, Epsilon, nil); got != DecisionDirect {
		t.Errorf("C=0.9 应 direct，实际 %q", got)
	}
	if got := Decide(0.3, Epsilon, nil); got != DecisionLLMFallback {
		t.Errorf("C=0.3 应 llm_fallback，实际 %q", got)
	}
	if got := Decide(0.7, Epsilon, nil); got != DecisionAStar {
		t.Errorf("中间区间且 rng=nil 应稳定 astar，实际 %q", got)
	}
	if got := Decide(0.7, 0, rand.New(rand.NewSource(1))); got != DecisionAStar {
		t.Errorf("ε=0 应 astar，实际 %q", got)
	}
	if got := Decide(0.7, 1.0, rand.New(rand.NewSource(1))); got != DecisionExplore {
		t.Errorf("ε=1.0 应 explore，实际 %q", got)
	}
	// 阈值边界：0.85 直接执行；0.50 落入中间区间（≥θ_low）。
	if got := Decide(0.85, 0, nil); got != DecisionDirect {
		t.Errorf("C=0.85（θ_high 边界）应 direct，实际 %q", got)
	}
	if got := Decide(0.50, 0, nil); got != DecisionAStar {
		t.Errorf("C=0.50（θ_low 边界）应 astar，实际 %q", got)
	}
}

// chainNode 构造一条深度为 depth 的链（flow 控制流程标志）。
func chainNode(depth int, flow bool) *SearchNode {
	root := &SearchNode{Text: "root", Confidence: 0.7, Flow: flow}
	cur := root
	for i := 1; i < depth; i++ {
		child := &SearchNode{Text: fmt.Sprintf("n%d", i), Confidence: 0.7}
		cur.Children = []*SearchNode{child}
		cur = child
	}
	return root
}

// 深度钳制：基础深度 D_base=3；含流程标志补偿 +2；绝对上限 D_max=5 硬边界。
func TestMatch06DepthClamp(t *testing.T) {
	noFlow := AStarSearch([]*SearchNode{chainNode(7, false)}, SearchOptions{RefHash: 0xffffffffffffffff})
	if noFlow.Depth != SearchDepthBase {
		t.Errorf("无流程标志深度应为 D_base=%d，实际 %d（路径：%v）", SearchDepthBase, noFlow.Depth, pathTexts(noFlow.Path))
	}
	if last := noFlow.Path[len(noFlow.Path)-1].Node.Text; last != "n2" {
		t.Errorf("无流程标志应止步于 n2，实际 %q", last)
	}

	flow := AStarSearch([]*SearchNode{chainNode(7, true)}, SearchOptions{RefHash: 0xffffffffffffffff})
	if flow.Depth != SearchDepthBase+FlowDepthBonus {
		t.Errorf("含流程标志深度应为 %d，实际 %d", SearchDepthBase+FlowDepthBonus, flow.Depth)
	}
	if flow.Depth > SearchDepthMax {
		t.Fatalf("深度不得突破 D_max=%d，实际 %d", SearchDepthMax, flow.Depth)
	}
}

// 逻辑偏移惩罚公式：C_base × [1 + (1/Overlap)]²；Overlap ≤ 0 → +∞。
func TestMatch06LogicalPenaltyFormula(t *testing.T) {
	cases := []struct {
		name    string
		overlap float64
		want    float64
	}{
		{"overlap=1", 1.0, 0.5 * 4},       // (1+1)² = 4
		{"overlap=0.5", 0.5, 0.5 * 9},     // (1+2)² = 9
		{"overlap=0", 0, math.Inf(1)},     // 结构完全无关
		{"overlap<0", -0.25, math.Inf(1)}, // 防御负值
	}
	for _, tc := range cases {
		got := logicalOffsetPenalty(0.5, tc.overlap)
		if math.IsInf(tc.want, 1) {
			if !math.IsInf(got, 1) {
				t.Errorf("%s：应 +∞，实际 %v", tc.name, got)
			}
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s：Penalty 应为 %v，实际 %v", tc.name, tc.want, got)
		}
	}
}

// 物理湮灭：HTTP 4XX/5XX → Penalty=+∞，节点入 AC 黑名单，不再参与后续搜索。
func TestMatch06PhysicalAnnihilationAndBlacklist(t *testing.T) {
	root := &SearchNode{Text: "root", Confidence: 0.7, Children: []*SearchNode{
		{Text: "bad-404", Confidence: 0.8, HTTPStatus: 404},
		{Text: "ok", Confidence: 0.6, HTTPStatus: 200},
	}}
	bl := NewPhysicalBlacklist()
	res := AStarSearch([]*SearchNode{root}, SearchOptions{RefHash: 0xffffffffffffffff, Blacklist: bl})
	if last := res.Path[len(res.Path)-1].Node.Text; last != "ok" {
		t.Fatalf("物理湮灭分支应被剔除、选择 ok，实际末步 %q", last)
	}
	if len(res.Annihilated) != 1 || res.Annihilated[0].Text != "bad-404" {
		t.Fatalf("应记录 1 个湮灭节点 bad-404，实际 %v", texts(res.Annihilated))
	}
	if !bl.Has("bad-404") {
		t.Fatal("湮灭节点应写入 AC 物理黑名单")
	}
	if math.IsInf(res.Penalty, 1) {
		t.Fatal("存在可行路径时 Penalty 不得为 +∞")
	}

	// 第二次搜索复用黑名单：bad-404 直接被跳过。
	res2 := AStarSearch([]*SearchNode{root}, SearchOptions{RefHash: 0xffffffffffffffff, Blacklist: bl})
	if last := res2.Path[len(res2.Path)-1].Node.Text; last != "ok" {
		t.Fatalf("复用黑名单后仍应选择 ok，实际 %q", last)
	}
}

// 根节点本身物理错误：湮灭并入黑名单，无路径返回 +∞。
func TestMatch06PhysicalRootAnnihilation(t *testing.T) {
	root := &SearchNode{Text: "dead-root", Confidence: 0.7, HTTPStatus: 500}
	bl := NewPhysicalBlacklist()
	res := AStarSearch([]*SearchNode{root}, SearchOptions{RefHash: 0xffffffffffffffff, Blacklist: bl})
	if len(res.Path) != 0 || !math.IsInf(res.Penalty, 1) {
		t.Fatalf("物理错误根应无路径且 Penalty=+∞，实际 path=%v penalty=%v", pathTexts(res.Path), res.Penalty)
	}
	if !bl.Has("dead-root") {
		t.Fatal("物理错误根应入黑名单")
	}
}

// 祖父回溯连坐：深度 ≥2 的节点湮灭时，祖父（2 层向上）记连带惩罚，
// 搜索回溯到祖父的其他分支（审计记录 AncestorText=祖父）。
func TestMatch06GrandparentJointLiability(t *testing.T) {
	gp1 := &SearchNode{Text: "gp1", Confidence: 0.7, Children: []*SearchNode{{
		Text: "p1", Confidence: 0.7, Children: []*SearchNode{
			{Text: "bad", Confidence: 0.8, HTTPStatus: 404},
		},
	}}}
	gp2 := &SearchNode{Text: "gp2", Confidence: 0.7, Children: []*SearchNode{{
		Text: "p2", Confidence: 0.7, Children: []*SearchNode{
			{Text: "good", Confidence: 0.8, HTTPStatus: 200},
		},
	}}}
	res := AStarSearch([]*SearchNode{gp1, gp2}, SearchOptions{RefHash: 0xffffffffffffffff})
	if last := res.Path[len(res.Path)-1].Node.Text; last != "good" {
		t.Fatalf("应回溯并选择 gp2 分支的 good，实际末步 %q", last)
	}
	if len(res.JointLiability) != 1 {
		t.Fatalf("应记录 1 条祖父连坐，实际 %d", len(res.JointLiability))
	}
	li := res.JointLiability[0]
	if li.AncestorText != "gp1" {
		t.Errorf("连坐祖父应为 gp1，实际 %q", li.AncestorText)
	}
	if !(li.ExtraPenalty > 0) || math.IsInf(li.ExtraPenalty, 1) {
		t.Errorf("连坐附加惩罚应为有限正数，实际 %v", li.ExtraPenalty)
	}
}

// A* 最优路径：两支均可行时选择累计逻辑惩罚更小的分支。
func TestMatch06AStarOptimalPath(t *testing.T) {
	ref := uint64(0xffffffffffffffff)
	close1 := uint64(0xffffffffffffffff) // overlap=1.0 → 单步 0.7×4=2.8
	far := uint64(0xffff0000ffff0000)    // 与 ref 汉明 32 → overlap=0.5 → 单步 0.7×9=6.3
	b1 := &SearchNode{Text: "b1", Confidence: 0.7, SimHash: close1, Children: []*SearchNode{
		{Text: "leaf1", Confidence: 0.7, SimHash: close1},
	}}
	b2 := &SearchNode{Text: "b2", Confidence: 0.7, SimHash: far, Children: []*SearchNode{
		{Text: "leaf2", Confidence: 0.7, SimHash: far},
	}}
	res := AStarSearch([]*SearchNode{b1, b2}, SearchOptions{RefHash: ref})
	if len(res.Path) < 2 || res.Path[0].Node.Text != "b1" {
		t.Fatalf("应选择低惩罚分支 b1，实际路径 %v", pathTexts(res.Path))
	}
}

// 黑名单中的根直接跳过：无路径（Penalty=+∞），不报错。
func TestMatch06BlacklistedRootSkipped(t *testing.T) {
	bl := NewPhysicalBlacklist()
	bl.Add("root")
	root := &SearchNode{Text: "root", Confidence: 0.7}
	res := AStarSearch([]*SearchNode{root}, SearchOptions{RefHash: 0xffffffffffffffff, Blacklist: bl})
	if len(res.Path) != 0 || !math.IsInf(res.Penalty, 1) {
		t.Fatalf("黑名单根应无路径且 Penalty=+∞，实际 path=%v penalty=%v", pathTexts(res.Path), res.Penalty)
	}
}

// texts 提取路径/节点列表文本（测试断言辅助）。
func texts(nodes []*SearchNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Text)
	}
	return out
}

// pathTexts 提取 SearchResult.Path 的节点文本序列。
func pathTexts(steps []SearchStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Node.Text)
	}
	return out
}

// newTestMatcher 打开内存 SQLite、写入规则并构建 matcher（测试夹具）。
func newTestMatcher(t *testing.T, rules []Rule) *Matcher {
	t.Helper()
	m, err := New(":memory:")
	if err != nil {
		t.Fatalf("New matcher 失败：%v", err)
	}
	for _, r := range rules {
		if _, err := m.store.InsertRule(r); err != nil {
			m.Close()
			t.Fatalf("InsertRule 失败：%v", err)
		}
	}
	// 重新加载 AC（规则入库后重建自动机）。
	if err := m.reload(); err != nil {
		m.Close()
		t.Fatalf("reload 失败：%v", err)
	}
	return m
}
