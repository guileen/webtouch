package gateway

import (
	"context"
	"path/filepath"
	"testing"
)

// TestEnsureSessionForRetryAbsorbsEngineFlap 守住「网关→引擎这一段不可达」在整轮重试里的
// **有界快重拨**：引擎短窗（秒级）重启应被自动吸收，不把失败立刻推给调用方。
func TestEnsureSessionForRetryAbsorbsEngineFlap(t *testing.T) {
	flap := &EngineError{Code: codeUnavailable, Reason: "connection error: desc = connection refused"}
	eng := &fakeEngine{initFailTimes: 2, initFailErr: flap}
	gw := newTestGatewayOpts(t, eng, Options{
		AuthStatePath: filepath.Join(t.TempDir(), "gateway-auth-state.json"),
	})
	sess, err := gw.ensureSessionForRetry(context.Background(), chatReq("deepseek-web", "deepseek-chat"))
	if err != nil {
		t.Fatalf("期望重拨后建会话成功，得 err=%v", err)
	}
	if sess == nil || sess.ContextID == "" {
		t.Fatalf("会话未建: %+v", sess)
	}
	if got := len(eng.initNames); got != 3 {
		t.Fatalf("期望「失败 2 次 + 第 3 次成功」（共 3 次拨号），得 %d 次: %v", got, eng.initNames)
	}
}

// TestEnsureSessionForRetryGivesUpBounded 守住重拨是**有界**的：引擎持续不可达时在
// engineUnavailableRetries 处放弃、如实返回错误，不无限重试。
func TestEnsureSessionForRetryGivesUpBounded(t *testing.T) {
	flap := &EngineError{Code: codeUnavailable, Reason: "connection error: desc = connection refused"}
	eng := &fakeEngine{initFailTimes: 99, initFailErr: flap}
	gw := newTestGatewayOpts(t, eng, Options{
		AuthStatePath: filepath.Join(t.TempDir(), "gateway-auth-state.json"),
	})
	_, err := gw.ensureSessionForRetry(context.Background(), chatReq("deepseek-web", "deepseek-chat"))
	if err == nil {
		t.Fatal("引擎持续不可达时应放弃并返回错误")
	}
	if got := classifyChatError(err).Code; got != errCodeEngineUnavailable {
		t.Fatalf("放弃时错误仍应归 engine_unavailable，得 code=%s", got)
	}
	if got := len(eng.initNames); got != engineUnavailableRetries {
		t.Fatalf("拨号次数应有界＝%d，得 %d", engineUnavailableRetries, got)
	}
}

// TestChatInternalRetryAbsorbsEngineFlap 端到端证明「内部自动重试」对引擎短窗不可达真有效：
// 首轮撞上引擎不可达 → 整轮重试 → 重建会话时前 2 次拨号仍失败、第 3 次成功 → 重放一轮 ⇒ 200。
func TestChatInternalRetryAbsorbsEngineFlap(t *testing.T) {
	flap := &EngineError{Code: codeUnavailable, Reason: "connection error: desc = connection refused"}
	eng := &fakeEngine{initFailTimes: 2, initFailErr: flap, incrementAnswers: true}
	gw := newTestGatewayOpts(t, eng, Options{
		AuthStatePath: filepath.Join(t.TempDir(), "gateway-auth-state.json"),
	})
	out, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-chat",
		Message{Role: "user", Content: "你好"}))
	if err != nil {
		t.Fatalf("期望引擎短窗不可达被内部重试吸收为成功，得 err=%v", err)
	}
	if out == nil || len(out.Choices) == 0 {
		t.Fatalf("应答为空: %+v", out)
	}
}
