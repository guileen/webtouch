// 在线 Episode 采集与脱敏契约单测（spec/algorithm-kernel.md §5.0.12）。
//
// 覆盖：append-only JSONL 落盘与重启续写；字段完备（`shadow` 必须存在且可空）；
// a11y 骨架最小充分裁剪（≤32 字符截断 + 原始 rune 长度 + 截断标记）；
// 脱敏硬过滤（命中换占位符、未命中字节不变）；输入值与页面查询串永不落盘。
package engine

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/protobuf/proto"
)

// ── 单元：脱敏与截断 ──────────────────────────────────────────────

func TestEpisodeSanitizeText(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		hit    bool
		leaked string // 命中时必须消失的字面量
	}{
		{name: "email", in: "联系 zhang.san@example.com 谢谢", want: "联系 [REDACTED:EMAIL] 谢谢", hit: true, leaked: "zhang.san@example.com"},
		{name: "cn-mobile", in: "手机 13800138000 找回", want: "手机 [REDACTED:PHONE] 找回", hit: true, leaked: "13800138000"},
		{name: "intl-phone", in: "call +86 138 0013 8000 now", want: "call [REDACTED:PHONE] now", hit: true, leaked: "+86 138 0013 8000"},
		{name: "authorization-header", in: "Authorization: Bearer sk-abcdef123456", want: "[REDACTED:TOKEN]", hit: true, leaked: "sk-abcdef123456"},
		{name: "kv-secret", in: `{"password":"hunter2pw"}`, want: `{[REDACTED:TOKEN]}`, hit: true, leaked: "hunter2pw"},
		{name: "jwt", in: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVP ok", want: "[REDACTED:JWT] ok", hit: true, leaked: "dBjftJeZ4CVP"},
		{name: "uuid", in: "id 3f2504e0-4f89-11d3-9a0c-0305e82c3301 end", want: "id [REDACTED:UUID] end", hit: true, leaked: "3f2504e0-4f89"},
		{name: "prose-untouched", in: "your password must be long", want: "your password must be long", hit: false},
		{name: "plain-untouched", in: "登录 Search 搜索框", want: "登录 Search 搜索框", hit: false},
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
			if tc.leaked != "" && strings.Contains(got, tc.leaked) {
				t.Fatalf("明文泄漏: %q 仍在 %q 中", tc.leaked, got)
			}
		})
	}
}

func TestEpisodeTruncateRunesKeepsOriginalLength(t *testing.T) {
	long := strings.Repeat("搜", 40)
	got, truncated := truncateEpisodeRunes(long, episodeMaxTextRunes)
	if !truncated {
		t.Fatal("40 个 rune 应标记截断")
	}
	if n := len([]rune(got)); n != episodeMaxTextRunes {
		t.Fatalf("截断后 rune 数=%d, 期望 %d", n, episodeMaxTextRunes)
	}
	if got != strings.Repeat("搜", episodeMaxTextRunes) {
		t.Fatalf("截断结果应为前 %d 个 rune", episodeMaxTextRunes)
	}
	short := "登录"
	if out, tr := truncateEpisodeRunes(short, episodeMaxTextRunes); tr || out != short {
		t.Fatalf("短文本不应截断: %q truncated=%v", out, tr)
	}
}

// ── 单元：故障代号提取（§5.0.8 代号表口径） ────────────────────────

