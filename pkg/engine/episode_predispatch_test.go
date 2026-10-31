package engine

import (
	"context"
	"testing"
	"time"
	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"
)

// ── §5.0.14 决议 0/6：物理受体回传驱动派发前采样 ──────────────────────────

// noReceptorSinkSession 包裹模拟会话但**遮蔽**其「派发前受体回传」能力：Go 方法集按最外层
// 同名方法判优——本类型声明了不同签名的同名方法 ⇒ 不再满足 browser.PreDispatchReceptorSink，
// 精确模拟「会话不具备该能力」（旧会话 / 第三方实现）时引擎必须退回旧点采样。
type noReceptorSinkSession struct {
	*SimCDPSession
}

// SetPreDispatchReceptorSink 遮蔽内层同名方法（签名不同 ⇒ 能力探测失败）。
func (s *noReceptorSinkSession) SetPreDispatchReceptorSink(struct{}) {}

// receptorLessFactory 为每个 BrowserContext 造一个**无受体回传能力**的会话。
type receptorLessFactory struct {
	sim *SimCDPSession
}

// Create 实现 browser.SessionFactory。
func (f *receptorLessFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	f.sim = NewSimCDPSession("sim-no-sink", cfg)
	return &noReceptorSinkSession{SimCDPSession: f.sim}, nil
}

// §5.0.14 决议 0/6：① 受体派发点被采样消费（与物理层回传一致）；② 骨架取自 GT 命中元素；
// ④ 幂等——同一 Episode 即使受体被回传两次（真实恢复链会重试）也只采样一次。
// TestEpisodeCandidateSkeletonSymmetric 断言 §5.0.13 条款 7 的对称性硬线（CON-214 伪特征硬线）：
// **全部候选**（含非 GT 负样本）各携带**自身** 1 阶骨架，而非仅 GT 挂载（负样本骨架全空）。
func TestEpisodeCandidateSkeletonSymmetric(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), EpisodeDir: dir})
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95, Tag: "button", Role: "button"}})
	sess.SetPageURL("https://example.com/login")
	sess.SetPreDispatchReceptor(browser.PreDispatchReceptor{
		Found: true, X: 140, Y: 212, Sel: "form > button:nth-of-type(1)", Tag: "button", Text: "登录",
	})
	sess.SetPageSnapshot(browser.PageSnapshot{
		Nodes: []browser.PageSnapshotNode{
			{Tag: "button", Role: "button", Selector: "form > button:nth-of-type(1)",
				X: 100, Y: 200, W: 80, H: 24, Text: "登录",
				Neighbors: []browser.PageSnapshotNeighbor{{Rel: "parent", Tag: "form", Role: ""}}},
			{Tag: "a", Role: "link", Selector: "nav > a:nth-of-type(1)",
				X: 10, Y: 10, W: 40, H: 16, Text: "首页",
				Neighbors: []browser.PageSnapshotNeighbor{
					{Rel: "parent", Tag: "nav", Role: ""},
					{Rel: "sibling", Tag: "a", Role: "link", Text: "注册"},
				}},
		},
		GroundTruthIndex: 0,
	})
	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "ep-sym-skel", TargetSemantic: "登录", ExpectedAction: "click",
	}); err != nil {
		t.Fatalf("动作失败: %v", err)
	}
	lines, raw := episodeRawLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("期望 1 条 Episode, 实际 %d: %q", len(lines), raw)
	}
	ep := episodeDecode(t, lines[0])
	cands := episodeArr(t, ep, "candidates")
	if len(cands) != 2 {
		t.Fatalf("候选数=%d, 期望 2", len(cands))
	}
	gtSeen := 0
	for i, rawC := range cands {
		c := rawC.(map[string]any)
		gt, _ := c["is_ground_truth"].(bool)
		if gt {
			gtSeen++
		}
		sk := episodeObj(t, c, "skeleton")
		if got := episodeStr(t, sk, "source"); got != "page_snapshot_1hop" {
			t.Fatalf("候选 %d（gt=%v）skeleton.source=%q, 期望 page_snapshot_1hop（负样本同样必须挂骨架）",
				i, gt, got)
		}
		n := len(episodeArr(t, sk, "neighbors"))
		if n == 0 {
			t.Fatalf("候选 %d（gt=%v）骨架为空 ⇒ 违反 §5.0.13 条款 7 对称性硬线（CON-214）", i, gt)
		}
		if n > 8 {
			t.Fatalf("候选 %d 邻居数=%d, 超过每候选上限 8", i, n)
		}
		for _, rawNb := range episodeArr(t, sk, "neighbors") {
			nb := rawNb.(map[string]any)
			if got := episodeStr(t, nb, "class_hash"); got != "" {
				t.Fatalf("候选 %d 邻居 class_hash=%q, 必须恒空（条款 3④ 丢弃全部 class）", i, got)
			}
		}
	}
	if gtSeen != 1 {
		t.Fatalf("is_ground_truth 必须恰一条, 实际 %d", gtSeen)
	}
	if n := len(episodeArr(t, episodeObj(t, cands[1].(map[string]any), "skeleton"), "neighbors")); n != 2 {
		t.Fatalf("非 GT 候选（下标 1）邻居数=%d, 期望 2（对称挂载）", n)
	}
}

