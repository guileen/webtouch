// 规则模型与 SQLite 规则库（spec/algorithm-kernel.md §4.2）。
// 所有查询一律参数化（Prepared Statement），外部输入（target_semantic 等）只作为参数传递，
// 禁止拼接进 SQL——对应 TDD-MATCH-03 安全红线。
package matcher

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Rule 对应 uws_rules 表的一行（本阶段只实现 text_pattern 子集所需字段）。
type Rule struct {
	ID            int64
	RuleType      string
	TextPattern   string
	Selector      string
	Role          string
	Action        string
	SemanticLabel string
	Confidence    float64
	Source        string
	Status        string

	// ★Tier 1.5 接线：规则携带的结构通道参考哈希（StructuralSimHash64）。
	// 0 = 未提供（L3 退化为文本单通道）；由蒸馏/component_gene 写入时正式启用双通道。
	StructHash uint64
}

// Synonym 对应 site_synonyms 表的一行（spec/algorithm-kernel.md §4.3）。
// 防投毒状态机：TENTATIVE 仅 0.2 微扰动 → hit_count>=5 升 ACTIVE（权重 1.0）→
// 频繁兜底标记 POISONED 物理隔离。
type Synonym struct {
	ID           int64
	SiteDomain   string
	Term         string  // 同义词，如 "Sign in"
	BaseWeight   float64 // TENTATIVE=0.2 / ACTIVE=1.0
	Status       string  // TENTATIVE | ACTIVE | POISONED
	HitCount     int
	SourceRuleID int64
}

// 同义词状态常量。
const (
	StatusTentative = "TENTATIVE"
	StatusActive    = "ACTIVE"
	StatusPoisoned  = "POISONED"
)

// schema 与 spec/algorithm-kernel.md §4.2 对齐（按需裁剪字段）。
const schema = `
CREATE TABLE IF NOT EXISTS uws_rules (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_type       TEXT NOT NULL,
    text_pattern    TEXT,
    selector        TEXT,
    role            TEXT,
    action          TEXT NOT NULL,
    semantic_label  TEXT,
    confidence      REAL NOT NULL,
    source          TEXT,
    status          TEXT,
    struct_hash     INTEGER NOT NULL DEFAULT 0,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_uws_rules_text ON uws_rules(text_pattern);

CREATE TABLE IF NOT EXISTS site_synonyms (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    site_domain     TEXT NOT NULL DEFAULT '*',
    synonym         TEXT NOT NULL,
    base_weight     REAL NOT NULL DEFAULT 0.2,
    status          TEXT NOT NULL DEFAULT 'TENTATIVE',
    hit_count       INTEGER NOT NULL DEFAULT 0,
    source_rule_id  INTEGER,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_synonym_domain ON site_synonyms(site_domain);

CREATE TABLE IF NOT EXISTS uws_negative_keywords (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    keyword    TEXT NOT NULL UNIQUE,
    source     TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
`

// Store 封装 uws_rules 的读写，DSN 形如 ":memory:" 或文件路径。
type Store struct {
	db *sql.DB
}

