// 会话续页（v1.5.7，GLOBAL #118）：引擎把「会话名 → Target」绑定落盘，重启失忆后
// 同名会话接管原来那扇窗口，而不是再开一扇新的。
//
// 现场问题（2026-09-22 用户反馈）：同一个任务每次执行都新开一扇窗口——浏览器的标签页
// 活得比引擎久，但引擎的会话→Target 映射只在内存里，一次 daemon restart 就丢干净。
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/protobuf/proto"
)

// recordingFactory 记录每次 Create 收到的 Config，并按需返回模拟会话。
type recordingFactory struct {
	mu   sync.Mutex
	cfgs []browser.Config
	next int
}

func (f *recordingFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	f.mu.Lock()
	f.cfgs = append(f.cfgs, cfg)
	f.next++
	id := f.next
	f.mu.Unlock()
	return NewSimCDPSession(fmt.Sprintf("rec-%d", id), cfg), nil
}

func (f *recordingFactory) last() browser.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cfgs) == 0 {
		return browser.Config{}
	}
	return f.cfgs[len(f.cfgs)-1]
}

func (f *recordingFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cfgs)
}

func TestSessionStoreRoundTripAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("OpenSessionStore: %v", err)
	}
	if err := store.Save(SessionBinding{
		Session: "default", TargetID: "t-1", URL: "https://example.com/",
		Attach: "http://127.0.0.1:9222", OwnWindow: true,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, ok := store.Lookup("default")
	if !ok || b.TargetID != "t-1" || !b.OwnWindow || b.UpdatedAt.IsZero() {
		t.Fatalf("Lookup 回读不一致: %+v ok=%v", b, ok)
	}
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("落盘文件权限应为 0600，实际 %o", mode)
	}

	// 引擎重启 = 重新打开同一路径：绑定必须还在（这正是续页的前提）。
	reopened, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	if b2, ok := reopened.Lookup("default"); !ok || b2.TargetID != "t-1" {
		t.Fatalf("重开后绑定丢失: %+v ok=%v", b2, ok)
	}

	// CloseContext 语义：会话结束，绑定必须删除，否则下次会去接管一扇已关的窗。
	if err := reopened.Delete("default"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	again, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("再次重开: %v", err)
	}
	if _, ok := again.Lookup("default"); ok {
		t.Fatal("删除后不该再读到绑定")
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestSessionStoreCorruptFileDoesNotBlockEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSessionStore(path); err == nil {
		t.Fatal("坏文件必须报错（调用方据此降级为不持久化），不能静默当成空文件")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("坏文件应被备份为 .corrupt: %v", err)
	}
	// 降级路径：内存态存储仍然可用，任务不受影响。
	mem := NewSessionStoreInMemory()
	if err := mem.Save(SessionBinding{Session: "s", TargetID: "t"}); err != nil {
		t.Fatalf("内存态 Save: %v", err)
	}
	if _, ok := mem.Lookup("s"); !ok {
		t.Fatal("内存态存储应可读回")
	}
	// nil 接收者安全（未启用持久化时调用方无需判空）。
	var nilStore *SessionStore
	if _, ok := nilStore.Lookup("s"); ok {
		t.Fatal("nil 存储不应报有绑定")
	}
	if err := nilStore.Save(SessionBinding{Session: "s", TargetID: "t"}); err != nil {
		t.Fatalf("nil 存储 Save 应为空操作: %v", err)
	}
}

func TestInitializeContextPersistsBindingAndCloseContextDeletesIt(t *testing.T) {
	store := NewSessionStoreInMemory()
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Sessions: store})
	name := "autodl"
	init, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId: "task-1", ContextName: proto.String(name),
		AttachCdpUrl: proto.String("http://127.0.0.1:9222"),
	})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	b, ok := store.Lookup(name)
	if !ok {
		t.Fatal("初始化后应落盘会话→Target 绑定")
	}
	if b.TargetID != init.GetTargetId() {
		t.Fatalf("绑定 target 应为 %q，实际 %q", init.GetTargetId(), b.TargetID)
	}
	if b.Attach != "http://127.0.0.1:9222" {
		t.Fatalf("绑定应记下附着端点（换浏览器即失效），实际 %q", b.Attach)
	}

	if _, err := env.client.CloseContext(mdCtx("coordinator-key"), &uwspb.CloseContextRequest{ContextName: name}); err != nil {
		t.Fatalf("CloseContext: %v", err)
	}
	if _, ok := store.Lookup(name); ok {
		t.Fatal("CloseContext 后绑定必须删除")
	}
}

