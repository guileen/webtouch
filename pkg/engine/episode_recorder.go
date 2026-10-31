package engine

import (
	"log"
	"sort"
	"time"
	"unicode/utf8"
	"uws/pkg/browser"
	"uws/pkg/matcher"
	uwspb "uws/proto/gen/go/proto/v1"
)

// ── 记录器（与 runAction 的接线面）───────────────────────────────

// episodeRecorder 承载单次 runAction 的即时量，收尾时配对落盘为一条 Episode。
type episodeRecorder struct {
	srv *Server
	ep  *Episode

	// snap 是页面侧候选只读快照通道（§5.0.13 条款 3）；会话不支持时为 nil。
	snap browser.PageSnapshotSource

	dispatched   bool
	vlmAttempted bool
	// sampled 标记「派发前只读采样已尝试过」；pre 是采样产物（§5.0.14 条款 1①：采样时钟前移）。
	// 采样一旦成功，Episode 的 candidates[] / is_ground_truth / shadow_inference 全部取自它；
	// 采样失败或未走到采样点时 pre 为 nil ⇒ 退回候选池口径并如实标记来源。
	sampled bool
	pre     *preDispatchSample

	redactHits   int
	redactFields map[string]bool
}

// preDispatchSample 是「派发前只读采样」的产物（§5.0.14 条款 1）。
// 三者同点观测：候选集（派发前页面状态）＋ GT 锚定（页内三级判据）＋ 影子打分（只读旁路）。
type preDispatchSample struct {
	Candidates []EpisodeCandidate
	Shadow     *EpisodeShadow
	Source     string
	Count      int
	Truncated  bool
}

// beginEpisode 在 Episode 采集开启时创建记录器；未开启返回 nil（零开销）。
func (s *Server) beginEpisode(req *uwspb.ActionIntentRequest, bc *browser.BrowserContext,
	urlBefore, vaultRef string) *episodeRecorder {
	if s.episodes == nil || req == nil {
		return nil
	}
	rec := &episodeRecorder{
		srv:          s,
		redactFields: map[string]bool{},
		ep: &Episode{
			Schema:    EpisodeSchemaV1,
			EpisodeID: req.GetIntentId(),
			At:        time.Now().UTC(),
		},
	}
	rec.ep.Intent = EpisodeIntent{
		IntentID:       req.GetIntentId(),
		ExpectedAction: req.GetExpectedAction(),
		TargetSemantic: rec.text("intent.target_semantic", req.GetTargetSemantic()),
		NodeID:         req.GetNodeId(),
		ContextName:    rec.text("intent.context_name", req.GetContextHint()["context"]),
		HasValue:       req.GetValue() != "",
		ValueRuneLen:   utf8.RuneCountInString(req.GetValue()),
		VaultRef:       vaultRef,
	}
	sanitizedURL, stripped := episodePageURL(urlBefore)
	if stripped {
		sanitizedURL = rec.text("page.url_before", sanitizedURL)
	}
	rec.ep.Page.URLBefore = sanitizedURL
	rec.ep.Page.QueryStripped = stripped
	// §5.0.13 条款 4⑥：url_hash 只落哈希（去 query URL 的身份哈希）。
	rec.ep.Page.URLHash = episodeIdentityHash(sanitizedURL)
	if bc != nil {
		if src, ok := bc.Session().(browser.PageSnapshotSource); ok {
			rec.snap = src
		}
	}
	return rec
}

// text 对文本字段执行脱敏并登记命中面（条款 3）。
func (r *episodeRecorder) text(field, raw string) string {
	if raw == "" {
		return ""
	}
	out, hit := sanitizeEpisodeText(raw)
	if hit {
		r.redactHits++
		r.redactFields[field] = true
	}
	return out
}

// clip 脱敏 + 截断，返回（截断文本, 原始 rune 长度, 截断标记）。
func (r *episodeRecorder) clip(field, raw string) (string, int, bool) {
	clean := r.text(field, raw)
	n := utf8.RuneCountInString(clean)
	out, truncated := truncateEpisodeRunes(clean, episodeMaxTextRunes)
	return out, n, truncated
}

// markDispatched 记录「物理派发已进入执行层」（§5.0.11 派发标记时点）。
func (r *episodeRecorder) markDispatched() {
	if r != nil {
		r.dispatched = true
	}
}

