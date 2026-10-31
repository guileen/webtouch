package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestGateway 构造测试网关：fake engine、无等待 sleep、answerStable=0。
func newTestGateway(t *testing.T, engine *fakeEngine) *Gateway {
	t.Helper()
	cfg := DefaultConfig("127.0.0.1:50051")
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	stable := time.Duration(0)
	gw, err := NewGateway(&cfg, Options{
		EngineFactory: func(string) (EngineClient, error) { return engine, nil },
		AnswerStable:  &stable,
		Sleep:         func(time.Duration) {},
		AuthStatePath: filepath.Join(t.TempDir(), "gateway-auth-state.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

func chatReq(channelID, model string, messages ...Message) ChatRequest {
	return ChatRequest{ChannelID: channelID, Model: model, Messages: messages, SessionID: DefaultSessionID}
}

func TestChatHappyPath(t *testing.T) {
	engine := newFakeEngine()
	gw := newTestGateway(t, engine)
	out, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "你好"}))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.Choices[0].Message.Content != "确定性应答" {
		t.Fatalf("answer=%q", out.Choices[0].Message.Content)
	}
	calls := engine.callKinds()
	want := []fakeCall{
		{semantic: "chat_input", action: "input", value: "你好"},
		{semantic: "send_button", action: "click"},
	}
	if len(calls) < len(want)+1 {
		t.Fatalf("调用数不足: %d", len(calls))
	}
	for i, w := range want {
		c := calls[i+1] // 第一个是 baseline extract
		if c.semantic != w.semantic || c.action != w.action || c.value != w.value {
			t.Fatalf("第 %d 步=%+v, want %+v", i, c, w)
		}
	}
	if engine.initNames[0] != "default" {
		t.Fatalf("context_name=%q", engine.initNames[0])
	}
}

func TestChatBlocksLoggedOutAccountBeforeInput(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_out"}`}, nil
		}
		return ActionResult{Success: false, ErrorReason: "unexpected page evaluation"}, nil
	}
	gw := newTestGateway(t, engine)
	_, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "绝不能发送"}))
	authErr, ok := err.(*AuthRequiredError)
	if !ok || authErr.Code != "login_required" {
		t.Fatalf("err=%T %v, want login_required", err, err)
	}
	for _, call := range engine.callKinds() {
		if call.semantic == "chat_input" || call.semantic == "send_button" {
			t.Fatalf("未登录时绝不能输入或发送: %+v", call)
		}
	}
}

func TestChatCachesSuccessfulAuthCheckWithinTTL(t *testing.T) {
	engine := newFakeEngine()
	engine.incrementAnswers = true
	checks := 0
	engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			checks++
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		return ActionResult{Success: true, ExtractedData: `{"status":"not_dispatched","error":"fake direct unavailable"}`}, nil
	}
	gw := newTestGateway(t, engine)
	for _, content := range []string{"第一轮", "第二轮"} {
		if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: content})); err != nil {
			t.Fatalf("Chat(%s): %v", content, err)
		}
	}
	if checks != 1 {
		t.Fatalf("TTL 内应只检查一次登录态，实际 %d 次", checks)
	}
}

