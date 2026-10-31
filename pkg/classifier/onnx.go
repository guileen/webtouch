// 真实 ONNX 运行时适配器（A 案：ort-go CGO，USER_ACTION.md #1）。
//
// 决策核（Scorer 接口 + θ 阈值三分）在 classifier.go；本文件负责把
// classifier.onnx（LightGBM → TreeEnsembleClassifier，5 维特征）接进引擎进程内：
//   - NewOnnxScorer：加载模型字节 + 空特征 Sanity Check（OnnxReady 语义：
//     模型可加载且空特征推理正常，不再是「文件存在即 ready」）；
//   - Features：按 spec/algorithm-kernel.md §5.0.1 构造 5 维特征向量，
//     与 pipeline/exporter/train_onnx.py feature_vector 逐位对齐（跨语言对拍见 onnx_test.go）。
package classifier

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode/utf8"

	"uws/pkg/matcher"

	ort "github.com/yalue/onnxruntime_go"
)

// NFeatures 是 ONNX 分类器输入特征维度（spec/algorithm-kernel.md §5.0.18）。
const NFeatures = 10

var (
	ortEnvOnce sync.Once
	ortEnvErr  error
)

// OnnxScorer 是真实 ONNX 打分器：进程内 ort-go 会话，零 RPC、零常驻服务。
// Score 消费 5 维特征并返回 p(class 1) ∈ [0,1]。
type OnnxScorer struct {
	mu      sync.Mutex // 复用 tensor 的 Run 必须串行
	session *ort.AdvancedSession
	input   *ort.Tensor[float32]
	output  *ort.Tensor[float32]
}

// NewOnnxScorer 加载模型字节并执行空特征 Sanity Check。
// libPath 为 libonnxruntime 共享库路径（macOS 13 x86_64 用 1.23.x dylib，
// Linux 用官方 libonnxruntime.so.<ver>）；空串时交给 ort-go 默认查找。
func NewOnnxScorer(libPath string, modelBytes []byte) (*OnnxScorer, error) {
	if len(modelBytes) == 0 {
		return nil, errors.New("onnx: 空模型字节")
	}
	if libPath != "" {
		ort.SetSharedLibraryPath(libPath)
	}
	ortEnvOnce.Do(func() { ortEnvErr = ort.InitializeEnvironment() })
	if ortEnvErr != nil {
		return nil, fmt.Errorf("onnx: 初始化 ORT 环境: %w", ortEnvErr)
	}
	sc := &OnnxScorer{}
	if err := sc.load(modelBytes); err != nil {
		_ = sc.Close()
		return nil, err
	}
	// Sanity Check：空特征推理正常才认为模型就绪（OnnxReady 真实语义）。
	if _, err := sc.Score(context.Background(), make([]float32, NFeatures)); err != nil {
		_ = sc.Close()
		return nil, fmt.Errorf("onnx: 空特征 Sanity Check 失败: %w", err)
	}
	return sc, nil
}

func (s *OnnxScorer) load(modelBytes []byte) error {
	input, err := ort.NewTensor(ort.NewShape(1, NFeatures), make([]float32, NFeatures))
	if err != nil {
		return fmt.Errorf("onnx: 创建输入 tensor: %w", err)
	}
	s.input = input
	output, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 2))
	if err != nil {
		return fmt.Errorf("onnx: 创建输出 tensor: %w", err)
	}
	s.output = output
	session, err := ort.NewAdvancedSessionWithONNXData(modelBytes,
		[]string{"features"}, []string{"probabilities"},
		[]ort.Value{s.input}, []ort.Value{s.output}, nil)
	if err != nil {
		return fmt.Errorf("onnx: 创建会话: %w", err)
	}
	s.session = session
	return nil
}

// Score 运行 ONNX 推理并返回二分类 p(class 1)。
func (s *OnnxScorer) Score(_ context.Context, features []float32) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return 0, errors.New("onnx: scorer 已关闭")
	}
	data := s.input.GetData()
	if len(features) != len(data) {
		return 0, fmt.Errorf("onnx: 特征维度 %d != %d", len(features), len(data))
	}
	copy(data, features)
	if err := s.session.Run(); err != nil {
		return 0, fmt.Errorf("onnx: 推理失败: %w", err)
	}
	out := s.output.GetData()
	if len(out) < 2 {
		return 0, errors.New("onnx: 输出形状异常")
	}
	return float64(out[1]), nil
}

