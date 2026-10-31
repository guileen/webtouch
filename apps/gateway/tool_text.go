package gateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 本文件处理「模型把工具调用写成文本」的常见形态（2026-10-03 派单 d8d0b0ac3）。
//
// 背景：web 通道用隐蔽 Markdown 约定（```run:<tool>```）承载工具调用，但模型经常不照约定，
// 把调用写成别的文本形态，网关此前只认隐蔽块 ⇒ 调用方拿到纯 content、`tool_calls` 为空
// （agent-hub 实测导航步命中率 2/8）。本文件把两类可判定的文本形态结构化回吐：
//  ① JSON：裸 schema 键对象（{"intent":…,"anchors":…}）、{"name":…,"arguments":…} 对象/数组，
//     兼容模型回显 "tools=[…]" 前缀；
//  ② DSML/XML：站点原生工具标记（<｜tool_calls｜><｜invoke name=…><｜parameter name=…>…），
//     DeepSeek 等在隐蔽模式下会把它当纯文本吐回。
//
// 只在「请求声明了 tools」时启用，且必须能绑定到某个声明过的工具（含 schema 键名校验），
// 避免把普通 JSON 正文误判成工具调用。

// ParseToolCallsFromText 是「响应文本 → 工具调用」的总入口：先按既有隐蔽 Markdown 块
// （run:/exec:）解析；未命中再按 JSON / DSML 两类文本形态解析。命中时把被识别的文本片段
// 从正文里摘除，使 content 只剩真正的自然语言。tools 为空（未声明工具）时退化为旧行为。
func ParseToolCallsFromText(raw string, tools []Tool) (cleanText string, calls []ToolCall) {
	clean, calls := ParseStealthActionCalls(raw)
	if len(calls) > 0 {
		// 隐蔽块的参数可能被写成自由文本（如 "intent=continue anchors=[…]"），按 schema 重整。
		return clean, normalizeFreeTextArgs(calls, tools)
	}
	if len(tools) == 0 {
		return raw, nil
	}
	if subs, cs, ok := extractJSONCalls(raw, tools); ok {
		return removeFragments(raw, subs), cs
	}
	if subs, cs, ok := extractDSMLCalls(raw, tools); ok {
		return removeFragments(raw, subs), cs
	}
	return raw, nil
}

func newToolCall(name, argsJSON string) ToolCall {
	var tc ToolCall
	tc.ID = fmt.Sprintf("call_%d_%s", time.Now().UnixNano(), name)
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = argsJSON
	return tc
}

func removeFragments(raw string, frags []string) string {
	out := raw
	for _, f := range frags {
		if f != "" {
			out = strings.Replace(out, f, "", 1)
		}
	}
	out = strings.TrimSpace(out)
	// 摘除后常见残留：模型把消息里的 "tools=[…] tool_choice=…" 头原样回显在调用前后，清掉。
	out = stripToolsEcho(out)
	out = strings.TrimPrefix(out, "tools=")
	out = strings.TrimPrefix(out, "tool_calls=")
	return strings.TrimSpace(out)
}

// stripToolsEcho 丢弃正文开头的工具声明回显行（tools=… / tool_calls=… / [<tool>] tool_choice=…）。
func stripToolsEcho(out string) string {
	for {
		line, rest, has := strings.Cut(out, "\n")
		t := strings.TrimSpace(line)
		if t == "" && has {
			out = rest
			continue
		}
		if isToolsEchoLine(t) {
			if !has {
				return ""
			}
			out = rest
			continue
		}
		return out
	}
}

func isToolsEchoLine(t string) bool {
	if strings.HasPrefix(t, "tools=") || strings.HasPrefix(t, "tool_calls=") {
		return true
	}
	return strings.HasPrefix(t, "[") && strings.Contains(t, "tool_choice=")
}

// ── 声明工具索引 ──

