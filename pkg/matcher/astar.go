// A* 多步搜索（TDD-MATCH-06，spec/algorithm-kernel.md §5）。
//
// 中间置信度区间（θ_low ≤ C < θ_high）的候选触发 ε=0.02 探索或 A* 搜索：
//   - 深度钳制：基础深度 D_base=3；含流程标志（step/checkout）补偿 +2；
//     绝对上限 D_max=5 为硬边界（防 A* 深度爆炸）；
//   - 物理湮灭：命中 HTTP 4XX/5XX 的节点 Penalty=+∞，节点进入 AC 黑名单，
//     后续搜索直接跳过；
//   - 逻辑偏移：Penalty = C_base × [1 + (1/SimHash_Overlap)]²，
//     祖父回溯连坐（2 层向上）记连带惩罚，使搜索回溯到祖父的其他分支。
//
// 降级说明：与 TDD-MATCH-05 一致，本层为纯逻辑实现（零外部依赖）；
// 真实 DOM/AXTree 候选图输入通道落地后，仅需把 SearchNode.Children 换成
// 引擎候选流，决策与惩罚语义不变。
package matcher

import (
	"container/heap"
	"math"
	"math/rand"
	"sync"
)

// 决策阈值与搜索参数（spec/algorithm-kernel.md §5 / §5.1）。
const (
	ThetaHigh       = 0.85 // C ≥ θ_high：直接执行
	ThetaLow        = 0.50 // C < θ_low：LLM 兜底
	Epsilon         = 0.02 // 中间区间探索概率 ε
	SearchDepthBase = 3    // A* 基础深度 D_base
	SearchDepthMax  = 5    // A* 绝对上限 D_max（硬边界）
	FlowDepthBonus  = 2    // 流程标志（step/checkout）深度补偿
)

// SearchDecision 是置信度带的决策结果。
type SearchDecision string

const (
	DecisionDirect      SearchDecision = "direct"       // C ≥ θ_high：直接执行
	DecisionLLMFallback SearchDecision = "llm_fallback" // C < θ_low：LLM 兜底
	DecisionExplore     SearchDecision = "explore"      // 中间区间 + ε 探索命中
	DecisionAStar       SearchDecision = "astar"        // 中间区间 → A* 多步搜索
)

// Decide 依据置信度与 ε 探索概率给出决策。
// rng 为 nil 或 epsilon <= 0 时，中间区间稳定返回 AStar（确定性测试路径）；
// 探索仅在 rng != nil 且 epsilon > 0 时按随机数触发。
func Decide(conf, epsilon float64, rng *rand.Rand) SearchDecision {
	if conf >= ThetaHigh {
		return DecisionDirect
	}
	if conf < ThetaLow {
		return DecisionLLMFallback
	}
	if rng != nil && epsilon > 0 && rng.Float64() < epsilon {
		return DecisionExplore
	}
	return DecisionAStar
}

// SearchNode 是 A* 搜索图中的一个候选步骤节点。
// Children 为下一步候选（引擎/DOM 通道接线后由候选流填充）。
type SearchNode struct {
	Text       string  // 节点文本/语义标签
	Confidence float64 // C_base
	SimHash    uint64  // 结构签名；0 = 由 SimHash64(Text) 推导
	Flow       bool    // 流程标志（step/checkout）：深度补偿 +2
	HTTPStatus int     // 0/2xx 正常；4xx/5xx 物理湮灭（Penalty=+∞ 入黑名单）
	Children   []*SearchNode
}

// SearchStep 是结果路径中的一步（含惩罚明细，供审计/决策追踪）。
type SearchStep struct {
	Node    *SearchNode
	Penalty float64 // 本步逻辑偏移惩罚（路径内永不出现 +∞，湮灭节点只进 Annihilated）
	Overlap float64 // 本步与目标参考结构的 SimHash 重叠度
}

// LiabilityRecord 是祖父回溯连坐审计记录（TDD-MATCH-06 语义：2 层向上连坐）。
type LiabilityRecord struct {
	AncestorText string
	AtDepth      int     // 湮灭/剪枝节点深度（0-based）
	ReasonText   string  // 触发连坐的子孙节点
	ExtraPenalty float64 // 附加到祖父分支的有限惩罚
}

