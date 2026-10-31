// 判定层证据链（v1.5.4，docs/evidence/research/uws-verdict-evidence-design-2026-08-27.md §6.1）：
// ExecuteAction 响应回显实际命中元素 + 动作前后 URL 证据，让调用方识破
// 「点击命中容器却报成功」假阳性（drama-h5 §3.2 / skincare 复测实证）。
package engine

import (
	"context"
	"testing"

	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"
)

type evidenceSimFactory struct {
	SimFactory
	last *SimCDPSession
}

func (f *evidenceSimFactory) Create(ctx context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s, err := f.SimFactory.Create(ctx, cfg)
	if err != nil {
		return nil, err
	}
	f.last = s.(*SimCDPSession)
	return f.last, nil
}

func evidenceEnv(t *testing.T, f *evidenceSimFactory) (*testEnv, *SimCDPSession) {
	t.Helper()
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(2, f),
	})
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"),
		&uwspb.InitContextRequest{TaskId: "evidence-task"}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	return env, f.last
}

func evidenceExec(t *testing.T, env *testEnv, semantic, action string) (*uwspb.ActionResultResponse, error) {
	t.Helper()
	return env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "evidence-" + semantic,
		TargetSemantic: semantic,
		ExpectedAction: action,
		ContextHint:    map[string]string{"context": ""},
	})
}

func TestEvidenceHitElementEchoAndURLChanged(t *testing.T) {
	// 正向：点击真实导航（URL 变化），响应回显命中元素 + page_url + url_changed=true。
	f := &evidenceSimFactory{}
	env, sess := evidenceEnv(t, f)
	sess.SetPageURL("https://serp.example/search")
	sess.SetNavOnAction("https://detail.example/product/1")
	sess.SetLastHit(browser.HitElement{
		Selector: "div#chapter-grid > li:nth-of-type(4)",
		Tag:      "li", X: 10, Y: 20, W: 100, H: 40, Text: "第4集",
	})

	resp, err := evidenceExec(t, env, "第4集", "click")
	if err != nil {
		t.Fatalf("click 应成功: %v", err)
	}
	if !resp.Success || !resp.LocalVerification.EventTriggered {
		t.Fatalf("success/event_triggered 应为 true: %+v", resp)
	}
	if resp.HitElement == nil {
		t.Fatal("hit_element 应回显")
	}
	if resp.HitElement.Selector != "div#chapter-grid > li:nth-of-type(4)" {
		t.Fatalf("hit_element.selector = %q", resp.HitElement.Selector)
	}
	if resp.HitElement.Text != "第4集" || resp.HitElement.Tag != "li" {
		t.Fatalf("hit_element 元信息不符: %+v", resp.HitElement)
	}
	if resp.PageUrl != "https://detail.example/product/1" {
		t.Fatalf("page_url = %q", resp.PageUrl)
	}
	if !resp.UrlChanged {
		t.Fatal("url_changed 应为 true（点击已导航）")
	}
}

func TestEvidenceFalsePositiveClickRevealsNoNavigation(t *testing.T) {
	// 假阳性场景（drama-h5「第 4 集」/ skincare「第 1 个搜索结果」）：
	// 点击报 success + event_triggered，但 URL 未变 + 命中元素是容器——
	// 证据链让调用方可据此识破，而不是把 event_triggered 当真。
	f := &evidenceSimFactory{}
	env, sess := evidenceEnv(t, f)
	sess.SetPageURL("https://serp.example/search")
	sess.SetLastHit(browser.HitElement{
		Selector: "div#chapter-grid",
		Tag:      "div", X: 100, Y: 200, W: 360, H: 120, Text: "第4集",
	})

	resp, err := evidenceExec(t, env, "第4集", "click")
	if err != nil {
		t.Fatalf("引擎仍按旧语义返回成功（判定留给调用方）: %v", err)
	}
	if !resp.Success || !resp.LocalVerification.EventTriggered {
		t.Fatalf("旧语义应保持 success+event_triggered: %+v", resp)
	}
	if resp.UrlChanged {
		t.Fatal("url_changed 应为 false（页面未导航）")
	}
	if resp.HitElement == nil || resp.HitElement.Selector != "div#chapter-grid" {
		t.Fatalf("hit_element 应回显容器真身: %+v", resp.HitElement)
	}
	// 关键：success=true + url_changed=false + 命中容器 → 调用方（assertion 模式）
	// 可判定「假阳性」，不再被 event_triggered 蒙蔽。
	if resp.Success && !resp.UrlChanged && resp.HitElement.Selector == "div#chapter-grid" {
		t.Log("证据齐全：调用方可判定该 click 为假阳性（未导航 + 命中容器）")
	}
}

func TestEvidenceExtractEchoesHitElement(t *testing.T) {
	// extract 同样回显命中元素（skincare 复测「extract:成分表 命中相关搜索建议」：
	// 调用方可看到命中的是建议框而不是成分表节点）。
	f := &evidenceSimFactory{}
	env, sess := evidenceEnv(t, f)
	sess.SetPageURL("https://serp.example/search")
	sess.SetLastHit(browser.HitElement{
		Selector: "div#related-searches > li:nth-of-type(2)",
		Tag:      "li", X: 300, Y: 700, W: 200, H: 30, Text: "红宝石精华成分表",
	})

	resp, err := evidenceExec(t, env, "成分表", "extract")
	if err != nil {
		t.Fatalf("extract 应成功: %v", err)
	}
	if resp.HitElement == nil || resp.HitElement.Text != "红宝石精华成分表" {
		t.Fatalf("hit_element 应回显建议框文本: %+v", resp.HitElement)
	}
	if resp.HitElement.Selector != "div#related-searches > li:nth-of-type(2)" {
		t.Fatalf("hit_element.selector = %q", resp.HitElement.Selector)
	}
}

type uaCaptureFactory struct {
	SimFactory
	got browser.Config
}

func (f *uaCaptureFactory) Create(ctx context.Context, cfg browser.Config) (browser.CDPSession, error) {
	f.got = cfg
	return f.SimFactory.Create(ctx, cfg)
}

func TestInitializeContextNoUAOverrideByDefault(t *testing.T) {
	// 2026-08-27 指纹修复：未显式指定 UA 时不再覆盖为 "UWS/1.4 Engine"，
	// 附着模式保留真实浏览器指纹（百度/smzdm 对 UA 异常的引擎 Tab 弹验证码，
	// 同一 Chrome 的普通 Tab 不弹——dom_probe 实证 navigator.userAgent 差异）。
	f := &uaCaptureFactory{}
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(1, f),
	})
	if _, err := env.client.InitializeContext(mdCtx("coordinator-key"),
		&uwspb.InitContextRequest{TaskId: "ua-task"}); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if f.got.UserAgent != "" {
		t.Fatalf("未指定 UA 时不应覆盖浏览器 UA，got %q", f.got.UserAgent)
	}
}
