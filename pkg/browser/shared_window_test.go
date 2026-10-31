package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"uws/pkg/browser/internal/wstest"
	"uws/pkg/browser/window"
)

// ---- 假 CDP 服务端：演「锚页 + 中键开 Tab + 窗口归属」 ----

// fakeSharedBrowser 是一个能演共享窗口全流程的最小 CDP 服务端：target 列表、每个
// target 的窗口归属、中键后要冒出来的新 Tab、关掉了谁，都是可断言的脚本状态。
type fakeSharedBrowser struct {
	mu         sync.Mutex
	nextTarget int
	nextWindow int
	targets    []pageTarget
	windows    map[string]int
	// newTabs 是每次中键抬起时要冒出来的 Tab 的 URL（空 = 一个也不冒）。
	newTabs []string
	// newTabWindow 强制新 Tab 的窗口归属（0 = 落在锚页所在窗口，即正确行为）。
	newTabWindow int
	// windowErrFor 让指定 target 读窗口归属时报错。
	windowErrFor map[string]bool
	// evalValue 覆盖 Runtime.evaluate 的返回值（演锚页被导航走的拒绝路径）。
	evalValue any
	// mouseMovedTimeouts 让前 N 次 mouseMoved 回「context deadline exceeded」
	// （演真机上「窗口不在前台 → 输入被门控挂起」的形态）。
	mouseMovedTimeouts int
	// pressError 让 mousePressed 报错**但 Tab 照开**（演「点击已生效、回执没等到」）。
	pressError string

	createCalls []map[string]any
	closed      []string
	spawned     []string
	dispatches  []string
	// calls 按顺序记录「sessionId:method」，用来断言护栏下发到了**锚页 session**。
	calls []string
}

func (f *fakeSharedBrowser) serve(conn net.Conn, br *bufio.Reader) {
	for {
		op, payload, err := wstest.ReadServerFrame(br)
		if err != nil {
			return
		}
		if op != wstest.OpText {
			continue
		}
		var msg map[string]any
		if json.Unmarshal(payload, &msg) != nil {
			continue
		}
		method, _ := msg["method"].(string)
		sessionID, _ := msg["sessionId"].(string)
		params, _ := msg["params"].(map[string]any)
		id, _ := msg["id"].(float64)
		resp := f.handle(sessionID, method, params)
		resp["id"] = int64(id)
		b, _ := json.Marshal(resp)
		_ = wstest.WriteServerText(conn, b)
	}
}

func (f *fakeSharedBrowser) handle(sessionID, method string, params map[string]any) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sessionID+":"+method)
	ok := func(result map[string]any) map[string]any { return map[string]any{"result": result} }
	fail := func(message string) map[string]any {
		return map[string]any{"error": map[string]any{"code": -32000, "message": message}}
	}
	switch method {
	case "Target.getTargets":
		infos := make([]map[string]any, 0, len(f.targets))
		for _, t := range f.targets {
			infos = append(infos, map[string]any{"targetId": t.TargetID, "type": "page", "url": t.URL})
		}
		return ok(map[string]any{"targetInfos": infos})
	case "Target.createTarget":
		f.createCalls = append(f.createCalls, params)
		f.nextTarget++
		f.nextWindow++
		targetID := fmt.Sprintf("T%d", f.nextTarget)
		f.targets = append(f.targets, pageTarget{TargetID: targetID, Type: "page", URL: "about:blank"})
		f.windows[targetID] = 100 + f.nextWindow
		return ok(map[string]any{"targetId": targetID})
	case "Target.attachToTarget":
		targetID, _ := params["targetId"].(string)
		return ok(map[string]any{"sessionId": "sess-" + targetID})
	case "Browser.getWindowForTarget":
		targetID, _ := params["targetId"].(string)
		if f.windowErrFor[targetID] {
			return fail("cannot find target for window lookup")
		}
		windowID, exists := f.windows[targetID]
		if !exists {
			return fail("No target with given id")
		}
		return ok(map[string]any{"windowId": windowID})
	case "Target.closeTarget":
		targetID, _ := params["targetId"].(string)
		f.closed = append(f.closed, targetID)
		kept := f.targets[:0]
		for _, t := range f.targets {
			if t.TargetID != targetID {
				kept = append(kept, t)
			}
		}
		f.targets = kept
		delete(f.windows, targetID)
		return ok(map[string]any{})
	case "Runtime.evaluate":
		value := any(map[string]any{"ok": true, "x": 30, "y": 30})
		if f.evalValue != nil {
			value = f.evalValue
		}
		return ok(map[string]any{"result": map[string]any{"value": value}})
	case "Input.dispatchMouseEvent":
		typ, _ := params["type"].(string)
		f.dispatches = append(f.dispatches, typ)
		if typ == "mouseMoved" && f.mouseMovedTimeouts > 0 {
			f.mouseMovedTimeouts--
			return fail("context deadline exceeded")
		}
		if typ == "mousePressed" && f.pressError != "" {
			// 点击已经生效（Tab 该开还是开），但这次调用报了错——真机上就是
			// 「回执没等到、Tab 其实已经出来了」，调用方必须靠宽限判定救回来。
			f.spawnTabsLocked(sessionID)
			return fail(f.pressError)
		}
		if typ == "mouseReleased" {
			f.spawnTabsLocked(sessionID)
		}
		return ok(map[string]any{})
	default:
		return ok(map[string]any{})
	}
}

