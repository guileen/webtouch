// online_logs 动作日志落库（algorithm-kernel.md §4.4）：
//   - SQLite（modernc.org/sqlite，纯 Go 零 CGO）；
//   - PII 脱敏强制（SEC-01/03）：敏感字段名（pwd/pass/ssn/phone/id_card）的语义与
//     值在写入前替换为 [REDACTED]；node_features / decision_trace 只保留长度与类型
//     元信息，不落任何原始文本/值；
//   - 快照压缩：规格写 zstd，当前以明文 JSON 落库（体积小、便于审计），
//     zstd 压缩待接入压缩依赖后补齐（见 USER_ACTION.md）。
package engine

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"time"

	uwspb "uws/proto/gen/go/proto/v1"

	_ "modernc.org/sqlite"
)

// redacted 是 PII 脱敏替换值（algorithm-kernel.md §4.4）。
const redacted = "[REDACTED]"

// sensitiveFieldRe 命中即视为敏感字段（密码/证件号/电话等）：
// pwd / pass(word) / ssn / phone / id_card（含 password 与常见组合）。
var sensitiveFieldRe = regexp.MustCompile(`(?i)(pwd|pass|ssn|phone|id_card)`)

const onlineLogsSchema = `CREATE TABLE IF NOT EXISTS online_logs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id      TEXT NOT NULL,
    site_domain     TEXT,
    page_url        TEXT,
    ax_tree_hash    TEXT,
    node_features   TEXT,
    snapshot_blob   BLOB,
    snapshot_uri    TEXT,
    snapshot_size   INTEGER,
    confidence      REAL,
    decision        TEXT,
    action_taken    TEXT,
    success         INTEGER,
    latency_ms      INTEGER,
    context_state   TEXT,
    decision_trace  TEXT,
    pii_masked      INTEGER DEFAULT 1,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);`

// LogEntry 对应 online_logs 一行。
type LogEntry struct {
	RequestID     string
	SiteDomain    string
	PageURL       string
	AXTreeHash    string
	NodeFeatures  string
	SnapshotBlob  []byte
	SnapshotURI   string
	SnapshotSize  int64
	Confidence    float64
	Decision      string
	ActionTaken   string
	Success       bool
	LatencyMs     int64
	ContextState  string
	DecisionTrace string
	PIIContains   bool
	CreatedAt     time.Time
}

// LogStore 是 online_logs 的 SQLite 落库。
type LogStore struct {
	db *sql.DB
}

