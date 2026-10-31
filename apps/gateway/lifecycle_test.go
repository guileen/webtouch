package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newLifecycleGateway 构造一个带空闲阈值、关掉后台协程的网关（回收由测试显式驱动）。
func newLifecycleGateway(t *testing.T, engine *fakeEngine, ttl time.Duration) *Gateway {
	t.Helper()
	gw := newPurgeGateway(t, engine)
	gw.sessionIdleTTL = ttl
	return gw
}

func addIdleSession(gw *Gateway, sid, channelID string, idle time.Time) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	created := idle.Add(-time.Minute)
	gw.sessions[sid] = &Session{
		ID: sid, ChannelID: channelID, EngineAddr: "127.0.0.1:50051",
		ContextID: "ctx-" + sid, TargetID: "tgt-" + sid,
		CreatedAt: created, LastUsedAt: idle,
	}
}

// TestReapIdleSessions 空闲超过 TTL 的会话被收口：先删站点会话（Gemini 支持）再关 Tab。
func TestReapIdleSessions(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeEvaluator(t, "/app/25096c6ba2166662", `{"httpStatus":200,"domListed":false}`)
	gw := newLifecycleGateway(t, engine, 10*time.Minute)
	addIdleSession(gw, "s-idle", "gemini-web", time.Now().Add(-30*time.Minute))
	addLiveSession(gw, "s-fresh", "gemini-web")
	gw.bindSessionConversation("s-idle", testCid)

	reaped := gw.reapIdleSessions(time.Now())
	if len(reaped) != 1 || reaped[0] != "s-idle" {
		t.Fatalf("期望只回收 s-idle，实际 %v", reaped)
	}
	if _, ok := gw.sessions["s-idle"]; ok {
		t.Fatal("回收后会话应从映射中消失")
	}
	if _, ok := gw.sessions["s-fresh"]; !ok {
		t.Fatal("未空闲的会话不得被回收")
	}
	if len(engine.closeNames) != 1 || engine.closeNames[0] != "s-idle" {
		t.Fatalf("应调用 CloseContext 关页，实际 %v", engine.closeNames)
	}
	items := gw.purge.List()
	if len(items) != 1 || items[0].ChannelID != "gemini-web" {
		t.Fatalf("Gemini 会话收口应带上清账，实际 %+v", items)
	}
}

// TestReapSkipsBusySession 有在途请求（会话锁被占）的会话不得被回收打断。
func TestReapSkipsBusySession(t *testing.T) {
	gw := newLifecycleGateway(t, newFakeEngine(), 10*time.Minute)
	addIdleSession(gw, "s-busy", "gemini-web", time.Now().Add(-30*time.Minute))

	unlock := gw.sessionLock("s-busy")
	reaped := gw.reapIdleSessions(time.Now())
	unlock()
	if len(reaped) != 0 {
		t.Fatalf("在途会话不应被回收，实际 %v", reaped)
	}
	if _, ok := gw.sessions["s-busy"]; !ok {
		t.Fatal("在途会话必须保留")
	}
	if len(gw.reapIdleSessions(time.Now())) != 1 {
		t.Fatal("锁释放后应可回收")
	}
}

// TestReapDisabledWhenTTLZero ttl<=0 = 关闭回收（不启动也不回收）。
func TestReapDisabledWhenTTLZero(t *testing.T) {
	gw := newLifecycleGateway(t, newFakeEngine(), 0)
	addIdleSession(gw, "s-idle", "gemini-web", time.Now().Add(-24*time.Hour))
	if got := gw.reapIdleSessions(time.Now()); len(got) != 0 {
		t.Fatalf("ttl=0 时不应回收，实际 %v", got)
	}
}

