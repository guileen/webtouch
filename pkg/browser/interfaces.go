// 会话扩展能力接口：CDP 会话（真实/模拟）可选的物理执行、异步验证与候选通道。
// 引擎通过类型断言消费；缺失时降级（probe-only / 语义直查）。
package browser

import (
	"context"

	"uws/pkg/browser/input"
	"uws/pkg/browser/modelbridge"
	"uws/pkg/challenge"
	"uws/pkg/contract"
)

// 物理动作被调用方 ctx 中断时的观测串：引擎据此区分「可以安全重放」与
// 「可能已经落地、结果未知」——取消不能撤销已经发给 Chrome 的点击或导航，
// 所以这两件事必须能被上层分开处理（2026-09-23 审查结论）。
const (
	// ObservedCanceledBeforeDispatch：定位/等待阶段就被取消，没有任何物理事件派发。
	// 上层可以安全地用同一个 intent 重试（幂等去重状态可以立刻释放）。
	ObservedCanceledBeforeDispatch = "canceled_before_dispatch"
	// ObservedCanceledAfterDispatch：鼠标/键盘/导航事件已经派发，页面结果未知。
	// 上层不得盲目重放（可能把一次点击变成两次），应保留去重状态并让调用方核对页面。
	ObservedCanceledAfterDispatch = "canceled_after_dispatch"
)

// IsCanceledObserved 报告 observed 是否为中断语义，以及在中断前是否已派发物理事件。
func IsCanceledObserved(observed string) (canceled bool, dispatched bool) {
	switch observed {
	case ObservedCanceledBeforeDispatch:
		return true, false
	case ObservedCanceledAfterDispatch:
		return true, true
	default:
		return false, false
	}
}

// ActionSession 是物理执行层的扩展能力：CDP 会话可选的「执行原子动作」接口。
// 真实 CDP 适配实现后同样实现本接口；缺失时引擎以 probe-only 模式降级。
//
// ctx 是**调用方生命周期**的载体（gRPC 请求 ctx → 定位/等待/物理派发同一 deadline）：
// 取消会让物理层停下并回报 canceled_before/after_dispatch，而不是在
// Background 上继续跑满自己的预算（2026-09-23 审查结论）。
type ActionSession interface {
	ExecuteAction(ctx context.Context, action, value, semantic string) (ok bool, observed string)
}

// SelectorActionSession 是物理层「按知识库规则选择器定位」的扩展能力
// （真实 CDP 会话实现；模拟会话无需实现，引擎自动降级为纯语义直查）。
// selector 来自知识库规则（uws_rules.selector）：引擎 executePhysical 解析
// 命中的规则后传入，CDP 侧优先 querySelectorAll(selector)，找不到再回退语义评分。
type SelectorActionSession interface {
	ActionSession
	ExecuteActionWithSelector(ctx context.Context, action, value, semantic, selector string) (ok bool, observed string)
}

// NodeActionSession 是物理层「按探针 node_id 确定性引用执行」的扩展能力
// （2026-08-27 混合指挥，v1.5.5）：node_id 为探针 BFS 输入序编号
// （GLOBAL §5 #57），执行层按同一 BFS 序（仅元素节点，children 遍历）回查
// 定位器，跳过语义/选择器派发。VLM 指挥官返回的 node_id 经存在性校验后
// 由协调器编译为 ActionIntentRequest.node_id 触达本原语。缺失时引擎回退
// 语义直查（行为零变化）。
type NodeActionSession interface {
	ExecuteActionByNode(ctx context.Context, action, value string, nodeID int32) (ok bool, observed string)
}

// PageTreeExporter 是 CDP 会话可选的「导出极简 AXTree」能力
// （2026-08-27 混合指挥，v1.5.5）：ExportToVLMFormat 的页内运行时出口，
// 返回 `[node_id] tag(role) "text" [offscreen]` 多行文本，供协调器
// commander / uws run --plan 外发 VLM 指挥官。缺失时引擎返回 UNIMPLEMENTED。
type PageTreeExporter interface {
	ExportPageTree(ctx context.Context) (tree string, err error)
}

