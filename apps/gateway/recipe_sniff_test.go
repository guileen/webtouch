package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"uws/apps/gateway/recipe"
	"uws/pkg/contract"
)

// S2 嗅探编译的 fixture（形状取自 2026-10-02 千问真机抓存，值一律用假值，
// 不落真实游客令牌）。页面路由与 API 路径刻意不同，用来盯住「match 只写页面路由」。
const (
	sniffFixtureUUID   = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	sniffFixtureBuild  = "20261002.01"
	sniffFixtureXsrf   = "926fe280-c400-49f2-af5b-8a958c97092f"
	sniffFixtureSign   = "deadbeefcafe1234"
	sniffFixtureApiURL = "https://api.qianwen.com/api/v2/session/page/list?biz_id=ai_qwen&ut=" + sniffFixtureUUID +
		"&bl=" + sniffFixtureBuild + "&_ts=1790913138770&_nonce=279652961&_sign=" + sniffFixtureSign
)

func sniffFixtureCapture() contract.RecipeCapture {
	return contract.RecipeCapture{
		CapturedAt:   "2026-10-02T12:00:00+08:00",
		RequestID:    "1001.1",
		ResourceType: "Fetch",
		Method:       "POST",
		URL:          sniffFixtureApiURL,
		Headers: map[string]string{
			"Content-Type":  "application/json",
			"X-Platform":    "pc_tongyi",
			"X-DeviceId":    sniffFixtureUUID, // 与 query 的 ut 同值 ⇒ 应复用同一槽位
			"X-XSRF-TOKEN":  sniffFixtureXsrf, // 凭据类头 ⇒ 不落 recipe
			"User-Agent":    "Mozilla/5.0 (Macintosh) HeadlessChrome/154.0.0.0 Safari/537.36",
			"sec-ch-ua":     `"Chromium";v="154"`,
			"Authorization": "[REDACTED]",
		},
		PostData: `{"limit":50,"next_token":"","sort_field":"modifiedTime"}`,
		Response: &contract.RecipeCaptureResponse{Status: 200, MimeType: "application/json"},
	}
}

func sniffFixturePage(withState bool) *recipe.PageContextSnapshot {
	p := &recipe.PageContextSnapshot{
		URL:      "https://www.qianwen.com/chat/2f1c9d3e4b5a60718293a4b5c6d7e8f9",
		Pathname: "/chat/2f1c9d3e4b5a60718293a4b5c6d7e8f9",
	}
	if withState {
		p.LocalStorage = map[string]string{"deviceId": sniffFixtureUUID}
		p.Window = map[string]any{"__SITE__": map[string]any{"buildId": sniffFixtureBuild}}
	}
	return p
}

// 有页面态：值回溯拿到真实 producer，且 match 只写页面路由。
func TestSniffCompileWithPageState(t *testing.T) {
	res, err := CompileCaptureRecipe(SniffOptions{Site: "qianwen", Page: sniffFixturePage(true)},
		[]contract.RecipeCapture{sniffFixtureCapture()})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	rec := res.Recipe
	if rec.Status != recipe.StatusDraft {
		t.Fatalf("产物必须恒为 DRAFT，得到 %v", rec.Status)
	}
	if rec.Match.Origin != "https://www.qianwen.com" {
		t.Fatalf("match.origin 应为页面 origin: %q", rec.Match.Origin)
	}
	if !strings.HasPrefix(rec.Match.PathRe, "^/chat/2f1c9d3e") {
		t.Fatalf("match.path_re 必须是页面路由而非 API 路径: %q", rec.Match.PathRe)
	}
	if strings.Contains(rec.Match.PathRe, "api") {
		t.Fatalf("match 不得出现 API 路径: %q", rec.Match.PathRe)
	}
	if !strings.Contains(rec.Request.URLTemplate, "ut={{ut}}") {
		t.Fatalf("URL 模板应含 ut 槽位: %q", rec.Request.URLTemplate)
	}
	if strings.Contains(rec.Request.URLTemplate, sniffFixtureSign) {
		t.Fatalf("签名字面量不得落模板: %q", rec.Request.URLTemplate)
	}
	if rec.Request.Headers["Content-Type"] != "application/json" {
		t.Fatalf("功能面头应保留: %+v", rec.Request.Headers)
	}
	if rec.Request.Headers["X-DeviceId"] != "{{ut}}" {
		t.Fatalf("同值头应复用同一槽位: %+v", rec.Request.Headers)
	}
	if _, has := rec.Request.Headers["X-XSRF-TOKEN"]; has {
		t.Fatalf("凭据头不得进 recipe: %+v", rec.Request.Headers)
	}
	if _, has := rec.Request.Headers["Authorization"]; has {
		t.Fatalf("已脱敏的凭据头不得进 recipe: %+v", rec.Request.Headers)
	}
	byName := map[string]recipe.Slot{}
	for _, s := range rec.Slots {
		byName[s.Name] = s
	}
	if s := byName["ut"]; s.Producer != recipe.ProducerLocalStorage || s.Key != "deviceId" {
		t.Fatalf("ut 应回溯到 localStorage.deviceId，得到 %+v", s)
	}
	if s := byName["bl"]; s.Producer != recipe.ProducerPageGlobal || s.Path != "__SITE__.buildId" {
		t.Fatalf("bl 应回溯到 window.__SITE__.buildId，得到 %+v", s)
	}
	if s := byName["ts"]; s.Producer != recipe.ProducerCounter {
		t.Fatalf("_ts 应走 counter 启发，得到 %+v", s)
	}
	if s := byName["nonce"]; s.Producer != recipe.ProducerCounter {
		t.Fatalf("_nonce 应走 counter 启发，得到 %+v", s)
	}
	if !hasNote(res.Notes, "query:_sign") {
		t.Fatalf("签名参数应留丢弃痕迹: %+v", res.Notes)
	}
}

