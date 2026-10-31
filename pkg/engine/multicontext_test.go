// 多命名上下文（多标签会话）测试：v1.5.3 追加 context_name / CloseContext /
// context_hint["context"] 路由（2026-08-25 用户需求：多 Session 多 Tab 并行，
// 每个会话固定绑定一个浏览器标签页；开启新会话 = 开启新 Tab）。
package engine

import (
	"testing"

	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestEngineNamedContextsRouteIndependently(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")

	initA, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId: "task-a", ContextName: proto.String("tab-a"),
	})
	if err != nil {
		t.Fatalf("init tab-a: %v", err)
	}
	initB, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId: "task-b", ContextName: proto.String("tab-b"),
	})
	if err != nil {
		t.Fatalf("init tab-b: %v", err)
	}
	if initA.TargetId == initB.TargetId {
		t.Fatalf("两个命名上下文不应共享同一 Target: %s", initA.TargetId)
	}

	env.srv.mu.Lock()
	active := len(env.srv.contexts)
	env.srv.mu.Unlock()
	if active != 2 {
		t.Fatalf("期望 2 个命名上下文，实际 %d", active)
	}

	// 同一客户端下按 context_hint["context"] 路由到各自标签页。
	respA, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "intent-x", TargetSemantic: "probe", ExpectedAction: "extract",
		ContextHint: map[string]string{"context": "tab-a"},
	})
	if err != nil || !respA.Success {
		t.Fatalf("execute on tab-a: err=%v resp=%+v", err, respA)
	}
	respB, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "intent-x", TargetSemantic: "probe", ExpectedAction: "extract",
		ContextHint: map[string]string{"context": "tab-b"},
	})
	if err != nil || !respB.Success {
		t.Fatalf("execute on tab-b: err=%v resp=%+v", err, respB)
	}
	// 幂等缓存按「客户端 + 上下文名 + intent_id」隔离：同 intent 跨 Tab 不串缓存。
	if respA.DecisionTraceId == respB.DecisionTraceId {
		t.Fatalf("跨上下文幂等缓存串了: %s", respA.DecisionTraceId)
	}

	// 缺省 context_hint 走默认上下文（未初始化 → FAILED_PRECONDITION）。
	_, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "intent-default", TargetSemantic: "probe", ExpectedAction: "extract",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("默认上下文未初始化应 FAILED_PRECONDITION，实际 %v", err)
	}
}

func TestEngineCloseContextReleasesTab(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	if _, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId: "task-a", ContextName: proto.String("tab-a"),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	active, _ := env.srv.pool.Stats()
	if active != 1 {
		t.Fatalf("期望 1 个活跃 Target，实际 %d", active)
	}

	closeResp, err := env.client.CloseContext(ctx, &uwspb.CloseContextRequest{ContextName: "tab-a"})
	if err != nil || !closeResp.Ok {
		t.Fatalf("close: err=%v resp=%+v", err, closeResp)
	}
	active, _ = env.srv.pool.Stats()
	if active != 0 {
		t.Fatalf("关闭后 Target 应释放，实际活跃 %d", active)
	}

	// 幂等：重复关闭返回 ok。
	if _, err := env.client.CloseContext(ctx, &uwspb.CloseContextRequest{ContextName: "tab-a"}); err != nil {
		t.Fatalf("重复关闭应幂等: %v", err)
	}

	// 关闭后的上下文上执行动作 → FAILED_PRECONDITION。
	_, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "intent-x", TargetSemantic: "probe", ExpectedAction: "extract",
		ContextHint: map[string]string{"context": "tab-a"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("已关闭上下文应 FAILED_PRECONDITION，实际 %v", err)
	}
}

func TestEngineReinitSameNameReusesTab(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	init1, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId: "task-1", ContextName: proto.String("tab-a"),
	})
	if err != nil {
		t.Fatalf("init#1: %v", err)
	}
	init2, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId: "task-2", ContextName: proto.String("tab-a"),
	})
	if err != nil {
		t.Fatalf("init#2: %v", err)
	}
	if init1.TargetId != init2.TargetId {
		t.Fatalf("v1.5.6 同名会话应复用同一 Tab（续页），实际 init1=%q init2=%q", init1.TargetId, init2.TargetId)
	}
	active, _ := env.srv.pool.Stats()
	if active != 1 {
		t.Fatalf("同名复用不应积累标签页，实际活跃 %d", active)
	}
}

func TestEngineDefaultContextReusesTarget(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	// 无 context_name = 默认上下文；v1.5.6 起同样按「同名会话复用」语义续页。
	init1, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{TaskId: "task-1"})
	if err != nil {
		t.Fatalf("init#1: %v", err)
	}
	init2, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{TaskId: "task-2"})
	if err != nil {
		t.Fatalf("init#2: %v", err)
	}
	if init1.TargetId != init2.TargetId {
		t.Fatalf("默认上下文重复初始化应复用同一 Target，实际 init1=%q init2=%q", init1.TargetId, init2.TargetId)
	}
	active, _ := env.srv.pool.Stats()
	if active != 1 {
		t.Fatalf("默认上下文重复初始化不应积累，实际活跃 %d", active)
	}
}
