package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"uws/pkg/browser"
	"uws/pkg/challenge"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestEngineCircuitBreakerTripsOnChallenge(t *testing.T) {
	env := newTestEnv(t)

	initResp, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId:      "task-challenge-1",
		ContextName: proto.String("ctx-challenge-1"),
	})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}

	cid := initResp.GetContextId()
	s := env.srv
	s.mu.Lock()
	var bc *browser.BrowserContext
	for _, c := range s.contexts {
		if c.ID == cid || len(s.contexts) == 1 {
			bc = c
			break
		}
	}
	if bc == nil {
		s.mu.Unlock()
		t.Fatalf("context %s not found in engine", cid)
	}
	sim := bc.Session().(*SimCDPSession)
	// 模拟触发挑战态
	sim.SetChallenge(true)
	s.mu.Unlock()

	intent := &uwspb.ActionIntentRequest{
		IntentId:       "intent-1",
		TargetSemantic: "登录",
		ExpectedAction: "click",
		ContextHint:    map[string]string{"context": "ctx-challenge-1"},
	}

	// 第一次调用：探测到挑战态，触发熔断，返回 UNAUTHENTICATED
	_, err = env.client.ExecuteAction(mdCtx("coordinator-key"), intent)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected UNAUTHENTICATED, got %v", err)
	}

	cb := s.CircuitBreakerFor(bc.ID)
	if cb.State() != challenge.CircuitOpen {
		t.Fatalf("expected circuit breaker state OPEN, got %s", cb.State())
	}

	// 第二次调用：熔断器已开路，任何新的自动化尝试被硬性拒止（防乱点），不产生任何物理调用
	intent2 := &uwspb.ActionIntentRequest{
		IntentId:       "intent-2",
		TargetSemantic: "重新点击",
		ExpectedAction: "click",
		ContextHint:    map[string]string{"context": "ctx-challenge-1"},
	}
	_, err = env.client.ExecuteAction(mdCtx("coordinator-key"), intent2)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected UNAUTHENTICATED due to open circuit breaker, got %v", err)
	}

	// 人工接管：经由 HumanInputForwarder 逆向点击并解除挑战
	sim.SetChallenge(false) // 人工点击后挑战已通过
	if err := sim.DispatchHumanInput(context.Background(), challenge.HumanInputEvent{
		Type: challenge.HumanInputClick,
		X:    100, Y: 200,
	}); err != nil {
		t.Fatalf("DispatchHumanInput: %v", err)
	}
	cb.Reset()

	// 验证人工解除后自动化执行恢复
	intent3 := &uwspb.ActionIntentRequest{
		IntentId:       "intent-3",
		TargetSemantic: "登录",
		ExpectedAction: "evaluate",
		Value:          proto.String(`{"status":"logged_in"}`),
		ContextHint:    map[string]string{"context": "ctx-challenge-1"},
	}
	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), intent3)
	if err != nil {
		t.Fatalf("ExecuteAction after reset failed: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success after reset")
	}
}

func TestEngineAntiThrashingConsecutiveFailures(t *testing.T) {
	env := newTestEnv(t)

	initResp, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId:      "task-thrashing-1",
		ContextName: proto.String("ctx-thrash-1"),
	})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}

	cid := initResp.GetContextId()
	s := env.srv
	s.mu.Lock()
	var bc *browser.BrowserContext
	for _, c := range s.contexts {
		if c.ID == cid || len(s.contexts) == 1 {
			bc = c
			break
		}
	}
	if bc == nil {
		s.mu.Unlock()
		t.Fatalf("context %s not found in engine", cid)
	}
	s.mu.Unlock()

	cb := s.CircuitBreakerFor(bc.ID)

	// 连续 3 次失败
	for i := 1; i <= 3; i++ {
		cb.RecordFailure("click_timeout")
	}

	if cb.State() != challenge.CircuitOpen {
		t.Fatalf("expected circuit breaker to trip to OPEN after 3 failures, got %s", cb.State())
	}

	// 此时任何动作均应被 AllowAction 拦截
	intent := &uwspb.ActionIntentRequest{
		IntentId:       "intent-thrash-test",
		TargetSemantic: "盲目重试按钮",
		ExpectedAction: "click",
		ContextHint:    map[string]string{"context": "ctx-thrash-1"},
	}
	_, err = env.client.ExecuteAction(mdCtx("coordinator-key"), intent)
	// 2026-09-23 误报修复：防乱点熔断（连续失败）不是页面存在人机验证的证据，
	// 不得上报 UNAUTHENTICATED/CHALLENGE_INTERCEPTED —— 否则协调器会误判为
	// WAF/CAPTCHA 并走「VLM 破解 → 人工接管」流程（实测：空白页连点 3 次不存在
	// 元素即报 challenge_intercepted）。
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected RESOURCE_EXHAUSTED (anti-thrashing, not a challenge), got %v", err)
	}
	if cb.LastSignal().IsHumanChallenge() {
		t.Fatalf("expected anti-thrashing signal to be flagged as non-challenge")
	}
}

