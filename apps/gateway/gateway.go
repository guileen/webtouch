package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"uws/pkg/clisession"
	"uws/pkg/contract"
)

const (
	// DefaultSessionID 与 Python DEFAULT_SESSION_ID 对齐。
	DefaultSessionID = "default"
	// devAPIKey 是引擎零配置开发内置 Key（与 pkg/engine.DevAPIKey 一致）。
	devAPIKey = "uws_dev_key"
)

// deadlineWaits 是 DEADLINE_EXCEEDED 的 Fibonacci 退避（1s/2s，最多 2 次重试，
// GLOBAL.md §5 裁决 #2 统一协调器侧语义）。
var deadlineWaits = []time.Duration{1 * time.Second, 2 * time.Second}

// BuildVersion 是网关二进制自身的构建标识（版本+git sha），由 make 的 -ldflags 注入。
// 运行中的网关据此能被机器核对是否为最新构建（`/v1/health` 的 version 字段 + 启动日志），
// 专治「源码新版本 / 进程旧版本」漂移——见 `make gateway-restart`。
var BuildVersion = "dev"

// Session 是一个会话：绑定一个浏览器标签页（引擎命名上下文，v1.5.3）。
type Session struct {
	ID         string
	ChannelID  string
	EngineAddr string
	ContextID  string
	TargetID   string
	CreatedAt  time.Time
	// LastUsedAt 是最近一次实际使用（本轮活动）时间；空闲回收按 max(CreatedAt, LastUsedAt) 判定（契约 §9）。
	LastUsedAt time.Time
	// Model 是已锁定的模式族名（如 "快速模式"/"Flash"）；"" = 未锁定。
	Model    string
	Thinking *bool
	Search   *bool
	// OwnConversationID 是本会话**自己绑定过**的站点服务端对话 ID（契约 §9）。
	// 只在站点回传过真实 ID（响应体自报 / echo 槽位回显 / S2 恢复入参）时写入，
	// **不用页签当前地址推断**；关会话清账的目标只能取它（2026-10-03 派单 d27954eeb）。
	OwnConversationID string
}

// Options 是 NewGateway 构造参数。
type Options struct {
	// EngineFactory 按 engine_addr 构造引擎客户端；nil = 生产 gRPC 客户端。
	EngineFactory func(addr string) (EngineClient, error)
	// EngineAPIKey 是引擎 x-api-key（EngineFactory 为 nil 时生效；空 = 开发内置 Key）。
	EngineAPIKey string
	// AttachCDP 是 InitializeContext 附着的 CDP 调试端点（如 http://127.0.0.1:9222）。
	// 非空时所有 web 通道会话共享该 Chrome 的登录态（桌面客户端场景）；
	// 空 = 引擎自建隔离 Chrome（服务端默认）。
	AttachCDP string
	// ChatTimeout 是单轮聊天总超时（默认 60s）。
	ChatTimeout time.Duration
	// VideoTimeout 是单轮视频生成总超时（默认 300s）。
	VideoTimeout time.Duration
	// ChatRetryAttempts 是「整轮重试」的额外次数（默认 1；<=0 关闭重试）。见 chatRetry*FromEnv。
	ChatRetryAttempts int
	// ChatRetryBackoff 是整轮重试的退避基数（第 n 次重试前 sleep n×基数；默认 2.5s）。
	ChatRetryBackoff time.Duration
	// AnswerStable 是新回答文本连续稳定多久视为完成（nil = 默认 1.5s；0 = 立即完成，测试用）。
	AnswerStable *time.Duration
	// ProfileManager 是可选的提供商沙箱管理器（nil = 默认 ~/.webtouch/profiles）。
	ProfileManager *ProviderProfileManager
	// Pacer 是拟人化频率与速度控制器（nil = 默认 DefaultHumanPacerConfig）。
	Pacer *HumanPacer
	// Sleep 是可注入的等待函数（默认 time.Sleep；测试注入 no-op）。
	Sleep func(time.Duration)
	// Logger 是可注入日志（默认 log.Default()）。
	Logger *log.Logger
	// AuthCheckTTL 是登录成功/未知状态的最长缓存时长；未登录/挑战锁定需显式 reset。0 使用默认 15 分钟。
	AuthCheckTTL time.Duration
	// AuthStatePath 存储登录状态元数据（不含 Cookie/令牌）；空值使用 ~/.uws/gateway-auth-state.json。
	AuthStatePath string
	// TabsStatePath 存储「网关自己开过的标签页」账本；空值使用 ~/.uws/gateway-tabs.json。
	TabsStatePath string
	// PurgeQueuePath 是「待删除列表」落盘路径（空 = ~/.uws/gateway-purge-queue.json，"-" = 关闭该能力）。
	PurgeQueuePath string
	// PurgeInterval 是后台清账间隔（0 = 默认 10 分钟 / 可由 UWS_PURGE_INTERVAL 覆盖；<0 = 不启动后台清账）。
	PurgeInterval time.Duration
	// KeepSessions 是「调试保留」总开关（契约 §14）：true 时任何关会话清账都**不删站点对话**，
	// 历史留在站点侧供 review。零值 false ＝ 现行为逐字不变。进程级由 UWS_KEEP_SESSIONS=1 置位，
	// 单次请求可用 `?keep=1`／头 `x-uws-keep-sessions: 1` 覆盖（见 Handler）。
	KeepSessions bool
	// SessionIdleTTL 是会话空闲回收阈值（0 = 默认 10 分钟 / 由 UWS_SESSION_IDLE_TTL 覆盖；<0 = 不启动回收）。
	SessionIdleTTL time.Duration
	// ChromeProfile 是自动拉起调试 Chrome 的 user-data-dir（空 = ~/.uws-chrome-debug）。
	ChromeProfile string
	// BrowserHostInterval 是宿主探活周期（0 = 默认 15s / 由 UWS_BROWSER_HOST_INTERVAL 覆盖；<0 = 不探活）。
	BrowserHostInterval time.Duration
	// CLISessions 是对外 CLI 的本机 lease。nil 表示迁移期 HTTP 不校验 CLI 句柄。
	CLISessions *clisession.Store
}