func TestChatReasoningModelSupportsSearch(t *testing.T) {
	// 现代 DeepSeek 体系：智能搜索与深度思考相互独立；通用模式（deepseek-chat）
	// 请求 search=true/false 时都要把开关下发到物理层。
	for _, searchVal := range []bool{false, true} {
		engine := newFakeEngine()
		gw := newTestGateway(t, engine)
		search := searchVal
		req := chatReq("deepseek-web", "deepseek-chat",
			Message{Role: "user", Content: "你好"})
		req.Search = &search
		out, err := gw.Chat(context.Background(), req)
		if err != nil {
			t.Fatalf("Chat(search=%v): %v", searchVal, err)
		}
		if out.Choices[0].Message.Content != "确定性应答" {
			t.Fatalf("answer=%q", out.Choices[0].Message.Content)
		}
		var searchSwitchCalled bool
		for _, c := range engine.callKinds() {
			if c.semantic == "deepseek_search_switch" {
				searchSwitchCalled = true
				if searchVal && c.value != "on" {
					t.Fatalf("期望 search 开关为 on，实际: %+v", c)
				}
			}
		}
		if !searchSwitchCalled {
			t.Fatalf("search=%v 时应下发 deepseek_search_switch 开关", searchVal)
		}
		if sess := gw.getSession(DefaultSessionID); sess == nil || sess.Search == nil || *sess.Search != searchVal {
			t.Fatalf("会话应记录 search=%v: %+v", searchVal, sess)
		}
	}
	// 专家模式：页面没有智能搜索开关，请求要忽略（不下发物理动作）。
	engine := newFakeEngine()
	gw := newTestGateway(t, engine)
	yes := true
	req := chatReq("deepseek-web", "deepseek-reasoner", Message{Role: "user", Content: "你好"})
	req.Search = &yes
	if _, err := gw.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat(reasoner, search=true): %v", err)
	}
	for _, c := range engine.callKinds() {
		if c.semantic == "deepseek_search_switch" {
			t.Fatalf("专家模式不应下发 search 开关: %+v", c)
		}
	}
}

func TestModelSearchableFlag(t *testing.T) {
	p, err := newProvider("deepseek", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.modelSearchable("deepseek-reasoner") {
		t.Fatal("deepseek-reasoner（专家模式）页面没有智能搜索开关，应视为不可搜索")
	}
	if !p.modelSearchable("deepseek-chat") {
		t.Fatal("deepseek-chat 应可搜索")
	}
	if !p.modelSearchable("不存在的模型") {
		t.Fatal("未收录模型应保守可搜索")
	}
}

func TestChatStreamEmitsDeltas(t *testing.T) {
	engine := newFakeEngine()
	engine.extractPlan = []extractStep{
		{text: "ab", active: true, done: false},
		{text: "abcd", active: true, done: true},
	}
	gw := newTestGateway(t, engine)
	events, err := gw.ChatStream(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "hi"}))
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	var stop int
	for ev := range events {
		if ev.Err != nil {
			t.Fatalf("流错误: %v", ev.Err)
		}
		if ev.Delta != "" {
			deltas = append(deltas, ev.Delta)
		}
		if ev.FinishReason != "" {
			stop++
		}
	}
	if len(deltas) != 2 || deltas[0] != "ab" || deltas[1] != "cd" {
		t.Fatalf("deltas=%v", deltas)
	}
	if stop != 1 {
		t.Fatalf("stop 次数=%d", stop)
	}
}

