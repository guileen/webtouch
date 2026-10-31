// 混合指挥引擎侧 node_id 确定性引用（v1.5.5）：
//   - ActionIntentRequest.node_id > 0 时跳过语义/选择器派发，直接按 BFS 序回查
//     定位器执行（GLOBAL §5 #57 / docs/agent-interaction-contract-2026-09-23.md）；
//   - 新增 ExportPageTree RPC（导出极简 AXTree，供协调器外发 VLM 指挥官）。
//
// 契约层双层验证见 tests/integration/test_grpc_contract.py（append-only）。
package engine

import (
	"context"
	"strings"
	"testing"

	"uws/pkg/browser"
	"uws/pkg/vlm"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// nodeSimFactory 捕获最近创建的 SimCDPSession，供断言 node_id 动作记录。
type nodeSimFactory struct {
	SimFactory
	last *SimCDPSession
}

func (f *nodeSimFactory) Create(ctx context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s, err := f.SimFactory.Create(ctx, cfg)
	if err != nil {
		return nil, err
	}
	f.last = s.(*SimCDPSession)
	return f.last, nil
}

func nodeEnv(t *testing.T, f *nodeSimFactory) (*testEnv, *SimCDPSession) {
	t.Helper()
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Pool: browser.NewPool(2, f)})
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"),
		&uwspb.InitContextRequest{TaskId: "node-task"}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	return env, f.last
}

// TestNodeDirectExecuteBypassesSemantic 验证 node_id>0 时绕过语义/选择器派发：
// 即便 target_semantic 完全无法命中，仍按 node_id 确定性执行并回显命中证据。
func TestNodeDirectExecuteBypassesSemantic(t *testing.T) {
	f := &nodeSimFactory{}
	env, sess := nodeEnv(t, f)
	nodeID := int32(12)
	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "node-1",
		TargetSemantic: "node:12",
		ExpectedAction: "click",
		NodeId:         &nodeID,
	})
	if err != nil {
		t.Fatalf("node_id 应绕过语义匹配并成功: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success 应为 true: %+v", resp)
	}
	found := false
	for _, a := range sess.Actions() {
		if strings.Contains(a, "node|click|12|") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("应记录 node|click|12|，实际 acts=%v", sess.Actions())
	}
	if resp.HitElement == nil || resp.HitElement.Selector != "sim:node:12" {
		t.Fatalf("hit_element 应回显 sim:node:12: %+v", resp.HitElement)
	}
}

// TestNodeIdNegativeInvalidArgument 验证负 node_id 输入边界（append-only 字段校验）。
func TestNodeIdNegativeInvalidArgument(t *testing.T) {
	env := newTestEnv(t)
	initCtx(t, env.client, "coordinator-key")
	nodeID := int32(-1)
	_, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "node-neg",
		TargetSemantic: "登录",
		ExpectedAction: "click",
		NodeId:         &nodeID,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("负 node_id 应 INVALID_ARGUMENT，got %v", err)
	}
}

// TestExportPageTreeRPC 验证新增 ExportPageTree RPC 生命周期与数据通路。
func TestExportPageTreeRPC(t *testing.T) {
	env := newTestEnv(t)
	// 未初始化 → FAILED_PRECONDITION。
	if _, err := env.client.ExportPageTree(mdCtx("coordinator-key"),
		&uwspb.ExportPageTreeRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("未初始化应 FAILED_PRECONDITION，got %v", err)
	}
	initCtx(t, env.client, "coordinator-key")
	resp, err := env.client.ExportPageTree(mdCtx("coordinator-key"), &uwspb.ExportPageTreeRequest{})
	if err != nil {
		t.Fatalf("ExportPageTree: %v", err)
	}
	if !strings.Contains(resp.Tree, "立即购买") {
		t.Fatalf("tree 应含默认样例 AXTree: %q", resp.Tree)
	}
}

// TestNodeDirectLearnsSelector 验证 node_id 确定性执行成功后回写真实定位器：
// 命中元素证据（selector + 可见文本）→ learned「页面 + 元素文本 → 选择器」。
func TestNodeDirectLearnsSelector(t *testing.T) {
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: &vlm.MockClient{}, Learned: learned}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	// 脚本化命中元素证据：模拟 resolveNodeById 回查得到的真实选择器与可见文本。
	sess.SetLastHit(browser.HitElement{Selector: "button.buy", Tag: "button", Text: "立即购买"})

	nodeID := int32(12)
	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "node-learn",
		TargetSemantic: "node:12",
		ExpectedAction: "click",
		NodeId:         &nodeID,
	})
	if err != nil {
		t.Fatalf("node_id 执行: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success=false: %+v", resp)
	}
	e, ok := learned.Lookup(vlmPageURL, "立即购买")
	if !ok || e.Selector != "button.buy" {
		t.Fatalf("node_id learned 未回写 selector: %+v ok=%v", e, ok)
	}
}
