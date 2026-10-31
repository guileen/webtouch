// L3 SimHash 物理校验（TDD-MATCH-05，spec/algorithm-kernel.md §5/§6.2 step 3）。
//
// 三级级联召回的最后一级：仅对 Top-K 候选做结构比对，汉明距离 > D_h=5
// 的候选必须被剔除（防止「文本命中但结构完全不同的页面」被误执行）。
//
// 降级说明（USER_ACTION.md #6）：v1.4 冻结契约无 node_features 输入 RPC，
// 本层以「候选节点文本 → 64-bit SimHash」作为结构签名的逻辑层代理，
// 与 pipeline/cc_fetcher/topology.py 的 simhash64 逐位对齐（跨语言同构）；
// 真实 DOM/AXTree 特征通道落地后，仅需把 SimHash64 的输入换成结构特征串。
package matcher

import (
	"crypto/md5"
	"math/bits"
	"regexp"
	"sort"
	"strings"
)

// SimHashDistanceThreshold 是 L3 物理校验的汉明距离硬阈值（D_h，algorithm-kernel §5）。
const SimHashDistanceThreshold = 5

// simHashBits 是 64-bit SimHash 的位宽。
const simHashBits = 64

// simHashTokenRe 与 pipeline/cc_fetcher/topology.py 的 _TOKEN_RE 对齐：
// 小写字母/数字/CJK 词元，长度 ≥ 2（跳过单字符噪声与停用词）。
var simHashTokenRe = regexp.MustCompile(`[a-z0-9\p{Han}]{2,}`)

// Tokens 返回与 pipeline/cc_fetcher/topology.py _TOKEN_RE 逐位对齐的分词结果：
// 小写字母/数字/CJK 词元，长度 ≥ 2。供 SimHash64 与 pkg/classifier 特征契约
// （algorithm-kernel §5.0.1 f1 jaccard）共用，保证跨语言同构。
func Tokens(text string) []string {
	return simHashTokenRe.FindAllString(strings.ToLower(text), -1)
}

// SimHash64 对文本分词后计算 64-bit SimHash（词频加权、确定性）。
// 算法与 Python 侧 simhash64 完全一致：token → md5 前 8 字节小端；
// 逐位投票（命中 +weight，未命中 -weight）；v[i] > 0 置位。
func SimHash64(text string) uint64 {
	weights := make(map[string]int)
	for _, tok := range Tokens(text) {
		weights[tok]++
	}
	if len(weights) == 0 {
		return 0
	}
	var v [simHashBits]int
	for tok, weight := range weights {
		sum := md5.Sum([]byte(tok))
		h := uint64(sum[0]) | uint64(sum[1])<<8 | uint64(sum[2])<<16 | uint64(sum[3])<<24 |
			uint64(sum[4])<<32 | uint64(sum[5])<<40 | uint64(sum[6])<<48 | uint64(sum[7])<<56
		for i := 0; i < simHashBits; i++ {
			if h&(uint64(1)<<i) != 0 {
				v[i] += weight
			} else {
				v[i] -= weight
			}
		}
	}
	var out uint64
	for i := 0; i < simHashBits; i++ {
		if v[i] > 0 {
			out |= uint64(1) << i
		}
	}
	return out
}

// HammingDistance 返回两个 64-bit SimHash 的汉明距离。
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// SimHashOverlap 返回结构重叠度 ∈ [0,1]：1 - Hamming/64。
// 是 A* 逻辑偏移惩罚公式（Penalty = C_base × [1 + (1/Overlap)]²）的输入。
func SimHashOverlap(a, b uint64) float64 {
	return 1 - float64(HammingDistance(a, b))/simHashBits
}

// RefineByStructure 对 Top-K 候选执行 L3 SimHash 物理校验：
//  1. 按置信度降序选取前 topK 个候选（L3 仅校验 Top-K 短名单）；
//  2. 逐个计算候选结构 SimHash 并与 refHash 比对，汉明距离 > D_h=5 的候选被剔除；
//  3. 保留候选按输入顺序返回（调用方决策流不感知重排），剔除候选单独返回供审计。
//
// topK <= 0 视为全量校验；空候选池安全返回空结果。
func RefineByStructure(cands []Candidate, refHash uint64, topK int) (kept []Candidate, rejected []Candidate) {
	if len(cands) == 0 {
		return nil, nil
	}
	// ★Tier 1.5 接线：候选携带结构通道时，自动升级为双通道（文本 OR 结构[且身份兼容]）。
	// 参考结构哈希优先取调用方显式传入；否则从候选命中的最高置信度规则上取 StructHash。
	refStruct, oracleTag, oracleRole, hasStructRef := bestStructRule(cands)
	if !hasStructRef || refStruct == 0 {
		return refineTextChannel(cands, refHash, topK)
	}
	return RefineByStructureDual(cands, refHash, refStruct, oracleTag, oracleRole, topK)
}