func TestChatWaitsForSSEDoneNotPause(t *testing.T) {
	engine := newFakeEngine()
	engine.extractPlan = []extractStep{
		{text: "前半", active: true, done: false},
		{text: "前半完整", active: true, done: true},
	}
	cfg := DefaultConfig("127.0.0.1:50051")
	_ = cfg.Normalize("127.0.0.1:50051")
	stable := time.Hour // 即使文本“稳定”，SSE 未 done 也不得提前返回
	gw, err := NewGateway(&cfg, Options{
		EngineFactory: func(string) (EngineClient, error) { return engine, nil },
		AnswerStable:  &stable,
		Sleep:         func(time.Duration) {},
		ChatTimeout:   5 * time.Second,
		AuthStatePath: filepath.Join(t.TempDir(), "gateway-auth-state.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "hi"}))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := out.Choices[0].Message.Content; got != "前半完整" {
		t.Fatalf("应等到 SSE done 才返回，实际 %q", got)
	}
}

func TestChatGeminiModelSwitchBeforeInput(t *testing.T) {
	engine := newFakeEngine()
	gw := newTestGateway(t, engine)
	req := chatReq("gemini-web", "gemini-3.6-flash", Message{Role: "user", Content: "你好"})
	if _, err := gw.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	calls := engine.callKinds()
	var modelSwitch int
	for i, c := range calls {
		if c.semantic == "gemini_model_switch" {
			modelSwitch++
			if c.value != "Flash" {
				t.Fatalf("模型切换 value=%q, want Flash", c.value)
			}
			if i >= len(calls)-1 || calls[i+1].semantic != "gemini_input" {
				t.Fatalf("模型切换应发生在输入之前: %+v", calls)
			}
		}
	}
	if modelSwitch != 1 {
		t.Fatalf("模型切换次数=%d", modelSwitch)
	}
	// 第二轮同族不重复切换
	engine.incrementAnswers = true // 多轮基线需与新回答不同
	if _, err := gw.Chat(context.Background(), chatReq("gemini-web", "gemini-3.8-flash",
		Message{Role: "user", Content: "再来"})); err != nil {
		t.Fatalf("第二轮 Chat: %v", err)
	}
	if n := countCalls(engine.callKinds(), "gemini_model_switch"); n != 1 {
		t.Fatalf("同族别名不应重复切换，次数=%d", n)
	}
}

func TestChatDeepSeekModeChangeRequiresNewSession(t *testing.T) {
	engine := newFakeEngine()
	gw := newTestGateway(t, engine)
	if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "q1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-expert",
		Message{Role: "user", Content: "q2"})); err != nil {
		t.Fatal(err)
	}
	if len(engine.initNames) != 2 {
		t.Fatalf("模式变更应重建 Tab（init 次数=%d）", len(engine.initNames))
	}
	if len(engine.closeNames) != 1 || engine.closeNames[0] != "default" {
		t.Fatalf("closeNames=%v", engine.closeNames)
	}
	var switched bool
	for _, c := range engine.callKinds() {
		if c.semantic == "deepseek_mode_switch" && c.value == "深度思考" {
			switched = true
		}
	}
	if !switched {
		t.Fatal("应执行 deepseek_mode_switch 深度思考")
	}
}

func TestChatDeadlineRetriesThenSucceeds(t *testing.T) {
	engine := newFakeEngine()
	engine.deadlineExtracts = 2 // 最多 2 次退避后成功
	gw := newTestGateway(t, engine)
	out, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "hi"}))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.Choices[0].Message.Content != "确定性应答" {
		t.Fatalf("answer=%q", out.Choices[0].Message.Content)
	}
}

func TestChatTargetDestroyedRecovery(t *testing.T) {
	engine := newFakeEngine()
	engine.incrementAnswers = true // 重建后的第二轮基线需与新回答不同
	engine.failOnce["chat_input"] = &EngineError{Code: codeAborted, Reason: "target_destroyed: Tab 被用户关闭"}
	gw := newTestGateway(t, engine)
	out, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "hi"}))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.Choices[0].Message.Content != "确定性应答-2" {
		t.Fatalf("answer=%q", out.Choices[0].Message.Content)
	}
	if len(engine.initNames) != 2 {
		t.Fatalf("target_destroyed 应重建 Tab（init 次数=%d）", len(engine.initNames))
	}
	if len(engine.closeNames) != 1 {
		t.Fatalf("closeNames=%v", engine.closeNames)
	}
}

func TestChatRequiresUserMessage(t *testing.T) {
	engine := newFakeEngine()
	gw := newTestGateway(t, engine)
	_, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "assistant", Content: "hi"}))
	be, ok := err.(*BadRequestError)
	if !ok {
		t.Fatalf("期望 BadRequestError，实际 %v", err)
	}
	if be.Reason != "messages 缺少 user 消息" {
		t.Fatalf("reason=%q", be.Reason)
	}
}

// deepseekPatchStream 生成 DeepSeek 补丁协议的自证分片（快照首块 + APPEND 增量 + 收尾）。
// 直发路径按配方声明的 response.kind 分帧，fixture 必须喂真实协议帧而不是裸文本。
func deepseekPatchStream(parts ...string) string {
	var b strings.Builder
	for i, p := range parts {
		if i == 0 {
			b.WriteString(`data: {"v":{"response":{"fragments":[{"id":2,"type":"RESPONSE","content":` + strconv.Quote(p) + `}]}}}`)
		} else {
			b.WriteString("\ndata: " + `{"p":"response/fragments/-1/content","o":"APPEND","v":` + strconv.Quote(p) + `}`)
		}
		b.WriteString("\n\n")
	}
	b.WriteString(`data: {"p":"response/status","o":"SET","v":"FINISHED"}`)
	payload, _ := json.Marshal(map[string]any{"chunks": []string{b.String()}, "done": true})
	return string(payload)
}

