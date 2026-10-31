// M5 引擎接线实现层测试（tdd-checklist.md §3.3 / §3.4）：
//   - TDD-ENG-06/07：verifier 状态机接入引擎 ExecuteAction（经真实 gRPC bufconn）；
//   - TDD-KB-01..05/07：知识库热更/回滚 gRPC 全链路（注入多版本 fetcher）。
//
// 契约层（tests/integration/）为双层验证；本文件为验收实现层。
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"uws/pkg/knowledge"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── 共享测试工具 ──────────────────────────────────────────────────────

// simSessionOf 返回当前测试客户端的模拟 CDP 会话（测试可脚本化导航事件）。
func simSessionOf(t *testing.T, srv *Server) *SimCDPSession {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, bc := range srv.contexts {
		if s, ok := bc.Session().(*SimCDPSession); ok {
			return s
		}
	}
	t.Fatal("no sim session found")
	return nil
}

// kbFetcher 是多版本内存 fetcher（测试注入，模拟对象存储产物）。
type kbFetcher struct {
	mu        sync.Mutex
	artifacts map[string]*knowledge.Artifact
	errs      map[string]error
}

func (f *kbFetcher) Fetch(_ context.Context, version, _ string) (*knowledge.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[version]; err != nil {
		return nil, err
	}
	art, ok := f.artifacts[version]
	if !ok {
		return nil, errors.New("artifact not found: " + version)
	}
	cp := *art
	cp.Rules = make(map[string]string, len(art.Rules))
	for k, v := range art.Rules {
		cp.Rules[k] = v
	}
	cp.RulesFull = append([]knowledge.RuleSnapshot(nil), art.RulesFull...)
	cp.Synonyms = append([]knowledge.SynonymSnapshot(nil), art.Synonyms...)
	cp.NegativeKeywords = append([]string(nil), art.NegativeKeywords...)
	return &cp, nil
}

func kbArtifact(version, minEngine string, keyRules int, onnxReady bool, memMB int64) *knowledge.Artifact {
	return &knowledge.Artifact{
		Version:           version,
		SchemaHash:        "sha256:schema-" + version,
		ModelHash:         "sha256:model-" + version,
		ArtifactURI:       "s3://uws-artifacts/" + version,
		MinEngineVersion:  minEngine,
		KeyRuleIndexCount: keyRules,
		OnnxReady:         onnxReady,
		MemoryEstimateMB:  memMB,
		Rules: map[string]string{
			"l1:login": "ACTIVE",
		},
	}
}

// kbEnv 构建注入多版本知识库的引擎测试环境。
func kbEnv(t *testing.T, artifacts map[string]*knowledge.Artifact, initial string) *testEnv {
	t.Helper()
	f := &kbFetcher{artifacts: artifacts, errs: map[string]error{}}
	store := knowledge.NewStore(f, artifacts[initial])
	return newTestEnvWithOptions(t, Options{Keys: testKeys(), Knowledge: store})
}

func kbArtifacts() map[string]*knowledge.Artifact {
	return map[string]*knowledge.Artifact{
		"v1.3.9": kbArtifact("v1.3.9", "v1.3.0", 10, true, 100),
		"v1.4.0": kbArtifact("v1.4.0", "v1.4.0", 12, true, 100),
	}
}

func kbVersion(t *testing.T, env *testEnv) *uwspb.KnowledgeVersionResponse {
	t.Helper()
	kv, err := env.client.GetKnowledgeVersion(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("GetKnowledgeVersion: %v", err)
	}
	return kv
}

// ── TDD-ENG-06：pushState 捕获 → 立即 NAVIGATING，绝不立即判完成 ────────

func TestEng06SpaPushStateNavigatingGRPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	sess.SetNavEvents(true, 0, false) // 动作触发 SPA 软路由（pushState）
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "eng06-1", TargetSemantic: "link", ExpectedAction: "click",
	})
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !resp.Success {
		t.Fatalf("动作应执行成功: %+v", resp)
	}
	if resp.ContextState != uwspb.ContextState_NAVIGATING {
		t.Fatalf("pushState 后状态 = %v, want NAVIGATING", resp.ContextState)
	}
	if resp.ContextState == uwspb.ContextState_NAVIGATED {
		t.Fatal("pushState 触发时不得立即判定 NAVIGATED")
	}
}

// ── TDD-ENG-07：Network Idle 500ms → NAVIGATED；忙碌连接 → DEADLINE_EXCEEDED ──

func TestEng07NetworkIdleStableTransitionGRPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	sess.SetNavEvents(true, 0, false)
	if resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "eng07-1", TargetSemantic: "link", ExpectedAction: "click",
	}); err != nil || resp.ContextState != uwspb.ContextState_NAVIGATING {
		t.Fatalf("点击后应 NAVIGATING: err=%v resp=%+v", err, resp)
	}

	sess.SetNavEvents(false, 0, false) // 活动连接 <2，保持静息
	start := time.Now()
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "eng07-2", TargetSemantic: "wait", ExpectedAction: "wait", TimeoutMs: 1200,
	})
	if err != nil {
		t.Fatalf("等待动作: %v", err)
	}
	if !resp.Success {
		t.Fatalf("等待动作应成功: %+v", resp)
	}
	if resp.ContextState != uwspb.ContextState_NAVIGATED {
		t.Fatalf("静息 500ms 后状态 = %v, want NAVIGATED", resp.ContextState)
	}
	if elapsed := time.Since(start); elapsed < 450*time.Millisecond {
		t.Errorf("静息窗口未真正等待: elapsed=%v", elapsed)
	}
}

func TestEng07BusyConnectionsDeadlineExceededGRPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	sess.SetNavEvents(true, 3, false)
	if resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "eng07b-1", TargetSemantic: "link", ExpectedAction: "click",
	}); err != nil || resp.ContextState != uwspb.ContextState_NAVIGATING {
		t.Fatalf("点击后应 NAVIGATING: err=%v resp=%+v", err, resp)
	}

	sess.SetNavEvents(false, 3, false) // 3 个未完结连接，永不静息
	_, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "eng07b-2", TargetSemantic: "wait", ExpectedAction: "wait", TimeoutMs: 200,
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("忙碌连接超时应 DEADLINE_EXCEEDED, got %v", err)
	}
}

func TestEng07DomMutationResetsIdleWindowGRPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	sess.SetNavEvents(true, 0, false)
	if resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "eng07c-1", TargetSemantic: "link", ExpectedAction: "click",
	}); err != nil || resp.ContextState != uwspb.ContextState_NAVIGATING {
		t.Fatalf("点击后应 NAVIGATING: err=%v resp=%+v", err, resp)
	}

	sess.SetNavEvents(false, 0, true) // 导航后发生 DOM 突变（防抖 + 重新静息）
	start := time.Now()
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "eng07c-2", TargetSemantic: "wait", ExpectedAction: "wait", TimeoutMs: 1200,
	})
	if err != nil {
		t.Fatalf("等待动作: %v", err)
	}
	if resp.ContextState != uwspb.ContextState_NAVIGATED {
		t.Fatalf("DOM 静息后状态 = %v, want NAVIGATED", resp.ContextState)
	}
	if elapsed := time.Since(start); elapsed < 450*time.Millisecond {
		t.Errorf("DOM 突变后静息窗口未等待: elapsed=%v", elapsed)
	}
}

// ── TDD-KB-01：dry_run 仅下载校验，不切换 ───────────────────────────────

func TestKb01DryRunNoSwitchGRPC(t *testing.T) {
	env := kbEnv(t, kbArtifacts(), "v1.3.9")
	ctx := mdCtx("operator-key")

	resp, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v1.4.0", DryRun: true})
	if err != nil {
		t.Fatalf("dry_run: %v", err)
	}
	if !resp.Ok {
		t.Fatalf("dry_run 应成功: %+v", resp)
	}
	if resp.ActiveVersion != "v1.3.9" {
		t.Errorf("dry_run 不应切换: active=%s", resp.ActiveVersion)
	}
	if kv := kbVersion(t, env); kv.Version != "v1.3.9" {
		t.Errorf("dry_run 后版本不应变化: %s", kv.Version)
	}

	// 未知版本 → fetch 失败，不切换
	missing, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v9.9.9", DryRun: true})
	if err != nil {
		t.Fatalf("未知版本 dry_run: %v", err)
	}
	if missing.Ok || missing.Reason == "" {
		t.Fatalf("未知版本应失败并给出原因: %+v", missing)
	}
	if kv := kbVersion(t, env); kv.Version != "v1.3.9" {
		t.Errorf("fetch 失败后版本不应变化: %s", kv.Version)
	}
}

// ── TDD-KB-02：原子切换 + 版本/哈希一致性 ───────────────────────────────