// ToolFunction 描述可供模型调用的函数结构。
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Tool 是 OpenAI 兼容工具定义。
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolCall 是模型给出的动作调用结构。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatRequest 是一次 chat 请求的解析结果（OpenAI 兼容子集 + web 通道扩展字段）。
type ChatRequest struct {
	ChannelID    string // 已解析的通道 id（强制通道前缀或模型路由）
	Model        string
	Messages     []Message
	Tools        []Tool
	ToolChoice   any
	Instructions string
	Stream       bool
	Thinking     *bool
	Search       *bool
	NewSession   bool
	SessionID    string
	// SiteConversationID 是调用方要接着聊的站点服务端对话 ID（契约 §9 S2）。
	// 非空且需要新建/重建页签时，页签直接落到该对话地址；站点未登记模板一律拒绝（fail-closed）。
	SiteConversationID string
	// CloseAfter 是一次性会话（契约 §9）：本轮回答交付后立即收口（先删站点会话再关 Tab）。
	CloseAfter bool
}

// Message 是 OpenAI 兼容消息。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ChoiceMessage 是 ChatCompletion 的 choices[].message。
type ChoiceMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// Choice 是 ChatCompletion 的 choices 条目。
type Choice struct {
	Index        int           `json:"index"`
	Message      ChoiceMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// ChatCompletion 是 OpenAI 兼容非流式响应形状（契约 §5）。
type ChatCompletion struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Model     string `json:"model"`
	SessionID string `json:"session_id"`
	ContextID string `json:"context_id"`
	TargetID  string `json:"target_id,omitempty"`
	Transport string `json:"uws_transport,omitempty"` // 实际走的那一档：in_page_direct（§13 L0）/ dom（§13 L1）
	// SiteConversationID 是站点服务端的对话 ID（契约 §9「站点会话身份」）；
	// nil = 尚无服务端对话或读不到（显式 null，不用页地址/猜值填充）。
	SiteConversationID *string `json:"site_conversation_id"`
	// PageURL 是回合结束时该页签的真实地址；nil = 读不到。
	PageURL *string  `json:"page_url"`
	Choices []Choice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ChatEvent 是流式聊天事件（handler 序列化为 SSE data 帧）。
type ChatEvent struct {
	Delta          string     // 增量内容（首帧带 role=assistant）
	ReasoningDelta string     // 增量思考内容（deepseek-reasoner / thinking）
	Role           bool       // 首帧：携带 role
	FinishReason   string     // "stop" = 流结束, "tool_calls" = 动作调用
	ToolCalls      []ToolCall // 识别到的动作调用
	Transport      string     // 实际走的那一档：in_page_direct / dom（契约 §13 逐层降级阶梯）
	// SiteConversationID / PageURL 只在完成帧回填（契约 §9 S1）；nil = 读不到（显式 null）。
	SiteConversationID *string
	PageURL            *string
	Err                error // 非 nil = 流错误（SSE error 事件）
}

// ChatError 是聊天轮次执行失败（HTTP 状态/错误码由 classifyChatError 按 Code 与语义判定，
// 见契约 §5「/v1/chat 失败分类」）。
type ChatError struct {
	Reason   string
	Semantic string
	Code     string // 失败分类码；留空时由 classifyChatError 推定
}

func (e *ChatError) Error() string { return e.Reason }

// BadRequestError 是 400 语义错误（OpenAI invalid_request_error）。
type BadRequestError struct{ Reason string }

func (e *BadRequestError) Error() string { return e.Reason }

// NotFoundError 是 404 语义错误（OpenAI invalid_request_error + 可用列表提示）。
type NotFoundError struct{ Reason string }

func (e *NotFoundError) Error() string { return e.Reason }

// NotImplementedError 是 501 语义错误（如 openai_http 通道 M1 未实现）。
type NotImplementedError struct{ Reason string }

func (e *NotImplementedError) Error() string { return e.Reason }

// AuthRequiredError 映射 HTTP 401：用户需先完成登录或人机验证，网关没有执行写动作。
type AuthRequiredError struct{ Reason, Code string }

func (e *AuthRequiredError) Error() string { return e.Reason }

// AuthCheckError 映射 HTTP 503：无法确认账号状态时宁可拒绝，也不冒险提交内容。
type AuthCheckError struct{ Reason, Code string }

func (e *AuthCheckError) Error() string { return e.Reason }

// IsTargetDestroyed 判断错误是否由标签页被关闭触发（自动重建整轮重试）。
// 用 errors.As 以穿透 fmt.Errorf("%w；…") 包装（见 classifyChatError 注释）。
func IsTargetDestroyed(err error) bool {
	var ce *ChatError
	if errors.As(err, &ce) {
		return !strings.Contains(ce.Reason, "submission_outcome_unknown") && strings.Contains(ce.Reason, "target_destroyed")
	}
	var ee *EngineError
	return errors.As(err, &ee) && ee.IsTargetDestroyed()
}

// Gateway 是统一网关核心：通道/模型解析 + 会话管理 + 聊天编排。
type Gateway struct {
	cfg           *Config
	engineFactory func(addr string) (EngineClient, error)
	providers     map[string]*ChatProvider // 按通道 id
	profileMgr    *ProviderProfileManager
	pacer         *HumanPacer
	chatTimeout   time.Duration
	videoTimeout  time.Duration
	// chatRetryAttempts / chatRetryBackoff：整轮重试上限与退避（2026-10-03 派单 d1957d57b）。
	// 可配常量：Options.ChatRetryAttempts/Backoff，或环境变量 UWS_CHAT_RETRY_ATTEMPTS /
	// UWS_CHAT_RETRY_BACKOFF_MS / UWS_CHAT_RETRY_ON_TIMEOUT。
	chatRetryAttempts  int
	chatRetryBackoff   time.Duration
	chatRetryOnTimeout bool
	answerStable       time.Duration
	sleep              func(time.Duration)
	logger             *log.Logger
	attachCDP          string
	authCheckTTL       time.Duration
	auth               map[string]authSnapshot // channel_id -> 最近一次只读登录检查
	authStatePath      string
	authPersistMu      sync.Mutex
	tabs               map[string]string // session_id -> 网关自己开过的标签页 target_id
	tabsStatePath      string

	mu      sync.Mutex
	engines map[string]EngineClient // 按 engine_addr
	purge   *PurgeQueue             // 「待删除列表」（站点会话清账）
	// keepSessions 是「调试保留」总开关（契约 §14）：关会话清账不删站点对话，历史留站点侧。
	keepSessions bool
	sessions     map[string]*Session
	locks        map[string]*sync.Mutex
	// sessionIdleTTL 是空闲回收阈值（<=0 = 关闭回收，见 lifecycle.go）。
	sessionIdleTTL time.Duration
	// siteGates 是「同站点串行 + 最小间隔」的低频门（见 lifecycle.go）。
	siteGates map[string]*siteGate
	current   string
	// browserHost 是调试 Chrome 宿主守护者（attach-cdp 为空时=nil，宿主不归网关管）。
	browserHost *BrowserHost
	// readiness 记录「一次真实推理」的端到端证据（进程 ≠ 可用，见 readiness.go）。
	readiness readinessState
	// cliSessions 是 CLI 对外句柄的本机 lease。nil = 不校验（迁移期旧调用）。
	cliSessions *clisession.Store
}

// NewGateway 构造网关；cfg 必须已 Normalize。
func NewGateway(cfg *Config, opts Options) (*Gateway, error) {
	if cfg == nil {
		return nil, fmt.Errorf("gateway: cfg 不能为 nil")
	}
	factory := opts.EngineFactory
	if factory == nil {
		factory = defaultEngineFactory(opts.EngineAPIKey)
	}
	profileMgr := opts.ProfileManager
	if profileMgr == nil && opts.AttachCDP == "" {
		profileMgr, _ = NewProviderProfileManager("")
	}
	pacer := opts.Pacer
	if pacer == nil {
		pacer = NewHumanPacer(DefaultHumanPacerConfig())
	}
	if opts.Sleep != nil {
		pacer.SetSleepFunc(opts.Sleep)
	}
	g := &Gateway{
		cfg:                cfg,
		engineFactory:      factory,
		providers:          make(map[string]*ChatProvider, len(cfg.Channels)),
		profileMgr:         profileMgr,
		pacer:              pacer,
		chatTimeout:        DefaultChatTimeout,
		answerStable:       1500 * time.Millisecond,
		chatRetryAttempts:  chatRetryAttemptsFromEnv(),
		chatRetryBackoff:   chatRetryBackoffFromEnv(),
		chatRetryOnTimeout: chatRetryOnTimeoutFromEnv(),
		sleep:              time.Sleep,
		logger:             log.Default(),
		attachCDP:          opts.AttachCDP,
		authCheckTTL:       15 * time.Minute,
		auth:               make(map[string]authSnapshot),
		engines:            make(map[string]EngineClient),
		sessions:           make(map[string]*Session),
		locks:              make(map[string]*sync.Mutex),
		cliSessions:        opts.CLISessions,
	}
	// 浏览器宿主纳管（2026-10-01「关窗 ≠ 断链路」）：attach-cdp 非空即纳管，
	// 探活周期可由 Options 或 UWS_BROWSER_HOST_INTERVAL 覆盖；<0 = 只报告不探活。
	if opts.AttachCDP != "" && opts.BrowserHostInterval >= 0 {
		interval := opts.BrowserHostInterval
		if interval == 0 {
			interval = BrowserHostIntervalFromEnv()
		}
		g.browserHost = NewBrowserHost(opts.AttachCDP, opts.ChromeProfile, interval, g.logger)
	}
	if opts.AuthStatePath != "" {
		g.authStatePath = opts.AuthStatePath
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		g.authStatePath = filepath.Join(home, ".uws", "gateway-auth-state.json")
	}
	g.loadAuthState()
	switch {
	case opts.TabsStatePath != "":
		g.tabsStatePath = opts.TabsStatePath
	case opts.AuthStatePath != "":
		// 测试/多实例把状态目录注入 AuthStatePath：账本跟着走同一目录，
		// 绝不写进生产 ~/.uws（2026-10-02 单测污染账本的教训）。
		g.tabsStatePath = filepath.Join(filepath.Dir(opts.AuthStatePath), "gateway-tabs.json")
	default:
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			g.tabsStatePath = filepath.Join(home, ".uws", "gateway-tabs.json")
		}
	}
	g.loadTabLedger()
	// 「调试保留」：Options 显式置位优先，否则读 UWS_KEEP_SESSIONS（默认关）。
	g.keepSessions = opts.KeepSessions || keepSessionsFromEnv()
	// 会话生命周期：空闲回收阈值来自 Options（0 = 环境变量默认，<0 = 关闭回收）。
	ttl := opts.SessionIdleTTL
	if ttl == 0 {
		ttl = DefaultSessionIdleTTL()
	}
	if ttl > 0 {
		g.sessionIdleTTL = ttl
	}
	if opts.PurgeQueuePath != "-" {
		path := opts.PurgeQueuePath
		if path == "" {
			path = DefaultPurgeQueuePath()
		}
		g.purge = NewPurgeQueue(path)
		interval := opts.PurgeInterval
		if interval == 0 {
			interval = defaultPurgeInterval()
		}
		if interval > 0 {
			g.StartPurgeWorker(interval)
		}
	}
	if opts.ChatTimeout > 0 {
		g.chatTimeout = opts.ChatTimeout
	}
	g.videoTimeout = opts.VideoTimeout
	if g.videoTimeout <= 0 {
		g.videoTimeout = DefaultVideoTimeout
	}
	if opts.ChatRetryAttempts > 0 {
		g.chatRetryAttempts = opts.ChatRetryAttempts
	}
	if opts.ChatRetryBackoff > 0 {
		g.chatRetryBackoff = opts.ChatRetryBackoff
	}
	if opts.AnswerStable != nil {
		g.answerStable = *opts.AnswerStable
	}
	if opts.Sleep != nil {
		g.sleep = opts.Sleep
	}
	if opts.AuthCheckTTL > 0 {
		g.authCheckTTL = opts.AuthCheckTTL
	}
	if opts.Logger != nil {
		g.logger = opts.Logger
	}
	for i := range cfg.Channels {
		ch := &cfg.Channels[i]
		if ch.Type != ChannelTypeWeb {
			continue // openai_http 等通道 M1 仅登记，路由层 501
		}
		p, err := newProvider(ch.Site, ch.Models)
		if err != nil {
			return nil, fmt.Errorf("gateway: 通道 %s: %w", ch.ID, err)
		}
		g.providers[ch.ID] = p
	}
	return g, nil
}

