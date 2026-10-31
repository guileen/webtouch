package gateway

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"uws/apps/gateway/recipe"
	"uws/pkg/contract"
)

// T-25 S2「嗅探编译」：把 S1 落盘的原始请求快照（contract.RecipeCapture）切成
// 「稳定骨架 ＋ 槽位」，产出契约 §13 的 recipe JSON。
//
// 与 recipe.Synthesizer 的分工（两者不重复）：
//   - Synthesizer 吃「已归因的交互三元组」（带 prompt / 页面态 / 响应体），服务首轮闭环；
//   - 本编译器吃**未归因的原始抓存**（可一次多份），producer 判定＝「页面态值回溯优先、
//     形状与命名启发兜底」，且**凡走启发分支都留 SniffNote**（§13 边界：证据只到一站，
//     不得假装泛化）。
//
// 硬约束：
//  1. 凭据类值（凭据头 / 疑似 token·签名）不落 recipe 文本，也不得降级 static；
//     归因不了就丢该字段 ＋ 留痕（由 S3/S4 fail-closed 兜底），绝不猜。
//  2. match 只写页面路由（PathRe），API 路径只出现在 request.url_template。
//  3. 产物恒为 DRAFT：编译 ≠ 可用，激活只认真实页自证（§13 激活门禁）。

// SniffOptions 是一次嗅探编译的输入。Page 决定 match 与槽位的值回溯，必需。
type SniffOptions struct {
	Site string                      // 站点标识（写进 recipe.site）
	Page *recipe.PageContextSnapshot // 页面态：URL/Pathname 必需，Window/LocalStorage/Cookies 用于回溯
}

// SniffNote 是一条编译留痕（丢弃、保守处理或启发式归因）。
type SniffNote struct {
	Where  string `json:"where"`  // "query:ut" / "header:authorization" / "body:next_token" / "slot:reqid"
	Reason string `json:"reason"` // 为什么这么处理
}

// SniffResult 是编译产物：recipe ＋ 取证留痕。
type SniffResult struct {
	Recipe *recipe.Recipe `json:"recipe"`
	Notes  []SniffNote    `json:"notes,omitempty"`
	Main   string         `json:"main_request_id"`
}

