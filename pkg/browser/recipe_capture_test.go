package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// qianwenRequestWillBeSent 是千问真机抓包形状的 fixture（2026-10-02 实测：
// POST https://chat2.qianwen.com/api/v2/chat，query 带 ut/nonce/timestamp 签名参数，
// Cookie 里带 b-user-id）。用它锁「四元组解析 ＋ 凭据头脱敏」两条。
const qianwenRequestWillBeSent = `{
  "requestId": "12345.67",
  "loaderId": "ABC",
  "documentURL": "https://www.qianwen.com/chat",
  "request": {
    "url": "https://chat2.qianwen.com/api/v2/chat?biz_id=ai_qwen&ut=ut-secret&nonce=n1&timestamp=1759",
    "method": "POST",
    "headers": {
      "Content-Type": "application/json",
      "Cookie": "b-user-id=secret-token; cna=zzz",
      "Authorization": "Bearer top-secret",
      "x-platform": "web"
    },
    "postData": "{\"session_id\":\"ed8edb06ece356383ec1ee8baae8a70a\",\"content\":\"42\"}",
    "postDataEntries": [{"bytes": "eyJzZXNzaW9uX2lkIjoiZWQ4ZSJ9"}]
  },
  "type": "XHR",
  "initiator": {"type": "script"},
  "timestamp": 1759384800.5
}`

func TestParseCaptureRequestQuadrupleFields(t *testing.T) {
	rp, ok := parseCaptureRequest(json.RawMessage(qianwenRequestWillBeSent))
	if !ok {
		t.Fatal("千问 XHR 快照应被解析为抓存候选")
	}
	if rp.RequestID != "12345.67" || rp.Type != "XHR" {
		t.Fatalf("原有解析面被破坏: %+v", rp)
	}
	if rp.Method != "POST" || !strings.Contains(rp.URL, "chat2.qianwen.com/api/v2/chat") {
		t.Fatalf("方法/URL 解析错误: %q %q", rp.Method, rp.URL)
	}
	if rp.Headers["x-platform"] != "web" || rp.Headers["Content-Type"] != "application/json" {
		t.Fatalf("请求头解析错误: %+v", rp.Headers)
	}
	if !strings.Contains(rp.PostData, "ed8edb06ece356383ec1ee8baae8a70a") {
		t.Fatalf("请求体解析错误: %q", rp.PostData)
	}
	if len(rp.Entries) != 1 || rp.Entries[0] != "eyJzZXNzaW9uX2lkIjoiZWQ4ZSJ9" {
		t.Fatalf("postDataEntries 未展开: %+v", rp.Entries)
	}

	// 非 XHR/Fetch 不解析（原有口子：Document 直接不进在途簿记，也不抓存）。
	doc := `{"requestId":"1","type":"Document","request":{"url":"https://a/b","method":"POST"}}`
	if _, ok := parseCaptureRequest(json.RawMessage(doc)); ok {
		t.Fatal("Document 不应被当作抓存候选")
	}
	if _, ok := parseCaptureRequest(json.RawMessage(`{`)); ok {
		t.Fatal("坏 JSON 不应被当作抓存候选")
	}
}

func TestRecipeCaptureDirEnvOverride(t *testing.T) {
	t.Setenv("UWS_RECIPE_CAPTURES", "/tmp/uws-captures-test")
	if got := RecipeCaptureDir(); got != "/tmp/uws-captures-test" {
		t.Fatalf("env 覆盖未生效: %q", got)
	}
	t.Setenv("UWS_RECIPE_CAPTURES", "  ")
	if got := RecipeCaptureDir(); got != "" {
		t.Fatalf("空串应关闭抓存, got %q", got)
	}
}

func TestIsCaptureCandidate(t *testing.T) {
	cases := []struct {
		typ, method string
		want        bool
	}{
		{"XHR", "POST", true},
		{"Fetch", "PUT", true},
		{"XHR", "post", true},
		{"XHR", "GET", false},
		{"Fetch", "HEAD", false},
		{"XHR", "OPTIONS", false},
		{"Document", "POST", false},
		{"Script", "POST", false},
	}
	for _, c := range cases {
		if got := IsCaptureCandidate(c.typ, c.method); got != c.want {
			t.Fatalf("IsCaptureCandidate(%q,%q) = %v, want %v", c.typ, c.method, got, c.want)
		}
	}
}