// spawnTabsLocked 演「中键在锚页里开出 Tab」：新 Tab 落在**锚页所在的那扇窗口**
// （真实 Chrome 语义）；newTabWindow 非 0 时用于演「落错窗口」的异常路径。
func (f *fakeSharedBrowser) spawnTabsLocked(sessionID string) {
	anchorWindow := f.windows[strings.TrimPrefix(sessionID, "sess-")]
	for _, url := range f.newTabs {
		f.nextTarget++
		targetID := fmt.Sprintf("T%d", f.nextTarget)
		windowID := anchorWindow
		if f.newTabWindow != 0 {
			windowID = f.newTabWindow
		}
		f.targets = append(f.targets, pageTarget{TargetID: targetID, Type: "page", URL: url})
		f.windows[targetID] = windowID
		f.spawned = append(f.spawned, targetID)
	}
}

// sessionMethodCalls 数出某条 session 上收到过几次某条命令。
func (f *fakeSharedBrowser) sessionMethodCalls(sessionID, method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == sessionID+":"+method {
			n++
		}
	}
	return n
}

func (f *fakeSharedBrowser) state() (closed, spawned []string, createCalls int, targets []pageTarget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closed...), append([]string(nil), f.spawned...),
		len(f.createCalls), append([]pageTarget(nil), f.targets...)
}

func (f *fakeSharedBrowser) start(t *testing.T) string {
	t.Helper()
	return wstest.Start(t, func(conn net.Conn, br *bufio.Reader) { f.serve(conn, br) })
}

// startMany 为 n 个会话各开一条连接（测试服务器是单连接模型），
// 但它们共享同一份假浏览器状态——这正是「同一个浏览器实例」的模型。
func (f *fakeSharedBrowser) startMany(t *testing.T, n int) []string {
	t.Helper()
	urls := make([]string, 0, n)
	for i := 0; i < n; i++ {
		urls = append(urls, f.start(t))
	}
	return urls
}

func (f *fakeSharedBrowser) setNewTabs(urls ...string) {
	f.mu.Lock()
	f.newTabs = urls
	f.mu.Unlock()
}

func sharedTestSession(t *testing.T, wsURL string, cfg Config) *cdpSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := newCDPClient(ctx, wsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &cdpSession{
		id:       "cdp-shared-test",
		cfg:      cfg,
		client:   c,
		closedCh: make(chan struct{}),
		inflight: make(map[string]time.Time),
	}
}

// newSharedRegistry 给每个用例一份独立注册表：3a 起共享窗口注册表是可注入的
// （window.Registry），取代原先「重置包级 map」的用例间全局耦合。
func newSharedRegistry(t *testing.T) *window.Registry {
	t.Helper()
	return window.NewRegistry()
}

