// Gemini 网页聊天 batchexecute NDJSON 流解析（2026-08-25 真实抓包）。
//
// gemini.google.com 的补全端点不是 SSE，而是 Google batchexecute RPC：
//
//	POST /_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate
//
// 响应 = anti-XSSI 前缀 `)]}'` + `<长度>\n<帧>` NDJSON 帧。
// 内容在 wrb.fr 帧的 rc_<candidate> 文本数组里；收尾 = 状态帧
// `{"44":true,"46":[...],"52":[]}` 或传输帧 `["e",...]`。
// 与 DeepSeek 补丁协议（deepseek_sse.go）分开收口，Sink 自动路由。
package modelbridge

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

// IsGeminiStreamURL 判定请求是否命中 gemini.google.com 的流式补全端点。
func IsGeminiStreamURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	return strings.Contains(p, "bardchataui/data/assistant") ||
		strings.HasSuffix(p, "/streamgenerate")
}

// feedGeminiLine 消费 Gemini batchexecute NDJSON 的一行：
//
//	)]}'                          ← anti-XSSI 前缀，忽略
//	<length>                      ← 帧字节长度，忽略（帧本身自成一行）
//	[["wrb.fr",null,"<inner>",...]] ← wrb.fr 业务帧（内容/状态）
//	[["di",...],["af.httprm",...]]  ← 传输帧，忽略
//	[["e",4,null,null,<len>]]       ← 传输结束帧
func (s *Sink) feedGeminiLine(line []byte) {
	t := bytes.TrimSpace(line)
	if len(t) == 0 || bytes.HasPrefix(t, []byte(")]}'")) || isAllDigits(t) {
		return
	}
	if t[0] != '[' {
		return // 未知行（心跳/注释）跳过
	}
	var frames [][]json.RawMessage
	if json.Unmarshal(t, &frames) != nil {
		return
	}
	for _, f := range frames {
		if len(f) == 0 {
			continue
		}
		var kind string
		if json.Unmarshal(f[0], &kind) != nil {
			continue
		}
		switch kind {
		case "wrb.fr":
			if len(f) >= 3 {
				var inner string
				if json.Unmarshal(f[2], &inner) == nil {
					s.ingestGeminiInner([]byte(inner))
				}
			}
		case "e":
			// 传输结束帧（最后一个 wrb.fr 状态帧之后）：标记流完成。
			s.markDone()
		}
	}
}

// isAllDigits 判断一行是否纯数字（NDJSON 长度前缀）。
func isAllDigits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ingestGeminiInner 处理 Gemini wrb.fr 帧的内层 JSON（反序列化后的字符串）：
//
//	[null, ["c_..","r_.."], <state>, null, [<fragments>], ...]
//	- state（idx 2）：{"44":true,"46":["c_..",""],"52":[]} 等收尾状态；
//	- fragments（idx 4）：[["rc_<candidate>",["文本","增量"],...], ...]
func (s *Sink) ingestGeminiInner(inner []byte) {
	var parts []json.RawMessage
	if json.Unmarshal(inner, &parts) != nil {
		return
	}
	// 站点自报会话 ID（契约 §9 取值来源②）：内层 index 1 = ["c_<16hex>","r_<16hex>"]，
	// 2026-10-01 真机抓包逐字确认。必须在下面 state["46"]/["52"] 的 markDone() 早退**之前**取，
	// 否则收尾状态帧会吞掉它（L0 直发不驱动 SPA 路由，页地址拿不到 ID）。
	if len(parts) >= 2 {
		var ids []json.RawMessage
		if json.Unmarshal(parts[1], &ids) == nil && len(ids) > 0 {
			var cid string
			if json.Unmarshal(ids[0], &cid) == nil {
				s.setConversationID(cid)
			}
		}
	}
	if len(parts) >= 3 {
		var state map[string]json.RawMessage
		if json.Unmarshal(parts[2], &state) == nil {
			// "46" = 下一轮 IDs / "52" = 后续帧队列（空数组 = 收尾）：
			// 出现即说明本轮回答已完成（最后状态帧）。
			if _, ok := state["46"]; ok {
				s.markDone()
				return
			}
			if raw, ok := state["52"]; ok && string(raw) == "[]" {
				s.markDone()
				return
			}
		}
	}
	if len(parts) < 5 {
		return
	}
	var frags [][]json.RawMessage
	if json.Unmarshal(parts[4], &frags) != nil {
		return
	}
	for _, f := range frags {
		if len(f) < 2 {
			continue
		}
		var candID string
		if json.Unmarshal(f[0], &candID) != nil || !strings.HasPrefix(candID, "rc_") {
			continue
		}
		var texts []string
		if json.Unmarshal(f[1], &texts) != nil {
			continue
		}
		total := strings.Join(texts, "")
		if s.geminiText == nil {
			s.geminiText = map[string]string{}
		}
		prev := s.geminiText[candID]
		if total == prev {
			continue // 同一帧重复下发（实测有重复 chunk），无增量不重复消费
		}
		s.geminiText[candID] = total
		delta := total
		if strings.HasPrefix(total, prev) {
			delta = total[len(prev):]
		}
		if delta == "" {
			continue
		}
		s.content.WriteString(delta)
		s.append(Chunk{Content: delta})
	}
}
