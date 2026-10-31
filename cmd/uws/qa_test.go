package main

import (
	"strings"
	"testing"
)

func TestQAHelp(t *testing.T) {
	help := qaHelp()
	if !strings.Contains(help, "WebTouch QA Studio") {
		t.Errorf("qaHelp missing title")
	}
	if !strings.Contains(help, "--viewports") {
		t.Errorf("qaHelp missing --viewports")
	}
	if !strings.Contains(help, "--ref-img") {
		t.Errorf("qaHelp missing --ref-img")
	}
}

func TestRunQAMissingURL(t *testing.T) {
	err := runQA([]string{})
	if err != nil {
		t.Errorf("expected nil when no args (prints help), got %v", err)
	}

	err2 := runQA([]string{"--viewports", "390"})
	if err2 == nil || !strings.Contains(err2.Error(), "缺少必填参数") {
		t.Errorf("expected error for missing --url, got %v", err2)
	}
}