func TestEpisodePreDispatchReceptorDrivesSampling(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), EpisodeDir: dir})
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95, Tag: "button", Role: "button"}})
	sess.SetPageSnapshot(browser.PageSnapshot{
		Nodes: []browser.PageSnapshotNode{
			{Tag: "button", Role: "button", Selector: "form > button:nth-of-type(1)", X: 100, Y: 200, W: 80, H: 24, Text: "登录"},
			{Tag: "a", Role: "link", Selector: "nav > a:nth-of-type(1)", X: 10, Y: 10, W: 40, H: 16, Text: "首页"},
		},
		GroundTruthIndex: 0,
		// 骨架由页内脚本从 **GT 命中元素**（此处即候选 0）取 1 阶。
		Skeleton: []browser.PageSnapshotNeighbor{
			{Rel: "parent", Tag: "form", Role: ""},
			{Rel: "sibling", Tag: "a", Role: "link", Text: "忘记密码"},
		},
	})
	rec := browser.PreDispatchReceptor{Found: true, X: 140, Y: 212, Sel: "form > button:nth-of-type(1)", Tag: "button", Text: "登录"}
	// 两次回传：真实恢复链（clickWithRecovery ≤3 次尝试）会多次回传受体。
	sess.SetPreDispatchReceptor(rec, rec)

	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "ep-receptor", TargetSemantic: "登录", ExpectedAction: "click",
	}); err != nil {
		t.Fatalf("动作失败: %v", err)
	}

	if n := sess.PageSnapshotCalls(); n != 1 {
		t.Fatalf("④ 幂等：受体回传两次也只应采样一次，实际 %d 次", n)
	}
	if n := sess.PreDispatchReceptorCalls(); n != 2 {
		t.Fatalf("替身应回传两次受体（验证幂等槽语义），实际 %d 次", n)
	}
	p := sess.LastPageSnapshotPivot()
	if !p.HasPoint || p.PointX != 140 || p.PointY != 212 {
		t.Fatalf("① 采样枢轴应携带物理受体派发点 (140,212)，实际 %+v", p)
	}
	if rec := sess.LastPreDispatchReceptor(); !rec.Found || rec.X != 140 || rec.Y != 212 || rec.Tag != "button" {
		t.Fatalf("替身应记录到物理层回传的受体，实际 %+v", rec)
	}

	lines, raw := episodeRawLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("期望 1 条 Episode, 实际 %d: %q", len(lines), raw)
	}
	ep := episodeDecode(t, lines[0])
	prod := episodeObj(t, ep, "production")
	if got := episodeStr(t, prod, "candidates_source"); got != EpisodeCandidatesPageProjection {
		t.Fatalf("candidates_source=%q, 期望 %q", got, EpisodeCandidatesPageProjection)
	}
	cands := episodeArr(t, ep, "candidates")
	if len(cands) != 2 {
		t.Fatalf("页面侧候选数=%d, 期望 2", len(cands))
	}
	gt := 0
	for i, item := range cands {
		c := item.(map[string]any)
		if v, _ := c["is_ground_truth"].(bool); v {
			gt++
			if i != 0 {
				t.Fatalf("② GT 应为锚点命中候选 0，实际 %d", i)
			}
			sk := episodeObj(t, c, "skeleton")
			if got := episodeStr(t, sk, "source"); got != "page_snapshot_1hop" {
				t.Fatalf("② 骨架应取自 GT 命中元素，source=%q", got)
			}
			if n := len(episodeArr(t, sk, "neighbors")); n != 2 {
				t.Fatalf("② GT 元素 1 阶骨架节点数=%d, 期望 2", n)
			}
		}
	}
	if gt != 1 {
		t.Fatalf("② is_ground_truth 必须恰一条，实际 %d", gt)
	}
}