// geminiNDJSONStream 生成 Gemini batchexecute NDJSON 分片（累计文本 + 收尾状态帧）。
func geminiNDJSONStream(totals ...string) string {
	lines := []string{")]}'"}
	for _, total := range totals {
		inner := fmt.Sprintf(`[null,["c_x","r_y"],null,null,[["rc_abc",[%s]]]]`, strconv.Quote(total))
		lines = append(lines, fmt.Sprintf(`[["wrb.fr",null,%s]]`, strconv.Quote(inner)))
	}
	lines = append(lines, `[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],{\"44\":true,\"46\":[\"c_x\",\"\"]}]"]]`)
	payload, _ := json.Marshal(map[string]any{"chunks": []string{strings.Join(lines, "\n")}, "done": true})
	return string(payload)
}

func countCalls(calls []fakeCall, semantic string) int {
	n := 0
	for _, c := range calls {
		if c.semantic == semantic {
			n++
		}
	}
	return n
}

func TestDirectTransportChatRoundSuccess(t *testing.T) {
	activateRecipeFixture(t, "deepseek")
	engine := newFakeEngine()
	evalStep := 0
	engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		evalStep++
		if evalStep == 1 {
			return ActionResult{
				Success:       true,
				ExtractedData: `{"status":"streaming_started","ok":true}`,
			}, nil
		}
		if strings.Contains(req.Value, "chunks") {
			return ActionResult{Success: true, ExtractedData: deepseekPatchStream("直发", "增量", "应答")}, nil
		}
		return ActionResult{Success: true}, nil
	}

	gw := newTestGateway(t, engine)
	out, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "直发测试"}))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.Choices[0].Message.Content != "直发增量应答" {
		t.Fatalf("unexpected content: %q", out.Choices[0].Message.Content)
	}
	for _, c := range engine.callKinds() {
		if c.semantic == "chat_input" || c.semantic == "send_button" {
			t.Fatalf("直发成功时不应调用 DOM 动作: %+v", c)
		}
	}
}

func TestDirectTransportStreamRoundSuccess(t *testing.T) {
	activateRecipeFixture(t, "gemini")
	engine := newFakeEngine()
	evalStep := 0
	engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		evalStep++
		if evalStep == 1 {
			return ActionResult{
				Success:       true,
				ExtractedData: `{"status":"streaming_started","ok":true}`,
			}, nil
		}
		if strings.Contains(req.Value, "chunks") {
			return ActionResult{Success: true, ExtractedData: geminiNDJSONStream("流式", "流式直发")}, nil
		}
		return ActionResult{Success: true}, nil
	}

	gw := newTestGateway(t, engine)
	req := chatReq("gemini-web", "gemini-3.6-flash", Message{Role: "user", Content: "流式测试"})
	req.Stream = true
	events, err := gw.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var deltas []string
	for ev := range events {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		if ev.Delta != "" {
			deltas = append(deltas, ev.Delta)
		}
	}
	joined := strings.Join(deltas, "")
	if joined != "流式直发" {
		t.Fatalf("unexpected stream joined: %q", joined)
	}
	for _, c := range engine.callKinds() {
		if c.semantic == "gemini_input" || c.semantic == "gemini_send" {
			t.Fatalf("流式直发时不应调用 DOM 动作: %+v", c)
		}
	}
}

func TestEnsureSessionInjectsProviderProfile(t *testing.T) {
	engine := newFakeEngine()
	gw := newTestGateway(t, engine)

	ch := gw.cfg.Channel("deepseek-web")
	if ch == nil {
		t.Fatal("channel deepseek-web not found")
	}
	sess, err := gw.ensureSession(context.Background(), ch, "prof-test-session", "")
	if err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session")
	}
	prof, err := gw.profileMgr.EnsureProviderProfile("deepseek")
	if err != nil {
		t.Fatalf("EnsureProviderProfile: %v", err)
	}
	if prof.ProfileID != "webtouch-ai-deepseek" {
		t.Fatalf("unexpected profileID: %s", prof.ProfileID)
	}
	if prof.Network.WebRTCPolicy != "disable_non_proxied_udp" {
		t.Fatalf("expected disable_non_proxied_udp, got %s", prof.Network.WebRTCPolicy)
	}
}

