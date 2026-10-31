// R1 知识库热更→匹配管线接线实现层测试（tdd-checklist.md §3.4 / USER_ACTION.md 审查 R1）：
//   - 热更后 ExecuteAction 命中新版本规则（register_now 置信度从 classifier 0.90
//     变为规则 0.80，banner 黑名单从放行变 NOT_FOUND）；
//   - P0 自动回滚后 matcher 同步回滚版本（"登录" 从大面积失明恢复执行）。
//
// 契约层（tests/integration/test_knowledge.py::test_r1_*）为真实 HTTP fetcher 双层验证。
package engine

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"uws/pkg/knowledge"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// kbArtifactFull 构造携带完整规则快照的 artifact（R1 重建 matcher 的输入）。
func kbArtifactFull(version, minEngine string, rules []knowledge.RuleSnapshot,
	syns []knowledge.SynonymSnapshot, kws []string) *knowledge.Artifact {
	statusMap := make(map[string]string, len(rules))
	for _, r := range rules {
		key := r.RuleType + ":" + r.SemanticLabel
		if r.SemanticLabel == "" {
			key = r.RuleType + ":" + r.TextPattern
		}
		statusMap[key] = strings.ToUpper(r.Status)
	}
	return &knowledge.Artifact{
		Version:           version,
		SchemaHash:        "sha256:schema-" + version,
		ModelHash:         "sha256:model-" + version,
		ArtifactURI:       "s3://uws-artifacts/" + version,
		MinEngineVersion:  minEngine,
		KeyRuleIndexCount: len(rules),
		OnnxReady:         true,
		MemoryEstimateMB:  100,
		Rules:             statusMap,
		RulesFull:         rules,
		Synonyms:          syns,
		NegativeKeywords:  kws,
	}
}

func assertConfidence(t *testing.T, got float32, want float64) {
	t.Helper()
	if math.Abs(float64(got)-want) > 1e-6 {
		t.Fatalf("confidence = %v, want %v", got, want)
	}
}

// ── R1：热更改变匹配行为（规则 + 黑名单双层验证） ─────────────────────

func TestR1HotReloadChangesMatchingGRPC(t *testing.T) {
	rulesV139 := []knowledge.RuleSnapshot{
		{RuleType: "text_pattern", TextPattern: "登录", SemanticLabel: "登录", Action: "click", Confidence: 0.95, Status: "active"},
	}
	rulesV140 := []knowledge.RuleSnapshot{
		{RuleType: "text_pattern", TextPattern: "登录", SemanticLabel: "登录", Action: "click", Confidence: 0.95, Status: "active"},
		{RuleType: "text_pattern", TextPattern: "register_now", SemanticLabel: "register_now", Action: "click", Confidence: 0.8, Status: "ACTIVE"},
	}
	artifacts := map[string]*knowledge.Artifact{
		"v1.3.9": kbArtifactFull("v1.3.9", "v1.3.0", rulesV139, nil, nil),
		"v1.4.0": kbArtifactFull("v1.4.0", "v1.4.0", rulesV140, nil, []string{"banner"}),
	}
	env := kbEnv(t, artifacts, "v1.3.9")
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")

	// 热更前：register_now 无规则 → classifier mock 0.90 执行。
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "r1-pre-reg", TargetSemantic: "register_now", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("热更前 register_now 应执行: err=%v resp=%+v", err, resp)
	}
	assertConfidence(t, resp.Confidence, 0.90)

	// 热更前：banner_login 无黑名单 → classifier 0.90 执行。
	resp, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "r1-pre-ban", TargetSemantic: "banner_login", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("热更前 banner_login 应执行: err=%v resp=%+v", err, resp)
	}
	assertConfidence(t, resp.Confidence, 0.90)

	// 热更 v1.4.0（新增 register_now 规则 + banner 黑名单）。
	rl, err := env.client.ReloadKnowledge(mdCtx("operator-key"), &uwspb.ReloadRequest{Version: "v1.4.0"})
	if err != nil || !rl.Ok {
		t.Fatalf("热更 v1.4.0 失败: err=%v resp=%+v", err, rl)
	}

	// 热更后：register_now 命中规则 0.8（A* 单步）执行，不再是 classifier 0.90。
	resp, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "r1-post-reg", TargetSemantic: "register_now", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("热更后 register_now 应执行: err=%v resp=%+v", err, resp)
	}
	assertConfidence(t, resp.Confidence, 0.80)

	// 热更后：banner_login 命中黑名单 → NOT_FOUND（matcher 已消费新 artifact）。
	_, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "r1-post-ban", TargetSemantic: "banner_login", ExpectedAction: "click",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("热更后 banner_login 应 NOT_FOUND, got %v", err)
	}

	// 回滚 v1.3.9 → 行为还原（register_now 回 classifier、banner_login 放行）。
	rl, err = env.client.ReloadKnowledge(mdCtx("operator-key"), &uwspb.ReloadRequest{Version: "v1.3.9"})
	if err != nil || !rl.Ok {
		t.Fatalf("回滚 v1.3.9 失败: err=%v resp=%+v", err, rl)
	}
	resp, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "r1-roll-reg", TargetSemantic: "register_now", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("回滚后 register_now 应执行: err=%v resp=%+v", err, resp)
	}
	assertConfidence(t, resp.Confidence, 0.90)
	resp, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "r1-roll-ban", TargetSemantic: "banner_login", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("回滚后 banner_login 应执行: err=%v resp=%+v", err, resp)
	}
}