func declaredTool(tools []Tool, name string) (Tool, bool) {
	for _, t := range tools {
		if t.Function.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// toolPropNames 返回声明工具的参数键集合（来自 parameters.properties）。
func toolPropNames(t Tool) map[string]bool {
	props, ok := t.Function.Parameters["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return nil
	}
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}

// ── JSON 形态 ──

type jsonToolSpan struct {
	start, end int
	calls      []ToolCall
}

// extractJSONCalls 扫描正文里能绑定到声明工具的 JSON 片段（对象/数组/被 tools= 包裹的数组），
// 返回待摘除的原文片段与结构化调用。只接受有把握的形态，读不到就不动正文。
func extractJSONCalls(raw string, tools []Tool) (frags []string, calls []ToolCall, ok bool) {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '{' && raw[i] != '[' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw[i:]))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			continue
		}
		consumed := int(dec.InputOffset())
		end := i + consumed
		cs := callsFromJSONValue(v, tools)
		if len(cs) > 0 {
			frag := raw[i:end]
			// 一并摘掉紧邻的 "tools=" / "tool_calls=" 回显前缀。
			frag = extendPrefix(raw, i, frag)
			frags = append(frags, frag)
			calls = append(calls, cs...)
		}
		i = end - 1
	}
	return frags, calls, len(calls) > 0
}

// extendPrefix 把片段前紧邻的 tools=/tool_calls= 前缀并入待摘除文本。
func extendPrefix(raw string, start int, frag string) string {
	head := strings.TrimRight(raw[:start], " \t")
	for _, p := range []string{"tools=", "tool_calls="} {
		if strings.HasSuffix(head, p) {
			return raw[start-len(p) : start+len(frag)]
		}
	}
	return frag
}

func callsFromJSONValue(v any, tools []Tool) []ToolCall {
	switch t := v.(type) {
	case []any:
		var out []ToolCall
		for _, e := range t {
			out = append(out, callsFromJSONValue(e, tools)...)
		}
		return out
	case map[string]any:
		if inner, ok := t["tool_calls"]; ok {
			return callsFromJSONValue(inner, tools)
		}
		if fn, ok := t["function"]; ok {
			if m, ok := fn.(map[string]any); ok {
				return callsFromJSONValue(m, tools)
			}
		}
		if name, ok := t["name"].(string); ok && name != "" {
			if _, declared := declaredTool(tools, name); !declared {
				return nil
			}
			// 仅认 OpenAI 形态的 arguments；不认 parameters（那是工具定义回显，非调用）。
			if args, ok := t["arguments"]; ok {
				if b, ok := marshalArgs(args); ok {
					return []ToolCall{newToolCall(name, b)}
				}
			}
			return nil
		}
		// 裸 schema 键对象：所有键都在某个声明工具的 properties 里。
		if name, ok := matchSchemaTool(t, tools); ok {
			b, err := json.Marshal(t)
			if err != nil {
				return nil
			}
			return []ToolCall{newToolCall(name, string(b))}
		}
	}
	return nil
}

// matchSchemaTool 找出 properties 覆盖 obj 全部键的声明工具；多个候选时取覆盖键最多的。
func matchSchemaTool(obj map[string]any, tools []Tool) (string, bool) {
	best, bestN := "", 0
	for _, t := range tools {
		props := toolPropNames(t)
		if len(props) == 0 {
			continue
		}
		n := 0
		matched := true
		for k := range obj {
			if !props[k] {
				matched = false
				break
			}
			n++
		}
		if matched && n > bestN {
			best, bestN = t.Function.Name, n
		}
	}
	return best, bestN > 0
}

