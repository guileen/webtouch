// TDD-ENG-21（新窗口接管，2026-09-22 用户现场反馈批次 P0-1）：
// 动作打开新窗口/新标签页后，引擎默认把会话作用面切到新 target，并在响应里回报
// new_target_id / active_target_id 供调用方续页；原 Target 仍保留可用。
// 现场根因：微信快捷登录的授权弹窗「允许」必须人工点（引擎看得见、打不到）。
package engine

import (
	"testing"

	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"
)

func eng21Env(t *testing.T) (*testEnv, *SimCDPSession) {
	t.Helper()
	f := &evidenceSimFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(2, f),
	})
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"),
		&uwspb.InitContextRequest{TaskId: "eng21-task"}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	return env, f.last
}

func eng21Click(t *testing.T, env *testEnv, hints map[string]string) *uwspb.ActionResultResponse {
	t.Helper()
	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "eng21-click",
		TargetSemantic: "快捷登录",
		ExpectedAction: "click",
		ContextHint:    hints,
	})
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	return resp
}

func TestEng21NewWindowTakeoverDefault(t *testing.T) {
	env, sess := eng21Env(t)
	orig := sess.ID()
	sess.SetNewWindow("popup-1")

	resp := eng21Click(t, env, nil)
	if resp.NewTargetId != "popup-1" {
		t.Fatalf("new_target_id = %q, want popup-1", resp.NewTargetId)
	}
	if resp.ActiveTargetId != "popup-1" {
		t.Fatalf("active_target_id = %q, want popup-1（默认应接管新窗口）", resp.ActiveTargetId)
	}
	if resp.ContextState != uwspb.ContextState_NEW_WINDOW_OPENED {
		t.Fatalf("context_state = %v, want NEW_WINDOW_OPENED", resp.ContextState)
	}
	switches := sess.Switches()
	if len(switches) != 1 || switches[0] != orig+"->popup-1" {
		t.Fatalf("接管轨迹 = %v, want [%s->popup-1]", switches, orig)
	}
}

func TestEng21NewWindowTakeoverDisabled(t *testing.T) {
	env, sess := eng21Env(t)
	orig := sess.ID()
	sess.SetNewWindow("popup-1")

	resp := eng21Click(t, env, map[string]string{"follow_new_target": "0"})
	if resp.NewTargetId != "popup-1" {
		t.Fatalf("new_target_id = %q, want popup-1", resp.NewTargetId)
	}
	if resp.ActiveTargetId != orig {
		t.Fatalf("active_target_id = %q, want %q（开关关闭时不得接管）", resp.ActiveTargetId, orig)
	}
	if got := sess.Switches(); len(got) != 0 {
		t.Fatalf("开关关闭时不应发生切换: %v", got)
	}
}

func TestEng21PinnedTargetRoutesBeforeAndAfterTakeover(t *testing.T) {
	env, sess := eng21Env(t)
	orig := sess.ID()
	sess.SetNewWindow("popup-2")

	// 接管当轮：调用方钉的还是接管前的 Target（PinnedTarget），必须照常路由。
	resp := eng21Click(t, env, map[string]string{"target_id": orig})
	if resp.NewTargetId != "popup-2" || resp.ActiveTargetId != "popup-2" {
		t.Fatalf("接管当轮回报不符: new=%q active=%q", resp.NewTargetId, resp.ActiveTargetId)
	}

	// 接管后：调用方改用新 Target 续页，也必须路由回同一上下文。
	resp2 := eng21Click(t, env, map[string]string{"target_id": "popup-2"})
	if !resp2.Success {
		t.Fatalf("接管后按新 target 续页应成功: %+v", resp2)
	}
	if resp2.ActiveTargetId != "popup-2" {
		t.Fatalf("active_target_id = %q, want popup-2", resp2.ActiveTargetId)
	}
}
