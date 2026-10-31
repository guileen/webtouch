// 决策时点候选池与排序位序落盘契约单测（spec/algorithm-kernel.md §5.0.15，`T-30` 数据前置）。
//
// 覆盖：决策序＝confidence 降序稳定排序（同位序保持池内输入序）；rank 0 基；
// is_selected 唯一；空池 / 未评估（skipped）与「评估后为空」不同号；K 截断；
// 落盘为 additive 字段且不改动入参候选池。
package engine

import (
	"reflect"
	"testing"
	"uws/pkg/browser"
	"uws/pkg/matcher"
	uwspb "uws/proto/gen/go/proto/v1"
)

func newDecisionPoolRecorder() *episodeRecorder {
	return &episodeRecorder{redactFields: map[string]bool{}}
}

func TestDecisionPoolRankOrderIsConfidenceDescStable(t *testing.T) {
	r := newDecisionPoolRecorder()
	ep := &Episode{}
	src := []matcher.Candidate{
		{NodeText: "登录", Stage: "L1", Confidence: 0.50, Source: "page_probe"},
		{NodeText: "注册", Stage: "L1", Confidence: 0.95, Source: "page_probe"},
		{NodeText: "帮助", Stage: "L2", Confidence: 0.70, Source: "page_l2_kb"},
		// 与上一条置信度相同：稳定排序须保持池内输入序（帮助在前）。
		{NodeText: "忘记密码", Stage: "L1", Confidence: 0.70, Source: "page_onnx"},
	}
	before := append([]matcher.Candidate(nil), src...)

	source, count, truncated := r.recordDecisionPool(ep, episodeOutcome{
		Candidates: src, PoolEvaluated: true, ExecSemantic: "帮助",
	})

	if source != "matcher" || count != 4 || truncated {
		t.Fatalf("source=%q count=%d truncated=%v, 期望 matcher/4/false", source, count, truncated)
	}
	wantText := []string{"注册", "帮助", "忘记密码", "登录"}
	for i, want := range wantText {
		got := ep.DecisionPool[i]
		if got.Rank != i || got.Text != want {
			t.Fatalf("rank=%d text=%q, 期望 rank=%d text=%q（决策序）", got.Rank, got.Text, i, want)
		}
	}
	if ep.DecisionPool[0].Confidence != 0.95 || ep.DecisionPool[0].IsSelected {
		t.Fatalf("rank0 应为 0.95 且未被选中（派发目标是 rank1）：%+v", ep.DecisionPool[0])
	}
	if got := ep.DecisionPool[1].Source; got != "page_l2_kb" {
		t.Fatalf("rank1.source=%q, 期望 page_l2_kb（来源在入池落点标注）", got)
	}
	if !ep.DecisionPool[1].IsSelected {
		t.Fatalf("rank1（帮助＝本次派发目标）应被标记选中：%+v", ep.DecisionPool[1])
	}
	selCount := 0
	for _, c := range ep.DecisionPool {
		if c.IsSelected {
			selCount++
		}
	}
	if selCount != 1 {
		t.Fatalf("is_selected 为真的条目=%d, 期望恰 1（唯一派发目标）", selCount)
	}
	// 入参池不得被改写（纯增量观测）。
	for i := range src {
		if !reflect.DeepEqual(src[i], before[i]) {
			t.Fatalf("入参候选池被改写：src[%d]=%+v, 期望 %+v", i, src[i], before[i])
		}
	}
}

func TestDecisionPoolEmptyArrayWhenPoolNotEvaluated(t *testing.T) {
	r := newDecisionPoolRecorder()
	ep := &Episode{}
	source, count, truncated := r.recordDecisionPool(ep, episodeOutcome{PoolEvaluated: false})
	if source != "skipped" || count != 0 || truncated {
		t.Fatalf("source=%q count=%d truncated=%v, 期望 skipped/0/false", source, count, truncated)
	}
	if ep.DecisionPool == nil {
		t.Fatal("decision_pool 必须落空数组而非 null（§5.0.15 条款 1.1）")
	}
	if len(ep.DecisionPool) != 0 {
		t.Fatalf("未评估候选池时不得有决策池条目: %+v", ep.DecisionPool)
	}
}

