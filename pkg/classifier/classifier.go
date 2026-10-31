package classifier

import (
	"context"
	"sort"
)

// 阈值常量（spec/algorithm-kernel.md §5：C≥θ_high 直接执行；C<θ_low LLM 兜底）。
const (
	ThresholdHigh = 0.85
	ThresholdLow  = 0.50
)

// Scorer 对候选节点特征向量打分，返回置信度 C∈[0,1]。
// 真实 ONNX 运行时适配器待 onnxruntime 就绪后实现（见 USER_ACTION.md）。
type Scorer interface {
	Score(ctx context.Context, features []float32) (float64, error)
}

// Decision 是分类后的三态决策。
type Decision int

const (
	DecisionExecute  Decision = iota // C ≥ θ_high：直接执行
	DecisionExplore                  // θ_low ≤ C < θ_high：ε探索 / A*
	DecisionFallback                 // C < θ_low：LLM 兜底 + 日志
)

func (d Decision) String() string {
	switch d {
	case DecisionExecute:
		return "EXECUTE"
	case DecisionExplore:
		return "EXPLORE"
	case DecisionFallback:
		return "FALLBACK"
	}
	return "UNKNOWN"
}

// Classifier 是决策核：消费 Scorer 分数并映射为三态决策。
type Classifier struct {
	scorer Scorer
}

// New 构造决策核。
func New(scorer Scorer) *Classifier {
	return &Classifier{scorer: scorer}
}

// Evaluate 对特征向量打分并映射决策；分数原样透传，Scorer 错误向上传播。
func (c *Classifier) Evaluate(ctx context.Context, features []float32) (Decision, float64, error) {
	score, err := c.scorer.Score(ctx, features)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case score >= ThresholdHigh:
		return DecisionExecute, score, nil
	case score < ThresholdLow:
		return DecisionFallback, score, nil
	default:
		return DecisionExplore, score, nil
	}
}

// ScorePair 是黄金回归集的一条样本。
type ScorePair struct {
	Positive bool
	Score    float64
}

// AUC 用 Mann-Whitney U（秩和法）计算黄金集的 ROC-AUC（TDD-ONNX-03 回归线）。
// tie 分数取平均秩；正/负样本任一为空返回 0。
func AUC(pairs []ScorePair) float64 {
	sorted := make([]ScorePair, len(pairs))
	copy(sorted, pairs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score < sorted[j].Score })

	nPos, nNeg := 0, 0
	for _, p := range sorted {
		if p.Positive {
			nPos++
		} else {
			nNeg++
		}
	}
	if nPos == 0 || nNeg == 0 {
		return 0
	}

	rankSum := 0.0
	for i := 0; i < len(sorted); {
		j := i
		for j < len(sorted) && sorted[j].Score == sorted[i].Score {
			j++
		}
		avgRank := float64(i+j+1) / 2 // 1-based 平均秩
		for k := i; k < j; k++ {
			if sorted[k].Positive {
				rankSum += avgRank
			}
		}
		i = j
	}

	fPos, fNeg := float64(nPos), float64(nNeg)
	return (rankSum - fPos*(fPos+1)/2) / (fPos * fNeg)
}