// SearchResult 是 A* 搜索输出。
type SearchResult struct {
	Decision       SearchDecision
	Path           []SearchStep // 深度受限的最优路径（Penalty 升序首条可行路径）
	Penalty        float64      // 累计惩罚（无路径 = +∞）
	Depth          int          // 路径节点数
	Annihilated    []*SearchNode
	JointLiability []LiabilityRecord
}

// PhysicalBlacklist 是运行时 AC 物理黑名单（内存态、并发安全）。
// 持久化（跨进程 AC 黑名单落库）归引擎/CDP 层，本层只保证搜索内一致性。
type PhysicalBlacklist struct {
	mu   sync.RWMutex
	seen map[string]struct{}
}

// NewPhysicalBlacklist 构造空物理黑名单。
func NewPhysicalBlacklist() *PhysicalBlacklist {
	return &PhysicalBlacklist{seen: make(map[string]struct{})}
}

// Has 判断节点文本是否在物理黑名单中。
func (b *PhysicalBlacklist) Has(text string) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.seen[text]
	return ok
}

// Add 将节点文本加入物理黑名单（幂等）。
func (b *PhysicalBlacklist) Add(text string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen[text] = struct{}{}
}

// Len 返回物理黑名单规模。
func (b *PhysicalBlacklist) Len() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.seen)
}

// SearchOptions 是 A* 搜索参数。
type SearchOptions struct {
	RefHash   uint64             // 目标参考结构 SimHash（0 = 中性，overlap 取 1）
	Blacklist *PhysicalBlacklist // 物理黑名单（可空）
}

// MaxSearchDepth 返回给定流程标志下的有效深度上限（D_base 或 +2，封顶 D_max）。
func MaxSearchDepth(flow bool) int {
	d := SearchDepthBase
	if flow {
		d += FlowDepthBonus
	}
	if d > SearchDepthMax {
		d = SearchDepthMax
	}
	return d
}

// logicalOffsetPenalty 计算逻辑偏移惩罚：C_base × [1 + (1/Overlap)]²。
// Overlap ≤ 0 视为结构完全无关 → +∞（避免除零；语义上不可达）。
func logicalOffsetPenalty(cBase, overlap float64) float64 {
	if overlap <= 0 {
		return math.Inf(1)
	}
	return cBase * math.Pow(1+1/overlap, 2)
}

// nodeHash 返回节点结构签名：显式 SimHash 优先，否则由文本推导。
func nodeHash(n *SearchNode) uint64 {
	if n.SimHash != 0 {
		return n.SimHash
	}
	return SimHash64(n.Text)
}

// overlapFor 计算节点与参考结构的重叠度；无参考（0）时中性取 1。
func overlapFor(n *SearchNode, ref uint64) float64 {
	if ref == 0 {
		return 1
	}
	return SimHashOverlap(nodeHash(n), ref)
}

// astarItem 是优先队列元素：路径 + 累计惩罚 + 入队序号（确定性平局裁决）。
type astarItem struct {
	steps   []SearchStep
	penalty float64
	seq     int64
}

type astarQueue []*astarItem

func (q astarQueue) Len() int { return len(q) }
func (q astarQueue) Less(i, j int) bool {
	if q[i].penalty != q[j].penalty {
		return q[i].penalty < q[j].penalty
	}
	if len(q[i].steps) != len(q[j].steps) {
		return len(q[i].steps) < len(q[j].steps)
	}
	return q[i].seq < q[j].seq
}
func (q astarQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *astarQueue) Push(x any)   { *q = append(*q, x.(*astarItem)) }
func (q *astarQueue) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return it
}

