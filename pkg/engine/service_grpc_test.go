package engine

import (
	"context"
	"fmt"
	"net"
	"testing"
	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ── TDD-API-02：幂等 TTL（同 intent_id 返回缓存，副作用不重复） ─────────

func TestApi02IdempotencyTtl(t *testing.T) {
	env := newTestEnv(t)
	initCtx(t, env.client, "coordinator-key")

	intent := &uwspb.ActionIntentRequest{
		IntentId: "idem-1", TargetSemantic: "登录", ExpectedAction: "click",
	}
	r1, err := env.client.ExecuteAction(mdCtx("coordinator-key"), intent)
	if err != nil {
		t.Fatalf("第一次执行: %v", err)
	}
	r2, err := env.client.ExecuteAction(mdCtx("coordinator-key"), intent)
	if err != nil {
		t.Fatalf("第二次执行: %v", err)
	}
	if !proto.Equal(r1, r2) {
		t.Errorf("同 intent_id 应返回相同缓存结果\nr1=%+v\nr2=%+v", r1, r2)
	}
	if r1.DecisionTraceId == "" {
		t.Error("decision_trace_id 不应为空")
	}

	r3, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "idem-2", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil {
		t.Fatalf("新意图执行: %v", err)
	}
	if r3.DecisionTraceId == r1.DecisionTraceId {
		t.Error("不同 intent_id 不应命中同一缓存")
	}
}

// ── TDD-API-03：双向流请求/响应一一对应、顺序一致 ───────────────────────

func TestApi03ExecuteActionStreamOrder(t *testing.T) {
	env := newTestEnv(t)
	initCtx(t, env.client, "coordinator-key")

	stream, err := env.client.ExecuteActionStream(mdCtx("coordinator-key"))
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	ids := []string{"s-1", "s-2", "s-3", "s-4", "s-5"}
	for _, id := range ids {
		if err := stream.Send(&uwspb.ActionIntentRequest{
			IntentId: id, TargetSemantic: "登录", ExpectedAction: "click",
		}); err != nil {
			t.Fatalf("send %s: %v", id, err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	for i, id := range ids {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv %s: %v", id, err)
		}
		if resp.IntentId != id {
			t.Fatalf("第 %d 个响应顺序错乱: got %s want %s", i, resp.IntentId, id)
		}
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("流应已结束")
	}
}

// ── TDD-API-04：API Key 鉴权与角色越权 ───────────────────────────────────

func TestApi04ApiKeyAuthz(t *testing.T) {
	env := newTestEnv(t)
	intent := &uwspb.ActionIntentRequest{
		IntentId: "a-1", TargetSemantic: "登录", ExpectedAction: "click",
	}

	// 无 Key / 非法 Key → UNAUTHENTICATED
	if _, err := env.client.ExecuteAction(context.Background(), intent); status.Code(err) != codes.Unauthenticated {
		t.Errorf("无 Key 应 UNAUTHENTICATED: %v", err)
	}
	if _, err := env.client.ExecuteAction(mdCtx("bad-key"), intent); status.Code(err) != codes.Unauthenticated {
		t.Errorf("非法 Key 应 UNAUTHENTICATED: %v", err)
	}

	// monitor/operator 越权调 ExecuteAction → PERMISSION_DENIED
	if _, err := env.client.ExecuteAction(mdCtx("monitor-key"), intent); status.Code(err) != codes.PermissionDenied {
		t.Errorf("monitor 调 ExecuteAction 应 PERMISSION_DENIED: %v", err)
	}
	if _, err := env.client.ExecuteAction(mdCtx("operator-key"), intent); status.Code(err) != codes.PermissionDenied {
		t.Errorf("operator 调 ExecuteAction 应 PERMISSION_DENIED: %v", err)
	}
	// coordinator 调 ReloadKnowledge → PERMISSION_DENIED（仅 operator）
	if _, err := env.client.ReloadKnowledge(mdCtx("coordinator-key"), &uwspb.ReloadRequest{Version: EngineVersion, DryRun: true}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("coordinator 调 ReloadKnowledge 应 PERMISSION_DENIED: %v", err)
	}
	// operator 调 ReloadKnowledge(dry_run) → 允许且不切换
	resp, err := env.client.ReloadKnowledge(mdCtx("operator-key"), &uwspb.ReloadRequest{Version: EngineVersion, DryRun: true})
	if err != nil {
		t.Fatalf("operator ReloadKnowledge 应允许: %v", err)
	}
	if !resp.Ok || resp.ActiveVersion != EngineVersion {
		t.Errorf("dry_run 应 OK 且版本不变: %+v", resp)
	}
	// monitor 调 HealthCheck → 允许
	if _, err := env.client.HealthCheck(mdCtx("monitor-key"), &uwspb.Empty{}); err != nil {
		t.Errorf("monitor HealthCheck 应允许: %v", err)
	}
}

// ── TDD-SEC-06：输入边界（XSS/长度/协议前缀） ─────────────────────────────

func TestSec06InputSanitization(t *testing.T) {
	env := newTestEnv(t)
	cases := []*uwspb.ActionIntentRequest{
		{IntentId: "x1", TargetSemantic: "<script>alert(1)</script>", ExpectedAction: "click"},
		{IntentId: "x2", TargetSemantic: "a\"b'c/d", ExpectedAction: "click"},
		{IntentId: "x3", TargetSemantic: "登录", ExpectedAction: "input", Value: proto.String("javascript:alert(1)")},
		{IntentId: "x4", TargetSemantic: "登录", ExpectedAction: "click",
			ContextHint: map[string]string{"bad key": "v"}},
		{IntentId: "", TargetSemantic: "登录", ExpectedAction: "click"},
		{IntentId: "x6", TargetSemantic: "登录", ExpectedAction: "click",
			ContextHint: map[string]string{"k": string(make([]byte, 257))}},
	}
	for i, c := range cases {
		if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), c); status.Code(err) != codes.InvalidArgument {
			t.Errorf("case %d 应 INVALID_ARGUMENT: %v", i, err)
		}
	}
}

// 2026-08-27：全角/CJK 标点语义放行——drama 完整标题「都市逆袭：外卖小哥逆天改命」
// 含全角冒号「：」，此前被 semanticRe 白名单拒 INVALID_ARGUMENT；修复后必须放行。
// 同时保持安全红线：ASCII 危险字符（< > " ' /）仍须被拒。
func TestSec06AllowFullWidthPunctuationSemantic(t *testing.T) {
	ok := []string{
		"都市逆袭：外卖小哥逆天改命",
		"第 4 集（付费）",
		"《都市逆袭》",
		"“外卖小哥”——逆天改命",
		"VIP会员：解锁",
	}
	for _, sem := range ok {
		err := validateIntent(&uwspb.ActionIntentRequest{
			IntentId: "ok-" + sem, TargetSemantic: sem, ExpectedAction: "click",
		})
		if status.Code(err) == codes.InvalidArgument {
			t.Errorf("语义 %q 应放行，却 INVALID_ARGUMENT: %v", sem, err)
		}
	}
	bad := []string{`<script>alert(1)</script>`, `a"b'c/d`, `a<b>c`}
	for _, sem := range bad {
		err := validateIntent(&uwspb.ActionIntentRequest{
			IntentId: "bad-" + sem, TargetSemantic: sem, ExpectedAction: "click",
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("语义 %q 应 INVALID_ARGUMENT，却 %v", sem, err)
		}
	}
}

// 2026-08-28：ASCII 点号语义放行——drama 页 viewCount「25.8M」（数字+点+字母）
// 此前被 semanticRe 白名单拒 INVALID_ARGUMENT；修复后必须放行。
// 同时保持安全红线：ASCII 危险字符（< > " ' /）仍须被拒。
func TestSec06AllowDotInNumericSemantic(t *testing.T) {
	ok := []string{
		"25.8M 播放",
		"播放量 3.2 万",
		"第 1.5 集",
	}
	for _, sem := range ok {
		err := validateIntent(&uwspb.ActionIntentRequest{
			IntentId: "ok-" + sem, TargetSemantic: sem, ExpectedAction: "click",
		})
		if status.Code(err) == codes.InvalidArgument {
			t.Errorf("语义 %q 应放行，却 INVALID_ARGUMENT: %v", sem, err)
		}
	}
	bad := []string{`<script>alert(1)</script>`, `a"b'c/d`, `a<b>c`, `onclick=alert`}
	for _, sem := range bad {
		err := validateIntent(&uwspb.ActionIntentRequest{
			IntentId: "bad-" + sem, TargetSemantic: sem, ExpectedAction: "click",
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("语义 %q 应 INVALID_ARGUMENT，却 %v", sem, err)
		}
	}
}

// ── TDD-API-06：HealthCheck / GetKnowledgeVersion 一致性 ──────────────────

func TestApi06HealthVersionConsistency(t *testing.T) {
	env := newTestEnv(t)

	hc, err := env.client.HealthCheck(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if !hc.Ok || hc.Version != EngineVersion {
		t.Errorf("初始健康状态应 OK 且版本 %s: %+v", EngineVersion, hc)
	}
	if hc.MaxTargets <= 0 {
		t.Errorf("max_targets 应 >0: %+v", hc)
	}
	kv, err := env.client.GetKnowledgeVersion(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("GetKnowledgeVersion: %v", err)
	}
	art := DefaultArtifact()
	if kv.Version != hc.Version || kv.SchemaHash != art.SchemaHash || kv.ModelHash != art.ModelHash {
		t.Errorf("版本/哈希与内置 artifact 不一致: kv=%+v hc=%+v", kv, hc)
	}

	initCtx(t, env.client, "coordinator-key")
	hc2, err := env.client.HealthCheck(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("HealthCheck after init: %v", err)
	}
	if hc2.ActiveTargets != 1 {
		t.Errorf("初始化后 active_targets 应=1: %+v", hc2)
	}
}

// ── InitializeContext 字段接线（视口默认/UA/时区/初始导航） ──────────────

func TestInitContextWiring(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	resp, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId:          "task-2",
		Timezone:        "Asia/Shanghai",
		LaunchOverrides: &uwspb.BrowserLaunchOverrides{UserAgent: proto.String("UWS-Test/1.0")},
		InitialUrl:      proto.String("https://example.com/login"),
		Viewport:        &uwspb.ViewportConfig{Width: 1440, Height: 900, DeviceScaleFactor: 2},
	})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if !resp.Ok || resp.TargetId == "" || resp.ContextId == "" {
		t.Errorf("响应应携带 target/context id: %+v", resp)
	}
	// 通过 HealthCheck 验证 Target 已入池
	hc, err := env.client.HealthCheck(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if hc.ActiveTargets != 1 {
		t.Errorf("active_targets 应=1: %+v", hc)
	}

	// 空 task_id → INVALID_ARGUMENT
	if _, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("空 task_id 应 INVALID_ARGUMENT: %v", err)
	}
}

// ── 知识库热更接线：真实版本切换（operator 角色） ────────────────────────

func TestReloadKnowledgeWire(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("operator-key")
	// dry_run 当前版本 → OK 不切换
	r, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: EngineVersion, DryRun: true})
	if err != nil || !r.Ok {
		t.Fatalf("dry_run 应 OK: %+v err=%v", r, err)
	}
	// 不存在的版本 → ok=false + reason
	r2, err := env.client.ReloadKnowledge(ctx, &uwspb.ReloadRequest{Version: "v9.9.9", DryRun: true})
	if err != nil {
		t.Fatalf("reload 不应抛 gRPC 错误: %v", err)
	}
	if r2.Ok || r2.Reason == "" {
		t.Errorf("未知版本应 ok=false 带 reason: %+v", r2)
	}
}

