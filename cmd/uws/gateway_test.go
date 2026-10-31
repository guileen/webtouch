package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestGatewayGuide(t *testing.T) {
	guide := gatewayGuide()
	if !strings.Contains(guide, "uws api") {
		t.Errorf("guide 应包含 uws api")
	}
	if !strings.Contains(guide, "/internal/provider/chat") {
		t.Errorf("guide 应包含 /internal/provider/chat")
	}
	if !strings.Contains(guide, "127.0.0.1:8000") {
		t.Errorf("guide 应包含默认端口 127.0.0.1:8000")
	}
}

func TestNormalizeGatewayArgs(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "port only",
			input:    []string{"--port", "8800"},
			expected: []string{"--listen=127.0.0.1:8800"},
		},
		{
			name:     "host and port",
			input:    []string{"--host", "0.0.0.0", "--port", "9000"},
			expected: []string{"--listen=0.0.0.0:9000"},
		},
		{
			name:     "strip legacy target",
			input:    []string{"--target", "llm_chat", "--listen", "127.0.0.1:8000"},
			expected: []string{"--listen", "127.0.0.1:8000"},
		},
		{
			name:     "keep other flags",
			input:    []string{"--engine-addr", "127.0.0.1:50051", "--chat-timeout", "30s"},
			expected: []string{"--engine-addr", "127.0.0.1:50051", "--chat-timeout", "30s"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := normalizeGatewayArgs(tc.input)
			if len(actual) != len(tc.expected) {
				t.Fatalf("length mismatch: got %v, want %v", actual, tc.expected)
			}
			for i := range actual {
				if actual[i] != tc.expected[i] {
					t.Errorf("arg[%d] got %s, want %s", i, actual[i], tc.expected[i])
				}
			}
		})
	}
}

func TestRunGatewayHelp(t *testing.T) {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runGateway([]string{"help"})
	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runGateway help failed: %v", err)
	}

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	output := buf.String()

	if !strings.Contains(output, "浏览器 provider 执行服务") {
		t.Errorf("output 应包含浏览器执行服务说明: %s", output)
	}
	if !strings.Contains(output, "webtouch session") || !strings.Contains(output, "/internal/provider/chat") {
		t.Errorf("output 应说明 CLI 是对外契约且点名已废弃的 provider 路径: %s", output)
	}
}