func TestEnsureSessionAttachedChromeKeepsNativeBrowserIdentity(t *testing.T) {
	engine := newFakeEngine()
	gw := newTestGateway(t, engine)
	gw.attachCDP = "http://127.0.0.1:9222"

	ch := gw.cfg.Channel("deepseek-web")
	if ch == nil {
		t.Fatal("channel deepseek-web not found")
	}
	if _, err := gw.ensureSession(context.Background(), ch, "attached-profile-test", ""); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	if len(engine.plainInitNames) != 1 || len(engine.profileInitNames) != 0 {
		t.Fatalf("attached Chrome must not receive a provider fingerprint override: plain=%v profiled=%v", engine.plainInitNames, engine.profileInitNames)
	}
}

func TestMultiSessionContextDifferentTabsIsolation(t *testing.T) {
	engine := newFakeEngine()
	engine.incrementAnswers = true
	gw := newTestGateway(t, engine)

	// 1. 发起 Session A 的第一轮对话
	reqA1 := chatReq("deepseek-web", "deepseek-chat", Message{Role: "user", Content: "Session A Round 1"})
	reqA1.SessionID = "session-alpha"
	resA1, err := gw.Chat(context.Background(), reqA1)
	if err != nil {
		t.Fatalf("Chat A1 failed: %v", err)
	}
	if resA1.SessionID != "session-alpha" {
		t.Fatalf("expected session-alpha, got %s", resA1.SessionID)
	}
	if resA1.ContextID != "ctx-session-alpha" {
		t.Fatalf("expected ctx-session-alpha, got %s", resA1.ContextID)
	}
	if resA1.TargetID != "tgt-session-alpha" {
		t.Fatalf("expected tgt-session-alpha, got %s", resA1.TargetID)
	}

	// 2. 发起 Session B 的第一轮对话（不同 context/会话）
	reqB1 := chatReq("deepseek-web", "deepseek-chat", Message{Role: "user", Content: "Session B Round 1"})
	reqB1.SessionID = "session-beta"
	resB1, err := gw.Chat(context.Background(), reqB1)
	if err != nil {
		t.Fatalf("Chat B1 failed: %v", err)
	}
	if resB1.SessionID != "session-beta" {
		t.Fatalf("expected session-beta, got %s", resB1.SessionID)
	}
	if resB1.ContextID != "ctx-session-beta" {
		t.Fatalf("expected ctx-session-beta, got %s", resB1.ContextID)
	}
	if resB1.TargetID != "tgt-session-beta" {
		t.Fatalf("expected tgt-session-beta, got %s", resB1.TargetID)
	}

	// 验证两者 Tab ContextID 与 TargetID 绝对互斥
	if resA1.ContextID == resB1.ContextID {
		t.Fatalf("Session A 与 Session B 不应共用同一个 ContextID Tab: %s", resA1.ContextID)
	}
	if resA1.TargetID == resB1.TargetID {
		t.Fatalf("Session A 与 Session B 不应共用同一个 TargetID: %s", resA1.TargetID)
	}

	// 3. 检查 ListSessions 快照
	snap := gw.ListSessions()
	if len(snap.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(snap.Sessions))
	}
	if snap.Tabs["session-alpha"] != "ctx-session-alpha" || snap.Tabs["session-beta"] != "ctx-session-beta" {
		t.Fatalf("unexpected tabs map: %+v", snap.Tabs)
	}
	if snap.TargetIDs["session-alpha"] != "tgt-session-alpha" || snap.TargetIDs["session-beta"] != "tgt-session-beta" {
		t.Fatalf("unexpected target_ids map: %+v", snap.TargetIDs)
	}

	// 4. 发起 Session A 的第二轮对话，断言复用同一个 Tab
	initCountBeforeA2 := len(engine.initNames)
	reqA2 := chatReq("deepseek-web", "deepseek-chat", Message{Role: "user", Content: "Session A Round 2"})
	reqA2.SessionID = "session-alpha"
	resA2, err := gw.Chat(context.Background(), reqA2)
	if err != nil {
		t.Fatalf("Chat A2 failed: %v", err)
	}
	if resA2.ContextID != resA1.ContextID || resA2.TargetID != resA1.TargetID {
		t.Fatalf("Session A 多轮未复用相同 Tab: round1=%s, round2=%s", resA1.ContextID, resA2.ContextID)
	}
	if len(engine.initNames) != initCountBeforeA2 {
		t.Fatalf("Session A 第二轮不应重新创建 Tab（初始化次数增加）")
	}

	// 5. 发起 Session B 的第二轮对话，断言复用同一个 Tab
	initCountBeforeB2 := len(engine.initNames)
	reqB2 := chatReq("deepseek-web", "deepseek-chat", Message{Role: "user", Content: "Session B Round 2"})
	reqB2.SessionID = "session-beta"
	resB2, err := gw.Chat(context.Background(), reqB2)
	if err != nil {
		t.Fatalf("Chat B2 failed: %v", err)
	}
	if resB2.ContextID != resB1.ContextID || resB2.TargetID != resB1.TargetID {
		t.Fatalf("Session B 多轮未复用相同 Tab: round1=%s, round2=%s", resB1.ContextID, resB2.ContextID)
	}
	if len(engine.initNames) != initCountBeforeB2 {
		t.Fatalf("Session B 第二轮不应重新创建 Tab（初始化次数增加）")
	}

	// 6. 验证 ContextHint["context"] 路由隔离
	for _, call := range engine.calls {
		hintCtx := call.hints["context"]
		if hintCtx != "session-alpha" && hintCtx != "session-beta" {
			t.Fatalf("动作未携带正确的 session 路由 hint: %+v", call)
		}
	}
}

