package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ResponseRequest 是 Agent Hub 内部浏览器 provider 使用的 Responses 请求体。
type ResponseRequest struct {
	Model        string   `json:"model"`
	Instructions string   `json:"instructions,omitempty"`
	Input        any      `json:"input"` // string 或 []map[string]any
	Tools        []Tool   `json:"tools,omitempty"`
	ToolChoice   any      `json:"tool_choice,omitempty"`
	Stream       bool     `json:"stream,omitempty"`
	Temperature  *float64 `json:"temperature,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	NewSession   bool     `json:"new_session,omitempty"`
	ChannelID    string   `json:"channel_id,omitempty"`
}

// ContentPart 是 message 中的内容片段。
type ContentPart struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// OutputItem 是 Responses API output 数组条目。
type OutputItem struct {
	Type      string        `json:"type"` // "message" 或 "function_call"
	ID        string        `json:"id"`
	Role      string        `json:"role,omitempty"`
	Content   []ContentPart `json:"content,omitempty"`
	CallID    string        `json:"call_id,omitempty"`
	Name      string        `json:"name,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
}

// ResponseObject 是 Responses API 非流式响应体。
type ResponseObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"` // "response"
	CreatedAt int64  `json:"created_at"`
	Model     string `json:"model"`
	Status    string `json:"status"` // "completed" | "in_progress" | "failed"
	SessionID string `json:"session_id,omitempty"`
	TargetID  string `json:"target_id,omitempty"`
	// 站点会话身份（契约 §9「站点会话身份」字段级冻结）：read 不到为 null，恒定出现、不省略。
	SiteConversationID *string      `json:"site_conversation_id"`
	PageURL            *string      `json:"page_url"`
	Output             []OutputItem `json:"output"`
	Usage              struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// ParseResponseRequest 从 HTTP 请求解析 ResponseRequest。
