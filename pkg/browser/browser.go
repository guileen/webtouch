package browser

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"uws/pkg/fingerprint"
)

// ErrResourceExhausted 表示 Target 池容量已满且等待超时
// （对应 gRPC RESOURCE_EXHAUSTED：降级阻塞，拒绝低优先级动作）。
var ErrResourceExhausted = errors.New("target pool exhausted")

// ErrStateBundleTooLate indicates an import attempted after the first page
// navigation. Storage restoration must take place in a clean tab so the
// caller cannot mistake a next-navigation effect for current-page recovery.
var ErrStateBundleTooLate = errors.New("state bundle must be imported before first navigation")

// 生命周期与超时阈值（spec/algorithm-kernel.md §7.3 Runtime Plane）。
const (
	defaultAcquireTimeout = 30 * time.Second
	maxNavigations        = 500
	maxContextAge         = 4 * time.Hour
	defaultDrainTimeout   = 30 * time.Second
	defaultUserAgent      = "UWS/1.4 Engine"
	defaultTimezone       = "UTC"
)

var (
	errContextClosed = errors.New("browser context closed")
	errDrainTimeout  = errors.New("drain timeout")
)

// Proxy 对应 proto InitContextRequest 的 ProxyConfig。
type Proxy struct {
	Host     string
	Port     int
	Username string
	Password string
}

// Viewport 对应 proto InitContextRequest 的 ViewportConfig。
type Viewport struct {
	Width             int
	Height            int
	DeviceScaleFactor float64
}

// Cookie 对应 proto InitContextRequest 的 Cookie。
type Cookie struct {
	Name     string
	Value    string
	Domain   string
	Path     string
	HTTPOnly bool
	Secure   bool
	Expires  int64
}

// Config 是一次 InitializeContext 的完整上下文配置（四元组沙盒隔离）。
type Config struct {
	TaskID   string
	Proxy    *Proxy
	Viewport *Viewport
	// ViewportExplicit 记录调用方是否显式指定了 viewport（区别于 engine 层注入的
	// 1920x1080 默认值）。附着模式未显式指定时应跟随窗口实际尺寸，而不是强设默认
	// 视口（页面宽度=窗口宽度，避免“比窗口宽又无横向滚动条”，GLOBAL #72/#84）。
	ViewportExplicit bool
	Cookies          []Cookie
	Timezone         string
	UserAgent        string
	AttachCDPURL     string
	InitialURL       string
	// ProfileID 指名一个持久浏览器身份（空 = 每次用临时目录，用完即焚）。
	// 指定后本会话落盘到 ~/.uws/profiles/<id>/，同一 id 每次进来都是同一台设备
	// （Cookie/LocalStorage/扩展都留着）——指纹体系的身份层，见 profile_dir.go。
	ProfileID string
	// Fingerprint 是浏览器身份档案（空 = 不伪装，用浏览器真实身份）。它是 UA/UA-CH/
	// 时区/语言/屏幕这些字段的**事实源**：档案在，就按档案来（见 pkg/fingerprint；
	// 启动参数在 Create 里落地，CDP 覆盖在 initTarget 里落地）。
	Fingerprint *fingerprint.Profile
	// WindowMode 覆盖窗口策略（"" = 默认：可见浏览器开专属小窗、不抢焦点；
	// "tab" = 旧行为，在本会话所在窗口新建标签）。见 window.go。
	WindowMode string
	// ReuseTargetID 指定「接管这扇已经存在的标签页」而不是新建（会话续页，
	// GLOBAL #118）。引擎把「会话名 → Target」绑定落盘，重启失忆后凭它接着在
	// 原来那扇窗口干，不再每次开一扇新窗。
	ReuseTargetID string
	// RequireReuseTarget 表示该 Target ID 来自持久 session 绑定，接管失败必须报错，
	// 不得静默退回其他 Target；避免续页请求落到浏览器内的另一个会话。
	// 例外（2026-10-03）：调用方显式给了 InitialURL 时允许回退新开窗口——绑定页签
	// 已被关掉（如空闲回收）时，从 initial_url 重新开始比整条链路失败更合理。
	RequireReuseTarget bool
	// ReuseOwnWindow 说明被接管的那扇窗口是不是「专属小窗」（续页时按原策略
	// 记回 ownWindow，窗口不会被当成普通标签处理）。
	ReuseOwnWindow bool
}

// ProbeResult 是页面对物理环境的探活结果。
type ProbeResult struct {
	UserAgent string
	Timezone  string
}

// CDPSession 抽象一条 CDP 会话，并包含导出 Cookies 和 LocalStorage 状态的能力。
type CDPSession interface {
	ID() string
	Probe() ProbeResult
	Navigate(ctx context.Context, url string) error
	SetCookie(c Cookie)
	Cookies() []Cookie
	ExportState(ctx context.Context, targetOrigin string) (*SerializedSessionState, error)
	Close() error
}

