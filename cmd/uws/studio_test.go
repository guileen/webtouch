package main

import (
	"strings"
	"testing"
)

func TestStudioHelp(t *testing.T) {
	help := studioHelp()
	if !strings.Contains(help, "WebTouch") {
		t.Fatalf("expected help to contain 'WebTouch', got:\n%s", help)
	}
	if !strings.Contains(help, "--port") {
		t.Fatalf("expected help to contain '--port', got:\n%s", help)
	}
}

func TestRunStudioHelpFlag(t *testing.T) {
	if err := runStudio([]string{"--help"}); err != nil {
		t.Fatalf("runStudio(--help) returned error: %v", err)
	}
}
