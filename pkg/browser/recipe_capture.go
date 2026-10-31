package browser

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// —— T-25 S1：站点 recipe「嗅探抓存」原始快照 ——
//
// 口径（docs/t25-recipe-slicing-plan-2026-10-01.md §3）：本片只做**被动抓取**——
// 把 CDP Network 事件里已经流过的请求方法/头/体与响应元数据原样落盘到
// ~/.uws/recipes/captures/，供 S2「嗅探编译」把 URL/头/体切成骨架＋槽位。
// 这一段刻意不解析、不判生产者、不碰页面：抓错了是抓存的错、切错了是编译的错，
// 两段证据分开才能分别归因。
//
// 三条硬约束：
//  1. 落盘失败只记日志：抓存是旁路，绝不能影响导航判定与聊天 SSE 主链路。
//  2. 凭据类请求/响应头（cookie/authorization 等）落盘前按值脱敏，只留键名。
//  3. 候选判定不能只复用 modelbridge.IsChatCompletionURL —— 它按 path 里的
//     `chat/completion` 匹配，千问的 `/api/v2/chat` 天然漏网（2026-10-02 实测）。
//     主判据＝XHR/Fetch 且非安全方法（必有请求体）；响应侧 mimeType 为
//     text/event-stream 时兜底升级，覆盖 GET 形态的流式端点。
//
// 已知边界：
//   - Cookie 不在 Network.requestWillBeSent 的 request.headers 里（Chrome 经
//     requestWillBeSentExtraInfo.associatedCookies 单独上报，且 JS 无法自设该头）；
//     S1 不订阅该事件——S4 回放走页内 fetch，Cookie 由浏览器自己带上，无需入库。
//   - 提交链路只增不减（S1 不做目录轮转/GC），抓存目录内容含会话标识与请求体明文，
//     仅供本机 S2 使用（目录 0700、文件 0600）。

const redactedHeaderValue = "[REDACTED]"

// credentialHeaders 是落盘前必须脱敏的头部（小写比较）。按「凭据」而非
// 「全部头」脱敏：S2 要靠头部的稳定字段切槽位，只摘掉能直接冒用身份的那些。
var credentialHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"x-auth-token":        true,
	"x-csrf-token":        true,
}

// RecipeCaptureDir 返回原始快照目录。UWS_RECIPE_CAPTURES 可覆盖；显式设为
// 空串表示关闭抓存（与 recipe.ActiveRecipeDir 同口径）；默认
// ~/.uws/recipes/captures。
func RecipeCaptureDir() string {
	if v, ok := os.LookupEnv("UWS_RECIPE_CAPTURES"); ok {
		return strings.TrimSpace(v)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".uws", "recipes", "captures")
}

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

// IsCaptureCandidate 判定请求是否值得抓存：XHR/Fetch 且非安全方法
// （GET/HEAD/OPTIONS 无请求体，不承载站点会话参数）。响应侧若为事件流，
// 由 IsCaptureContentType 兜底放行。
func IsCaptureCandidate(resourceType, method string) bool {
	if resourceType != "XHR" && resourceType != "Fetch" {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "", "GET", "HEAD", "OPTIONS":
		return false
	}
	return true
}

// IsCaptureContentType 判定响应是否为事件流（text/event-stream，忽略 charset 参数）。
func IsCaptureContentType(mimeType string) bool {
	m := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m == "text/event-stream"
}

// SanitizeHeaders 复制头部并把凭据类键的值替换为 [REDACTED]（键名保留：
// S2 需要知道「这里有个 cookie 头」，但它不许拿到可直接冒用身份的值）。
func SanitizeHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if credentialHeaders[strings.ToLower(strings.TrimSpace(k))] {
			out[k] = redactedHeaderValue
			continue
		}
		out[k] = v
	}
	return out
}

// captureRequestParams 是 Network.requestWillBeSent 里抓存关心的字段：
// URL（原有解析面）＋ S1 新扩的方法/头/体（四元组）。
type captureRequestParams struct {
	RequestID string
	Type      string
	URL       string
	Method    string
	Headers   map[string]string
	PostData  string
	Entries   []string
}

