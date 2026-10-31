package gateway

import (
	"regexp"
	"strings"
)

var (
	// thinkTagRegex 匹配标准 <think>...</think> 闭合标签
	thinkTagRegex = regexp.MustCompile(`(?s)^\s*<think>(.*?)</think>\s*(.*)$`)
	// unclosedThinkRegex 匹配未闭合的 <think> 标签（常见于流式截断或思考中）
	unclosedThinkRegex = regexp.MustCompile(`(?s)^\s*<think>(.*)$`)
)

// ExtractReasoningAndAnswer 从模型原始回答中分离深度思考链 (reasoning_content) 与最终正文答复。
// 兼容 DeepSeek R1 原生 `<think>...</think>` 格式与现代 Web 端的思考包装。
func ExtractReasoningAndAnswer(raw string) (reasoning string, answer string) {
	if strings.TrimSpace(raw) == "" {
		return "", ""
	}

	// 1. 尝试匹配完整闭合的 <think>...</think>
	if matches := thinkTagRegex.FindStringSubmatch(raw); len(matches) == 3 {
		return strings.TrimSpace(matches[1]), strings.TrimSpace(matches[2])
	}

	// 2. 尝试匹配未闭合的 <think>（仍在思考中）
	if matches := unclosedThinkRegex.FindStringSubmatch(raw); len(matches) == 2 {
		return strings.TrimSpace(matches[1]), ""
	}

	// 3. 无思考标签，全量作为正文
	return "", raw
}