// Config 暴露配置（内部 provider 模型目录、健康检查用）。
func (g *Gateway) Config() *Config { return g.cfg }

// Provider 返回通道对应的站点 Provider（nil = 非 web 通道）。
func (g *Gateway) Provider(channelID string) *ChatProvider {
	return g.providers[channelID]
}

// engineFor 按通道解析/缓存引擎客户端。
func (g *Gateway) engineFor(ch *ChannelConfig) (EngineClient, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.engines[ch.EngineAddr]; ok {
		return e, nil
	}
	e, err := g.engineFactory(ch.EngineAddr)
	if err != nil {
		return nil, err
	}
	g.engines[ch.EngineAddr] = e
	return e, nil
}

// sessionLock 返回会话锁（不同会话可并行，同一会话串行）。
func (g *Gateway) sessionLock(id string) func() {
	g.mu.Lock()
	l := g.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		g.locks[id] = l
	}
	g.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// getSession 只读取会话。
func (g *Gateway) getSession(id string) *Session {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessions[id]
}

// sessionEntryURL 解析「新页签 / 重建页签」的落点（契约 §9 S2）：带 site_conversation_id
// 时落该站点对话页，否则落通道默认入口。站点未登记模板 / ID 非法 ⇒ fail-closed 报错，
// 不用通道首页冒充恢复（否则调用方会以为接着聊上了，实际是新对话）。
func sessionEntryURL(ch *ChannelConfig, siteConvID string) (string, error) {
	id := strings.TrimSpace(siteConvID)
	if id == "" {
		return ch.PageURL, nil
	}
	u, ok := SiteConversationPageURL(ch.Site, id)
	if !ok {
		return "", &BadRequestError{Reason: fmt.Sprintf(
			"通道 %q（站点 %q）未登记会话地址模板或 ID 非法，无法按 site_conversation_id=%q 恢复（fail-closed）",
			ch.ID, ch.Site, id)}
	}
	return u, nil
}

