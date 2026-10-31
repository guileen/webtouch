// VLM 感知兜底（2026-08-27）：决策带 NOT_FOUND / 物理 no_match 时，
// learned 命中 → 确定性执行；否则截图 → VLM 候选 → 验证执行 → 学习回写。
package engine

import (
	"context"
	"strings"
	"testing"

	"uws/pkg/browser"
	"uws/pkg/vlm"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const vlmPageURL = "https://myread.top/store"

type vlmSimFactory struct {
	SimFactory
	pageURL string
	last    *SimCDPSession
}

func (f *vlmSimFactory) Create(ctx context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s, err := f.SimFactory.Create(ctx, cfg)
	if err != nil {
		return nil, err
	}
	sess := s.(*SimCDPSession)
	sess.SetPageURL(f.pageURL)
	f.last = sess
	return sess, nil
}

func vlmEnv(t *testing.T, fb *vlm.Fallback, factory browser.SessionFactory) (*testEnv, *SimCDPSession) {
	t.Helper()
	env := newTestEnvWithOptions(t, Options{
		Keys: testKeys(),
		Pool: browser.NewPool(2, factory),
		VLM:  fb,
	})
	_, err := env.client.InitializeContext(mdCtx("coordinator-key"),
		&uwspb.InitContextRequest{TaskId: "vlm-task"})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	// 取工厂创建的会话（断言物理层调用用）
	var sess *SimCDPSession
	if f, ok := factory.(*vlmSimFactory); ok {
		sess = f.last
	}
	return env, sess
}

func vlmExec(t *testing.T, env *testEnv, semantic, action string) (*uwspb.ActionResultResponse, error) {
	t.Helper()
	return env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "vlm-" + semantic,
		TargetSemantic: semantic,
		ExpectedAction: action,
		ContextHint:    map[string]string{"context": ""},
	})
}

func vlmExecValue(t *testing.T, env *testEnv, semantic, action, value string) (*uwspb.ActionResultResponse, error) {
	t.Helper()
	return env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId:       "vlm-value-" + semantic,
		TargetSemantic: semantic,
		ExpectedAction: action,
		Value:          &value,
		ContextHint:    map[string]string{"context": ""},
	})
}

func TestVLMDisabledNoFallback(t *testing.T) {
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, _ := vlmEnv(t, nil, f)
	if _, err := vlmExec(t, env, "missing", "click"); status.Code(err) != codes.NotFound {
		t.Fatalf("VLM 关闭时应保持 NOT_FOUND，got %v", err)
	}
}

func TestVLMResolveClickLearns(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{{
		Found: true, Text: "Godfall: Helios' Inverse Scale", XRatio: 0.5, YRatio: 0.2,
	}}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	// §5.0.9 条款 6：VLM 坐标点击的 learned 写入须过落点自证（命中元素可见文本），
	// 真实会话的落点文本由物理层随 elementFromPoint 一并取证；此处脚本化该证据，
	// 未脚本化（无文本）时本用例的 learned 断言应为「不写」。
	sess.SetLastHit(browser.HitElement{Selector: "sim:click:1017,604", Text: "missing"})
	resp, err := vlmExec(t, env, "missing", "click")
	if err != nil {
		t.Fatalf("VLM 兜底应成功: %v", err)
	}
	if !resp.Success {
		t.Fatalf("resp.Success = false: %+v", resp)
	}
	if mock.Calls != 1 {
		t.Fatalf("VLM calls = %d, want 1", mock.Calls)
	}
	// Sim 截图默认 1920x1080 → 0.5*1920=960, 0.2*1080=216
	if !strings.Contains(strings.Join(sess.Actions(), "\n"), "click_at|960,216") {
		t.Fatalf("未调用坐标点击: %v", sess.Actions())
	}
	e, ok := learned.Lookup(vlmPageURL, "missing")
	if !ok || e.Selector != "sim:click:960,216" {
		t.Fatalf("learned 未回写: %+v ok=%v", e, ok)
	}
}