// parseCaptureRequest 解析 requestWillBeSent 事件参数：只认 XHR/Fetch（与原有
// 在途簿记口径一致，其它类型无抓存价值），并保留原有解析面 requestId/type/url。
func parseCaptureRequest(params json.RawMessage) (captureRequestParams, bool) {
	var p struct {
		RequestID string `json:"requestId"`
		Type      string `json:"type"`
		Request   struct {
			URL             string            `json:"url"`
			Method          string            `json:"method"`
			Headers         map[string]string `json:"headers"`
			PostData        string            `json:"postData"`
			PostDataEntries []struct {
				Bytes string `json:"bytes"`
			} `json:"postDataEntries"`
		} `json:"request"`
	}
	if json.Unmarshal(params, &p) != nil {
		return captureRequestParams{}, false
	}
	if p.Type != "XHR" && p.Type != "Fetch" {
		return captureRequestParams{}, false
	}
	entries := make([]string, 0, len(p.Request.PostDataEntries))
	for _, e := range p.Request.PostDataEntries {
		if e.Bytes != "" {
			entries = append(entries, e.Bytes)
		}
	}
	return captureRequestParams{
		RequestID: p.RequestID,
		Type:      p.Type,
		URL:       p.Request.URL,
		Method:    p.Request.Method,
		Headers:   p.Request.Headers,
		PostData:  p.Request.PostData,
		Entries:   entries,
	}, true
}

// maxPendingCaptures 限制「已发出、等响应补齐」的在途快照数：超出先丢最旧的。
// XHR/Fetch 突发流量下也不至于因为抓存把会话内存撑爆。
const maxPendingCaptures = 128

// recipeCaptureSink 是会话级的抓存状态：保存候选请求，等响应侧补齐后落盘。
// 自带互斥锁，与 cdpSession.mu 无嵌套依赖（不得在持有 s.mu 时调用）。
type recipeCaptureSink struct {
	dir string

	mu      sync.Mutex
	pending map[string]*RecipeCapture
	order   []string

	// flush 是落盘动作，默认 SaveRecipeCapture；单测替换为同步记录以去除时序抖动。
	flush func(dir string, c *RecipeCapture) (string, error)
}

func newRecipeCaptureSink() *recipeCaptureSink {
	return newRecipeCaptureSinkAt(RecipeCaptureDir())
}

func newRecipeCaptureSinkAt(dir string) *recipeCaptureSink {
	return &recipeCaptureSink{
		dir:     dir,
		pending: make(map[string]*RecipeCapture),
		flush:   SaveRecipeCapture,
	}
}

// begin 记录一次候选请求（响应未到）。requestId 为空或缺 dir 时无操作。
func (sink *recipeCaptureSink) begin(reqID, resourceType, method, rawURL string, headers map[string]string, postData string, entries []string) {
	if sink == nil || sink.dir == "" || reqID == "" {
		return
	}
	c := &RecipeCapture{
		CapturedAt:      time.Now().Format(time.RFC3339Nano),
		RequestID:       reqID,
		ResourceType:    resourceType,
		Method:          method,
		URL:             rawURL,
		Headers:         SanitizeHeaders(headers),
		PostData:        postData,
		PostDataEntries: entries,
	}
	sink.mu.Lock()
	if _, dup := sink.pending[reqID]; !dup {
		for len(sink.order) >= maxPendingCaptures {
			delete(sink.pending, sink.order[0])
			sink.order = sink.order[1:]
		}
		sink.order = append(sink.order, reqID)
	}
	sink.pending[reqID] = c
	sink.mu.Unlock()
}

