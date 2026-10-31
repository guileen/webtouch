package engine

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"uws/pkg/browser"
	"uws/pkg/challenge"
	"uws/pkg/classifier"
	"uws/pkg/fingerprint"
	"uws/pkg/knowledge"
	"uws/pkg/matcher"
	"uws/pkg/verifier"
	"uws/pkg/viewerhub"
	"uws/pkg/vlm"
	uwspb "uws/proto/gen/go/proto/v1"
)

const (
	// defaultContextName 是「每客户端单上下文」旧语义的上下文名（context_hint 缺省）。
	defaultContextName = ""
	// contextSep 用于拼接「客户端 Key + 上下文名」的复合 map key。
	contextSep = "\x00"
	// DevAPIKey 是零配置开发模式的内置 Key（生产必须 --api-key-file 覆盖）。
	DevAPIKey = "uws_dev_key"
	// IdempotencyTTL 是幂等缓存 TTL（proto v1.5 / TDD-API-02）。
	IdempotencyTTL = 60 * time.Second
	// sweepInterval 是幂等账本（cache/inflight/indeterminate）的清扫间隔：账本条目
	// 过去只在「同一个 intent 再来」时才被删，长跑引擎上会随不同 intent_id 数单调增长
	// （2026-09-23 审查结论 item 3）。这里按间隔做有界回收，代价是一次摊销扫描。
	sweepInterval = 30 * time.Second
	// defaultVerifyTimeout 是动作局部验证的默认总超时（proto timeout_ms 默认 1500）。
	defaultVerifyTimeout = 1500 * time.Millisecond
	// verifyPollInterval 是稳定性等待的轮询间隔。
	verifyPollInterval = 10 * time.Millisecond
	// AutoRollbackThreshold 是 P0 自动回滚触发阈值：
	// 连续 50 个 ExecuteAction 返回 NOT_FOUND（大面积失明）→ 自动回滚
	// （spec/knowledge-versioning.md §5.2 / TDD-KB-06）。
	AutoRollbackThreshold = 50
)

// EngineVersion 是引擎自身版本（知识库兼容性校验基准）。
// 故意用 var 而非 const，以便构建时用 -ldflags "-X uws/pkg/engine.EngineVersion=<VERSION>+<sha>"
// 注入真实构建版本，让运行中二进制可被 `uws daemon status` 的 HealthCheck 核对（防源码/进程漂移）。
// 手工 go build 不注入时回落到 VERSION 文件的默认值（Makefile build 同步保持）。
var EngineVersion = "v1.5.5"

// contextKey 生成客户端级上下文槽位 key：同一客户端可持有多个命名上下文
// （= 多个浏览器标签页，多会话并行，v1.5.3）；缺省名保持旧「单上下文」语义。
func contextKey(clientKey, name string) string {
	return clientKey + contextSep + name
}

// ckeyFor 计算当前请求的上下文槽位 key。
//   - 空 context_name（默认上下文）：保留旧语义，按 ClientKey（API Key + 连接地址）
//     隔离，同一连接多次初始化复用（R3）。
//   - 具名 context_name（v1.5.6 Session ID）：按 API Key 全局寻址——同一 Key 的
//     不同 CLI 进程也能续作同一标签页（会话续页）；不同 Key 物理隔离。
func (s *Server) ckeyFor(ctx context.Context, name string) string {
	if name == "" {
		return contextKey(s.auth.ClientKey(ctx), name)
	}
	return "sess|" + s.auth.APIKey(ctx) + contextSep + name
}

type cachedResult struct {
	expiresAt time.Time
	resp      *uwspb.ActionResultResponse
}

// pageVerifier 是单个 Target 的 SPA 时序状态包装（TDD-ENG-06/07）：
// verifier 状态机 + 本次调用内「刚完成 NAVIGATED 稳定化」标记，
// 用于在 ActionResultResponse 中向协调器通告导航完成。
type pageVerifier struct {
	mu           sync.Mutex // 串行化同 Target 的 ExecuteAction（并发动作竞争页面状态，R5）
	v            *verifier.Verifier
	navCompleted bool
}