// TestVLMLearnedReuseLandingMismatchForgets：learned 复用命中的落点与目标语义无关时，
// 复用路径同样判 miss（§5.0.9 条款 6 适用路径）：不返回 SUCCEEDED、不换出口重问 VLM，
// 并清掉失效条目（否则坏条目会被反复复用、把整页点进防乱点熔断）。
func TestVLMLearnedReuseLandingMismatchForgets(t *testing.T) {
	learned, _ := vlm.NewLearnedStore("")
	if err := learned.Learn(vlmPageURL, "missing", vlm.LearnedEntry{Selector: "button.legacy"}); err != nil {
		t.Fatal(err)
	}
	mock := &vlm.MockClient{Results: []vlm.Result{{Found: true, Action: "click", XRatio: 0.5, YRatio: 0.5}}}
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	// 页面改版：原 selector 仍在，但已落到无关元素（命中文本 = 订阅入口）。
	sess.SetLastHit(browser.HitElement{Selector: "button.legacy", Text: "Sign up for newsletter"})

	_, err := vlmExec(t, env, "missing", "click")
	if status.Code(err) != codes.Aborted || !strings.Contains(err.Error(), FaultClickLandingMismatch) {
		t.Fatalf("复用判 miss 应返回 Aborted + %s，got %v", FaultClickLandingMismatch, err)
	}
	if mock.Calls != 0 {
		t.Fatalf("判 miss 不得换出口重问 VLM，calls=%d", mock.Calls)
	}
	if _, ok := learned.Lookup(vlmPageURL, "missing"); ok {
		t.Fatal("判 miss 的失效条目应被清掉（下次重新学习）")
	}
}

// TestVLMLearnedReuseVerifiedStaysLearned：落点自证通过时，复用路径维持原出口
// （免 VLM）并累加热度；回归底线——别把正常复用也判成 miss。
func TestVLMLearnedReuseVerifiedStaysLearned(t *testing.T) {
	learned, _ := vlm.NewLearnedStore("")
	if err := learned.Learn(vlmPageURL, "missing", vlm.LearnedEntry{Selector: "button.legacy"}); err != nil {
		t.Fatal(err)
	}
	mock := &vlm.MockClient{}
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	sess.SetLastHit(browser.HitElement{Selector: "button.legacy", Text: "Missing Target"})

	resp, err := vlmExec(t, env, "missing", "click")
	if err != nil || !resp.Success {
		t.Fatalf("落点自证通过应成功：resp=%+v err=%v", resp, err)
	}
	if mock.Calls != 0 {
		t.Fatalf("learned 命中不应调 VLM，calls=%d", mock.Calls)
	}
	e, ok := learned.Lookup(vlmPageURL, "missing")
	if !ok || e.Hits != 2 {
		t.Fatalf("命中应累加热度：%+v ok=%v", e, ok)
	}
}

