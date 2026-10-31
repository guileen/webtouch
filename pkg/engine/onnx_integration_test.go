// 真实 ONNX（A 案 ort-go CGO）引擎集成测试：
//   - 页面候选 Confidence=0 节点经 ONNX 打分进入候选池（候选文本 × 目标语义）；
//   - 正例（候选=目标=登录）C≥0.85 → 执行；非黑名单负例（提交订单 × 登录）
//     C<0.50 → NOT_FOUND（决策带/ L3 结构校验）。
//
// ORT 共享库缺失时自动 skip（CI/无库环境不阻塞 pre-commit）。
package engine

import (
	"os"
	"path/filepath"
	"testing"

	"uws/pkg/browser"
	"uws/pkg/knowledge"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func onnxIntegrationLib(t *testing.T) string {
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

func onnxIntegrationModel(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "classifier", "testdata", "classifier.onnx"))
	if err != nil {
		t.Fatalf("读测试模型失败: %v", err)
	}
	return data
}

// artifactWithModel 基于内置 artifact 构造携带真实 ONNX 模型的知识库产物
// （RulesFull/同义词/黑名单与零配置内置一致，仅换 model 通道）。
func artifactWithModel(model []byte) *knowledge.Artifact {
	art := DefaultArtifact()
	art.ModelHash = "sha256:test-real-onnx"
	art.OnnxReady = len(model) > 0
	art.OnnxModel = model
	return art
}

func TestOnnxScorerPositiveCandidateExecutesGRPC(t *testing.T) {
	lib := onnxIntegrationLib(t)
	if lib == "" {
		t.Skip("未找到 libonnxruntime（设置 ONNXRUNTIME_SHARED_LIBRARY_PATH 或先 make ort-lib）")
	}
	art := artifactWithModel(onnxIntegrationModel(t))
	env := newTestEnvWithOptions(t, Options{
		Keys:           testKeys(),
		Knowledge:      knowledge.NewStore(&memoryFetcher{art: art}, art),
		OnnxRuntimeLib: lib,
	})
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	// 页面候选 Confidence=0（真实 DOM/WASM 采集语义）：ONNX 打分 0.999 → 执行。
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录"}})
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "onnx-pos", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("真实 ONNX 正例应执行: err=%v resp=%+v", err, resp)
	}
	if resp.Confidence < 0.85 {
		t.Fatalf("真实 ONNX 正例置信度 %v < θ_high 0.85", resp.Confidence)
	}
}

func TestOnnxScorerNegativeCandidateNotFoundGRPC(t *testing.T) {
	lib := onnxIntegrationLib(t)
	if lib == "" {
		t.Skip("未找到 libonnxruntime")
	}
	art := artifactWithModel(onnxIntegrationModel(t))
	env := newTestEnvWithOptions(t, Options{
		Keys:           testKeys(),
		Knowledge:      knowledge.NewStore(&memoryFetcher{art: art}, art),
		OnnxRuntimeLib: lib,
	})
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	// 非黑名单负例（提交订单 × 登录，种子模型 0.0008）：决策带 C<θ_low
	// 或 L3 结构剔除 → NOT_FOUND（不得误执行）。
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "提交订单"}})
	_, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "onnx-neg", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("真实 ONNX 负例应 NOT_FOUND, got %v", err)
	}
}

func TestOnnxScorerDowngradesWithoutLib(t *testing.T) {
	// 未配置 ORT 库时引擎零配置启动，决策核保持 mock（既有行为不回归）。
	art := artifactWithModel(onnxIntegrationModel(t))
	env := newTestEnvWithOptions(t, Options{
		Keys:      testKeys(),
		Knowledge: knowledge.NewStore(&memoryFetcher{art: art}, art),
	})
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录"}})
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "downgrade", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("无 ORT 库应保持 mock 决策核执行: err=%v resp=%+v", err, resp)
	}
	// mock 语义 seam：无候选置信度时走 classifier 兜底 0.90。
	if resp.Confidence < 0.85 {
		t.Fatalf("mock 兜底置信度 %v < 0.85", resp.Confidence)
	}
}

// TestOnnxScorerPhase2_10DFeaturesAndDecisionPoolGRPC 验证 Phase 2（§5.0.18）：
// 页面候选携带 10 维属性（Tag/Role/Geometry/Landmark/Context）经真实 ONNX 打分入池，
// 且高置信度候选弹性通过 L3 校验执行成功。
func TestOnnxScorerPhase2_10DFeaturesAndDecisionPoolGRPC(t *testing.T) {
	lib := onnxIntegrationLib(t)
	if lib == "" {
		t.Skip("未找到 libonnxruntime")
	}
	art := artifactWithModel(onnxIntegrationModel(t))
	env := newTestEnvWithOptions(t, Options{
		Keys:           testKeys(),
		Knowledge:      knowledge.NewStore(&memoryFetcher{art: art}, art),
		OnnxRuntimeLib: lib,
	})
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	// 构造 10 维多模态候选：带有 Tag=button, Role=button, Landmark=main
	sess.SetPageCandidates([]browser.PageCandidate{
		{
			Text:     "立即登录",
			Tag:      "button",
			Role:     "button",
			W:        120,
			H:        40,
			Landmark: "main",
			Context:  "user account login",
		},
	})
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId:       "phase2-10d",
		TargetSemantic: "登录",
		ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("Phase 2 10 维候选经 ONNX 打分与弹性 L3 准入应执行成功: err=%v resp=%+v", err, resp)
	}
	if resp.Confidence < 0.85 {
		t.Fatalf("Phase 2 候选置信度 %v 应 >= 0.85 (EXECUTE)", resp.Confidence)
	}
}