// §5.0.14 决议 6：会话**不具备**受体回传能力时，引擎维持旧点采样一次（退回补充身份口径：
// 无受体派发点、GT 全 false、candidates_truncated=true），既有行为不得回退。
func TestEpisodePreDispatchSamplingWithoutReceptorSink(t *testing.T) {
	dir := t.TempDir()
	factory := &receptorLessFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(), EpisodeDir: dir, SessionFactory: factory,
	})
	initCtx(t, env.client, "coordinator-key")
	if factory.sim == nil {
		t.Fatal("会话工厂应已构造过模拟会话")
	}
	sess := factory.sim
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95, Tag: "button", Role: "button"}})
	// 页内无 pivot ⇒ 文档序 ＋ 恒置截断 ＋ GT 无法判定（-1）：本替身脚本化该结果。
	sess.SetPageSnapshot(browser.PageSnapshot{
		Nodes: []browser.PageSnapshotNode{
			{Tag: "button", Role: "button", Selector: "form > button:nth-of-type(1)", X: 100, Y: 200, W: 80, H: 24, Text: "登录"},
			{Tag: "a", Role: "link", Selector: "nav > a:nth-of-type(1)", X: 10, Y: 10, W: 40, H: 16, Text: "首页"},
		},
		Truncated:        true,
		GroundTruthIndex: browser.PageSnapshotGroundTruthUnknown,
	})
	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "ep-no-receptor", TargetSemantic: "登录", ExpectedAction: "click",
	}); err != nil {
		t.Fatalf("动作失败: %v", err)
	}
	if n := sess.PageSnapshotCalls(); n != 1 {
		t.Fatalf("无受体能力会话仍应旧点采样恰一次，实际 %d 次", n)
	}
	if n := sess.PreDispatchReceptorCalls(); n != 0 {
		t.Fatalf("无受体能力会话不得回传受体，实际 %d 次", n)
	}
	if p := sess.LastPageSnapshotPivot(); p.HasPoint {
		t.Fatalf("无受体能力时枢轴不得携带受体派发点，实际 %+v", p)
	} else if p.Tag != "button" || p.Text != "登录" {
		t.Fatalf("无受体能力时应走补充身份（Tag ＋ Text）口径，实际 %+v", p)
	}
	lines, raw := episodeRawLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("期望 1 条 Episode, 实际 %d: %q", len(lines), raw)
	}
	ep := episodeDecode(t, lines[0])
	prod := episodeObj(t, ep, "production")
	if got := episodeStr(t, prod, "candidates_source"); got != EpisodeCandidatesPageProjection {
		t.Fatalf("candidates_source=%q, 期望 %q（采样能力不受受体能力影响）", got, EpisodeCandidatesPageProjection)
	}
	if got, _ := prod["candidates_truncated"].(bool); !got {
		t.Fatal("无 pivot 时 candidates_truncated 必须为 true")
	}
	for i, item := range episodeArr(t, ep, "candidates") {
		if v, _ := item.(map[string]any)["is_ground_truth"].(bool); v {
			t.Fatalf("无 pivot 时候选 %d 不得被标为 ground truth（不编造 gold）", i)
		}
	}
}