func TestVLMExtractLearnsSelector(t *testing.T) {
	// 提取假闭环修复（GLOBAL §5 #50）：VLM 给坐标 → ExtractAt 回读真实定位器，
	// learned 写 selector（而非仅 NodeText），下次可确定性提取、不再烧 VLM。
	mock := &vlm.MockClient{Results: []vlm.Result{{
		Found: true, Text: "水,甘油", XRatio: 0.5, YRatio: 0.5,
	}}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, _ := vlmEnv(t, fb, f)

	// 语义取 "missing"（featuresFor 触发 FALLBACK → VLM）；"成分表" 会走默认
	// EXECUTE 分支、不进 VLM。
	resp, err := vlmExec(t, env, "missing", "extract")
	if err != nil {
		t.Fatalf("VLM extract 应成功: %v", err)
	}
	if !resp.Success || resp.ExtractedData == nil {
		t.Fatalf("extract 响应不符: %+v", resp)
	}
	e, ok := learned.Lookup(vlmPageURL, "missing")
	if !ok || e.Selector != "sim:extract:960,540" {
		t.Fatalf("extract learned 未回写 selector: %+v ok=%v", e, ok)
	}
}

func TestVLMLearnedHitSkipsVLM(t *testing.T) {
	mock := &vlm.MockClient{}
	learned, _ := vlm.NewLearnedStore("")
	if err := learned.Learn(vlmPageURL, "missing",
		vlm.LearnedEntry{Selector: "div.learned"}); err != nil {
		t.Fatal(err)
	}
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	if _, err := vlmExec(t, env, "missing", "click"); err != nil {
		t.Fatalf("learned 命中应成功: %v", err)
	}
	if mock.Calls != 0 {
		t.Fatalf("learned 命中不应调 VLM, calls=%d", mock.Calls)
	}
	if !strings.Contains(strings.Join(sess.Actions(), "\n"), "sel=div.learned") {
		t.Fatalf("未使用 learned 选择器: %v", sess.Actions())
	}
	if e, ok := learned.Lookup(vlmPageURL, "missing"); !ok || e.Hits != 2 {
		t.Fatalf("learned 命中应累加 hits: %+v ok=%v", e, ok)
	}
}

func TestVLMExtractText(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{{
		Found: true, Text: "Godfall: Helios' Inverse Scale",
	}}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, _ := vlmEnv(t, fb, f)
	resp, err := vlmExec(t, env, "missing", "extract")
	if err != nil {
		t.Fatalf("extract 兜底应成功: %v", err)
	}
	if resp.GetExtractedData() != "Godfall: Helios' Inverse Scale" {
		t.Fatalf("extracted = %q", resp.GetExtractedData())
	}
	if e, ok := learned.Lookup(vlmPageURL, "missing"); !ok || e.NodeText == "" {
		t.Fatalf("extract 未学习: %+v ok=%v", e, ok)
	}
}

func TestVLMResolveErrorKeepsNotFound(t *testing.T) {
	mock := &vlm.MockClient{Err: context.DeadlineExceeded}
	fb := &vlm.Fallback{Enabled: true, Client: mock}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, _ := vlmEnv(t, fb, f)
	if _, err := vlmExec(t, env, "missing", "click"); status.Code(err) != codes.NotFound {
		t.Fatalf("VLM 失败应保持 NOT_FOUND，got %v", err)
	}
}

// noMatchSession：指定语义在物理层返回 no_match（模拟真实 CDP 听不到 Vue div 卡片）。
type noMatchSession struct {
	*SimCDPSession
	fail map[string]bool
}

func (s *noMatchSession) ExecuteAction(ctx context.Context, action, value, semantic string) (bool, string) {
	if s.fail[semantic] {
		return false, "no_match"
	}
	return s.SimCDPSession.ExecuteAction(ctx, action, value, semantic)
}

func (s *noMatchSession) ExecuteActionWithSelector(ctx context.Context, action, value, semantic, selector string) (bool, string) {
	if s.fail[semantic] {
		return false, "no_match"
	}
	return s.SimCDPSession.ExecuteActionWithSelector(ctx, action, value, semantic, selector)
}

type noMatchFactory struct {
	pageURL string
	last    *noMatchSession
}

func (f *noMatchFactory) Create(ctx context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s := &noMatchSession{
		SimCDPSession: NewSimCDPSession("nomatch-1", cfg),
		fail:          map[string]bool{"Godfall": true},
	}
	s.SetPageURL(f.pageURL)
	f.last = s
	return s, nil
}

func TestVLMPostPhysicalNoMatch(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{{
		Found: true, Text: "Godfall: Helios' Inverse Scale",
	}}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned}
	f := &noMatchFactory{pageURL: vlmPageURL}
	env, _ := vlmEnv(t, fb, f)
	resp, err := vlmExec(t, env, "Godfall", "click")
	if err != nil {
		t.Fatalf("物理 no_match 后 VLM 兜底应成功: %v", err)
	}
	if !resp.Success {
		t.Fatalf("resp.Success = false")
	}
	if mock.Calls != 1 {
		t.Fatalf("VLM calls = %d", mock.Calls)
	}
	if !strings.Contains(strings.Join(f.last.Actions(), "\n"), "click|Godfall: Helios' Inverse Scale|") {
		t.Fatalf("未用 VLM 文本重试: %v", f.last.Actions())
	}
	if e, ok := learned.Lookup(vlmPageURL, "Godfall"); !ok || e.Semantic != "Godfall: Helios' Inverse Scale" {
		t.Fatalf("未学习语义映射: %+v ok=%v", e, ok)
	}
}

