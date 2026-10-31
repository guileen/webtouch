package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uws/pkg/browser"
	"uws/pkg/matcher"
	uwspb "uws/proto/gen/go/proto/v1"
)

// ── 测试工具 ─────────────────────────────────────────────────────

func episodeRawLines(t *testing.T, dir string) ([]string, string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("Episode 目录 %s 下没有 .jsonl", dir)
	}
	var lines []string
	var all strings.Builder
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		all.Write(b)
		for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.TrimSpace(ln) != "" {
				lines = append(lines, ln)
			}
		}
	}
	return lines, all.String()
}

func episodeDecode(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("Episode 行不是合法 JSON: %v (%s)", err, line)
	}
	return m
}

func episodeObj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("字段 %q 不是对象: %v", key, m[key])
	}
	return v
}

func episodeArr(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok {
		t.Fatalf("字段 %q 不是数组: %v", key, m[key])
	}
	return v
}

func episodeStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("字段 %q 不是字符串: %v", key, m[key])
	}
	return v
}

// ── 第二批：页面侧候选投影与 is_ground_truth（spec/algorithm-kernel.md §5.0.13）──

func TestEpisodeSanitizeWideNumericRule(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     string
		hit      bool
		mustKeep string
	}{
		{name: "wide-phone", in: "订单号 8613800138000 已提交", want: "订单号 [REDACTED:PHONE] 已提交", hit: true},
		{name: "wide-plus", in: "客服 +8613800138000", want: "客服 [REDACTED:PHONE]", hit: true},
		{name: "decimal-not-touched", in: "版本 3.14159265358979", want: "版本 3.14159265358979", hit: false},
		{name: "uuid-not-partially-eaten", in: "id 3f2504e0-4f89-11d3-9a0c-0305e82c3301", want: "id [REDACTED:UUID]", hit: true},
		{name: "short-digits-not-touched", in: "共 20260925 条", want: "共 20260925 条", hit: false},
		{name: "narrow-phone-still-first", in: "电话 13800138000", want: "电话 [REDACTED:PHONE]", hit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hit := sanitizeEpisodeText(tc.in)
			if hit != tc.hit {
				t.Fatalf("hit=%v, 期望 %v（in=%q）", hit, tc.hit, tc.in)
			}
			if got != tc.want {
				t.Fatalf("sanitize(%q)=%q, 期望 %q", tc.in, got, tc.want)
			}
			if !tc.hit && got != tc.in {
				t.Fatalf("未命中字段的字节被改变: %q → %q", tc.in, got)
			}
		})
	}
}

