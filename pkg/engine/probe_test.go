// WASM 探针截断信号接线测试（USER_ACTION #14）：
//   - 引擎累计 uws_wasm_truncation_total（Server.WasmTruncationTotal）；
//   - decision_trace 带 wasm_truncated 降级标记（online_logs，SEC-03 仅元信息）。
package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"
)

func TestWasmTruncationCounter(t *testing.T) {
	env := newTestEnv(t)
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95}})

	// 截断动作：计数器 +1。
	sess.SetProbeStats(browser.ProbeStats{Kept: 8000, Truncated: true, Overflow: 12000, NeedsFallback: true})
	if _, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "wasm-trunc-1", TargetSemantic: "登录", ExpectedAction: "click",
	}); err != nil {
		t.Fatalf("截断动作应成功: %v", err)
	}
	if got := env.srv.WasmTruncationTotal(); got != 1 {
		t.Fatalf("截断计数器应 +1, got %d", got)
	}

	// 未截断动作：计数器不再增长。
	sess.SetProbeStats(browser.ProbeStats{Kept: 10})
	if _, err := env.client.ExecuteAction(ctx, &uwspb.ActionIntentRequest{
		IntentId: "wasm-trunc-2", TargetSemantic: "登录", ExpectedAction: "click",
	}); err != nil {
		t.Fatalf("未截断动作应成功: %v", err)
	}
	if got := env.srv.WasmTruncationTotal(); got != 1 {
		t.Fatalf("未截断不应累计, got %d", got)
	}
}

func TestWasmTruncationDecisionTrace(t *testing.T) {
	req := &uwspb.ActionIntentRequest{IntentId: "trace-1", TargetSemantic: "登录", ExpectedAction: "click"}
	resp := &uwspb.ActionResultResponse{
		IntentId: "trace-1", Success: true, Confidence: 0.95,
		LocalVerification: &uwspb.LocalVerification{},
	}

	e := entryFromAction(req, resp, 5, 12, time.Now(), true)
	var dt map[string]any
	if err := json.Unmarshal([]byte(e.DecisionTrace), &dt); err != nil {
		t.Fatalf("decision_trace 非 JSON: %v", err)
	}
	if v, ok := dt["wasm_truncated"].(bool); !ok || !v {
		t.Fatalf("截断动作应标记 wasm_truncated=true: %+v", dt)
	}

	e2 := entryFromAction(req, resp, 5, 12, time.Now(), false)
	if !strings.Contains(e2.DecisionTrace, `"wasm_truncated":false`) {
		t.Fatalf("未截断动作应标记 false: %s", e2.DecisionTrace)
	}
}
