package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestGatewayOpts(t *testing.T, engine *fakeEngine, opts Options) *Gateway {
	t.Helper()
	cfg := DefaultConfig("127.0.0.1:50051")
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	stable := time.Duration(0)
	opts.EngineFactory = func(string) (EngineClient, error) { return engine, nil }
	opts.AnswerStable = &stable
	if opts.Sleep == nil {
		opts.Sleep = func(time.Duration) {}
	}
	gw, err := NewGateway(&cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

func readLedger(t *testing.T, path string) tabsLedger {
	t.Helper()
	var led tabsLedger
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return led
		}
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &led); err != nil {
		t.Fatalf("账本解析失败: %v", err)
	}
	return led
}

// TestTabLedgerRecordsAndPrunes 网关开的页签要记账，会话关闭要出账。
func TestTabLedgerRecordsAndPrunes(t *testing.T) {
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
	led := readLedger(t, path)
	if led.Targets[DefaultSessionID] == "" {
		t.Fatalf("会话页签未记账: %+v", led.Targets)
	}
	gw.closeSession(context.Background(), DefaultSessionID)
	if led = readLedger(t, path); len(led.Targets) != 0 {
		t.Fatalf("会话关闭后应出账，仍有 %v", led.Targets)
	}
}

// TestReapOrphanTabsClosesOnlyLedgerTabs 启动回收只关账本里的页。
func TestReapOrphanTabsClosesOnlyLedgerTabs(t *testing.T) {
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

	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-tabs.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"targets":{"s-old":"TAB-A","s-old2":"TAB-B"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	gw := newTestGatewayOpts(t, newFakeEngine(), Options{
		AttachCDP:           srv.URL,
		BrowserHostInterval: -1,
		TabsStatePath:       path,
		AuthStatePath:       filepath.Join(dir, "auth.json"),
	})
	if n := gw.ReapOrphanTabs(context.Background()); n != 2 {
		t.Fatalf("回收数=%d, want 2（closed=%v）", n, closed)
	}
	if len(closed) != 2 {
		t.Fatalf("应关 2 个页签: %v", closed)
	}
	if led := readLedger(t, path); len(led.Targets) != 0 {
		t.Fatalf("回收成功后账本应清空: %v", led.Targets)
	}
}

// TestReapOrphanTabsKeepsFailedEntries 关闭失败（Chrome 未就绪）时保留账本，下次再试。
func TestReapOrphanTabsKeepsFailedEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-tabs.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"targets":{"s-old":"TAB-A"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	gw := newTestGatewayOpts(t, newFakeEngine(), Options{
		AttachCDP:           srv.URL,
		BrowserHostInterval: -1,
		TabsStatePath:       path,
		AuthStatePath:       filepath.Join(dir, "auth.json"),
	})
	if n := gw.ReapOrphanTabs(context.Background()); n != 0 {
		t.Fatalf("回收数=%d, want 0", n)
	}
	if led := readLedger(t, path); led.Targets["s-old"] != "TAB-A" {
		t.Fatalf("失败条目应保留: %v", led.Targets)
	}
}