// lifecycleEvaluator 组合页面：登录探针 logged_in + 会话 URL + Gemini 删除 RPC。
func lifecycleEvaluator(t *testing.T, pathname, deleteJSON string) func(ActionRequest) (ActionResult, error) {
	t.Helper()
	return func(req ActionRequest) (ActionResult, error) {
		switch {
		case strings.Contains(req.Value, "__uws_auth_probe"):
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		case req.Value == "location.pathname":
			b, _ := json.Marshal(pathname)
			return ActionResult{Success: true, ExtractedData: string(b)}, nil
		case strings.Contains(req.Value, "GzXR5e"):
			return ActionResult{Success: true, ExtractedData: deleteJSON}, nil
		default:
			return ActionResult{Success: true, ExtractedData: `{"status":"not_dispatched"}`}, nil
		}
	}
}

// TestFinishOneShotClosesSession 一次性会话收口：先删站点会话（Gemini 支持）再关 Tab。
// 失败轮（roundErr != nil）不收口——留给重试与空闲兜底。
func TestFinishOneShotClosesSession(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = lifecycleEvaluator(t, "/app/25096c6ba2166662", `{"httpStatus":200,"domListed":false}`)
	gw := newLifecycleGateway(t, engine, 0)
	addLiveSession(gw, "s-once", "gemini-web")
	gw.bindSessionConversation("s-once", testCid)

	gw.finishOneShot(context.Background(), "s-once", nil)
	if _, ok := gw.sessions["s-once"]; ok {
		t.Fatal("一次性会话应在轮次结束后被收口")
	}
	if len(engine.closeNames) != 1 || engine.closeNames[0] != "s-once" {
		t.Fatalf("应调用 CloseContext 关页，实际 %v", engine.closeNames)
	}
	items := gw.purge.List()
	if len(items) != 1 || items[0].State != PurgeStateDeleted {
		t.Fatalf("应留下已删除的清账记录，实际 %+v", items)
	}

	addLiveSession(gw, "s-retry", "gemini-web")
	gw.finishOneShot(context.Background(), "s-retry", context.DeadlineExceeded)
	if _, ok := gw.sessions["s-retry"]; !ok {
		t.Fatal("失败轮不应收口")
	}
}