// Close 释放会话与 tensor；幂等（重复调用安全）。
func (s *OnnxScorer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if s.session != nil {
		if err := s.session.Destroy(); err != nil {
			errs = append(errs, err)
		}
		s.session = nil
	}
	if s.input != nil {
		if err := s.input.Destroy(); err != nil {
			errs = append(errs, err)
		}
		s.input = nil
	}
	if s.output != nil {
		if err := s.output.Destroy(); err != nil {
			errs = append(errs, err)
		}
		s.output = nil
	}
	return errors.Join(errs...)
}

// NodeContext 携带候选节点的结构、几何与上下文属性（spec/algorithm-kernel.md §5.0.18）。
type NodeContext struct {
	Tag              string
	Role             string
	Width            float64
	Height           float64
	ContextText      string
	Landmark         string
	DistanceToAnchor float64 // 负数表示无锚点参考（默认采用中性 0.5 距离先验）
}

// Features10 构造 10 维特征向量（spec/algorithm-kernel.md §5.0.18），
// 与 pipeline/exporter/train_onnx.py feature_vector 逐位对齐：
//
//	f0 simhash_sim            — 1 − 汉明距(SimHash64(candidate), SimHash64(target)) / 64
//	f1 jaccard                — 候选与目标 token 集合 Jaccard
//	f2 exact_match            — 归一化小写完全相等（0/1）
//	f3 substring              — 目标语义是候选子串或候选是目标子串（0/1）
//	f4 log_len                — log1p(候选 rune 数)/8，封顶 1.0
//	f5 role_compat            — 标签与角色（Tag/Role）与意图动作类型的契合度
//	f6 visual_prominence      — 视觉几何显著度（min(log10(1+area)/6.0, 1.0)）
//	f7 context_echo           — 上下文回响与别名包含度
//	f8 landmark_zone          — 所属 AX 地标区域先验偏置（main=1.0, nav=0.6, header/aside=0.4, other=0.2）
//	f9 spatial_distance_decay — 空间相对距离衰减（1.0 / (1.0 + d/1000.0)）
func Features10(candidate, target string, ctx NodeContext) []float32 {
	c := strings.ToLower(strings.TrimSpace(candidate))
	t := strings.ToLower(strings.TrimSpace(target))

	var sim float64
	switch {
	case c == "" && t == "":
		sim = 1
	case c == "" || t == "":
		sim = 0
	default:
		sim = 1 - float64(matcher.HammingDistance(matcher.SimHash64(c), matcher.SimHash64(t)))/64
	}

	ct, tt := tokenSet(c), tokenSet(t)
	union := len(ct)
	inter := 0
	for k := range tt {
		if _, ok := ct[k]; ok {
			inter++
		} else {
			union++
		}
	}
	var jacc float64
	if union > 0 {
		jacc = float64(inter) / float64(union)
	}

	exact := 0.0
	if c == t {
		exact = 1
	}
	substr := 0.0
	if (t != "" && strings.Contains(c, t)) || (c != "" && strings.Contains(t, c)) {
		substr = 1
	}
	logLen := math.Min(math.Log1p(float64(utf8.RuneCountInString(c)))/8, 1)

	// f5 role_compat
	tagL := strings.ToLower(strings.TrimSpace(ctx.Tag))
	roleL := strings.ToLower(strings.TrimSpace(ctx.Role))
	var roleCompat float64
	if strings.Contains(t, "input") || strings.Contains(t, "type") || strings.Contains(t, "fill") ||
		strings.Contains(t, "text") || strings.Contains(t, "search_box") {
		if tagL == "input" || tagL == "textarea" || roleL == "textbox" || roleL == "searchbox" {
			roleCompat = 1.0
		} else {
			roleCompat = 0.0
		}
	} else if strings.Contains(t, "click") || strings.Contains(t, "select") || strings.Contains(t, "button") ||
		strings.Contains(t, "link") || strings.Contains(t, "tab") || tagL == "" {
		if tagL == "button" || tagL == "a" || roleL == "button" || roleL == "link" || roleL == "tab" || roleL == "menuitem" {
			roleCompat = 1.0
		} else if tagL == "input" {
			roleCompat = 0.4
		} else if tagL == "div" || tagL == "span" || tagL == "p" || tagL == "li" || strings.HasPrefix(tagL, "h") {
			roleCompat = 0.2
		} else if tagL == "" && roleL == "" {
			roleCompat = 0.5
		} else {
			roleCompat = 0.0
		}
	} else {
		if tagL == "button" || tagL == "a" || tagL == "input" || tagL == "select" || tagL == "textarea" ||
			roleL == "button" || roleL == "link" {
			roleCompat = 0.8
		} else {
			roleCompat = 0.3
		}
	}

	// f6 visual_prominence
	var visualProminence float64
	if ctx.Width <= 0 || ctx.Height <= 0 {
		if ctx.Width == 0 && ctx.Height == 0 {
			visualProminence = 0.5 // 中性默认值
		} else {
			visualProminence = 0.0
		}
	} else {
		area := ctx.Width * ctx.Height
		visualProminence = math.Min(math.Log10(1.0+area)/6.0, 1.0)
	}

	// f7 context_echo
	ctxText := strings.ToLower(strings.TrimSpace(ctx.ContextText))
	var contextEcho float64
	if ctxText != "" && (strings.Contains(ctxText, t) || strings.Contains(t, ctxText)) {
		contextEcho = 1.0
	} else if ctxText != "" {
		cTokens := tokenSet(ctxText)
		hasInter := false
		for tok := range tt {
			if _, ok := cTokens[tok]; ok {
				hasInter = true
				break
			}
		}
		if hasInter {
			contextEcho = 0.5
		}
	} else if ctxText == "" && (substr > 0 || exact > 0) {
		contextEcho = 1.0
	}

	// f8 landmark_zone
	lmL := strings.ToLower(strings.TrimSpace(ctx.Landmark))
	var landmarkZone float64
	switch lmL {
	case "main", "article":
		landmarkZone = 1.0
	case "nav":
		landmarkZone = 0.6
	case "header", "aside":
		landmarkZone = 0.4
	default:
		landmarkZone = 0.2
	}

	// f9 spatial_distance_decay
	var spatialDecay float64
	if ctx.DistanceToAnchor >= 0.0 {
		spatialDecay = 1.0 / (1.0 + ctx.DistanceToAnchor/1000.0)
	} else {
		spatialDecay = 0.5
	}

	return []float32{
		float32(sim),
		float32(jacc),
		float32(exact),
		float32(substr),
		float32(logLen),
		float32(roleCompat),
		float32(visualProminence),
		float32(contextEcho),
		float32(landmarkZone),
		float32(spatialDecay),
	}
}

