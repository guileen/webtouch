// 测试骨架：TDD-ENG-01..13（tdd-checklist.md §3），Go 逻辑层用例。
// 与 tests/integration/test_engine.py（gRPC 契约层）双层验证；骨架阶段全部 t.Skip。
package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ── mock（内存 CDP 会话，可注入慢速导航）──────────────────────────────

type mockSession struct {
	mu      sync.Mutex
	id      string
	cfg     Config
	cookies []Cookie
	slow    chan struct{}
}

func (m *mockSession) ID() string { return m.id }

func (m *mockSession) Probe() ProbeResult {
	ua := m.cfg.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	tz := m.cfg.Timezone
	if tz == "" {
		tz = defaultTimezone
	}
	return ProbeResult{UserAgent: ua, Timezone: tz}
}

func (m *mockSession) Navigate(ctx context.Context, _ string) error {
	if m.slow != nil {
		select {
		case <-m.slow:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *mockSession) SetCookie(c Cookie) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cookies = append(m.cookies, c)
}

func (m *mockSession) Cookies() []Cookie {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Cookie(nil), m.cookies...)
}

func (m *mockSession) ExportState(_ context.Context, _ string) (*SerializedSessionState, error) {
	return &SerializedSessionState{}, nil
}

func (m *mockSession) Close() error { return nil }

type mockFactory struct {
	mu     sync.Mutex
	nextID int
	fail   error
}

func (f *mockFactory) Create(_ context.Context, cfg Config) (CDPSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	f.nextID++
	s := &mockSession{id: fmt.Sprintf("sess-%d", f.nextID), cfg: cfg}
	for _, c := range cfg.Cookies {
		s.SetCookie(c)
	}
	return s, nil
}

// ── helpers ───────────────────────────────────────────────────────────

func acquire(t *testing.T, p *Pool, cfg Config) *BrowserContext {
	t.Helper()
	bc, err := p.Acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Acquire(%q) 失败: %v", cfg.TaskID, err)
	}
	return bc
}

