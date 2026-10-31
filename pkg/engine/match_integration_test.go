// TDD-MATCH-05/06 集成层测试（USER_ACTION.md #6/#12）：
//   - MATCH-05：SimCDPSession 注入页面候选 → 引擎 runAction 执行 L3 SimHash
//     结构校验（结构不一致候选被剔除 → NOT_FOUND；L2 同义词候选不被误伤）；
//   - MATCH-06：中间置信度候选 → A* 多步搜索（路径末步执行；HTTP 4XX/5XX
//     物理湮灭 → 无可行路径 NOT_FOUND + 入服务级黑名单）。
//
// 页面候选通道是引擎内部模拟（WASM 探针 AXTree 的降级注入），非契约缺口。
package engine

import (
	"testing"

	"uws/pkg/browser"
	"uws/pkg/matcher"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── TDD-MATCH-05 集成层：L3 SimHash 物理校验 ──────────────────────────

func TestMatch05L3StructureRejectIntegrationGRPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	// 结构一致：L3 保留并执行。
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95}})
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "m05-keep", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("结构一致候选应执行: err=%v resp=%+v", err, resp)
	}

	// 文本含模式但结构不一致（"登录广告" vs 参考 "登录"，SimHash 距离 > D_h=5）：
	// L3 剔除 → 全池被拒 → NOT_FOUND（页面上无此节点）。
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录广告", Confidence: 0.95}})
	_, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "m05-rej", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("L3 应剔除结构不一致候选 → NOT_FOUND, got %v", err)
	}

	// L2 同义词候选（Confidence=0 走规则/同义词匹配）不被 L3 文本代理误伤：
	// "Sign in" 经 site_synonyms TENTATIVE 0.7 入围并执行。
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "Sign in"}})
	resp, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "m05-l2", TargetSemantic: "Sign in", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("L2 同义词候选应执行: err=%v resp=%+v", err, resp)
	}
	assertConfidence(t, resp.Confidence, 0.70)
}

// TDD-MATCH-05 延伸（Tier 1.5）：规则级 StructHash 注入 → 真实引擎双通道 L3。
// rename（"立即登录" vs 参考 "登录"）文本 Hamming 超阈，但候选结构哈希与
// 规则库组件基因（component_gene）写入的结构参考一致 → 结构通道救回并执行。
// 反证：无规则 StructHash 时退化为文本单通道 → 该候选被 L3 剔除（NOT_FOUND）。
func TestMatch05RuleStructHashDualChannelIntegrationGRPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	refStruct := matcher.StructuralSimHash64(matcher.StructFeature{Tag: "button", Role: "button"})
	// 规则库写入该语义的结构参考（component_gene 产物）。
	if _, err := env.srv.matcher.InsertRule(matcher.Rule{
		RuleType: "text_pattern", TextPattern: "立即登录", Action: "click",
		SemanticLabel: "登录", Role: "button", Confidence: 0.95,
		Source: "component_gene", Status: "active", StructHash: refStruct,
	}); err != nil {
		t.Fatalf("注入规则 StructHash 失败: %v", err)
	}

	// 候选为 rename 文本（与参考结构一致）：双通道应保留并执行。
	sess.SetPageCandidates([]browser.PageCandidate{{
		Text: "立即登录", Confidence: 0.95, StructHash: refStruct,
		Tag: "button", Role: "button",
	}})
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "m05-structdual", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("规则级 StructHash 应启用双通道救回 rename 候选: err=%v resp=%+v", err, resp)
	}

	// 对照：候选结构远隔（div/generic）→ 结构通道不兼容，文本又超阈 → NOT_FOUND。
	farStruct := matcher.StructuralSimHash64(matcher.StructFeature{Tag: "div", Role: "generic"})
	sess.SetPageCandidates([]browser.PageCandidate{{
		Text: "立即登录", Confidence: 0.95, StructHash: farStruct,
		Tag: "div", Role: "generic",
	}})
	_, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "m05-structdual-far", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("结构远隔候选应 NOT_FOUND, got %v", err)
	}
}

// ── TDD-MATCH-06 集成层：A* 多步搜索 ──────────────────────────────────

func TestMatch06AStarSearchIntegrationGRPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	// 中间置信度（0.7）根候选 + 子图：A* 搜索到路径末步并执行该节点。
	// 子节点显式声明与参考结构一致（SimHash=SimHash64("login_form")），
	// 避免文本代理误伤（真实 DOM 特征通道落地后由结构特征填充）。
	sess.SetPageCandidates([]browser.PageCandidate{{
		Text: "login_form", Confidence: 0.7, Flow: true,
		Children: []browser.PageCandidate{{
			Text: "login_form_submit", Confidence: 0.9,
			SimHash: matcher.SimHash64("login_form"),
		}},
	}})
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "m06-1", TargetSemantic: "login_form", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("A* 候选应执行: err=%v resp=%+v", err, resp)
	}
	acts := sess.Actions()
	if len(acts) != 1 || acts[0] != "click|login_form_submit|" {
		t.Fatalf("A* 应执行路径末步 login_form_submit, got %v", acts)
	}

	// 物理湮灭：根节点 HTTP 403 → +∞ 惩罚入黑名单，无可行路径 → NOT_FOUND。
	sess.SetPageCandidates([]browser.PageCandidate{{
		Text: "register_now", Confidence: 0.8, HTTPStatus: 403,
	}})
	_, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "m06-2", TargetSemantic: "register_now", ExpectedAction: "click",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("物理湮灭节点应 NOT_FOUND, got %v", err)
	}
	if n := env.srv.astarBlacklist.Len(); n != 1 {
		t.Fatalf("湮灭节点应入服务级物理黑名单, len=%d", n)
	}
}

// TestCandidateTreeNestedDirectMatchGRPC 验证真实 DOM 候选树（WASM 探针
// Children 填充后）的嵌套子节点也能直接入围匹配：容器 0.4 + 子按钮 0.95，
// 预序遍历收集子节点 → C≥θ_high 直接执行子节点（不依赖 A*）。
func TestCandidateTreeNestedDirectMatchGRPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	sess.SetPageCandidates([]browser.PageCandidate{{
		Text: "container", Confidence: 0.4,
		Children: []browser.PageCandidate{{Text: "登录", Confidence: 0.95}},
	}})
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "tree-1", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("树形子候选应直接执行: err=%v resp=%+v", err, resp)
	}
	acts := sess.Actions()
	if len(acts) != 1 || acts[0] != "click|登录|" {
		t.Fatalf("应执行子节点 登录, got %v", acts)
	}
}