var (
	sniffUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	sniffHexRe  = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

// credentialKeyHints 命中即按「疑似凭据」处理：不落 recipe 文本、不降 static。
var credentialKeyHints = []string{"token", "csrf", "xsrf", "auth", "secret", "sig", "password", "cookie", "apikey", "api_key"}

// CompileCaptureRecipe 把同站若干份原始抓存编译成一份 §13 recipe（恒 DRAFT）。
func CompileCaptureRecipe(opts SniffOptions, captures []contract.RecipeCapture) (*SniffResult, error) {
	site := strings.ToLower(strings.TrimSpace(opts.Site))
	if site == "" {
		return nil, fmt.Errorf("sniff: 缺少 site")
	}
	if len(captures) == 0 {
		return nil, fmt.Errorf("sniff: 没有可编译的抓存快照")
	}
	page := opts.Page
	if page == nil || strings.TrimSpace(page.URL) == "" {
		return nil, fmt.Errorf("sniff: 缺少页面态 URL（match 必须写页面路由，不能拿 API 路径凑）")
	}
	pageURL, err := url.Parse(page.URL)
	if err != nil || pageURL.Host == "" {
		return nil, fmt.Errorf("sniff: 页面 URL 无法解析: %q", page.URL)
	}
	pagePath := page.Pathname
	if strings.TrimSpace(pagePath) == "" {
		pagePath = pageURL.Path
	}
	if strings.TrimSpace(pagePath) == "" {
		pagePath = "/"
	}

	main, notes := pickSniffCapture(captures)
	reqURL, err := url.Parse(main.URL)
	if err != nil || reqURL.Host == "" {
		return nil, fmt.Errorf("sniff: 主请求 URL 无法解析: %q", main.URL)
	}

	c := &sniffCompiler{
		page:    page,
		byName:  map[string]bool{},
		byValue: map[string]string{},
		notes:   notes,
	}
	rawQuery := c.sniffQuery(reqURL.RawQuery)
	headers := c.sniffHeaders(main.Headers)
	bodyTemplate := c.sniffBody(main.PostData, headerLookup(main.Headers, "content-type"))

	tpl := reqURL.EscapedPath()
	if strings.TrimSpace(tpl) == "" {
		tpl = "/"
	}
	if rawQuery != "" {
		tpl += "?" + rawQuery
	}

	rec := &recipe.Recipe{
		Site:   site,
		Status: recipe.StatusDraft,
		Match:  recipe.MatchRule{Origin: pageURL.Scheme + "://" + pageURL.Host, PathRe: "^" + regexp.QuoteMeta(pagePath)},
		Request: recipe.RequestSpec{
			Method:       strings.ToUpper(strings.TrimSpace(main.Method)),
			URLTemplate:  tpl,
			Headers:      headers,
			BodyTemplate: bodyTemplate,
		},
		Slots:    c.slots,
		Response: recipe.ResponseSpec{Kind: sniffResponseKind(main), Frames: "unverified"},
		Drift:    &recipe.DriftSpec{Probe: "recipe_probe", Triggers: []string{"on_failure", "scheduled"}},
	}
	if err := rec.Validate(); err != nil {
		return nil, fmt.Errorf("sniff: 编译产物不合法: %w", err)
	}
	if len(c.notes) > 0 {
		rec.Metadata = map[string]string{"sniff_note_count": fmt.Sprint(len(c.notes))}
	}
	return &SniffResult{Recipe: rec, Notes: c.notes, Main: main.RequestID}, nil
}

type sniffCompiler struct {
	page    *recipe.PageContextSnapshot
	slots   []recipe.Slot
	byName  map[string]bool
	byValue map[string]string // 值 → 已分配槽位名（同值复用，避免同一个 token 两处两种写法）
	notes   []SniffNote
}

func (c *sniffCompiler) note(where, reason string) {
	c.notes = append(c.notes, SniffNote{Where: where, Reason: reason})
}

// slotFor 判定该值能否成为槽位：命中返回占位名（同值复用），否则 false（保留字面量）。
func (c *sniffCompiler) slotFor(name, value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if existing, ok := c.byValue[value]; ok {
		return existing, true
	}
	slotName := sniffSlotName(name)
	if slotName == "" {
		return "", false
	}
	slot, basis, ok := classifySniffValue(slotName, value, c.page)
	if !ok {
		return "", false
	}
	if c.byName[slot.Name] {
		for i := 2; ; i++ {
			cand := fmt.Sprintf("%s_%d", slot.Name, i)
			if !c.byName[cand] {
				slot.Name = cand
				break
			}
		}
	}
	c.byName[slot.Name] = true
	c.byValue[value] = slot.Name
	c.slots = append(c.slots, slot)
	c.note("slot:"+slot.Name, basis)
	return slot.Name, true
}

// classifySniffValue 判定单个值的 producer：页面态回溯优先，形状/命名启发兜底，凭据类直接放弃。
func classifySniffValue(name, value string, page *recipe.PageContextSnapshot) (recipe.Slot, string, bool) {
	if page != nil {
		if path, ok := bfsFindString(page.Window, value); ok {
			return recipe.Slot{Name: name, Producer: recipe.ProducerPageGlobal, Path: path, Scope: "page_load"},
				"页面态回溯：window." + path, true
		}
		for k, v := range page.LocalStorage {
			if v == value {
				return recipe.Slot{Name: name, Producer: recipe.ProducerLocalStorage, Key: k, Scope: "page_load"},
					"页面态回溯：localStorage." + k, true
			}
		}
		for k, v := range page.Cookies {
			if v == value {
				return recipe.Slot{Name: name, Producer: recipe.ProducerCookie, Key: k, Scope: "request"},
					"页面态回溯：cookie." + k, true
			}
		}
		pathname := page.Pathname
		if strings.TrimSpace(pathname) == "" && page.URL != "" {
			if u, err := url.Parse(page.URL); err == nil {
				pathname = u.Path
			}
		}
		if pathname != "" && strings.Contains(pathname, value) {
			return recipe.Slot{Name: name, Producer: recipe.ProducerFromURL,
					Re: `(?:^|/)(` + regexp.QuoteMeta(value) + `)(?:$|[?#/])`, Scope: "conversation"},
				"页面态回溯：命中页面路径 ⇒ from_url", true
		}
	}
	// 页面态对不上，且名字像凭据/签名 ⇒ 不猜、不落 static（交 S3/S4 的 local_storage/签名器生产者）
	if looksCredentialKey(name) {
		return recipe.Slot{}, "", false
	}
	lower := strings.ToLower(name)
	if sniffDigits(value) && (strings.Contains(lower, "reqid") || strings.Contains(lower, "req_id") ||
		strings.Contains(lower, "counter") || strings.Contains(lower, "seq") ||
		strings.Contains(lower, "ts") || strings.Contains(lower, "nonce")) {
		return recipe.Slot{Name: name, Producer: recipe.ProducerCounter, Seed: "last_sent_" + name, Step: 1, Scope: "page_load"},
			"形状启发：命名疑似计数器/时间戳 ⇒ counter（未与页面态对拍，S3 复核）", true
	}
	if sniffUUIDRe.MatchString(value) {
		return recipe.Slot{Name: name, Producer: recipe.ProducerFreshUUID, Scope: "request"},
			"形状启发：UUID ⇒ fresh_uuid（未与页面态对拍，S3 复核）", true
	}
	if len(value) >= 16 && sniffHexRe.MatchString(value) {
		return recipe.Slot{Name: name, Producer: recipe.ProducerFreshHex, Length: len(value), Scope: "request"},
			fmt.Sprintf("形状启发：%d 位十六进制 ⇒ fresh_hex（未与页面态对拍，S3 复核）", len(value)), true
	}
	return recipe.Slot{}, "", false
}

// sniffQuery 把原始 query 切成骨架＋槽位：逐参数判生产者，命中即 {{slot}}，未命中保留原样。
// rawValue 原样保留——渲染端对槽位值做 encodeURIComponent，与原始编码串同构（只编码一次）。
func (c *sniffCompiler) sniffQuery(rawQuery string) string {
	if strings.TrimSpace(rawQuery) == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		name, rawValue, ok := strings.Cut(part, "=")
		if !ok {
			out = append(out, part)
			continue
		}
		decoded, err := url.QueryUnescape(rawValue)
		if err != nil {
			decoded = rawValue
		}
		if slotName, hit := c.slotFor(name, decoded); hit {
			out = append(out, name+"={{"+slotName+"}}")
			continue
		}
		if looksCredentialKey(name) && sniffLooksDynamic(decoded) {
			c.note("query:"+name, "疑似凭据/签名且无法归因 ⇒ 未落静态（S3 补取值器或站点签名器）")
			continue
		}
		out = append(out, name+"="+rawValue)
	}
	return strings.Join(out, "&")
}