func TestVLMRefineRegionZoom(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{
		{Found: true, Text: "card", XRatio: 0.5, YRatio: 0.5},
		{Found: true, Text: "card", XRatio: 0.8, YRatio: 0.6},
	}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Refine: true}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	resp, err := vlmExec(t, env, "missing", "click")
	if err != nil {
		t.Fatalf("refine 兜底应成功: %v", err)
	}
	if !resp.Success {
		t.Fatal("resp.Success = false")
	}
	if mock.Calls != 2 {
		t.Fatalf("VLM calls = %d, want 2（粗定位 + 精修）", mock.Calls)
	}
	// 视口 1920x1080：粗点 (960,540) → 裁剪 1152x486 @ (384,297)
	clip := sess.LastClipRegion()
	if len(clip) != 4 || clip[0] != 384 || clip[1] != 297 || clip[2] != 1152 || clip[3] != 486 {
		t.Fatalf("clip region = %v", clip)
	}
	// 精修：px = 384 + 0.8*1152 = 1305, py = 297 + 0.6*486 = 588
	if !strings.Contains(strings.Join(sess.Actions(), "\n"), "click_at|1305,588") {
		t.Fatalf("未使用精修坐标点击: %v", sess.Actions())
	}
	if e, ok := learned.Lookup(vlmPageURL, "missing"); !ok || e.Selector != "sim:click:1305,588" {
		t.Fatalf("learned selector = %+v ok=%v", e, ok)
	}
}

func TestRefineRegionClamp(t *testing.T) {
	x, y, w, h, ok := refineRegion(0, 0, 412, 915)
	if !ok || x != 0 || y != 0 || w <= 0 || h <= 0 {
		t.Fatalf("edge clamp = %d,%d %dx%d ok=%v", x, y, w, h, ok)
	}
	x, y, w, h, ok = refineRegion(412, 915, 412, 915)
	if !ok || x+w > 412 || y+h > 915 {
		t.Fatalf("bottom-right clamp = %d,%d %dx%d ok=%v", x, y, w, h, ok)
	}
}

// ── VLM 主控会话（GLOBAL §5 #52）：VLM 自决 crop_zoom/back/done/impossible ──────────

func TestVLMMainControlImpossibleDistinctSignal(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{{Action: vlm.ActionImpossible}}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, _ := vlmEnv(t, fb, f)
	_, err := vlmExec(t, env, "missing", "click")
	if status.Code(err) != codes.Aborted {
		t.Fatalf("VLM 主控判 impossible 应返回 Aborted（区分信号），got %v", err)
	}
	if mock.Calls != 1 {
		t.Fatalf("VLM calls = %d, want 1", mock.Calls)
	}
}

func TestVLMMainControlCropZoomLoop(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{
		{Action: vlm.ActionCropZoom, XRatio: 0.5, YRatio: 0.5, WRatio: 0.3, HRatio: 0.3},
		{Action: vlm.ActionClick, Found: true, Text: "Godfall: Helios' Inverse Scale", XRatio: 0.6, YRatio: 0.7},
	}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	// §5.0.9 条款 6：主控坐标点击同样要过落点自证才能写 learned。
	// 脚本化落点证据（命中元素可见文本＝目标语义）；真实会话由物理层在
	// elementFromPoint 解析受体时一并取证，缺证据时 learned 写入被拦。
	sess.SetLastHit(browser.HitElement{Selector: "sim:click:1017,604", Text: "missing"})
	resp, err := vlmExec(t, env, "missing", "click")
	if err != nil {
		t.Fatalf("主控 crop→click 应成功: %v", err)
	}
	if !resp.Success {
		t.Fatal("resp.Success = false")
	}
	if mock.Calls != 2 {
		t.Fatalf("VLM calls = %d, want 2（crop 首轮 + click 次轮）", mock.Calls)
	}
	// 视口 1920x1080：crop = (0.5*1920-576/2, 0.5*1080-324/2) = 672,378 576x324
	if clip := sess.LastClipRegion(); len(clip) != 4 || clip[0] != 672 || clip[1] != 378 || clip[2] != 576 || clip[3] != 324 {
		t.Fatalf("clip region = %v, want [672 378 576 324]", clip)
	}
	// 次轮对裁剪图 (576x324) 报 (0.6,0.7) → 视口绝对 (672+345, 378+226) = 1017,604
	if !strings.Contains(strings.Join(sess.Actions(), "\n"), "click_at|1017,604") {
		t.Fatalf("未用主控坐标点击: %v", sess.Actions())
	}
	// 能力上下文/轮次/裁剪标记透传
	if len(mock.Reqs) != 2 {
		t.Fatalf("reqs = %d", len(mock.Reqs))
	}
	if mock.Reqs[0].Round != 0 || mock.Reqs[0].Crop || mock.Reqs[0].Capabilities == "" {
		t.Fatalf("req0 = %+v", mock.Reqs[0])
	}
	if mock.Reqs[1].Round != 1 || !mock.Reqs[1].Crop {
		t.Fatalf("req1 = %+v", mock.Reqs[1])
	}
	if e, ok := learned.Lookup(vlmPageURL, "missing"); !ok || e.Selector != "sim:click:1017,604" {
		t.Fatalf("learned selector = %+v ok=%v", e, ok)
	}
}