// §5.0.14 条款 1③：pivot 三段式解析（KB 规则选择器优先 → Tag ＋ Text 退化 → 无 pivot）。
func TestEpisodeSnapshotPivotResolution(t *testing.T) {
	kb := []matcher.Candidate{{
		NodeText: "登录", Tag: "button", Role: "button",
		Matches: []matcher.Match{{Rule: matcher.Rule{Selector: "#login-btn"}}},
	}}
	if got := episodeSnapshotPivot(kb, "登录", nil); got.Selector != "#login-btn" || got.Tag != "button" || got.Text != "登录" {
		t.Fatalf("KB 规则选择器应作 pivot.Selector 并同时带上退化身份，实际 %+v", got)
	}
	fuzzy := episodeSnapshotPivot([]matcher.Candidate{{NodeText: "登录", Tag: "button"}}, "登录", nil)
	if fuzzy.Selector != "" || fuzzy.Tag != "button" || fuzzy.Text != "登录" {
		t.Fatalf("无 KB 选择器时应退化为 Tag ＋ Text，实际 %+v", fuzzy)
	}
	if none := episodeSnapshotPivot([]matcher.Candidate{{NodeText: "登录", Tag: "button"}}, "注册", nil); none != (browser.PageSnapshotPivot{}) {
		t.Fatalf("决策目标不在候选池内且无受体时不得拿别的候选冒充 pivot，实际 %+v", none)
	}
	// §5.0.14 决议 0/1：物理受体派发点入枢轴（HasPoint/PointX/PointY），补充身份仍随行；
	// 目标不在候选池内时**只**靠受体点（这正是「无候选池路径」的救活通道）。
	rec := &browser.PreDispatchReceptor{Found: true, X: 140, Y: 212, Sel: "form > button", Tag: "button", Text: "登录"}
	withRec := episodeSnapshotPivot(kb, "登录", rec)
	if !withRec.HasPoint || withRec.PointX != 140 || withRec.PointY != 212 {
		t.Fatalf("受体派发点必须进枢轴，实际 %+v", withRec)
	}
	if withRec.Selector != "#login-btn" || withRec.Tag != "button" || withRec.Text != "登录" {
		t.Fatalf("受体在时补充身份仍应随行（页内按「有受体即用受体」取舍），实际 %+v", withRec)
	}
	onlyRec := episodeSnapshotPivot(nil, "登录", rec)
	if !onlyRec.HasPoint || onlyRec.PointX != 140 || onlyRec.PointY != 212 || onlyRec.Selector != "" {
		t.Fatalf("候选池为空时枢轴应仅由受体点构成，实际 %+v", onlyRec)
	}
	if notFound := episodeSnapshotPivot(nil, "登录", &browser.PreDispatchReceptor{}); notFound.HasPoint {
		t.Fatalf("受体未解析（Found=false）时不得伪造派发点，实际 %+v", notFound)
	}
}

// §5.0.14 条款 1①/1④：导航型 click 的候选与 GT 亦取自**派发前**页面状态；
// `page_projection_after_nav` 已废弃（不再产生），每动作恰一次采样。
func TestEpisodePreDispatchSamplingSurvivesNavigation(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), EpisodeDir: dir})
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "第4集", Confidence: 0.95, Tag: "li", Role: "link"}})
	sess.SetPageURL("https://serp.example/search")
	sess.SetNavOnAction("https://detail.example/product/1")
	// §5.0.14 决议 0：物理层在派发前回传受体（本替身脚本化）；采样由此驱动。
	sess.SetPreDispatchReceptor(browser.PreDispatchReceptor{
		Found: true, X: 60, Y: 40, Sel: "ul > li:nth-of-type(4)", Tag: "li", Text: "第4集",
	})
	sess.SetPageSnapshot(browser.PageSnapshot{
		Nodes: []browser.PageSnapshotNode{
			{Tag: "li", Role: "link", Selector: "ul > li:nth-of-type(4)", X: 10, Y: 20, W: 100, H: 40, Text: "第4集"},
			{Tag: "li", Role: "link", Selector: "ul > li:nth-of-type(1)", X: 10, Y: 200, W: 100, H: 40, Text: "第1集"},
		},
		GroundTruthIndex: 0,
		Skeleton:         []browser.PageSnapshotNeighbor{{Rel: "parent", Tag: "ul", Role: "list"}},
	})
	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "ep-predisp", TargetSemantic: "第4集", ExpectedAction: "click",
	})
	if err != nil {
		t.Fatalf("click 应成功: %v", err)
	}
	if !resp.GetUrlChanged() {
		t.Fatal("本例应发生导航（否则测不到 after_nav 退化）")
	}
	lines, raw := episodeRawLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("期望 1 条 Episode, 实际 %d: %q", len(lines), raw)
	}
	ep := episodeDecode(t, lines[0])
	prod := episodeObj(t, ep, "production")
	if got := episodeStr(t, prod, "candidates_source"); got != EpisodeCandidatesPageProjection {
		t.Fatalf("导航型 click 的 candidates_source=%q, 期望 %q（派发前采样；after_nav 已废弃）",
			got, EpisodeCandidatesPageProjection)
	}
	cands := episodeArr(t, ep, "candidates")
	if len(cands) != 2 {
		t.Fatalf("导航型 click 应保留派发前的 2 条候选，实际 %d", len(cands))
	}
	gt := 0
	for i, item := range cands {
		c := item.(map[string]any)
		if v, _ := c["is_ground_truth"].(bool); v {
			gt++
			if i != 0 {
				t.Fatalf("GT 应为页内判据给出的候选 0，实际 %d", i)
			}
		}
	}
	if gt != 1 {
		t.Fatalf("is_ground_truth 必须恰一条，实际 %d", gt)
	}
	if n := sess.PageSnapshotCalls(); n != 1 {
		t.Fatalf("每动作恰一次派发前采样，实际 %d 次", n)
	}
	if p := sess.LastPageSnapshotPivot(); p.Tag != "li" || p.Text != "第4集" || p.Selector != "" {
		t.Fatalf("无 KB 规则时枢轴应退化为 Tag ＋ Text，实际 %+v", p)
	}
	// §5.0.14 决议 0：枢轴须带物理受体派发点（GT 的唯一合法锚点）。
	if p := sess.LastPageSnapshotPivot(); !p.HasPoint || p.PointX != 60 || p.PointY != 40 {
		t.Fatalf("枢轴应带物理受体派发点 (60,40)，实际 %+v", p)
	}
	if rec := sess.LastPreDispatchReceptor(); !rec.Found || rec.X != 60 || rec.Y != 40 {
		t.Fatalf("替身应记录到派发前回传的受体，实际 %+v", rec)
	}
}