func TestKb02AtomicSwitchGRPC(t *testing.T) {
	env := kbEnv(t, kbArtifacts(), "v1.3.9")
	ctx := mdCtx("operator-key")

	resp, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v1.4.0"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !resp.Ok || resp.ActiveVersion != "v1.4.0" || resp.PreviousVersion != "v1.3.9" {
		t.Fatalf("切换响应异常: %+v", resp)
	}
	if resp.SwitchTimeMs >= 1 {
		t.Errorf("写锁窗口 >=1ms: %dms（规格要求 <1ms）", resp.SwitchTimeMs)
	}
	kv := kbVersion(t, env)
	if kv.Version != "v1.4.0" || kv.SchemaHash != "sha256:schema-v1.4.0" || kv.ModelHash != "sha256:model-v1.4.0" {
		t.Errorf("HealthCheck 版本/哈希未对齐: %+v", kv)
	}
}

// ── TDD-KB-03：Sanity Check 失败时旧版本保持有效 ────────────────────────

func TestKb03SanityCheckFailKeepsOldGRPC(t *testing.T) {
	badIndex := kbArtifacts()
	badIndex["v1.4.0"] = kbArtifact("v1.4.0", "v1.4.0", 0, true, 100) // 关键规则索引缺失
	env := kbEnv(t, badIndex, "v1.3.9")
	ctx := mdCtx("operator-key")

	resp, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v1.4.0"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if resp.Ok || resp.Reason != "sanity_check_failed" {
		t.Fatalf("索引缺失应 sanity_check_failed: %+v", resp)
	}
	if kv := kbVersion(t, env); kv.Version != "v1.3.9" {
		t.Errorf("Sanity 失败后不应切换: %s", kv.Version)
	}

	badOnnx := kbArtifacts()
	badOnnx["v1.4.0"] = kbArtifact("v1.4.0", "v1.4.0", 12, false, 100) // ONNX 空特征推理异常
	env2 := kbEnv(t, badOnnx, "v1.3.9")
	ctx2 := mdCtx("operator-key")
	resp2, err := env2.client.ReloadKnowledge(ctx2, &uwspb.ReloadRequest{Version: "v1.4.0"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if resp2.Ok || resp2.Reason != "sanity_check_failed" {
		t.Fatalf("ONNX 异常应 sanity_check_failed: %+v", resp2)
	}
	if kv := kbVersion(t, env2); kv.Version != "v1.3.9" {
		t.Errorf("ONNX Sanity 失败后不应切换: %s", kv.Version)
	}
}

// ── TDD-KB-04：内存不足拒绝切换 → RESOURCE_EXHAUSTED ───────────────────

func TestKb04MemoryLimitRejectGRPC(t *testing.T) {
	heavy := map[string]*knowledge.Artifact{
		"v1.3.9": kbArtifact("v1.3.9", "v1.3.0", 10, true, 200),
		"v1.4.0": kbArtifact("v1.4.0", "v1.4.0", 12, true, 200), // 200+200=400 > 300MB
	}
	env := kbEnv(t, heavy, "v1.3.9")
	ctx := mdCtx("operator-key")

	resp, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v1.4.0"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if resp.Ok || resp.Reason != knowledge.ErrResourceExhausted.Error() {
		t.Fatalf("应 RESOURCE_EXHAUSTED: %+v", resp)
	}
	if kv := kbVersion(t, env); kv.Version != "v1.3.9" {
		t.Errorf("内存不足时不应切换: %s", kv.Version)
	}
}

// ── TDD-KB-05：版本兼容性；force=true 跳过（紧急回滚路径） ──────────────

func TestKb05VersionCompatibilityForceGRPC(t *testing.T) {
	incompat := kbArtifacts()
	incompat["v1.4.0"] = kbArtifact("v1.4.0", "v2.0.0", 12, true, 100)
	env := kbEnv(t, incompat, "v1.3.9")
	ctx := mdCtx("operator-key")

	resp, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v1.4.0"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if resp.Ok || resp.Reason != "version_incompatible" {
		t.Fatalf("应拒绝不兼容版本: %+v", resp)
	}
	if kv := kbVersion(t, env); kv.Version != "v1.3.9" {
		t.Errorf("不兼容时不应切换: %s", kv.Version)
	}

	forceResp, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v1.4.0", Force: true})
	if err != nil {
		t.Fatalf("force reload: %v", err)
	}
	if !forceResp.Ok || forceResp.ActiveVersion != "v1.4.0" {
		t.Fatalf("force=true 应跳过兼容性检查: %+v", forceResp)
	}
}

// ── TDD-KB-07：回滚一致性（经 ReloadKnowledge 回滚旧版本，ACTIVE→STAGING）─