func TestVLMMainControlBackThenClick(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{
		{Action: vlm.ActionBack},
		{Action: vlm.ActionDone, Found: true, Text: "Godfall: Helios' Inverse Scale", XRatio: 0.5, YRatio: 0.2},
	}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	resp, err := vlmExec(t, env, "missing", "click")
	if err != nil {
		t.Fatalf("主控 back→done 应成功: %v", err)
	}
	if !resp.Success {
		t.Fatal("resp.Success = false")
	}
	acts := strings.Join(sess.Actions(), "\n")
	if !strings.Contains(acts, "back") || !strings.Contains(acts, "click_at|960,216") {
		t.Fatalf("back 后应重新整页点击: %v", sess.Actions())
	}
	if mock.Calls != 2 {
		t.Fatalf("VLM calls = %d, want 2", mock.Calls)
	}
}

func TestVLMMainControlRoundLimitDegrades(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{
		{Action: vlm.ActionCropZoom, XRatio: 0.5, YRatio: 0.5, WRatio: 0.3, HRatio: 0.3},
		{Action: vlm.ActionCropZoom, XRatio: 0.5, YRatio: 0.5, WRatio: 0.3, HRatio: 0.3},
		{Action: vlm.ActionCropZoom, XRatio: 0.5, YRatio: 0.5, WRatio: 0.3, HRatio: 0.3},
	}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9, MaxRounds: 3}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, _ := vlmEnv(t, fb, f)
	_, err := vlmExec(t, env, "missing", "click")
	if status.Code(err) != codes.Aborted {
		t.Fatalf("主控轮数耗尽应返回 Aborted（避免死循环），got %v", err)
	}
	if mock.Calls != 3 {
		t.Fatalf("VLM calls = %d, want 3（到 maxRounds 即止）", mock.Calls)
	}
}

func TestVLMMainControlExtractDone(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{
		{Action: vlm.ActionDone, Found: true, Text: "成分表：A醇 0.1%"},
	}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, _ := vlmEnv(t, fb, f)
	resp, err := vlmExec(t, env, "missing", "extract")
	if err != nil {
		t.Fatalf("主控 done(extract) 应成功: %v", err)
	}
	if resp.GetExtractedData() != "成分表：A醇 0.1%" {
		t.Fatalf("extracted = %q", resp.GetExtractedData())
	}
	if e, ok := learned.Lookup(vlmPageURL, "missing"); !ok || e.NodeText == "" {
		t.Fatalf("extract 未学习: %+v ok=%v", e, ok)
	}
}

// TestVLMMainControlInputFillsValue：规则库未覆盖站点（如 Airwallex）的 input 语义兜底
// —— VLM 返回 ActionInput + 输入框坐标 → 引擎点击聚焦 → 填入请求值 → learned 回写 selector。
// 2026-08-31：修复「VLM 主控动作集合无 input，输入无人兜底」的缺口（GLOBAL §5 #70）。
func TestVLMMainControlInputFillsValue(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{{
		Action: vlm.ActionInput, Found: true, Text: "产品名称", XRatio: 0.5, YRatio: 0.2,
	}}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	resp, err := vlmExecValue(t, env, "missing", "input", "auto-renewable week")
	if err != nil {
		t.Fatalf("VLM input 兜底应成功: %v", err)
	}
	if !resp.Success {
		t.Fatalf("resp.Success = false: %+v", resp)
	}
	if mock.Calls != 1 {
		t.Fatalf("VLM calls = %d, want 1", mock.Calls)
	}
	// Sim 截图默认 1920x1080 → 0.5*1920=960, 0.2*1080=216：InputAt 坐标就近定位输入框并填值。
	acts := strings.Join(sess.Actions(), "\n")
	if !strings.Contains(acts, "input_at|960,216|auto-renewable week") {
		t.Fatalf("未按坐标就近输入框填值: %v", acts)
	}
	if strings.Contains(acts, "click_at|960,216") {
		t.Fatalf("InputAt 不应再走点击聚焦回退: %v", acts)
	}
	e, ok := learned.Lookup(vlmPageURL, "missing")
	if !ok || e.Selector != "sim:input:960,216" {
		t.Fatalf("input learned 未回写 selector: %+v ok=%v", e, ok)
	}
}

