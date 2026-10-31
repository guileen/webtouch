package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ── R4：同 intent_id 在途去重（不重复物理执行） ──────────────────────────

// blockingSession 让物理执行可挂起，用于确定性验证在途去重。
type blockingSession struct {
	*SimCDPSession
	startOnce sync.Once
	started   chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	execCount int
}

func newBlockingSession(id string, cfg browser.Config) *blockingSession {
	return &blockingSession{
		SimCDPSession: NewSimCDPSession(id, cfg),
		started:       make(chan struct{}),
		release:       make(chan struct{}),
	}
}

func (s *blockingSession) ExecuteAction(_ context.Context, action, value, semantic string) (bool, string) {
	s.mu.Lock()
	s.execCount++
	s.mu.Unlock()
	s.startOnce.Do(func() { close(s.started) })
	<-s.release
	return true, "blocked"
}

func (s *blockingSession) Executed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execCount
}

type blockingFactory struct {
	sessions []*blockingSession
}

func (f *blockingFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s := newBlockingSession(fmt.Sprintf("block-%d", len(f.sessions)+1), cfg)
	f.sessions = append(f.sessions, s)
	return s, nil
}

// selectorRecordingSession 记录 executePhysical 传入的选择器，验证 R-A 接线：
// 知识库规则 Selector → 引擎 → SelectorActionSession。
type selectorRecordingSession struct {
	*SimCDPSession
	mu       sync.Mutex
	gotAct   string
	gotValue string
	gotSem   string
	gotSel   string
	calls    int
}

func (s *selectorRecordingSession) ExecuteAction(_ context.Context, action, value, semantic string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotAct, s.gotValue, s.gotSem, s.gotSel = action, value, semantic, ""
	s.calls++
	return true, "sem:" + semantic
}

func (s *selectorRecordingSession) ExecuteActionWithSelector(_ context.Context, action, value, semantic, selector string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotAct, s.gotValue, s.gotSem, s.gotSel = action, value, semantic, selector
	s.calls++
	return true, "sel:" + selector
}

type selectorRecordingFactory struct {
	sessions []*selectorRecordingSession
}

func (f *selectorRecordingFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s := &selectorRecordingSession{
		SimCDPSession: NewSimCDPSession(fmt.Sprintf("sel-%d", len(f.sessions)+1), cfg),
	}
	f.sessions = append(f.sessions, s)
	return s, nil
}

// sseChatFactory 记录会话，供「extract 优先消费聊天 SSE 快照」测试注入
// browser.ChatStreamSource 模拟数据（方案 1：网络层干净文本替代 DOM）。
type sseChatFactory struct {
	sessions []*SimCDPSession
}

func (f *sseChatFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	s := NewSimCDPSession(fmt.Sprintf("sse-%d", len(f.sessions)+1), cfg)
	f.sessions = append(f.sessions, s)
	return s, nil
}

// debugInfoSession 包装 SimCDPSession 并实现 browser.TargetDebugInfo：
// 模拟真实 CDP 会话暴露真实 targetId 与页面级调试 URL。
type debugInfoSession struct {
	*SimCDPSession
	targetID string
	debugURL string
}

func (s *debugInfoSession) TargetID() string     { return s.targetID }
func (s *debugInfoSession) DebugPageURL() string { return s.debugURL }

type debugInfoFactory struct {
	sessions []*debugInfoSession
}

func (f *debugInfoFactory) Create(_ context.Context, cfg browser.Config) (browser.CDPSession, error) {
	n := len(f.sessions) + 1
	s := &debugInfoSession{
		SimCDPSession: NewSimCDPSession(fmt.Sprintf("dbg-%d", n), cfg),
		targetID:      fmt.Sprintf("real-cdp-target-%d", n),
		debugURL:      fmt.Sprintf("ws://127.0.0.1:9222/devtools/page/real-cdp-target-%d", n),
	}
	f.sessions = append(f.sessions, s)
	return s, nil
}

// 前置（2026-08-27）：InitializeContext 必须把真实 CDP targetId 暴露给客户端
// （TargetId = 真实 targetId；SessionUrl = 页面级调试端点；ContextId 仍为 Pool
// 内部 ID），且 CaptureScreenshot 按新 TargetId 仍能正确路由。
func TestInitContextExposesRealTargetID(t *testing.T) {
	factory := &debugInfoFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Pool: browser.NewPool(1, factory)})
	ctx := mdCtx("coordinator-key")

	resp, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{TaskId: "task-dbg"})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if resp.TargetId != "real-cdp-target-1" {
		t.Errorf("TargetId 应为真实 CDP targetId，got %q", resp.TargetId)
	}
	if resp.ContextId == "" || resp.ContextId == resp.TargetId {
		t.Errorf("ContextId 应为独立 Pool 内部 ID，got %q (TargetId=%q)", resp.ContextId, resp.TargetId)
	}
	if resp.SessionUrl != "ws://127.0.0.1:9222/devtools/page/real-cdp-target-1" {
		t.Errorf("SessionUrl 应为真实页面调试端点，got %q", resp.SessionUrl)
	}

	// 路由回归：CaptureScreenshot 用新的 TargetId 必须命中同一上下文。
	shot, err := env.client.CaptureScreenshot(ctx, &uwspb.CaptureScreenshotRequest{TargetId: resp.TargetId})
	if err != nil {
		t.Fatalf("CaptureScreenshot(真实 targetId) 应路由成功: %v", err)
	}
	if len(shot.Png) == 0 {
		t.Errorf("截图字节不应为空")
	}
}