// markVLM 记录本次走过多模态兜底通道。
func (r *episodeRecorder) markVLM() {
	if r != nil {
		r.vlmAttempted = true
	}
}

// §5.0.14 决议 9（候选空集轻量重试）的上界：≤2 次 × 400ms 退避。
// 变量形式仅供单测缩短退避，生产恒为缺省值。
var (
	episodeEmptyCandidateRetries = 2
	episodeEmptyCandidateBackoff = 400 * time.Millisecond
)

// capturePreDispatch 在**物理派发之前**做一次只读采样（§5.0.14 条款 1① / 决议 6）。
//
// 时序：决策目标已定 → 本函数（Phase A 解析 pivot → Phase B 页内只读快照 ＋ GT 锚定 ＋
// 影子打分）→ 物理派发。只读：不发 Input.* 命令、不改 DOM、不触发导航。采样异常只影响
// Episode（fail-soft），绝不影响决策与派发。幂等：同一 Episode 只采样一次。
//
// receptor 是**物理层在派发前回传的受体**（§5.0.14 决议 0，可由 browser.PreDispatchReceptorSink
// 在 disp.mark() 之前回调进来）；非 nil 且 Found 时它是 GT 的唯一合法锚点。无该能力的会话
// 与直发通道传 nil ⇒ 退回补充身份（KB 选择器 → Tag ＋ Text 页内唯一）口径，行为不回退。
func (r *episodeRecorder) capturePreDispatch(cands []matcher.Candidate, execSemantic string,
	receptor *browser.PreDispatchReceptor) {
	if r == nil || r.snap == nil || r.sampled {
		return
	}
	r.sampled = true
	pivot := episodeSnapshotPivot(cands, execSemantic, receptor)
	pivot.TargetSemantic = execSemantic
	snap, ok := r.snap.PageSnapshot(pivot)
	// §5.0.14 决议 9（`INT-218②`）：页内快照**成功但候选为 0** 时，允许在同一采样槽内轻量等待重试
	// （≤2 次 × 400ms 退避），给晚水合的 SPA 一次机会；仍为空则**如实落空集**（消费端 fail-closed）。
	// 硬边界：不改变采样时点（仍严格在派发前）与唯一性（幂等槽不被抢占）、不触碰决策与影子。
	// 快照**失败不重试**（`ok=false` 与「无能力」同号，决议 5；对无能力会话重试纯属浪费预算）。
	for retry := 0; ok && len(snap.Nodes) == 0 && retry < episodeEmptyCandidateRetries; retry++ {
		time.Sleep(episodeEmptyCandidateBackoff)
		snap, ok = r.snap.PageSnapshot(pivot)
	}
	if !ok {
		// 取不到快照（会话无只读能力 / 页内脚本失败）：pre 保持 nil，退回候选池口径。
		return
	}
	pre := &preDispatchSample{
		Source:    EpisodeCandidatesPageProjection,
		Count:     len(snap.Nodes),
		Truncated: snap.Truncated,
	}
	for i, n := range snap.Nodes {
		text, runes, truncated := r.clip("candidates[].text", n.Text)
		cand := EpisodeCandidate{
			Index:         i,
			Stage:         "page_projection",
			Tag:           n.Tag,
			Role:          n.Role,
			SelectorHash:  episodeIdentityHash(n.Selector),
			Interactive:   true,
			X:             n.X,
			Y:             n.Y,
			W:             n.W,
			H:             n.H,
			Text:          text,
			TextRuneLen:   runes,
			TextTruncated: truncated,
		}
		// §5.0.13 条款 7：1 阶骨架**对称挂载**到**每一个**候选（含非 GT 负样本），消除
		// 「仅 GT 挂骨架 ⇒ 骨架成伪特征」（CON-214 硬线；`INT-218①`）。空邻居不落占位。
		if sk := r.skeletonFromNeighbors(n.Neighbors, n.NeighborsTruncated); len(sk.Neighbors) > 0 {
			cand.Skeleton = sk
		}
		pre.Candidates = append(pre.Candidates, cand)
	}
	// §5.0.14 条款 2：GT 由页内三级判据判定并回带下标，恒 ≤1 条；无法判定（-1 / 越界）
	// 时保持全 false——引擎侧绝不编造、绝不回填陈旧命中。
	if idx := snap.GroundTruthIndex; idx >= 0 && idx < len(pre.Candidates) {
		pre.Candidates[idx].IsGroundTruth = true
		// 兼容（条款 7④）：候选自身无邻居时退回快照顶层骨架（＝GT 候选 neighbors 的别名）。
		if len(pre.Candidates[idx].Skeleton.Neighbors) == 0 {
			if sk := r.skeletonFromSnapshot(snap); len(sk.Neighbors) > 0 {
				pre.Candidates[idx].Skeleton = sk
			}
		}
	}
	// §5.0.14 条款 3①：影子只读旁路与生产决策同点观测，结果只落 shadow_inference。
	pre.Shadow = r.srv.shadowInference(pre.Candidates, execSemantic)
	r.pre = pre
}