// TestVLMMainControlInputDirectSelector：VLM 直接给 selector 时不再坐标点击，直接填值。
func TestVLMMainControlInputDirectSelector(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{{
		Action: vlm.ActionInput, Found: true, Text: "产品描述", Selector: "textarea#description",
	}}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)
	resp, err := vlmExecValue(t, env, "missing", "input", "订阅有效期内，可阅读本书城所有短篇故事")
	if err != nil {
		t.Fatalf("VLM input(selector) 应成功: %v", err)
	}
	if !resp.Success {
		t.Fatalf("resp.Success = false")
	}
	acts := strings.Join(sess.Actions(), "\n")
	if !strings.Contains(acts, "input|missing|订阅有效期内，可阅读本书城所有短篇故事|sel=textarea#description") {
		t.Fatalf("未按 VLM selector 填值: %v", acts)
	}
	if e, ok := learned.Lookup(vlmPageURL, "missing"); !ok || e.Selector != "textarea#description" {
		t.Fatalf("input(selector) learned 未回写: %+v ok=%v", e, ok)
	}
}

// TestVLMActionMismatchRejected：请求 input 而 VLM 自报 click 时，物理层不得按自报
// 动作派发一次坐标点击并计为成功（§5.0.9 条款 4：意图-动作相容性守卫）。
// 历史行为：vlmActOnClick 无条件执行点击 ⇒ 假成功 + learned 记下错配动作。
func TestVLMActionMismatchRejected(t *testing.T) {
	mock := &vlm.MockClient{Results: []vlm.Result{{
		Action: vlm.ActionClick, Found: true, Text: "邮箱输入框", XRatio: 0.5, YRatio: 0.2,
	}}}
	learned, _ := vlm.NewLearnedStore("")
	fb := &vlm.Fallback{Enabled: true, Client: mock, Learned: learned, Timeout: 5e9}
	f := &vlmSimFactory{pageURL: vlmPageURL}
	env, sess := vlmEnv(t, fb, f)

	resp, err := vlmExecValue(t, env, "missing", "input", "订阅有效期内")
	if err == nil || resp != nil {
		t.Fatalf("动作错配不得报成功: resp=%+v err=%v", resp, err)
	}
	if st, _ := status.FromError(err); !strings.Contains(st.Message(), FaultVLMActionMismatch) {
		t.Fatalf("Aborted 回执必须携带结构化代号: %v", err)
	}
	acts := strings.Join(sess.Actions(), "\n")
	if strings.Contains(acts, "click_at") || strings.Contains(acts, "input_at") {
		t.Fatalf("动作错配不得派发任何物理事件: %v", acts)
	}
	if e, ok := learned.Lookup(vlmPageURL, "missing"); ok {
		t.Fatalf("动作错配不得写 learned: %+v", e)
	}
}

// 成功出口证据门槛（§5.0.9 条款 5）：vlmOutcomeSuccess 是生产者自报标记，出口必须
// 再校验观测串是否携带**请求动作对应**的页面侧证据。
func TestVLMObservationBacksAction(t *testing.T) {
	cases := []struct {
		expected string
		observed string
		want     bool
	}{
		{"input", "input:missing", true},
		{"input", "input_at:960,216", true},
		{"fill", "input:missing", true},
		{"select", "select:missing", true},
		{"input", "click_at:960,216", false},
		{"click", "click:missing", true},
		{"click", "click_at:960,216", true},
		{"click", "input:missing", false},
		{"click", "", false},
		{"extract", "水,甘油", true},
		{"extract", "", false},
	}
	for _, c := range cases {
		if got := vlmObservationBacksAction(c.expected, c.observed); got != c.want {
			t.Fatalf("vlmObservationBacksAction(%q, %q) = %v，want %v", c.expected, c.observed, got, c.want)
		}
	}
}
