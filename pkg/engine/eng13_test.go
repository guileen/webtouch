// TDD-ENG-13 引擎映射层测试（tdd-checklist.md §3.3）：
//   - 模拟 CDP 会话脚本化「新窗口打开」→ ExecuteAction 返回 NEW_WINDOW_OPENED；
//   - 事件为一次性（读后清零），后续动作回到 STABLE；
//   - 真实 CDP 事件驱动验证见 pkg/browser/cdp_session_test.go（无 Chrome 自动 skip）。
package engine

import (
	"testing"

	uwspb "uws/proto/gen/go/proto/v1"
)

func TestEng13NewWindowOpenedMapping(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	// 模拟点击 target="_blank"：CDP 事件源报告新 Target。
	sess.SetNewWindow("target-2")
	resp, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "eng13-win", TargetSemantic: "新标签", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("新窗口动作应成功: err=%v resp=%+v", err, resp)
	}
	if resp.ContextState != uwspb.ContextState_NEW_WINDOW_OPENED {
		t.Fatalf("应返回 NEW_WINDOW_OPENED, got %s", resp.ContextState)
	}

	// 一次性语义：事件已消费，原 Target 继续执行并回到 STABLE。
	resp, err = env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "eng13-next", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("后续动作应成功: err=%v resp=%+v", err, resp)
	}
	if resp.ContextState != uwspb.ContextState_STABLE {
		t.Fatalf("无新窗口时应 STABLE, got %s", resp.ContextState)
	}
}