// DirectScriptEvaluator 自 2026-09-23 起定义下沉至 pkg/contract（应用层与算法层需要引用它，
// 但不应因此依赖 pkg/browser）。此处保留别名，既有实现与调用点无需改动。
type DirectScriptEvaluator = contract.DirectScriptEvaluator

// VerifierEvents 是 CDP 会话可选的异步验证事件源（TDD-ENG-06/07）。
// 真实 CDP 适配器从 History.navigated / Network events / MutationObserver 喂入；
// 模拟会话由测试脚本化（SetNavEvents），每次 NavigationState 读取后事件位清零。
type VerifierEvents interface {
	NavigationState() (pushState bool, activeConns int, domMutated bool)
}

// WindowEvents 是 CDP 会话可选的新窗口事件源（TDD-ENG-13）。
// 物理点击 target="_blank" / window.open 后报告新 Target ID；
// 引擎据此返回 ContextState_NEW_WINDOW_OPENED，且原 Target 保持可用。
// 读取为一次性语义（读后清零），与 VerifierEvents.NavigationState 一致。
type WindowEvents interface {
	NewWindowOpened() (targetID string, ok bool)
}

// TargetSwitcher 是 CDP 会话可选的目标切换能力（TDD-ENG-13 续：新窗口接管，
// 2026-09-22 用户现场反馈批次 P0-1）。
//
// 语义：把会话「当前作用的 Target」切到同一 Chrome 内的另一个页面（弹窗授权页、
// 新标签页）。切换后该会话的动作/探针/截图全部落在新页，原 Target 仍存活（用户
// 可继续看见它），因此可以切回。
//
// 硬约束（R1 Focus Invariant）：实现方**不得**调用 Target.activateTarget /
// Page.bringToFront，切换只做 CDP attach + 事件重挂 + 身份重放，绝不抬升窗口或
// 抢占用户前台焦点。返回 previous 为切换前的 Target ID（未发生切换时 = 原值）。
type TargetSwitcher interface {
	SwitchTarget(ctx context.Context, targetID string) (previous string, err error)
	// ActiveTargetID 返回当前实际作用的 Target ID（未切换时等于 TargetDebugInfo.TargetID）。
	ActiveTargetID() string
}

// PageCandidate 是「页面→候选」通道的候选节点（模拟 WASM 探针 AXTree 输出；真实 CDP 会话由 DOM 特征采集填充）。
type PageCandidate struct {
	Text       string
	Confidence float64 // >0 时覆盖匹配置信度（探针/特征打分模拟）；0 = 规则匹配
	SimHash    uint64  // 0 = SimHash64(Text)
	StructHash uint64  // ★Tier 1.5 接线：阻尼 DOM 拓扑哈希（matcher.StructuralSimHash64）
	Tag        string  // ★结构身份（供 L3 Malhotra 兼容门禁）
	Role       string  // ★结构身份
	Flow       bool    // 流程标志（step/checkout）：A* 深度补偿 +2
	HTTPStatus int     // 0/2xx 正常；4xx/5xx 物理湮灭（A* 惩罚 +∞ 入黑名单）
	Children   []PageCandidate
	X          float64 // 物理坐标与几何尺寸（§5.0.18）
	Y          float64
	W          float64
	H          float64
	DocumentX  float64 // 文档坐标（CSS px），供视口外候选截图；X/Y 保持当前视口坐标
	DocumentY  float64
	Selector   string // 精确 CSS 选择器（§5.0.19 裁块复选）
	Landmark   string // 所属地标（§5.0.17/§5.0.18）
	Context    string // 上下文回响文本
}

// CandidateSource 是 CDP 会话可选的页面候选通道（引擎 runAction 消费；缺失时回退到语义直查 MatchQuery）。
type CandidateSource interface {
	PageCandidates() []PageCandidate
}