func TestEpisodeFailureCodeExtraction(t *testing.T) {
	cases := []struct{ in, want string }{
		{"input_failed:TARGET_NOT_EDITABLE: a \"Skip to search\"", "TARGET_NOT_EDITABLE"},
		{"vlm_landing_unverified:成功出口缺请求动作对应的页面侧证据", "vlm_landing_unverified"},
		{"vlm: action mismatch expected=\"input\" got=\"click\" (VLM_ACTION_MISMATCH)", "VLM_ACTION_MISMATCH"},
		{"TARGET_ROLE_MISMATCH:敏感受体", "TARGET_ROLE_MISMATCH"},
		{"challenge_intercepted", "challenge_intercepted"},
		{"click:登录", ""}, // 成功观测串不编造代号
	}
	for _, tc := range cases {
		if got := episodeFailureCode(tc.in); got != tc.want {
			t.Fatalf("episodeFailureCode(%q)=%q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

// ── 单元：派发状态口径（§5.0.11 条款 3） ──────────────────────────

// errEpisodeTestStub 代表「动作已进入执行层并返回错误」（触发派发状态的错误分支）。
var errEpisodeTestStub = errors.New("indeterminate")

// 物理层「定位阶段失败」表示元素未定位、物理事件从未发出 —— 不得记成「已派发/结果未知」
// （2026-09-25 真机抽检发现：example.com 上 click "More information" 连报
//
//	no_match|url=... 却落盘 dispatched_outcome_unknown，与 §5.0.11 条款 3 相反）。
func TestEpisodeDispatchStateLocateFailureIsNotDispatched(t *testing.T) {
	locateFailures := []struct{ name, observed string }{
		{"定位未命中候选", "no_match|url=https://example.com/"},
		{"求值返回空", "eval_empty|url=https://example.com/"},
		{"求值抛异常", "eval_error:boom|url=https://example.com/"},
		{"探针目标不存在", "target_not_found"},
		{"node_id 不受支持", "node_id_unsupported"},
	}
	for _, tc := range locateFailures {
		t.Run(tc.name, func(t *testing.T) {
			rec := &episodeRecorder{dispatched: true}
			got := rec.dispatchState(episodeOutcome{Observed: tc.observed, Err: errEpisodeTestStub}, "")
			if got != EpisodeNotDispatched {
				t.Fatalf("dispatchState(%q)=%q, 期望 %q（定位阶段失败=物理事件未发出）", tc.observed, got, EpisodeNotDispatched)
			}
		})
	}

	// 反例：真正进入派发后结果未知的样本必须保持 unknown（不得被上面的规则吞掉）。
	rec := &episodeRecorder{dispatched: true}
	if got := rec.dispatchState(episodeOutcome{Err: errEpisodeTestStub}, ""); got != EpisodeDispatchedUnknown {
		t.Fatalf("无观测串的已派发失败=%q, 期望 %q", got, EpisodeDispatchedUnknown)
	}
	// 反例：已派发 + 结构化代号 = 结局已知。
	if got := rec.dispatchState(episodeOutcome{Observed: "input_failed:WRITE_NOT_PERSISTED: x", Err: errEpisodeTestStub}, browser.FaultWriteNotPersisted); got != EpisodeDispatchedKnown {
		t.Fatalf("已派发+结构化代号=%q, 期望 %q", got, EpisodeDispatchedKnown)
	}
	// 反例：中断代号语义优先保留（未/已派发两态）。
	if got := rec.dispatchState(episodeOutcome{Observed: browser.ObservedCanceledBeforeDispatch}, ""); got != EpisodeCanceledBeforeDispatch {
		t.Fatalf("未派发中断=%q, 期望 %q", got, EpisodeCanceledBeforeDispatch)
	}
	if got := rec.dispatchState(episodeOutcome{Observed: browser.ObservedCanceledAfterDispatch}, ""); got != EpisodeCanceledAfterDispatch {
		t.Fatalf("已派发中断=%q, 期望 %q", got, EpisodeCanceledAfterDispatch)
	}
}

// ── 单元：append-only JSONL 与 shadow 字段 ────────────────────────

func TestEpisodeWriterAppendOnlyAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := ""
	for i := 0; i < 2; i++ {
		w, err := OpenEpisodeWriter(dir)
		if err != nil {
			t.Fatalf("OpenEpisodeWriter: %v", err)
		}
		if i == 0 {
			path = w.Path()
		} else if w.Path() != path {
			t.Fatalf("同日文件应稳定复用: %q → %q", path, w.Path())
		}
		if err := w.Append(&Episode{
			Schema:    EpisodeSchemaV1,
			EpisodeID: "ep-" + string(rune('a'+i)),
			Intent:    EpisodeIntent{IntentID: "ep-" + string(rune('a'+i))},
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("Episode 文件权限=%o, 期望 600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("重启后应续写为 2 行（append-only）, 实际 %d 行: %q", len(lines), string(raw))
	}
	for _, ln := range lines {
		var ep Episode
		if err := json.Unmarshal([]byte(ln), &ep); err != nil {
			t.Fatalf("每行必须是合法 JSON: %v (%q)", err, ln)
		}
	}
	// shadow_inference 字段必须存在（允许为 null）——§5.0.13 条款 1③ 的「字段必须存在」。
	if !strings.Contains(lines[0], `"shadow_inference":null`) {
		t.Fatalf("shadow_inference 字段缺失或非 null: %q", lines[0])
	}
	var ep Episode
	if err := json.Unmarshal([]byte(lines[0]), &ep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ep.ShadowInference = &EpisodeShadow{Top1: 0, Scores: []float64{0.9}, ModelHash: "seed"}
	blob, err := json.Marshal(&ep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), `"shadow_inference":{"scores"`) {
		t.Fatalf("影子观察应可序列化: %s", string(blob))
	}
}

// ── 集成：真实 gRPC + Sim 会话 → 一条完整 Episode ─────────────────

func TestEpisodeRecorderLandsFastPathEpisode(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), EpisodeDir: dir})
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)

	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95, Tag: "button", Role: "button"}})
	sess.SetPageURL("https://example.com/login?token=abc123def456")
	sess.SetLastHit(browser.HitElement{
		Selector: "form > button:nth-of-type(1)", Tag: "button", Text: "登录",
	})
	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "ep-1", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil || !resp.Success {
		t.Fatalf("动作应成功: err=%v resp=%+v", err, resp)
	}

	rawLines, rawText := episodeRawLines(t, dir)
	if len(rawLines) != 1 {
		t.Fatalf("期望 1 条 Episode, 实际 %d: %q", len(rawLines), rawText)
	}
	ep := episodeDecode(t, rawLines[0])

	if got := episodeStr(t, ep, "schema"); got != EpisodeSchemaV1 {
		t.Fatalf("schema=%q, 期望 %q", got, EpisodeSchemaV1)
	}
	intent := episodeObj(t, ep, "intent")
	if got := episodeStr(t, intent, "target_semantic"); got != "登录" {
		t.Fatalf("intent.target_semantic=%q", got)
	}
	if got := episodeStr(t, intent, "expected_action"); got != "click" {
		t.Fatalf("intent.expected_action=%q", got)
	}
	cands := episodeArr(t, ep, "candidates")
	if len(cands) != 1 {
		t.Fatalf("候选数=%d, 期望 1", len(cands))
	}
	c0 := cands[0].(map[string]any)
	if got := episodeStr(t, c0, "text"); got != "登录" {
		t.Fatalf("candidates[0].text=%q", got)
	}
	if got := episodeStr(t, c0, "stage"); got != "L1" {
		t.Fatalf("candidates[0].stage=%q", got)
	}
	if got, ok := c0["text_rune_len"].(float64); !ok || int(got) != 2 {
		t.Fatalf("candidates[0].text_rune_len=%v, 期望 2", c0["text_rune_len"])
	}
	prod := episodeObj(t, ep, "production")
	if got := episodeStr(t, prod, "decision"); got != "DIRECT" {
		t.Fatalf("production.decision=%q, 期望 DIRECT（C=0.95 ≥ θ_high）", got)
	}
	if got := episodeStr(t, prod, "dispatch_state"); got != EpisodeDispatchedKnown {
		t.Fatalf("production.dispatch_state=%q, 期望 %q", got, EpisodeDispatchedKnown)
	}
	if got := episodeObj(t, prod, "selected")["ref"].(string); got == "" {
		t.Fatal("production.selected.ref 不能为空（成功执行必有选中候选）")
	}
	post := episodeObj(t, ep, "posterior")
	if got := episodeStr(t, post, "state"); got != "confirmed" {
		t.Fatalf("posterior.state=%q, 期望 confirmed", got)
	}
	if _, ok := ep["shadow_inference"]; !ok {
		t.Fatal("shadow_inference 字段必须存在（允许为 null）")
	}
	if ep["shadow_inference"] != nil {
		t.Fatalf("未接影子通道（无 ONNX 会话），shadow_inference 应为 null: %v", ep["shadow_inference"])
	}
	// 页面查询串可能携带 Token ⇒ 只留 origin+path，且原查询串不得落盘。
	if strings.Contains(rawText, "abc123def456") {
		t.Fatalf("URL 查询串不得落盘: %s", rawText)
	}
	if got := episodeStr(t, episodeObj(t, ep, "page"), "url_before"); got != "https://example.com/login" {
		t.Fatalf("page.url_before=%q, 期望去掉查询串的 URL", got)
	}
}