// ── R3：同客户端重复 InitializeContext 不泄漏 Target 槽位 ────────────────

func TestReinitReleasesOldTarget(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	for i := 0; i < 5; i++ {
		if _, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
			TaskId: fmt.Sprintf("task-%d", i),
		}); err != nil {
			t.Fatalf("第 %d 次 InitializeContext: %v", i, err)
		}
	}
	hc, err := env.client.HealthCheck(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if hc.ActiveTargets != 1 {
		t.Fatalf("重复初始化应释放旧 Target，active_targets 应=1: %+v", hc)
	}
}

// v1.5.6 会话续页：同名 context_name（Session ID）重复初始化应复用同一标签页，
// 而不是“释放旧页开新页”——uws run/goal 由此可跨命令续作同一页面。
func TestSameContextNameReusesLiveTarget(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	first, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId:      "task-a",
		ContextName: proto.String("session-esa"),
	})
	if err != nil {
		t.Fatalf("首次 InitializeContext: %v", err)
	}
	second, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId:      "task-b",
		ContextName: proto.String("session-esa"),
		InitialUrl:  proto.String("https://example.com/2"),
	})
	if err != nil {
		t.Fatalf("续页 InitializeContext: %v", err)
	}
	if second.ContextId != first.ContextId {
		t.Fatalf("同名会话应复用同一 Context（续页），got %q != %q", second.ContextId, first.ContextId)
	}
	other, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId:      "task-c",
		ContextName: proto.String("session-other"),
	})
	if err != nil {
		t.Fatalf("异名会话 InitializeContext: %v", err)
	}
	if other.ContextId == first.ContextId {
		t.Fatalf("不同会话应各自独立 Context: %q", other.ContextId)
	}
	hc, err := env.client.HealthCheck(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if hc.ActiveTargets != 2 {
		t.Fatalf("两个命名会话应占用 2 个槽位, got %d", hc.ActiveTargets)
	}
}

