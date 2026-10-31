// online_logs 落库与 PII 脱敏测试（TDD-SEC-01/03 实现层：
//
//	SEC-01 = 敏感字段值写入前替换 [REDACTED] + pii_masked=true；
//	SEC-03 = node_features/snapshot/decision_trace 全树无明文，仅长度/类型元信息）。
package engine

import (
	"strings"
	"testing"
	"time"

	uwspb "uws/proto/gen/go/proto/v1"
)

// ── 脱敏单元 ─────────────────────────────────────────────────────────

func TestIsSensitiveSemantic(t *testing.T) {
	cases := map[string]bool{
		"password_input": true,
		"pwd":            true,
		"pass":           true,
		"ssn_input":      true,
		"phone_input":    true,
		"id_card":        true,
		"username_input": false,
		"login_button":   false,
		"submit_btn":     false,
	}
	for semantic, want := range cases {
		if got := isSensitiveSemantic(semantic); got != want {
			t.Errorf("isSensitiveSemantic(%q) = %v, want %v", semantic, got, want)
		}
	}
}

// ── entryFromAction：敏感字段值不落明文 ────────────────────────────────

func TestEntryFromActionRedactsSensitive(t *testing.T) {
	req := &uwspb.ActionIntentRequest{
		IntentId:       "sec-1",
		TargetSemantic: "password_input",
		ExpectedAction: "input",
		Value:          protoString("mySecret123"),
	}
	resp := &uwspb.ActionResultResponse{
		IntentId:     "sec-1",
		Success:      true,
		Confidence:   0.9,
		ContextState: uwspb.ContextState_STABLE,
	}
	e := entryFromAction(req, resp, 1, 12, time.Now(), false)
	if !e.PIIContains {
		t.Fatal("敏感字段 pii_masked 应为 true")
	}
	blob := string(e.SnapshotBlob)
	if strings.Contains(blob, "mySecret123") || strings.Contains(blob, "password_input") {
		t.Fatalf("快照含敏感明文: %s", blob)
	}
	if !strings.Contains(blob, redacted) {
		t.Fatalf("快照应含 [REDACTED]: %s", blob)
	}
	for name, field := range map[string]string{
		"node_features":  e.NodeFeatures,
		"decision_trace": e.DecisionTrace,
	} {
		if strings.Contains(field, "mySecret123") || strings.Contains(field, "password_input") {
			t.Fatalf("%s 含原始明文: %s", name, field)
		}
	}
}

func TestEntryFromActionNonSensitiveRedactsValue(t *testing.T) {
	// R2 定案：快照永不落 value 原文——手机号/证件号等 PII 可能出现在任意字段名
	// （如 username 填手机号），语义关键词匹配无法覆盖，结构上禁止明文落库。
	req := &uwspb.ActionIntentRequest{
		IntentId:       "sec-2",
		TargetSemantic: "username_input",
		ExpectedAction: "input",
		Value:          protoString("alice"),
	}
	resp := &uwspb.ActionResultResponse{
		IntentId:     "sec-2",
		Success:      true,
		Confidence:   0.9,
		ContextState: uwspb.ContextState_STABLE,
	}
	e := entryFromAction(req, resp, 1, 5, time.Now(), false)
	if !e.PIIContains {
		t.Fatal("含 value 即应 pii_masked=true（快照不落明文）")
	}
	blob := string(e.SnapshotBlob)
	if strings.Contains(blob, "alice") {
		t.Fatalf("快照不得保留 value 原文: %s", blob)
	}
	if !strings.Contains(blob, redacted) {
		t.Fatalf("快照应含 [REDACTED]: %s", blob)
	}
	// 非敏感语义标签本身仍保留（审计可读）。
	if !strings.Contains(blob, "username_input") {
		t.Fatalf("非敏感语义应保留: %s", blob)
	}
}

func TestEntryFromActionPhoneInNonSensitiveFieldRedacted(t *testing.T) {
	req := &uwspb.ActionIntentRequest{
		IntentId:       "sec-3",
		TargetSemantic: "username_input",
		ExpectedAction: "input",
		Value:          protoString("13800138000"),
	}
	resp := &uwspb.ActionResultResponse{
		IntentId:     "sec-3",
		Success:      true,
		Confidence:   0.9,
		ContextState: uwspb.ContextState_STABLE,
	}
	e := entryFromAction(req, resp, 1, 5, time.Now(), false)
	blob := string(e.SnapshotBlob)
	if strings.Contains(blob, "13800138000") {
		t.Fatalf("手机号明文不得落库: %s", blob)
	}
	if !e.PIIContains || !strings.Contains(blob, redacted) {
		t.Fatalf("pii_masked=true 且含 [REDACTED]: pii=%v blob=%s", e.PIIContains, blob)
	}
}

// ── SQLite 落库往返 ──────────────────────────────────────────────────

func TestLogStoreRecordRoundTrip(t *testing.T) {
	ls, err := NewLogStore(":memory:")
	if err != nil {
		t.Fatalf("NewLogStore: %v", err)
	}
	defer ls.Close()

	e := LogEntry{
		RequestID:     "r-1",
		NodeFeatures:  `[{"candidate_count":1,"confidence":0.9}]`,
		SnapshotBlob:  []byte(`{"semantic":"[REDACTED]"}`),
		SnapshotSize:  28,
		Confidence:    0.9,
		Decision:      "direct",
		ActionTaken:   "input",
		Success:       true,
		LatencyMs:     12,
		ContextState:  "STABLE",
		DecisionTrace: `{"intent_id":"r-1"}`,
		PIIContains:   true,
		CreatedAt:     time.Now(),
	}
	if err := ls.Record(e); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var (
		reqID    string
		pii      bool
		blob     []byte
		decision string
	)
	if err := ls.db.QueryRow(
		`SELECT request_id, pii_masked, snapshot_blob, decision_trace FROM online_logs WHERE request_id = ?`,
		"r-1",
	).Scan(&reqID, &pii, &blob, &decision); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if reqID != "r-1" || !pii || string(blob) != `{"semantic":"[REDACTED]"}` || decision != `{"intent_id":"r-1"}` {
		t.Fatalf("往返不一致: req=%s pii=%v blob=%s dt=%s", reqID, pii, blob, decision)
	}
}

func protoString(s string) *string { return &s }