// complete 用响应元数据补齐并落盘。落盘条件＝请求侧候选（非安全方法）或
// 响应侧事件流；两者都不满足即丢弃（GET JSON 之类不是 recipe 素材）。
// 落盘在独立 goroutine 里做，失败只记日志。
func (sink *recipeCaptureSink) complete(reqID string, status int, mimeType string, headers map[string]string, failure string) {
	if sink == nil || sink.dir == "" || reqID == "" {
		return
	}
	sink.mu.Lock()
	c, ok := sink.pending[reqID]
	if ok {
		delete(sink.pending, reqID)
		for i, id := range sink.order {
			if id == reqID {
				sink.order = append(sink.order[:i], sink.order[i+1:]...)
				break
			}
		}
	}
	sink.mu.Unlock()
	if !ok {
		return
	}
	if !IsCaptureCandidate(c.ResourceType, c.Method) && !IsCaptureContentType(mimeType) {
		return
	}
	if status != 0 || mimeType != "" || len(headers) > 0 {
		c.Response = &RecipeCaptureResponse{Status: status, MimeType: mimeType, Headers: SanitizeHeaders(headers)}
	}
	c.Failure = failure
	dir, flush := sink.dir, sink.flush
	go func() {
		if _, err := flush(dir, c); err != nil {
			log.Printf("browser: recipe 抓存落盘失败（不影响主链路）: %v", err)
		}
	}()
}

// SaveRecipeCapture 原子写入一份原始快照（同目录临时文件＋rename；目录 0700、
// 文件 0600）。文件名带时间戳与 requestId 前缀，同名重放不会互相覆盖。
func SaveRecipeCapture(dir string, c *RecipeCapture) (string, error) {
	if c == nil {
		return "", fmt.Errorf("browser: recipe 抓存缺少快照")
	}
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("browser: recipe 抓存目录为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("browser: 无法创建抓存目录 %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("browser: 无法序列化抓存快照: %w", err)
	}
	target := filepath.Join(dir, captureFileName(c))
	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return "", fmt.Errorf("browser: 无法创建抓存临时文件: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return "", fmt.Errorf("browser: 无法写入抓存临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("browser: 无法关闭抓存临时文件: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", fmt.Errorf("browser: 无法收紧抓存文件权限: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return "", fmt.Errorf("browser: 无法落盘抓存快照: %w", err)
	}
	return target, nil
}

// captureFileName 生成 `capture-<UTC 紧凑时间-毫秒>-<requestId 前 8 位>.json`。
func captureFileName(c *RecipeCapture) string {
	ts := time.Now().UTC().Format("20060102T150405.000")
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			return r
		}
		return -1
	}, c.RequestID)
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		id = "noreq"
	}
	return fmt.Sprintf("capture-%s-%s.json", ts, id)
}

// captureRequest / captureResponse / captureSettle 是 cdpSession 侧的接线入口，
// 全部容错为零操作：未初始化抓存（sink 为 nil）或目录关闭（空串）时不影响主链路。
func (s *cdpSession) captureRequest(reqID, resourceType, method, rawURL string, headers map[string]string, postData string, entries []string) {
	if s == nil || s.capture == nil {
		return
	}
	s.capture.begin(reqID, resourceType, method, rawURL, headers, postData, entries)
}

func (s *cdpSession) captureResponse(reqID string, status int, mimeType string, headers map[string]string) {
	if s == nil || s.capture == nil {
		return
	}
	s.capture.complete(reqID, status, mimeType, headers, "")
}

// captureSettle 在 loadingFinished / loadingFailed 时收尾没有 responseReceived 的
// 请求（被导航中断、连接失败等）：这类快照仍带着请求方法/头/体，对 S2 是有效证据。
func (s *cdpSession) captureSettle(params json.RawMessage, failure string) {
	if s == nil || s.capture == nil {
		return
	}
	var p struct {
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.capture.complete(p.RequestID, 0, "", nil, failure)
}

// captureFailureText 取 loadingFailed 的 errorText 落进快照：S2 据此区分
// 「接口本身如此」与「这条被抓时被中断了」。
func captureFailureText(params json.RawMessage) string {
	var p struct {
		ErrorText string `json:"errorText"`
	}
	if json.Unmarshal(params, &p) != nil || strings.TrimSpace(p.ErrorText) == "" {
		return "loading_failed"
	}
	return p.ErrorText
}