// v1.5.6 会话续页跨进程：同一 API Key 的不同 gRPC 连接（真实场景=两次 uws 命令）
// 必须能凭 context_name 命中同一标签页——上下文槽位按 API Key 全局寻址，
// 而不是把连接地址算进 key（否则每次 CLI 都像“新客户端”找不到旧会话）。
func TestNamedSessionReusesAcrossConnections(t *testing.T) {
	env := newTestEnv(t)
	conn2, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return env.lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	t.Cleanup(func() { _ = conn2.Close() })
	client2 := uwspb.NewUwsEngineClient(conn2)

	ctx1 := mdCtx("coordinator-key")
	ctx2 := metadata.AppendToOutgoingContext(context.Background(), "x-api-key", "coordinator-key")
	init1, err := env.client.InitializeContext(ctx1, &uwspb.InitContextRequest{
		TaskId:      "cli-1",
		ContextName: proto.String("session-esa"),
	})
	if err != nil {
		t.Fatalf("连接1 init: %v", err)
	}
	init2, err := client2.InitializeContext(ctx2, &uwspb.InitContextRequest{
		TaskId:      "cli-2",
		ContextName: proto.String("session-esa"),
	})
	if err != nil {
		t.Fatalf("连接2 init: %v", err)
	}
	if init2.ContextId != init1.ContextId {
		t.Fatalf("跨连接同名会话应复用同一 Context，got conn1=%q conn2=%q", init1.ContextId, init2.ContextId)
	}
	hc, err := env.client.HealthCheck(mdCtx("monitor-key"), &uwspb.Empty{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if hc.ActiveTargets != 1 {
		t.Fatalf("跨连接复用不应新增槽位, got %d", hc.ActiveTargets)
	}
}

// v1.5.6 共享浏览器各会话各管各的 Tab：ExecuteAction 携带 context_hint["target_id"]
// 时引擎必须校验该 Target 属于当前会话；不一致 = NotFound，绝不悄悄打到别的标签页。
func TestExecuteActionTargetPinRejectsForeignTab(t *testing.T) {
	factory := &debugInfoFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(2, factory),
	})
	ctx := mdCtx("coordinator-key")
	init, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId:      "pin-a",
		ContextName: proto.String("pin-esa"),
	})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if init.TargetId != "real-cdp-target-1" {
		t.Fatalf("首个会话应占用 real-cdp-target-1, got %q", init.TargetId)
	}

	// 钉扎正确 Target：正常执行。
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId:       "pin-ok",
		TargetSemantic: "登录",
		ExpectedAction: "click",
		ContextHint:    map[string]string{"context": "pin-esa", "target_id": init.TargetId},
	})
	if err != nil || !resp.Success {
		t.Fatalf("钉扎本会话 Target 应成功: err=%v resp=%+v", err, resp)
	}

	// 钉扎别的会话/已释放 Target：必须 NotFound 且零物理执行。
	if _, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId:       "pin-bad",
		TargetSemantic: "登录",
		ExpectedAction: "click",
		ContextHint:    map[string]string{"context": "pin-esa", "target_id": "real-cdp-target-999"},
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("异 Target 钉扎应 NotFound，got %v", err)
	}
	if got := factory.sessions[0].Executed(); got != 1 {
		t.Fatalf("被拒绝的动作不应物理执行（只应剩正确钉扎那 1 次），executed=%d", got)
	}
}

