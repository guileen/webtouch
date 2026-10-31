// Matcher 组合规则库（SQLite）、L1 AC 自动机、L2 同义词召回与 negative_keywords 黑名单：
// 命中规则即入围候选池。本阶段覆盖 TDD-MATCH-01/02/03/04；L3 SimHash 物理校验
// （TDD-MATCH-05）待 DOM/候选特征输入通道（见 USER_ACTION.md）。
package matcher

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// ErrBlockedByNegativeKeyword 表示节点/容器命中 negative_keywords 黑名单：
// 不得进入候选池，也不得进入 LLM 兜底（引擎映射为 gRPC NOT_FOUND）。
var ErrBlockedByNegativeKeyword = errors.New("matcher: blocked by negative keyword")

// semanticRecallPrior 是 L2 同义词召回的基础先验置信度。
// TENTATIVE 同义词在此之上仅做 0.2 微扰动（0.5+0.2=0.7，落 EXPLORE 区间）；
// 真实 BM25 倒排索引落地后由索引分数替代。
const semanticRecallPrior = 0.5

// Match 是一条规则的命中结果。
type Match struct {
	Rule Rule
}

// Candidate 是入围候选池的节点（L1 AC 命中 / L2 同义词召回）。
// Confidence 为候选置信度；Stage 标记来源层级（L1/L2），供决策与审计。
type Candidate struct {
	NodeText   string
	Matches    []Match
	Confidence float64
	Stage      string

	// ★Tier 1.5 接线：结构通道（TDD-PROBE-06）。
	// StructHash 是阻尼 DOM 拓扑哈希（StructuralSimHash64，0=未提供时回退文本通道）；
	// Tag/Role 供 L3 身份兼容门禁（StructRoleCompatible）与 A* 拓扑剪枝门。
	StructHash uint64
	Tag        string
	Role       string

	// ★§5.0.15（`T-30` 决策池落盘）：Source 是候选**入池通道**的来源标注，
	// 取值 page_probe / page_onnx / page_l2_kb / semantic_direct（空串＝未知）。
	// 纯增量观测字段：不参与任何匹配、排序或决策判定。
	Source string

	// ★§5.0.19（`T-30` 候选块复选）：Selector 与物理盒模型几何（CSS 像素）。
	Selector  string
	X         float64
	Y         float64
	W         float64
	H         float64
	DocumentX float64
	DocumentY float64
}

// Matcher 是匹配入口。
type Matcher struct {
	mu               sync.RWMutex // 热更（ReplaceAll/Insert*/reload）与 Match* 读并发安全（R7）
	store            *Store
	ac               *ACAutomaton
	synonyms         []Synonym
	negativeKeywords []string
}

// New 打开规则库并构建 L1 AC 自动机。
func New(dsn string) (*Matcher, error) {
	store, err := NewStore(dsn)
	if err != nil {
		return nil, err
	}
	m := &Matcher{store: store}
	if err := m.reload(); err != nil {
		store.Close()
		return nil, err
	}
	return m, nil
}

// reload 从规则库重建 AC 自动机（新增规则入库后调用）。
func (m *Matcher) reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadLocked()
}

// reloadLocked 假定调用方已持有写锁。
func (m *Matcher) reloadLocked() error {
	rules, err := m.store.ActiveTextPatterns()
	if err != nil {
		return fmt.Errorf("load active rules: %w", err)
	}
	m.ac = NewACAutomaton(rules)
	synonyms, err := m.store.ActiveSynonyms()
	if err != nil {
		return fmt.Errorf("load synonyms: %w", err)
	}
	m.synonyms = synonyms
	kws, err := m.store.NegativeKeywords()
	if err != nil {
		return fmt.Errorf("load negative keywords: %w", err)
	}
	m.negativeKeywords = kws
	return nil
}

// Close 释放规则库连接。
func (m *Matcher) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.Close()
}

// MatchNode 对单节点文本执行 L1 AC 匹配；命中则节点入围候选池。
// 空文本或未命中返回空候选，不报错。
func (m *Matcher) MatchNode(nodeText string) ([]Candidate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.matchNodeLocked(nodeText)
}

func (m *Matcher) matchNodeLocked(nodeText string) ([]Candidate, error) {
	if nodeText == "" {
		return nil, nil
	}
	hits := m.ac.Match(nodeText)
	if len(hits) == 0 {
		return nil, nil
	}
	matches := make([]Match, 0, len(hits))
	seen := make(map[string]bool, len(hits))
	for _, r := range hits {
		key := r.RuleType + "\x00" + r.TextPattern + "\x00" + r.Action
		if seen[key] {
			continue
		}
		seen[key] = true
		matches = append(matches, Match{Rule: r})
	}
	conf := 0.0
	for _, mt := range matches {
		if mt.Rule.Confidence > conf {
			conf = mt.Rule.Confidence
		}
	}
	return []Candidate{{NodeText: nodeText, Matches: matches, Confidence: conf, Stage: "L1"}}, nil
}

// InsertRule 写入一条规则并重建 L1 AC 自动机（引擎接线/规则热更使用）。
func (m *Matcher) InsertRule(r Rule) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, err := m.store.InsertRule(r)
	if err != nil {
		return 0, err
	}
	if err := m.reloadLocked(); err != nil {
		return 0, err
	}
	return id, nil
}

