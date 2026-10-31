// 在线 Episode 采集与脱敏（spec/algorithm-kernel.md §5.0.12，Tasks.md `T-22` 第一批代码）。
//
// 定位：**只读旁路**。Episode 由 runAction 在既有判定之外顺带记录——不改判定分支、
// 不改返回值、写盘失败只进日志（在线行为零变化）。它是 ONNX 晋升与「调试即变强」
// 的共同前置：没有候选集合与平行候选状态，离线端无法重算 §5.0.1 五维特征，
// 也无法事后消除反事实偏差。
//
// 落盘：append-only JSONL（单条一行、同日一文件），本机 ~/.uws/episodes/ 下，
// **不入交付仓**（含页面文本，属本机取证材料）。
//
// 裁剪与脱敏口径（§5.0.12 条款 2/3）：
//   - 只留候选/目标元素的标签元数据（tag/role/struct_hash/哈希化 selector）与
//     ≤32 字符截断文本，并**同时**记原始 rune 长度与截断标记（log_len 依赖原始长度、
//     exact_match/substring 依赖完整文本；近似候选的重算值不得用于毕业判定）；
//   - 丢弃全局 DOM、非目标区域文本、页面正文、Cookie；
//   - 输入值**永不落盘**（只留 rune 长度与 `@vault:` 引用名）；
//   - URL 只留 scheme://host/path（查询串/片段常带 Token）；
//   - 文本字段落盘前过正则硬过滤（邮箱/电话/Token/UUID），命中换占位符并记 redacted；
//     未命中字段的字节内容不变。
package engine

import "time"

// EpisodeSchemaV1 是 Episode 落盘记录的 schema 标识（字段演进时递增）。
const EpisodeSchemaV1 = "episode.v1"

// episodeMaxTextRunes 是裁剪后文本的 rune 上限（§5.0.12 条款 2：≤32 字符）。
const episodeMaxTextRunes = 32

// candidates_source 取值（§5.0.13 条款 2④/⑤）。
const (
	// EpisodeCandidatesPageProjection：候选来自页面侧只读快照投影（第二批起的正常口径）。
	EpisodeCandidatesPageProjection = "page_projection"
	// EpisodeCandidatesAfterNav：快照取自导航后的新页面（已不是决策时的页面状态），
	// 如实标记来源，由消费端 fail-closed 隔离（§5.0.13 条款 5）。
	EpisodeCandidatesAfterNav = "page_projection_after_nav"
	// EpisodeCandidatesEnginePool：会话无只读快照能力时的第一批遗留口径（候选池残留）。
	EpisodeCandidatesEnginePool = "engine_pool"
)

// 派发状态口径（§5.0.11：派发标记时点与中断语义）。
const (
	EpisodeNotDispatched          = "not_dispatched"
	EpisodeDispatchedKnown        = "dispatched_outcome_known"
	EpisodeDispatchedUnknown      = "dispatched_outcome_unknown"
	EpisodeCanceledBeforeDispatch = "canceled_before_dispatch"
	EpisodeCanceledAfterDispatch  = "canceled_after_dispatch"
)

// ── 记录结构（§5.0.12 条款 1 的六个必备面）────────────────────────

// Episode 是一次「意图 → 候选集合 → 生产决策 → 派发状态 → 页面后验」的完整观测。
type Episode struct {
	Schema     string             `json:"schema"`
	EpisodeID  string             `json:"episode_id"`
	At         time.Time          `json:"at"`
	Intent     EpisodeIntent      `json:"intent"`
	Page       EpisodePage        `json:"page"`
	Candidates []EpisodeCandidate `json:"candidates"`
	// DecisionPool 是**决策时点**引擎实际评估的候选池与**决策序**（spec §5.0.15，`T-30`）。
	// 与 `candidates[]`（页面侧锚点投影）是**两份不同的事实**：前者按置信度降序选举序，
	// 后者按「距受体距离」锚点序；拿 `candidates[]` 的 Index 算召回会得虚假高命中。
	// 永不为 null（空池落空数组）；不参与消费端 Tier 录取（§5.0.15 条款 3）。
	DecisionPool []EpisodeDecisionCandidate `json:"decision_pool"`
	Production   EpisodeProduction          `json:"production"`
	Posterior    EpisodePosterior           `json:"posterior"`
	// ShadowInference 是同一候选集上的**只读**影子观察（§5.0.13 条款 1③ / 条款 2⑤）。
	// 影子通道未接线（无 ONNX 会话）时允许为 null；接线后 scores 与 candidates 基数一一对应。
	// 只读：影子分数不得参与 decision（条款 1）。
	ShadowInference *EpisodeShadow   `json:"shadow_inference"`
	Redaction       EpisodeRedaction `json:"redaction"`
}