// NewLogStore 打开（或创建）online_logs SQLite 库。
func NewLogStore(path string) (*LogStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(onlineLogsSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &LogStore{db: db}, nil
}

// Close 释放数据库连接。
func (s *LogStore) Close() error {
	return s.db.Close()
}

// Record 写入一条动作日志（调用方已保证内容 PII 脱敏）。
func (s *LogStore) Record(e LogEntry) error {
	_, err := s.db.Exec(`
		INSERT INTO online_logs (
			request_id, site_domain, page_url, ax_tree_hash, node_features,
			snapshot_blob, snapshot_uri, snapshot_size, confidence, decision,
			action_taken, success, latency_ms, context_state, decision_trace,
			pii_masked, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RequestID, e.SiteDomain, e.PageURL, e.AXTreeHash, e.NodeFeatures,
		e.SnapshotBlob, e.SnapshotURI, e.SnapshotSize, e.Confidence, e.Decision,
		e.ActionTaken, e.Success, e.LatencyMs, e.ContextState, e.DecisionTrace,
		e.PIIContains, e.CreatedAt.UTC().Format(time.RFC3339),
	)
	return err
}

// QueryByRequestID 按 request_id 查询单条动作日志。
func (s *LogStore) QueryByRequestID(requestID string) (*LogEntry, error) {
	var e LogEntry
	var piiMasked int
	var createdAtStr string
	err := s.db.QueryRow(`
		SELECT request_id, site_domain, page_url, ax_tree_hash, node_features,
		       snapshot_blob, snapshot_uri, snapshot_size, confidence, decision,
		       action_taken, success, latency_ms, context_state, decision_trace,
		       pii_masked, created_at
		FROM online_logs WHERE request_id = ? ORDER BY id DESC LIMIT 1`,
		requestID,
	).Scan(
		&e.RequestID, &e.SiteDomain, &e.PageURL, &e.AXTreeHash, &e.NodeFeatures,
		&e.SnapshotBlob, &e.SnapshotURI, &e.SnapshotSize, &e.Confidence, &e.Decision,
		&e.ActionTaken, &e.Success, &e.LatencyMs, &e.ContextState, &e.DecisionTrace,
		&piiMasked, &createdAtStr,
	)
	if err != nil {
		return nil, err
	}
	e.PIIContains = piiMasked == 1
	t, _ := time.Parse(time.RFC3339, createdAtStr)
	if t.IsZero() {
		t, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)
	}
	e.CreatedAt = t
	return &e, nil
}

// ── PII 脱敏（SEC-01/03） ─────────────────────────────────────────────

// isSensitiveSemantic 判定目标语义是否命中敏感字段名。
func isSensitiveSemantic(semantic string) bool {
	return sensitiveFieldRe.MatchString(semantic)
}

// entryFromAction 从一次成功的 ExecuteAction 构造 online_logs 行。
// node_features / decision_trace 仅保留长度与类型元信息（SEC-03）：
// 不落任何原始节点文本、语义原文或值原文。
func entryFromAction(req *uwspb.ActionIntentRequest, resp *uwspb.ActionResultResponse,
	candidateCount int, latencyMs int64, now time.Time, wasmTruncated bool) LogEntry {
	semantic := req.GetTargetSemantic()
	value := req.GetValue()
	action := req.GetExpectedAction()
	sensitive := isSensitiveSemantic(semantic)

	dt, _ := json.Marshal(map[string]any{
		"intent_id":         req.GetIntentId(),
		"action":            action,
		"candidate_count":   candidateCount,
		"node_text_lengths": []int{},
		"confidence_type":   "float32",
		"wasm_truncated":    wasmTruncated, // 探针截断降级标记（algorithm-kernel.md §5）
	})
	nf, _ := json.Marshal([]any{
		map[string]any{
			"candidate_count": candidateCount,
			"confidence":      resp.GetConfidence(),
		},
	})
	snapSemantic := semantic
	// 快照永不落 value 原文（R2）：无论语义标签是否命中敏感词，用户输入一律
	// 以 [REDACTED] 落库——手机号/证件号等 PII 可能出现在任意字段名（如
	// username 填手机号），语义关键词匹配无法覆盖。spec/tdd-checklist SEC-03
	// 「snapshot_blob 中密码/手机号/证件号明文一律不得存在」由此获得结构性保证。
	snapValue := value
	if snapValue != "" {
		snapValue = redacted
	}
	if sensitive {
		snapSemantic = redacted
	}
	snap, _ := json.Marshal(map[string]any{
		"semantic":      snapSemantic,
		"value":         snapValue,
		"action":        action,
		"success":       resp.GetSuccess(),
		"confidence":    resp.GetConfidence(),
		"context_state": resp.GetContextState().String(),
	})

	return LogEntry{
		RequestID:     req.GetIntentId(),
		NodeFeatures:  string(nf),
		SnapshotBlob:  snap,
		SnapshotSize:  int64(len(snap)),
		Confidence:    float64(resp.GetConfidence()),
		Decision:      "direct",
		ActionTaken:   action,
		Success:       resp.GetSuccess(),
		LatencyMs:     latencyMs,
		ContextState:  resp.GetContextState().String(),
		DecisionTrace: string(dt),
		PIIContains:   sensitive || value != "",
		CreatedAt:     now,
	}
}