func testSharedCfg() Config {
	return Config{AttachCDPURL: "http://127.0.0.1:9222", ProfileID: "p1"}
}

// ---- 端到端（假 CDP）：两个同指纹会话 → 一扇窗口两个 Tab ----

func TestOpenTabInSharedWindowReusesOneWindow(t *testing.T) {
	reg := newSharedRegistry(t)
	fake := &fakeSharedBrowser{windows: map[string]int{}, newTabs: []string{"about:blank"}}
	urls := fake.startMany(t, 2)
	cfg := testSharedCfg()
	s1 := sharedTestSession(t, urls[0], cfg)
	s2 := sharedTestSession(t, urls[1], cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t1, err := reg.OpenTab(ctx, bridgeWindowSession{s1}, 1440, 900)
	if err != nil {
		t.Fatalf("会话 1 共享开 Tab: %v", err)
	}
	fake.mu.Lock()
	sharedWindow := fake.windows[t1]
	fake.mu.Unlock()
	if sharedWindow == 0 {
		t.Fatal("第一个 Tab 应落在新建的锚页窗口里")
	}
	if !s1.sharedWindow || !s1.sharedAnchorOwner || s1.sharedWindowID != sharedWindow {
		t.Fatalf("会话 1 应被记为「建窗者」: shared=%v owner=%v window=%d（want %d）",
			s1.sharedWindow, s1.sharedAnchorOwner, s1.sharedWindowID, sharedWindow)
	}

	fake.setNewTabs("about:blank")
	t2, err := reg.OpenTab(ctx, bridgeWindowSession{s2}, 1440, 900)
	if err != nil {
		t.Fatalf("会话 2 共享开 Tab: %v", err)
	}
	if t1 == t2 {
		t.Fatalf("两个会话必须有各自的 Tab，实际都是 %s", t1)
	}
	if s2.sharedAnchorOwner {
		t.Fatal("会话 2 是复用者，不该被记成建窗者（否则它会去搬用户挪过的窗口）")
	}
	closed, _, createCalls, targets := fake.state()
	if createCalls != 1 {
		t.Fatalf("同指纹只该建一扇共享窗口，实际 createTarget 调用 %d 次", createCalls)
	}
	if len(closed) != 0 {
		t.Fatalf("正常路径不该关任何 target: %v", closed)
	}
	fake.mu.Lock()
	if got := fake.windows[t2]; got != sharedWindow {
		fake.mu.Unlock()
		t.Fatalf("会话 2 的 Tab 落在 window=%d，应在共享窗口 %d", got, sharedWindow)
	}
	fake.mu.Unlock()
	urlByTarget := map[string]string{}
	for _, info := range targets {
		urlByTarget[info.TargetID] = info.URL
	}
	if len(targets) != 3 {
		t.Fatalf("共享窗口应只有锚页 + 两个会话 Tab，实际 %d 个 target: %+v", len(targets), targets)
	}
	for _, id := range []string{t1, t2} {
		if urlByTarget[id] != "about:blank" {
			t.Fatalf("%s 应是白板 Tab（随后由会话自己导航）: %q", id, urlByTarget[id])
		}
	}
	fake.mu.Lock()
	seq := strings.Join(fake.dispatches, ",")
	fake.mu.Unlock()
	want := "mouseMoved,mousePressed,mouseReleased,mouseMoved,mousePressed,mouseReleased"
	if seq != want {
		t.Fatalf("每个会话应恰好派发一次中键三连: %s（want %s）", seq, want)
	}
}

func TestOpenTabInSharedWindowFailsClosedWhenTabLandsElsewhere(t *testing.T) {
	reg := newSharedRegistry(t)
	fake := &fakeSharedBrowser{windows: map[string]int{}, newTabs: []string{"about:blank"}, newTabWindow: 999}
	wsURL := fake.start(t)
	s := sharedTestSession(t, wsURL, testSharedCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := reg.OpenTab(ctx, bridgeWindowSession{s}, 1440, 900)
	if err == nil || !strings.Contains(err.Error(), "归属校验失败") {
		t.Fatalf("Tab 落到别的窗口必须 fail-closed，实际 err=%v", err)
	}
	closed, spawned, _, _ := fake.state()
	if len(spawned) != 1 {
		t.Fatalf("用例前提：应恰好开出一个新 Tab，实际 %v", spawned)
	}
	if len(closed) != 1 || closed[0] != spawned[0] {
		t.Fatalf("误落的自建 Tab 必须被关掉（且只关它一个）: closed=%v spawned=%v", closed, spawned)
	}
	key, _ := sharedWindowKey(testSharedCfg())
	if _, ok := reg.Entry(key); !ok {
		t.Fatal("一次归属校验失败不该注销共享窗口句柄（下次会话还可复用）")
	}
}

func TestOpenTabInSharedWindowRejectsAmbiguousOrNonBlank(t *testing.T) {
	cases := []struct {
		name string
		tabs []string
		want string
	}{
		{"一次开出两个 Tab", []string{"about:blank", "about:blank"}, "语义不明"},
		{"新 Tab 不是白板", []string{"https://example.com/"}, "不是白板"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg := newSharedRegistry(t)
			fake := &fakeSharedBrowser{windows: map[string]int{}, newTabs: c.tabs}
			wsURL := fake.start(t)
			s := sharedTestSession(t, wsURL, testSharedCfg())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			_, err := reg.OpenTab(ctx, bridgeWindowSession{s}, 1440, 900)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应拒绝认领（%s），实际 err=%v", c.want, err)
			}
			closed, _, _, _ := fake.state()
			if len(closed) != 0 {
				t.Fatalf("分不清哪个是自己的时候一个都不许关（可能关掉用户页面）: %v", closed)
			}
		})
	}
}