// InsertSynonym 写入一条同义词并重建 L2 召回表（TENTATIVE 态默认 0.2 权重）。
func (m *Matcher) InsertSynonym(syn Synonym) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, err := m.store.InsertSynonym(syn)
	if err != nil {
		return 0, err
	}
	if err := m.reloadLocked(); err != nil {
		return 0, err
	}
	return id, nil
}

// InsertNegativeKeyword 写入一个黑名单关键词并刷新过滤表。
func (m *Matcher) InsertNegativeKeyword(keyword string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, err := m.store.InsertNegativeKeyword(keyword)
	if err != nil {
		return 0, err
	}
	if err := m.reloadLocked(); err != nil {
		return 0, err
	}
	return id, nil
}

// ReplaceAll 以知识库 artifact 的全量快照原子替换规则/同义词/黑名单并重建
// L1 AC 自动机（R1：知识库热更后 matcher 与 active 版本保持一致；
// 单事务 + 写锁保证读者要么全旧要么全新，无半更新状态）。
func (m *Matcher) ReplaceAll(rules []Rule, synonyms []Synonym, keywords []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.ReplaceAll(rules, synonyms, keywords); err != nil {
		return err
	}
	return m.reloadLocked()
}

// MatchQuery 是语义定位入口（引擎 ExecuteAction 使用）：
//  1. 先做 negative_keywords 黑名单过滤（节点/容器命中即拦截，返回 ErrBlockedByNegativeKeyword）；
//  2. L1 AC 精确命中入候选池；
//  3. L2 site_synonyms 同义词召回（TENTATIVE 0.5+0.2 扰动，ACTIVE 升至 1.0，POISONED 隔离）。
//
// hints 为容器上下文（如 zonal_keyword），用于黑名单判定。
func (m *Matcher) MatchQuery(query string, hints map[string]string) ([]Candidate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.blacklisted(query, hints) {
		return nil, ErrBlockedByNegativeKeyword
	}
	cands, err := m.matchNodeLocked(query)
	if err != nil {
		return nil, err
	}
	norm := normalizeTerm(query)
	for _, syn := range m.synonyms {
		if normalizeTerm(syn.Term) != norm {
			continue
		}
		weight := syn.BaseWeight
		if weight <= 0 {
			weight = 0.2
		}
		conf := semanticRecallPrior + weight
		if conf > 1 {
			conf = 1
		}
		cands = append(cands, Candidate{
			NodeText:   syn.Term,
			Confidence: conf,
			Stage:      "L2",
		})
	}
	return cands, nil
}

// RulesBySemantic 按语义标签模糊检索规则（引擎 ExecuteAction 语义定位用）。
func (m *Matcher) RulesBySemantic(term string) ([]Rule, error) {
	return m.store.RulesBySemantic(term)
}

// SynonymsForSemantic 返回与某语义同源的 L2 同义词（落点自证背书集，§5.0.9 条款 7）。
func (m *Matcher) SynonymsForSemantic(term string) ([]Synonym, error) {
	return m.store.SynonymsForSemantic(term)
}

// StructuralReference 返回规则库中某语义的最强结构参考哈希与身份 role。
// 供引擎在候选无 Matches（ONNX/置信度打分候选，pageCandidatePool 路径）时显式取
// 规则级结构参考，避免 bestStructRule(cands) 因候选缺 Matches 而退化文本单通道
// （Tier 1.5 双通道 L3 接线：规则 StructHash 由 component_gene 写入后正式启明）。
func (m *Matcher) StructuralReference(semantic string) (hash uint64, role string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rules, err := m.store.RulesBySemantic(semantic)
	if err != nil {
		return 0, "", false
	}
	best := -1.0
	for _, r := range rules {
		if r.StructHash != 0 && r.Confidence > best {
			best = r.Confidence
			hash = r.StructHash
			role = r.Role // 规则表仅携带 role 列；结构性 tag 校验由结构哈希承担
			ok = true
		}
	}
	return hash, role, ok
}

func (m *Matcher) blacklisted(query string, hints map[string]string) bool {
	if len(m.negativeKeywords) == 0 {
		return false
	}
	texts := make([]string, 0, 1+len(hints))
	texts = append(texts, query)
	for _, v := range hints {
		texts = append(texts, v)
	}
	for _, kw := range m.negativeKeywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		for _, t := range texts {
			if containsKeyword(t, kw) {
				return true
			}
		}
	}
	return false
}

// containsKeyword 对 CJK 关键词做子串匹配，对 ASCII 关键词做边界 token 匹配
// （空格/下划线/连字符为边界，避免 "ad" 误伤 "read" 等合法语义）。
func containsKeyword(text, kw string) bool {
	if kw == "" {
		return false
	}
	if isCJK(kw) {
		return strings.Contains(text, kw)
	}
	return hasBoundaryToken(text, kw)
}

func isCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func hasBoundaryToken(text, kw string) bool {
	text = strings.ToLower(text)
	kw = strings.ToLower(kw)
	for i := 0; ; {
		idx := strings.Index(text[i:], kw)
		if idx < 0 {
			return false
		}
		pos := i + idx
		beforeOK := pos == 0 || isBoundary(text[pos-1])
		after := pos + len(kw)
		afterOK := after >= len(text) || isBoundary(text[after])
		if beforeOK && afterOK {
			return true
		}
		i = pos + utf8.RuneLen(rune(text[pos]))
	}
}

func isBoundary(b byte) bool {
	return b == '_' || b == ' ' || b == '-' || b == '.'
}

func normalizeTerm(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
