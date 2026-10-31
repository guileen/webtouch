// 实验探针一：ast_semantic_hash LSH 的拓扑连续性探测（research-竞品调研与零点线-2026-08-28 §State:D）。
//
// 公理：Lipschitz 连续性边界定理。SPA（React/Vue）在 VDOM diff 与 Hydration 中高频注入/销毁
// 无语义幽灵包装节点（Fragment/Slot/CSS-in-JS Hash Class），导致 DOM 树局部深度与节点度数
// 剧烈非线性形变，而视觉与操作语义保持静止。若特征提取向量对树编辑距离（TED）缺乏拓扑阻尼，
// LSH 会把同构语义判为正交向量（哈希雪崩）。
//
// 本探针是「实证实验」而非验收门禁：测量无阻尼 / 阻尼两套特征流在
//
//	① 祖先链插入 1~3 层 <div> 包装（同构异形注入）
//	② 追加/乱序动态 class（css-1a2b3c 属性噪声注入）
//
// 下的 Jaccard 相似度与 SimHash 重叠度，输出存活判定（J ≥ 0.85）。
package matcher

import (
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// ---- 合成 SPA DOM 夹具（纯逻辑层，确定性） ----

type probeNode struct {
	tag      string
	role     string
	text     string
	attrs    []string // 稳定属性名（aria-*/data-*），不含 class
	classes  []string // 动态 class（css-1a2b3c 类噪声）
	children []*probeNode
}

// buildProbePage 构造一棵典型 SPA 页面树；wrappers>0 时在目标按钮祖先链
// 直接上方插入 wrappers 层无语义 <div> 幽灵包装（role=generic + hash class）。
func buildProbePage(wrappers int) (root, target *probeNode) {
	root = &probeNode{tag: "html", role: "document"}
	body := &probeNode{tag: "body", role: "main"}
	app := &probeNode{tag: "div", role: "application", classes: []string{"app-root"}}
	main := &probeNode{tag: "main", role: "region"}
	section := &probeNode{tag: "section", role: "group", text: "结算", attrs: []string{"data-testid", "aria-label"}}
	grid := &probeNode{tag: "div", role: "list", classes: []string{"episode-grid"}}
	target = &probeNode{tag: "button", role: "button", text: "提交订单", attrs: []string{"data-testid", "aria-label"}}
	grid.children = []*probeNode{target}
	section.children = []*probeNode{grid}
	main.children = []*probeNode{section}
	app.children = []*probeNode{main}
	body.children = []*probeNode{app}
	root.children = []*probeNode{body}
	for i := 0; i < wrappers; i++ {
		w := &probeNode{tag: "div", role: "generic", classes: []string{"css-1a2b3c", "css-x9y8z7"}}
		w.children = []*probeNode{grid.children[0]}
		grid.children = []*probeNode{w}
	}
	return root, target
}

// probeAncestors 返回 root→target 的完整路径（含 target，末端为 target）。
func probeAncestors(root, target *probeNode) []*probeNode {
	var path []*probeNode
	var walk func(n *probeNode, cur []*probeNode) bool
	walk = func(n *probeNode, cur []*probeNode) bool {
		if n == target {
			path = append(append([]*probeNode(nil), cur...), n)
			return true
		}
		for _, c := range n.children {
			if walk(c, append(cur, n)) {
				return true
			}
		}
		return false
	}
	walk(root, nil)
	return path
}

// probeSubtreePhrases 追加子树特征：相对目标节点的层数（r1/r2…）。
func probeSubtreePhrases(n *probeNode, rel int, ph *[]string) {
	for _, c := range n.children {
		*ph = append(*ph, "sub:"+c.tag+"@r"+strconv.Itoa(rel+1))
		*ph = append(*ph, "subrole:"+c.role+"@r"+strconv.Itoa(rel+1))
		for _, a := range c.attrs {
			*ph = append(*ph, "subattr:"+a)
		}
		probeSubtreePhrases(c, rel+1, ph)
	}
}

// featurePhrasesV1 无阻尼特征流：绝对深度 + 有序祖先链（含精确距离）+ 全属性（含 class）。
// 目的：演示「缺乏拓扑阻尼」时 LSH 对祖先链插入的哈希雪崩。
func featurePhrasesV1(anc []*probeNode, target *probeNode) []string {
	depth := len(anc) // target 绝对深度（anc 含 target）
	ph := []string{
		"tag:" + target.tag,
		"role:" + target.role,
		"depth:d" + strconv.Itoa(depth),
	}
	ph = append(ph, Tokens(target.text)...)
	for _, a := range target.attrs {
		ph = append(ph, "attr:"+a)
	}
	for _, c := range target.classes {
		ph = append(ph, "cls:"+c)
	}
	probeSubtreePhrases(target, 0, &ph)
	// 祖先链：距离从 1（父）开始；距离 token 用 dN 前缀保证 ≥2 字符可分词。
	for i := len(anc) - 2; i >= 0; i-- {
		a := anc[i]
		dist := len(anc) - 1 - i
		ph = append(ph, "anc:"+a.tag+"@d"+strconv.Itoa(dist))
		ph = append(ph, "ancrole:"+a.role+"@d"+strconv.Itoa(dist))
		for _, c := range a.classes {
			ph = append(ph, "anccls:"+c)
		}
	}
	return ph
}

// featurePhrasesV2 拓扑阻尼特征流：相对深度 + 仅取最近「语义祖先」（跳过 generic/空 role
// 幽灵包装）+ 稳定属性（不含 class）。即「树高度归一化」：位置全部相对子树根，
// 祖先语境只保留最近语义容器，包装节点不进入 LSH 流。
func featurePhrasesV2(anc []*probeNode, target *probeNode) []string {
	ph := []string{
		"tag:" + target.tag,
		"role:" + target.role,
	}
	ph = append(ph, Tokens(target.text)...)
	for _, a := range target.attrs {
		ph = append(ph, "attr:"+a)
	}
	probeSubtreePhrases(target, 0, &ph)
	for i := len(anc) - 2; i >= 0; i-- {
		a := anc[i]
		if a.role == "" || a.role == "generic" || a.role == "none" {
			continue // 拓扑阻尼：跳过无语义幽灵包装节点
		}
		ph = append(ph, "sem-anc-role:"+a.role)
		break // 只保留最近语义祖先（容器消歧的最小语境）
	}
	return ph
}

// featurePhrasesNoise 噪声探针特征流 = 阻尼流 + 目标自身 class（用于测量衰减梯度）。
func featurePhrasesNoise(anc []*probeNode, target *probeNode) []string {
	ph := featurePhrasesV2(anc, target)
	for _, c := range target.classes {
		ph = append(ph, "cls:"+c)
	}
	return ph
}

// probeSimTokens 与 SimHash64 同口径：把特征短语拼接后按 _TOKEN_RE 分词。
func probeSimTokens(ph []string) []string {
	return Tokens(strings.Join(ph, " "))
}

// probeJaccard 返回两个特征流的 Jaccard 相似度（token 集合口径）。
func probeJaccard(a, b []string) float64 {
	sa, sb := probeSimTokens(a), probeSimTokens(b)
	setA := make(map[string]bool, len(sa))
	setB := make(map[string]bool, len(sb))
	for _, t := range sa {
		setA[t] = true
	}
	for _, t := range sb {
		setB[t] = true
	}
	inter := 0
	for t := range setA {
		if setB[t] {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 1
	}
	return float64(inter) / float64(union)
}

// probeOverlap 返回 SimHash 重叠度（1 - 汉明/64，即 LSH 碰撞率的工程口径）。
func probeOverlap(a, b []string) float64 {
	return SimHashOverlap(SimHash64(strings.Join(a, " ")), SimHash64(strings.Join(b, " ")))
}

// probeHamming 返回两个特征流的 LSH 汉明距离（对照 D_h=5 硬阈值）。
func probeHamming(a, b []string) int {
	return HammingDistance(SimHash64(strings.Join(a, " ")), SimHash64(strings.Join(b, " ")))
}

// TestProbeLshTopologicalContinuityWrapper 探测面①：祖先链插入 1~3 层 <div> 包装。
// 存活判定按实验表双口径输出：
//   - LSH 决策口径：无阻尼流 Hamming > D_h=5 → L3 判为「结构正交」→ 同构节点被剔除（哈希雪崩）；
//     阻尼流 Hamming ≤ D_h=5 且 J ≥ 0.85 → 特征向量可用（树高度归一化生效）。
func TestProbeLshTopologicalContinuityWrapper(t *testing.T) {
	for k := 1; k <= 3; k++ {
		root0, tgt0 := buildProbePage(0)
		root1, tgt1 := buildProbePage(k)
		anc0 := probeAncestors(root0, tgt0)
		anc1 := probeAncestors(root1, tgt1)

		v1a := featurePhrasesV1(anc0, tgt0)
		v1b := featurePhrasesV1(anc1, tgt1)
		v2a := featurePhrasesV2(anc0, tgt0)
		v2b := featurePhrasesV2(anc1, tgt1)

		j1, o1, h1 := probeJaccard(v1a, v1b), probeOverlap(v1a, v1b), probeHamming(v1a, v1b)
		j2, o2, h2 := probeJaccard(v2a, v2b), probeOverlap(v2a, v2b), probeHamming(v2a, v2b)
		t.Logf("wrappers=%d | 无阻尼 J=%.4f overlap=%.4f hamming=%d(L3剔除=%v) | 阻尼 J=%.4f overlap=%.4f hamming=%d(L3剔除=%v)",
			k, j1, o1, h1, h1 > SimHashDistanceThreshold, j2, o2, h2, h2 > SimHashDistanceThreshold)

		if h1 <= SimHashDistanceThreshold {
			t.Errorf("wrappers=%d：无阻尼特征流 hamming=%d 未越过 D_h=%d（哈希雪崩未在决策空间复现）",
				k, h1, SimHashDistanceThreshold)
		}
		if j2 < 0.85 || h2 > SimHashDistanceThreshold {
			t.Errorf("wrappers=%d：阻尼特征流 J=%.4f/hamming=%d 未达存活线（J≥0.85 且 ≤D_h=%d）",
				k, j2, h2, SimHashDistanceThreshold)
		}
	}
}

// TestProbeLshAttributeNoiseDecay 探测面②：追加/乱序动态 class（css-1a2b3c）。
// 存活判定（实验表）：Jaccard 相似度衰减必须平滑、严禁阶跃式断崖；
// 附加发现口径：L3 决策（Hamming > D_h=5 即剔除）存在二元断崖——相似度平滑衰减时
// 决策已一步归零。对照：按 spec §4.1「仅保留 aria-*/data-*」排除 class 的阻尼流 J≡1。
func TestProbeLshAttributeNoiseDecay(t *testing.T) {
	noisePool := []string{
		"css-1a2b3c", "css-x9y8z7", "css-k4j2h1", "css-p9q0r2",
		"css-m3n4b5", "css-v6c7x8", "css-z2w3y4", "css-a5s6d7",
		"css-f8g9h0", "css-j1k2l3", "css-q3w4e5",
	}
	rng := rand.New(rand.NewSource(7))
	shuffled := append([]string(nil), noisePool...)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	root0, tgt0 := buildProbePage(0)
	anc0 := probeAncestors(root0, tgt0)
	base := featurePhrasesNoise(anc0, tgt0)
	if j := probeJaccard(base, base); j != 1 {
		t.Fatalf("基线自相似度应为 1，实际 %.4f", j)
	}

	var jacc []float64
	prev := 1.0
	maxStep := 0.0
	cliffK := -1 // 汉明首次 > D_h=5 的噪声量（L3 二元断崖）
	for k := 0; k <= 10; k++ {
		root, tgt := buildProbePage(0)
		anc := probeAncestors(root, tgt)
		tgt.classes = shuffled[:k]
		ph := featurePhrasesNoise(anc, tgt)
		j := probeJaccard(base, ph)
		o := probeOverlap(base, ph)
		h := probeHamming(base, ph)
		jacc = append(jacc, j)
		step := prev - j
		if step > maxStep {
			maxStep = step
		}
		if cliffK < 0 && h > SimHashDistanceThreshold {
			cliffK = k
		}
		t.Logf("noise_k=%2d | J=%.4f overlap=%.4f hamming=%d（D_h=%d 剔除=%v）",
			k, j, o, h, SimHashDistanceThreshold, h > SimHashDistanceThreshold)
		prev = j
	}

	if maxStep > 0.25 {
		t.Errorf("噪声衰减出现阶跃式断崖：max consecutive ΔJ=%.4f > 0.25", maxStep)
	}
	for i := 1; i < len(jacc); i++ {
		if jacc[i] > jacc[i-1]+1e-9 {
			t.Errorf("噪声衰减非单调：J(k=%d)=%.4f > J(k=%d)=%.4f", i, jacc[i], i-1, jacc[i-1])
		}
	}
	t.Logf("噪声衰减 maxStep=%.4f（≤0.25 平滑）；L3 二元断崖出现在 noise_k=%d（此后 Hamming>D_h=5 全剔除）",
		maxStep, cliffK)
	if cliffK < 0 {
		t.Logf("10 级噪声内汉明均未越过 D_h=5（本夹具下 L3 未见断崖）")
	}

	// 对照：spec §4.1 属性白名单（仅 aria-*/data-*，排除 class）→ 阻尼流对属性噪声完全免疫。
	rootN, tgtN := buildProbePage(0)
	ancN := probeAncestors(rootN, tgtN)
	tgtN.classes = shuffled[:10]
	jExcl := probeJaccard(featurePhrasesV2(ancN, tgtN), featurePhrasesV2(anc0, tgt0))
	hExcl := probeHamming(featurePhrasesV2(ancN, tgtN), featurePhrasesV2(anc0, tgt0))
	t.Logf("排除 class 的阻尼流：10 级噪声下 J=%.4f hamming=%d（免疫）", jExcl, hExcl)
	if jExcl != 1 || hExcl != 0 {
		t.Errorf("排除 class 后属性噪声应完全免疫（J=1/hamming=0），实际 J=%.4f/hamming=%d", jExcl, hExcl)
	}
}

// TestProbeLshDampedBeatsUndamped 验证阻尼流在每个包装层数下都严格优于无阻尼流
// （树高度归一化带来的单调收益，作为设计结论的回归锚点）。
func TestProbeLshDampedBeatsUndamped(t *testing.T) {
	for k := 1; k <= 3; k++ {
		root0, tgt0 := buildProbePage(0)
		root1, tgt1 := buildProbePage(k)
		anc0 := probeAncestors(root0, tgt0)
		anc1 := probeAncestors(root1, tgt1)
		j1 := probeJaccard(featurePhrasesV1(anc0, tgt0), featurePhrasesV1(anc1, tgt1))
		j2 := probeJaccard(featurePhrasesV2(anc0, tgt0), featurePhrasesV2(anc1, tgt1))
		if !(j2 >= j1) {
			t.Fatalf("wrappers=%d：阻尼流 J=%.4f 应 ≥ 无阻尼流 J=%.4f", k, j2, j1)
		}
	}
}

// fitLinearR2 噪声衰减线性拟合优度（供报告：越接近 1 越「平滑线性」）。
func fitLinearR2(ys []float64) float64 {
	n := len(ys)
	if n < 3 {
		return math.NaN()
	}
	xs := make([]float64, n)
	for i := range xs {
		xs[i] = float64(i)
	}
	meanX, meanY := 0.0, 0.0
	for i := range xs {
		meanX += xs[i]
		meanY += ys[i]
	}
	meanX /= float64(n)
	meanY /= float64(n)
	var sxx, sxy, syy float64
	for i := range xs {
		dx, dy := xs[i]-meanX, ys[i]-meanY
		sxx += dx * dx
		sxy += dx * dy
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return math.NaN()
	}
	r := sxy / math.Sqrt(sxx*syy)
	return r * r
}

// TestProbeLshNoiseDecayLinearFit 输出噪声衰减的线性拟合优度（R²），
// 供「平滑线性衰减」判定参考；R² 低于 0.8 视为衰减形态偏离线性（需报告）。
func TestProbeLshNoiseDecayLinearFit(t *testing.T) {
	noisePool := []string{
		"css-1a2b3c", "css-x9y8z7", "css-k4j2h1", "css-p9q0r2",
		"css-m3n4b5", "css-v6c7x8", "css-z2w3y4", "css-a5s6d7",
		"css-f8g9h0", "css-j1k2l3", "css-q3w4e5",
	}
	root0, tgt0 := buildProbePage(0)
	anc0 := probeAncestors(root0, tgt0)
	base := featurePhrasesNoise(anc0, tgt0)
	var jacc []float64
	for k := 0; k <= 10; k++ {
		root, tgt := buildProbePage(0)
		anc := probeAncestors(root, tgt)
		tgt.classes = noisePool[:k]
		jacc = append(jacc, probeJaccard(base, featurePhrasesNoise(anc, tgt)))
	}
	r2 := fitLinearR2(jacc)
	t.Logf("噪声衰减线性拟合 R²=%.4f（1=完全线性；<0.8 需报告形态偏离）", r2)
}