// PageSnapshotNode 是页面侧只读快照里的一个交互节点
// （spec/algorithm-kernel.md §5.0.13 条款 2/3：Episode 的 candidates[] 来源）。
// Selector 是页内 CSS 路径，引擎侧**只落哈希**、不落原文（条款 4⑥）。
type PageSnapshotNode struct {
	Tag      string
	Role     string
	Selector string
	X        float64
	Y        float64
	W        float64
	H        float64
	// Text 已按条款 4② 处理：input/textarea 的值不进文本（无标签源时落 `[VAL_MASKED]`）。
	Text string
	// Neighbors 是本节点**自身**的 1 阶骨架（§5.0.13 条款 7：**全部候选对称挂载**——
	// 消除「仅 GT 挂骨架 ⇒ 骨架退化为标签伪特征」）。协议同条款 3④：只留 `tag` ＋ `role`、
	// 顺序 parent → children → siblings、每候选上限 8 条（超出按文档序截断）。
	Neighbors []PageSnapshotNeighbor
	// NeighborsTruncated 表示本节点 1 阶邻居因超上限被截断（条款 7③）。
	NeighborsTruncated bool
	// Landmark 是节点所属地标区域（§5.0.17 条款 1：main / nav / header / footer / aside / other）。
	Landmark string
}

