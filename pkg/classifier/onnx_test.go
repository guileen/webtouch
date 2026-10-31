// 真实 ONNX（A 案 ort-go CGO）测试：
//   - Features 与 pipeline/exporter/train_onnx.py feature_vector 逐位对拍（golden.json）；
//   - OnnxScorer 对黄金 (candidate, target) 打分与 Python onnxruntime 输出一致；
//   - 空特征 Sanity Check（OnnxReady 语义）。
//
// ORT 共享库缺失时自动 skip（CI/无库环境不阻塞 pre-commit），
// 与 CDP 集成测试「无 Chrome 自动 skip」同一纪律。
package classifier

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

type goldenPair struct {
	Candidate string    `json:"candidate"`
	Target    string    `json:"target"`
	Features  []float32 `json:"features"`
	Score     float64   `json:"score"`
}

func testdataDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata")
}

func onnxLibPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("ONNXRUNTIME_SHARED_LIBRARY_PATH"); p != "" {
		return p
	}
	p := filepath.Join("..", "..", "build", "ort", "libonnxruntime.1.23.0.dylib")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func loadGolden(t *testing.T) []goldenPair {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testdataDir(t), "golden.json"))
	if err != nil {
		t.Fatalf("读 golden.json 失败: %v", err)
	}
	var pairs []goldenPair
	if err := json.Unmarshal(data, &pairs); err != nil {
		t.Fatalf("解析 golden.json 失败: %v", err)
	}
	return pairs
}

func TestFeaturesAlignedWithPython(t *testing.T) {
	for _, p := range loadGolden(t) {
		got := Features(p.Candidate, p.Target)
		if len(got) != len(p.Features) {
			t.Fatalf("%q×%q: 维度 %d != %d", p.Candidate, p.Target, len(got), len(p.Features))
		}
		for i := range got {
			if math.Abs(float64(got[i])-float64(p.Features[i])) > 1e-6 {
				t.Errorf("%q×%q f%d: %v != %v（与 Python feature_vector 不一致）",
					p.Candidate, p.Target, i, got[i], p.Features[i])
			}
		}
	}
}

func TestOnnxScorerGoldenScores(t *testing.T) {
	lib := onnxLibPath(t)
	if lib == "" {
		t.Skip("未找到 libonnxruntime（设置 ONNXRUNTIME_SHARED_LIBRARY_PATH 或先 make ort-lib）")
	}
	model, err := os.ReadFile(filepath.Join(testdataDir(t), "classifier.onnx"))
	if err != nil {
		t.Fatalf("读测试模型失败: %v", err)
	}
	sc, err := NewOnnxScorer(lib, model)
	if err != nil {
		t.Fatalf("NewOnnxScorer: %v", err)
	}
	defer sc.Close()

	for _, p := range loadGolden(t) {
		got, err := sc.Score(context.Background(), Features(p.Candidate, p.Target))
		if err != nil {
			t.Fatalf("Score(%q, %q): %v", p.Candidate, p.Target, err)
		}
		if math.Abs(got-p.Score) > 1e-4 {
			t.Errorf("%q×%q: Go=%v Python ORT=%v（分差超 1e-4）", p.Candidate, p.Target, got, p.Score)
		}
	}
}

func TestOnnxScorerSanityEmptyFeatures(t *testing.T) {
	lib := onnxLibPath(t)
	if lib == "" {
		t.Skip("未找到 libonnxruntime")
	}
	model, err := os.ReadFile(filepath.Join(testdataDir(t), "classifier.onnx"))
	if err != nil {
		t.Fatalf("读测试模型失败: %v", err)
	}
	// NewOnnxScorer 内建空特征 Sanity Check：能构造成功即 OnnxReady 语义成立。
	sc, err := NewOnnxScorer(lib, model)
	if err != nil {
		t.Fatalf("NewOnnxScorer 空特征 sanity: %v", err)
	}
	defer sc.Close()
	got, err := sc.Score(context.Background(), make([]float32, NFeatures))
	if err != nil {
		t.Fatalf("空特征 Score: %v", err)
	}
	if math.Abs(got-0.000802) > 1e-3 {
		t.Errorf("空特征概率 %v != 0.000802（Python ORT 参考）", got)
	}
}