// TestCloseContextDeletesBindingWithoutInMemoryContext 钉住 2026-10-03 的补丁：
// 引擎重启后内存里没有上下文，但落盘绑定还在（页签已经被关掉的那种死绑定）。
// CloseContext 必须也把它删掉，运维才不必为清死绑定去重启引擎。
func TestCloseContextDeletesBindingWithoutInMemoryContext(t *testing.T) {
	store := NewSessionStoreInMemory()
	if err := store.Save(SessionBinding{
		Session: "default", TargetID: "dead-target",
		Attach: "http://127.0.0.1:9222",
	}); err != nil {
		t.Fatal(err)
	}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Sessions: store})
	if _, err := env.client.CloseContext(mdCtx("coordinator-key"), &uwspb.CloseContextRequest{ContextName: "default"}); err != nil {
		t.Fatalf("CloseContext: %v", err)
	}
	if _, ok := store.Lookup("default"); ok {
		t.Fatal("无内存上下文时 CloseContext 也必须删除落盘绑定")
	}
}

func TestInitializeContextResumesPersistedTargetAfterRestart(t *testing.T) {
	store := NewSessionStoreInMemory()
	if err := store.Save(SessionBinding{
		Session: "default", TargetID: "kept-target",
		Attach: "http://127.0.0.1:9222", OwnWindow: true,
	}); err != nil {
		t.Fatal(err)
	}
	factory := &recordingFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Sessions: store, SessionFactory: factory})
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId: "task-after-restart", ContextName: proto.String("default"),
		AttachCdpUrl: proto.String("http://127.0.0.1:9222"),
	}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	got := factory.last()
	if got.ReuseTargetID != "kept-target" {
		t.Fatalf("重启后同名会话必须接管落盘的原 Target，实际 ReuseTargetID=%q（0 = 又开了新窗口）", got.ReuseTargetID)
	}
	if !got.ReuseOwnWindow {
		t.Fatal("续页应带上原窗口策略（own_window）")
	}
}

func TestInitializeContextDoesNotResumeAcrossDifferentBrowser(t *testing.T) {
	store := NewSessionStoreInMemory()
	if err := store.Save(SessionBinding{
		Session: "default", TargetID: "kept-target", Attach: "http://127.0.0.1:9222",
	}); err != nil {
		t.Fatal(err)
	}
	factory := &recordingFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Sessions: store, SessionFactory: factory})
	// 换了附着对象（用户日常 Chrome）：targetId 不再指向同一扇窗，绑定必须失效。
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId: "task-other-browser", ContextName: proto.String("default"),
		AttachCdpUrl: proto.String("http://127.0.0.1:9333"),
	}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if got := factory.last(); got.ReuseTargetID != "" {
		t.Fatalf("换了浏览器不得接管旧 Target，实际 %q", got.ReuseTargetID)
	}
}

func TestInitializeContextWithoutStoreKeepsOldBehaviour(t *testing.T) {
	factory := &recordingFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), SessionFactory: factory})
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId: "task-1", ContextName: proto.String("default"),
		AttachCdpUrl: proto.String("http://127.0.0.1:9222"),
	}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if factory.count() != 1 || factory.last().ReuseTargetID != "" {
		t.Fatalf("未配置存储时应保持旧行为（新建、不接管），实际 cfg=%+v", factory.last())
	}
}