// Options 是 NewServer 的构造参数；全部字段可选（零值走内置默认）。
type Options struct {
	Pool *browser.Pool
	// SessionFactory 是 Target 池的会话工厂（默认 SimFactory）。
	// 传入 browser.CDPFactory 即启用真实 CDP 物理执行。
	SessionFactory browser.SessionFactory
	// LeaseStore enables the profile single-writer lifecycle for all sessions
	// created by this engine. HolderID must identify this engine node; a missing
	// value is rejected at Create time rather than silently disabling leases.
	LeaseStore    fingerprint.LeaseBackend
	LeaseHolderID string
	LeaseTTL      time.Duration
	Knowledge     *knowledge.Store
	Matcher       *matcher.Matcher
	Classifier    *classifier.Classifier
	Keys          []APIKey
	MaxTargets    int // >0 时覆盖 Pool 容量（0 = CPU×1.5，仅当 Pool 为 nil 时生效）
	// LogPath 是 online_logs SQLite 落库路径（algorithm-kernel.md §4.4）；
	// 空 = 不记录动作日志（零配置模式）。
	LogPath string
	// EpisodeDir 是在线 Episode 的 append-only JSONL 落盘目录
	// （algorithm-kernel.md §5.0.12，T-22）；空 = 不采集（零配置模式）。
	// 只读旁路：不改判定、不改返回值，写盘失败只记日志。
	EpisodeDir string
	// OnnxRuntimeLib 是 libonnxruntime 共享库路径（A 案 ort-go CGO，
	// USER_ACTION.md #1）；空 = 不启用真实 ONNX（决策核降级为确定性 mock）。
	// 模型字节来自 KB artifact（OnnxModel），热更/回滚随 artifact 同步重建。
	OnnxRuntimeLib string
	// ShadowOnnxModelPath 是**只读影子通道**的 ONNX 模型文件路径（§5.0.14 条款 3①）：
	// 与决策 scorer 正交——独立入口 ＋ 独立字段（Server.shadowScorer），构造路径不经
	// syncScorerWithKnowledge、不写 onnxScorer、不进 pageCandidatePool；共享库复用
	// OnnxRuntimeLib。空 = 影子未接线（shadow_inference 如实 null，由消费端计数）。
	ShadowOnnxModelPath string
	// ShadowOnnxModelVersion 是影子模型的版本串（空 = 取模型文件名；只作溯源，不改判定）。
	ShadowOnnxModelVersion string
	// ViewerHub 是可选的观看小窗传输（pkg/viewerhub）。非 nil 时，每个支持
	// Page.startScreencast 的会话都会把画面/指针/脱敏动作被动推给本机 Viewer；
	// 会话之间共用浏览器 Profile（Cookie/缓存），但各有各的 tab/窗口/面板，
	// 互不争抢（R5）。nil = 不推流（默认，零开销）。
	ViewerHub *viewerhub.Hub
	// VLM 是视觉感知兜底（2026-08-27）：语义/选择器「听不见」时截图 → VLM 候选 →
	// 确定性验证执行 → 成功样本写回 learned（下次免 VLM）。nil = 关闭（默认）。
	VLM *vlm.Fallback
	// Sessions 是「会话名 → Target」绑定的落盘存储（v1.5.7 会话续页，GLOBAL #118）。
	// 非 nil 时，命名会话的目标会落盘；引擎重启失忆后同名会话直接接管原来那扇
	// 窗口继续干，而不是再开一扇新窗。nil = 不持久化（旧行为，测试无需改动）。
	Sessions *SessionStore
	// VaultResolve 解引用「凭据直填」引用 `@vault:<名>`（2026-09-23）：默认读本机
	// `pkg/vault`（`~/.uws/vault.{key,age}`，见 vault.ResolvePaths）；测试可注入
	// 假实现免碰真库。返回错误即 fail-closed。
	VaultResolve func(name string) (string, error)
}