func TestOnnxScorerDecisionBand(t *testing.T) {
	lib := onnxLibPath(t)
	if lib == "" {
		t.Skip("未找到 libonnxruntime")
	}
	model, err := os.ReadFile(filepath.Join(testdataDir(t), "classifier.onnx"))
	if err != nil {
		t.Fatalf("读测试模型失败: %v", err)
	}
	sc, err := NewOnnxScorer(lib, model)
	if err != nil {
		t.Fatalf("NewOnnxScorer: %v", err)
	}
	defer sc.Close()
	clf := New(sc)
	ctx := context.Background()

	// 自匹配正例（候选=目标=登录）→ EXECUTE。
	d, score, err := clf.Evaluate(ctx, Features("登录", "登录"))
	if err != nil || d != DecisionExecute || score < ThresholdHigh {
		t.Fatalf("正例: decision=%v score=%v err=%v", d, score, err)
	}
	// 非黑名单负例（候选=提交订单, 目标=登录）→ FALLBACK（C < θ_low）。
	d, score, err = clf.Evaluate(ctx, Features("提交订单", "登录"))
	if err != nil || d != DecisionFallback || score >= ThresholdLow {
		t.Fatalf("负例: decision=%v score=%v err=%v", d, score, err)
	}
}

func TestOnnxScorerBadInput(t *testing.T) {
	lib := onnxLibPath(t)
	if lib == "" {
		t.Skip("未找到 libonnxruntime")
	}
	model, err := os.ReadFile(filepath.Join(testdataDir(t), "classifier.onnx"))
	if err != nil {
		t.Fatalf("读测试模型失败: %v", err)
	}
	sc, err := NewOnnxScorer(lib, model)
	if err != nil {
		t.Fatalf("NewOnnxScorer: %v", err)
	}
	defer sc.Close()
	if _, err := sc.Score(context.Background(), []float32{1, 2}); err == nil {
		t.Fatal("维度不匹配应报错")
	}
	if _, err := NewOnnxScorer(lib, nil); err == nil {
		t.Fatal("空模型字节应报错")
	}
}

func TestFeatures10AndHeuristicScore(t *testing.T) {
	ctx := NodeContext{
		Tag:              "button",
		Role:             "button",
		Width:            120,
		Height:           40,
		ContextText:      "account settings",
		Landmark:         "main",
		DistanceToAnchor: 100,
	}
	f10 := Features10("Edit Core Settings", "click:Edit Core Settings", ctx)
	if len(f10) != 10 {
		t.Fatalf("Features10 维度 %d != 10", len(f10))
	}
	if f10[0] < 0.8 || f10[1] < 0.7 || f10[2] != 0.0 || f10[3] != 1.0 {
		t.Fatalf("文本包含特征异常: %v", f10[:4])
	}
	if f10[5] != 1.0 {
		t.Fatalf("click 意图下 button role_compat 期望 1.0，实际 %f", f10[5])
	}
	if f10[6] <= 0 || f10[6] > 1.0 {
		t.Fatalf("visual_prominence 期望 (0, 1]，实际 %f", f10[6])
	}
	if f10[8] != 1.0 {
		t.Fatalf("main landmark 期望 1.0，实际 %f", f10[8])
	}
	if f10[9] <= 0.8 {
		t.Fatalf("distance 100 衰减 期望 > 0.8，实际 %f", f10[9])
	}

	score := HeuristicScore(f10)
	if score < 0.70 {
		t.Fatalf("高相关候选启发式打分期望 >= 0.70，实际 %f", score)
	}

	// 纯字面完全相等：
	fExact := Features10("Edit Core Settings", "Edit Core Settings", ctx)
	if fExact[2] != 1.0 || fExact[5] != 0.8 {
		t.Fatalf("通用意图下 exact=1.0, role=0.8，实际 exact=%f role=%f", fExact[2], fExact[5])
	}
	exactScore := HeuristicScore(fExact)
	if exactScore < 0.85 {
		t.Fatalf("完全相等高相关打分期望 >= 0.85，实际 %f", exactScore)
	}

	negCtx := NodeContext{
		Tag:              "aside",
		Role:             "",
		Width:            10,
		Height:           10,
		ContextText:      "advertisement",
		Landmark:         "other",
		DistanceToAnchor: 2000,
	}
	fNeg := Features10("无关推广", "Edit Core Settings", negCtx)
	negScore := HeuristicScore(fNeg)
	if negScore >= 0.50 {
		t.Fatalf("无关负样本启发式打分期望 < 0.50 (FALLBACK)，实际 %f", negScore)
	}
}
