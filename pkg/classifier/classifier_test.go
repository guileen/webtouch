// TDD-ONNX-01/03（逻辑层）：mock scorer 注入验证阈值分离与黄金集 AUC。
// 性能红线（TDD-ONNX-02）→ benchmarks/bench_classifier.py。
package classifier

import (
	"context"
	"errors"
	"testing"
)

type stubScorer struct {
	score float64
	err   error
}

func (s stubScorer) Score(_ context.Context, _ []float32) (float64, error) {
	return s.score, s.err
}

func TestOnnx01HardNegativeSeparation(t *testing.T) {
	c := New(stubScorer{score: 0.90})
	dec, score, err := c.Evaluate(context.Background(), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec != DecisionExecute {
		t.Errorf("正样本 0.90 应 EXECUTE，实际 %s", dec)
	}
	if score != 0.90 {
		t.Errorf("分数应透传 0.90，实际 %v", score)
	}

	c = New(stubScorer{score: 0.30})
	dec, _, err = c.Evaluate(context.Background(), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec != DecisionFallback {
		t.Errorf("困难负样本 0.30 应 FALLBACK，实际 %s", dec)
	}
}

func TestOnnx01ThresholdBoundaries(t *testing.T) {
	cases := []struct {
		score float64
		want  Decision
	}{
		{ThresholdHigh, DecisionExecute},         // C ≥ θ_high → 执行
		{ThresholdHigh - 0.001, DecisionExplore}, // 中间区间 → ε探索/A*
		{ThresholdLow, DecisionExplore},
		{ThresholdLow - 0.001, DecisionFallback}, // C < θ_low → LLM 兜底
	}
	for _, tc := range cases {
		c := New(stubScorer{score: tc.score})
		dec, _, err := c.Evaluate(context.Background(), nil)
		if err != nil {
			t.Fatalf("score=%v: %v", tc.score, err)
		}
		if dec != tc.want {
			t.Errorf("score=%v 应 %s，实际 %s", tc.score, tc.want, dec)
		}
	}
}

func TestOnnx01ScorerErrorPropagates(t *testing.T) {
	c := New(stubScorer{err: errors.New("inference failed")})
	if _, _, err := c.Evaluate(context.Background(), nil); err == nil {
		t.Fatal("Scorer 错误应向上透传")
	}
}

func TestOnnx03GoldenRegressionAuc(t *testing.T) {
	// 黄金困难负样本集：正样本（兄弟节点）9 个高分 + 负样本（SimHash 近邻干扰）9 个低分，
	// 唯一 1 对混淆（负 0.87 > 正 0.86）→ AUC≈0.98，验收线 ≥0.90。
	pairs := []ScorePair{
		{Positive: true, Score: 0.95},
		{Positive: true, Score: 0.94},
		{Positive: true, Score: 0.93},
		{Positive: true, Score: 0.92},
		{Positive: true, Score: 0.91},
		{Positive: true, Score: 0.90},
		{Positive: true, Score: 0.89},
		{Positive: true, Score: 0.88},
		{Positive: true, Score: 0.86},
		{Positive: false, Score: 0.87}, // 唯一混淆对
		{Positive: false, Score: 0.85},
		{Positive: false, Score: 0.80},
		{Positive: false, Score: 0.75},
		{Positive: false, Score: 0.70},
		{Positive: false, Score: 0.65},
		{Positive: false, Score: 0.60},
		{Positive: false, Score: 0.55},
		{Positive: false, Score: 0.45},
	}
	if auc := AUC(pairs); auc < 0.90 {
		t.Errorf("黄金集 AUC=%v，应 ≥0.90", auc)
	}
}

func TestOnnx03AucDetectsDegradation(t *testing.T) {
	// 倒置分数（正样本全低分）必须捕获劣化：AUC 显著低于 0.90。
	pairs := []ScorePair{
		{Positive: true, Score: 0.30},
		{Positive: true, Score: 0.20},
		{Positive: true, Score: 0.10},
		{Positive: false, Score: 0.95},
		{Positive: false, Score: 0.90},
		{Positive: false, Score: 0.85},
	}
	if auc := AUC(pairs); auc >= 0.10 {
		t.Errorf("劣化模型 AUC=%v，应 <0.10", auc)
	}
}
