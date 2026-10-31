package recipe

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// InteractionTrace 承载物理交互因果采样的输入三元组数据。
type InteractionTrace struct {
	Site            string              `json:"site"`             // 站点标识 (gemini, deepseek, chatgpt 等)
	Prompt          string              `json:"prompt"`           // 用户发送的 Prompt
	Model           string              `json:"model,omitempty"`  // 模型名称
	RenderedText    string              `json:"rendered_text"`    // 页面最终渲染的完整文本
	RequestURL      string              `json:"request_url"`      // CDP 捕获到的补全网络请求 URL
	RequestMethod   string              `json:"request_method"`   // GET / POST
	RequestHeaders  map[string]string   `json:"request_headers"`  // 请求头快照
	RequestBody     string              `json:"request_body"`     // 请求体字符串
	ResponseStatus  int                 `json:"response_status"`  // 响应状态码
	ResponseHeaders map[string]string   `json:"response_headers"` // 响应头快照
	ResponseBody    string              `json:"response_body"`    // 响应体样本（NDJSON / SSE 片段）
	PageSnapshot    PageContextSnapshot `json:"page_snapshot"`    // 页面全局变量与环境快照
}

// PageContextSnapshot 记录页面在发送瞬间的只读全局环境快照。
type PageContextSnapshot struct {
	URL          string            `json:"url"`                     // 当前页面 location.href
	Pathname     string            `json:"pathname"`                // 当前页面 location.pathname
	Cookies      map[string]string `json:"cookies,omitempty"`       // document.cookie 解析表
	Window       map[string]any    `json:"window,omitempty"`        // window 全局对象树（如 WIZ_global_data）
	LocalStorage map[string]string `json:"local_storage,omitempty"` // localStorage
}

// Synthesizer 负责从 InteractionTrace 自动推导并生成合规的 Recipe DSL。
type Synthesizer struct {
	MaxBFSDepth int // 限制 window 对象树 BFS 搜索最大层深，防止死循环
}

// NewSynthesizer 创建因果推导合成器内核实例。
func NewSynthesizer() *Synthesizer {
	return &Synthesizer{
		MaxBFSDepth: 7,
	}
}

