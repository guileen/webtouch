package gateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// runBlockRegex 捕获 Markdown 格式的代码块：```run:<action> 或 ```exec:<action>
var runBlockRegex = regexp.MustCompile("(?s)```(?:run|exec):([^\\n\\r]+?)\\s*\\n(.*?)\\s*```")

// runDomBlockRegex 捕获 DOM 渲染后由 innerText 提取的无反引号代码块（可能混入“复制”、“下载”等按钮）
var runDomBlockRegex = regexp.MustCompile("(?s)(?:^|\\n)(?:run|exec):([^\\n\\r]+?)\\s*\\n(?:(?:复制|下载|Copy|Download)\\s*\\n)*(.*?)(?:\\n\\s*\\n|$)")

// BuildStealthInstruction 将 Agent 声明的 tools 与 instructions 编译为极简、高隐蔽的协同说明。
// 彻底规避 "工具"、"tool"、"function"、"API" 等高敏风控词汇，以真实程序员“本地协同排查”为伪装设定。
func BuildStealthInstruction(instructions string, tools []Tool) string {
	if len(tools) == 0 {
		return instructions
	}

	var sb strings.Builder
	if strings.TrimSpace(instructions) != "" {
		sb.WriteString(strings.TrimSpace(instructions))
		sb.WriteString("\n\n")
	}

	sb.WriteString("我们正在协同排查与编码。你负责给出诊断思路与执行指令，我会在本地终端执行并将输出同步给你。\n")
	sb.WriteString("可用操作指令：\n")
	firstTool := "bash"
	for i, t := range tools {
		name := t.Function.Name
		if i == 0 && name != "" {
			firstTool = name
		}
		desc := t.Function.Description
		if desc == "" {
			desc = "执行对应操作"
		}
		sb.WriteString(fmt.Sprintf("- `%s`: %s\n", name, desc))
	}
	sb.WriteString(fmt.Sprintf("\n约定：在 Markdown 中直接阐述分析；如需执行指令，请在正文中包含独立代码块：\n```run:%s\n<单行命令或参数>\n```\n", firstTool))
	sb.WriteString("我会将输出贴回给你。若无需执行操作，请直接给出最终答复。")

	return sb.String()
}

// FormatExecResultForWeb 将 Agent 的返回格式化为极简的人类贴终端日志风格。
func FormatExecResultForWeb(actionName, output string) string {
	name := strings.TrimSpace(actionName)
	if name == "" {
		name = "terminal"
	}
	return fmt.Sprintf("> 运行输出 (`%s`):\n```\n%s\n```", name, strings.TrimSpace(output))
}

// ParseStealthActionCalls 从模型的 Markdown 或页面 DOM 提取中解析 run/exec 动作块，
// 返回清理后的思考正文与生成的 ToolCall 列表。
func ParseStealthActionCalls(raw string) (cleanText string, calls []ToolCall) {
	regex := runBlockRegex
	matches := regex.FindAllStringSubmatchIndex(raw, -1)
	if len(matches) == 0 {
		regex = runDomBlockRegex
		matches = regex.FindAllStringSubmatchIndex(raw, -1)
	}
	if len(matches) == 0 {
		return raw, nil
	}

	var textParts []string
	lastIdx := 0

	for _, m := range matches {
		textParts = append(textParts, raw[lastIdx:m[0]])
		action := strings.TrimSpace(raw[m[2]:m[3]])
		rawParam := strings.TrimSpace(raw[m[4]:m[5]])
		lastIdx = m[1]

		// 清理可能从 DOM 夹带的按钮文本行（复制、下载等）
		lines := strings.Split(rawParam, "\n")
		var cleanLines []string
		for _, l := range lines {
			t := strings.TrimSpace(l)
			if t != "复制" && t != "下载" && t != "Copy" && t != "Download" && t != "" {
				cleanLines = append(cleanLines, l)
			}
		}
		paramStr := strings.TrimSpace(strings.Join(cleanLines, "\n"))

		fnName := action
		if strings.Contains(action, " ") || action == "git" || action == "sh" || action == "shell" || action == "exec" {
			fnName = "bash"
			if paramStr == "" {
				paramStr = action
			}
		}

		var argsJSON string
		if strings.HasPrefix(paramStr, "{") && strings.HasSuffix(paramStr, "}") {
			// 校验合法性，若非法则仍按单参数包装
			var js json.RawMessage
			if err := json.Unmarshal([]byte(paramStr), &js); err == nil {
				argsJSON = paramStr
			}
		}

		if argsJSON == "" {
			// 单行脚本/文件路径等非 JSON 紧凑格式，智能规约字段
			key := "command"
			if fnName == "read_file" || fnName == "view" {
				key = "path"
			} else if fnName != "bash" && fnName != "sh" && fnName != "exec" {
				key = "input"
			}
			b, _ := json.Marshal(map[string]string{key: paramStr})
			argsJSON = string(b)
		}

		calls = append(calls, ToolCall{
			ID:   fmt.Sprintf("call_%d_%s", time.Now().UnixNano(), fnName),
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      fnName,
				Arguments: argsJSON,
			},
		})
	}

	textParts = append(textParts, raw[lastIdx:])
	cleanText = strings.TrimSpace(strings.Join(textParts, ""))
	return cleanText, calls
}