// contextAndBreaker 取本次初始化命中的上下文与其熔断器（测试内共享小工具）。
func contextAndBreaker(t *testing.T, env *testEnv, initResp *uwspb.InitContextResponse) (*browser.BrowserContext, *challenge.CircuitBreaker) {
	t.Helper()
	s := env.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.contexts {
		if c.ID == initResp.GetContextId() || len(s.contexts) == 1 {
			return c, s.CircuitBreakerFor(c.ID)
		}
	}
	t.Fatalf("context %s not found in engine", initResp.GetContextId())
	return nil, nil
}

// TestEngineExtractNoMatchDoesNotTripAntiThrashing（2026-09-23）：只读 extract 的
// 确定性 no_match 不是「乱点风险」，既不得计入防乱点熔断，也不得被熔断拦下。
// 实测背景：IANA 公开页上 3 次 extract no_match 把整条 session 冻结，属无意义空转。
func TestEngineExtractNoMatchDoesNotTripAntiThrashing(t *testing.T) {
	env := newTestEnv(t)
	initResp, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId:      "task-extract-notrip",
		ContextName: proto.String("ctx-extract-notrip"),
	})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	_, cb := contextAndBreaker(t, env, initResp)

	for i := 1; i <= 4; i++ {
		_, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
			IntentId:       fmt.Sprintf("intent-extract-notrip-%d", i),
			TargetSemantic: "页面上不存在的文本",
			ExpectedAction: "extract",
			ContextHint:    map[string]string{"context": "ctx-extract-notrip"},
		})
		if status.Code(err) == codes.ResourceExhausted {
			t.Fatalf("第 %d 次只读 extract 不得被防乱点熔断拦下: %v", i, err)
		}
	}
	if cb.State() == challenge.CircuitOpen {
		t.Fatalf("只读 extract 的 no_match 不得把熔断推到 OPEN，state=%s", cb.State())
	}

	// 对照：熔断 OPEN 后 extract 仍须放行（只读），click 仍被拦截（乱点风险）。
	for i := 1; i <= 3; i++ {
		cb.RecordFailure("click_timeout")
	}
	if cb.State() != challenge.CircuitOpen {
		t.Fatalf("click 失败应能推到 OPEN，state=%s", cb.State())
	}
	_, err = env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "intent-click-blocked",
		TargetSemantic: "盲目重试按钮",
		ExpectedAction: "click",
		ContextHint:    map[string]string{"context": "ctx-extract-notrip"},
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("熔断 OPEN 时 click 必须被拦下，got %v", err)
	}
	_, err = env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "intent-extract-open",
		TargetSemantic: "页面上不存在的文本",
		ExpectedAction: "extract",
		ContextHint:    map[string]string{"context": "ctx-extract-notrip"},
	})
	if status.Code(err) == codes.ResourceExhausted {
		t.Fatalf("熔断 OPEN 时只读 extract 仍须放行，got %v", err)
	}
}

// TestEngineAntiThrashingNotReportedAsChallenge 是误报修复的正向护栏：熔断信号
// 必须被判定为「非人机验证」，且返回码为 RESOURCE_EXHAUSTED，消息里不得出现
// 「challenge intercepted」字样（协调器据此区分 WAF 拦截与自动化熔断）。
func TestEngineAntiThrashingNotReportedAsChallenge(t *testing.T) {
	env := newTestEnv(t)

	initResp, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId:      "task-thrash-2",
		ContextName: proto.String("ctx-thrash-2"),
	})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	s := env.srv
	s.mu.Lock()
	var bc *browser.BrowserContext
	for _, c := range s.contexts {
		if c.ID == initResp.GetContextId() || len(s.contexts) == 1 {
			bc = c
			break
		}
	}
	if bc == nil {
		s.mu.Unlock()
		t.Fatalf("context %s not found in engine", initResp.GetContextId())
	}
	s.mu.Unlock()

	cb := s.CircuitBreakerFor(bc.ID)
	for i := 1; i <= 3; i++ {
		cb.RecordFailure("no_match")
	}
	if cb.LastSignal().IsHumanChallenge() {
		t.Fatalf("连续失败熔断不得被标记为人机验证挑战")
	}

	intent := &uwspb.ActionIntentRequest{
		IntentId:       "intent-thrash-2",
		TargetSemantic: "不存在的元素",
		ExpectedAction: "click",
		ContextHint:    map[string]string{"context": "ctx-thrash-2"},
	}
	_, err = env.client.ExecuteAction(mdCtx("coordinator-key"), intent)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected RESOURCE_EXHAUSTED, got %v", err)
	}
	if strings.Contains(status.Convert(err).Message(), "challenge intercepted") {
		t.Fatalf("熔断消息不得声称 challenge intercepted: %v", err)
	}
}
