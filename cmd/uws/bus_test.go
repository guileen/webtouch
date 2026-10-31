package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestBusEnv(t *testing.T) {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := busEnv(nil)
	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("busEnv failed: %v", err)
	}

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	output := buf.String()

	if !strings.Contains(output, "export OPENAI_BASE_URL=") {
		t.Errorf("busEnv 应包含 OPENAI_BASE_URL: %s", output)
	}
	if !strings.Contains(output, "export OPENAI_API_KEY=") {
		t.Errorf("busEnv 应包含 OPENAI_API_KEY: %s", output)
	}
}

func TestBusGuide(t *testing.T) {
	guide := busGuide()
	if !strings.Contains(guide, "webtouch bus status") {
		t.Errorf("guide 应包含 webtouch bus status")
	}
	if !strings.Contains(guide, "webtouch bus env") {
		t.Errorf("guide 应包含 webtouch bus env")
	}
	if !strings.Contains(guide, "uws bus status") {
		t.Errorf("guide 应标明弃用别名 uws bus status")
	}
}