// PageHeading 是页面内的章节标题结构（§5.0.17 条款 2：空间路标与标题树索引）。
type PageHeading struct {
	Text  string  `json:"text"`
	Level int     `json:"level"`
	Tag   string  `json:"tag"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

// PageSnapshotNeighbor 是目标的 1 阶骨架节点（条款 3④：只留 tag + role，class 一律丢弃）。
type PageSnapshotNeighbor struct {
	Rel  string // parent / child / sibling
	Tag  string
	Role string
	Text string
}

// PageSnapshot 是一次页面侧只读快照的结果（§5.0.14 条款 1：派发前采样）。
// Truncated 表示候选全集超过 K=64 且已按「距枢轴矩形中心欧氏距离最近」截断（条款 3③）；
// 无 pivot 时退化为文档序并恒置 true（§5.0.14 条款 1⑤）。
type PageSnapshot struct {
	Nodes     []PageSnapshotNode
	Truncated bool
	// Skeleton 是 **GT 候选** 1 阶骨架的别名（§5.0.13 条款 7④ 兼容口径）：由 cdpSession
	// 从 GroundTruthIndex 所指节点的 Neighbors 回填；无 GT（-1）时为空。
	Skeleton []PageSnapshotNeighbor
	// GroundTruthIndex 是页内三级判据（§5.0.14 条款 2）判出的 ground truth 候选下标，
	// 恒 ≤1 条；PageSnapshotGroundTruthUnknown（-1）表示无法判定。
	// 调用方**必须**如实落全 false，不得回填陈旧命中或猜测。
	GroundTruthIndex int
	// Headings 是页面提取的可见标题树索引（§5.0.17 条款 2）。
	Headings []PageHeading
}

// PageSnapshotGroundTruthUnknown 是「页内三级判据无法判定 GT」的哨兵值（§5.0.14 条款 2）：
// pivot 缺失 / 快照为空 / 判据三级皆不成立时为该值。
const PageSnapshotGroundTruthUnknown = -1

// PageSnapshotPivot 是派发前采样的**枢轴**：决策目标的派发前只读身份（§5.0.14 决议 0/1）。
//
// **受体派发点（唯一正解）**：HasPoint 为 true 时，页内以 (PointX, PointY) 走
// elementFromPoint 取受体元素作为命中代理；该点由物理层在「受体已解析、物理事件未发出」
// 时经 PreDispatchReceptorSink 回传（决议 0③，硬上界是 disp.mark()）。
// **补充身份（兜底）**：无受体派发点时才退化为 Selector（KB 规则选择器）→「Tag ＋ Text
// 且页内唯一」；两者皆不成立 ⇒ 无 pivot（候选退化文档序、GT 一律无法判定）。
// 所有字段都只作页内定位用，**不落盘**（落盘只留身份哈希，见 §5.0.13 条款 4⑥）。
type PageSnapshotPivot struct {
	// HasPoint 表示本枢轴携带物理受体派发点（决议 0 (c)）；false 时只能用补充身份。
	HasPoint bool
	PointX   float64
	PointY   float64
	Selector string
	Tag      string
	Text     string
	// TargetSemantic 是意图语义目标（spec/algorithm-kernel.md §5.0.16）；
	// 在无物理受体派发点与补充身份时，用于 CDP 原生检索粗锚点自举。
	TargetSemantic string
	// HasCoarsePoint 表示携带了 CDP 原生检索自举出的粗锚点（§5.0.16 条款 3）。
	// 粗锚点用于恢复欧氏距离排序（局部性自举），严禁参与 GT 判定。
	HasCoarsePoint bool
	CoarseX        float64
	CoarseY        float64
}

// PreDispatchReceptor 是物理层在派发前解析出的**受体**（§5.0.14 决议 0）：
// 「定位已完成、物理事件尚未发出」这一时刻的目标元素只读身份。
//
// 它是 is_ground_truth 的唯一合法锚点来源——不得由候选池、KB 语义文本或引擎侧推断提供。
// 该结构只承载定位事实，**不落盘**（落盘只留哈希）。
type PreDispatchReceptor struct {
	Found bool
	X     float64 // 受体命中点（物理派发坐标）
	Y     float64
	Sel   string // 页内 CSS 路径（只作补充身份，落盘前须哈希）
	Tag   string
	Text  string
}

// PreDispatchReceptorSink 是会话可选的「派发前受体回传」能力（§5.0.14 决议 0③/决议 6）。
//
// **语义**：实现必须在「受体已解析、物理事件发出之前」——即 disp.mark() **之前**——
// 以受体参数调用一次已注册的回调；回调是纯参数、只读、fail-soft（实现不得因回调
// 返回值或 panic 改变决策与派发）。未注册（nil）时实现不产生任何额外开销。
// **相反语义的参照**：PointerObserver 是「派发已成功之后」的通知，本接口严格早于它。
type PreDispatchReceptorSink interface {
	SetPreDispatchReceptorSink(func(PreDispatchReceptor))
}

// PageSnapshotSource 是会话可选的「页面侧候选只读快照」通道（§5.0.13 条款 3）。
// 只读：实现不得发出任何 Input.* 域命令，也不得改动 DOM、不得触发导航。
// **采样时点**：§5.0.14 条款 1 要求该调用发生在物理派发之前（决策目标已定、事件未发）。
// 第二个返回值为 false 表示本会话/本页取不到快照（调用方须退回既有路径并如实标记来源）。
type PageSnapshotSource interface {
	PageSnapshot(pivot PageSnapshotPivot) (PageSnapshot, bool)
}

// ProbeStats 是 WASM 探针一次采集的截断/溢出状态快照
// （spec/algorithm-kernel.md §5：溢出触发 uws_wasm_truncation_total + JS 慢速遍历降级）。
type ProbeStats struct {
	Kept          int
	Truncated     bool
	Overflow      int
	NeedsFallback bool
	// Degradation 是感知降级的结构化代号（spec/algorithm-kernel.md §5.0.8 条款 3）：
	// 当前只有 `csp_wasm_blocked`（严格 CSP 站点页内 WASM 探针不可实例化）。
	// 非空即代表本次采集走的是降级通道、ref/AX 证据不可用——上层不得把该态下的
	// 无证据结论当作已验证。
	Degradation string
}

// ProbeStatsSource 是 CDP 会话可选的探针状态通道（真实 WASM 注入路径；
// 模拟会话由测试脚本化）。引擎在消费页面候选后读取：截断即累计
// wasmTruncTotal，并在 decision_trace 标记降级。
type ProbeStatsSource interface {
	ProbeStats() ProbeStats
}

// Screenshotter 是 CDP 会话可选的页面截图能力（v1.5.1 CaptureScreenshot RPC 数据源；
// VLM 视觉打码求解输入，TDD-COORD-04）。
type Screenshotter interface {
	CaptureScreenshot(ctx context.Context) (png []byte, width, height int, err error)
}

// ScreenshotterRegion 是「针对性截图/缩放」能力（2026-08-27）：
// 按视口 CSS 坐标裁剪一个区域截图（CDP Page.captureScreenshot clip）。
// 目标：VLM 兜底先全页粗定位 → 裁剪目标区域 → 放大精修坐标，
// 避免整页等比缩小丢失小字细节（图片进模型前会被缩到 ~800×800 等效）。
type ScreenshotterRegion interface {
	CaptureScreenshotRegion(ctx context.Context, x, y, w, h int) (png []byte, width, height int, err error)
}

// LongPageScreenshotter 为长页面的只读宏观切片路由提供文档尺寸与视口外裁图。
// 坐标均为 CSS px；CaptureDocumentRegion 不滚动页面，调用方仅在切片被选中后
// 才可通过 ScrollDocumentTo 将目标放入实际可见视口执行。
type LongPageScreenshotter interface {
	DocumentMetrics(ctx context.Context) (viewportW, viewportH, documentW, documentH int, err error)
	CaptureDocumentRegion(ctx context.Context, x, y, w, h int) (png []byte, err error)
	ScrollDocumentTo(ctx context.Context, y int) error
}

// ViewportSetter 是 CDP 会话设置设备视口与缩放比例的能力（供 QA Studio 等多视口验收管线使用）。
type ViewportSetter interface {
	SetViewport(ctx context.Context, width, height int, scale float64, mobile bool) error
}

// TargetDebugInfo 暴露真实 CDP targetId 与页面级调试端点（2026-08-27 前置：
// InitializeContext 的 target_id / session_url 必须携带真实 CDP targetId，
// 不能返回 Pool 内部 ID ctx-N，否则误导调试与对拍）。由真实 cdpSession
// 实现；模拟会话无需实现（引擎回退到 Pool 内部 ID，行为零变化）。
type TargetDebugInfo interface {
	TargetID() string
	// DebugPageURL 返回页面级 CDP 调试端点（ws://host:port/devtools/page/<targetId>）；
	// 不可用时返回空串。
	DebugPageURL() string
}

// WindowReporter 由「知道自己跑在专属窗口还是共用标签里」的会话实现（真实
// cdpSession）。引擎持久化「会话名 → Target」绑定时用它记下窗口策略，续页时
// 按同样策略接管（v1.5.7 会话续页；模拟会话不实现，绑定按普通标签记）。
type WindowReporter interface {
	OwnWindow() bool
}

// VLMProbeSession 是 VLM 感知兜底的物理能力（2026-08-27）：
//   - PageURL 返回当前页面 URL（学习存储键）；
//   - ClickAt 按视口 CSS 坐标物理点击，并返回命中元素的 CSS 路径（供引擎自进化回写；
//     命中元素为 body/文档根时视为无目标）。
type VLMProbeSession interface {
	PageURL() string
	ClickAt(ctx context.Context, x, y int) (selector string, ok bool, observed string)
	// InputAt 在视口坐标处定位「就近输入框」并填入 value（2026-08-31 input 兜底）：
	// 坐标命中元素自身/祖先/后代中的 input/textarea/select 均可（Airwallex 描述
	// textarea 无 placeholder/aria-label，语义评分抓不到，必须按坐标就近定位）。
	// 返回输入框自身的 CSS 路径（供 learned 回写；下次同页面免 VLM）。
	// semantic 为意图语义，供 §5.0.9 条款 1 的意图-受体角色守卫在派发前比对；
	// 落盘后必须过 §5.0.9 条款 2 的回读校验，回读不过返回 `vlm_landing_unverified`。
	InputAt(ctx context.Context, x, y int, value, semantic string) (selector string, ok bool, observed string)
	// ExtractAt 在视口坐标处解析元素并提取可见文本 + CSS 路径（v1.5.5 自进化）：
	// 供引擎 learned 回写真实定位器（修复 extract 只写 NodeText 的假闭环）。
	// 返回 (selector, text, ok)；命中 body/文档根或不可提取时 ok=false。
	ExtractAt(ctx context.Context, x, y int) (selector string, text string, ok bool)
}

// HistoryNavigator 是 VLM 主控「back」原语（2026-08-27）：会话可选地触发浏览器后退，
// 回到上一页重新观察。真实 CDP 经 Runtime.evaluate("history.back()")；模拟会话由测试
// 脚本化。缺失时引擎把「back」动作降级为无解（不盲目继续）。
type HistoryNavigator interface {
	GoBack(ctx context.Context) error
}

// HitElement 是动作实际命中元素的证据快照（v1.5.4 判定层证据链：
// docs/evidence/research/uws-verdict-evidence-design-2026-08-27.md §6.1）。引擎把它回显到
// ActionResultResponse.hit_element，调用方据此区分「点中精确目标」vs
// 「点中容器/推荐位」（后者 + URL 不变 = 假阳性，如 drama-h5「第 4 集」）。
type HitElement struct {
	Selector string  // CSS 路径（如 div#app > section > button:nth-of-type(2)）
	Tag      string  // 元素标签（小写）
	X, Y     float64 // 视口命中点（CSS px）
	W, H     float64 // 元素尺寸（CSS px）
	Text     string  // 可见文本（截断 ≤120）
}

// HitElementSource 是 CDP 会话可选的「最近一次命中元素」通道（v1.5.4）：
// 语义/选择器/VLM 定位成功的动作后，引擎读取并回显到响应。真实 cdpSession
// 与 SimCDPSession 实现；缺失时引擎回退空证据（行为零变化）。
type HitElementSource interface {
	LastHitElement() (HitElement, bool)
}

// PageURLSource 是 CDP 会话可选的「当前页面 URL」通道（v1.5.4 证据链：计算 url_changed 导航判定）。
type PageURLSource interface {
	PageURL() string
}

// ChatStreamChunk 与 ChatStreamSnapshot 是聊天 SSE 流解析产物（modelbridge 兼容别名）。
type ChatStreamChunk = modelbridge.Chunk
type ChatStreamSnapshot = modelbridge.Snapshot

// ChatStreamSource 是 CDP 会话可选的聊天 SSE 捕获通道（Network 层被动捕获）。
type ChatStreamSource interface {
	ChatStreamSnapshot() ChatStreamSnapshot
	ChatStreamDrain() []ChatStreamChunk
}

// ChallengeProber 是 CDP 会话可选的人机验证感知能力（战役一：静态判准 + DOM/iframe 探针）。
type ChallengeProber interface {
	ProbeChallenge(ctx context.Context) (*challenge.ChallengeSignal, bool)
}

// HumanInputForwarder 是 CDP 会话可选的人类接管物理输入能力（战役二：Canvas 反向注入）。
type HumanInputForwarder interface {
	DispatchHumanInput(ctx context.Context, ev challenge.HumanInputEvent) error
}

// —— 输入域子包（`uws/pkg/browser/input`）的兼容别名（复杂度治理·阶段 2·单元 2a）——
// 旧符号在本包继续可用：`pkg/engine` 消费的结构化代号零改动（与 modelbridge 别名同处）。
const (
	FaultTargetTopologyRupture = input.FaultTargetTopologyRupture
	FaultTargetRoleMismatch    = input.FaultTargetRoleMismatch
	FaultVLMlandingUnverified  = input.FaultVLMlandingUnverified
	FaultInputOccluded         = input.FaultInputOccluded
	FaultTargetNotEditable     = input.FaultTargetNotEditable
	FaultWriteNotPersisted     = input.FaultWriteNotPersisted
	FaultLocateNoCandidate     = input.FaultLocateNoCandidate
)
