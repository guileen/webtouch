// 真机集成例（2026-08-25 用户实测反馈的闭环验证）：页面未就绪点歪 → 录音/弹层 →
// 无法继续。自愈决策的纯逻辑单测随实现迁至 pkg/browser/input/recovery_test.go。
package browser

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCDPSessionTargetDestroyed（真实 Chrome）：标签页被关闭（模拟用户手动关 Tab）
// 后 ExecuteAction 必须显式返回 target_destroyed，供 API 层自动重建新 Tab。
func TestCDPSessionTargetDestroyed(t *testing.T) {
	requireCDP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := (&CDPFactory{Headless: true}).Create(ctx, Config{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer s.Close()

	sess := s.(*cdpSession)
	// 从测试侧关闭本会话标签页（等价于用户手动关 Tab / Target.targetDestroyed）。
	_, _ = sess.client.Send(ctx, "Target.closeTarget",
		map[string]any{"targetId": sess.targetID})
	// 等事件落地。
	deadline := time.Now().Add(3 * time.Second)
	for !(bridgeInputSession{sess}).TargetDestroyed() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	ok, obs := sess.ExecuteAction(ctx, "click", "gemini_new_chat", "")
	if ok {
		t.Fatal("目标已销毁仍返回成功")
	}
	if !strings.Contains(obs, "target_destroyed") {
		t.Fatalf("期望 target_destroyed，实际 %q", obs)
	}
}
