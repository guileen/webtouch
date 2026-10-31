package engine

import (
	"strings"
	"testing"

	"uws/pkg/browser"
	"uws/pkg/matcher"
	"uws/pkg/vlm"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestClickLandingReferenceAnchorsTargetUnlessAStarMultiStep 固化 §5.0.9 条款 7 的
// 基准锚定：只有 A* **多步换步**（路径长度 ≥2）才改用 `exec_semantic`；长度 1 的退化
// 「路径」（单候选绑定 / 根即答案）仍按 `target_semantic` 判——真机反证（`Order history`
// ⇒ 单候选 `Sign in` 报 `SUCCEEDED`）正是这一档被漏判。
func TestClickLandingReferenceAnchorsTargetUnlessAStarMultiStep(t *testing.T) {
	cases := []struct {
		name         string
		target, exec string
		depth        int
		want         string
	}{
		{"直选（无 A*）", "Order history", "", 0, "Order history"},
		{"单候选绑定（长度 1 的路径，根即答案）", "Order history", "Sign in", 1, "Order history"},
		{"A* 多步换步（长度 2）", "订单历史", "账户设置", 2, "账户设置"},
		{"多步但 exec 为空 ⇒ 退回目标语义", "订单历史", "", 3, "订单历史"},
	}
	for _, c := range cases {
		if got := clickLandingReference(c.target, c.exec, c.depth); got != c.want {
			t.Errorf("%s: clickLandingReference(%q, %q, %d) = %q, want %q",
				c.name, c.target, c.exec, c.depth, got, c.want)
		}
	}
}

// TestClickLandingRefsAliasesAndEndorsed 固化条款 7 的基准集：内部语义标签按 §5.0.2
// 别名展开（`submit_btn` → 「提交」）算相关；与目标语义无关的同义词文本**不**背书
// （真机实证：池内唯一候选 `Sign in` 被绑定给 `Order history` 曾因此报 `SUCCEEDED`）。
func TestClickLandingRefsAliasesAndEndorsed(t *testing.T) {
	if got := clickLandingVerdict("submit_btn", browser.HitElement{Text: "提交"}, true); got != landingRelevant {
		t.Fatalf("内部标签应经 §5.0.2 别名展开算相关，got %v", got)
	}
	// 同义词对（登录 / 登入）文本不互含：无 KB 背书时判 miss（背书集由同源链接提供，
	// 见 TestEndorsedSynonymsOnlyTargetLinked）；无关同义词文本不背书 ⇒ 判 miss。
	if got := clickLandingVerdict("登录", browser.HitElement{Text: "登入"}, true); got != landingMiss {
		t.Fatalf("无同源背书时同义词对属文本不互含 ⇒ miss，got %v", got)
	}
	if got := clickLandingVerdictRefs(clickLandingRefs("Order history", nil),
		browser.HitElement{Text: "Sign in"}, true); got != landingMiss {
		t.Fatalf("池内无关同义词文本不得背书 ⇒ 判 miss，got %v", got)
	}
}

// TestEndorsedSynonymsOnlyTargetLinked 固化条款 7 的背书同源判据（2026-09-30 真机订正）：
// 背书集只取 `site_synonyms.source_rule_id` 指向「语义标签命中目标语义」的规则的同义词；
// 池内同义词召回按页面节点文本发生、与目标语义无关，不得作为背书来源。
func TestEndorsedSynonymsOnlyTargetLinked(t *testing.T) {
	m, err := matcher.New(":memory:")
	if err != nil {
		t.Fatalf("matcher.New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	id, err := m.InsertRule(matcher.Rule{
		RuleType: "text_pattern", TextPattern: "登录", SemanticLabel: "登录",
		Action: "click", Status: "active",
	})
	if err != nil {
		t.Fatalf("InsertRule: %v", err)
	}
	if _, err := m.InsertSynonym(matcher.Synonym{
		Term: "登入", BaseWeight: 0.2, Status: "TENTATIVE", SourceRuleID: id,
	}); err != nil {
		t.Fatalf("InsertSynonym: %v", err)
	}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Matcher: m})

	endorsed := env.srv.endorsedSynonyms("登录")
	if len(endorsed) != 1 || endorsed[0] != "登入" {
		t.Fatalf("同源同义词应进背书集，got %v", endorsed)
	}
	if got := clickLandingVerdictRefs(clickLandingRefs("登录", endorsed),
		browser.HitElement{Text: "登入"}, true); got != landingRelevant {
		t.Fatalf("同源同义词落点应算相关（防假 miss），got %v", got)
	}
	if extra := env.srv.endorsedSynonyms("Order history"); len(extra) != 0 {
		t.Fatalf("与目标语义无关时不得取到背书：%v", extra)
	}
}