// validateSiteConversationRequest 校验恢复入参（契约 §9 S2）：命名禁令之外，还要求
// 「带 ID 就必须能落该对话页」——站点未登记模板一律拒绝，不静默忽略。
func validateSiteConversationRequest(ch *ChannelConfig, req ChatRequest) error {
	id := strings.TrimSpace(req.SiteConversationID)
	if id == "" {
		return nil
	}
	if req.NewSession {
		return &BadRequestError{Reason: "new_session 与 site_conversation_id 互斥：要新开对话就不要带 site_conversation_id"}
	}
	if ch == nil {
		return &BadRequestError{Reason: fmt.Sprintf("site_conversation_id=%q 需要先解析到 web 通道", id)}
	}
	if _, ok := SiteConversationPageURL(ch.Site, id); !ok {
		return &BadRequestError{Reason: fmt.Sprintf(
			"通道 %q（站点 %q）未登记会话地址模板，不接受 site_conversation_id=%q（fail-closed，不猜地址）", ch.ID, ch.Site, id)}
	}
	return nil
}

// ensureSession 确保 session_id 已绑定一个浏览器标签页（引擎命名上下文）。
// 首次请求（或标签被关闭后重建）创建新 Tab 并导航到落点：siteConvID 非空 =
// 该站点对话页（契约 §9 S2 恢复语义），否则 = 通道 page_url。
func (g *Gateway) ensureSession(ctx context.Context, ch *ChannelConfig, sid, siteConvID string) (*Session, error) {
	entryURL, err := sessionEntryURL(ch, siteConvID)
	if err != nil {
		return nil, err
	}
	if sess := g.getSession(sid); sess != nil && sess.ContextID != "" {
		return sess, nil
	}
	eng, err := g.engineFor(ch)
	if err != nil {
		return nil, &ChatError{Reason: fmt.Sprintf("引擎不可用(%s): %v", ch.EngineAddr, err), Code: errCodeEngineUnavailable}
	}

	var init InitResult
	// Provider fingerprint overrides belong only to Gateway-owned isolated Chrome.
	// An attached desktop Chrome already has a real browser identity and applying a
	// fixed provider profile can make its UA/UA-CH disagree with the actual version.
	if accountID := strings.TrimSpace(ch.AccountID); accountID != "" {
		if g.attachCDP != "" {
			return nil, &BadRequestError{Reason: fmt.Sprintf("通道 %q 配置了 account_id=%q，但 attach-cdp 的账号/Profile 由指定 Chrome 决定；请改用 WebTouch 隔离浏览器以应用账号绑定", ch.ID, accountID)}
		}
		if g.profileMgr == nil {
			return nil, &BadRequestError{Reason: fmt.Sprintf("通道 %q 配置了 account_id=%q，但 WebTouch Profile 管理器不可用", ch.ID, accountID)}
		}
		prof, pErr := g.profileMgr.ResolveAccountProfile(accountID)
		if pErr != nil {
			return nil, &BadRequestError{Reason: fmt.Sprintf("通道 %q 的账号/Profile 绑定无效: %v", ch.ID, pErr)}
		}
		profBytes, _ := json.Marshal(prof)
		init, err = eng.InitializeContextWithProfile(ctx, "uws-api-"+sid, entryURL, sid, g.attachCDP, prof.ProfileID, string(profBytes))
	} else if ch.Site != "" && g.profileMgr != nil && g.attachCDP == "" {
		prof, pErr := g.profileMgr.EnsureProviderProfile(ch.Site)
		if pErr == nil && prof != nil {
			profBytes, _ := json.Marshal(prof)
			init, err = eng.InitializeContextWithProfile(ctx, "uws-api-"+sid, entryURL, sid, g.attachCDP, prof.ProfileID, string(profBytes))
		} else {
			init, err = eng.InitializeContext(ctx, "uws-api-"+sid, entryURL, sid, g.attachCDP)
		}
	} else {
		init, err = eng.InitializeContext(ctx, "uws-api-"+sid, entryURL, sid, g.attachCDP)
	}
	if err != nil {
		return nil, &ChatError{Reason: fmt.Sprintf("initialize_context(%s): %v", sid, err)}
	}
	if !init.OK {
		return nil, &ChatError{Reason: fmt.Sprintf("initialize_context(%s): %s", sid, init.ErrorReason)}
	}
	if strings.HasPrefix(init.TargetID, "sim-") {
		g.logger.Printf("警告: 引擎处于模拟会话（sim），返回内容不是真实浏览器操作！请以 --browser cdp 启动引擎")
	}
	sess := &Session{
		ID: sid, ChannelID: ch.ID, EngineAddr: ch.EngineAddr,
		ContextID: init.ContextID, TargetID: init.TargetID, CreatedAt: time.Now(),
	}
	// S2 恢复入参就是「本会话自己绑定的对话」：建会话时记下，供关会话清账取目标用。
	if cid := strings.TrimSpace(siteConvID); cid != "" {
		if norm, ok := NormalizeSiteConversationID(ch.Site, cid); ok {
			sess.OwnConversationID = norm
		}
	}
	g.mu.Lock()
	g.sessions[sid] = sess
	g.current = sid
	g.mu.Unlock()
	g.recordTab(sid, sess.TargetID)
	return sess, nil
}

