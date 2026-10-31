// 点击/输入自愈纯逻辑单测（2026-08-25 用户实测反馈：页面未就绪点歪 → 录音/弹层
// → 无法继续；本组覆盖自愈决策函数，DOM 级检测与真机闭环走宿主集成例）。
package input

import "testing"

func TestNeedSendEnabled(t *testing.T) {
	if !needSendEnabled("gemini_send") {
		t.Fatal("gemini_send 应要求按钮启用")
	}
	if !needSendEnabled("send_button") {
		t.Fatal("send_button 应要求按钮启用")
	}
	if needSendEnabled("gemini_new_chat") {
		t.Fatal("新对话按钮不应要求 enabled")
	}
}

func TestIsGeminiClick(t *testing.T) {
	if !IsGeminiClick("gemini_new_chat") {
		t.Fatal("gemini_* 点击语义应启用自愈")
	}
	if !IsGeminiClick("gemini_send") {
		t.Fatal("gemini_send 应启用自愈")
	}
	if IsGeminiClick("chat_input") {
		t.Fatal("非 Gemini 语义不应启用 Gemini 自愈")
	}
}
