package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"uws/pkg/browser"
	"uws/pkg/knowledge"
	"uws/pkg/matcher"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ── ExecuteActionStream（TDD-API-03：请求/响应一一对应、顺序一致） ──

func (s *Server) ExecuteActionStream(stream uwspb.UwsEngine_ExecuteActionStreamServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if verr := validateIntent(req); verr != nil {
			return verr
		}
		resp, err := s.execute(stream.Context(), req)
		if err != nil {
			// 流中断（如未初始化 → FAILED_PRECONDITION），客户端重建会话。
			return err
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// ── ReloadKnowledge（TDD-KB-01..05 接线） ──

func (s *Server) ReloadKnowledge(ctx context.Context, req *uwspb.ReloadRequest) (*uwspb.ReloadResponse, error) {
	resp := s.kb.Reload(ctx, knowledge.ReloadRequest{
		Version:       req.GetVersion(),
		ArtifactURI:   req.GetArtifactUri(),
		DryRun:        req.GetDryRun(),
		Force:         req.GetForce(),
		EngineVersion: EngineVersion,
	})
	if resp.OK && !req.GetDryRun() {
		// R1：切换成功后 matcher 必须同步到新版本，否则热更不改变匹配行为。
		if err := s.syncMatcherWithKnowledge(); err != nil {
			return nil, status.Errorf(codes.Internal,
				"knowledge switched to %s but matcher sync failed: %v", resp.ActiveVersion, err)
		}
		// A 案：热更后 scorer 同步到新 artifact 的模型字节。
		s.syncScorerWithKnowledge()
	}
	return &uwspb.ReloadResponse{
		Ok:              resp.OK,
		ActiveVersion:   resp.ActiveVersion,
		PreviousVersion: resp.PreviousVersion,
		SwitchTimeMs:    resp.SwitchTimeMs,
		Reason:          resp.Reason,
	}, nil
}

// ── HealthCheck / GetKnowledgeVersion（TDD-API-06 一致性） ──

func (s *Server) HealthCheck(_ context.Context, _ *uwspb.Empty) (*uwspb.HealthStatus, error) {
	active, max := s.pool.Stats()
	version, _, _ := s.activeKnowledge()
	hc := &uwspb.HealthStatus{
		Ok:            version != "",
		Version:       version,
		ActiveTargets: int32(active),
		MaxTargets:    int32(max),
	}
	// 自动回滚标记（proto v1.5 新增；knowledge-versioning §5.2）。
	if rb := s.kb.LastAutoRollback(); rb != nil {
		hc.RolledBack = proto.Bool(true)
		hc.RollbackReason = proto.String(fmt.Sprintf("auto_rollback %s → %s", rb.From, rb.To))
	}
	return hc, nil
}

func (s *Server) GetKnowledgeVersion(_ context.Context, _ *uwspb.Empty) (*uwspb.KnowledgeVersionResponse, error) {
	v, sh, mh := s.activeKnowledge()
	return &uwspb.KnowledgeVersionResponse{Version: v, SchemaHash: sh, ModelHash: mh}, nil
}

func (s *Server) activeKnowledge() (version, schemaHash, modelHash string) {
	h, v := s.kb.AcquireHandle()
	if h == nil {
		return "", "", ""
	}
	return v, h.SchemaHash, h.ModelHash
}

// mockScorer 是决策核的确定性 mock 评分器（真实 ONNX 适配待接线）。
type mockScorer struct{}

func (mockScorer) Score(_ context.Context, features []float32) (float64, error) {
	if len(features) > 0 && features[0] >= 1 {
		return 0.30, nil // 困难负样本信号 → FALLBACK
	}
	return 0.90, nil // 默认可信 → EXECUTE
}

func featuresFor(semantic string) []float32 {
	switch strings.ToLower(strings.TrimSpace(semantic)) {
	case "missing", "unknown", "not_found":
		return []float32{1}
	}
	if isHardNegativeSemantic(semantic) {
		return []float32{1}
	}
	return []float32{0}
}

// isHardNegativeSemantic 识别 mock 困难负样本语义集合（黄金集 TDD-ONNX-03 使用）。
// 真实 ONNX 模型导出后由模型打分替代，此集合仅服务决策核集成验证。
func isHardNegativeSemantic(semantic string) bool {
	s := strings.ToLower(strings.TrimSpace(semantic))
	for _, marker := range []string{
		"hard_negative", "spam", "fake", "hidden", "tracking", "bait", "deco", "ghost", "noise",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// WSTNShadowStats 返回 WSTN 消费端 Shadow 旁路审计的统计指标（触发总数与校验通过数）。
func (s *Server) WSTNShadowStats() (total, valid uint64) {
	return s.wstnShadowTotal.Load(), s.wstnShadowValid.Load()
}

// wstnShadowAudit 执行 WSTN 契约消费端 Dry-run 旁路审计。
// 铁律：在页面状态 ROC 汉明距标定与 Tier 2 因果录制完成前，生产执行流 100% 保持既有 A* 逻辑，
// 本旁路仅用于结构契约校验、路由预测比对与审计日志记录。
func (s *Server) wstnShadowAudit(ctx context.Context, bc *browser.BrowserContext, req *uwspb.ActionIntentRequest, cands []matcher.Candidate, productionSelected string) {
	s.wstnShadowTotal.Add(1)

	currentStateID := "state_current"
	if bc != nil && bc.ID != "" {
		currentStateID = "ctx_" + bc.ID
	}
	targetIntent := req.GetTargetSemantic()
	if targetIntent == "" {
		targetIntent = req.GetExpectedAction()
	}
	if targetIntent == "" {
		targetIntent = "default_action"
	}
	targetStateID := "target_" + targetIntent
	if req.GetExpectedAction() != "" && req.GetTargetSemantic() != "" {
		targetStateID = req.GetExpectedAction() + "_" + targetIntent
	}

	wstnReq := matcher.NewWSTNRouteRequest(currentStateID, targetStateID, targetIntent, nil)
	if err := wstnReq.Validate(); err != nil {
		log.Printf("[WSTN-SHADOW] ⚠️ 请求契约校验未通过: %v", err)
		return
	}

	s.wstnShadowValid.Add(1)
	log.Printf("[WSTN-SHADOW] 旁路审计: 当前状态=%s 目标状态=%s 意图=%q (生产选中=%q, 候选数=%d)",
		currentStateID, targetStateID, targetIntent, productionSelected, len(cands))
}