// §5.0.14 条款 2：页内三级判据无法判定（gt_index = -1 / 越界）时 GT 必须全 false，不得回填。
func TestEpisodeGroundTruthUnknownStaysFalse(t *testing.T) {
	for _, tc := range []struct {
		name string
		idx  int
	}{
		{name: "无法判定", idx: browser.PageSnapshotGroundTruthUnknown},
		{name: "越界防御", idx: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			env := newTestEnvWithOptions(t, Options{Keys: testKeys(), EpisodeDir: dir})
			initCtx(t, env.client, "coordinator-key")
			sess := simSessionOf(t, env.srv)
			sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95, Tag: "button", Role: "button"}})
			sess.SetPreDispatchReceptor(browser.PreDispatchReceptor{Found: true, X: 140, Y: 212, Tag: "button", Text: "登录"})
			sess.SetPageSnapshot(browser.PageSnapshot{
				Nodes: []browser.PageSnapshotNode{
					{Tag: "button", Role: "button", Selector: "form > button:nth-of-type(1)", X: 100, Y: 200, W: 80, H: 24, Text: "登录"},
					{Tag: "a", Role: "link", Selector: "nav > a:nth-of-type(1)", X: 10, Y: 10, W: 40, H: 16, Text: "首页"},
				},
				GroundTruthIndex: tc.idx,
			})
			if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
				IntentId: "ep-gt-unknown", TargetSemantic: "登录", ExpectedAction: "click",
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
				t.Fatalf("页面侧候选数=%d, 期望 2", len(cands))
			}
			for i, item := range cands {
				if v, _ := item.(map[string]any)["is_ground_truth"].(bool); v {
					t.Fatalf("无法判定时候选 %d 不得被标为 ground truth（不编造 gold）", i)
				}
			}
			prod := episodeObj(t, ep, "production")
			if got := episodeStr(t, prod, "candidates_source"); got != EpisodeCandidatesPageProjection {
				t.Fatalf("candidates_source=%q, 期望 %q", got, EpisodeCandidatesPageProjection)
			}
		})
	}
}

