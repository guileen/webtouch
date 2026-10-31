package engine

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	"uws/pkg/browser"
	"uws/pkg/classifier"
	"uws/pkg/matcher"
	uwspb "uws/proto/gen/go/proto/v1"
)

// episodeLocateFailures 是物理层「定位阶段失败」族的观测串片段（CDP 会话诊断口径，
// see pkg/browser/cdp_session.go::classifyFindFailure 与 executeByNode）：元素未定位
// ⇒ 物理事件从未发出。真因仍原样留在 production.observed_code，不改写、不重编码。
var episodeLocateFailures = []string{
	"no_match",            // 探针 JS 正常执行但 found:false
	"eval_empty",          // Runtime.evaluate 成功但取值为空
	"eval_error:",         // Runtime.evaluate 抛异常
	"target_not_found",    // 定位未返回目标元素
	"node_id_unsupported", // 会话不支持 node_id 确定性定位
}

// episodeLocateStageFailed 判断观测串是否属定位阶段失败族。
func episodeLocateStageFailed(observed string) bool {
	for _, f := range episodeLocateFailures {
		if strings.Contains(observed, f) {
			return true
		}
	}
	return false
}

// dispatchState 判定派发状态（§5.0.11）：取消（未/已派发）优先，其次定位阶段失败
// （物理事件从未发出），其次未进入执行层；已派发且有结构化代号=已知负面结局；
// 已派发但既无代号也无观测=结果未知。
//
// 「进入执行层调用」只是派发的**粗代理**：它无法区分「事件已发出」与「定位阶段就失败」。
// 故凡观测串属定位阶段失败族（no_match / eval_empty / eval_error / target_not_found /
// node_id_unsupported）一律判为未派发 —— 与 §5.0.11 条款 3 同向（定位阶段失败可安全重放，
// 不得被记成「结果未知」而让调用方放弃重试）。
func (r *episodeRecorder) dispatchState(o episodeOutcome, code string) string {
	if canceled, after := browser.IsCanceledObserved(o.Observed); canceled {
		if after {
			return EpisodeCanceledAfterDispatch
		}
		return EpisodeCanceledBeforeDispatch
	}
	if episodeLocateStageFailed(o.Observed) {
		return EpisodeNotDispatched
	}
	if !r.dispatched {
		return EpisodeNotDispatched
	}
	if code != "" {
		return EpisodeDispatchedKnown
	}
	if o.Err == nil || o.Resp != nil {
		return EpisodeDispatchedKnown
	}
	return EpisodeDispatchedUnknown
}

// posterior 构造页面后验（条款 1 ④）。失败/未派发一律不回填陈旧命中元素。
func (r *episodeRecorder) posterior(o episodeOutcome) EpisodePosterior {
	p := EpisodePosterior{State: "unknown", ReadbackSource: "none"}
	if o.Resp != nil && o.Resp.GetSuccess() {
		p.State = "confirmed"
		if he := o.Resp.GetHitElement(); he != nil {
			p.HitTag = he.GetTag()
			p.HitSelectorHash = episodeIdentityHash(he.GetSelector())
			text, runes, _ := r.clip("posterior.hit_text", he.GetText())
			p.HitText, p.HitTextRuneLen = text, runes
		}
		p.URLChanged = o.Resp.GetUrlChanged()
		p.ValueUpdated = o.Resp.GetLocalVerification().GetValueUpdated()
		return p
	}
	if r.dispatched {
		p.State = "failed"
		return p
	}
	if o.Err != nil {
		p.State = "not_dispatched"
	}
	return p
}

// elapsedMs 返回本记录器自创建以来的毫秒数。
func (r *episodeRecorder) elapsedMs() int64 {
	return time.Since(r.ep.At).Milliseconds()
}

// episodeSelected 是生产实际作用目标的最小描述。
type episodeSelected struct {
	Ref          string
	Tag          string
	Role         string
	SelectorHash string
	RawText      string
}