// ── R1：P0 自动回滚后 matcher 同步回滚版本（恢复「大面积失明」） ─────────

func TestR1AutoRollbackRestoresMatchingGRPC(t *testing.T) {
	rulesV139 := []knowledge.RuleSnapshot{
		{RuleType: "text_pattern", TextPattern: "登录", SemanticLabel: "登录", Action: "click", Confidence: 0.95, Status: "active"},
	}
	rulesV140 := []knowledge.RuleSnapshot{
		{RuleType: "text_pattern", TextPattern: "register_now", SemanticLabel: "register_now", Action: "click", Confidence: 0.8, Status: "active"},
	}
	artifacts := map[string]*knowledge.Artifact{
		"v1.3.9": kbArtifactFull("v1.3.9", "v1.3.0", rulesV139, nil, nil),
		// 坏版本：黑名单封杀「登录」→ 所有登录请求大面积失明。
		"v1.4.0": kbArtifactFull("v1.4.0", "v1.4.0", rulesV140, nil, []string{"登录"}),
	}
	env := kbEnv(t, artifacts, "v1.3.9")
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")

	// 基线：v1.3.9 登录可执行。
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "r1a-base", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("基线登录应执行: err=%v resp=%+v", err, resp)
	}

	// 热更坏版本 v1.4.0。
	rl, err := env.client.ReloadKnowledge(mdCtx("operator-key"), &uwspb.ReloadRequest{Version: "v1.4.0"})
	if err != nil || !rl.Ok {
		t.Fatalf("热更坏版本失败: err=%v resp=%+v", err, rl)
	}

	// 连续 50 次 NOT_FOUND（黑名单拦截）→ P0 自动回滚。
	for i := 0; i < 50; i++ {
		_, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
			IntentId: fmt.Sprintf("r1a-blind-%d", i), TargetSemantic: "登录", ExpectedAction: "click",
		})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("第 %d 次登录应 NOT_FOUND, got %v", i, err)
		}
	}

	kv := kbVersion(t, env)
	if kv.Version != "v1.3.9" {
		t.Fatalf("应自动回滚到 v1.3.9, got %s", kv.Version)
	}
	hc, err := env.client.HealthCheck(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if !hc.GetRolledBack() {
		t.Fatal("HealthCheck 应附带回滚标记")
	}

	// R1 关键断言：回滚后 matcher 同步 → 登录恢复执行（不再是「永久失明」）。
	resp, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "r1a-recover", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("回滚后登录应恢复执行: err=%v resp=%+v", err, resp)
	}
	assertConfidence(t, resp.Confidence, 0.95)
}