func TestEpisodeProjectsPageCandidates(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), EpisodeDir: dir})
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95, Tag: "button", Role: "button"}})
	sess.SetPageURL("https://example.com/login?token=abc123def456")
	sess.SetPreDispatchReceptor(browser.PreDispatchReceptor{
		Found: true, X: 140, Y: 212, Sel: "form > button:nth-of-type(1)", Tag: "button", Text: "登录",
	})
	sess.SetPageSnapshot(browser.PageSnapshot{
		Nodes: []browser.PageSnapshotNode{
			{Tag: "button", Role: "button", Selector: "form > button:nth-of-type(1)",
				X: 100, Y: 200, W: 80, H: 24, Text: "登录"},
			{Tag: "a", Role: "link", Selector: "nav > a:nth-of-type(1)",
				X: 10, Y: 10, W: 40, H: 16, Text: "首页"},
		},
		Skeleton: []browser.PageSnapshotNeighbor{
			{Rel: "parent", Tag: "form", Role: ""},
			{Rel: "sibling", Tag: "a", Role: "link", Text: "忘记密码"},
		},
		// §5.0.14 条款 2：GT 由页内三级判据判定并回带下标（本替身直接脚本化该结果）。
		GroundTruthIndex: 0,
	})
	sess.SetLastHit(browser.HitElement{
		Selector: "form > button:nth-of-type(1)", Tag: "button", Text: "登录", X: 140, Y: 212, W: 80, H: 24,
	})
	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "ep-proj", TargetSemantic: "登录", ExpectedAction: "click",
	}); err != nil {
		t.Fatalf("动作失败: %v", err)
	}
	rawLines, rawText := episodeRawLines(t, dir)
	if len(rawLines) != 1 {
		t.Fatalf("期望 1 条 Episode, 实际 %d: %q", len(rawLines), rawText)
	}
	ep := episodeDecode(t, rawLines[0])
	cands := episodeArr(t, ep, "candidates")
	if len(cands) != 2 {
		t.Fatalf("页面侧候选数=%d, 期望 2（不得为 null/空）", len(cands))
	}
	gt := 0
	for i, raw := range cands {
		c := raw.(map[string]any)
		if v, _ := c["is_ground_truth"].(bool); v {
			gt++
			if i != 0 {
				t.Fatalf("ground truth 应为页内判据给出的候选 0，实际 %d", i)
			}
			sk := episodeObj(t, c, "skeleton")
			if got := episodeStr(t, sk, "source"); got != "page_snapshot_1hop" {
				t.Fatalf("skeleton.source=%q, 期望 page_snapshot_1hop", got)
			}
			if n := len(episodeArr(t, sk, "neighbors")); n != 2 {
				t.Fatalf("1 阶骨架节点数=%d, 期望 2", n)
			}
		}
		if _, ok := c["interactive"].(bool); !ok {
			t.Fatalf("候选 %d 缺 interactive 字段: %v", i, c)
		}
	}
	if gt != 1 {
		t.Fatalf("is_ground_truth 必须恰一条，实际 %d", gt)
	}
	prod := episodeObj(t, ep, "production")
	if got := episodeStr(t, prod, "candidates_source"); got != EpisodeCandidatesPageProjection {
		t.Fatalf("candidates_source=%q, 期望 %q", got, EpisodeCandidatesPageProjection)
	}
	if got, _ := prod["page_candidate_count"].(float64); int(got) != 2 {
		t.Fatalf("page_candidate_count=%v, 期望 2", prod["page_candidate_count"])
	}
	if got, _ := prod["candidates_truncated"].(bool); got {
		t.Fatal("2 条候选不应标记截断")
	}
	page := episodeObj(t, ep, "page")
	// §5.0.13 条款 4⑥：url_before 只留 scheme://host/path，url_hash 只落哈希。
	if got := episodeStr(t, page, "url_before"); got != "https://example.com/login" {
		t.Fatalf("page.url_before=%q, 期望去掉查询串", got)
	}
	if got := episodeStr(t, page, "url_hash"); len(got) != 12 {
		t.Fatalf("url_hash=%q, 期望 12 位身份哈希", got)
	}
	if strings.Contains(rawText, "abc123def456") {
		t.Fatalf("URL 查询串不得落盘: %s", rawText)
	}
}