// 方案 1（2026-08-25）：extract + context_hint["prefer_sse"]="1" 时，引擎必须
// 返回网络层 SSE 快照的干净文本（Epoch>0），而不是 DOM observed；无 hint 或
// Epoch=0（尚未捕获聊天流）保持既有回退语义。
func TestExtractPrefersChatSSESnapshot(t *testing.T) {
	cases := []struct {
		name     string
		hint     map[string]string
		snap     browser.ChatStreamSnapshot
		want     string
		wantDone bool
	}{
		{
			name: "prefer_sse 且已有捕获 → 干净 SSE 文本",
			hint: map[string]string{"prefer_sse": "1"},
			snap: browser.ChatStreamSnapshot{
				Content: "没有-1-4-7角标污染的干净回答",
				Epoch:   3,
				Done:    true,
			},
			want:     "没有-1-4-7角标污染的干净回答",
			wantDone: true,
		},
		{
			name: "prefer_sse 生成中（有内容未结束）→ 返回内容但 done=false",
			hint: map[string]string{"prefer_sse": "1"},
			snap: browser.ChatStreamSnapshot{
				Content: "半截回答",
				Epoch:   2,
				Done:    false,
			},
			want:     "半截回答",
			wantDone: false,
		},
		{
			name:     "prefer_sse 空快照且未结束（思考/生成中）→ 显式空串，等 SSE",
			hint:     map[string]string{"prefer_sse": "1"},
			snap:     browser.ChatStreamSnapshot{Epoch: 4},
			want:     "",
			wantDone: false,
		},
		{
			// 空响应收尾（如重复/被页面作废的请求、getResponseBody 空 body）：
			// 快照已 done 但无内容，SSE 通道不可用 → 回退 DOM observed，
			// 不能继续返回空串让 API 轮询到超时（2026-08-26 实测修复）。
			name:     "空快照且已 done（空响应收尾）→ 回退 DOM，不带完成标记",
			hint:     map[string]string{"prefer_sse": "1"},
			snap:     browser.ChatStreamSnapshot{Epoch: 5, Done: true},
			want:     "extract:chat_answer",
			wantDone: false,
		},
		{
			// loadingFailed/中止/响应体拉取失败：即便快照残留半截内容也不可信，
			// 回退 DOM observed（页面可能已渲染完整回答），不带完成标记。
			name: "捕获轮失败（Failed）→ 回退 DOM，不带完成标记",
			hint: map[string]string{"prefer_sse": "1"},
			snap: browser.ChatStreamSnapshot{
				Content: "半截回答",
				Epoch:   2,
				Failed:  true,
			},
			want:     "extract:chat_answer",
			wantDone: false,
		},
		{
			name:     "无 prefer_sse hint → 保持 observed 回退",
			snap:     browser.ChatStreamSnapshot{Epoch: 2, Content: "不应被消费"},
			want:     "extract:chat_answer",
			wantDone: false,
		},
		{
			name:     "prefer_sse 但 Epoch=0（未捕获）→ 回退 observed",
			hint:     map[string]string{"prefer_sse": "1"},
			snap:     browser.ChatStreamSnapshot{},
			want:     "extract:chat_answer",
			wantDone: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := &sseChatFactory{}
			env := newTestEnvWithOptions(t, Options{
				Keys: testKeys(),
				Pool: browser.NewPool(1, factory),
			})
			initCtx(t, env.client, "coordinator-key")
			factory.sessions[0].SetChatStream(tc.snap, nil)

			resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"),
				&uwspb.ActionIntentRequest{
					IntentId:       "sse-extract-1",
					TargetSemantic: "chat_answer",
					ExpectedAction: "extract",
					ContextHint:    tc.hint,
				})
			if err != nil {
				t.Fatalf("ExecuteAction: %v", err)
			}
			if !resp.Success {
				t.Fatalf("动作应成功: %+v", resp)
			}
			if got := resp.GetExtractedData(); got != tc.want {
				t.Fatalf("extracted_data = %q, want %q", got, tc.want)
			}
			if got := resp.GetChatStreamDone(); got != tc.wantDone {
				t.Fatalf("chat_stream_done = %v, want %v", got, tc.wantDone)
			}
		})
	}
}