func TestIsCaptureContentType(t *testing.T) {
	if !IsCaptureContentType("text/event-stream") || !IsCaptureContentType("Text/Event-Stream; charset=utf-8") {
		t.Fatal("事件流 mimeType 应命中")
	}
	if IsCaptureContentType("application/json") || IsCaptureContentType("") {
		t.Fatal("非事件流 mimeType 不应命中")
	}
}

func TestSanitizeHeaders(t *testing.T) {
	got := SanitizeHeaders(map[string]string{
		"Cookie":        "b-user-id=secret-token",
		"Authorization": "Bearer top-secret",
		"Set-Cookie":    "sid=xyz",
		"Content-Type":  "application/json",
	})
	for _, k := range []string{"Cookie", "Authorization", "Set-Cookie"} {
		if got[k] != redactedHeaderValue {
			t.Fatalf("凭据头 %s 未脱敏: %q", k, got[k])
		}
	}
	if got["Content-Type"] != "application/json" {
		t.Fatalf("非凭据头不应被改动: %q", got["Content-Type"])
	}
	if SanitizeHeaders(nil) != nil {
		t.Fatal("空头部应返回 nil")
	}
}

func TestCaptureFailureText(t *testing.T) {
	got := captureFailureText(json.RawMessage(`{"requestId":"1","errorText":"net::ERR_ABORTED"}`))
	if got != "net::ERR_ABORTED" {
		t.Fatalf("errorText 未透传: %q", got)
	}
	if got := captureFailureText(json.RawMessage(`{"requestId":"1"}`)); got != "loading_failed" {
		t.Fatalf("缺 errorText 应回落占位符: %q", got)
	}
}

func TestSaveRecipeCaptureAtomic(t *testing.T) {
	dir := t.TempDir()
	in := &RecipeCapture{
		CapturedAt:      time.Now().Format(time.RFC3339Nano),
		RequestID:       "12345.67",
		ResourceType:    "XHR",
		Method:          "POST",
		URL:             "https://chat2.qianwen.com/api/v2/chat",
		Headers:         SanitizeHeaders(map[string]string{"Cookie": "x"}),
		PostData:        `{"session_id":"ed8e"}`,
		PostDataEntries: []string{"eyJz"},
		Response:        &RecipeCaptureResponse{Status: 200, MimeType: "application/json"},
	}
	path, err := SaveRecipeCapture(dir, in)
	if err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	if filepath.Dir(path) != dir || !strings.HasSuffix(path, ".json") {
		t.Fatalf("落点不对: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	var back RecipeCapture
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("落盘内容不是可解析 JSON: %v", err)
	}
	if back.Method != "POST" || back.Headers["Cookie"] != redactedHeaderValue ||
		back.Response == nil || back.Response.Status != 200 || back.PostData == "" {
		t.Fatalf("回读字段丢失: %+v", back)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("文件权限应为 0600: %v %v", fi, err)
	}
	// 原子写不得留下临时文件。
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("目录应只剩一份快照, got %d", len(ents))
	}
	if _, err := SaveRecipeCapture("", in); err == nil {
		t.Fatal("空目录应报错")
	}
}

func awaitCapture(t *testing.T, ch <-chan *RecipeCapture) *RecipeCapture {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("超时未收到抓存快照")
	}
	return nil
}