func TestConversationIDAliasMapping(t *testing.T) {
	engine := newFakeEngine()
	gw := newTestGateway(t, engine)

	handler := NewHandler(gw, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// 传入 conversation_id 应当正确建立独立 Tab，而不是回退为 default
	body := `{"model":"deepseek-chat","conversation_id":"conv-room-42","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(ts.URL+"/internal/provider/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("HTTP Post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out ChatCompletion
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SessionID != "conv-room-42" {
		t.Fatalf("expected session_id to be resolved from conversation_id conv-room-42, got: %s", out.SessionID)
	}
	if out.ContextID != "ctx-conv-room-42" {
		t.Fatalf("expected context_id ctx-conv-room-42, got: %s", out.ContextID)
	}
	if out.TargetID != "tgt-conv-room-42" {
		t.Fatalf("expected target_id tgt-conv-room-42, got: %s", out.TargetID)
	}
}

// 通道变了必须重建页签（2026-10-03 派单 d613c7908）：同一 session_id 从 deepseek 转到
// gemini 时不能沿用 DeepSeek 页——否则本通道语义会打在别的站点上，白等一轮再 502。
func TestRequiresNewSessionOnChannelChange(t *testing.T) {
	engine := newFakeEngine()
	gw := newTestGateway(t, engine)
	req := chatReq("gemini-web", "gemini-flash", Message{Role: "user", Content: "hi"})
	p := gw.providers[req.ChannelID]
	if p == nil {
		t.Fatalf("测试配置缺少通道 %q 的 provider", req.ChannelID)
	}
	sess := &Session{ID: DefaultSessionID, ChannelID: "deepseek-web", ContextID: "ctx-1",
		Model: p.resolveModel(req.Model)}
	if !gw.requiresNewSession(req, p, sess) {
		t.Fatal("通道从 deepseek-web 换到 gemini-web 时必须重建页签")
	}
	sess.ChannelID = req.ChannelID
	if gw.requiresNewSession(req, p, sess) {
		t.Fatal("同通道同模型应复用页签（不做无谓重建）")
	}
}