// closeSession 关闭会话（内存出账）并释放其标签页（引擎 CloseContext，幂等）。
// DELETE /v1/sessions、new_session 重建、标签被关闭后自动重建前调用。
//
// 返回 tabClosed=页签确认已关（本来就没有页签也算）；关不掉时返回 false ＋ 原因，
// 并且**账本条目保留**——T-34 缺陷根因是「先出账、后关页、且忽略 CloseContext 错误」，
// 页签没关成时账本里已查不到它 ⇒ 永久孤儿，启动回收也看不见。保留条目后，
// 启动回收（ReapOrphanTabs）与下一次 DELETE 都还有落点。
func (g *Gateway) closeSession(ctx context.Context, sid string) (tabClosed bool, closeErr error) {
	g.mu.Lock()
	sess := g.sessions[sid]
	delete(g.sessions, sid)
	if g.current == sid {
		g.current = ""
	}
	g.mu.Unlock()
	if sess == nil || sess.ContextID == "" {
		// 无内存会话：进程重启遗留的页签只能借调试端点确认（attach 模式）。
		// 注意：**不**在这里清引擎落盘绑定——本函数也被「首次建会话」路径调用，
		// 那条路径要靠绑定续页（网关重启后接管原来那扇窗）。清绑定只属于
		// 显式删除（见 closeSessionWithPurge 的 dropEngineBindings）。
		if !g.closeLedgerTab(ctx, sid) {
			return false, fmt.Errorf("调试端点不可用，无法确认会话 %s 的页签已关（账本保留待回收）", sid)
		}
		return true, nil
	}
	ch := g.cfg.Channel(sess.ChannelID)
	if ch == nil {
		return false, fmt.Errorf("通道 %q 已不在配置里，无法确认会话 %s 的页签已关（账本保留待回收）", sess.ChannelID, sid)
	}
	eng, err := g.engineFor(ch)
	if err != nil {
		return false, fmt.Errorf("引擎不可用，无法确认会话 %s 的页签已关（账本保留待回收）: %w", sid, err)
	}
	if err := eng.CloseContext(ctx, sid); err != nil {
		return false, fmt.Errorf("关闭会话 %s 的页签失败（账本保留待回收）: %w", sid, err)
	}
	g.forgetTab(sid)
	return true, nil
}

