package main

import "testing"

func TestVLMAPIKeyAutoReadsDeepSeekOrUWS(t *testing.T) {
	t.Setenv("UWS_VLM_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	if got := vlmAPIKey(); got != "" {
		t.Fatalf("两者均未设置时应为空，got %q", got)
	}

	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek")
	if got := vlmAPIKey(); got != "sk-deepseek" {
		t.Fatalf("应回退 DEEPSEEK_API_KEY，got %q", got)
	}

	t.Setenv("UWS_VLM_API_KEY", "sk-uws")
	if got := vlmAPIKey(); got != "sk-uws" {
		t.Fatalf("UWS_VLM_API_KEY 应优先，got %q", got)
	}
}
