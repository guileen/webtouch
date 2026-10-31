package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"time"
	"uws/pkg/classifier"
)

// episodeShadowScorer 是**只读影子通道**打分器的最小接口（§5.0.14 条款 3①）。
//
// 与决策 scorer（`Server.onnxScorer`，`*classifier.OnnxScorer`）在**类型上**就是两条线：
// 影子只读候选集、结果只落 `shadow_inference`，不参与 `decision`、不进 `pageCandidatePool`。
type episodeShadowScorer interface {
	Score(ctx context.Context, features []float32) (float64, error)
}

// newShadowOnnxScorer 是影子 scorer 的**构造注入点**（默认 = classifier.NewOnnxScorer）。
// 抽成包级变量只为让单测在无本机 libonnxruntime 的情况下仍能断言正交性
// （§5.0.14 条款 3① 的硬断言：构造影子 scorer 后 `s.onnxScorer` 仍为 nil）。
var newShadowOnnxScorer = func(libPath string, modelBytes []byte) (episodeShadowScorer, error) {
	return classifier.NewOnnxScorer(libPath, modelBytes)
}

// syncShadowScorer 构造**独立**影子打分器（§5.0.14 条款 3①，取代 §5.0.13 条款 1
// 的「影子一律不接线」）：独立配置入口（`--shadow-onnx-model` ＋ 复用 `--onnx-runtime-lib`），
// 构造路径**不经过** `syncScorerWithKnowledge`、**不写** `s.onnxScorer`、
// **不进** `pageCandidatePool`。任一前置缺失（无 lib / 无模型路径 / 模型不可读 /
// 构造失败）只记日志并保持 `shadow_inference = null`（条款 3③ 降级如实）。
func (s *Server) syncShadowScorer() {
	if s == nil || s.onnxLib == "" || s.shadowModelPath == "" {
		return
	}
	model, err := os.ReadFile(s.shadowModelPath)
	if err != nil {
		log.Printf("警告: 影子 ONNX 模型不可读（%v）；shadow_inference 保持 null", err)
		return
	}
	sc, err := newShadowOnnxScorer(s.onnxLib, model)
	if err != nil {
		log.Printf("警告: 影子 ONNX 打分器构建失败（%v）；shadow_inference 保持 null", err)
		return
	}
	version := s.shadowVersionSeed
	if version == "" {
		version = filepath.Base(s.shadowModelPath)
	}
	sum := sha256.Sum256(model)
	hash := hex.EncodeToString(sum[:])[:12]
	s.scorerMu.Lock()
	s.shadowScorer = sc
	s.shadowModelVersion = version
	s.shadowModelHash = hash
	s.scorerMu.Unlock()
	log.Printf("影子通道已启用（只读旁路，不参与决策）: model_version=%s model_hash=%s", version, hash)
}

// shadowInference 在**同一候选集**上做只读影子打分（§5.0.14 条款 3 / 条款 1①）。
// 返回 nil 表示影子通道未接线（无 lib / 无模型 / 构造失败）——此时字段允许为 null。
// 只读：分数只落盘、不参与 decision，也不写 learned（`T-3` / G-PROOF 门只约束
// 「影子进决策带」与在线 learned 写入，只读旁路不受此门限制）。
func (s *Server) shadowInference(cands []EpisodeCandidate, targetSemantic string) *EpisodeShadow {
	if s == nil || len(cands) == 0 {
		return nil
	}
	s.scorerMu.RLock()
	sc := s.shadowScorer
	version := s.shadowModelVersion
	hash := s.shadowModelHash
	s.scorerMu.RUnlock()
	if sc == nil {
		return nil
	}
	shadow := &EpisodeShadow{
		Scores:       make([]float64, 0, len(cands)),
		Top1:         -1,
		ModelVersion: version,
		ModelHash:    hash,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	top := 0.0
	for i, c := range cands {
		nodeCtx := classifier.NodeContext{
			Tag:              c.Tag,
			Role:             c.Role,
			Width:            c.W,
			Height:           c.H,
			DistanceToAnchor: -1.0,
		}
		score, err := sc.Score(ctx, classifier.Features10(c.Text, targetSemantic, nodeCtx))
		if err != nil {
			log.Printf("episode: 影子打分失败（本次影子整体丢弃）: %v", err)
			return nil
		}
		shadow.Scores = append(shadow.Scores, score)
		if i == 0 || score > top {
			top, shadow.Top1 = score, i
		}
	}
	return shadow
}