// Synthesize 执行三元组因果推导核心算法。
func (s *Synthesizer) Synthesize(trace InteractionTrace) (*Recipe, error) {
	if strings.TrimSpace(trace.Site) == "" {
		return nil, fmt.Errorf("synthesizer: trace 缺少 site")
	}
	if strings.TrimSpace(trace.Prompt) == "" {
		return nil, fmt.Errorf("synthesizer: trace 缺少 prompt")
	}
	if strings.TrimSpace(trace.RequestURL) == "" {
		return nil, fmt.Errorf("synthesizer: trace 缺少 request_url")
	}

	parsedReqURL, err := url.Parse(trace.RequestURL)
	if err != nil {
		return nil, fmt.Errorf("synthesizer: 无法解析 request_url: %w", err)
	}

	parsedPageURL, _ := url.Parse(trace.PageSnapshot.URL)
	origin := ""
	if parsedPageURL != nil && parsedPageURL.Scheme != "" && parsedPageURL.Host != "" {
		origin = fmt.Sprintf("%s://%s", parsedPageURL.Scheme, parsedPageURL.Host)
	} else if parsedReqURL.Scheme != "" && parsedReqURL.Host != "" {
		origin = fmt.Sprintf("%s://%s", parsedReqURL.Scheme, parsedReqURL.Host)
	}

	recipe := &Recipe{
		Site:   strings.ToLower(trace.Site),
		Status: StatusDraft,
		Match: MatchRule{
			Origin: origin,
			PathRe: "^" + regexp.QuoteMeta(parsedReqURL.Path),
		},
		Request: RequestSpec{
			Method:      trace.RequestMethod,
			URLTemplate: parsedReqURL.Path,
			Headers:     make(map[string]string),
			TimeoutMs:   60000,
		},
		Slots: []Slot{
			{
				Name:     "prompt",
				Producer: ProducerPrompt,
				Scope:    "request",
			},
		},
		Response: ResponseSpec{
			Kind:   s.detectResponseKind(trace.ResponseBody, trace.ResponseHeaders),
			Frames: "verified",
		},
		Drift: &DriftSpec{
			Probe:    "recipe_probe",
			Triggers: []string{"on_failure", "scheduled"},
		},
	}

	if trace.Model != "" {
		recipe.Slots = append(recipe.Slots, Slot{
			Name:     "model",
			Producer: ProducerModel,
			Fallback: trace.Model,
			Scope:    "request",
		})
	}

	// 1. 过滤保留关键请求头（如 Content-Type, X-Same-Domain, x-goog-ext-* 等安全头）
	for hk, hv := range trace.RequestHeaders {
		lk := strings.ToLower(hk)
		if lk == "content-type" || lk == "x-same-domain" || strings.HasPrefix(lk, "x-goog-ext") || lk == "authorization" {
			recipe.Request.Headers[hk] = hv
		}
	}

	// 2. 槽位推导：提取 URL Query 参数中的动态槽位
	queryValues := parsedReqURL.Query()
	urlTplQuery := url.Values{}
	for qk, qvs := range queryValues {
		val := qvs[0]
		slotName := s.sanitizeSlotName(qk)

		if slot, found := s.deriveSlot(slotName, val, trace); found {
			recipe.Slots = append(recipe.Slots, slot)
			urlTplQuery.Set(qk, fmt.Sprintf("{{%s}}", slot.Name))
		} else {
			// 未识别为动态槽位的常量保持
			urlTplQuery.Set(qk, val)
		}
	}
	if len(urlTplQuery) > 0 {
		recipe.Request.URLTemplate = parsedReqURL.Path + "?" + urlTplQuery.Encode()
		// 解除 url.Values 将 {{...}} 转义为 %7B%7B...%7D%7D 的编码
		recipe.Request.URLTemplate = strings.ReplaceAll(recipe.Request.URLTemplate, "%7B%7B", "{{")
		recipe.Request.URLTemplate = strings.ReplaceAll(recipe.Request.URLTemplate, "%7D%7D", "}}")
	}

	// 3. 参数挖掘与请求体模板构造
	if trace.RequestBody != "" {
		s.mineBodyParameters(trace, recipe)
	}

	// 4. 针对特定站点（如 Gemini batchexecute）补齐结构化 body_builders
	if recipe.Site == "gemini" && strings.Contains(trace.RequestBody, "f.req") {
		if recipe.Request.BodyBuilders == nil {
			recipe.Request.BodyBuilders = make(map[string]BodyBuilder)
		}
		recipe.Request.BodyBuilders["f.req"] = BodyBuilder{
			Kind: "json_outer",
			Template: []any{
				nil,
				"json_string([[{{message}},0,null,null,null,null,0],[{{hl}}],null,{{conv}}])",
			},
		}
	}

	if err := recipe.Validate(); err != nil {
		return nil, fmt.Errorf("synthesizer: 生成的 recipe 校验未通过: %w", err)
	}

	return recipe, nil
}

// deriveSlot 对单一未知 Token 运用 BFS 及启发式规则自动反推 SlotProducer。
func (s *Synthesizer) deriveSlot(name, val string, trace InteractionTrace) (Slot, bool) {
	val = strings.TrimSpace(val)
	if val == "" {
		return Slot{}, false
	}

	// a. 优先从 window 全局变量树通过 BFS 精确反推（事实源优先）
	if len(trace.PageSnapshot.Window) > 0 {
		if foundPath, ok := s.bfsSearchWindow(trace.PageSnapshot.Window, val); ok {
			return Slot{
				Name:     name,
				Producer: ProducerPageGlobal,
				Path:     foundPath,
				Scope:    "page_load",
			}, true
		}
	}

	// b. 计数器特征判定（明确包含 reqid、counter、seq 或 step 命名）
	if strings.Contains(name, "reqid") || strings.Contains(name, "counter") || strings.Contains(name, "seq") {
		return Slot{
			Name:     name,
			Producer: ProducerCounter,
			Seed:     "last_sent_reqid",
			Step:     100000,
			Scope:    "page_load",
		}, true
	}

	// c. UUID 特征判定
	if isUUIDLike(val) {
		return Slot{
			Name:     name,
			Producer: ProducerFreshUUID,
			Scope:    "request",
		}, true
	}

	// d. 从当前 URL / pathname 正则反推（如会话 ID）
	pathname := trace.PageSnapshot.Pathname
	if pathname == "" && trace.PageSnapshot.URL != "" {
		if u, err := url.Parse(trace.PageSnapshot.URL); err == nil {
			pathname = u.Path
		}
	}
	if strings.Contains(pathname, val) {
		re := fmt.Sprintf(`(?:^|/)(%s)(?:$|[?#/])`, regexp.QuoteMeta(val))
		return Slot{
			Name:     name,
			Producer: ProducerFromURL,
			Re:       re,
			Fallback: nil,
			Scope:    "conversation",
		}, true
	}

	// e. 从 Cookies 反推
	for ck, cv := range trace.PageSnapshot.Cookies {
		if cv == val {
			return Slot{
				Name:     ck,
				Producer: ProducerCookie,
				Scope:    "request",
			}, true
		}
	}

	return Slot{}, false
}