// TestDeterministicClickLandingMismatchFails：确定性路径（语义 / 选择器直查）的点击出口
// 同样判落点自证（§5.0.9 条款 7）——真机反证形态「单候选被绑定给无关语义」不得再报
// `SUCCEEDED`，也不得换出口重问 VLM（判 miss 属硬失败）。
func TestDeterministicClickLandingMismatchFails(t *testing.T) {
	mock := &vlm.MockClient{}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	sess.SetLastHit(browser.HitElement{Selector: "div#hero", Text: "Sign in"})

	_, err := vlmExec(t, env, "Order history", "click")
	if status.Code(err) != codes.Aborted || !strings.Contains(err.Error(), FaultClickLandingMismatch) {
		t.Fatalf("确定性路径判 miss 应返回 Aborted + %s，got %v", FaultClickLandingMismatch, err)
	}
	if mock.Calls != 0 {
		t.Fatalf("判 miss 不得换出口重问 VLM，calls=%d", mock.Calls)
	}
}

// TestStateSwitchSemanticSkipsLandingSelfCheck（§5.0.9 条款 8）：站点控件语义的落点文本
// 是站点固定 UI 文案而不是语义标识（真机 `ref=gemini_model_switch hit=Flash`、
// `ref=gemini_send hit=Send message`），不得据此判 miss 硬失败——否则每次模型切换/发送
// 都会被自己的落点自证挡死（通道 500／502）。
func TestStateSwitchSemanticSkipsLandingSelfCheck(t *testing.T) {
	for _, s := range []string{"gemini_model_switch", "deepseek_thinking_switch", "gemini_send", "gemini_new_chat"} {
		if landingSelfCheckApplies(s) {
			t.Fatalf("站点控件语义不适用文本互含落点自证：%s", s)
		}
	}
	if !landingSelfCheckApplies("Order history") || landingSelfCheckApplies("node:42") {
		t.Fatal("普通语义适用、node_id 寻址不适用——既有口径不得被本条改动")
	}

	mock := &vlm.MockClient{}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	sess.SetLastHit(browser.HitElement{Selector: "button#picker", Tag: "button", Text: "Flash"})

	_, err := vlmExec(t, env, "gemini_model_switch", "click")
	if err != nil && strings.Contains(err.Error(), FaultClickLandingMismatch) {
		t.Fatalf("状态型切换语义不得因落点文本判 miss：%v", err)
	}

	// 真机扩围实例（2026-10-01 13:28:07 引擎日志 hit="Send message"）：发送按钮同理。
	sess.SetLastHit(browser.HitElement{Selector: "button#send", Tag: "button", Text: "Send message"})
	_, err = vlmExec(t, env, "gemini_send", "click")
	if err != nil && strings.Contains(err.Error(), FaultClickLandingMismatch) {
		t.Fatalf("站点固定控件语义不得因落点文本判 miss：%v", err)
	}
}

