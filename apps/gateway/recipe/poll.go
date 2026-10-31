package recipe

import (
	"encoding/json"
	"strings"
)

// 轮询补正文（response.kind=sse_handoff_poll）的回传标记。
//
// 背景（契约 §13，2026-10-02 ChatGPT 真机实测）：主请求的 SSE 只给「前奏」
// （到 stream_handoff 为止，答案正文不在其中），正文由站点 WebSocket conduit 投递。
// 直发不订阅该 WS，而是按 recipe 声明轮询会话对象把正文读回来；页内执行器把
// 轮询到的正文以**单条 JSON 行**塞进流队列，与协议帧（SSE 原文）明确区分。
const (
	pollAnswerKey = "__uws_poll_answer"
	pollErrorKey  = "__uws_poll_error"
)

// IsPollResponse 判定该 recipe 的正文是否走「轮询会话对象」而非主流。
func IsPollResponse(r *Recipe) bool {
	return r != nil && r.Response.Kind == ResponseKindSSEHandoffPoll
}

func pollFieldFromChunk(raw, key string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") || !strings.Contains(trimmed, key) {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return "", false
	}
	v, ok := m[key].(string)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}

// PollAnswerFromChunk 判定一条流块是否为「轮询到的正文」，是则返回正文。
func PollAnswerFromChunk(raw string) (string, bool) {
	return pollFieldFromChunk(raw, pollAnswerKey)
}

// PollErrorFromChunk 判定一条流块是否为「轮询失败」，是则返回错误文本。
func PollErrorFromChunk(raw string) (string, bool) {
	return pollFieldFromChunk(raw, pollErrorKey)
}
