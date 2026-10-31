package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 2026-10-03 派单 d8657acc9：「调试保留」标志——开着时任何关会话清账都**不删**站点对话，
// 历史留站点侧供 review；未设时行为逐字不变（默认仍是删）。

func newKeepGateway(t *testing.T, engine *fakeEngine, keep bool) *Gateway {
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
		KeepSessions:   keep,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

func matchingPurgeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	engine := newFakeEngine()
	engine.evaluateFunc = purgeTargetEvaluator(t, "/app/25096c6ba2166662", []map[string]string{
		{"href": "/app/25096c6ba2166662", "title": "调试保留验证"},
	})
	return engine
}

// 标志开：关会话清账不删站点对话，回 skipped/debug_keep，队列里不留任何删除记录。
func TestKeepSessionsSkipsPurgeOnClose(t *testing.T) {
	gw := newKeepGateway(t, matchingPurgeEngine(t), true)
	addBoundSession(gw, "s-keep", targetCid)

	info := gw.purgeSessionOnClose(context.Background(), "s-keep", true)
	if info["status"] != "skipped" || info["reason"] != "debug_keep" {
		t.Fatalf("调试保留下必须 skipped/debug_keep: %+v", info)
	}
	if info["conversation_id"] != targetCid {
		t.Fatalf("日志与响应都要带 conv 便于 review: %+v", info)
	}
	if n := len(gw.purge.List()); n != 0 {
		t.Fatalf("调试保留不得产生任何删除记录: %d 条 %+v", n, gw.purge.List())
	}
}

// 请求级 ?keep=1 覆盖进程默认（网关未开调试保留时也能单请求保留）。
func TestKeepSessionsRequestOverrideOnDelete(t *testing.T) {
	gw := newKeepGateway(t, matchingPurgeEngine(t), false)
	addBoundSession(gw, "s-keepreq", targetCid)
	h := NewHandler(gw, nil)

	rec := doJSON(t, h, http.MethodDelete, "/v1/sessions/s-keepreq?purge=1&keep=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"debug_keep"`) {
		t.Fatalf("?keep=1 应保留站点对话: %s", rec.Body.String())
	}
	if n := len(gw.purge.List()); n != 0 {
		t.Fatalf("不得产生删除记录: %+v", gw.purge.List())
	}
	if len(gw.ListSessions().Sessions) != 0 {
		t.Fatalf("网关会话本身仍应解绑（调试保留只挡站点对话删除）: %+v", gw.ListSessions().Sessions)
	}
}

// 请求级 ?keep=0 覆盖进程级开启：网关开着也能单次显式删（review 完想清掉时用）。
func TestKeepSessionsRequestCanOverrideOff(t *testing.T) {
	gw := newKeepGateway(t, matchingPurgeEngine(t), true)
	addBoundSession(gw, "s-keepoff", targetCid)
	h := NewHandler(gw, nil)

	rec := doJSON(t, h, http.MethodDelete, "/v1/sessions/s-keepoff?purge=1&keep=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"deleted"`) {
		t.Fatalf("?keep=0 应恢复删除: %s", rec.Body.String())
	}
	if n := len(gw.purge.Pending()); n != 0 {
		t.Fatalf("匹配删除应直接销账: %+v", gw.purge.List())
	}
}

// 环境变量 UWS_KEEP_SESSIONS=1 即进程级开启；默认（未设）必须是关。
func TestKeepSessionsFromEnv(t *testing.T) {
	t.Setenv(KeepSessionsEnv, "1")
	gw := newKeepGateway(t, matchingPurgeEngine(t), false)
	if !gw.keepSessions {
		t.Fatal("UWS_KEEP_SESSIONS=1 应置位进程级调试保留")
	}
	t.Setenv(KeepSessionsEnv, "")
	gw2 := newKeepGateway(t, matchingPurgeEngine(t), false)
	if gw2.keepSessions {
		t.Fatal("未设环境变量时必须默认关（不得反转默认）")
	}
}

// 头 x-uws-keep-sessions: 1 与 ?keep=1 同义（调用侧不改进程环境也能逐请求声明）。
func TestKeepSessionsHeaderOverride(t *testing.T) {
	gw := newKeepGateway(t, matchingPurgeEngine(t), false)
	addBoundSession(gw, "s-keephdr", targetCid)
	h := NewHandler(gw, nil)

	req := httptest.NewRequest(http.MethodDelete, "/v1/sessions/s-keephdr?purge=1", nil)
	req.Header.Set(KeepSessionsHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"debug_keep"`) {
		t.Fatalf("头声明应保留站点对话: %s", rec.Body.String())
	}
}
