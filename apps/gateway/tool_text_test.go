package gateway

import (
	"encoding/json"
	"testing"
)

// navTool 是导航步在 agent-hub 侧的声明（2026-10-03 派单 d8d0b0ac3 真实 schema）。
func navTool() []Tool {
	return []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name: "emit_navigation",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"intent":  map[string]any{"type": "string"},
					"anchors": map[string]any{"type": "array"},
					"reason":  map[string]any{"type": "string"},
				},
				"required": []any{"intent", "anchors"},
			},
		},
	}}
}

func argsOf(t *testing.T, calls []ToolCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &m); err != nil {
		t.Fatalf("arguments 不是 JSON：%q（%v）", calls[0].Function.Arguments, err)
	}
	return m
}

// TestParseToolCallsFromText_JSON 覆盖基线里占比最高的形态：模型把调用写成 JSON 文本。
func TestParseToolCallsFromText_JSON(t *testing.T) {
	tools := navTool()
	anchor := `["labs/world-agent/agent-hub/docs/orchestration/model-roles-and-flow.md"]`
	cases := map[string]string{
		"裸 schema 键对象":  `{"intent":"continue","anchors":` + anchor + `,"reason":"同一话题"}`,
		"tools= 前缀 + JSON": "tools=[emit_navigation] tool_choice=required session=ahub-deepseek\n\n" + `{"intent":"continue","anchors":` + anchor + `,"reason":"延续话题"}`,
		"name+arguments 数组": `tools=[{"name":"emit_navigation","arguments":{"intent":"continue","anchors":` + anchor + `,"reason":"x"}}]`,
		"tool_calls 包裹":    `{"tool_calls":[{"name":"emit_navigation","arguments":{"intent":"continue","anchors":` + anchor + `}}]}`,
	}
	for name, raw := range cases {
		clean, calls := ParseToolCallsFromText(raw, tools)
		if len(calls) != 1 {
			t.Fatalf("%s：calls=%d，期望 1（clean=%q）", name, len(calls), clean)
		}
		if calls[0].Function.Name != "emit_navigation" {
			t.Fatalf("%s：name=%s", name, calls[0].Function.Name)
		}
		args := argsOf(t, calls)
		if args["intent"] != "continue" {
			t.Fatalf("%s：intent=%v", name, args["intent"])
		}
		anchors, ok := args["anchors"].([]any)
		if !ok || len(anchors) != 1 {
			t.Fatalf("%s：anchors=%v", name, args["anchors"])
		}
		if clean != "" {
			t.Fatalf("%s：命中后 content 应为空，实际 %q", name, clean)
		}
	}
}

// TestParseToolCallsFromText_DSML 覆盖站点原生 DSML/XML 形态（竖线变体 + 截断未闭合）。
func TestParseToolCallsFromText_DSML(t *testing.T) {
	tools := navTool()
	full := `<｜｜tool_calls｜｜><｜｜invoke name="emit_navigation"｜｜>` +
		`<｜｜parameter name="intent"｜｜>continue<｜｜/parameter｜｜>` +
		`<｜｜parameter name="anchors"｜｜>["labs/world-agent/agent-hub/docs/orchestration/model-roles-and-flow.md"]<｜｜/parameter｜｜>` +
		`<｜｜/invoke｜｜><｜｜/tool_calls｜｜>`
	clean, calls := ParseToolCallsFromText(full, tools)
	if len(calls) != 1 {
		t.Fatalf("calls=%d，期望 1（clean=%q）", len(calls), clean)
	}
	args := argsOf(t, calls)
	if args["intent"] != "continue" {
		t.Fatalf("intent=%v", args["intent"])
	}
	if _, ok := args["anchors"].([]any); !ok {
		t.Fatalf("anchors 未还原为数组：%v", args["anchors"])
	}
	if clean != "" {
		t.Fatalf("DSML 命中后 content 应为空，实际 %q", clean)
	}

	// 半角竖线 + 未闭合（模型常截断）
	trunc := `<|tool_calls|><|invoke name="emit_navigation"|><|parameter name="intent"|>continue<|/parameter|>`
	if _, cs := ParseToolCallsFromText(trunc, tools); len(cs) != 1 {
		t.Fatalf("半角/未闭合 DSML 应命中，实际 calls=%d", len(cs))
	}
}

// TestParseToolCallsFromText_StealthFreeTextArgs 隐蔽块参数被写成 k=v 自由文本时按 schema 重整
// （基线 run 1 返回 {"input":…}，调用方判 intent_invalid）。
func TestParseToolCallsFromText_StealthFreeTextArgs(t *testing.T) {
	raw := "```run:emit_navigation\nintent=continue anchors=[\"labs/world-agent/agent-hub/docs/orchestration/model-roles-and-flow.md\"] reason=\"用户要求接着展开同一份设计文档的 §五\"\n```"
	_, calls := ParseToolCallsFromText(raw, navTool())
	if len(calls) != 1 {
		t.Fatalf("calls=%d，期望 1", len(calls))
	}
	args := argsOf(t, calls)
	if args["intent"] != "continue" || args["reason"] == nil {
		t.Fatalf("自由文本未按 schema 重整：%v", args)
	}
	if _, bad := args["input"]; bad {
		t.Fatalf("不得残留自造键 input：%v", args)
	}
	if anchors, ok := args["anchors"].([]any); !ok || len(anchors) != 1 {
		t.Fatalf("anchors=%v", args["anchors"])
	}
}

// TestParseToolCallsFromText_NoFalsePositives 守住不误判：非调用文本、schema 回显、缺 arguments 一律不动。
func TestParseToolCallsFromText_NoFalsePositives(t *testing.T) {
	tools := navTool()
	cases := map[string]string{
		"纯头回显（无调用）":     "tools=[emit_navigation] tool_choice=required session=ahub-deepseek",
		"schema 定义回显":    `tools=[{"name":"emit_navigation","description":"判断话题","parameters":{"type":"object","properties":{"intent":{"type":"string"}}}}]`,
		"无关 JSON":         `{"foo":"bar","n":1}`,
		"普通自然语言":         "这份设计文档的 §五 讲的是 emoji 映射，建议先补一张映射表。",
		"未声明工具的同形 JSON": `{"intent":"continue"}`, // tools 为空时不得解析
	}
	for name, raw := range cases {
		ts := tools
		if name == "未声明工具的同形 JSON" {
			ts = nil
		}
		clean, calls := ParseToolCallsFromText(raw, ts)
		if len(calls) != 0 {
			t.Fatalf("%s：误判为工具调用 %v", name, calls)
		}
		if clean != raw {
			t.Fatalf("%s：正文被改动 %q → %q", name, raw, clean)
		}
	}
}

// TestParseToolCallsFromText_FallsBackToStealthBlock 既有隐蔽块解析不得回归。
func TestParseToolCallsFromText_FallsBackToStealthBlock(t *testing.T) {
	raw := "先看日志。\n```run:bash\ndf -h\n```\n然后回报。"
	clean, calls := ParseToolCallsFromText(raw, []Tool{{Type: "function", Function: ToolFunction{Name: "bash"}}})
	if len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("calls=%v", calls)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &m); err != nil || m["command"] != "df -h" {
		t.Fatalf("args=%s", calls[0].Function.Arguments)
	}
	if clean != "先看日志。\n\n然后回报。" {
		t.Fatalf("clean=%q", clean)
	}
}