// StateBundleSession is optional because simulated/third-party SessionFactory
// implementations need not expose browser secrets. Production CDP sessions
// implement it; the engine fails closed when a caller requests import on an
// unsupported session rather than pretending the state was restored.
type StateBundleSession interface {
	ExportStateBundle(ctx context.Context, profileID, targetOrigin string) (*fingerprint.StateBundle, error)
	ImportStateBundle(ctx context.Context, bundle *fingerprint.StateBundle) error
}

// SessionFactory 为每个 BrowserContext 创建独立 CDP 会话（物理隔离 Cookie/缓存）。
type SessionFactory interface {
	Create(ctx context.Context, cfg Config) (CDPSession, error)
}

type acquireReq struct {
	ctx  context.Context
	slot chan struct{}
}

// Pool 管理 Target 池：惰性创建、FIFO 排队、等待超时后返回 RESOURCE_EXHAUSTED。
type Pool struct {
	maxSlots       int
	available      int
	acquireTimeout time.Duration

	mu       sync.Mutex
	queue    []*acquireReq
	contexts map[string]*BrowserContext

	factory SessionFactory
	now     func() time.Time
	nextID  atomic.Int64
}

// NewPool 创建 Target 池；maxSlots <= 0 时按 CPU_Cores × 1.5 计算。
func NewPool(maxSlots int, factory SessionFactory) *Pool {
	if factory == nil {
		panic("browser: nil SessionFactory")
	}
	if maxSlots <= 0 {
		maxSlots = runtime.NumCPU() * 3 / 2
		if maxSlots < 1 {
			maxSlots = 1
		}
	}
	return &Pool{
		maxSlots:       maxSlots,
		available:      maxSlots,
		acquireTimeout: defaultAcquireTimeout,
		contexts:       make(map[string]*BrowserContext),
		factory:        factory,
		now:            time.Now,
	}
}

// Acquire 从池中申请一个独占 BrowserContext；容量满时 FIFO 排队，
// 超过 acquireTimeout 返回 ErrResourceExhausted。
func (p *Pool) Acquire(parent context.Context, cfg Config) (*BrowserContext, error) {
	ctx, cancel := context.WithTimeout(parent, p.acquireTimeout)
	defer cancel()

	req := &acquireReq{ctx: ctx, slot: make(chan struct{}, 1)}
	p.mu.Lock()
	if p.available > 0 {
		p.available--
		p.mu.Unlock()
		return p.createContext(ctx, cfg)
	}
	p.queue = append(p.queue, req)
	p.mu.Unlock()

	select {
	case <-req.slot:
		return p.createContext(ctx, cfg)
	case <-ctx.Done():
		p.removeReq(req)
		select {
		case <-req.slot:
			p.releaseSlot()
		default:
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrResourceExhausted
		}
		return nil, ctx.Err()
	}
}

// Release 销毁并释放指定上下文（任务结束 → Target 随 BrowserContext 销毁，池大小回落）。
func (p *Pool) Release(id string) {
	p.mu.Lock()
	bc := p.contexts[id]
	if bc != nil {
		delete(p.contexts, id)
	}
	p.mu.Unlock()
	if bc == nil {
		return
	}
	_ = bc.Close()
	p.releaseSlot()
}

// Stats 返回（活跃 Target 数, 池上限）。
func (p *Pool) Stats() (active, max int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.contexts), p.maxSlots
}

func (p *Pool) createContext(ctx context.Context, cfg Config) (*BrowserContext, error) {
	sess, err := p.factory.Create(ctx, cfg)
	if err != nil {
		p.releaseSlot()
		return nil, err
	}
	id := fmt.Sprintf("ctx-%d", p.nextID.Add(1))
	bc := &BrowserContext{
		ID:           id,
		cfg:          cfg,
		session:      sess,
		factory:      p.factory,
		startedAt:    p.now(),
		now:          p.now,
		drainTimeout: defaultDrainTimeout,
		restartDone:  make(chan struct{}),
	}
	p.mu.Lock()
	p.contexts[id] = bc
	p.mu.Unlock()
	return bc, nil
}

func (p *Pool) removeReq(req *acquireReq) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, r := range p.queue {
		if r == req {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			return
		}
	}
}

func (p *Pool) releaseSlot() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) > 0 {
		req := p.queue[0]
		p.queue = p.queue[1:]
		req.slot <- struct{}{}
		return
	}
	p.available++
}