// AStarSearch 对中间置信度候选执行深度钳制 A* 搜索：
//  1. 根候选按自身逻辑偏移惩罚入队（黑名单/物理湮灭/结构无关根直接跳过）；
//  2. 每次弹出累计惩罚最小的路径；到达深度上限或无子节点即返回该路径；
//  3. 扩展子节点：物理湮灭（4XX/5XX）→ +∞ 剪枝并入黑名单、祖父连坐；
//     结构无关（overlap ≤ 0）→ 剪枝并连坐（不入黑名单）；
//     祖父（2 层向上）被连坐后，其子分支扩展附加有限惩罚，驱使回溯。
//
// 无可行路径时返回空 Path、Penalty=+∞（调用方按 NOT_FOUND 处理）。
func AStarSearch(roots []*SearchNode, opts SearchOptions) SearchResult {
	var res SearchResult
	res.Decision = DecisionAStar
	q := &astarQueue{}
	var seq int64
	liability := make(map[string]float64)

	liabilityRecord := func(path []SearchStep, reason *SearchNode, extra float64) {
		if len(path) < 2 {
			return // 无祖父（至少 2 层：父 + 祖父）
		}
		gp := path[len(path)-2].Node
		liability[gp.Text] += extra
		res.JointLiability = append(res.JointLiability, LiabilityRecord{
			AncestorText: gp.Text,
			AtDepth:      len(path), // reason 节点将位于 len(path) 深度
			ReasonText:   reason.Text,
			ExtraPenalty: extra,
		})
	}

	for _, r := range roots {
		if r == nil || opts.Blacklist.Has(r.Text) {
			continue
		}
		if r.HTTPStatus >= 400 && r.HTTPStatus <= 599 {
			opts.Blacklist.Add(r.Text)
			res.Annihilated = append(res.Annihilated, r)
			continue
		}
		ov := overlapFor(r, opts.RefHash)
		p := logicalOffsetPenalty(r.Confidence, ov)
		if math.IsInf(p, 1) {
			continue // 结构无关根：不可达
		}
		heap.Push(q, &astarItem{
			steps:   []SearchStep{{Node: r, Penalty: p, Overlap: ov}},
			penalty: p,
			seq:     seq,
		})
		seq++
	}

	for q.Len() > 0 {
		it := heap.Pop(q).(*astarItem)
		last := it.steps[len(it.steps)-1].Node
		capDepth := MaxSearchDepth(pathHasFlow(it.steps))
		if len(it.steps) >= capDepth || len(last.Children) == 0 {
			res.Path = it.steps
			res.Penalty = it.penalty
			res.Depth = len(it.steps)
			return res
		}
		for _, c := range last.Children {
			if c == nil || opts.Blacklist.Has(c.Text) {
				continue
			}
			ov := overlapFor(c, opts.RefHash)
			cp := logicalOffsetPenalty(c.Confidence, ov)
			if c.HTTPStatus >= 400 && c.HTTPStatus <= 599 {
				// 物理湮灭：Penalty=+∞，节点入 AC 黑名单，祖父连坐。
				opts.Blacklist.Add(c.Text)
				res.Annihilated = append(res.Annihilated, c)
				liabilityRecord(it.steps, c, finiteJointPenalty(c.Confidence))
				continue
			}
			if math.IsInf(cp, 1) {
				// 逻辑死路：结构与目标完全无关，剪枝 + 祖父连坐（不入黑名单）。
				liabilityRecord(it.steps, c, finiteJointPenalty(c.Confidence))
				continue
			}
			// 祖父连坐附加：last 是 c 的父节点，父的祖父连带惩罚影响本步。
			surcharge := liability[last.Text]
			newSteps := append(append([]SearchStep(nil), it.steps...),
				SearchStep{Node: c, Penalty: cp, Overlap: ov})
			heap.Push(q, &astarItem{
				steps:   newSteps,
				penalty: it.penalty + cp + surcharge,
				seq:     seq,
			})
			seq++
		}
	}

	res.Penalty = math.Inf(1)
	return res
}

// finiteJointPenalty 是祖父连坐的有限附加惩罚代理：
// 物理湮灭/逻辑死路自身的 +∞ 不能直接累加（会毒化整支），
// 取原因节点在 overlap=1 时的最大逻辑惩罚 C_base×4 作为「分支不可信」的有限代价。
func finiteJointPenalty(cBase float64) float64 {
	return cBase * 4
}

// pathHasFlow 判断路径中是否存在流程标志节点（决定深度补偿）。
func pathHasFlow(steps []SearchStep) bool {
	for _, s := range steps {
		if s.Node.Flow {
			return true
		}
	}
	return false
}
