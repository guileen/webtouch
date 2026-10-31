package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// T-34：关页失败不得出账 —— 账本条目保留，页签仍有落点（启动回收 / 调试端点）。
func TestCloseSessionKeepsLedgerWhenEngineFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-tabs.json")
	engine := newFakeEngine()
	gw := newTestGatewayOpts(t, engine, Options{
		TabsStatePath: path,
		AuthStatePath: filepath.Join(dir, "auth.json"),
	})
	if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "hi"})); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if led := readLedger(t, path); led.Targets[DefaultSessionID] == "" {
		t.Fatalf("会话页签未记账: %+v", led.Targets)
	}

	engine.failAlways["close_context"] = &EngineError{Code: codeInternal, Reason: "引擎暂不可用"}
	tabClosed, err := gw.closeSession(context.Background(), DefaultSessionID)
	if tabClosed || err == nil {
		t.Fatalf("关页失败应报 tab_closed=false ＋ 原因: %v %v", tabClosed, err)
	}
	if led := readLedger(t, path); led.Targets[DefaultSessionID] == "" {
		t.Fatalf("关页失败必须保留账本条目（否则页签成永久孤儿）: %+v", led.Targets)
	}
	if snap := gw.ListSessions(); len(snap.Sessions) != 0 {
		t.Fatalf("网关会话本身应已出账: %+v", snap.Sessions)
	}

	// 关不掉的那条仍能被启动回收补关（同一 helper 走调试端点）。
	var closed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/json/close/") {
			closed = append(closed, strings.TrimPrefix(r.URL.Path, "/json/close/"))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	gw.attachCDP = srv.URL
	if n := gw.ReapOrphanTabs(context.Background()); n != 1 {
		t.Fatalf("启动回收应补关 1 个页签（closed=%v）", closed)
	}
	if led := readLedger(t, path); len(led.Targets) != 0 {
		t.Fatalf("补关成功后应出账: %+v", led.Targets)
	}
}

// 成功路径：关页成功才出账，且返回值确认页签已关。
func TestCloseSessionPrunesLedgerOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-tabs.json")
	gw := newTestGatewayOpts(t, newFakeEngine(), Options{
		TabsStatePath: path,
		AuthStatePath: filepath.Join(dir, "auth.json"),
	})
	if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "hi"})); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	tabClosed, err := gw.closeSession(context.Background(), DefaultSessionID)
	if !tabClosed || err != nil {
		t.Fatalf("成功关页应报 tab_closed=true: %v %v", tabClosed, err)
	}
	if led := readLedger(t, path); len(led.Targets) != 0 {
		t.Fatalf("成功关页后应出账: %+v", led.Targets)
	}
}

// DELETE /v1/sessions 响应必须区分「页签已关／未关」（T-34）。
func TestDeleteSessionResponseReportsTabState(t *testing.T) {
	h, fake := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{
		"channel_id": "deepseek-web", "session_id": "s-tabstate",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("建会话失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodDelete, "/v1/sessions/s-tabstate?purge=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("关会话失败: %d %s", rec.Code, rec.Body.String())
	}
	var ok map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatal(err)
	}
	if ok["tab_closed"] != true {
		t.Fatalf("成功路径应报 tab_closed=true: %v", ok)
	}
	if _, has := ok["tab_error"]; has {
		t.Fatalf("成功路径不该带 tab_error: %v", ok)
	}

	rec = doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{
		"channel_id": "deepseek-web", "session_id": "s-tabstate",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("重建会话失败: %d %s", rec.Code, rec.Body.String())
	}
	fake.failAlways["close_context"] = &EngineError{Code: codeInternal, Reason: "引擎失联"}
	rec = doJSON(t, h, http.MethodDelete, "/v1/sessions/s-tabstate?purge=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除本身仍应 200（会话已出账）: %d %s", rec.Code, rec.Body.String())
	}
	var bad map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &bad); err != nil {
		t.Fatal(err)
	}
	if bad["tab_closed"] != false || bad["tab_retained"] != true {
		t.Fatalf("失败路径应报 tab_closed=false ＋ tab_retained=true: %v", bad)
	}
	if s, _ := bad["tab_error"].(string); !strings.Contains(s, "页签") {
		t.Fatalf("应给出关页失败原因: %v", bad)
	}
}

// 缺口③（2026-10-02 派单 d1f5493c6 登记、2026-10-03 d613c7908 收口）：**网关没有内存会话**时
// （网关进程重启过、页签是上一次留下的），DELETE /v1/sessions/<sid> 也必须让引擎删掉
// 「会话名 → Target」落盘绑定——否则下次同名会话还会去接管一扇已经关掉的窗。
func TestCloseSessionDropsEngineBindingWithoutMemorySession(t *testing.T) {
	dir := t.TempDir()
	engine := newFakeEngine()
	gw := newTestGatewayOpts(t, engine, Options{
		TabsStatePath: filepath.Join(dir, "gateway-tabs.json"),
		AuthStatePath: filepath.Join(dir, "auth.json"),
	})
	_, tabClosed, err := gw.closeSessionWithPurge(context.Background(), "s-orphan", false, false)
	if !tabClosed || err != nil {
		t.Fatalf("无内存会话且账本无条目时应报「页签已关」: tabClosed=%v err=%v", tabClosed, err)
	}
	for _, n := range engine.closeNames {
		if n == "s-orphan" {
			return
		}
	}
	t.Fatalf("无内存会话时也必须通知引擎清绑定，实际 closeNames=%v", engine.closeNames)
}