// R-A（2026-08-25）：内置知识库含 DeepSeek chat 规则（带 Selector）时，
// ExecuteAction("chat_input") 必须把规则选择器传给物理层 SelectorActionSession。
func TestExecutePhysicalUsesRuleSelector(t *testing.T) {
	factory := &selectorRecordingFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Pool: browser.NewPool(1, factory)})
	initCtx(t, env.client, "coordinator-key")

	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "chat-1", TargetSemantic: "chat_input", ExpectedAction: "input", Value: proto.String("你好"),
	})
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !resp.Success {
		t.Fatalf("动作应成功: %+v", resp)
	}
	s := factory.sessions[0]
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gotSel == "" {
		t.Fatal("应把知识库规则选择器传给物理层（gotSel 为空）")
	}
	if s.gotSel != "textarea[placeholder*='发送消息'], textarea" {
		t.Fatalf("选择器不符: %q", s.gotSel)
	}
	if s.gotSem != "chat_input" || s.gotAct != "input" || s.gotValue != "你好" {
		t.Fatalf("动作参数不符: act=%q val=%q sem=%q", s.gotAct, s.gotValue, s.gotSem)
	}
}

// 多轮/新会话修复（2026-08-25）：new_chat_button 规则必须携带 text= 选择器
// 贯通物理层（DeepSeek「开启新对话」是纯 div，只能按文本定位）。
func TestExecutePhysicalUsesNewChatTextSelector(t *testing.T) {
	factory := &selectorRecordingFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Pool: browser.NewPool(1, factory)})
	initCtx(t, env.client, "coordinator-key")

	_, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "new-chat-1", TargetSemantic: "new_chat_button", ExpectedAction: "click",
	})
	if err != nil {
		t.Fatalf("ExecuteAction(new_chat_button): %v", err)
	}
	s := factory.sessions[0]
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gotSel != "text=开启新对话" {
		t.Fatalf("new_chat_button 应携带 text= 选择器，got %q", s.gotSel)
	}
	if s.gotSem != "new_chat_button" || s.gotAct != "click" {
		t.Fatalf("动作参数不符: act=%q sem=%q", s.gotAct, s.gotSem)
	}
}

func TestInflightDedupSameIntent(t *testing.T) {
	factory := &blockingFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), Pool: browser.NewPool(2, factory)})
	initCtx(t, env.client, "coordinator-key")

	req := &uwspb.ActionIntentRequest{
		IntentId: "dedup-1", TargetSemantic: "登录", ExpectedAction: "click",
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := env.client.ExecuteAction(mdCtx("coordinator-key"), req)
		firstDone <- err
	}()
	sess := factory.sessions[0]
	<-sess.started // 第一次已进入物理执行

	// 在途重复提交同 intent_id：不得二次物理执行，返回 DEADLINE_EXCEEDED。
	if _, err := env.client.ExecuteAction(mdCtx("coordinator-key"), req); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("在途重复提交应 DEADLINE_EXCEEDED，got %v", err)
	}

	close(sess.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("第一次执行应成功: %v", err)
	}
	if got := sess.Executed(); got != 1 {
		t.Fatalf("物理执行应恰好 1 次，got %d", got)
	}

	// 完成后同 intent 再提交 → 幂等缓存命中，不再物理执行。
	r3, err := env.client.ExecuteAction(mdCtx("coordinator-key"), req)
	if err != nil {
		t.Fatalf("完成后再提交应命中缓存: %v", err)
	}
	if !r3.Success || sess.Executed() != 1 {
		t.Fatalf("缓存命中后应成功且不重复执行: resp=%+v executed=%d", r3, sess.Executed())
	}
}

// ── R5：同 Target 并发 ExecuteAction 无数据竞争（页面状态串行化） ────────

func TestConcurrentExecuteActionSameTarget(t *testing.T) {
	env := newTestEnv(t)
	initCtx(t, env.client, "coordinator-key")

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
				IntentId: fmt.Sprintf("c-%d", i), TargetSemantic: "登录", ExpectedAction: "click",
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发执行应全部成功: %v", err)
		}
	}
}

// ── WSTN 消费端 Shadow 旁路审计单测 ──────────────────────────────────────

func TestWSTNShadowAudit(t *testing.T) {
	env := newTestEnv(t)
	initCtx(t, env.client, "coordinator-key")

	req := &uwspb.ActionIntentRequest{
		IntentId:       "wstn-shadow-1",
		TargetSemantic: "登录",
		ExpectedAction: "click",
	}

	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), req)
	if err != nil {
		t.Fatalf("ExecuteAction failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("ExecuteAction was not successful")
	}

	total, valid := env.srv.WSTNShadowStats()
	if total == 0 {
		t.Errorf("expected wstn shadow total > 0, got %d", total)
	}
	if valid == 0 {
		t.Errorf("expected wstn shadow valid > 0, got %d", valid)
	}
}