// 无页面态：启发式归因必须留痕，且不假装是页面态证据。
func TestSniffCompileWithoutPageStateMarksHeuristic(t *testing.T) {
	res, err := CompileCaptureRecipe(SniffOptions{Site: "qianwen", Page: sniffFixturePage(false)},
		[]contract.RecipeCapture{sniffFixtureCapture()})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	var ut recipe.Slot
	for _, s := range res.Recipe.Slots {
		if s.Name == "ut" {
			ut = s
		}
	}
	if ut.Producer != recipe.ProducerFreshUUID {
		t.Fatalf("无页面态时 ut 应走形状启发 fresh_uuid，得到 %+v", ut)
	}
	if !hasNote(res.Notes, "slot:ut") {
		t.Fatalf("启发式归因必须留痕: %+v", res.Notes)
	}
	for _, n := range res.Notes {
		if n.Where == "slot:ut" && !strings.Contains(n.Reason, "形状启发") {
			t.Fatalf("启发式痕迹须写明依据: %+v", n)
		}
	}
}

// 同站多份快照：优先选响应为事件流的那一份作主请求。
func TestSniffCompilePrefersEventStreamCapture(t *testing.T) {
	chat := contract.RecipeCapture{
		RequestID: "2002.9", ResourceType: "Fetch", Method: "POST",
		URL:      "https://api.example.com/api/v2/chat?biz_id=ai_qwen",
		Headers:  map[string]string{"Content-Type": "application/json"},
		PostData: `{"messages":[{"content":"hi"}]}`,
		Response: &contract.RecipeCaptureResponse{Status: 200, MimeType: "text/event-stream; charset=utf-8"},
	}
	other := sniffFixtureCapture()
	res, err := CompileCaptureRecipe(SniffOptions{Site: "qianwen", Page: sniffFixturePage(true)},
		[]contract.RecipeCapture{other, chat})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if res.Main != "2002.9" {
		t.Fatalf("应选事件流那一份作主请求，得到 %q", res.Main)
	}
	if res.Recipe.Response.Kind != "sse_data_json" {
		t.Fatalf("响应类型应为 sse_data_json: %q", res.Recipe.Response.Kind)
	}
	if !hasNote(res.Notes, "capture:"+other.RequestID) {
		t.Fatalf("未选中的快照应留痕: %+v", res.Notes)
	}
}

// 缺页面态 URL 必须 fail-closed（不能拿 API 路径凑 match）。
func TestSniffCompileRequiresPageURL(t *testing.T) {
	if _, err := CompileCaptureRecipe(SniffOptions{Site: "qianwen", Page: &recipe.PageContextSnapshot{}},
		[]contract.RecipeCapture{sniffFixtureCapture()}); err == nil {
		t.Fatal("缺页面态 URL 时应报错")
	}
	if _, err := CompileCaptureRecipe(SniffOptions{Site: ""}, []contract.RecipeCapture{sniffFixtureCapture()}); err == nil {
		t.Fatal("缺 site 时应报错")
	}
}

// 编译产物序列化后不得出现任何凭据明文（凭据头值 / 签名值）。
func TestSniffCompileNeverEmitsCredentialPlaintext(t *testing.T) {
	res, err := CompileCaptureRecipe(SniffOptions{Site: "qianwen", Page: sniffFixturePage(true)},
		[]contract.RecipeCapture{sniffFixtureCapture()})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	raw, err := json.Marshal(res.Recipe)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{sniffFixtureXsrf, sniffFixtureSign, "926fe280"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("recipe 文本泄漏凭据 %q: %s", secret, raw)
		}
	}
}

func hasNote(notes []SniffNote, where string) bool {
	for _, n := range notes {
		if n.Where == where {
			return true
		}
	}
	return false
}