// dropEngineBindings 清掉各引擎里该会话名的落盘绑定（幂等；重复地址只调一次）。
// 缺口③（2026-10-02 派单 d1f5493c6 登记、2026-10-03 d613c7908 收口）：显式删除会话时
// 「网关也重启过」的话，内存里没有会话、引擎侧的「会话名 → Target」落盘绑定却还在 ⇒
// 单靠 DELETE /v1/sessions/<sid> 清不掉，下一次同名会话还要去接管一扇已经关掉的窗。
// 这里对每个已登记引擎做一次幂等 CloseContext（引擎无内存上下文时也会删绑定，见
// pkg/engine/service_page.go）；失败只记日志——绑定是续页优化，不该让删除本身失败。
func (g *Gateway) dropEngineBindings(ctx context.Context, sid string) {
	if sid == "" {
		return
	}
	seen := map[string]bool{}
	for i := range g.cfg.Channels {
		ch := &g.cfg.Channels[i]
		if seen[ch.EngineAddr] {
			continue
		}
		seen[ch.EngineAddr] = true
		eng, err := g.engineFor(ch)
		if err != nil {
			continue
		}
		if err := eng.CloseContext(ctx, sid); err != nil && g.logger != nil {
			g.logger.Printf("会话 %s 的引擎绑定清理失败（%s）: %v", sid, ch.EngineAddr, err)
		}
	}
}