// episodeSelectedOf 把执行层语义回映射到候选池里的那一个候选（找不到则留空）。
func episodeSelectedOf(cands []matcher.Candidate, execSemantic string) episodeSelected {
	c := episodeCandidateOf(cands, execSemantic)
	if c == nil {
		return episodeSelected{}
	}
	return episodeSelected{
		Ref:          episodeCandidateRef(*c),
		Tag:          c.Tag,
		Role:         c.Role,
		SelectorHash: episodeIdentityHash(bestSelector(*c)),
		RawText:      c.NodeText,
	}
}

// episodeCandidateOf 返回候选池中 NodeText == execSemantic 的那一条（无则 nil，绝不猜）。
func episodeCandidateOf(cands []matcher.Candidate, execSemantic string) *matcher.Candidate {
	for i := range cands {
		if cands[i].NodeText == execSemantic {
			return &cands[i]
		}
	}
	return nil
}

// episodeCandidateRef 是候选的稳定短引用（身份哈希，无原文）。
func episodeCandidateRef(c matcher.Candidate) string {
	identity := strings.Join([]string{
		c.Stage, c.Tag, c.Role, episodeUint64Hash(c.StructHash), bestSelector(c), c.NodeText,
	}, "|")
	return episodeIdentityHash(identity)
}

// bestSelector 取候选命中规则里最具体的 ruleselector 作为元素身份来源。
func bestSelector(c matcher.Candidate) string {
	for _, m := range c.Matches {
		if m.Rule.Selector != "" {
			return m.Rule.Selector
		}
	}
	return ""
}

// episodeUint64Hash 把结构哈希渲染为定长十六进制串（0 视为未提供）。
func episodeUint64Hash(h uint64) string {
	if h == 0 {
		return ""
	}
	return fmt.Sprintf("%016x", h)
}

// episodePageURL 把 URL 裁剪为 scheme://host/path（丢弃查询串/片段/用户信息）。
// 返回（裁剪结果, 是否发生了丢弃）。解析失败时原样返回，交由脱敏规则兜底。
func episodePageURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw, false
	}
	stripped := u.RawQuery != "" || u.Fragment != "" || u.User != nil
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String(), stripped
}

// episodeRespURL 读响应里的动作后 URL（缺证据时返回空串）。
func episodeRespURL(resp *uwspb.ActionResultResponse) string {
	if resp == nil {
		return ""
	}
	return resp.GetPageUrl()
}

// ── 决策标签（离线端按此分组；标签由既有判定函数产出，不复制阈值）────

// episodeMatcherDecision 把候选置信度带的决策映射为 Episode 标签。
func episodeMatcherDecision(d matcher.SearchDecision) string {
	switch d {
	case matcher.DecisionDirect:
		return "DIRECT"
	case matcher.DecisionExplore:
		return "EXPLORE"
	case matcher.DecisionAStar:
		return "ASTAR"
	case matcher.DecisionLLMFallback:
		return "LLM_FALLBACK"
	}
	return strings.ToUpper(string(d))
}

// episodeClassifierDecision 把无候选时决策核的三态映射为 Episode 标签。
func episodeClassifierDecision(d classifier.Decision) string {
	switch d {
	case classifier.DecisionExecute:
		return "EXECUTE"
	case classifier.DecisionExplore:
		return "EXPLORE"
	case classifier.DecisionFallback:
		return "LLM_FALLBACK"
	}
	return "UNKNOWN"
}

// episodeDirectDecision 标注未经过决策核的物理直发通道（drag / evaluate / node_id）。
func episodeDirectDecision(evaluate, node, drag bool) string {
	switch {
	case evaluate:
		return "EVALUATE"
	case node:
		return "NODE_DIRECT"
	case drag:
		return "DRAG_DIRECT"
	}
	return "PHYSICAL_DIRECT"
}