// Server 实现 uwspb.UwsEngineServer，把已点亮的逻辑包接线到冻结契约。
type Server struct {
	uwspb.UnimplementedUwsEngineServer

	pool    *browser.Pool
	kb      *knowledge.Store
	matcher *matcher.Matcher
	clf     *classifier.Classifier
	auth    *Authenticator
	onnxLib string

	scorerMu   sync.RWMutex // 保护 onnxScorer / shadowScorer（热更重建 vs runAction 并发读取）
	onnxScorer *classifier.OnnxScorer

	// shadowScorer 是**只读影子通道**打分器（§5.0.14 条款 3）：与决策 scorer 完全独立，
	// 只落 Episode.shadow_inference，不参与 decision、不进 pageCandidatePool。
	// 其构造路径绝不触碰 onnxScorer（单测硬断言见 pkg/engine/shadow_test.go）。
	shadowScorer       episodeShadowScorer
	shadowModelPath    string
	shadowVersionSeed  string
	shadowModelVersion string
	shadowModelHash    string

	vlmFb *vlm.Fallback

	mu       sync.Mutex
	contexts map[string]*browser.BrowserContext

	// sessions 持久化「会话名 → Target」绑定（可为 nil = 不持久化）。
	sessions *SessionStore

	// viewers 是每个浏览器会话的观看推流器（key = BrowserContext.ID）。
	viewerHub  *viewerhub.Hub
	viewerMu   sync.Mutex
	viewers    map[string]*viewerhub.Session
	viewerStop map[string]context.CancelFunc
	// viewerSessions 让反向意图（壳 → 引擎）能找到对应的真实浏览器会话：意图只带
	// 会话 id，搬窗口这件事必须落在这个会话自己的那扇窗上（R5）。
	viewerSessions map[string]browser.CDPSession
	cache          map[string]cachedResult
	inflight       map[string]struct{} // intent_id 在途去重（R4：同 intent 并发提交不重复物理执行）
	// indeterminate 记录「物理事件已派发但结果未知」的 intent（值=判定时刻）：
	// 取消/超时不能撤销已发给 Chrome 的点击或导航，所以这段时间内不能释放去重状态，
	// 否则同一 intent 的并发重试会把一次点击变成两次（2026-09-23 审查结论）。
	indeterminate map[string]time.Time
	// lastSweep 是上一次幂等账本清扫的时刻（sweepInterval 节流用，见 sweepLedgerLocked）。
	lastSweep time.Time
	traceSeq  atomic.Uint64

	pageMu sync.Mutex
	pages  map[string]*pageVerifier

	// notFoundStreak 连续 NOT_FOUND 计数（P0 自动回滚信号，TDD-KB-06）。
	notFoundStreak atomic.Int32
	logs           *LogStore // online_logs 落库（--log-path），nil = 不记录
	// episodes 是在线 Episode 的 append-only 落盘（--episode-dir），nil = 不采集。
	episodes *EpisodeWriter
	// wasmTruncTotal 是 WASM 探针截断累计次数（uws_wasm_truncation_total 数据源；
	// Prometheus 导出待指标依赖定案，先以原子计数器 + WasmTruncationTotal 观测）。
	wasmTruncTotal atomic.Uint64

	// wstnShadowTotal 记录 WSTN 旁路预测触发次数（只作比对审计，不影响生产 A*）。
	wstnShadowTotal atomic.Uint64
	// wstnShadowValid 记录 WSTN 契约校验通过次数。
	wstnShadowValid atomic.Uint64

	// astarBlacklist 是 A* 搜索的运行时物理黑名单（HTTP 4XX/5XX 湮灭节点，
	// 跨请求驻留，TDD-MATCH-06 集成层）。
	astarBlacklist *matcher.PhysicalBlacklist

	// 防频繁乱点熔断中枢（战役一）：每个会话独立的熔断器，杜绝风控封禁
	breakerMu sync.Mutex
	breakers  map[string]*challenge.CircuitBreaker

	// mutexSlots 把「互斥键 → 拥有者上下文」登记在引擎内存里（P1 同站点跨
	// session 复用，2026-09-23 用户授权自决：共享同窗 + 不重导航 + 动作串行）。
	// 窗口按设计持久、任务结束没有信号，故这里**不引租约**（租约只能靠 TTL 猜：
	// 猜短了夺走前一个正在用的窗，猜长了后来者僵等）；拥有者 CloseContext 时
	// 归还该键并解绑借用者。由 s.mu 保护，与 s.contexts 同锁。
	mutexSlots map[string]mutexSlot

	debugSrv *DebugServer

	// vaultResolve 解引用 `@vault:<名>`（凭据直填）：明文只在执行进程内出现一次，
	// 不进 argv（--plan 文本里只有条目名）、不进 Python、不过 gRPC、不进日志。
	vaultResolve func(name string) (string, error)
}

// mutexSlot 是一个互斥键的拥有关系：拥有者上下文 + 拥有者的上下文 key
// （用于区分「拥有者关闭」与「借用者关闭」——后者不得关掉共享窗口）。
type mutexSlot struct {
	bc    *browser.BrowserContext
	owner string // s.ckeyFor 的结果
}