// NewStore 打开 SQLite 并初始化 schema。
func NewStore(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// 旧库（v1.5.2 前）未含 struct_hash 列：幂等补列，避免 ActiveTextPatterns 等
	// 查询因「no such column」失败（Tier 1.5 结构通道接线，兼容既有 artifact）。
	if _, err := db.Exec(
		`ALTER TABLE uws_rules ADD COLUMN struct_hash INTEGER NOT NULL DEFAULT 0`,
	); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, fmt.Errorf("migrate struct_hash column: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 释放数据库连接。
func (s *Store) Close() error {
	return s.db.Close()
}

// InsertRule 以参数化语句写入一条规则，返回自增 ID。
func (s *Store) InsertRule(r Rule) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO uws_rules
			(rule_type, text_pattern, selector, role, action, semantic_label, confidence, source, status, struct_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RuleType, r.TextPattern, r.Selector, r.Role, r.Action,
		r.SemanticLabel, r.Confidence, r.Source, r.Status, int64(r.StructHash),
	)
	if err != nil {
		return 0, fmt.Errorf("insert rule: %w", err)
	}
	return res.LastInsertId()
}

// ActiveTextPatterns 加载启用中的 text_pattern 规则，用于构建 L1 AC 自动机。
func (s *Store) ActiveTextPatterns() ([]Rule, error) {
	rows, err := s.db.Query(
		`SELECT id, rule_type, text_pattern, selector, role, action, semantic_label,
		        confidence, source, status, struct_hash
		   FROM uws_rules
		  WHERE status = 'active' AND rule_type = 'text_pattern'
		    AND text_pattern IS NOT NULL AND text_pattern <> ''`,
	)
	if err != nil {
		return nil, fmt.Errorf("query active text patterns: %w", err)
	}
	defer rows.Close()

	var out []Rule
	for rows.Next() {
		var r Rule
		var structHash int64
		if err := rows.Scan(&r.ID, &r.RuleType, &r.TextPattern, &r.Selector, &r.Role,
			&r.Action, &r.SemanticLabel, &r.Confidence, &r.Source, &r.Status,
			&structHash); err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		r.StructHash = uint64(structHash)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RulesBySemantic 按语义标签模糊检索规则。
// term 来自外部（如 target_semantic），仅作为 LIKE 参数绑定，绝不参与 SQL 拼接。
func (s *Store) RulesBySemantic(term string) ([]Rule, error) {
	rows, err := s.db.Query(
		`SELECT id, rule_type, text_pattern, selector, role, action, semantic_label,
		        confidence, source, status, struct_hash
		   FROM uws_rules
		  WHERE semantic_label LIKE ?`,
		"%"+term+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("query rules by semantic: %w", err)
	}
	defer rows.Close()

	var out []Rule
	for rows.Next() {
		var r Rule
		var structHash int64
		if err := rows.Scan(&r.ID, &r.RuleType, &r.TextPattern, &r.Selector, &r.Role,
			&r.Action, &r.SemanticLabel, &r.Confidence, &r.Source, &r.Status,
			&structHash); err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		r.StructHash = uint64(structHash)
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertSynonym 写入一条同义词；默认站点通配 '*'、TENTATIVE 态、0.2 基础权重。
func (s *Store) InsertSynonym(syn Synonym) (int64, error) {
	if syn.SiteDomain == "" {
		syn.SiteDomain = "*"
	}
	if syn.Status == "" {
		syn.Status = StatusTentative
	}
	if syn.BaseWeight <= 0 {
		syn.BaseWeight = 0.2
	}
	res, err := s.db.Exec(
		`INSERT INTO site_synonyms (site_domain, synonym, base_weight, status, hit_count, source_rule_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		syn.SiteDomain, syn.Term, syn.BaseWeight, syn.Status, syn.HitCount, syn.SourceRuleID,
	)
	if err != nil {
		return 0, fmt.Errorf("insert synonym: %w", err)
	}
	return res.LastInsertId()
}

// ActiveSynonyms 加载非 POISONED 的同义词（L2 召回白名单）。
func (s *Store) ActiveSynonyms() ([]Synonym, error) {
	rows, err := s.db.Query(
		`SELECT id, site_domain, synonym, base_weight, status, hit_count, source_rule_id
		   FROM site_synonyms
		  WHERE status != ?`,
		StatusPoisoned,
	)
	if err != nil {
		return nil, fmt.Errorf("query active synonyms: %w", err)
	}
	defer rows.Close()

	var out []Synonym
	for rows.Next() {
		var syn Synonym
		if err := rows.Scan(&syn.ID, &syn.SiteDomain, &syn.Term, &syn.BaseWeight,
			&syn.Status, &syn.HitCount, &syn.SourceRuleID); err != nil {
			return nil, fmt.Errorf("scan synonym: %w", err)
		}
		out = append(out, syn)
	}
	return out, rows.Err()
}

// SynonymsForSemantic 按语义标签检索规则并回取**同源**同义词（`source_rule_id` 关联）：
// 落点自证的 KB 背书豁免用它取基准——只认与目标语义同源的同义对（§5.0.9 条款 7）。
// 「池内 L2 一律背书」不成立：候选池的同义词召回按**页面节点文本**发生，与目标语义无关。
// term 来自外部（target_semantic），仅作 LIKE 参数绑定，绝不参与 SQL 拼接。
func (s *Store) SynonymsForSemantic(term string) ([]Synonym, error) {
	rows, err := s.db.Query(
		`SELECT s.id, s.site_domain, s.synonym, s.base_weight, s.status, s.hit_count, s.source_rule_id
		   FROM site_synonyms s JOIN uws_rules r ON r.id = s.source_rule_id
		  WHERE r.semantic_label LIKE ? AND s.status != ?
		  ORDER BY s.id`,
		"%"+term+"%", StatusPoisoned,
	)
	if err != nil {
		return nil, fmt.Errorf("query synonyms by semantic: %w", err)
	}
	defer rows.Close()

	var out []Synonym
	for rows.Next() {
		var syn Synonym
		if err := rows.Scan(&syn.ID, &syn.SiteDomain, &syn.Term, &syn.BaseWeight,
			&syn.Status, &syn.HitCount, &syn.SourceRuleID); err != nil {
			return nil, fmt.Errorf("scan synonym: %w", err)
		}
		out = append(out, syn)
	}
	return out, rows.Err()
}

// InsertNegativeKeyword 写入一个黑名单关键词（重复写入幂等忽略）。
func (s *Store) InsertNegativeKeyword(keyword string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO uws_negative_keywords (keyword) VALUES (?)`,
		keyword,
	)
	if err != nil {
		return 0, fmt.Errorf("insert negative keyword: %w", err)
	}
	return res.LastInsertId()
}

// ReplaceAll 在单事务内全量替换规则/同义词/黑名单（R1 热更原子性：
// 读者要么看到全旧快照，要么看到全新快照，无半更新状态）。
// status 归一为小写（exporter 可能产出 ACTIVE/active 混合写法）。
func (s *Store) ReplaceAll(rules []Rule, synonyms []Synonym, keywords []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin replace: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range []string{
		`DELETE FROM uws_rules`,
		`DELETE FROM site_synonyms`,
		`DELETE FROM uws_negative_keywords`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("clear tables: %w", err)
		}
	}
	for _, r := range rules {
		if r.RuleType == "text_pattern" && r.TextPattern == "" {
			continue // 无文本模式无法进 AC；防御性跳过
		}
		if _, err := tx.Exec(
			`INSERT INTO uws_rules
				(rule_type, text_pattern, selector, role, action, semantic_label, confidence, source, status, struct_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.RuleType, r.TextPattern, r.Selector, r.Role, r.Action,
			r.SemanticLabel, r.Confidence, r.Source, normalizeStatus(r.Status),
			int64(r.StructHash),
		); err != nil {
			return fmt.Errorf("insert rule: %w", err)
		}
	}
	for _, syn := range synonyms {
		domain := syn.SiteDomain
		if domain == "" {
			domain = "*"
		}
		status := syn.Status
		if status == "" {
			status = StatusTentative
		}
		weight := syn.BaseWeight
		if weight <= 0 {
			weight = 0.2
		}
		if _, err := tx.Exec(
			`INSERT INTO site_synonyms (site_domain, synonym, base_weight, status, hit_count, source_rule_id)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			domain, syn.Term, weight, status, syn.HitCount, syn.SourceRuleID,
		); err != nil {
			return fmt.Errorf("insert synonym: %w", err)
		}
	}
	for _, kw := range keywords {
		if strings.TrimSpace(kw) == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO uws_negative_keywords (keyword) VALUES (?)`,
			kw,
		); err != nil {
			return fmt.Errorf("insert negative keyword: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace: %w", err)
	}
	return nil
}

func normalizeStatus(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "active"
	}
	return s
}

// NegativeKeywords 返回全部黑名单关键词。
func (s *Store) NegativeKeywords() ([]string, error) {
	rows, err := s.db.Query(`SELECT keyword FROM uws_negative_keywords ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query negative keywords: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var kw string
		if err := rows.Scan(&kw); err != nil {
			return nil, fmt.Errorf("scan negative keyword: %w", err)
		}
		out = append(out, kw)
	}
	return out, rows.Err()
}