// SessionsSnapshot 是 /v1/sessions GET 响应数据。
type SessionsSnapshot struct {
	Current   string            `json:"session_id"`
	Sessions  []string          `json:"sessions"`
	Tabs      map[string]string `json:"tabs"`
	TargetIDs map[string]string `json:"target_ids,omitempty"`
	Models    map[string]string `json:"models"`
	Channels  map[string]string `json:"channels"`
}

// ListSessions 返回会话列表快照。
func (g *Gateway) ListSessions() SessionsSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := SessionsSnapshot{
		Current:   g.current,
		Sessions:  make([]string, 0, len(g.sessions)),
		Tabs:      make(map[string]string, len(g.sessions)),
		TargetIDs: make(map[string]string, len(g.sessions)),
		Models:    make(map[string]string, len(g.sessions)),
		Channels:  make(map[string]string, len(g.sessions)),
	}
	for id := range g.sessions {
		out.Sessions = append(out.Sessions, id)
	}
	// 稳定排序便于测试与展示
	for i := 0; i < len(out.Sessions); i++ {
		for j := i + 1; j < len(out.Sessions); j++ {
			if out.Sessions[j] < out.Sessions[i] {
				out.Sessions[i], out.Sessions[j] = out.Sessions[j], out.Sessions[i]
			}
		}
	}
	for _, id := range out.Sessions {
		s := g.sessions[id]
		out.Tabs[id] = s.ContextID
		out.TargetIDs[id] = s.TargetID
		out.Models[id] = s.Model
		out.Channels[id] = s.ChannelID
	}
	return out
}

// resolveChannel 解析强制通道（path 前缀）或模型路由。
func (g *Gateway) resolveChannel(channelID, model string) (*ChannelConfig, error) {
	if channelID != "" {
		ch := g.cfg.Channel(channelID)
		if ch == nil {
			return nil, &NotFoundError{Reason: fmt.Sprintf("未知通道 %q", channelID)}
		}
		return ch, nil
	}
	if model == "" {
		return nil, &BadRequestError{Reason: "model 必填（客户端请使用 Agent Hub 的 GET /v1/models）"}
	}
	ch, _, err := g.cfg.ChannelByModel(model)
	if err != nil {
		return nil, &NotFoundError{Reason: err.Error() + "（请从 Agent Hub GET /v1/models 查看可用模型）"}
	}
	return ch, nil
}

// EvaluatorForSession 返回指定会话的页内脚本执行器（In-Page Direct Transport 基础）。
func (g *Gateway) EvaluatorForSession(sid string) contract.DirectScriptEvaluator {
	sess := g.getSession(sid)
	if sess == nil || sess.ContextID == "" {
		return nil
	}
	ch := g.cfg.Channel(sess.ChannelID)
	if ch == nil {
		return nil
	}
	eng, err := g.engineFor(ch)
	if err != nil {
		return nil
	}
	return NewEngineScriptEvaluator(eng, sid)
}

