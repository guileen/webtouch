// 互斥键（P1 同站点跨 session 复用，2026-09-23 用户授权自决）：
// 同键任务共享同窗且后来者不重导航；不同键互不影响；拥有者关闭归还互斥槽并
// 解绑借用者，借用者关闭不得关掉共享窗口。
package engine

import (
	"testing"

	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/protobuf/proto"
)

const testAPIKey = "coordinator-key"

// ctxSlotKey 复现引擎对具名会话的槽位 key（"sess|" + API Key + \x00 + 名字）。
// 不能直接用 srv.ckeyFor(mdCtx(k), name)：测试侧的 mdCtx 是 *outgoing* 元数据，
// 而引擎读的是拦截器塞进 *incoming* 的 API Key，两者算出来的 key 不同。
func ctxSlotKey(name string) string { return "sess|" + testAPIKey + contextSep + name }

// bcForName 取指定命名会话绑定的上下文（不在则 nil）。
func bcForName(env *testEnv, name string) *browser.BrowserContext {
	env.srv.mu.Lock()
	defer env.srv.mu.Unlock()
	return env.srv.contexts[ctxSlotKey(name)]
}

// simNavFor 取指定会话的导航次数（「后来者不重导航」断言用）。
func simNavFor(t *testing.T, env *testEnv, name string) int {
	t.Helper()
	bc := bcForName(env, name)
	if bc == nil {
		t.Fatalf("上下文 %q 不在引擎里", name)
	}
	sim, ok := bc.Session().(*SimCDPSession)
	if !ok {
		t.Fatalf("会话类型 %T，期望 *SimCDPSession", bc.Session())
	}
	sim.mu.Lock()
	defer sim.mu.Unlock()
	return sim.navCnt
}

// initWith 发起一次 InitializeContext（可选互斥键 + 初始 URL）。
func initWith(t *testing.T, env *testEnv, name, mutexKey, url string) *uwspb.InitContextResponse {
	t.Helper()
	req := &uwspb.InitContextRequest{TaskId: "task-" + name, ContextName: proto.String(name)}
	if mutexKey != "" {
		req.MutexKey = proto.String(mutexKey)
	}
	if url != "" {
		req.InitialUrl = proto.String(url)
	}
	resp, err := env.client.InitializeContext(mdCtx(testAPIKey), req)
	if err != nil {
		t.Fatalf("InitializeContext(%s): %v", name, err)
	}
	return resp
}

func TestMutexKeySharesTargetAndSkipsRenavigation(t *testing.T) {
	env := newTestEnv(t)
	owner := initWith(t, env, "mutex-a", "login:example.com", "https://example.com/login")
	if got := simNavFor(t, env, "mutex-a"); got != 1 {
		t.Fatalf("拥有者导航次数=%d，want 1", got)
	}

	// 后来者：同互斥键 + 自己的 initial_url ⇒ 必须复用同一 Target 且**不导航**。
	borrower := initWith(t, env, "mutex-b", "login:example.com", "https://example.com/other")
	if borrower.GetTargetId() != owner.GetTargetId() {
		t.Fatalf("同互斥键应共享同窗：owner=%s borrower=%s", owner.GetTargetId(), borrower.GetTargetId())
	}
	if got := simNavFor(t, env, "mutex-a"); got != 1 {
		t.Fatalf("后来者不得重导航：导航次数=%d，want 1", got)
	}
	if bcForName(env, "mutex-b") != bcForName(env, "mutex-a") {
		t.Fatal("借用者应绑定到拥有者同一上下文")
	}
}

func TestMutexKeyDistinctKeysAreIsolated(t *testing.T) {
	env := newTestEnv(t)
	a := initWith(t, env, "mutex-a", "login:example.com", "https://example.com/login")
	b := initWith(t, env, "mutex-b", "login:other.com", "https://other.com/login")
	if a.GetTargetId() == b.GetTargetId() {
		t.Fatalf("不同互斥键不得共享窗口：都拿到 %s", a.GetTargetId())
	}
}

func TestMutexKeyBorrowerCloseKeepsSharedWindow(t *testing.T) {
	env := newTestEnv(t)
	owner := initWith(t, env, "mutex-a", "login:example.com", "https://example.com/login")
	initWith(t, env, "mutex-b", "login:example.com", "")

	if _, err := env.client.CloseContext(mdCtx(testAPIKey), &uwspb.CloseContextRequest{ContextName: "mutex-b"}); err != nil {
		t.Fatalf("借用者 CloseContext: %v", err)
	}
	if bcForName(env, "mutex-b") != nil {
		t.Fatal("借用者关闭后自己的绑定应被解绑")
	}
	// 共享窗口必须还在：第三个同键会话仍拿到同一 Target。
	c := initWith(t, env, "mutex-c", "login:example.com", "")
	if c.GetTargetId() != owner.GetTargetId() {
		t.Fatalf("借用者关闭不得关掉共享窗口：want %s got %s", owner.GetTargetId(), c.GetTargetId())
	}
}

func TestMutexKeyOwnerCloseReleasesSlot(t *testing.T) {
	env := newTestEnv(t)
	owner := initWith(t, env, "mutex-a", "login:example.com", "https://example.com/login")
	initWith(t, env, "mutex-b", "login:example.com", "")

	if _, err := env.client.CloseContext(mdCtx(testAPIKey), &uwspb.CloseContextRequest{ContextName: "mutex-a"}); err != nil {
		t.Fatalf("拥有者 CloseContext: %v", err)
	}
	if bcForName(env, "mutex-b") != nil {
		t.Fatal("拥有者关闭后借用者应被解绑（否则指向已释放的 Target）")
	}
	// 互斥槽归还空闲：后来者新建自己的窗口，而不是接管已关闭的那扇。
	d := initWith(t, env, "mutex-d", "login:example.com", "")
	if d.GetTargetId() == owner.GetTargetId() {
		t.Fatalf("拥有者关闭后互斥槽应归还空闲，仍复用了 %s", d.GetTargetId())
	}
}