// episodeSnapshotPivot 解析派发前采样的枢轴（§5.0.14 决议 0/1/3）：
// **物理受体派发点优先**（receptor，由物理层在派发前回传）——它是 GT 的唯一合法锚点；
// 补充身份（KB 规则选择器 → Tag ＋ Text）只在无受体点时被页内脚本启用，二者并存时窗口
// 由页内脚本按「有受体即用受体」取舍。目标既不在候选池内、又无受体时返回空枢轴——页内
// 随即无法判定 GT（全 false），绝不会拿别的候选冒充决策目标。
func episodeSnapshotPivot(cands []matcher.Candidate, execSemantic string,
	receptor *browser.PreDispatchReceptor) browser.PageSnapshotPivot {
	pivot := browser.PageSnapshotPivot{}
	if c := episodeCandidateOf(cands, execSemantic); c != nil {
		pivot.Selector = bestSelector(*c)
		pivot.Tag = c.Tag
		pivot.Text = c.NodeText
	}
	if receptor != nil && receptor.Found {
		pivot.HasPoint = true
		pivot.PointX = receptor.X
		pivot.PointY = receptor.Y
	}
	return pivot
}

// episodeOutcome 是 runAction 收尾时一次性交出的观测（全部来自既有局部量）。
type episodeOutcome struct {
	Resp         *uwspb.ActionResultResponse
	Err          error
	Decision     string
	ExecSemantic string
	Candidates   []matcher.Candidate
	Confidence   float64
	CandCount    int
	Observed     string
	// PoolEvaluated 表示本次动作**是否走过候选池匹配**（§5.0.15 条款 2）。
	// false（drag / node_id 直连 / evaluate / 无匹配器）时决策池为空是「未评估」，
	// 与「评估后无候选保留」是两回事，落盘必须区分。
	PoolEvaluated bool
}