// EpisodeIntent 是意图与目标语义（条款 1 ①）。输入值只留长度与引用名。
type EpisodeIntent struct {
	IntentID       string `json:"intent_id"`
	ExpectedAction string `json:"expected_action"`
	TargetSemantic string `json:"target_semantic"`
	NodeID         int32  `json:"node_id"`
	ContextName    string `json:"context_name"`
	HasValue       bool   `json:"has_value"`
	ValueRuneLen   int    `json:"value_rune_len"`
	VaultRef       string `json:"vault_ref"`
}

// EpisodePage 是采集时刻的页面位置（条款 1 ⑥）。查询串/片段一律丢弃。
type EpisodePage struct {
	URLBefore     string `json:"url_before"`
	URLAfter      string `json:"url_after"`
	URLChanged    bool   `json:"url_changed"`
	QueryStripped bool   `json:"query_stripped"`
	// URLHash 是去 query URL 的身份哈希（§5.0.13 条款 4⑥：只落哈希，不落原文）。
	URLHash string `json:"url_hash"`
}

// EpisodeCandidate 是候选集合的一员（条款 1 ② + 条款 2 的标签元数据）。
type EpisodeCandidate struct {
	Index         int           `json:"index"`
	Stage         string        `json:"stage"`
	Tag           string        `json:"tag"`
	Role          string        `json:"role"`
	StructHash    string        `json:"struct_hash"`
	SelectorHash  string        `json:"selector_hash"`
	Confidence    float64       `json:"confidence"`
	Text          string        `json:"text"`
	TextRuneLen   int           `json:"text_rune_len"`
	TextTruncated bool          `json:"text_truncated"`
	MatchedRules  []EpisodeRule `json:"matched_rules"`
	// §5.0.13 条款 2②/③ 追加字段（页面侧候选投影的口径；旧样本缺省为零值）。
	Interactive   bool            `json:"interactive"`
	X             float64         `json:"x"`
	Y             float64         `json:"y"`
	W             float64         `json:"w"`
	H             float64         `json:"h"`
	IsGroundTruth bool            `json:"is_ground_truth"`
	Skeleton      EpisodeSkeleton `json:"skeleton"`
}

// EpisodeRule 是命中规则的最小身份（不含规则文本原文，只留 id/类型/标签/状态）。
type EpisodeRule struct {
	RuleID        int64   `json:"rule_id"`
	RuleType      string  `json:"rule_type"`
	SemanticLabel string  `json:"semantic_label"`
	Status        string  `json:"status"`
	Confidence    float64 `json:"confidence"`
	SelectorHash  string  `json:"selector_hash"`
}

// EpisodeProduction 是生产决策与其派发状态（条款 1 ③）。
type EpisodeProduction struct {
	Decision      string          `json:"decision"`
	Selected      EpisodeSelected `json:"selected"`
	Confidence    float64         `json:"confidence"`
	CandCount     int             `json:"cand_count"`
	DispatchState string          `json:"dispatch_state"`
	ObservedCode  string          `json:"observed_code"`
	FailureCode   string          `json:"failure_code"`
	ExecOK        bool            `json:"exec_ok"`
	VLMAttempted  bool            `json:"vlm_attempted"`
	ElapsedMs     int64           `json:"elapsed_ms"`
	// §5.0.13 条款 2④/⑤：候选集来源与页面侧候选数（消费端 fail-closed 的第一手依据）。
	CandidatesSource    string `json:"candidates_source"`
	PageCandidateCount  int    `json:"page_candidate_count"`
	CandidatesTruncated bool   `json:"candidates_truncated"`
	// §5.0.15：决策池来源与如实计数。DecisionPoolSource = `matcher`（走了候选池匹配）
	// | `skipped`（drag / node_id 直连 / evaluate / 无匹配器——压根没评估候选池）。
	// 空池 + `matcher` 与空池 + `skipped` 是**两回事**，离线统计不得混算（同 engine_pool 教训）。
	DecisionPoolSource    string `json:"decision_pool_source"`
	DecisionPoolCount     int    `json:"decision_pool_count"`
	DecisionPoolTruncated bool   `json:"decision_pool_truncated"`
}