func waitQueue(t *testing.T, p *Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		q := len(p.queue)
		p.mu.Unlock()
		if q >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %d 个排队请求超时（当前 %d）", n, q)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitInflight(bc *BrowserContext, n int) {
	deadline := time.Now().Add(time.Second)
	for {
		bc.mu.Lock()
		v := bc.inFlight
		bc.mu.Unlock()
		if v >= n {
			return
		}
		if time.Now().After(deadline) {
			panic("in-flight 计数未达到")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ── TDD-ENG-01：代理/时区注入后 UA 与时区探活一致 ──────────────────────

func TestEng01InitContextProxyTimezone(t *testing.T) {
	f := &mockFactory{}
	p := NewPool(2, f)
	bc := acquire(t, p, Config{
		TaskID:    "t1",
		Proxy:     &Proxy{Host: "proxy.example.com", Port: 8080},
		Timezone:  "Asia/Shanghai",
		UserAgent: "UWS-Test-UA/1.0",
	})
	defer p.Release(bc.ID)

	got := bc.Probe()
	if got.UserAgent != "UWS-Test-UA/1.0" {
		t.Errorf("UA 探活不一致: got %q, want %q", got.UserAgent, "UWS-Test-UA/1.0")
	}
	if got.Timezone != "Asia/Shanghai" {
		t.Errorf("时区探活不一致: got %q, want %q", got.Timezone, "Asia/Shanghai")
	}
	sess := bc.session.(*mockSession)
	if sess.cfg.Proxy == nil || sess.cfg.Proxy.Host != "proxy.example.com" || sess.cfg.Proxy.Port != 8080 {
		t.Errorf("代理未注入会话: %+v", sess.cfg.Proxy)
	}
}

// ── TDD-ENG-02：5 个并发 Context 的 Cookie/缓存物理隔离 ─────────────────

func TestEng02ConcurrentContextIsolation(t *testing.T) {
	f := &mockFactory{}
	p := NewPool(5, f)
	var bcs []*BrowserContext
	for i := 0; i < 5; i++ {
		bc := acquire(t, p, Config{TaskID: fmt.Sprintf("user-%d", i), Timezone: "Asia/Shanghai"})
		bc.SetCookie(Cookie{Name: "session", Value: fmt.Sprintf("login-%d", i), Domain: ".example.com"})
		bcs = append(bcs, bc)
	}
	defer func() {
		for _, bc := range bcs {
			p.Release(bc.ID)
		}
	}()

	for i, bc := range bcs {
		cookies := bc.Cookies()
		want := fmt.Sprintf("login-%d", i)
		if len(cookies) != 1 || cookies[0].Value != want {
			t.Errorf("context %d Cookie 泄漏/丢失: %v (want %s)", i, cookies, want)
		}
	}
}

// ── TDD-ENG-09：预置 Cookie 注入 + 并发隔离组合验证 ─────────────────────

func TestEng09CookiesInjection(t *testing.T) {
	f := &mockFactory{}
	p := NewPool(2, f)
	bcA := acquire(t, p, Config{
		TaskID: "a",
		Cookies: []Cookie{
			{Name: "sid", Value: "abc", Domain: ".a.com"},
			{Name: "theme", Value: "dark"},
		},
	})
	bcB := acquire(t, p, Config{
		TaskID: "b",
		Cookies: []Cookie{
			{Name: "sid", Value: "xyz", Domain: ".b.com"},
		},
	})
	defer func() {
		p.Release(bcA.ID)
		p.Release(bcB.ID)
	}()

	a := bcA.Cookies()
	if len(a) != 2 || a[0].Value != "abc" || a[1].Value != "dark" {
		t.Errorf("A 注入 Cookie 不完整: %v", a)
	}
	b := bcB.Cookies()
	if len(b) != 1 || b[0].Value != "xyz" {
		t.Errorf("B 注入 Cookie 不完整或泄漏 A 的 Cookie: %v", b)
	}
}

// ── TDD-ENG-10：Target 池配额 + FIFO 队列 + RESOURCE_EXHAUSTED ─────────

func TestEng10TargetPoolQuota(t *testing.T) {
	f := &mockFactory{}
	p := NewPool(2, f)
	p.acquireTimeout = 80 * time.Millisecond

	bc1 := acquire(t, p, Config{TaskID: "1"})
	bc2 := acquire(t, p, Config{TaskID: "2"})

	start := time.Now()
	if _, err := p.Acquire(context.Background(), Config{TaskID: "3"}); !errors.Is(err, ErrResourceExhausted) {
		t.Fatalf("容量满时未返回 RESOURCE_EXHAUSTED: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("未等待超时即拒绝（elapsed=%v）", elapsed)
	}

	// FIFO：A 先入队、B 后入队，释放 bc1 后队首（A）先获得槽位
	p.acquireTimeout = 5 * time.Second
	aDone := make(chan string, 1)
	bDone := make(chan string, 1)
	var aID, bID string
	go func() {
		if bca, err := p.Acquire(context.Background(), Config{TaskID: "A"}); err == nil {
			aDone <- bca.ID
		}
	}()
	waitQueue(t, p, 1) // A 已入队
	go func() {
		if bcb, err := p.Acquire(context.Background(), Config{TaskID: "B"}); err == nil {
			bDone <- bcb.ID
		}
	}()
	waitQueue(t, p, 2) // B 已入队

	p.Release(bc1.ID)
	select {
	case aID = <-aDone:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("FIFO：队首 A 未在 bc1 释放后获得槽位")
	}
	select {
	case <-bDone:
		t.Fatal("FIFO：B 不应在 A 之前获得槽位")
	default:
	}

	p.Release(bc2.ID)
	select {
	case bID = <-bDone:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("FIFO：队次 B 未在 bc2 释放后获得槽位")
	}

	if _, max := p.Stats(); max != 2 {
		t.Errorf("池上限应为 2, got %d", max)
	}
	// 全部销毁后池大小回落为 0
	p.Release(aID)
	p.Release(bID)
	if active, _ := p.Stats(); active != 0 {
		t.Errorf("池大小未回落: active=%d", active)
	}
}

// ── TDD-ENG-11：500 次 Navigation / 4h 优雅重启，已确认结果不丢失 ──────

func TestEng11TargetLifecycleRestart(t *testing.T) {
	f := &mockFactory{}
	p := NewPool(1, f)
	bc := acquire(t, p, Config{TaskID: "t"})
	defer p.Release(bc.ID)

	// 4h 老化触发优雅重启
	sess1 := bc.SessionID()
	bc.mu.Lock()
	bc.startedAt = bc.now().Add(-5 * time.Hour)
	bc.mu.Unlock()
	if err := bc.Navigate(context.Background(), "https://example.com/1"); err != nil {
		t.Fatalf("4h 老化后导航失败: %v", err)
	}
	sess2 := bc.SessionID()
	if sess1 == sess2 {
		t.Error("4h 老化未触发优雅重启")
	}
	if bc.NavCount() != 1 {
		t.Errorf("重启后导航计数未重置: %d", bc.NavCount())
	}

	// 累计 500 次导航触发重启，已确认动作结果不丢失
	for i := 0; i < maxNavigations-1; i++ {
		if err := bc.Navigate(context.Background(), fmt.Sprintf("https://example.com/%d", i)); err != nil {
			t.Fatalf("第 %d 次导航失败: %v", i, err)
		}
	}
	sess3 := bc.SessionID() // 4h 重启后会话（此时 navCount=500）
	if err := bc.Navigate(context.Background(), "https://example.com/final"); err != nil {
		t.Fatalf("500 次后导航失败: %v", err)
	}
	sess4 := bc.SessionID()
	if sess3 == sess4 {
		t.Error("500 次 Navigation 未触发优雅重启")
	}
	if bc.NavCount() != 1 {
		t.Errorf("重启后导航计数未重置: %d", bc.NavCount())
	}
	if want := 1 + (maxNavigations - 1) + 1; bc.ConfirmedCount() != want {
		t.Errorf("已确认动作结果丢失: got %d, want %d", bc.ConfirmedCount(), want)
	}

	// 优雅重启排空超时：in-flight 阻塞时返回 errDrainTimeout
	m := bc.session.(*mockSession)
	m.slow = make(chan struct{})
	bc.drainTimeout = 50 * time.Millisecond
	slowDone := make(chan error, 1)
	go func() {
		slowDone <- bc.Navigate(context.Background(), "https://example.com/slow")
	}()
	waitInflight(bc, 1)
	bc.mu.Lock()
	bc.navCount = maxNavigations
	bc.mu.Unlock()
	if err := bc.Navigate(context.Background(), "https://example.com/restart"); !errors.Is(err, errDrainTimeout) {
		t.Errorf("期望排空超时 errDrainTimeout, got %v", err)
	}
	close(m.slow)
	if err := <-slowDone; err != nil {
		t.Fatalf("慢速导航异常: %v", err)
	}
}

// 以下用例仍为骨架（未点亮，避免与并行 matcher/proto 任务文件重叠）：
// TDD-ENG-03/04/05/13（物理执行、拦截检测、新窗口）留待 M4 阶段。

func TestEng03FittsCurveTrajectory(t *testing.T) {
	t.Skip("skeleton: TDD-ENG-03 待实现（CDP 轨迹非直线、服从 Fitts 三次贝塞尔）")
}

func TestEng04ZIndexOcclusionAbort(t *testing.T) {
	t.Skip("skeleton: TDD-ENG-04 待实现（透明弹窗遮挡 → 平移或 ABORTED，禁止穿透点击）")
}

func TestEng05ChallengeInterceptedNoDeadlock(t *testing.T) {
	t.Skip("skeleton: TDD-ENG-05 待实现（403+Turnstile 特征 → CHALLENGE_INTERCEPTED）")
}

func TestEng13NewWindowOpened(t *testing.T) {
	t.Skip("skeleton: TDD-ENG-13 待实现（target=_blank → NEW_WINDOW_OPENED，原 Target 保留）")
}
