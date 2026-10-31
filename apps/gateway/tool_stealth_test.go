package gateway

import (
	"strings"
	"testing"
)

func TestBuildStealthInstruction(t *testing.T) {
	tools := []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "bash",
				Description: "执行终端命令",
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "read_file",
				Description: "读取本地文件内容",
			},
		},
	}

	res := BuildStealthInstruction("You are a helpful coding assistant.", tools)
	if !strings.Contains(res, "You are a helpful coding assistant.") {
		t.Errorf("expected instruction to contain original instructions")
	}
	if !strings.Contains(res, "- `bash`: 执行终端命令") {
		t.Errorf("expected bash tool instruction")
	}
	if !strings.Contains(res, "```run:bash") {
		t.Errorf("expected run: block format convention")
	}
	// 验证不包含 "工具" 或 "tool" 敏感风控词
	if strings.Contains(strings.ToLower(res), "tool") {
		t.Errorf("instruction should avoid the word 'tool' for stealth")
	}
	if strings.Contains(res, "工具") {
		t.Errorf("instruction should avoid '工具' for stealth")
	}
}

func TestFormatExecResultForWeb(t *testing.T) {
	out := FormatExecResultForWeb("bash", "M file.go\n?? new.go")
	expected := "> 运行输出 (`bash`):\n```\nM file.go\n?? new.go\n```"
	if out != expected {
		t.Errorf("got %q, want %q", out, expected)
	}
}

func TestParseStealthActionCalls_SingleCommand(t *testing.T) {
	raw := "我需要先核对 git 状态：\n\n```run:bash\ngit status --short\n```\n\n请稍等。"
	clean, calls := ParseStealthActionCalls(raw)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "bash" {
		t.Errorf("call name = %q, want bash", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"command":"git status --short"}` {
		t.Errorf("call arguments = %q", calls[0].Function.Arguments)
	}
	if strings.Contains(clean, "```run:bash") {
		t.Errorf("clean text should not contain run:bash block")
	}
	if !strings.Contains(clean, "我需要先核对 git 状态") {
		t.Errorf("clean text should preserve thinking context: %q", clean)
	}
}

func TestParseStealthActionCalls_JSONArguments(t *testing.T) {
	raw := "定位到了目标，读取前 50 行：\n\n```run:read_file\n{\"path\": \"apps/gateway/chat.go\", \"lines\": \"1-50\"}\n```\n"
	clean, calls := ParseStealthActionCalls(raw)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("call name = %q, want read_file", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, `"apps/gateway/chat.go"`) {
		t.Errorf("call arguments = %q", calls[0].Function.Arguments)
	}
	if strings.Contains(clean, "```run:read_file") {
		t.Errorf("clean text should not contain run:read_file block")
	}
}

func TestParseStealthActionCalls_IgnoresNormalCodeBlocks(t *testing.T) {
	raw := "这里是 Go 代码示例：\n\n```go\npackage main\nfunc main() {}\n```\n请参考。"
	clean, calls := ParseStealthActionCalls(raw)

	if len(calls) != 0 {
		t.Fatalf("expected 0 calls for standard go code block, got %d", len(calls))
	}
	if clean != raw {
		t.Errorf("clean text should equal raw for normal code blocks")
	}
}

func TestParseStealthActionCalls_DOMFormat(t *testing.T) {
	raw := "我们先看工作区状态。\n\nrun:bash\n复制\n下载\ngit status\n\n请把输出贴回来，我再判断下一步。"
	clean, calls := ParseStealthActionCalls(raw)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "bash" {
		t.Errorf("call name = %q, want bash", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"command":"git status"}` {
		t.Errorf("call arguments = %q, want {\"command\":\"git status\"}", calls[0].Function.Arguments)
	}
	if strings.Contains(clean, "run:bash") || strings.Contains(clean, "复制") {
		t.Errorf("clean text should strip code header and buttons: %q", clean)
	}
}
