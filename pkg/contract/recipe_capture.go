package contract

import "strings"

// RecipeCaptureResponse 是一次交换的响应侧元数据（不含响应体：S2 判槽位只需
// 状态/类型/头；响应体抓取属 S4 回放期的运行时能力）。
type RecipeCaptureResponse struct {
	Status   int               `json:"status"`
	MimeType string            `json:"mime_type,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// RecipeCapture 是一次被判定「值得编译成 recipe」的原始 HTTP 交换快照。
type RecipeCapture struct {
	CapturedAt      string                 `json:"captured_at"`
	RequestID       string                 `json:"request_id"`
	ResourceType    string                 `json:"resource_type"`
	Method          string                 `json:"method"`
	URL             string                 `json:"url"`
	Headers         map[string]string      `json:"headers,omitempty"`
	PostData        string                 `json:"post_data,omitempty"`
	PostDataEntries []string               `json:"post_data_entries,omitempty"`
	Response        *RecipeCaptureResponse `json:"response,omitempty"`
	Failure         string                 `json:"failure,omitempty"`
}

// IsCaptureContentType 判定响应是否为事件流（text/event-stream，忽略 charset 参数）。
func IsCaptureContentType(mimeType string) bool {
	m := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m == "text/event-stream"
}
