package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// handleChat 处理 Agent Hub 私有 provider chat 契约（forcedChannel 为空时按 model/channel_id 路由）。
func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request, forcedChannel string) {
	if !h.browserProviderReady(w, r) {
		return
	}
	req, err := h.parseChatRequest(r, forcedChannel)
	if err != nil {
		writeChatError(w, err)
		return
	}
	if req.Stream {
		h.streamChat(w, r, req)
		return
	}
	out, err := h.g.Chat(r.Context(), req)
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// parseChatRequest 解析 OpenAI 兼容请求体 + x-uws-* 头 + 通道/模型路由。
func (h *Handler) parseChatRequest(r *http.Request, forcedChannel string) (ChatRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return ChatRequest{}, &BadRequestError{Reason: "读取请求体失败: " + err.Error()}
	}
	var raw map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			return ChatRequest{}, &BadRequestError{Reason: "invalid json"}
		}
	}
	if raw == nil {
		return ChatRequest{}, &BadRequestError{Reason: "body 必须为 JSON 对象"}
	}
	messagesRaw, ok := raw["messages"].([]any)
	if !ok || len(messagesRaw) == 0 {
		return ChatRequest{}, &BadRequestError{Reason: "messages 必须为非空数组"}
	}
	req := ChatRequest{
		ChannelID:  forcedChannel,
		Model:      jsonString(raw["model"]),
		Stream:     jsonBool(raw["stream"]),
		SessionID:  jsonString(raw["session_id"]),
		NewSession: jsonBool(raw["new_session"]),
		CloseAfter: jsonBool(raw["close_after"]),
		// 站点会话身份（契约 §9 S2）：命名禁令——不得复用 conversation_id（那是网关会话键）。
		SiteConversationID: jsonString(raw["site_conversation_id"]),
	}
	if req.SessionID == "" {
		req.SessionID = jsonString(raw["conversation_id"])
	}
	if req.SessionID == "" {
		req.SessionID = DefaultSessionID
	}
	if b, ok := flexBool(raw["thinking"]); ok {
		req.Thinking = &b
	}
	if b, ok := flexBool(raw["search"]); ok {
		req.Search = &b
	}
	// x-uws-* 头覆盖 body 扩展字段（契约 §4）。
	if v := r.Header.Get("x-uws-session-id"); v != "" {
		req.SessionID = v
	}
	if v := r.Header.Get("x-uws-conversation-id"); v != "" {
		req.SessionID = v
	}
	if v := r.Header.Get("x-uws-new-session"); v != "" {
		if b, ok := parseBoolString(v); ok {
			req.NewSession = b
		}
	}
	if v := r.Header.Get("x-uws-site-conversation-id"); v != "" {
		req.SiteConversationID = v
	}
	if v := r.Header.Get("x-uws-thinking"); v != "" {
		if b, ok := parseBoolString(v); ok {
			req.Thinking = &b
		}
	}
	if v := r.Header.Get("x-uws-search"); v != "" {
		if b, ok := parseBoolString(v); ok {
			req.Search = &b
		}
	}
	if v := r.Header.Get("x-uws-close-after"); v != "" {
		if b, ok := parseBoolString(v); ok {
			req.CloseAfter = b
		}
	}
	// temperature / max_tokens：web 通道不支持，忽略并记录（契约 §8）。
	if _, ok := raw["temperature"]; ok {
		h.log.Printf("忽略 temperature（web 通道不支持）")
	}
	if _, ok := raw["max_tokens"]; ok {
		h.log.Printf("忽略 max_tokens（web 通道不支持）")
	}
	ch, err := h.g.resolveChannel(req.ChannelID, req.Model)
	if err != nil {
		return ChatRequest{}, err
	}
	if ch.Type != ChannelTypeWeb {
		return ChatRequest{}, &NotImplementedError{
			Reason: fmt.Sprintf("通道 %q 类型 %q 尚未实现（M1 仅 uws_web）", ch.ID, ch.Type),
		}
	}
	req.ChannelID = ch.ID
	// 解析 tools 与 instructions
	if toolsRaw, ok := raw["tools"].([]any); ok {
		for _, t := range toolsRaw {
			m, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fnRaw, _ := m["function"].(map[string]any)
			if fnRaw == nil {
				fnRaw = m
			}
			params, _ := fnRaw["parameters"].(map[string]any)
			req.Tools = append(req.Tools, Tool{
				Type: "function",
				Function: ToolFunction{
					Name:        jsonString(fnRaw["name"]),
					Description: jsonString(fnRaw["description"]),
					Parameters:  params,
				},
			})
		}
	}
	req.ToolChoice = raw["tool_choice"]
	req.Instructions = jsonString(raw["instructions"])

	for _, m := range messagesRaw {
		obj, ok := m.(map[string]any)
		if !ok {
			continue
		}
		req.Messages = append(req.Messages, Message{
			Role:       jsonString(obj["role"]),
			Content:    jsonString(obj["content"]),
			Name:       jsonString(obj["name"]),
			ToolCallID: jsonString(obj["tool_call_id"]),
		})
	}
	return req, nil
}