// TestSessionBindingJSONShape 钉住落盘字段名：以后的迁移/外部工具按这个读。
func TestSessionBindingJSONShape(t *testing.T) {
	raw, err := json.Marshal(SessionBinding{Session: "s", TargetID: "t", OwnWindow: true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"session", "target_id", "own_window", "updated_at"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("落盘字段 %q 缺失: %s", k, raw)
		}
	}
}

// ── 绑定有效期（2026-09-22 用户定调）───────────────────────────────────────
//
// 用户口径：「孤儿窗自动回收没必要——万一它留在那里，用户可能还要看一看；可以设一个
// 超时时间。」所以超时收缩的是**接管资格**（引擎不再去接管太久以前的窗），不是窗口
// 生命周期：SessionStore 全程没有任何关窗能力，过期只删绑定条目。

func TestSessionStoreExpiredBindingIsNotResumable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("OpenSessionStore: %v", err)
	}
	base := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	store.SetTTL(time.Hour)
	if err := store.Save(SessionBinding{
		Session: "default", TargetID: "t-1", Attach: "http://127.0.0.1:9222",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := store.TTL(); got != time.Hour {
		t.Fatalf("TTL 应为 1h，实际 %s", got)
	}
	if _, ok := store.Lookup("default"); !ok {
		t.Fatal("刚写下的绑定必须可用于续页")
	}
	// 时间推进到 TTL 之外：绑定不再用于接管（窗口照旧留在浏览器里）。
	store.now = func() time.Time { return base.Add(2 * time.Hour) }
	if _, ok := store.Lookup("default"); ok {
		t.Fatal("超过 TTL 的绑定不得再用于接管")
	}
}

func TestSessionStoreSetTTLPrunesExpiredEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	old, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("OpenSessionStore: %v", err)
	}
	past := time.Now().Add(-72 * time.Hour)
	old.now = func() time.Time { return past }
	if err := old.Save(SessionBinding{
		Session: "stale", TargetID: "t-old", Attach: "http://127.0.0.1:9222",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 「引擎重启」：重开同一路径 + 配置 TTL ⇒ 过期条目当场清掉，文件不留残留。
	restarted, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	restarted.SetTTL(time.Hour)
	if _, ok := restarted.Lookup("stale"); ok {
		t.Fatal("启动时的过期清理应删掉旧绑定")
	}
	if got := restarted.Sessions(); len(got) != 0 {
		t.Fatalf("过期条目不应残留在诊断视图里: %+v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f sessionStoreFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("清理后仍应是合法 JSON: %s (%v)", raw, err)
	}
	if _, ok := f.Entries["stale"]; ok {
		t.Fatalf("过期条目不该留在文件里: %s", raw)
	}
}

func TestSessionStoreZeroTTLKeepsBindingForever(t *testing.T) {
	store := NewSessionStoreInMemory()
	base := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	store.SetTTL(0)
	if err := store.Save(SessionBinding{Session: "s", TargetID: "t"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	store.now = func() time.Time { return base.Add(365 * 24 * time.Hour) }
	if _, ok := store.Lookup("s"); !ok {
		t.Fatal("TTL=0 表示永不过期（旧行为）")
	}
}

// 引擎侧：过期绑定不得再触发接管，任务应老实新开一扇窗。
func TestInitializeContextDoesNotResumeExpiredBinding(t *testing.T) {
	store := NewSessionStoreInMemory()
	base := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	store.SetTTL(30 * time.Minute)
	if err := store.Save(SessionBinding{
		Session: "default", TargetID: "kept-target",
		Attach: "http://127.0.0.1:9222", OwnWindow: true,
	}); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base.Add(6 * time.Hour) }
	factory := &recordingFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Sessions: store, SessionFactory: factory})
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"), &uwspb.InitContextRequest{
		TaskId: "task-after-ttl", ContextName: proto.String("default"),
		AttachCdpUrl: proto.String("http://127.0.0.1:9222"),
	}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if got := factory.last(); got.ReuseTargetID != "" {
		t.Fatalf("过期绑定不得接管，实际 ReuseTargetID=%q", got.ReuseTargetID)
	}
}