func (h *Handler) parseResponseRequest(r *http.Request, forcedChannel string) (ChatRequest, ResponseRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return ChatRequest{}, ResponseRequest{}, &BadRequestError{Reason: "读取请求体失败: " + err.Error()}
	}
	var raw map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			return ChatRequest{}, ResponseRequest{}, &BadRequestError{Reason: "invalid json"}
		}
	}
	if raw == nil {
		return ChatRequest{}, ResponseRequest{}, &BadRequestError{Reason: "body 必须为 JSON 对象"}
	}

	respReq := ResponseRequest{
		ChannelID:    forcedChannel,
		Model:        jsonString(raw["model"]),
		Instructions: jsonString(raw["instructions"]),
		Input:        raw["input"],
		Stream:       jsonBool(raw["stream"]),
		SessionID:    jsonString(raw["session_id"]),
		NewSession:   jsonBool(raw["new_session"]),
	}

	if respReq.SessionID == "" {
		respReq.SessionID = jsonString(raw["conversation_id"])
	}
	if respReq.SessionID == "" {
		respReq.SessionID = DefaultSessionID
	}
	if v := r.Header.Get("x-uws-session-id"); v != "" {
		respReq.SessionID = v
	}
	if v := r.Header.Get("x-uws-conversation-id"); v != "" {
		respReq.SessionID = v
	}
	if v := r.Header.Get("x-uws-new-session"); v != "" {
		if b, ok := parseBoolString(v); ok {
			respReq.NewSession = b
		}
	}

	ch, err := h.g.resolveChannel(respReq.ChannelID, respReq.Model)
	if err != nil {
		return ChatRequest{}, respReq, err
	}
	if ch.Type != ChannelTypeWeb {
		return ChatRequest{}, respReq, &NotImplementedError{
			Reason: fmt.Sprintf("通道 %q 类型 %q 尚未实现（仅 uws_web）", ch.ID, ch.Type),
		}
	}
	respReq.ChannelID = ch.ID

	// 解析 tools
	if toolsRaw, ok := raw["tools"].([]any); ok {
		for _, t := range toolsRaw {
			m, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fnRaw, _ := m["function"].(map[string]any)
			if fnRaw == nil {
				fnRaw = m // 兼容 name 直接在顶层的 schema
			}
			params, _ := fnRaw["parameters"].(map[string]any)
			respReq.Tools = append(respReq.Tools, Tool{
				Type: "function",
				Function: ToolFunction{
					Name:        jsonString(fnRaw["name"]),
					Description: jsonString(fnRaw["description"]),
					Parameters:  params,
				},
			})
		}
	}

	// 转换为 Canonical ChatRequest
	chatReq := ChatRequest{
		ChannelID:    respReq.ChannelID,
		Model:        respReq.Model,
		Instructions: respReq.Instructions,
		Tools:        respReq.Tools,
		ToolChoice:   respReq.ToolChoice,
		Stream:       respReq.Stream,
		SessionID:    respReq.SessionID,
		NewSession:   respReq.NewSession,
	}

	// 解析 input（支持 string 或 []item）
	switch in := respReq.Input.(type) {
	case string:
		if strings.TrimSpace(in) == "" {
			return ChatRequest{}, respReq, &BadRequestError{Reason: "input 文本不可为空"}
		}
		chatReq.Messages = append(chatReq.Messages, Message{Role: "user", Content: in})
	case []any:
		for _, itemRaw := range in {
			item, ok := itemRaw.(map[string]any)
			if !ok {
				continue
			}
			typ := jsonString(item["type"])
			switch typ {
			case "function_call_output":
				callID := jsonString(item["call_id"])
				output := jsonString(item["output"])
				chatReq.Messages = append(chatReq.Messages, Message{
					Role:       "tool",
					Name:       jsonString(item["name"]),
					ToolCallID: callID,
					Content:    output,
				})
			case "message", "":
				role := jsonString(item["role"])
				if role == "" {
					role = "user"
				}
				var textContent string
				switch cnt := item["content"].(type) {
				case string:
					textContent = cnt
				case []any:
					for _, partRaw := range cnt {
						if pMap, ok := partRaw.(map[string]any); ok {
							if jsonString(pMap["type"]) == "text" {
								textContent += jsonString(pMap["text"])
							}
						}
					}
				}
				chatReq.Messages = append(chatReq.Messages, Message{
					Role:    role,
					Content: textContent,
				})
			}
		}
	default:
		return ChatRequest{}, respReq, &BadRequestError{Reason: "input 必须为字符串或对象数组"}
	}

	if len(chatReq.Messages) == 0 {
		return ChatRequest{}, respReq, &BadRequestError{Reason: "input 中未包含有效的对话内容"}
	}

	return chatReq, respReq, nil
}

// ConvertCompletionToResponseObject 将网关 ChatCompletion 包装为 OpenAI ResponseObject。
func ConvertCompletionToResponseObject(comp *ChatCompletion) *ResponseObject {
	resp := &ResponseObject{
		ID:                 fmt.Sprintf("resp_%s", strings.TrimPrefix(comp.ID, "chatcmpl-uws-")),
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Model:              comp.Model,
		Status:             "completed",
		SessionID:          comp.SessionID,
		TargetID:           comp.TargetID,
		SiteConversationID: comp.SiteConversationID,
		PageURL:            comp.PageURL,
		Output:             make([]OutputItem, 0),
	}

	if len(comp.Choices) > 0 {
		choice := comp.Choices[0]
		if choice.Message.Content != "" {
			resp.Output = append(resp.Output, OutputItem{
				Type: "message",
				ID:   fmt.Sprintf("msg_%d", time.Now().UnixNano()),
				Role: "assistant",
				Content: []ContentPart{
					{
						Type: "text",
						Text: choice.Message.Content,
					},
				},
			})
		}
		for _, tc := range choice.Message.ToolCalls {
			resp.Output = append(resp.Output, OutputItem{
				Type:      "function_call",
				ID:        fmt.Sprintf("fc_%d", time.Now().UnixNano()),
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	resp.Usage.InputTokens = comp.Usage.PromptTokens
	resp.Usage.OutputTokens = comp.Usage.CompletionTokens
	resp.Usage.TotalTokens = comp.Usage.TotalTokens

	return resp
}

// forcedChannelResponsesPath 解析 /v1/{channel}/responses 前缀。
func forcedChannelResponsesPath(path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) == 3 && segs[0] == "v1" && segs[2] == "responses" {
		return segs[1]
	}
	return ""
}