// streamChat 把 ChatStream 事件序列化为 SSE（data: {...}\n\n … data: [DONE]）。
func (h *Handler) streamChat(w http.ResponseWriter, r *http.Request, req ChatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported", "internal_error", "")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	events, err := h.g.ChatStream(r.Context(), req)
	if err != nil {
		writeSSE(w, map[string]any{
			"error": chatErrorObject(err),
		})
		return
	}
	for ev := range events {
		if ev.Err != nil {
			writeSSE(w, map[string]any{
				"error": chatErrorObject(ev.Err),
			})
			return
		}
		delta := map[string]any{}
		if ev.Role {
			delta["role"] = "assistant"
		}
		if ev.ReasoningDelta != "" {
			delta["reasoning_content"] = ev.ReasoningDelta
		}
		if ev.Delta != "" {
			delta["content"] = ev.Delta
		}
		if len(ev.ToolCalls) > 0 {
			var tcList []map[string]any
			for i, tc := range ev.ToolCalls {
				tcList = append(tcList, map[string]any{
					"index": i,
					"id":    tc.ID,
					"type":  tc.Type,
					"function": map[string]any{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				})
			}
			delta["tool_calls"] = tcList
		}
		if ev.Transport != "" {
			delta["transport"] = ev.Transport
		}
		if ev.FinishReason != "" {
			// 站点会话身份只在完成帧回填（契约 §9 S1）；读不到时为 JSON null，不伪造。
			delta["site_conversation_id"] = ev.SiteConversationID
			delta["page_url"] = ev.PageURL
		}
		var finishReason any
		if ev.FinishReason != "" {
			finishReason = ev.FinishReason
		}
		event := map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			}},
		}
		writeSSE(w, event)
		flusher.Flush()
	}
	writeSSE(w, []byte("data: [DONE]\n\n"))
}

// handleResponses 处理 Agent Hub 私有 Responses provider 契约。
func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request, forcedChannel string) {
	if !h.browserProviderReady(w, r) {
		return
	}
	chatReq, respReq, err := h.parseResponseRequest(r, forcedChannel)
	if err != nil {
		writeChatError(w, err)
		return
	}
	if respReq.Stream {
		h.streamResponses(w, r, chatReq, respReq)
		return
	}
	out, err := h.g.Chat(r.Context(), chatReq)
	if err != nil {
		writeChatError(w, err)
		return
	}
	respObj := ConvertCompletionToResponseObject(out)
	writeJSON(w, http.StatusOK, respObj)
}

// streamResponses 将流式聊天序列化为 OpenAI Responses API 规范的 SSE 事件流。
func (h *Handler) streamResponses(w http.ResponseWriter, r *http.Request, chatReq ChatRequest, respReq ResponseRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported", "internal_error", "")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	respID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	// 1. response.created
	writeSSEEvent(w, "response.created", map[string]any{
		"response": map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"model":      respReq.Model,
			"status":     "in_progress",
		},
	})
	flusher.Flush()

	events, err := h.g.ChatStream(r.Context(), chatReq)
	if err != nil {
		writeSSEEvent(w, "error", chatErrorObject(err))
		return
	}

	msgAdded := false
	var fullText strings.Builder
	var lastToolCalls []ToolCall
	// 站点会话身份只随完成帧出现（契约 §9 字段级冻结），在 response.completed 里回传。
	var finishConvID, finishPageURL *string

	for ev := range events {
		if ev.Err != nil {
			writeSSEEvent(w, "error", chatErrorObject(ev.Err))
			return
		}

		if ev.Delta != "" {
			fullText.WriteString(ev.Delta)
			if !msgAdded {
				writeSSEEvent(w, "response.output_item.added", map[string]any{
					"output_index": 0,
					"item": map[string]any{
						"id":      msgID,
						"type":    "message",
						"role":    "assistant",
						"content": []any{},
					},
				})
				writeSSEEvent(w, "response.content_part.added", map[string]any{
					"output_index":  0,
					"content_index": 0,
					"part": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				msgAdded = true
			}
			writeSSEEvent(w, "response.output_text.delta", map[string]any{
				"output_index":  0,
				"content_index": 0,
				"delta":         ev.Delta,
			})
			flusher.Flush()
		}

		if len(ev.ToolCalls) > 0 {
			lastToolCalls = ev.ToolCalls
		}
		if ev.FinishReason != "" {
			finishConvID, finishPageURL = ev.SiteConversationID, ev.PageURL
		}
	}

	cleanText, calls := ParseToolCallsFromText(fullText.String(), chatReq.Tools)
	if len(calls) > 0 {
		lastToolCalls = calls
	}

	if msgAdded {
		writeSSEEvent(w, "response.output_item.done", map[string]any{
			"output_index": 0,
			"item": map[string]any{
				"id":   msgID,
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": cleanText,
					},
				},
			},
		})
		flusher.Flush()
	}

	// 如果有识别到的动作调用，触发 function_call 事件
	for i, tc := range lastToolCalls {
		outIdx := 0
		if msgAdded {
			outIdx = i + 1
		} else {
			outIdx = i
		}
		fcID := fmt.Sprintf("fc_%d_%d", time.Now().UnixNano(), i)
		writeSSEEvent(w, "response.output_item.added", map[string]any{
			"output_index": outIdx,
			"item": map[string]any{
				"id":        fcID,
				"type":      "function_call",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": "",
			},
		})
		writeSSEEvent(w, "response.function_call_arguments.delta", map[string]any{
			"output_index": outIdx,
			"call_id":      tc.ID,
			"delta":        tc.Function.Arguments,
		})
		writeSSEEvent(w, "response.output_item.done", map[string]any{
			"output_index": outIdx,
			"item": map[string]any{
				"id":        fcID,
				"type":      "function_call",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		})
		flusher.Flush()
	}

	// 最终 response.completed 事件
	writeSSEEvent(w, "response.completed", map[string]any{
		"response": map[string]any{
			"id":                   respID,
			"object":               "response",
			"status":               "completed",
			"site_conversation_id": finishConvID,
			"page_url":             finishPageURL,
		},
	})
	flusher.Flush()
}

func writeSSEEvent(w http.ResponseWriter, event string, payload any) {
	data, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(data))
}