// TestParseCloseAfterRequest close_after 经 body 与 x-uws-close-after 头解析到请求（契约 §9）。
func TestParseCloseAfterRequest(t *testing.T) {
	h := &Handler{g: newLifecycleGateway(t, newFakeEngine(), 0)}
	req := httptest.NewRequest(http.MethodPost, "/internal/provider/chat",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"close_after":true}`))
	parsed, err := h.parseChatRequest(req, "deepseek-web")
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.CloseAfter {
		t.Fatal("body 的 close_after=true 应被解析")
	}
	req2 := httptest.NewRequest(http.MethodPost, "/internal/provider/chat",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("x-uws-close-after", "1")
	parsed2, err := h.parseChatRequest(req2, "deepseek-web")
	if err != nil {
		t.Fatal(err)
	}
	if !parsed2.CloseAfter {
		t.Fatal("x-uws-close-after 头应覆盖 body")
	}
}

// TestDeleteSessionRequiresExplicitPurge 2026-10-03 起：DELETE 默认只解绑，
// 删除必须显式 purge=1（且目标须过「会话绑定对话 == 页签当前对话」核对）。
// 起因＝默认清账目标取自页签地址、被站点 SPA 跳转带偏，误删一条不可恢复的真实对话。
func TestDeleteSessionRequiresExplicitPurge(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeEvaluator(t, "/app/25096c6ba2166662", `{"httpStatus":200,"domListed":false}`)
	gw := newLifecycleGateway(t, engine, 0)
	addLiveSession(gw, "s-del", "gemini-web")
	gw.bindSessionConversation("s-del", testCid)
	h := NewHandler(gw, nil)

	rec := doJSON(t, h, http.MethodDelete, "/v1/sessions/s-del", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(gw.purge.List()) != 0 {
		t.Fatalf("默认不得清账（须显式 purge=1），实际 %+v", gw.purge.List())
	}

	addLiveSession(gw, "s-del2", "gemini-web")
	gw.bindSessionConversation("s-del2", testCid)
	rec = doJSON(t, h, http.MethodDelete, "/v1/sessions/s-del2?purge=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(gw.purge.List()) != 0 {
		t.Fatalf("purge=0 不应新增清账，实际 %+v", gw.purge.List())
	}
}

// TestSiteTurnSerializesSameSite 同站点串行 + 最小间隔；不同站点互不阻塞。
func TestSiteTurnSerializesSameSite(t *testing.T) {
	t.Setenv(siteMinIntervalEnv, "0")
	gw := newLifecycleGateway(t, newFakeEngine(), 0)
	release, err := gw.siteTurn(context.Background(), "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		rel2, err := gw.siteTurn(context.Background(), "deepseek")
		if err == nil {
			rel2()
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("同站第二轮的取权不应在首轮释放前完成")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("首轮释放后同站第二轮应放行")
	}
}

// TestSiteTurnEnforcesMinInterval 最小间隔：同站两轮之间必须等待。
func TestSiteTurnEnforcesMinInterval(t *testing.T) {
	t.Setenv(siteMinIntervalEnv, "1")
	gw := newLifecycleGateway(t, newFakeEngine(), 0)
	var slept []time.Duration
	gw.sleep = func(d time.Duration) { slept = append(slept, d) }
	release, err := gw.siteTurn(context.Background(), "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := gw.siteTurn(context.Background(), "deepseek"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] <= 0 || slept[0] > time.Second {
		t.Fatalf("同站第二轮应等待 [0,1s]，实际 %v", slept)
	}
}

// TestSiteMinIntervalDefaults DeepSeek 默认更保守（用户口径：不要太频繁）。
func TestSiteMinIntervalDefaults(t *testing.T) {
	t.Setenv(siteMinIntervalEnv, "")
	if got := siteMinInterval("deepseek"); got != defaultDeepSeekSiteMinInterval {
		t.Fatalf("deepseek 默认间隔 = %s，期望 %s", got, defaultDeepSeekSiteMinInterval)
	}
	if got := siteMinInterval("gemini"); got != defaultSiteMinInterval {
		t.Fatalf("gemini 默认间隔 = %s，期望 %s", got, defaultSiteMinInterval)
	}
	if got := siteMinInterval("deepseek"); got <= siteMinInterval("gemini") {
		t.Fatal("DeepSeek 间隔应比其它站点更保守")
	}
}

// TestSessionIdleTtlFromOptions Options 提供的阈值生效；<0 关闭回收。
func TestSessionIdleTtlFromOptions(t *testing.T) {
	cfg := DefaultConfig("127.0.0.1:50051")
	stable := time.Duration(0)
	gw, err := NewGateway(&cfg, Options{
		EngineFactory:  func(string) (EngineClient, error) { return newFakeEngine(), nil },
		AnswerStable:   &stable,
		Sleep:          func(time.Duration) {},
		AuthStatePath:  filepath.Join(t.TempDir(), "auth.json"),
		PurgeQueuePath: filepath.Join(t.TempDir(), "purge.json"),
		PurgeInterval:  -1,
		SessionIdleTTL: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gw.sessionIdleTTL != 0 {
		t.Fatalf("<0 应关闭回收，实际 %s", gw.sessionIdleTTL)
	}
}

// TestSiteCircuitBreakerBans 验证站点熔断器触发后拒绝后续轮次，且错误码正确
func TestSiteCircuitBreakerBans(t *testing.T) {
	gw := newLifecycleGateway(t, newFakeEngine(), 0)
	// 触发禁言
	gw.TripSiteBreaker("deepseek", 100*time.Millisecond, "账号已被禁言")

	_, err := gw.siteTurn(context.Background(), "deepseek")
	if err == nil {
		t.Fatal("熔断期间应拒绝请求")
	}
	ce, ok := err.(*ChatError)
	if !ok || ce.Code != errCodeAccountBanned {
		t.Fatalf("期望 errCodeAccountBanned，实际 %+v", err)
	}

	// 等待冷却过后自动恢复
	time.Sleep(120 * time.Millisecond)
	rel, err := gw.siteTurn(context.Background(), "deepseek")
	if err != nil {
		t.Fatalf("冷却后应恢复放行，实际: %v", err)
	}
	rel()
}
