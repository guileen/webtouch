package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesNonStreamOverHTTP(t *testing.T) {
	fake := newFakeEngine()
	fake.extractPlan = []extractStep{
		{text: "我需要先查看当前目录的文件：\n\n```run:bash\nls -la\n```\n", done: true, active: true},
	}
	h, _ := newTestHandler(t, fake)

	rec := doJSON(t, h, http.MethodPost, "/internal/provider/responses", map[string]any{
		"model":        "deepseek-fast",
		"instructions": "You are a coding assistant.",
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "bash",
					"description": "执行终端命令",
				},
			},
		},
		"input": "请帮我检查工作区",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponseObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v, body: %s", err, rec.Body.String())
	}

	if resp.Object != "response" {
		t.Errorf("object=%q, want response", resp.Object)
	}
	if resp.Status != "completed" {
		t.Errorf("status=%q, want completed", resp.Status)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output length=%d, want 2 (1 message + 1 function_call)", len(resp.Output))
	}

	msgItem := resp.Output[0]
	if msgItem.Type != "message" || msgItem.Role != "assistant" {
		t.Errorf("expected first output to be assistant message: %+v", msgItem)
	}
	if len(msgItem.Content) == 0 || !strings.Contains(msgItem.Content[0].Text, "我需要先查看当前目录的文件") {
		t.Errorf("unexpected message text: %+v", msgItem.Content)
	}

	fcItem := resp.Output[1]
	if fcItem.Type != "function_call" || fcItem.Name != "bash" {
		t.Errorf("expected second output to be function_call bash: %+v", fcItem)
	}
	if fcItem.Arguments != `{"command":"ls -la"}` {
		t.Errorf("arguments = %q, want {\"command\":\"ls -la\"}", fcItem.Arguments)
	}
}

func TestResponsesWithFunctionCallOutputInput(t *testing.T) {
	fake := newFakeEngine()
	fake.extractPlan = []extractStep{
		{text: "看到了目录下存在 Makefile，接下来运行测试。\n", done: true, active: true},
	}
	h, _ := newTestHandler(t, fake)

	rec := doJSON(t, h, http.MethodPost, "/internal/provider/responses", map[string]any{
		"model": "deepseek-fast",
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_123",
				"name":    "bash",
				"output":  "Makefile\nmain.go\n",
			},
		},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponseObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(resp.Output) != 1 || resp.Output[0].Type != "message" {
		t.Fatalf("expected 1 message output, got %+v", resp.Output)
	}
	if !strings.Contains(resp.Output[0].Content[0].Text, "Makefile") {
		t.Errorf("unexpected output text: %s", resp.Output[0].Content[0].Text)
	}
}

func TestResponsesStreamOverHTTP(t *testing.T) {
	fake := newFakeEngine()
	fake.extractPlan = []extractStep{
		{text: "排查完成，运行指令：\n\n```run:bash\ngit diff\n```", done: true, active: true},
	}
	h, _ := newTestHandler(t, fake)

	rec := doJSON(t, h, http.MethodPost, "/internal/provider/responses", map[string]any{
		"model":  "deepseek-fast",
		"input":  "查看修改",
		"stream": true,
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.created") {
		t.Errorf("missing response.created in stream: %s", body)
	}
	if !strings.Contains(body, "event: response.output_text.delta") {
		t.Errorf("missing output_text.delta in stream: %s", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Errorf("missing response.completed in stream: %s", body)
	}

	// 检查是否有 function_call 事件触发
	if !strings.Contains(body, `"type":"function_call"`) {
		t.Errorf("expected function_call event in stream: %s", body)
	}
}

func TestChatCompletionsWithToolsAndToolCallOutput(t *testing.T) {
	fake := newFakeEngine()
	fake.extractPlan = []extractStep{
		{text: "检查配置：\n\n```run:read_file\n{\"path\": \"config.json\"}\n```", done: true, active: true},
	}
	h, _ := newTestHandler(t, fake)

	rec := doJSON(t, h, http.MethodPost, "/internal/provider/chat", map[string]any{
		"model": "deepseek-fast",
		"messages": []any{
			map[string]any{"role": "user", "content": "读一下配置"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "read_file",
				},
			},
		},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var comp ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &comp); err != nil {
		t.Fatal(err)
	}

	if len(comp.Choices) == 0 {
		t.Fatal("empty choices")
	}
	choice := comp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason=%q, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls count=%d, want 1", len(choice.Message.ToolCalls))
	}
	if choice.Message.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("tool name=%q, want read_file", choice.Message.ToolCalls[0].Function.Name)
	}
}