// Features 构造特征向量；为保持向后兼容，自动补充中性默认 NodeContext，返回 10 维特征。
func Features(candidate, target string) []float32 {
	return Features10(candidate, target, NodeContext{DistanceToAnchor: -1.0, Landmark: "main"})
}

// HeuristicScore 根据 §5.0.18 条款 2 提供确定性启发式加权打分（用于无真实 ONNX 模型时的确定性降级）。
func HeuristicScore(features []float32) float64 {
	if len(features) < NFeatures {
		return 0.0
	}
	// 权重向量：强调字面匹配、子串包含、语义重叠与角色地标
	weights := [10]float64{
		0.20, // f0 simhash_sim
		0.20, // f1 jaccard
		0.25, // f2 exact_match
		0.15, // f3 substring
		0.00, // f4 log_len
		0.10, // f5 role_compat
		0.05, // f6 visual_prominence
		0.15, // f7 context_echo
		0.05, // f8 landmark_zone
		0.05, // f9 spatial_distance_decay
	}
	score := 0.0
	for i := 0; i < 10; i++ {
		score += float64(features[i]) * weights[i]
	}
	if score > 1.0 {
		score = 1.0
	} else if score < 0.0 {
		score = 0.0
	}
	return score
}

// tokenSet 返回 token 集合（与 Python _tokens 同构）。
func tokenSet(text string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, tok := range matcher.Tokens(text) {
		out[tok] = struct{}{}
	}
	return out
}