// v1.5.6 续页语义：InitializeContextResponse 必须回传当前页 URL（真实 CDP 会话
// 可用时），协调器据此区分「续到旧页」与「新开 about:blank」，杜绝目标模式
// 第一轮把真实页面误报成空白页（react.dev 复现：VLM 幻觉“当前页是京东首页”）。
func TestInitResponseCarriesCurrentURLOnReuse(t *testing.T) {
	factory := &debugInfoFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(1, factory),
	})
	ctx := mdCtx("coordinator-key")
	if _, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId:      "url-a",
		ContextName: proto.String("url-esa"),
	}); err != nil {
		t.Fatalf("首次 InitializeContext: %v", err)
	}
	factory.sessions[0].SetNavOnAction("https://react.dev/learn/scaling-up-with-reducer-and-context")
	if _, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId:       "url-nav",
		TargetSemantic: "go",
		ExpectedAction: "click",
		ContextHint:    map[string]string{"context": "url-esa"},
	}); err != nil {
		t.Fatalf("导航动作: %v", err)
	}
	reuse, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{
		TaskId:      "url-b",
		ContextName: proto.String("url-esa"),
	})
	if err != nil {
		t.Fatalf("续页 InitializeContext: %v", err)
	}
	if reuse.CurrentUrl != "https://react.dev/learn/scaling-up-with-reducer-and-context" {
		t.Fatalf("续页响应应回传当前 URL，got %q", reuse.CurrentUrl)
	}
}