// finish 补齐候选集合、生产决策、页面后验与脱敏统计后落盘。
// 任何失败只记日志：Episode 采集不得影响在线动作。
func (r *episodeRecorder) finish(o episodeOutcome) {
	if r == nil || r.srv == nil || r.srv.episodes == nil {
		return
	}
	ep := r.ep

	// §5.0.13 条款 2①：candidates[] 必须来自页面侧候选集投影，永不为 null。
	projSource, pageCount, pageTruncated := r.projectCandidates(ep, o)
	// §5.0.15：决策池与决策序落盘（纯增量观测，与上一步的页面侧投影互不污染）。
	poolSource, poolCount, poolTruncated := r.recordDecisionPool(ep, o)

	selected := episodeSelectedOf(o.Candidates, o.ExecSemantic)
	sel := EpisodeSelected{
		Ref:          selected.Ref,
		Tag:          selected.Tag,
		Role:         selected.Role,
		SelectorHash: selected.SelectorHash,
		Skeleton:     EpisodeSkeleton{Source: "candidate_pool"},
	}
	// §5.0.13 条款 3④：命中的那一条候选的 1 阶骨架即生产目标的骨架。
	if gt := episodeGroundTruthCandidate(ep.Candidates); gt != nil && len(gt.Skeleton.Neighbors) > 0 {
		sel.Skeleton = gt.Skeleton
	}
	if selected.Ref != "" {
		text, runes, truncated := r.clip("production.selected.text", selected.RawText)
		sel.Text, sel.TextRuneLen, sel.TextTruncated = text, runes, truncated
	}

	code := episodeFailureCode(o.Observed)
	ep.Production = EpisodeProduction{
		Decision:      o.Decision,
		Selected:      sel,
		Confidence:    o.Confidence,
		CandCount:     o.CandCount,
		DispatchState: r.dispatchState(o, code),
		ObservedCode:  r.text("production.observed_code", o.Observed),
		FailureCode:   code,
		ExecOK:        o.Resp != nil && o.Resp.GetSuccess(),
		VLMAttempted:  r.vlmAttempted,
		ElapsedMs:     r.elapsedMs(),

		CandidatesSource:    projSource,
		PageCandidateCount:  pageCount,
		CandidatesTruncated: pageTruncated,

		DecisionPoolSource:    poolSource,
		DecisionPoolCount:     poolCount,
		DecisionPoolTruncated: poolTruncated,
	}

	// §5.0.14 条款 1①/3①：影子观察与生产决策同条配对，且必须取自**派发前**候选集。
	// 采样成功时直接沿用同点读数；未采样（退回候选池口径）时按既有口径重算（未接线为 null）。
	if r.pre != nil {
		ep.ShadowInference = r.pre.Shadow
	} else {
		ep.ShadowInference = r.srv.shadowInference(ep.Candidates, o.ExecSemantic)
	}

	ep.Posterior = r.posterior(o)
	if urlAfter := episodeRespURL(o.Resp); urlAfter != "" {
		clean, _ := episodePageURL(urlAfter)
		ep.Page.URLAfter = r.text("page.url_after", clean)
		ep.Page.URLChanged = ep.Page.URLAfter != ep.Page.URLBefore
	}
	if o.Resp != nil && o.Resp.GetUrlChanged() {
		ep.Page.URLChanged = true
	}

	fields := make([]string, 0, len(r.redactFields))
	for f := range r.redactFields {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	ep.Redaction = EpisodeRedaction{Redacted: r.redactHits > 0, Hits: r.redactHits, Fields: fields}

	if err := r.srv.episodes.Append(ep); err != nil {
		log.Printf("episode: 落盘失败 intent=%q: %v", ep.EpisodeID, err)
	}
}

// decisionPoolMax 是决策池落盘上限（spec §5.0.15 条款 2：与页面侧投影 K 一致，便于对拍）。
const decisionPoolMax = 64

// recordDecisionPool 落盘**决策时点**的候选池与**决策序**（spec §5.0.15，`T-30` 数据前置）。
//
// 决策序 ＝ 按 confidence 降序稳定排序（与 matcher.RefineByStructureDual / refineTextChannel
// 用于 top-K 选举的 order 同源同比较器），同位序保持池内输入序（页面候选预序遍历序 ∪ 语义直查序）。
// 注意：candidatesFor 的返回序是 append(kept, l2...)，**不是**决策序；本函数的排序结果才是。
//
// 返回（来源, 落盘条数, 是否被上限截断）。fail-soft：纯观测，绝不影响决策。
func (r *episodeRecorder) recordDecisionPool(ep *Episode, o episodeOutcome) (string, int, bool) {
	if !o.PoolEvaluated {
		// 压根没评估候选池：空数组 + 来源如实标 skipped（不得与「评估后为空」混算）。
		ep.DecisionPool = []EpisodeDecisionCandidate{}
		return "skipped", 0, false
	}
	ordered := make([]matcher.Candidate, len(o.Candidates))
	copy(ordered, o.Candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Confidence > ordered[j].Confidence
	})
	truncated := len(ordered) > decisionPoolMax
	if truncated {
		ordered = ordered[:decisionPoolMax]
	}
	// 与 EpisodeCandidate 同口径：直接重排下标、不按文本去重（§5.0.13 条款 3⑥）。
	pool := make([]EpisodeDecisionCandidate, 0, len(ordered))
	for i, c := range ordered {
		text, runes, textTruncated := r.clip("decision_pool[].text", c.NodeText)
		pool = append(pool, EpisodeDecisionCandidate{
			Rank:          i,
			Stage:         c.Stage,
			Source:        c.Source,
			Confidence:    c.Confidence,
			SelectorHash:  episodeIdentityHash(bestSelector(c)),
			StructHash:    episodeUint64Hash(c.StructHash),
			Tag:           c.Tag,
			Role:          c.Role,
			Text:          text,
			TextRuneLen:   runes,
			TextTruncated: textTruncated,
			IsSelected:    o.ExecSemantic != "" && c.NodeText == o.ExecSemantic,
		})
	}
	ep.DecisionPool = pool
	return "matcher", len(pool), truncated
}