func TestDecisionPoolEvaluatedButEmptyStaysDistinguishable(t *testing.T) {
	r := newDecisionPoolRecorder()
	ep := &Episode{}
	source, count, _ := r.recordDecisionPool(ep, episodeOutcome{PoolEvaluated: true})
	if source != "matcher" || count != 0 || len(ep.DecisionPool) != 0 {
		t.Fatalf("评估后空池：source=%q count=%d len=%d, 期望 matcher/0/0（与 skipped 不同号）",
			source, count, len(ep.DecisionPool))
	}
}

func TestDecisionPoolTruncatesAtCap(t *testing.T) {
	r := newDecisionPoolRecorder()
	ep := &Episode{}
	src := make([]matcher.Candidate, 0, decisionPoolMax+6)
	for i := 0; i < decisionPoolMax+6; i++ {
		src = append(src, matcher.Candidate{
			NodeText: "候选", Stage: "L1", Confidence: float64(i) / 100, Source: "page_probe",
		})
	}
	source, count, truncated := r.recordDecisionPool(ep, episodeOutcome{Candidates: src, PoolEvaluated: true})
	if source != "matcher" || !truncated || count != decisionPoolMax {
		t.Fatalf("source=%q truncated=%v count=%d, 期望 matcher/true/%d", source, truncated, count, decisionPoolMax)
	}
	if len(ep.DecisionPool) != decisionPoolMax || ep.DecisionPool[0].Rank != 0 || ep.DecisionPool[decisionPoolMax-1].Rank != decisionPoolMax-1 {
		t.Fatalf("截断后位序不连续: len=%d", len(ep.DecisionPool))
	}
	// 置信度降序：截断保留的应是最高分那批（0.69 → 0.06）。
	if got := ep.DecisionPool[0].Confidence; got != 0.69 {
		t.Fatalf("截断后 rank0.confidence=%v, 期望 0.69（按决策序取前 K）", got)
	}
}

func TestEpisodeDecisionPoolLandsInJSON(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), EpisodeDir: dir})
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95, Tag: "button", Role: "button"}})
	sess.SetPageURL("https://example.com/login")
	sess.SetLastHit(browser.HitElement{Selector: "form > button", Tag: "button", Text: "登录"})
	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "ep-pool-1", TargetSemantic: "登录", ExpectedAction: "click",
	}); err != nil {
		t.Fatalf("动作应成功: %v", err)
	}

	rawLines, _ := episodeRawLines(t, dir)
	ep := episodeDecode(t, rawLines[0])
	pool := episodeArr(t, ep, "decision_pool")
	if len(pool) != 1 {
		t.Fatalf("decision_pool 条数=%d, 期望 1", len(pool))
	}
	c0 := pool[0].(map[string]any)
	if got, ok := c0["rank"].(float64); !ok || int(got) != 0 {
		t.Fatalf("decision_pool[0].rank=%v, 期望 0", c0["rank"])
	}
	if got := episodeStr(t, c0, "source"); got != "page_probe" {
		t.Fatalf("decision_pool[0].source=%q, 期望 page_probe", got)
	}
	if got, ok := c0["is_selected"].(bool); !ok || !got {
		t.Fatalf("decision_pool[0].is_selected=%v, 期望 true（本条即派发目标）", c0["is_selected"])
	}
	if _, ok := c0["is_ground_truth"]; ok {
		t.Fatal("decision_pool 不得携带 is_ground_truth（GT 单一事实源在 candidates[]）")
	}
	prod := episodeObj(t, ep, "production")
	if got := episodeStr(t, prod, "decision_pool_source"); got != "matcher" {
		t.Fatalf("production.decision_pool_source=%q, 期望 matcher", got)
	}
	if got, ok := prod["decision_pool_count"].(float64); !ok || int(got) != 1 {
		t.Fatalf("production.decision_pool_count=%v, 期望 1", prod["decision_pool_count"])
	}
	if got, ok := prod["decision_pool_truncated"].(bool); !ok || got {
		t.Fatalf("production.decision_pool_truncated=%v, 期望 false", prod["decision_pool_truncated"])
	}
}