// EpisodeDecisionCandidate 是决策池里的一条候选（spec §5.0.15 条款 1）。
//
// 字段口径与 `EpisodeCandidate` 一致（文本同 `clip` 口径、选择器只落哈希），
// 差别在语义：`Rank` 是**决策序**位序，`IsSelected` 是本次实际派发目标（至多一条）。
// **不携带 `is_ground_truth`**：GT 的唯一定义留在页面侧 `candidates[]`（单一事实源）。
type EpisodeDecisionCandidate struct {
	Rank          int     `json:"rank"`
	Stage         string  `json:"stage"`
	Source        string  `json:"source"`
	Confidence    float64 `json:"confidence"`
	SelectorHash  string  `json:"selector_hash"`
	StructHash    string  `json:"struct_hash"`
	Tag           string  `json:"tag"`
	Role          string  `json:"role"`
	Text          string  `json:"text"`
	TextRuneLen   int     `json:"text_rune_len"`
	TextTruncated bool    `json:"text_truncated"`
	IsSelected    bool    `json:"is_selected"`
}

// EpisodeSelected 是生产实际作用的目标元素（物理执行层语义）。
type EpisodeSelected struct {
	Ref           string          `json:"ref"`
	Tag           string          `json:"tag"`
	Role          string          `json:"role"`
	SelectorHash  string          `json:"selector_hash"`
	Text          string          `json:"text"`
	TextRuneLen   int             `json:"text_rune_len"`
	TextTruncated bool            `json:"text_truncated"`
	Skeleton      EpisodeSkeleton `json:"skeleton"`
}

// EpisodeSkeleton 是目标元素的 a11y 骨架（条款 2 / §5.0.13 条款 3④）。
//
// 第二批起由**页面侧只读快照**注入直系 1 阶父/子/兄弟（Source=page_snapshot_1hop）；
// 会话无只读快照能力时退回候选池元数据（Source=candidate_pool）并如实为空。
type EpisodeSkeleton struct {
	Source    string            `json:"source"`
	Neighbors []EpisodeNeighbor `json:"neighbors"`
	// Truncated 表示该候选的 1 阶邻居**因超每候选上限（8 条）被截断**（§5.0.13 条款 7③）。
	Truncated bool `json:"truncated"`
}

// EpisodeNeighbor 是 1 阶邻居的标签元数据（§5.0.13 条款 3④：只留 tag + role）。
type EpisodeNeighbor struct {
	Rel         string `json:"rel"`
	Tag         string `json:"tag"`
	Role        string `json:"role"`
	Text        string `json:"text"`
	TextRuneLen int    `json:"text_rune_len"`
	// ClassHash 恒空：§5.0.13 条款 3④ 订正为「丢弃全部 class」（class 哈希对同构节点
	// 无判别力，且多一处泄漏面）。字段保留仅为旧样本兼容。
	ClassHash string `json:"class_hash"`
}

// EpisodePosterior 是页面后验（条款 1 ④）。失败与未派发时不得回填陈旧命中元素。
type EpisodePosterior struct {
	State           string `json:"state"`
	HitTag          string `json:"hit_tag"`
	HitSelectorHash string `json:"hit_selector_hash"`
	HitText         string `json:"hit_text"`
	HitTextRuneLen  int    `json:"hit_text_rune_len"`
	URLChanged      bool   `json:"url_changed"`
	ValueUpdated    bool   `json:"value_updated"`
	ReadbackSource  string `json:"readback_source"`
}

// EpisodeShadow 是同一候选集上的影子观察（条款 1 ⑤）。生产决策与影子观察同条配对。
type EpisodeShadow struct {
	Scores    []float64 `json:"scores"`
	Top1      int       `json:"top1"`
	ModelHash string    `json:"model_hash"`
	// ModelVersion 是影子模型版本串（§5.0.13 条款 1③：影子观察必须可溯源到模型版本）。
	ModelVersion string `json:"model_version"`
}

// EpisodeRedaction 记录本次脱敏硬过滤的命中面（条款 3）。
type EpisodeRedaction struct {
	Redacted bool     `json:"redacted"`
	Hits     int      `json:"hits"`
	Fields   []string `json:"fields"`
}