// projectCandidates 用**派发前采样**的候选集填充 ep.Candidates（§5.0.13 条款 2/3
// ＋ §5.0.14 条款 1①：采样时钟前移，收尾绝不再取样）。
// 返回（candidates_source, 页面侧候选数, 是否被 K=64 截断）。
func (r *episodeRecorder) projectCandidates(ep *Episode, o episodeOutcome) (string, int, bool) {
	if r.pre != nil {
		ep.Candidates = append(ep.Candidates, r.pre.Candidates...)
		return r.pre.Source, r.pre.Count, r.pre.Truncated
	}
	// 未采样（会话无只读快照能力 / 未走到采样点 / 采样失败）：退回第一批口径（候选池残留），
	// 来源如实标记 engine_pool —— 消费端据此 fail-closed，不会被误当成页面投影样本。
	r.appendEnginePoolCandidates(ep, o.Candidates)
	return EpisodeCandidatesEnginePool, 0, false
}

// appendEnginePoolCandidates 是第一批的候选写入口径（engine_pool 来源，保持既有行为）。
func (r *episodeRecorder) appendEnginePoolCandidates(ep *Episode, cands []matcher.Candidate) {
	for i, c := range cands {
		text, runes, truncated := r.clip("candidates[].text", c.NodeText)
		cand := EpisodeCandidate{
			Index:         i,
			Stage:         c.Stage,
			Tag:           c.Tag,
			Role:          c.Role,
			StructHash:    episodeUint64Hash(c.StructHash),
			SelectorHash:  episodeIdentityHash(bestSelector(c)),
			Confidence:    c.Confidence,
			Text:          text,
			TextRuneLen:   runes,
			TextTruncated: truncated,
		}
		for _, m := range c.Matches {
			cand.MatchedRules = append(cand.MatchedRules, EpisodeRule{
				RuleID:        m.Rule.ID,
				RuleType:      m.Rule.RuleType,
				SemanticLabel: m.Rule.SemanticLabel,
				Status:        m.Rule.Status,
				Confidence:    m.Rule.Confidence,
				SelectorHash:  episodeIdentityHash(m.Rule.Selector),
			})
		}
		ep.Candidates = append(ep.Candidates, cand)
	}
}

// skeletonFromNeighbors 把**单个候选自身**的 1 阶骨架转为落盘结构
// （§5.0.13 条款 3④：只留 tag + role；条款 7：对称挂载 ＋ 截断标记）。
func (r *episodeRecorder) skeletonFromNeighbors(neighbors []browser.PageSnapshotNeighbor, truncated bool) EpisodeSkeleton {
	sk := EpisodeSkeleton{Source: "page_snapshot_1hop", Truncated: truncated}
	for _, nb := range neighbors {
		text, runes, _ := r.clip("candidates[].skeleton.text", nb.Text)
		sk.Neighbors = append(sk.Neighbors, EpisodeNeighbor{
			Rel: nb.Rel, Tag: nb.Tag, Role: nb.Role, Text: text, TextRuneLen: runes,
		})
	}
	return sk
}

// skeletonFromSnapshot 是**兼容路径**：取快照顶层骨架（§5.0.13 条款 7④：顶层 Skeleton
// 是 GT 候选 neighbors 的别名）；仅供候选自身无邻居时的旧口径/替身夹具回退使用。
func (r *episodeRecorder) skeletonFromSnapshot(snap browser.PageSnapshot) EpisodeSkeleton {
	return r.skeletonFromNeighbors(snap.Skeleton, false)
}

// episodeGroundTruthCandidate 返回被标为 ground truth 的那一条候选（无则 nil）。
func episodeGroundTruthCandidate(cands []EpisodeCandidate) *EpisodeCandidate {
	for i := range cands {
		if cands[i].IsGroundTruth {
			return &cands[i]
		}
	}
	return nil
}