func TestOpenTabInSharedWindowRebuildsWhenAnchorGone(t *testing.T) {
	reg := newSharedRegistry(t)
	key, _ := sharedWindowKey(testSharedCfg())
	reg.Seed(key, window.Handle{WindowID: 101, AnchorTargetID: "T-gone"})
	fake := &fakeSharedBrowser{windows: map[string]int{}, newTabs: []string{"about:blank"}}
	wsURL := fake.start(t)
	s := sharedTestSession(t, wsURL, testSharedCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	targetID, err := reg.OpenTab(ctx, bridgeWindowSession{s}, 1440, 900)
	if err != nil {
		t.Fatalf("锚页没了应重建窗口而不是失败: %v", err)
	}
	if !s.sharedAnchorOwner {
		t.Fatal("重建锚页窗的会话应被记为建窗者")
	}
	handle, ok := reg.Entry(key)
	if !ok || handle.AnchorTargetID == "T-gone" {
		t.Fatalf("注册表应指向新锚页: %+v", handle)
	}
	fake.mu.Lock()
	if fake.windows[targetID] != handle.WindowID {
		fake.mu.Unlock()
		t.Fatalf("新 Tab 必须落在新锚页窗 %d 里", handle.WindowID)
	}
	fake.mu.Unlock()
}

func TestOpenTabInSharedWindowRefusesToInjectIntoUserPage(t *testing.T) {
	reg := newSharedRegistry(t)
	fake := &fakeSharedBrowser{
		windows:   map[string]int{},
		newTabs:   []string{"about:blank"},
		evalValue: map[string]any{"ok": false, "err": "anchor-not-blank:https://ops-test.example/dashboard"},
	}
	wsURL := fake.start(t)
	s := sharedTestSession(t, wsURL, testSharedCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := reg.OpenTab(ctx, bridgeWindowSession{s}, 1440, 900)
	if err == nil || !strings.Contains(err.Error(), "锚页拒绝部署链接") {
		t.Fatalf("锚页被导航成用户页面时必须拒绝注入，实际 err=%v", err)
	}
	if _, _, _, targets := fake.state(); len(targets) != 1 {
		t.Fatalf("拒绝路径不该再开 Tab: %+v", targets)
	}
}

func TestOpenTabInSharedWindowClosesAnchorWhenWindowUnknown(t *testing.T) {
	reg := newSharedRegistry(t)
	fake := &fakeSharedBrowser{windows: map[string]int{}, windowErrFor: map[string]bool{"T1": true}}
	wsURL := fake.start(t)
	s := sharedTestSession(t, wsURL, testSharedCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := reg.OpenTab(ctx, bridgeWindowSession{s}, 1440, 900); err == nil {
		t.Fatal("读不到窗口归属时必须报错（这扇窗没法校验后续 Tab）")
	}
	closed, _, _, _ := fake.state()
	if len(closed) != 1 || closed[0] != "T1" {
		t.Fatalf("读不到归属的锚页窗应被关掉: %v", closed)
	}
}

func TestReleaseSharedWindowClosesAnchorOnlyWhenAlone(t *testing.T) {
	key, _ := sharedWindowKey(testSharedCfg())
	build := func(extra []pageTarget) (*fakeSharedBrowser, *cdpSession, *window.Registry) {
		reg := newSharedRegistry(t)
		fake := &fakeSharedBrowser{
			windows: map[string]int{"T1": 101},
			targets: append([]pageTarget{{TargetID: "T1", Type: "page", URL: "about:blank"}}, extra...),
		}
		for _, info := range extra {
			fake.windows[info.TargetID] = 101
		}
		wsURL := fake.start(t)
		s := sharedTestSession(t, wsURL, testSharedCfg())
		s.sharedWindow = true
		s.sharedWindowID = 101
		reg.Seed(key, window.Handle{WindowID: 101, AnchorTargetID: "T1"})
		return fake, s, reg
	}

	// 窗内只剩锚页：关锚页（窗口随之消失）并回收句柄。
	fake, s, reg := build(nil)
	reg.Release(bridgeWindowSession{s})
	closed, _, _, _ := fake.state()
	if len(closed) != 1 || closed[0] != "T1" {
		t.Fatalf("只剩锚页时应关掉它: %v", closed)
	}
	if _, ok := reg.Entry(key); ok {
		t.Fatal("窗口回收后句柄必须删掉")
	}

	// 窗内还有别的页面（别的会话或用户自己开的）：一律不动。
	other := pageTarget{TargetID: "T9", Type: "page", URL: "https://ops-test.example/dashboard"}
	fake2, s2, reg2 := build([]pageTarget{other})
	reg2.Release(bridgeWindowSession{s2})
	closed2, _, _, _ := fake2.state()
	if len(closed2) != 0 {
		t.Fatalf("窗内还有别的页面时一个都不许关: %v", closed2)
	}
	if _, ok := reg2.Entry(key); !ok {
		t.Fatal("句柄应保留，供后续会话继续复用")
	}
}

// ---- 锚页「输入必达」护栏（2026-09-24 真机缺陷修复） ----

// 锚页是临时 attach 的第二个 session，必须自己再装一层护栏；否则共享窗口不在前台时
// 中键会被门控挂起到超时（真机实测：会话 2 复用窗口 → 派发超时 → 退化专属小窗）。
func TestOpenTabInSharedWindowArmsAnchorSession(t *testing.T) {
	reg := newSharedRegistry(t)
	fake := &fakeSharedBrowser{windows: map[string]int{}, newTabs: []string{"about:blank"}}
	wsURL := fake.start(t)
	s := sharedTestSession(t, wsURL, testSharedCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	targetID, err := reg.OpenTab(ctx, bridgeWindowSession{s}, 1440, 900)
	if err != nil {
		t.Fatalf("共享开 Tab: %v", err)
	}
	key, _ := sharedWindowKey(testSharedCfg())
	handle, ok := reg.Entry(key)
	if !ok {
		t.Fatal("共享窗口应已注册")
	}
	if targetID == handle.AnchorTargetID {
		t.Fatal("用例前提：锚页与会话自己的 Tab 是两个 target")
	}
	anchorSession := "sess-" + handle.AnchorTargetID
	for _, m := range []string{"Page.enable", "Runtime.enable", "Emulation.setFocusEmulationEnabled", "Page.setWebLifecycleState"} {
		if got := fake.sessionMethodCalls(anchorSession, m); got == 0 {
			t.Fatalf("锚页 session %s 应收到 %s（实际没收到，护栏没装上）", anchorSession, m)
		}
	}
	if got := fake.sessionMethodCalls("sess-"+targetID, "Emulation.setFocusEmulationEnabled"); got != 0 {
		t.Fatalf("护栏只该装在锚页 session 上，不该误发到会话自己的 Tab（实际 %d 次）", got)
	}
}

// mouseMoved 超时（输入被门控）时：重放护栏 + 退避重试，且**不**多派发点击事件。
func TestDispatchAnchorMiddleClickRetriesOnlyMouseMoved(t *testing.T) {
	reg := newSharedRegistry(t)
	fake := &fakeSharedBrowser{
		windows:            map[string]int{},
		newTabs:            []string{"about:blank"},
		mouseMovedTimeouts: 1,
	}
	wsURL := fake.start(t)
	s := sharedTestSession(t, wsURL, testSharedCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := reg.OpenTab(ctx, bridgeWindowSession{s}, 1440, 900); err != nil {
		t.Fatalf("首次 mouseMoved 超时后应重试成功，实际 %v", err)
	}
	fake.mu.Lock()
	seq := strings.Join(fake.dispatches, ",")
	fake.mu.Unlock()
	want := "mouseMoved,mouseMoved,mousePressed,mouseReleased"
	if seq != want {
		t.Fatalf("只有 mouseMoved 可重试、点击事件绝不重试：%s（want %s）", seq, want)
	}
	key, _ := sharedWindowKey(testSharedCfg())
	handle, _ := reg.Entry(key)
	anchorSession := "sess-" + handle.AnchorTargetID
	if got := fake.sessionMethodCalls(anchorSession, "Emulation.setFocusEmulationEnabled"); got < 2 {
		t.Fatalf("重试前必须重放锚页护栏（应 ≥2 次下发，实际 %d 次）", got)
	}
	if got := fake.sessionMethodCalls(anchorSession, "Page.setWebLifecycleState"); got < 2 {
		t.Fatalf("重试前必须重放页面生命周期护栏（应 ≥2 次下发，实际 %d 次）", got)
	}
	closed, _, createCalls, _ := fake.state()
	if createCalls != 1 || len(closed) != 0 {
		t.Fatalf("重试语义下不该多建窗、也不该关任何 target: createCalls=%d closed=%v", createCalls, closed)
	}
}

// 派发报错但新 Tab 已经出现：属「点击已生效、回执没等到」，必须按成功继续走归属校验，
// 不能把已经开出来的 Tab 判成失败丢成孤儿。
func TestOpenTabInSharedWindowClaimsTabDespiteDispatchError(t *testing.T) {
	reg := newSharedRegistry(t)
	fake := &fakeSharedBrowser{
		windows:    map[string]int{},
		newTabs:    []string{"about:blank"},
		pressError: "context deadline exceeded",
	}
	wsURL := fake.start(t)
	s := sharedTestSession(t, wsURL, testSharedCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	targetID, err := reg.OpenTab(ctx, bridgeWindowSession{s}, 1440, 900)
	if err != nil {
		t.Fatalf("派发报错但 Tab 已出现时应按成功处理，实际 %v", err)
	}
	_, spawned, _, _ := fake.state()
	if len(spawned) != 1 || spawned[0] != targetID {
		t.Fatalf("应认领那个已经开出来的 Tab: target=%s spawned=%v", targetID, spawned)
	}
	handle, ok := reg.Entry(mustSharedKey(t))
	if !ok {
		t.Fatal("按成功处理后共享窗口必须已注册")
	}
	fake.mu.Lock()
	got := fake.windows[targetID]
	fake.mu.Unlock()
	if got != handle.WindowID {
		t.Fatalf("认领的 Tab 应在共享窗口 %d 里，实际 window=%d", handle.WindowID, got)
	}
}

func mustSharedKey(t *testing.T) string {
	t.Helper()
	key, ok := sharedWindowKey(testSharedCfg())
	if !ok {
		t.Fatal("测试配置应有共享键")
	}
	return key
}