// sniffHeaders 只留功能面稳定头；浏览器托管头（fetch 自动补）与凭据头一律不进 recipe。
func (c *sniffCompiler) sniffHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		v := strings.TrimSpace(headers[k])
		lk := strings.ToLower(k)
		switch {
		case v == "" || v == "[REDACTED]":
			c.note("header:"+k, "值为空或 S1 已脱敏 ⇒ 不落 recipe")
		case isBrowserManagedHeader(lk):
			// 浏览器自带（回放时 fetch 自动补），不进 recipe
		case isCredentialHeader(lk) || looksCredentialKey(lk):
			c.note("header:"+k, "凭据类头 ⇒ 不落文本（S3 用 local_storage/签名器生产者补）")
		case isFunctionalHeader(lk):
			if slotName, hit := c.slotFor(k, v); hit {
				out[k] = "{{" + slotName + "}}"
			} else {
				out[k] = v
			}
		default:
			c.note("header:"+k, "非功能面白名单头 ⇒ 保守不收")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sniffBody 把请求体切成骨架＋槽位（JSON / 表单 / 其它）。
func (c *sniffCompiler) sniffBody(body, contentType string) string {
	body = strings.TrimSpace(body)
	if body == "" || body == "-" {
		return ""
	}
	ct := strings.ToLower(contentType)
	trimmed := strings.TrimLeft(body, " \t\r\n")
	if strings.Contains(ct, "json") || strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if tpl, ok := c.sniffJSONBody(body); ok {
			return tpl
		}
	}
	if strings.Contains(ct, "x-www-form-urlencoded") || (strings.Contains(body, "=") && strings.Contains(body, "&")) {
		if tpl, ok := c.sniffFormBody(body); ok {
			return tpl
		}
	}
	c.note("body", "非 JSON/表单体 ⇒ 原样保留（复合串交 S3/S4 处理）")
	return body
}

// sniffJSONBody 只把**字符串叶子**换槽位（数字叶子保留字面量：替换成字符串会改类型）。
func (c *sniffCompiler) sniffJSONBody(raw string) (string, bool) {
	var root any
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return "", false
	}
	changed := false
	var walk func(v any, path string) any
	walk = func(v any, path string) any {
		switch t := v.(type) {
		case string:
			if slotName, hit := c.slotFor(path, t); hit {
				changed = true
				return "{{" + slotName + "}}"
			}
			if looksCredentialKey(path) && sniffLooksDynamic(t) {
				c.note("body:"+path, "疑似凭据且无法归因 ⇒ 未落静态（位置保留、值清空）")
				changed = true
				return ""
			}
			return t
		case map[string]any:
			for k, sub := range t {
				t[k] = walk(sub, k)
			}
			return t
		case []any:
			for i, sub := range t {
				t[i] = walk(sub, path)
			}
			return t
		default:
			return v
		}
	}
	out := walk(root, "")
	if !changed {
		return raw, true
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw, true
	}
	return string(b), true
}