// bfsSearchWindow 在 window 对象树上广度优先检索匹配的值（支持前缀匹配复合 Token，如 token:timestamp）。
func (s *Synthesizer) bfsSearchWindow(root map[string]any, targetVal string) (string, bool) {
	type queueItem struct {
		path  string
		obj   any
		depth int
	}

	queue := make([]queueItem, 0, 64)
	for k, v := range root {
		queue = append(queue, queueItem{path: k, obj: v, depth: 1})
	}

	// 目标值若为 prefix:timestamp（如 Google SNlM0e 的 AOvx0lJP...:179042...），提取前缀
	targetPrefix := targetVal
	if colonIdx := strings.Index(targetVal, ":"); colonIdx > 8 {
		targetPrefix = targetVal[:colonIdx]
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		if item.depth > s.MaxBFSDepth {
			continue
		}

		switch v := item.obj.(type) {
		case string:
			if v == targetVal || (targetPrefix != targetVal && v == targetPrefix) || strings.HasPrefix(targetVal, v) && len(v) >= 16 {
				return item.path, true
			}
		case float64:
			if fmt.Sprintf("%.0f", v) == targetVal {
				return item.path, true
			}
		case int, int64:
			if fmt.Sprintf("%d", v) == targetVal {
				return item.path, true
			}
		case map[string]any:
			for k, sub := range v {
				queue = append(queue, queueItem{
					path:  item.path + "." + k,
					obj:   sub,
					depth: item.depth + 1,
				})
			}
		case []any:
			for i, sub := range v {
				queue = append(queue, queueItem{
					path:  fmt.Sprintf("%s[%d]", item.path, i),
					obj:   sub,
					depth: item.depth + 1,
				})
			}
		}
	}

	return "", false
}

// mineBodyParameters 在请求体中挖掘已知参数并定位动态槽位。
func (s *Synthesizer) mineBodyParameters(trace InteractionTrace, recipe *Recipe) {
	body := trace.RequestBody

	// 检查是否包含 prompt
	if strings.Contains(body, trace.Prompt) {
		body = strings.ReplaceAll(body, trace.Prompt, "{{prompt}}")
	}

	// 检查 urlencoded 形式参数（形如 a=b&c=d）
	if strings.Contains(body, "=") && (strings.Contains(body, "&") || !strings.HasPrefix(strings.TrimSpace(body), "{")) {
		parts := strings.Split(body, "&")
		for _, part := range parts {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) == 2 {
				k := kv[0]
				v, _ := url.QueryUnescape(kv[1])
				slotName := s.sanitizeSlotName(k)
				if slot, found := s.deriveSlot(slotName, v, trace); found {
					recipe.Slots = append(recipe.Slots, slot)
					body = strings.ReplaceAll(body, kv[0]+"="+kv[1], kv[0]+"={{"+slot.Name+"}}")
				}
			}
		}
	}

	recipe.Request.BodyTemplate = body
}

// detectResponseKind 自动探测响应帧结构（NDJSON / SSE / JSON）。
func (s *Synthesizer) detectResponseKind(body string, headers map[string]string) string {
	ct := ""
	for hk, hv := range headers {
		if strings.EqualFold(hk, "content-type") {
			ct = strings.ToLower(hv)
			break
		}
	}

	if strings.Contains(body, ")]}'") || strings.Contains(body, "[[\"wrb.fr\"") || strings.Contains(body, "[[\"e\"") {
		return "ndjson_wrb_fr"
	}
	if strings.Contains(ct, "text/event-stream") || strings.Contains(body, "data:") {
		return "sse_data_json"
	}
	return "json"
}

func (s *Synthesizer) sanitizeSlotName(raw string) string {
	raw = strings.TrimPrefix(raw, "_")
	raw = strings.ReplaceAll(raw, ".", "_")
	raw = strings.ReplaceAll(raw, "-", "_")
	if raw == "" {
		return "token"
	}
	return raw
}

func isPureDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isUUIDLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	parts := strings.Split(s, "-")
	return len(parts) == 5 && len(parts[0]) == 8 && len(parts[1]) == 4 && len(parts[2]) == 4 && len(parts[3]) == 4 && len(parts[4]) == 12
}