// refineTextChannel 是原始文本单通道 L3（降级路径：无结构参考时保持既有语义）。
func refineTextChannel(cands []Candidate, refHash uint64, topK int) (kept []Candidate, rejected []Candidate) {
	n := len(cands)
	if topK <= 0 || topK > n {
		topK = n
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return cands[order[i]].Confidence > cands[order[j]].Confidence
	})
	inTopK := make(map[int]bool, topK)
	for _, i := range order[:topK] {
		inTopK[i] = true
	}
	kept = make([]Candidate, 0, topK)
	rejected = make([]Candidate, 0, topK)
	for i := range cands {
		if !inTopK[i] {
			continue
		}
		if HammingDistance(SimHash64(cands[i].NodeText), refHash) <= SimHashDistanceThreshold {
			kept = append(kept, cands[i])
		} else {
			rejected = append(rejected, cands[i])
		}
	}
	return kept, rejected
}

// RefineByStructureDual 是 Tier 1.5 双通道 L3（TDD-PROBE-06）：
// 候选保留 iff 文本 Hamming ≤ D_h **或**（结构 Hamming ≤ D_h 且 tag/role 兼容）。
// 这使 rename（文本漂移）时结构证据仍可确认同一拓扑位置（Tier 1.5 向量①）。
// oracleTag/oracleRole 为参考结构身份（规则/Golden 的 tag/role）；均为空时不拦截身份。
// refStructHash==0 时退化为文本单通道。保留候选按输入顺序返回。
func RefineByStructureDual(cands []Candidate, refHash, refStructHash uint64, oracleTag, oracleRole string, topK int) (kept []Candidate, rejected []Candidate) {
	if len(cands) == 0 {
		return nil, nil
	}
	n := len(cands)
	if topK <= 0 || topK > n {
		topK = n
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return cands[order[i]].Confidence > cands[order[j]].Confidence
	})
	inTopK := make(map[int]bool, topK)
	for _, i := range order[:topK] {
		inTopK[i] = true
	}
	kept = make([]Candidate, 0, topK)
	rejected = make([]Candidate, 0, topK)
	for i := range cands {
		if !inTopK[i] {
			continue
		}
		c := cands[i]
		textOK := HammingDistance(SimHash64(c.NodeText), refHash) <= SimHashDistanceThreshold
		structDist := HammingDistance(c.StructHash, refStructHash)
		// 身份兼容：候选与参考结构的 tag/role 兼容（Malhotra）。
		compat := StructRoleCompatible(
			StructFeature{Tag: c.Tag, Role: c.Role},
			StructFeature{Tag: oracleTag, Role: oracleRole},
		)
		structOK := structDist <= SimHashDistanceThreshold && compat
		if textOK || structOK {
			kept = append(kept, c)
		} else {
			rejected = append(rejected, c)
		}
	}
	return kept, rejected
}

// TopologicalPruneSoftThreshold 是 A*/ONNX 第一阶拓扑剪枝门的软阈值。
// 比 L3 硬阈值 D_h=5 更宽松（容忍结构通道的渲染抖动），但远小于全量（64）。
const TopologicalPruneSoftThreshold = 10

// TopologicalPruneGate 是 [State:D] A* 决策桥的第一阶拓扑剪枝门：
// 当且仅当候选结构哈希与参考结构哈希的 Hamming 距离 D_h ≤ 软阈值时，
// 节点方可进入 ONNX 张量重排区（防止全页候选图无条件全量灌入 ONNX 触发算力雪崩）。
//
// refStructHash==0（无结构参考）时门禁退化为 no-op（全量放行，保留既有路径）；
// 候选未提供 StructHash（=0）时视为无拓扑签名，被门禁排除（不进入 ONNX 重排区）。
func TopologicalPruneGate(cands []Candidate, refStructHash uint64, softThreshold int) []Candidate {
	if len(cands) == 0 {
		return nil
	}
	if refStructHash == 0 {
		return cands // 无结构参考：不启用拓扑剪枝（既有路径）
	}
	if softThreshold <= 0 {
		softThreshold = TopologicalPruneSoftThreshold
	}
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.StructHash == 0 {
			continue // 无拓扑签名：不得进入 ONNX 重排区（算力隔离）
		}
		if HammingDistance(c.StructHash, refStructHash) <= softThreshold {
			out = append(out, c)
		}
	}
	return out
}

// bestStructRule 从候选命中的规则上推导结构参考哈希与身份：
// 取最高置信度且 StructHash != 0 的规则（0 表示规则未提供结构通道）。
func bestStructRule(cands []Candidate) (hash uint64, tag string, role string, ok bool) {
	bestConf := -1.0
	for _, c := range cands {
		for _, m := range c.Matches {
			if m.Rule.StructHash != 0 && m.Rule.Confidence > bestConf {
				hash = m.Rule.StructHash
				role = m.Rule.Role
				tag = "" // 规则表未携带 tag 列；结构性 tag 校验由结构哈希承担
				bestConf = m.Rule.Confidence
				ok = true
			}
		}
	}
	return hash, tag, role, ok
}