// sniffFormBody 逐 k=v 判生产者（与 URL query 同构：保留原始编码、槽位交给渲染端编码）。
func (c *sniffCompiler) sniffFormBody(raw string) (string, bool) {
	parts := strings.Split(raw, "&")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		name, rawValue, ok := strings.Cut(part, "=")
		if !ok {
			out = append(out, part)
			continue
		}
		decoded, err := url.QueryUnescape(rawValue)
		if err != nil {
			decoded = rawValue
		}
		if slotName, hit := c.slotFor(name, decoded); hit {
			out = append(out, name+"={{"+slotName+"}}")
			continue
		}
		if looksCredentialKey(name) && sniffLooksDynamic(decoded) {
			c.note("body:"+name, "疑似凭据且无法归因 ⇒ 未落静态")
			continue
		}
		out = append(out, name+"="+rawValue)
	}
	return strings.Join(out, "&"), true
}

// pickSniffCapture 选主请求：事件流响应 > 请求体更长者。其余留痕。
func pickSniffCapture(captures []contract.RecipeCapture) (contract.RecipeCapture, []SniffNote) {
	score := func(c contract.RecipeCapture) int {
		s := len(c.PostData)
		if c.Response != nil && contract.IsCaptureContentType(c.Response.MimeType) {
			s += 1 << 20
		}
		return s
	}
	best := captures[0]
	bestScore := score(best)
	for _, c := range captures[1:] {
		if sc := score(c); sc > bestScore {
			best, bestScore = c, sc
		}
	}
	var notes []SniffNote
	for _, c := range captures {
		if c.RequestID != best.RequestID {
			notes = append(notes, SniffNote{Where: "capture:" + c.RequestID,
				Reason: "非主请求（同站多份快照只编译最像补全请求的一份）"})
		}
	}
	return best, notes
}

// sniffResponseKind 由响应元数据判分帧类型（快照无响应体，故不判更细的帧结构）。
func sniffResponseKind(c contract.RecipeCapture) string {
	if c.Response == nil {
		return "json"
	}
	if contract.IsCaptureContentType(c.Response.MimeType) {
		return "sse_data_json"
	}
	mt := strings.ToLower(c.Response.MimeType)
	if strings.Contains(mt, "ndjson") {
		return "ndjson_wrb_fr"
	}
	if strings.Contains(mt, "json") {
		return "json"
	}
	return "json"
}

// bfsFindString 在 window 对象树里广度优先找与目标相等的字符串（前缀匹配复合 token，如 at 的 ":毫秒" 后缀）。
func bfsFindString(root map[string]any, target string) (string, bool) {
	if len(root) == 0 || target == "" {
		return "", false
	}
	prefix := target
	if i := strings.Index(target, ":"); i > 8 {
		prefix = target[:i]
	}
	type item struct {
		path  string
		obj   any
		depth int
	}
	queue := make([]item, 0, 32)
	for k, v := range root {
		queue = append(queue, item{k, v, 1})
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth > 7 {
			continue
		}
		switch t := cur.obj.(type) {
		case string:
			if t == target || (prefix != target && t == prefix) {
				return cur.path, true
			}
		case float64:
			if fmt.Sprintf("%.0f", t) == target {
				return cur.path, true
			}
		case map[string]any:
			for k, v := range t {
				queue = append(queue, item{cur.path + "." + k, v, cur.depth + 1})
			}
		case []any:
			for i, v := range t {
				queue = append(queue, item{fmt.Sprintf("%s[%d]", cur.path, i), v, cur.depth + 1})
			}
		}
	}
	return "", false
}

func looksCredentialKey(name string) bool {
	lower := strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), "_$-"))
	for _, h := range credentialKeyHints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

func sniffLooksDynamic(v string) bool {
	if len(v) >= 12 {
		return true
	}
	if len(v) >= 8 && sniffDigits(v) {
		return true
	}
	return strings.ContainsAny(v, "=:+/")
}

func sniffSlotName(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimLeft(raw, "_")
	raw = strings.NewReplacer(".", "_", "-", "_", "$", "_", " ", "_").Replace(raw)
	if raw == "" {
		return ""
	}
	if sniffDigits(raw[:1]) {
		return "s_" + raw
	}
	return raw
}

func sniffDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isBrowserManagedHeader(lk string) bool {
	switch lk {
	case "user-agent", "referer", "origin", "host", "content-length", "connection",
		"accept", "accept-encoding", "accept-language", "priority", "cache-control", "pragma":
		return true
	}
	return strings.HasPrefix(lk, "sec-")
}

func isCredentialHeader(lk string) bool {
	switch lk {
	case "authorization", "proxy-authorization", "cookie", "set-cookie",
		"x-api-key", "x-auth-token", "x-csrf-token", "x-xsrf-token":
		return true
	}
	return false
}

func isFunctionalHeader(lk string) bool {
	switch lk {
	case "content-type", "x-same-domain", "x-requested-with", "x-platform",
		"x-device-id", "x-deviceid", "prod_id":
		return true
	}
	return strings.HasPrefix(lk, "x-goog-ext-")
}

func headerLookup(headers map[string]string, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