func TestRecipeCaptureSinkFlushAndDrop(t *testing.T) {
	dir := t.TempDir()
	sink := newRecipeCaptureSinkAt(dir)
	ch := make(chan *RecipeCapture, 4)
	sink.flush = func(d string, c *RecipeCapture) (string, error) { ch <- c; return "", nil }

	// ① 候选（非 GET）＋响应元数据 → 落盘，且凭据头已脱敏。
	sink.begin("r1", "XHR", "POST", "https://chat2.qianwen.com/api/v2/chat?ut=x",
		map[string]string{"Cookie": "b-user-id=secret", "Content-Type": "application/json"},
		`{"session_id":"ed8e"}`, nil)
	sink.complete("r1", 200, "application/json", map[string]string{"Set-Cookie": "sid=1"}, "")
	c := awaitCapture(t, ch)
	if c.Method != "POST" || c.Response == nil || c.Response.Status != 200 || c.Failure != "" {
		t.Fatalf("候选快照内容不对: %+v", c)
	}
	if c.Headers["Cookie"] != redactedHeaderValue || c.Response.Headers["Set-Cookie"] != redactedHeaderValue {
		t.Fatalf("请求/响应凭据头未脱敏: %+v / %+v", c.Headers, c.Response.Headers)
	}
	if c.PostData != `{"session_id":"ed8e"}` {
		t.Fatalf("请求体丢失: %q", c.PostData)
	}

	// ② 非候选（GET ＋ 普通 JSON）→ 不落盘。
	sink.begin("r2", "XHR", "GET", "https://www.qianwen.com/api/v1/list", nil, "", nil)
	sink.complete("r2", 200, "application/json", nil, "")
	// ③ 非 XHR/Fetch → 不落盘。
	sink.begin("r3", "Document", "POST", "https://www.qianwen.com/chat", nil, "", nil)
	sink.complete("r3", 200, "text/html", nil, "")
	// ④ 无响应的中断请求 → 仍落盘（带 failure）。
	sink.begin("r4", "Fetch", "POST", "https://chat2.qianwen.com/api/v2/chat", nil, "{}", nil)
	sink.complete("r4", 0, "", nil, "net::ERR_ABORTED")
	c = awaitCapture(t, ch)
	if c.RequestID != "r4" || c.Failure != "net::ERR_ABORTED" || c.Response != nil {
		t.Fatalf("中断请求快照不对: %+v", c)
	}
	select {
	case extra := <-ch:
		t.Fatalf("非候选请求不该落盘: %+v", extra)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestRecipeCaptureSinkSSEUpgrade(t *testing.T) {
	sink := newRecipeCaptureSinkAt(t.TempDir())
	ch := make(chan *RecipeCapture, 2)
	sink.flush = func(d string, c *RecipeCapture) (string, error) { ch <- c; return "", nil }

	// GET 形态的事件流端点：请求侧非候选，靠响应 mimeType 兜底放行。
	sink.begin("s1", "Fetch", "GET", "https://example.com/stream?q=1", nil, "", nil)
	sink.complete("s1", 200, "text/event-stream; charset=utf-8", nil, "")
	c := awaitCapture(t, ch)
	if c.RequestID != "s1" || c.Response == nil || c.Response.MimeType != "text/event-stream; charset=utf-8" {
		t.Fatalf("事件流兜底升级失败: %+v", c)
	}
}

func TestRecipeCaptureSinkEvictsOldestPending(t *testing.T) {
	sink := newRecipeCaptureSinkAt(t.TempDir())
	ch := make(chan *RecipeCapture, 2)
	sink.flush = func(d string, c *RecipeCapture) (string, error) { ch <- c; return "", nil }

	for i := 0; i < maxPendingCaptures+1; i++ {
		sink.begin(string(rune('a'))+string(rune('a'+i%26))+string(rune('0'+i%10))+"-"+string(rune('0'+i/26)), "XHR", "POST", "https://e/x", nil, "{}", nil)
	}
	sink.mu.Lock()
	n := len(sink.pending)
	sink.mu.Unlock()
	if n > maxPendingCaptures {
		t.Fatalf("在途快照未被剪枝: %d", n)
	}
}

func TestRecipeCaptureSinkDefaultSaveWritesFile(t *testing.T) {
	dir := t.TempDir()
	sink := newRecipeCaptureSinkAt(dir)
	sink.begin("req-1", "XHR", "POST", "https://chat2.qianwen.com/api/v2/chat", nil, "{}", nil)
	sink.complete("req-1", 200, "application/json", nil, "")

	deadline := time.Now().Add(2 * time.Second)
	for {
		ents, _ := os.ReadDir(dir)
		if len(ents) == 1 && strings.HasSuffix(ents[0].Name(), ".json") {
			raw, err := os.ReadFile(filepath.Join(dir, ents[0].Name()))
			if err != nil {
				t.Fatalf("读回失败: %v", err)
			}
			var back RecipeCapture
			if err := json.Unmarshal(raw, &back); err != nil || back.RequestID != "req-1" {
				t.Fatalf("默认落盘内容不对: %v %+v", err, back)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("默认落盘超时, 目录 %v", ents)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRecipeCaptureSinkDisabledWithoutDir(t *testing.T) {
	sink := newRecipeCaptureSinkAt("")
	called := false
	sink.flush = func(d string, c *RecipeCapture) (string, error) { called = true; return "", nil }
	sink.begin("r1", "XHR", "POST", "https://e/x", nil, "{}", nil)
	sink.complete("r1", 200, "application/json", nil, "")
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Fatal("目录为空时不应落盘")
	}
}