func TestEpisodeNeverPersistsInputValue(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnvWithOptions(t, Options{
		Keys:       testKeys(),
		EpisodeDir: dir,
		VaultResolve: func(string) (string, error) {
			return "resolved-secret-4f2a", nil
		},
	})
	initCtx(t, env.client, "coordinator-key")
	sess := simSessionOf(t, env.srv)
	sess.SetPageCandidates([]browser.PageCandidate{{Text: "登录", Confidence: 0.95, Tag: "button", Role: "button"}})

	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "ep-input", TargetSemantic: "登录", ExpectedAction: "input", Value: proto.String("hunter2pw-secret"),
	}); err != nil {
		t.Fatalf("明文输入动作: %v", err)
	}
	// 凭据直填：`@vault:<名>` 只留引用名，明文与其解析结果都不得落盘。
	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "ep-vault", TargetSemantic: "登录", ExpectedAction: "input", Value: proto.String("@vault:demo-login"),
	}); err != nil {
		t.Fatalf("凭据直填动作: %v", err)
	}

	rawLines, rawText := episodeRawLines(t, dir)
	if len(rawLines) != 2 {
		t.Fatalf("期望 2 条 Episode, 实际 %d: %q", len(rawLines), rawText)
	}
	for _, leak := range []string{"hunter2pw-secret", "resolved-secret-4f2a", "@vault:"} {
		if strings.Contains(rawText, leak) {
			t.Fatalf("明文/引用原文不得落盘: %q 出现在 %s", leak, rawText)
		}
	}
	plain := episodeObj(t, episodeDecode(t, rawLines[0]), "intent")
	if got := episodeStr(t, plain, "vault_ref"); got != "" {
		t.Fatalf("明文输入不应带 vault_ref: %q", got)
	}
	if has, _ := plain["has_value"].(bool); !has {
		t.Fatal("明文输入应记 has_value=true")
	}
	if got, _ := plain["value_rune_len"].(float64); int(got) != len("hunter2pw-secret") {
		t.Fatalf("value_rune_len=%v, 期望 %d", plain["value_rune_len"], len("hunter2pw-secret"))
	}
	vault := episodeObj(t, episodeDecode(t, rawLines[1]), "intent")
	if got := episodeStr(t, vault, "vault_ref"); got != "demo-login" {
		t.Fatalf("vault_ref=%q, 期望引用名 demo-login", got)
	}
}