// NewServer 构造引擎服务；默认接线：模拟 CDP 会话、内置知识库、
// 规则库与知识库同源（R1）+ mock 决策核（真实 ONNX 适配待接线，见 USER_ACTION.md）。
func NewServer(opts Options) (*Server, error) {
	if opts.Pool == nil {
		factory := opts.SessionFactory
		if factory == nil {
			factory = &SimFactory{}
		}
		if opts.LeaseStore != nil {
			factory = &browser.LeaseFactory{
				Factory: factory, Store: opts.LeaseStore,
				HolderID: opts.LeaseHolderID, TTL: opts.LeaseTTL,
			}
		}
		opts.Pool = browser.NewPool(opts.MaxTargets, factory)
	}
	if opts.Knowledge == nil {
		art := DefaultArtifact()
		opts.Knowledge = knowledge.NewStore(&memoryFetcher{art: art}, art)
	}
	if opts.Matcher == nil {
		m, err := matcherFromArtifact(opts.Knowledge.ActiveArtifact())
		if err != nil {
			return nil, fmt.Errorf("engine: matcher from knowledge: %w", err)
		}
		opts.Matcher = m
	}
	if opts.Classifier == nil {
		opts.Classifier = classifier.New(mockScorer{})
	}
	auth, err := NewAuthenticator(opts.Keys)
	if err != nil {
		return nil, err
	}
	srv := &Server{
		pool:              opts.Pool,
		viewerHub:         opts.ViewerHub,
		viewers:           make(map[string]*viewerhub.Session),
		viewerStop:        make(map[string]context.CancelFunc),
		viewerSessions:    make(map[string]browser.CDPSession),
		kb:                opts.Knowledge,
		matcher:           opts.Matcher,
		clf:               opts.Classifier,
		auth:              auth,
		onnxLib:           opts.OnnxRuntimeLib,
		shadowModelPath:   opts.ShadowOnnxModelPath,
		shadowVersionSeed: opts.ShadowOnnxModelVersion,
		vlmFb:             opts.VLM,
		sessions:          opts.Sessions,
		contexts:          make(map[string]*browser.BrowserContext),
		cache:             make(map[string]cachedResult),
		inflight:          make(map[string]struct{}),
		indeterminate:     make(map[string]time.Time),
		pages:             make(map[string]*pageVerifier),
		astarBlacklist:    matcher.NewPhysicalBlacklist(),
		vaultResolve:      opts.VaultResolve,
	}
	if srv.vaultResolve == nil {
		srv.vaultResolve = defaultVaultResolve
	}
	// 真实 ONNX（A 案）：启动时按内置/初始 KB artifact 构建 scorer；
	// 失败仅降级 mock（日志可见），不阻塞引擎启动（零配置模式无模型）。
	if opts.OnnxRuntimeLib != "" {
		srv.syncScorerWithKnowledge()
	}
	// 只读影子通道（§5.0.14 条款 3）：独立入口、独立字段；失败只降级
	// （shadow_inference 保持 null），不阻塞引擎启动、不影响决策链路。
	srv.syncShadowScorer()
	if opts.LogPath != "" {
		ls, err := NewLogStore(opts.LogPath)
		if err != nil {
			return nil, fmt.Errorf("engine: open online_logs(%s): %w", opts.LogPath, err)
		}
		srv.logs = ls
	}
	if opts.EpisodeDir != "" {
		// Episode 采集（§5.0.12，T-22）：目录不可写只降级为「不采集」并大声报错，
		// 不让只读旁路把引擎起不来。
		ew, err := OpenEpisodeWriter(opts.EpisodeDir)
		if err != nil {
			log.Printf("警告: Episode 采集目录不可用（%v）；本次运行不采集 Episode", err)
		} else {
			srv.episodes = ew
			log.Printf("Episode 采集已启用: %s（append-only JSONL，本机留档，不入交付仓）", ew.Path())
		}
	}
	if srv.viewerHub != nil {
		// 反向边（2026-09-12）：观看壳的展开/收回意图回到这里，由引擎把这个会话
		// 自己的真窗口搬到展开几何（或搬回原位）。意图永远不带页面输入。
		srv.viewerHub.SetIntentHandler(srv.handleViewerIntent)
	}
	return srv, nil
}

// Auth 暴露鉴权器（main/测试注册拦截器用）。
func (s *Server) Auth() *Authenticator { return s.auth }

// LogStore 暴露内部日志存储（供调试端点或测试使用）。
func (s *Server) LogStore() *LogStore { return s.logs }

// Close 释放资源。
func (s *Server) Close() {
	s.closeViewers()
	if s.matcher != nil {
		s.matcher.Close()
	}
	s.scorerMu.Lock()
	sc := s.onnxScorer
	s.onnxScorer = nil
	s.scorerMu.Unlock()
	if sc != nil {
		_ = sc.Close()
	}
	if s.logs != nil {
		_ = s.logs.Close()
	}
	if s.episodes != nil {
		_ = s.episodes.Close()
	}
	if s.debugSrv != nil {
		_ = s.debugSrv.Close()
	}
}