// ── 整轮重试与超时预算的可配常量（2026-10-03 派单 d1957d57b） ──
//
// 实测依据（同一 synth-prompt、同一固定 session_id 连打 6 次，gemini-flash）：
// 耗时 51.8／69.6(502)／84.7／44.0／31.1／31.0 秒 ⇒ 单轮真实耗时在 31–85s 之间摆动；
// 失败逐字原因是 `submission_outcome_unknown: engine error code=4: context deadline exceeded`
// （本网关自己的超时闸，不是站点返回的业务码）。定性＝「通道本身就慢 + 60s 预算切掉一部分」，
// 不是「冷建页预算不够」（冷建页只占约 15–22s）。两条加固据此定性：
//
//	① 抬预算：DefaultChatTimeout 60s → 120s，覆盖实测最坏 84.7s 并留余量。
//	② 不做「超时后整轮重放」：超时不能证明写操作未发生，重放可能让站点重复答一轮，
//	   违反 chat_submission.go 的 T-26 fail-closed 不变量；保留的重试只针对提交前就绪类失败。
const (
	defaultChatRetryAttempts = 1                       // 额外重试 1 次（共 2 次尝试）；仅对「提交前就绪类失败」生效
	defaultChatRetryBackoff  = 2500 * time.Millisecond // 第 n 次重试前 sleep n×基数
	// defaultChatRetryOnTimeout＝false：默认不对「本轮超时类失败」重试（T-26 安全默认，
	// 见上方 ②；UWS_CHAT_RETRY_ON_TIMEOUT=1 可显式放宽，自担重复风险）。
	defaultChatRetryOnTimeout = false
	// engineUnavailableRetries/Backoff：重建会话时对「引擎整段不可达」做的有界快重拨。
	// 引擎短暂重启（秒级）可被自动吸收，不必把失败立刻推给调用方；不是整轮重放，
	// 不触碰 T-26（重建会话时写操作尚未发生）。
	engineUnavailableRetries = 3
	engineUnavailableBackoff = 400 * time.Millisecond
	// DefaultChatTimeout 是单轮聊天总超时默认值；实测 gemini-flash 慢轮可达 85s，
	// 60s 会切掉合法慢轮，故抬到 120s。导出供 cmd/uws-gateway 的 --chat-timeout
	// 旗标默认值对齐（旗标默认值会经 Options.ChatTimeout 覆盖此常量，两者必须同源）。
	DefaultChatTimeout = 120 * time.Second
)

// chatRetryAttemptsFromEnv 读 UWS_CHAT_RETRY_ATTEMPTS（非负整数；非法值回落默认）。
func chatRetryAttemptsFromEnv() int {
	if v := strings.TrimSpace(os.Getenv("UWS_CHAT_RETRY_ATTEMPTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultChatRetryAttempts
}

// chatRetryBackoffFromEnv 读 UWS_CHAT_RETRY_BACKOFF_MS（毫秒；非法值回落默认）。
func chatRetryBackoffFromEnv() time.Duration {
	if v := strings.TrimSpace(os.Getenv("UWS_CHAT_RETRY_BACKOFF_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return defaultChatRetryBackoff
}

// chatRetryOnTimeoutFromEnv 读 UWS_CHAT_RETRY_ON_TIMEOUT（默认 0=关）。
// 置 1 才把「本轮超时类失败」纳入整轮重试；默认关闭以维持 chat_submission.go 的
// T-26 fail-closed 口径（提交结果不明时禁止重放，避免站点重复答一轮）。
func chatRetryOnTimeoutFromEnv() bool {
	if v := strings.TrimSpace(os.Getenv("UWS_CHAT_RETRY_ON_TIMEOUT")); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return defaultChatRetryOnTimeout
}

// KeepSessionsEnv 是「调试保留」的环境变量名（契约 §14）：=1/true 时关会话不清站点对话。
// 与请求级开关同语义（`?keep=1`／头 `x-uws-keep-sessions: 1`），一处定义、两端可用。
const KeepSessionsEnv = "UWS_KEEP_SESSIONS"

// KeepSessionsHeader 是「调试保留」的请求头名（调用侧可不用动网关进程环境即逐请求声明）。
const KeepSessionsHeader = "x-uws-keep-sessions"

// keepSessionsFromEnv 读 UWS_KEEP_SESSIONS（默认关；只有 1/true 才算开——「少删」方向，默认仍是删）。
func keepSessionsFromEnv() bool {
	v := strings.TrimSpace(os.Getenv(KeepSessionsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}
