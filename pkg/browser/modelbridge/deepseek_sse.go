// DeepSeek 网页聊天 SSE 补丁式协议解析（2026-08-25 真实抓包）。
//
// 与通用 SSE 解析（sse.go）分离：DeepSeek 新版流把回答经
// 「初始快照 + 显式 APPEND + 隐式 {"v":...} 增量 + FINISHED」下发，
// 补丁路径语义与 Gemini NDJSON（gemini_ndjson.go）完全不同，
// 站点协议差异各自收口在独立文件，Sink 按格式自动路由。
package modelbridge

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ssePatch 是 DeepSeek Web 新版 SSE 的补丁式事件：
//   - 初始快照：{"v":{"response":{"fragments":[{"id":2,"type":"RESPONSE","content":"```"}]}}}
//     （首个正文块随快照下发，之后全部走 APPEND 增量）
//   - 正文增量：{"p":"response/fragments/-1/content","o":"APPEND","v":"mark"}
//     （-1 = 最后一个 fragment；思考/正文是不同 fragment，靠 type 分流）
//   - 收尾：{"p":"response/status","o":"SET","v":"FINISHED"} 或
//     {"p":"response","o":"BATCH","v":[{"p":"quasi_status","v":"FINISHED"}, ...]}
type ssePatch struct {
	P string          `json:"p"`
	O string          `json:"o"`
	V json.RawMessage `json:"v"`
}

// sseFragment 是补丁协议中的一个内容片段（思考链或正文各一个）。
type sseFragment struct {
	id      int
	typ     string
	content string
}

// ingestPatch 识别并消费 DeepSeek 新版补丁协议事件；返回 true 表示已按新协议
// 处理（不再落入旧格式解析）。旧格式/无关事件返回 false。
func (s *Sink) ingestPatch(data string) bool {
	var p ssePatch
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return false
	}
	if p.P == "" && len(p.V) > 0 {
		// 初始快照形态：{"v":{"response":{"fragments":[...]}}}（无 p 字段）。
		var snap struct {
			Response struct {
				Fragments []struct {
					ID      int    `json:"id"`
					Type    string `json:"type"`
					Content string `json:"content"`
				} `json:"fragments"`
			} `json:"response"`
		}
		if json.Unmarshal(p.V, &snap) == nil && len(snap.Response.Fragments) > 0 {
			s.seedFragments(snap.Response.Fragments)
			return true
		}
		// 隐式增量形态（真实 DeepSeek 协议 2026-08-25 抓包）：首个补丁确立
		// 路径后，后续增量省略 p/o，仅 {"v":"<text>"} 沿上一补丁路径继续 APPEND。
		if p.V[0] == '"' {
			var text string
			if json.Unmarshal(p.V, &text) == nil {
				target := -1
				if s.lastFragSet {
					target = s.lastFrag
				}
				s.appendFragmentContent(target, text)
				return true
			}
		}
	}
	if p.P == "" {
		return false
	}
	switch {
	case p.P == "response/status" && p.O == "SET":
		var st string
		if json.Unmarshal(p.V, &st) == nil && strings.EqualFold(st, "FINISHED") {
			s.markDone()
		}
		return true
	case p.P == "response" && p.O == "BATCH":
		s.markDoneFromBatch(p.V)
		return true
	case strings.HasPrefix(p.P, "response/fragments/") &&
		strings.HasSuffix(p.P, "/content") && p.O == "APPEND":
		var text string
		if json.Unmarshal(p.V, &text) == nil {
			s.appendFragmentContent(fragmentIDFromPath(p.P), text)
			s.lastFrag = fragmentIDFromPath(p.P)
			s.lastFragSet = true
		}
		return true
	default:
		// 其它补丁路径（fragment type/insert 等）已按新协议消费，不落旧解析。
		return true
	}
}

// seedFragments 用初始快照建立片段表（首个正文块随快照下发）。
func (s *Sink) seedFragments(frags []struct {
	ID      int    `json:"id"`
	Type    string `json:"type"`
	Content string `json:"content"`
}) {
	for _, f := range frags {
		if _, ok := s.fragIdx[f.ID]; ok {
			continue
		}
		b := s.builderForType(f.Type)
		if f.Content != "" {
			b.WriteString(f.Content)
			// 首个正文块随快照下发（DeepSeek 常见形态，如整段短答只在快照里）：
			// 必须同时产出一个增量块，否则只读 Drain() 的消费方（网关直发路径）会看到空回答。
			if isThinkingType(f.Type) {
				s.append(Chunk{Reasoning: f.Content})
			} else {
				s.append(Chunk{Content: f.Content})
			}
		}
		if s.fragIdx == nil {
			s.fragIdx = map[int]int{}
		}
		s.fragIdx[f.ID] = len(s.frags)
		s.frags = append(s.frags, sseFragment{id: f.ID, typ: f.Type, content: f.Content})
	}
}

// appendFragmentContent 把 APPEND 增量追加到指定片段（-1 = 最后一个）并同步
// 到 content/reasoning 快照与增量块。
func (s *Sink) appendFragmentContent(id int, text string) {
	idx := s.fragIndex(id)
	if idx < 0 {
		// 无种子片段：按 RESPONSE 兜底创建（极端时序：-1 增量先于快照到达）。
		s.frags = append(s.frags, sseFragment{id: id, typ: "RESPONSE"})
		idx = len(s.frags) - 1
	}
	f := &s.frags[idx]
	f.content += text
	b := s.builderForType(f.typ)
	b.WriteString(text)
	if isThinkingType(f.typ) {
		s.append(Chunk{Reasoning: text})
	} else {
		s.append(Chunk{Content: text})
	}
}

func (s *Sink) fragIndex(id int) int {
	if id == -1 {
		if len(s.frags) == 0 {
			return -1
		}
		return len(s.frags) - 1
	}
	if i, ok := s.fragIdx[id]; ok {
		return i
	}
	return -1
}

// builderForType 按片段类型路由到 content / reasoning 累计器。
func (s *Sink) builderForType(typ string) *strings.Builder {
	if isThinkingType(typ) {
		return &s.reasoning
	}
	return &s.content
}

func isThinkingType(typ string) bool {
	switch strings.ToUpper(strings.TrimSpace(typ)) {
	case "THINKING", "REASONING", "REASON", "THINK":
		return true
	default:
		return false
	}
}

// fragmentIDFromPath 解析 response/fragments/<id>/content 中的 id（-1 = 最后）。
func fragmentIDFromPath(p string) int {
	rest := strings.TrimPrefix(p, "response/fragments/")
	rest = strings.TrimSuffix(rest, "/content")
	id, err := strconv.Atoi(rest)
	if err != nil {
		return -1
	}
	return id
}

// markDoneFromBatch 处理 BATCH 补丁里的 quasi_status=FINISHED。
func (s *Sink) markDoneFromBatch(v json.RawMessage) {
	var entries []struct {
		P string          `json:"p"`
		V json.RawMessage `json:"v"`
	}
	if json.Unmarshal(v, &entries) != nil {
		return
	}
	for _, e := range entries {
		if e.P != "quasi_status" {
			continue
		}
		var st string
		if json.Unmarshal(e.V, &st) == nil && strings.EqualFold(st, "FINISHED") {
			s.markDone()
			return
		}
	}
}
