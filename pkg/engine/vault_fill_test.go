// 凭据直填（2026-09-23）：值写成 `@vault:<名>` 时，由引擎在本机解引用后再填表。
// 关注点：①计划文本/argv 里只有条目名，明文不经过协调器；②取不到凭据必须
// fail-closed（绝不把 `@vault:xxx` 这串引用原样填进页面）；③evaluate 不参与解引用。
package engine

import (
	"errors"
	"strings"
	"testing"

	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// 注：常量与辅助（testAPIKey / mdCtx / initCtx / selectorRecordingFactory）
// 见 service_test.go 与 mutex_test.go。

func TestEngVaultFillResolvesReferenceBeforeTyping(t *testing.T) {
	factory := &selectorRecordingFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys:         testKeys(),
		Pool:         browser.NewPool(1, factory),
		VaultResolve: func(name string) (string, error) { return "s3cret-" + name, nil },
	})
	initCtx(t, env.client, "coordinator-key")

	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "fill-1", TargetSemantic: "chat_input", ExpectedAction: "input",
		Value: proto.String("@vault:autodl"),
	})
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !resp.Success {
		t.Fatalf("动作应成功: %+v", resp)
	}
	s := factory.sessions[0]
	s.mu.Lock()
	gotValue, gotAct, calls := s.gotValue, s.gotAct, s.calls
	s.mu.Unlock()
	if calls == 0 {
		t.Fatal("应发生一次物理输入")
	}
	if gotAct != "input" {
		t.Fatalf("动作类型应为 input，实际 %q", gotAct)
	}
	if gotValue != "s3cret-autodl" {
		t.Fatalf("填进去的应是解引用后的真值，实际 %q", gotValue)
	}
	if strings.Contains(gotValue, "@vault:") {
		t.Fatalf("引用串绝不能原样落到页面上: %q", gotValue)
	}
}

func TestEngVaultFillFailsClosedWhenCredentialMissing(t *testing.T) {
	factory := &selectorRecordingFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(1, factory),
		VaultResolve: func(string) (string, error) {
			return "", errors.New("vault: 没有这一条")
		},
	})
	initCtx(t, env.client, "coordinator-key")

	_, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "fill-2", TargetSemantic: "chat_input", ExpectedAction: "input",
		Value: proto.String("@vault:nothere"),
	})
	if err == nil {
		t.Fatal("凭据取不到时必须报错停下（fail-closed）")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("错误码应为 FAILED_PRECONDITION，实际 %s（%v）", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "@vault:nothere") {
		t.Fatalf("错误里应点名缺哪一个条目，实际: %v", err)
	}
	// 协调器按此前缀把「凭据缺失」与「Target 崩溃」分开（proto 错误码表
	// FAILED_PRECONDITION 行）：前缀丢了会导致重试/重建上下文白做一次。
	if !strings.HasPrefix(status.Convert(err).Message(), vaultUnresolvedPrefix) {
		t.Fatalf("错误消息应以 %q 开头，实际: %v", vaultUnresolvedPrefix, err)
	}
	for _, s := range factory.sessions {
		s.mu.Lock()
		calls, value := s.calls, s.gotValue
		s.mu.Unlock()
		if calls != 0 || strings.Contains(value, "@vault:") {
			t.Fatalf("fail-closed 时不得往页面填任何东西（calls=%d value=%q）", calls, value)
		}
	}
}

func TestEngVaultFillIgnoresPlainAndEvaluateValues(t *testing.T) {
	factory := &selectorRecordingFactory{}
	called := 0
	env := newTestEnvWithOptions(t, Options{
		Keys:         testKeys(),
		Pool:         browser.NewPool(1, factory),
		VaultResolve: func(string) (string, error) { called++; return "x", nil },
	})
	initCtx(t, env.client, "coordinator-key")

	// 普通文本（不是引用）照旧原样填，且不该去碰 vault。
	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "fill-3", TargetSemantic: "chat_input", ExpectedAction: "input",
		Value: proto.String("你好"),
	}); err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	// evaluate 是 JS 源码通道，不参与解引用。
	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "fill-4", TargetSemantic: "eval_label", ExpectedAction: "evaluate",
		Value: proto.String("@vault:autodl"),
	}); err != nil {
		t.Fatalf("ExecuteAction(evaluate): %v", err)
	}
	if called != 0 {
		t.Fatalf("非引用 / evaluate 不该触发 vault 解析，实际调用 %d 次", called)
	}
}
