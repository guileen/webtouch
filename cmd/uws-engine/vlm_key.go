package main

import "os"

// vlmAPIKey 返回 VLM 兜底的 API Key（2026-08-27 用户要求自动读取）：
// 优先 UWS_VLM_API_KEY（引擎专属变量），回退 DEEPSEEK_API_KEY（用户通用
// DeepSeek Key，已存在于 ~/.zshrc）。两者都未设置时返回空串（VLM 兜底禁用）。
func vlmAPIKey() string {
	if k := os.Getenv("UWS_VLM_API_KEY"); k != "" {
		return k
	}
	return os.Getenv("DEEPSEEK_API_KEY")
}