// BrowserContext 是一个任务独占的浏览器沙盒（独立会话 → Cookie/缓存物理隔离）。
type BrowserContext struct {
	ID  string
	cfg Config

	sessionMu sync.RWMutex
	session   CDPSession
	factory   SessionFactory

	mu          sync.Mutex
	wg          sync.WaitGroup
	navCount    int
	inFlight    int
	confirmed   []string
	closed      bool
	restarting  bool
	restartDone chan struct{}
	startedAt   time.Time

	now          func() time.Time
	drainTimeout time.Duration

	// pinTargetID 是客户端钉扎的 Target（InitializeContext 返回值）。新窗口接管
	// （TargetSwitcher）后会话作用面会前移，但客户端手里的钉扎值仍是它——两者都
	// 必须能被路由回本上下文，否则接管后第一次带 target_id 的动作就被误判为串上下文。
	pinTargetID string
}

// PinTarget 记录客户端钉扎的 Target ID（由引擎在 InitializeContext 时写入）。
func (c *BrowserContext) PinTarget(targetID string) {
	c.mu.Lock()
	c.pinTargetID = targetID
	c.mu.Unlock()
}

// PinnedTarget 返回客户端钉扎的 Target ID（未设置时为空）。
func (c *BrowserContext) PinnedTarget() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinTargetID
}

// Probe 返回页面对 UA/时区的探活结果（TDD-ENG-01）。
func (c *BrowserContext) Probe() ProbeResult {
	c.sessionMu.RLock()
	s := c.session
	c.sessionMu.RUnlock()
	return s.Probe()
}

// Cookies 返回当前会话可见的 Cookie 列表（TDD-ENG-02/09）。
func (c *BrowserContext) Cookies() []Cookie {
	c.sessionMu.RLock()
	s := c.session
	c.sessionMu.RUnlock()
	return s.Cookies()
}

// SetCookie 模拟页面/预置向会话写入 Cookie。
func (c *BrowserContext) SetCookie(cookie Cookie) {
	c.sessionMu.RLock()
	s := c.session
	c.sessionMu.RUnlock()
	s.SetCookie(cookie)
}

// SessionID 返回当前 CDP 会话 ID（用于验证优雅重启是否发生）。
func (c *BrowserContext) SessionID() string {
	c.sessionMu.RLock()
	s := c.session
	c.sessionMu.RUnlock()
	return s.ID()
}

// Session 返回当前 CDP 会话（引擎物理执行层通过类型断言调用扩展能力）。
func (c *BrowserContext) Session() CDPSession {
	c.sessionMu.RLock()
	defer c.sessionMu.RUnlock()
	return c.session
}

// ProfileID returns the identity selected when this context was created.
// It is intentionally read-only: changing a context's identity after Chrome
// has started would make the on-disk profile, injected fingerprint and
// authenticated state disagree.
func (c *BrowserContext) ProfileID() string { return c.cfg.ProfileID }

// NavCount 返回当前会话的导航次数。
func (c *BrowserContext) NavCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.navCount
}

// ConfirmedCount 返回已确认动作结果数（优雅重启不得丢失）。
func (c *BrowserContext) ConfirmedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.confirmed)
}

// Close 关闭上下文（任务结束销毁）。
func (c *BrowserContext) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	c.sessionMu.RLock()
	s := c.session
	c.sessionMu.RUnlock()
	return s.Close()
}

// Navigate 执行一次导航；达到 500 次或 4 小时先优雅重启（排空 30s，已确认结果不丢失）。
func (c *BrowserContext) Navigate(ctx context.Context, url string) error {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return errContextClosed
		}
		if !c.needsRestartLocked() {
			break
		}
		c.mu.Unlock()
		if err := c.restart(ctx); err != nil {
			return err
		}
	}
	c.wg.Add(1)
	c.inFlight++
	c.mu.Unlock()

	c.sessionMu.RLock()
	sess := c.session
	c.sessionMu.RUnlock()

	err := sess.Navigate(ctx, url)

	c.mu.Lock()
	if err == nil {
		c.navCount++
		c.confirmed = append(c.confirmed, url)
	}
	c.inFlight--
	c.mu.Unlock()
	c.wg.Done()
	return err
}

func (c *BrowserContext) needsRestartLocked() bool {
	return c.navCount >= maxNavigations || c.now().Sub(c.startedAt) >= maxContextAge
}

// restart 优雅重启：排空 in-flight（超时 30s）后以同一四元组重建 CDP 会话。
func (c *BrowserContext) restart(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errContextClosed
	}
	if c.restarting {
		done := c.restartDone
		c.mu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.restarting = true
	c.restartDone = make(chan struct{})
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.restarting = false
		close(c.restartDone)
		c.mu.Unlock()
	}()

	drained := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(drained)
	}()
	timer := time.NewTimer(c.drainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errDrainTimeout
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errContextClosed
	}
	sess, err := c.factory.Create(ctx, c.cfg)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	c.sessionMu.Lock()
	old := c.session
	c.session = sess
	c.sessionMu.Unlock()
	_ = old.Close()
	c.navCount = 0
	c.startedAt = c.now()
	c.mu.Unlock()
	return nil
}