// TestDeterministicClickLandingVerifiedSucceeds：落点自证通过时确定性出口不变
// （回归底线——别把正常点击也判成 miss；顺带钉住命中元素证据照常回显）。
func TestDeterministicClickLandingVerifiedSucceeds(t *testing.T) {
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, nil, f)
	sess.SetLastHit(browser.HitElement{Selector: "a#orders", Tag: "a", Text: "Order history"})

	resp, err := vlmExec(t, env, "Order history", "click")
	if err != nil || !resp.Success {
		t.Fatalf("落点相关应维持原出口：resp=%+v err=%v", resp, err)
	}
	if resp.HitElement == nil || resp.HitElement.Text != "Order history" {
		t.Fatalf("命中元素证据应照常回显: %+v", resp.HitElement)
	}
}

// TestClickLandingVerdict 覆盖 §5.0.9 条款 6 的三态判据：
// 命中自身 / 后代 / 祖先容器均算「相关」，明确点错算 miss，缺证据算 unverified。
func TestClickLandingVerdict(t *testing.T) {
	cases := []struct {
		name   string
		target string
		hit    browser.HitElement
		has    bool
		want   clickLanding
	}{
		{"点中自身", "sign in", browser.HitElement{Text: "Sign In"}, true, landingRelevant},
		{"点中后代（hit 文本更宽）", "submit", browser.HitElement{Text: "Submit order now"}, true, landingRelevant},
		{"点中祖先容器（目标更宽）", "download the report", browser.HitElement{Text: "Download"}, true, landingRelevant},
		{"只差空白分隔符", "search box", browser.HitElement{Text: "SearchBox"}, true, landingRelevant},
		{"中文子串命中", "登录", browser.HitElement{Text: "立即登录账号"}, true, landingRelevant},
		{"明确点错", "sign in", browser.HitElement{Text: "Sign up for newsletter"}, true, landingMiss},
		{"跨文字系统（中文语义 / 英文命中）⇒ 证据不足，不得判 miss", "登录", browser.HitElement{Text: "Log in"}, true, landingUnverified},
		{"跨文字系统（英文语义 / 中文命中）⇒ 证据不足", "sign in", browser.HitElement{Text: "注册"}, true, landingUnverified},
		{"汉字与非汉字混排仍算同侧（可判 miss）", "登录", browser.HitElement{Text: "注册 Sign up"}, true, landingMiss},
		{"命中文本为空 ⇒ 证据不足", "sign in", browser.HitElement{Selector: "button:nth-of-type(2)"}, true, landingUnverified},
		{"无命中元素 ⇒ 证据不足", "sign in", browser.HitElement{}, false, landingUnverified},
		{"目标语义为空 ⇒ 证据不足", "", browser.HitElement{Text: "anything"}, true, landingUnverified},
	}
	for _, c := range cases {
		if got := clickLandingVerdict(c.target, c.hit, c.has); got != c.want {
			t.Errorf("%s: clickLandingVerdict(%q, %+v, has=%v) = %v, want %v",
				c.name, c.target, c.hit, c.has, got, c.want)
		}
	}
}

// TestClickLearnGate 是 learned 写入门禁的判据表：点击族只在「相关」时放行，
// 非点击动作不受约束（沿用既有行为），无会话证据时点击不写。
func TestClickLearnGate(t *testing.T) {
	if allowed, why := clickLearnGate(nil, "input", "search"); !allowed || why != "" {
		t.Fatalf("非点击动作不应受落点自证约束：allowed=%v why=%q", allowed, why)
	}
	if allowed, why := clickLearnGate(nil, "click", "search"); allowed {
		t.Fatalf("无会话证据的点击不得写 learned（why=%q）", why)
	}
	if !isClickAction("click") || isClickAction("input") || isClickAction("extract") {
		t.Fatal("isClickAction 只应认 click")
	}
}

// TestNormalizeLandingText 固化归一化口径（小写 + 去全部空白）。
func TestNormalizeLandingText(t *testing.T) {
	if got := normalizeLandingText("  Sign In\t\n"); got != "signin" {
		t.Fatalf("normalizeLandingText = %q, want %q", got, "signin")
	}
}