func TestKb07RollbackConsistencyL2StatusGRPC(t *testing.T) {
	v140 := kbArtifact("v1.4.0", "v1.4.0", 12, true, 100)
	v140.Rules = map[string]string{
		"l1:login":      "ACTIVE",
		"l2:search_btn": "ACTIVE",
		"l2:submit_btn": "ACTIVE",
	}
	env := kbEnv(t, map[string]*knowledge.Artifact{
		"v1.3.9": kbArtifact("v1.3.9", "v1.3.0", 10, true, 100),
		"v1.4.0": v140,
	}, "v1.3.9")
	ctx := mdCtx("operator-key")

	if resp, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v1.4.0"}); err != nil || !resp.Ok {
		t.Fatalf("切到 v1.4.0 失败: err=%v resp=%+v", err, resp)
	}
	if _, st, ok := env.srv.kb.ExecuteAction("l2:search_btn"); !ok || st != "ACTIVE" {
		t.Fatalf("v1.4.0 中 l2:search_btn 应为 ACTIVE: st=%s ok=%v", st, ok)
	}

	rb, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v1.3.9"})
	if err != nil {
		t.Fatalf("回滚: %v", err)
	}
	if !rb.Ok || rb.ActiveVersion != "v1.3.9" || rb.PreviousVersion != "v1.4.0" {
		t.Fatalf("回滚响应异常: %+v", rb)
	}
	if v, st, ok := env.srv.kb.ExecuteAction("l2:search_btn"); !ok || v != "v1.3.9" {
		t.Fatalf("回滚后规则应存在: v=%s ok=%v", v, ok)
	} else if st != "STAGING" {
		t.Errorf("新 ACTIVE 规则回滚后应为 STAGING: %s", st)
	}
	if v, st, ok := env.srv.kb.ExecuteAction("l1:login"); !ok || v != "v1.3.9" || st != "ACTIVE" {
		t.Errorf("旧版本原有 ACTIVE 规则不应被污染: v=%s st=%s ok=%v", v, st, ok)
	}
}

// ── TDD-KB-06：P0 自动回滚（连续 50×NOT_FOUND → 回滚 + HealthCheck 标记）─

func TestKb06AutoRollbackP0GRPC(t *testing.T) {
	env := kbEnv(t, kbArtifacts(), "v1.3.9")
	ctx := mdCtx("operator-key")
	if resp, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v1.4.0"}); err != nil || !resp.Ok {
		t.Fatalf("切到 v1.4.0 失败: err=%v resp=%+v", err, resp)
	}

	initCtx(t, env.client, "coordinator-key")
	actx := mdCtx("coordinator-key")
	notFound := func(i int) {
		t.Helper()
		_, err := env.client.ExecuteAction(actx, &uwspb.ActionIntentRequest{
			IntentId:       fmt.Sprintf("kb06-nf-%d", i),
			TargetSemantic: "not_found",
			ExpectedAction: "click",
		})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("第 %d 次应 NOT_FOUND: %v", i, err)
		}
	}
	success := func() {
		t.Helper()
		if _, err := env.client.ExecuteAction(actx, &uwspb.ActionIntentRequest{
			IntentId:       "kb06-ok",
			TargetSemantic: "登录",
			ExpectedAction: "click",
		}); err != nil {
			t.Fatalf("成功动作: %v", err)
		}
	}

	// 49 次连续 NOT_FOUND → 版本不变。
	for i := 0; i < AutoRollbackThreshold-1; i++ {
		notFound(i)
	}
	if v := kbVersion(t, env).Version; v != "v1.4.0" {
		t.Fatalf("49 次 NOT_FOUND 后不应回滚: %s", v)
	}
	// 成功动作重置失明计数：再 49 次 NOT_FOUND 仍不回滚。
	success()
	for i := 0; i < AutoRollbackThreshold-1; i++ {
		notFound(100 + i)
	}
	if v := kbVersion(t, env).Version; v != "v1.4.0" {
		t.Fatalf("成功重置后 49 次 NOT_FOUND 不应回滚: %s", v)
	}
	// 第 50 次（重置后累计满阈值）→ 自动回滚到 v1.3.9 + HealthCheck 标记。
	notFound(200)
	if v := kbVersion(t, env).Version; v != "v1.3.9" {
		t.Fatalf("50 次连续 NOT_FOUND 应自动回滚到 v1.3.9: %s", v)
	}
	hc, err := env.client.HealthCheck(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if !hc.GetRolledBack() {
		t.Fatal("HealthCheck 应附带回滚标记 rolled_back=true")
	}
	if got := hc.GetRollbackReason(); got != "auto_rollback v1.4.0 → v1.3.9" {
		t.Fatalf("rollback_reason = %q", got)
	}
}