// marshalArgs 把 arguments 标准化为 JSON 字符串：既支持已是对象/数组，也支持被写成 JSON 字符串。
func marshalArgs(v any) (string, bool) {
	switch a := v.(type) {
	case string:
		var js any
		if err := json.Unmarshal([]byte(strings.TrimSpace(a)), &js); err != nil {
			// 非 JSON 的纯字符串参数也接受，按单值对象承载。
			b, _ := json.Marshal(map[string]string{"input": strings.TrimSpace(a)})
			return string(b), true
		}
		b, _ := json.Marshal(js)
		return string(b), true
	case nil:
		return "{}", true
	default:
		b, err := json.Marshal(a)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}

// ── DSML / XML 形态 ──

var (
	// 竖线变体：全角 ｜ / 半角 | / 两者混排，出现在尖括号内侧。
	barClass = "[｜|]*"
	// 一个 <tool_calls>…</tool_calls> 块；未闭合时匹配到正文结尾（模型常截断）。
	dsmlBlockRe = regexp.MustCompile(`(?s)<` + barClass + `\s*tool_calls\s*` + barClass + `>.*?(?:<` + barClass + `\s*/\s*tool_calls\s*` + barClass + `>|$)`)
	// <invoke name="X"> 后的块体（到下一个 invoke / 收尾）。
	dsmlInvokeRe = regexp.MustCompile(`(?is)<` + barClass + `\s*invoke\s+name\s*=\s*["']([^"']+)["'][^>]*>(.*?)(?:<` + barClass + `\s*/\s*invoke\s*` + barClass + `>|<` + barClass + `\s*invoke\s+name|$)`)
	// <parameter name="k">v</parameter>
	dsmlParamRe = regexp.MustCompile(`(?is)<` + barClass + `\s*parameter\s+name\s*=\s*["']([^"']+)["'][^>]*>(.*?)(?:<` + barClass + `\s*/\s*parameter\s*` + barClass + `>|<` + barClass + `\s*parameter\s+name|$)`)
)

func extractDSMLCalls(raw string, tools []Tool) (frags []string, calls []ToolCall, ok bool) {
	blocks := dsmlBlockRe.FindAllString(raw, -1)
	for _, blk := range blocks {
		for _, m := range dsmlInvokeRe.FindAllStringSubmatch(blk, -1) {
			name := strings.TrimSpace(m[1])
			if _, declared := declaredTool(tools, name); !declared {
				continue
			}
			args := map[string]any{}
			for _, pm := range dsmlParamRe.FindAllStringSubmatch(m[2], -1) {
				args[strings.TrimSpace(pm[1])] = coerceScalar(pm[2])
			}
			b, err := json.Marshal(args)
			if err != nil {
				continue
			}
			calls = append(calls, newToolCall(name, string(b)))
		}
		frags = append(frags, blk)
	}
	return frags, calls, len(calls) > 0
}

// coerceScalar 把 DSML 参数文本还原为 JSON 值：优先 JSON（数组/对象/引号字符串/数字/布尔），否则原样字符串。
func coerceScalar(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if s[0] == '[' || s[0] == '{' || s[0] == '"' {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			return v
		}
	}
	return s
}

// ── 隐蔽块自由文本参数重整 ──

// normalizeFreeTextArgs 把隐蔽块里被写成自由文本的参数按声明 schema 重整为结构化 JSON：
// 例 {"input":"intent=continue anchors=[\"x.md\"] reason=\"…\""} → {"intent":"continue","anchors":["x.md"],"reason":"…"}。
// 只在「单键对象 + 该工具声明了 properties + 能切出 ≥1 个声明键」时生效，否则原样返回。
func normalizeFreeTextArgs(calls []ToolCall, tools []Tool) []ToolCall {
	if len(tools) == 0 {
		return calls
	}
	for i := range calls {
		t, declared := declaredTool(tools, calls[i].Function.Name)
		if !declared {
			continue
		}
		props := toolPropNames(t)
		if len(props) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(calls[i].Function.Arguments), &m) != nil || len(m) != 1 {
			continue
		}
		free := ""
		for _, v := range m {
			if s, ok := v.(string); ok {
				free = s
			}
		}
		if strings.TrimSpace(free) == "" {
			continue
		}
		parsed, ok := parseKeyValueArgs(free, props)
		if !ok {
			continue
		}
		if b, err := json.Marshal(parsed); err == nil {
			calls[i].Function.Arguments = string(b)
		}
	}
	return calls
}

// parseKeyValueArgs 从 "k1=v1 k2=v2 …" 自由文本里切出声明过的键（值可为 JSON 或裸文本）。
func parseKeyValueArgs(free string, props map[string]bool) (map[string]any, bool) {
	type kv struct {
		key          string
		kstart, vstart int
	}
	var locs []kv
	for k := range props {
		idx := 0
		needle := k + "="
		for {
			j := strings.Index(free[idx:], needle)
			if j < 0 {
				break
			}
			pos := idx + j
			if pos == 0 || free[pos-1] == ' ' || free[pos-1] == '\n' || free[pos-1] == '\t' || free[pos-1] == ',' {
				locs = append(locs, kv{k, pos, pos + len(needle)})
			}
			idx = pos + len(needle)
		}
	}
	if len(locs) == 0 {
		return nil, false
	}
	sort.Slice(locs, func(a, b int) bool { return locs[a].kstart < locs[b].kstart })
	out := map[string]any{}
	for i, l := range locs {
		end := len(free)
		if i+1 < len(locs) {
			end = locs[i+1].kstart
		}
		val := strings.TrimSpace(free[l.vstart:end])
		val = strings.TrimRight(val, " ,;")
		out[l.key] = coerceScalar(val)
	}
	return out, len(out) > 0
}
