package engine

import (
	"log"
	"strings"
	"unicode"

	"uws/pkg/browser"
	"uws/pkg/browser/semantics"
	"uws/pkg/vlm"
)

// FaultClickLandingMismatch：点击落点自证判 miss（§5.0.8 条款 1 代号表 /
// §5.0.9 条款 6）。落点证据显示命中元素与目标语义无关 ⇒ 硬判失败，
// 不得返回 SUCCEEDED、不得写 learned，且不重试、不换出口。
const FaultClickLandingMismatch = "CLICK_LANDING_MISMATCH"

// clickLanding 是点击落点自证的三态判定（§5.0.9 条款 6）。
type clickLanding int

const (
	// landingUnverified：无 hit_element 或命中元素无可见文本 ⇒ 缺证据，不构成反证。
	landingUnverified clickLanding = iota
	// landingRelevant：hit 文本与目标语义互含 ⇒ 点中自身 / 其后代 / 其祖先容器。
	landingRelevant
	// landingMiss：hit 文本非空且与目标语义双向均不互含 ⇒ 明确点错。
	landingMiss
)

// normalizeLandingText 归一化落点文本与目标语义：转小写 ＋ 去全部空白
// （对齐「Sign In」/「signin」这类只差分隔符的写法；向右放宽以避免假 miss）。
func normalizeLandingText(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// clickLandingVerdict 判定一次点击的落点是否自证（§5.0.9 条款 6 三态判据）。
// miss 只在「同文字系统内双向均不互含」时成立（跨文字系统归证据不足，见下）。
// has=false（会话不支持 HitElementSource，或本次动作未记录命中元素）与
// 「命中文本为空」同归证据不足：缺证据不改判出口，但不得写 learned。
func clickLandingVerdict(target string, h browser.HitElement, has bool) clickLanding {
	return clickLandingVerdictRefs(clickLandingRefs(target, nil), h, has)
}

// clickLandingRefs 组装落点自证的比对基准集（§5.0.9 条款 6/7）：target 语义先按
// §5.0.2 别名表展开为页面候选文案（内部标签 `submit_btn` → 「提交」/`submit`/…，
// 未覆盖时退化为单元素原串）；endorsed 是**与目标语义同源**的 KB 背书同义词文本
// （由 endorsedSynonyms 取，同义词对天然不互含，见判据的豁免说明）。
func clickLandingRefs(target string, endorsed []string) []string {
	return append(semantics.Wants(target), endorsed...)
}

// endorsedSynonyms 取「与目标语义同源」的 KB 背书同义词文本（§5.0.9 条款 7 豁免）：
// 「文本互含」判据对同义词没有判别力（`登录` / `登入` 不互含），KB 已背书的映射不得
// 因文本不互含被判 miss——那会造出与 VLM 路径同形的假失败 ＋ 防乱点熔断。同源＝
// `site_synonyms.source_rule_id` 指向语义标签命中目标语义的规则。
// **不得改用候选池的 L2 候选**：池内召回按**页面节点文本**发生、与目标语义无关，
// 一律背书会把本条款要修的假成功原样放过（真机实证：`/tmp/t3det/det.html` 唯一候选
// `Sign in` 被绑定给 `Order history` 仍报 `SUCCEEDED`，spec §5.0.9 条款 7 真机复证①）。
func (s *Server) endorsedSynonyms(target string) []string {
	if s.matcher == nil || strings.TrimSpace(target) == "" {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, want := range semantics.Wants(target) {
		syns, err := s.matcher.SynonymsForSemantic(want)
		if err != nil {
			log.Printf("engine: landing endorsement lookup semantic=%q: %v", want, err)
			continue
		}
		for _, syn := range syns {
			if t := strings.TrimSpace(syn.Term); t != "" && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// clickLandingVerdictRefs 是基准集版本的三态判据（§5.0.9 条款 6/7）：基准集内任一
// 元素与落点互含 ⇒ 相关；至少一条**同文字系统**的比对可判且全部不互含 ⇒ miss；
// 无基准 / 空文本 / 基准全为跨文字系统 ⇒ 证据不足。
func clickLandingVerdictRefs(refs []string, h browser.HitElement, has bool) clickLanding {
	if !has {
		return landingUnverified
	}
	hit := normalizeLandingText(h.Text)
	if hit == "" {
		return landingUnverified
	}
	discriminated := false
	for _, ref := range refs {
		tgt := normalizeLandingText(ref)
		if tgt == "" {
			continue
		}
		if strings.Contains(hit, tgt) || strings.Contains(tgt, hit) {
			return landingRelevant
		}
		// 仅同文字系统的比对具备判别力（跨文字系统时「不互含」是必然）。
		if hasHan(hit) == hasHan(tgt) {
			discriminated = true
		}
	}
	// 跨文字系统（汉字 vs 非汉字）时互含判定没有判别力：中文语义不可能与纯拉丁文本
	// 互含，「双向不互含」是文字系统的必然、不是「点错」的反证。真机实例（2026-09-30）：
	// 语义「登录」命中英文按钮 `Log in`（VLM 语义匹配后坐标点击，点击本身正确）被误判
	// miss 硬失败、连败 3 轮触发防乱点熔断 ⇒ 归证据不足（出口不变、不写 learned）。
	if !discriminated {
		return landingUnverified
	}
	return landingMiss
}

// hasHan 判定字符串是否含汉字（CJK 统一表意文字）。落点自证用它划「互含判定是否
// 具备判别力」的边界：汉字与非汉字混排的文本仍算同侧（如 `登录 Sign in`）。
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// clickLandingOf 读取会话最近一次命中元素并给出三态判定。
func clickLandingOf(bc *browser.BrowserContext, target string) (clickLanding, browser.HitElement) {
	return clickLandingOfRefs(bc, clickLandingRefs(target, nil))
}

// clickLandingOfRefs 读取会话最近一次命中元素并按基准集给出三态判定。
func clickLandingOfRefs(bc *browser.BrowserContext, refs []string) (clickLanding, browser.HitElement) {
	if bc == nil {
		return landingUnverified, browser.HitElement{}
	}
	hs, ok := bc.Session().(browser.HitElementSource)
	if !ok {
		return landingUnverified, browser.HitElement{}
	}
	h, has := hs.LastHitElement()
	return clickLandingVerdictRefs(refs, h, has), h
}

// isClickAction 判定请求动作是否属点击族（落点自证只约束点击）。
func isClickAction(action string) bool {
	return action == "click"
}

// isNodeAddressed 判定目标语义是否为 node_id 寻址形式（`node:<n>`，v1.5.5 混合指挥）：
// 此时不存在独立的目标语义可供落点比对（学习键本身取自命中元素可见文本），
// 落点自证不适用——确定性寻址的正确性由调用方（探针 node_id）负责。
func isNodeAddressed(target string) bool {
	return strings.HasPrefix(strings.TrimSpace(target), "node:")
}

// landingSelfCheckApplies 判定该目标语义是否适用文本互含落点自证（§5.0.9 条款 6/7）。
// 两类不适用：① node_id 寻址（无独立目标语义）；② **站点控件语义**（§5.0.9 条款 8：
// 标识是引擎侧内部控件名、落点文本是站点固定 UI 文案——真机 2026-10-01 `hit=Flash` 对
// `ref=gemini_model_switch`、`hit=Send message` 对 `ref=gemini_send`），其正确性由各自的
// 专用后验（切换回读 ＋ 菜单自检 / verifyGeminiClick）负责，在此判 miss 只会造出假失败
// 并把整页点进防乱点熔断。
func landingSelfCheckApplies(targetSemantic string) bool {
	if isNodeAddressed(targetSemantic) {
		return false
	}
	return !semantics.IsSiteControlSemantic(targetSemantic)
}

// clickLearnGate 是 learned 写入门禁（§5.0.9 条款 6 核心防线）：点击族仅在落点
// 自证「相关」时允许写；miss 与证据不足一律不写。非点击动作沿用既有行为。
func clickLearnGate(bc *browser.BrowserContext, action, target string) (bool, string) {
	if !isClickAction(action) {
		return true, ""
	}
	if !landingSelfCheckApplies(target) {
		if isNodeAddressed(target) {
			return true, ""
		}
		return false, "landing=n/a(site control semantic)"
	}
	v, h := clickLandingOf(bc, target)
	switch v {
	case landingRelevant:
		return true, ""
	case landingMiss:
		return false, "landing=miss hit_text=" + h.Text
	default:
		return false, "landing=unverified(no hit text)"
	}
}

// learnedForget 删除一条已学定位（learned 复用判 miss 时）。
func (s *Server) learnedForget(url, target string) {
	if s.vlmFb == nil || s.vlmFb.Learned == nil || url == "" {
		return
	}
	if err := s.vlmFb.Learned.Forget(url, target); err != nil {
		log.Printf("vlm: forget url=%s semantic=%q: %v", url, target, err)
	}
}

// learnedReuseFault 对 learned 复用命中的落点做自证（§5.0.9 条款 6「适用路径」）：
// 学到的是「页面 + 语义 → 定位」，页面改版后同一 selector / 文本可能落到无关元素上
// ——此时必须与首学路径同样判 miss：不返回 SUCCEEDED、不重试，并清掉失效条目
// （下次自动重新走 VLM 学习；留着的坏条目会被反复复用、把整页点进防乱点熔断）。
func (s *Server) learnedReuseFault(bc *browser.BrowserContext, action, url, target string) (string, bool) {
	if !isClickAction(action) || isNodeAddressed(target) {
		return "", false
	}
	v, h := clickLandingOf(bc, target)
	if v != landingMiss {
		return "", false
	}
	s.learnedForget(url, target)
	log.Printf("vlm: learned landing mismatch url=%s semantic=%q hit=%q", url, target, h.Text)
	return FaultClickLandingMismatch + ":learned 落点与目标语义无关 hit=" + h.Text, true
}

// vlmLearnOnClick 是点击族 learned 写入的单点入口：非点击动作直接写，点击动作
// 过 clickLearnGate（§5.0.9 条款 6）。
func (s *Server) vlmLearnOnClick(bc *browser.BrowserContext, action, url, target string, e vlm.LearnedEntry) {
	if allowed, why := clickLearnGate(bc, action, target); !allowed {
		log.Printf("vlm: learned skipped url=%s semantic=%q action=%s %s", url, target, action, why)
		return
	}
	s.vlmLearn(url, target, e)
}

// clickLandingReference 决定确定性路径点击出口的比对基准（§5.0.9 条款 7）：
// A* **多步换步**（路径长度 ≥2）时末步是到目标的中间步骤，与目标文案无必然包含
// 关系，基准取 `exec_semantic`；其余（直选 / 单候选绑定 / 长度 1 的退化「路径」——
// 根即答案）执行的就是目标本身，基准取 `target_semantic`。
func clickLandingReference(targetSemantic, execSemantic string, aStarDepth int) string {
	if aStarDepth >= 2 && strings.TrimSpace(execSemantic) != "" {
		return execSemantic
	}
	return targetSemantic
}

// deterministicLandingFault 对确定性路径（语义 / 选择器直查）的点击出口判落点自证
// （§5.0.9 条款 7）：判 miss ⇒ 返回硬失败代号；不写 learned、不换出口（调用方据此
// 跳过 VLM 兜底重问）。真机反证：页面无 `Order history` 元素，单候选 `Sign in` 被强行
// 绑定给该语义后仍报 `SUCCEEDED`——与 VLM 路径同形的假成功。
func (s *Server) deterministicLandingFault(bc *browser.BrowserContext, action, targetSemantic, execSemantic string,
	aStarDepth int) (string, bool) {
	if !isClickAction(action) || !landingSelfCheckApplies(targetSemantic) {
		return "", false
	}
	ref := clickLandingReference(targetSemantic, execSemantic, aStarDepth)
	v, h := clickLandingOfRefs(bc, clickLandingRefs(ref, s.endorsedSynonyms(ref)))
	if v != landingMiss {
		return "", false
	}
	log.Printf("engine: deterministic click landing mismatch semantic=%q ref=%q hit=%q", targetSemantic, ref, h.Text)
	return FaultClickLandingMismatch + ":确定性路径落点与目标语义无关 ref=" + ref + " hit=" + h.Text, true
}

// landingHardFailObserved 判定失败观测串是否属于「不得换出口」的落点自证硬失败
// （§5.0.9 条款 6/7）：判 miss 后不得再回落 VLM 兜底重问——那等于换出口重演。
func landingHardFailObserved(observed string) bool {
	return strings.HasPrefix(observed, FaultClickLandingMismatch)
}
