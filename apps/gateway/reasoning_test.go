package gateway

import (
	"testing"
)

func TestExtractReasoningAndAnswer(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantReasoning string
		wantAnswer    string
	}{
		{
			name:          "标准闭合标签",
			raw:           "<think>\n用户询问 1+1 等于几。\n这是基础算术问题。\n</think>\n1+1 等于 2。",
			wantReasoning: "用户询问 1+1 等于几。\n这是基础算术问题。",
			wantAnswer:    "1+1 等于 2。",
		},
		{
			name:          "未闭合标签（思考进行中）",
			raw:           "<think>\n正在分析物理学问题...",
			wantReasoning: "正在分析物理学问题...",
			wantAnswer:    "",
		},
		{
			name:          "纯文本无思考标签",
			raw:           "你好！很高兴为你服务。",
			wantReasoning: "",
			wantAnswer:    "你好！很高兴为你服务。",
		},
		{
			name:          "思考内容包含代码块",
			raw:           "<think>\n考虑使用如下代码：\n```go\nfmt.Println(1)\n```\n确认无误。\n</think>\n这是解决方案。",
			wantReasoning: "考虑使用如下代码：\n```go\nfmt.Println(1)\n```\n确认无误。",
			wantAnswer:    "这是解决方案。",
		},
		{
			name:          "空字符串",
			raw:           "",
			wantReasoning: "",
			wantAnswer:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotR, gotA := ExtractReasoningAndAnswer(tc.raw)
			if gotR != tc.wantReasoning {
				t.Errorf("ExtractReasoningAndAnswer() gotReasoning = %q, want %q", gotR, tc.wantReasoning)
			}
			if gotA != tc.wantAnswer {
				t.Errorf("ExtractReasoningAndAnswer() gotAnswer = %q, want %q", gotA, tc.wantAnswer)
			}
		})
	}
}

func TestMakeCompletion_WithReasoning(t *testing.T) {
	g := &Gateway{}
	raw := "<think>\n先分析量子纠缠现象。\n</think>\n量子纠缠是量子力学的基本特性。"
	cmpl := g.makeCompletion(12345, "deepseek-reasoner", "sess_q", raw, "解释量子纠缠", nil)

	if len(cmpl.Choices) == 0 {
		t.Fatalf("choices 不应为空")
	}
	ch := cmpl.Choices[0]
	if ch.Message.ReasoningContent != "先分析量子纠缠现象。" {
		t.Errorf("ReasoningContent 不符合预期: %q", ch.Message.ReasoningContent)
	}
	if ch.Message.Content != "量子纠缠是量子力学的基本特性。" {
		t.Errorf("Content 不符合预期: %q", ch.Message.Content)
	}
}
