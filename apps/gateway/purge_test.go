package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- 夹具 ----------

// newPurgeGateway 构造一个关掉后台 worker、队列落临时目录的网关（避免写用户 ~/.uws）。
func newPurgeGateway(t *testing.T, engine *fakeEngine) *Gateway {
	t.Helper()
	if engine == nil {
		engine = newFakeEngine()
	}
	cfg := DefaultConfig("127.0.0.1:50051")
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	stable := time.Duration(0)
	gw, err := NewGateway(&cfg, Options{
		EngineFactory:  func(string) (EngineClient, error) { return engine, nil },
		AnswerStable:   &stable,
		Sleep:          func(time.Duration) {},
		AuthStatePath:  filepath.Join(t.TempDir(), "gateway-auth-state.json"),
		PurgeQueuePath: filepath.Join(t.TempDir(), "purge-queue.json"),
		PurgeInterval:  -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

// addLiveSession 直接登记一个「标签页还活着」的会话（不触发引擎初始化）。
func addLiveSession(gw *Gateway, sid, channelID string) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.sessions[sid] = &Session{
		ID: sid, ChannelID: channelID, EngineAddr: "127.0.0.1:50051",
		ContextID: "ctx-" + sid, TargetID: "tgt-" + sid, CreatedAt: time.Now(),
	}
}

// purgeEvaluator 模拟页面：location.pathname 返回 pathname，删除脚本返回 resultJSON。
func purgeEvaluator(t *testing.T, pathname, resultJSON string) func(ActionRequest) (ActionResult, error) {
	t.Helper()
	return func(req ActionRequest) (ActionResult, error) {
		switch {
		case req.Value == "location.pathname":
			b, _ := json.Marshal(pathname)
			return ActionResult{Success: true, ExtractedData: string(b)}, nil
		case req.Value == sidebarAnchorsScript:
			// 关会话清账要记「目标 id ＋ 标题」审计日志，会顺带读一次侧栏；此处给空表。
			return ActionResult{Success: true, ExtractedData: "[]"}, nil
		case strings.Contains(req.Value, "GzXR5e"):
			return ActionResult{Success: true, ExtractedData: resultJSON}, nil
		default:
			t.Errorf("未预期的页内脚本: %s", req.Value)
			return ActionResult{Success: true, ExtractedData: "null"}, nil
		}
	}
}

const testCid = "c_25096c6ba2166662"

// ---------- 纯函数 ----------

func TestNormalizeConversationID(t *testing.T) {
	cases := map[string]string{
		"c_25096c6ba2166662":     testCid,
		"25096c6ba2166662":       testCid,
		"/app/25096c6ba2166662":  testCid,
		"/app/25096c6ba2166662/": testCid,
		"https://gemini.google.com/app/25096c6ba2166662?hl=en": testCid,
		"  c_25096C6BA2166662  ":                               testCid,
	}
	for raw, want := range cases {
		got, err := NormalizeConversationID(raw)
		if err != nil {
			t.Fatalf("NormalizeConversationID(%q) 报错: %v", raw, err)
		}
		if got != want {
			t.Fatalf("NormalizeConversationID(%q)=%q，期望 %q", raw, got, want)
		}
	}
	for _, raw := range []string{"", "   ", "abc", "c_", "/app", "https://gemini.google.com/app", "c_zzzzzzzzzzzzzzzz", "12345"} {
		if got, err := NormalizeConversationID(raw); err == nil {
			t.Fatalf("NormalizeConversationID(%q) 期望报错，得到 %q", raw, got)
		}
	}
}

func TestBuildGeminiDeleteScript(t *testing.T) {
	script := BuildGeminiDeleteScript(testCid)
	for _, want := range []string{
		"GzXR5e", testCid, "generic", "SNlM0e", "cfb2h", "FdrFJe",
		"batchexecute", "X-Same-Domain", "_reqid", "domListed",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("删除脚本缺少 %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "c_c_") {
		t.Fatalf("会话 ID 前缀重复:\n%s", script)
	}
	// 换一种写法必须收敛到同一个脚本。
	if alt := BuildGeminiDeleteScript("/app/25096c6ba2166662"); alt != script {
		t.Fatal("同一会话的不同写法应生成同一脚本")
	}
}

func TestParseConversationDeleteResult(t *testing.T) {
	ok, err := ParseConversationDeleteResult([]byte(`{"httpStatus":200,"domListed":false,"requestId":3148101}`))
	if err != nil || !ok.Verified {
		t.Fatalf("2xx 且侧栏未列出应判 deleted：res=%+v err=%v", ok, err)
	}
	unverified, err := ParseConversationDeleteResult([]byte(`{"httpStatus":200,"domListed":true}`))
	if err != nil {
		t.Fatalf("2xx 但侧栏仍列出不算错误、算未核实: %v", err)
	}
	if unverified.Verified {
		t.Fatal("侧栏仍列出时不得判成功")
	}
	for _, raw := range []string{
		`{"httpStatus":500,"bodyHead":"boom"}`,
		`{"httpStatus":0}`,
		`{"error":"page_slots_missing"}`,
		`not json`,
	} {
		if _, err := ParseConversationDeleteResult([]byte(raw)); err == nil {
			t.Fatalf("%s 期望报错", raw)
		}
	}
}

// ---------- 待删除列表 ----------

func TestPurgeQueueEnqueueIdempotentAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.json")
	q := NewPurgeQueue(path)
	first, err := q.Enqueue(PurgeItem{ConversationID: testCid, ChannelID: "gemini-web", Reason: "session_closed"})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != PurgeStatePending {
		t.Fatalf("入列状态=%q", first.State)
	}
	again, err := q.Enqueue(PurgeItem{ConversationID: testCid, ChannelID: "gemini-web"})
	if err != nil {
		t.Fatal(err)
	}
	if again.EnqueuedAt.UnixNano() != first.EnqueuedAt.UnixNano() || len(q.List()) != 1 {
		t.Fatal("同一会话重复入列必须幂等")
	}
	// 删除成功 ⇒ deleted（销账）；失败 ⇒ failed 但仍在待删除集合里继续重试。
	if err := q.Settle(testCid, PurgeStateDeleted, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(q.Pending()); n != 0 {
		t.Fatalf("deleted 项不应算待删除：%d", n)
	}
	if err := q.Settle(testCid, PurgeStateFailed, "站点返回 2xx 但侧栏仍列出该会话"); err != nil {
		t.Fatal(err)
	}
	items := q.List()
	if len(items) != 1 || items[0].Attempts != 2 || items[0].LastError == "" {
		t.Fatalf("Settle 记账不对: %+v", items)
	}
	requeued, err := q.Enqueue(PurgeItem{ConversationID: testCid, ChannelID: "gemini-web"})
	if err != nil {
		t.Fatal(err)
	}
	if requeued.State != PurgeStatePending || requeued.LastError != "" {
		t.Fatalf("failed 项重新入列应回到 pending 且清空错误: %+v", requeued)
	}
	if n := len(q.Pending()); n != 1 {
		t.Fatalf("待删除数=%d", n)
	}
	if _, err := q.Enqueue(PurgeItem{ConversationID: testCid}); err != nil {
		t.Fatal(err)
	}
	if n := len(q.Pending()); n != 1 {
		t.Fatalf("重新入列后待删除数=%d", n)
	}
	// 落盘 + 重载。
	reloaded := NewPurgeQueue(path)
	got := reloaded.List()
	if len(got) != 1 || got[0].ConversationID != testCid || got[0].State != PurgeStatePending {
		t.Fatalf("重载结果=%+v", got)
	}
	ok, err := reloaded.Forget(testCid)
	if err != nil || !ok {
		t.Fatalf("Forget ok=%v err=%v", ok, err)
	}
	if ok, _ := reloaded.Forget(testCid); ok {
		t.Fatal("重复 Forget 应返回 false")
	}
	if len(NewPurgeQueue(path).List()) != 0 {
		t.Fatal("Forget 必须落盘")
	}
}

// ---------- 立即删除 ----------

func TestPurgeConversationVerified(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeEvaluator(t, "/app/25096c6ba2166662", `{"httpStatus":200,"domListed":false,"requestId":3148101}`)
	gw := newPurgeGateway(t, engine)
	addLiveSession(gw, "s-purge", "gemini-web")

	item, res, err := gw.PurgeConversation(context.Background(), PurgeItem{SessionID: "s-purge", ConversationID: "/app/25096c6ba2166662"})
	if err != nil {
		t.Fatalf("立即删除失败: %v", err)
	}
	if !res.Verified || item.ConversationID != testCid {
		t.Fatalf("res=%+v item=%+v", res, item)
	}
	items := gw.purge.List()
	if len(items) != 1 || items[0].State != PurgeStateDeleted {
		t.Fatalf("销账记录=%+v", items)
	}
	if n := len(gw.purge.Pending()); n != 0 {
		t.Fatalf("待删除数=%d（成功不该留重试）", n)
	}
	// 会话标签页不能被清账动作关掉（那是调用方的语义）。
	if len(engine.closeNames) != 0 {
		t.Fatalf("不应关闭已存在的会话标签页: %v", engine.closeNames)
	}
}

func TestPurgeConversationUnverifiedFallsBackToQueue(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeEvaluator(t, "/app/25096c6ba2166662", `{"httpStatus":200,"domListed":true}`)
	gw := newPurgeGateway(t, engine)
	addLiveSession(gw, "s-purge", "gemini-web")

	if _, _, err := gw.PurgeConversation(context.Background(), PurgeItem{SessionID: "s-purge", ConversationID: testCid}); err == nil {
		t.Fatal("侧栏仍列出时必须报错，不得谎报成功")
	}
	pending := gw.purge.Pending()
	if len(pending) != 1 || pending[0].ConversationID != testCid || pending[0].State != PurgeStateFailed {
		t.Fatalf("应落待删除列表（failed 也要继续重试）: %+v", gw.purge.List())
	}
	if pending[0].LastError == "" || pending[0].Attempts != 1 {
		t.Fatalf("失败原因与尝试次数要留痕: %+v", pending[0])
	}
}

func TestConversationIDForSessionOnNewChat(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeEvaluator(t, "/app", "")
	gw := newPurgeGateway(t, engine)
	addLiveSession(gw, "s-sniff", "gemini-web")
	if _, err := gw.ConversationIDForSession(context.Background(), "s-sniff"); err == nil {
		t.Fatal("新对话（无服务端会话 ID）必须报错而不是猜一个 ID")
	}
}

// ---------- 后台清账 ----------

func TestDrainPurgeCreatesTempSessionThenClosesIt(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeEvaluator(t, "/app/25096c6ba2166662", `{"httpStatus":200,"domListed":false}`)
	gw := newPurgeGateway(t, engine)
	if _, err := gw.purge.Enqueue(PurgeItem{ConversationID: testCid, ChannelID: "gemini-web", Reason: "session_closed"}); err != nil {
		t.Fatal(err)
	}

	rep := gw.DrainPurge(context.Background(), 0)
	if rep.Attempted != 1 || rep.Deleted != 1 || rep.Failed != 0 || rep.Skipped != 0 {
		t.Fatalf("清账报告=%+v", rep)
	}
	if items := gw.purge.List(); len(items) != 1 || items[0].State != PurgeStateDeleted {
		t.Fatalf("清账后记录=%+v", items)
	}
	// 为此新建的临时标签页必须收尾关闭，且不能把「当前会话」留在清账用的会话上。
	if len(engine.closeNames) != 1 || engine.closeNames[0] != "purge-25096c6ba2166662" {
		t.Fatalf("临时标签页未按预期关闭: %v", engine.closeNames)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.current != "" {
		t.Fatalf("清账后 current=%q，应恢复原值", gw.current)
	}
	if _, ok := gw.sessions["purge-25096c6ba2166662"]; ok {
		t.Fatal("临时清账会话应被移除")
	}
}

// ---------- 端点 ----------

func TestPurgeEndpoints(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeEvaluator(t, "/app/25096c6ba2166662", `{"httpStatus":200,"domListed":false}`)
	gw := newPurgeGateway(t, engine)
	addLiveSession(gw, "s-purge", "gemini-web")
	gw.bindSessionConversation("s-purge", testCid)
	h := NewHandler(gw, nil)

	rec := doJSON(t, h, http.MethodGet, "/v1/purge", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/purge status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 异步入列（显式给出 conversation_id）。
	rec = doJSON(t, h, http.MethodPost, "/v1/purge", map[string]any{"conversation_id": "/app/25096c6ba2166662", "channel_id": "gemini-web", "async": true})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("异步入列 status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 同步删除：会话标签页还在 ⇒ 立即删掉并销账。
	rec = doJSON(t, h, http.MethodPost, "/v1/purge", map[string]any{"session_id": "s-purge"})
	if rec.Code != http.StatusOK {
		t.Fatalf("立即删除 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Status != "deleted" {
		t.Fatalf("立即删除返回=%s err=%v", rec.Body.String(), err)
	}

	// 人工销账 + 重试。
	rec = doJSON(t, h, http.MethodDelete, "/v1/purge/"+testCid, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/purge/{cid} status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodDelete, "/v1/purge/"+testCid, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复销账应 404，得到 %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPost, "/v1/purge/"+testCid, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/purge/{cid} status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPost, "/v1/purge/drain?max=5", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drain status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodGet, "/v1/purge", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted"`) {
		t.Fatalf("列表应含 deleted 记录: %s", rec.Body.String())
	}
}

func TestPurgeDisabledReturns501(t *testing.T) {
	cfg := DefaultConfig("127.0.0.1:50051")
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	stable := time.Duration(0)
	gw, err := NewGateway(&cfg, Options{
		EngineFactory:  func(string) (EngineClient, error) { return newFakeEngine(), nil },
		AnswerStable:   &stable,
		Sleep:          func(time.Duration) {},
		AuthStatePath:  filepath.Join(t.TempDir(), "gateway-auth-state.json"),
		PurgeQueuePath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gw.purge != nil {
		t.Fatal("PurgeQueuePath=\"-\" 应关闭待删除列表")
	}
	rec := doJSON(t, NewHandler(gw, nil), http.MethodGet, "/v1/purge", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("关闭时 status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- 会话删除端点挂钩 ----------

func TestSessionsDeleteWithPurge(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeEvaluator(t, "/app/25096c6ba2166662", `{"httpStatus":200,"domListed":false}`)
	gw := newPurgeGateway(t, engine)
	addLiveSession(gw, "s-purge", "gemini-web")
	gw.bindSessionConversation("s-purge", testCid)
	h := NewHandler(gw, nil)

	rec := doJSON(t, h, http.MethodDelete, "/v1/sessions/s-purge?purge=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE session status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status string         `json:"status"`
		Purge  map[string]any `json:"purge"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "deleted" || out.Purge["status"] != "deleted" {
		t.Fatalf("响应=%s", rec.Body.String())
	}
	if items := gw.purge.List(); len(items) != 1 || items[0].State != PurgeStateDeleted {
		t.Fatalf("会话删除应同时销账: %+v", items)
	}
	// 2026-10-03 起（派单 d27954eeb）：不带参数**不再**默认清账，必须显式 purge=1。
	addLiveSession(gw, "s-default", "gemini-web")
	gw.bindSessionConversation("s-default", testCid)
	rec = doJSON(t, h, http.MethodDelete, "/v1/sessions/s-default", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "purge") {
		t.Fatalf("默认不得清账（须显式 purge=1）: %d %s", rec.Code, rec.Body.String())
	}
	// ?purge=0 显式只关页、不清账。
	addLiveSession(gw, "s-plain", "gemini-web")
	rec = doJSON(t, h, http.MethodDelete, "/v1/sessions/s-plain?purge=0", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "purge") {
		t.Fatalf("?purge=0 不应清账: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------- 千问（2026-10-02 接站第一站；契约 §13/§14、案卷 §十五） ----------

const testQianwenCid = "d53fa55f9a434b5986f283647b1d2a1b"

func TestNormalizeConversationIDQianwen(t *testing.T) {
	cases := map[string]string{
		testQianwenCid:                  testQianwenCid,
		"/chat/" + testQianwenCid:       testQianwenCid,
		"/chat/" + testQianwenCid + "/": testQianwenCid,
		"https://www.qianwen.com/chat/" + testQianwenCid + "?ch=1": testQianwenCid,
		"  D53FA55F9A434B5986F283647B1D2A1B  ":                     testQianwenCid,
	}
	for raw, want := range cases {
		got, err := NormalizeConversationID(raw)
		if err != nil || got != want {
			t.Fatalf("NormalizeConversationID(%q)=%q,%v，期望 %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"/chat/abc", "d53fa55f9a434b5986f283647b1d2a", "d53fa55f9a434b5986f283647b1d2a1bzz"} {
		if got, err := NormalizeConversationID(raw); err == nil {
			t.Fatalf("NormalizeConversationID(%q) 期望报错，得到 %q", raw, got)
		}
	}
}

func TestBuildQianwenDeleteScript(t *testing.T) {
	script := BuildQianwenDeleteScript(testQianwenCid)
	for _, want := range []string{
		"session/delete/batch", testQianwenCid, "session_ids", "qwenSign",
		"__QIANWEN_CHAT_SDK__", "chat2-api.qianwen.com", "session/page/list", "session/get", "domListed",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("千问删除脚本缺少 %q:\n%s", want, script)
		}
	}
	// 地址写法必须收敛到同一脚本；非法 ID 一律 fail-closed。
	if alt := BuildQianwenDeleteScript("/chat/" + testQianwenCid); alt != script {
		t.Fatal("同一会话的不同写法应生成同一脚本")
	}
	if bad := BuildQianwenDeleteScript("c_25096c6ba2166662"); !strings.Contains(bad, "invalid_conversation_id") {
		t.Fatalf("Gemini 形状的 ID 不该被千问脚本接受:\n%s", bad)
	}
}

func TestPurgeChannelInferenceByShape(t *testing.T) {
	g := &Gateway{cfg: ptrConfig(DefaultConfig("127.0.0.1:50051"))}
	if ch := g.purgeChannelFor("c_25096c6ba2166662"); ch == nil || ch.ID != "gemini-web" {
		t.Fatalf("16hex 应推断到 gemini-web，得到 %+v", ch)
	}
	if ch := g.purgeChannelFor(testQianwenCid); ch == nil || ch.ID != "qianwen-web" {
		t.Fatalf("32hex 应推断到 qianwen-web，得到 %+v", ch)
	}
	if ch := g.purgeChannelFor("c_0000000000000000"); ch == nil || ch.ID != "gemini-web" {
		t.Fatalf("零值 16hex 仍应推断到 gemini-web，得到 %+v", ch)
	}
}

func ptrConfig(c Config) *Config { return &c }