// §5.0.14 决议 9（`INT-218②`）候选空集轻量重试：页内快照**成功但候选为 0** 时在同一采样槽内
// 重试（≤2 次 × 退避），给晚水合 SPA 一次机会；仍为空 ⇒ **如实落空集**（不编造候选/GT）；
// 快照**失败**（`ok=false`，与「无能力」同号）**不重试**。重试不改采样时点与唯一性。
func TestEpisodeEmptyCandidateRetry(t *testing.T) {
	oldBackoff := episodeEmptyCandidateBackoff
	episodeEmptyCandidateBackoff = time.Millisecond
	defer func() { episodeEmptyCandidateBackoff = oldBackoff }()

	oneCand := browser.PageSnapshot{
		Nodes: []browser.PageSnapshotNode{
			{Tag: "button", Role: "button", Selector: "#go", X: 10, Y: 10, W: 80, H: 30, Text: "登录"},
		},
		GroundTruthIndex: 0,
	}
	empty := browser.PageSnapshot{}

	run := func(t *testing.T, name string, snaps ...browser.PageSnapshot) (*SimCDPSession, map[string]any) {
		t.Helper()
		dir := t.TempDir()
		env := newTestEnvWithOptions(t, Options{Keys: testKeys(), EpisodeDir: dir})
		initCtx(t, env.client, "coordinator-key")
		sess := simSessionOf(t, env.srv)
		sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95, Tag: "button", Role: "button"}})
		sess.SetPageURL("https://slow-hydrate.example/")
		// §5.0.14 决议 0：受体派发点驱动采样（回调内即采样槽）。
		sess.SetPreDispatchReceptor(browser.PreDispatchReceptor{Found: true, X: 12, Y: 18, Tag: "button", Text: "登录"})
		sess.SetPageSnapshotSequence(snaps...)
		if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
			IntentId: "ep-" + name, TargetSemantic: "登录", ExpectedAction: "click",
		}); err != nil {
			t.Fatalf("click 应成功: %v", err)
		}
		lines, raw := episodeRawLines(t, dir)
		if len(lines) != 1 {
			t.Fatalf("期望 1 条 Episode, 实际 %d: %q", len(lines), raw)
		}
		return sess, episodeDecode(t, lines[0])
	}

	// ① 首次空集、重试取到候选：重试生效，Episode 仍为页面投影且候选非空。
	sess, ep := run(t, "empty-retry-hit", empty, oneCand)
	prod := episodeObj(t, ep, "production")
	if got := episodeStr(t, prod, "candidates_source"); got != EpisodeCandidatesPageProjection {
		t.Fatalf("candidates_source=%q, 期望 %q", got, EpisodeCandidatesPageProjection)
	}
	if n := len(episodeArr(t, ep, "candidates")); n != 1 {
		t.Fatalf("重试后应取到 1 条候选，实际 %d", n)
	}
	if n := sess.PageSnapshotCalls(); n != 2 {
		t.Fatalf("空集后应恰重试 1 次（共 2 次页内调用），实际 %d", n)
	}

	// ② 三次全空：重试到上界（1＋2）后**如实落空集**，不编造候选。
	sess, ep = run(t, "empty-retry-exhausted", empty, empty, empty)
	prod = episodeObj(t, ep, "production")
	if got := episodeStr(t, prod, "candidates_source"); got != EpisodeCandidatesPageProjection {
		t.Fatalf("candidates_source=%q, 期望 %q（采样成功但空集）", got, EpisodeCandidatesPageProjection)
	}
	if got, ok := ep["candidates"]; !ok || got != nil {
		t.Fatalf("全空应如实落空集（null），实际 %v", got)
	}
	if n := sess.PageSnapshotCalls(); n != 3 {
		t.Fatalf("上界应为 1＋2 次重试＝3 次页内调用，实际 %d", n)
	}

	// ③ 快照失败（未脚本化 ⇒ ok=false）：**不得**重试，退回候选池口径（engine_pool）。
	sess, ep = run(t, "snapshot-failure-no-retry")
	prod = episodeObj(t, ep, "production")
	if got := episodeStr(t, prod, "candidates_source"); got != EpisodeCandidatesEnginePool {
		t.Fatalf("快照失败应退回 %q，实际 %q", EpisodeCandidatesEnginePool, got)
	}
	if n := sess.PageSnapshotCalls(); n != 1 {
		t.Fatalf("快照失败不得重试（应恰 1 次页内调用），实际 %d", n)
	}
}
