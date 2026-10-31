package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// siteConvEvaluator 模拟页面读数：location.href 返回给定地址，其余脚本走中性返回。
// failHref=true 时模拟「页内执行器读不到地址」（T-31 fail-closed 用例）。
func siteConvEvaluator(pageURL string, failHref bool) func(ActionRequest) (ActionResult, error) {
	return func(req ActionRequest) (ActionResult, error) {
		switch {
		case strings.Contains(req.Value, "__uws_auth_probe"):
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		case req.Value == "location.href":
			if failHref {
				return ActionResult{}, errors.New("页面已关闭")
			}
			b, _ := json.Marshal(pageURL)
			return ActionResult{Success: true, ExtractedData: string(b)}, nil
		case req.Value == "location.pathname":
			// 清账/回读路径读页面地址（本用例不改它们的行为）。
			return ActionResult{Success: true, ExtractedData: `"/"`}, nil
		default:
			// 与 fakeEngine 默认一致：L0 直发不可用 ⇒ 走 L1 DOM 路径。
			return ActionResult{Success: true, ExtractedData: `{"status":"not_dispatched","error":"fake direct unavailable"}`}, nil
		}
	}
}

// T-31 S1：chat 响应回传站点会话 ID 与页地址（契约 §9）。
func TestChatResponseCarriesSiteConversation(t *testing.T) {
	const conv = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	const pageURL = "https://chat.deepseek.com/a/chat/s/" + conv
	engine := newFakeEngine()
	engine.evaluateFunc = siteConvEvaluator(pageURL, false)
	gw := newTestGateway(t, engine)

	out, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "你好"}))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.SiteConversationID == nil || *out.SiteConversationID != conv {
		t.Fatalf("site_conversation_id=%v, want %q", out.SiteConversationID, conv)
	}
	if out.PageURL == nil || *out.PageURL != pageURL {
		t.Fatalf("page_url=%v, want %q", out.PageURL, pageURL)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"site_conversation_id":"`+conv+`"`) {
		t.Fatalf("响应 JSON 缺 site_conversation_id: %s", b)
	}
	if !strings.Contains(string(b), `"page_url":"`+pageURL+`"`) {
		t.Fatalf("响应 JSON 缺 page_url: %s", b)
	}
}

// T-31 S1 fail-closed：尚无服务端对话 / 读不到地址 / 站点未登记 ⇒ 字段为 null，不猜值。
func TestChatResponseSiteConversationFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		model   string
		pageURL string
		fail    bool
		wantID  string // "" = 期望 null
		wantURL string // "" = 期望 null
	}{
		{name: "新对话尚无 ID", channel: "gemini-web", model: "gemini-flash",
			pageURL: "https://gemini.google.com/app", wantURL: "https://gemini.google.com/app"},
		{name: "读不到页地址", channel: "deepseek-web", model: "deepseek-fast", fail: true},
		{name: "页地址与站点模板不匹配", channel: "deepseek-web", model: "deepseek-fast",
			pageURL: "https://gemini.google.com/app/0f1e2d3c4b5a6978",
			wantURL: "https://gemini.google.com/app/0f1e2d3c4b5a6978"},
		{name: "gemini 对话页", channel: "gemini-web", model: "gemini-flash",
			pageURL: "https://gemini.google.com/app/0f1e2d3c4b5a6978",
			wantID:  "c_0f1e2d3c4b5a6978", wantURL: "https://gemini.google.com/app/0f1e2d3c4b5a6978"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluateFunc = siteConvEvaluator(tc.pageURL, tc.fail)
			gw := newTestGateway(t, engine)
			out, err := gw.Chat(context.Background(), chatReq(tc.channel, tc.model, Message{Role: "user", Content: "你好"}))
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got := derefString(out.SiteConversationID); got != tc.wantID {
				t.Fatalf("site_conversation_id=%q, want %q", got, tc.wantID)
			}
			if got := derefString(out.PageURL); got != tc.wantURL {
				t.Fatalf("page_url=%q, want %q", got, tc.wantURL)
			}
			b, _ := json.Marshal(out)
			if tc.wantID == "" && !strings.Contains(string(b), `"site_conversation_id":null`) {
				t.Fatalf("期望显式 null，实际: %s", b)
			}
			if tc.wantURL == "" && !strings.Contains(string(b), `"page_url":null`) {
				t.Fatalf("期望显式 null，实际: %s", b)
			}
		})
	}
}

// T-31 S1（流式）：完成帧带站点会话身份，增量帧不带。
func TestChatStreamFinishCarriesSiteConversation(t *testing.T) {
	const conv = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	const pageURL = "https://chat.deepseek.com/a/chat/s/" + conv
	engine := newFakeEngine()
	engine.evaluateFunc = siteConvEvaluator(pageURL, false)
	gw := newTestGateway(t, engine)

	events, err := gw.ChatStream(context.Background(), chatReq("deepseek-web", "deepseek-fast",
		Message{Role: "user", Content: "你好"}))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var finish *ChatEvent
	for ev := range events {
		if ev.Err != nil {
			t.Fatalf("流错误: %v", ev.Err)
		}
		if ev.FinishReason != "" {
			e := ev
			finish = &e
			continue
		}
		if ev.SiteConversationID != nil || ev.PageURL != nil {
			t.Fatalf("增量帧不应带站点会话身份: %+v", ev)
		}
	}
	if finish == nil {
		t.Fatalf("未收到完成帧")
	}
	if finish.SiteConversationID == nil || *finish.SiteConversationID != conv {
		t.Fatalf("完成帧 site_conversation_id=%v, want %q", finish.SiteConversationID, conv)
	}
	if finish.PageURL == nil || *finish.PageURL != pageURL {
		t.Fatalf("完成帧 page_url=%v, want %q", finish.PageURL, pageURL)
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---------- S2 按 ID 恢复（契约 §9） ----------

// T-31 S2：带 site_conversation_id 时新页签直接落该对话页；不带则落通道首页；
// 关页后带同一 ID 重建也必须回到该对话页（不新开会话）。
func TestChatRestoresSiteConversationPage(t *testing.T) {
	const conv = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	const convURL = "https://chat.deepseek.com/a/chat/s/" + conv
	const homeURL = "https://chat.deepseek.com/"

	newGW := func() (*Gateway, *fakeEngine) {
		engine := newFakeEngine()
		engine.evaluateFunc = siteConvEvaluator(convURL, false)
		return newTestGateway(t, engine), engine
	}

	t.Run("不带 ID 落通道首页", func(t *testing.T) {
		gw, engine := newGW()
		if _, err := gw.Chat(context.Background(), chatReq("deepseek-web", "deepseek-fast",
			Message{Role: "user", Content: "你好"})); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if urls := engine.initURLsSeen(); len(urls) != 1 || urls[0] != homeURL {
			t.Fatalf("落点=%v, want [%q]", urls, homeURL)
		}
	})

	t.Run("带 ID 落对话页", func(t *testing.T) {
		gw, engine := newGW()
		req := chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "接着聊"})
		req.SiteConversationID = conv
		if _, err := gw.Chat(context.Background(), req); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if urls := engine.initURLsSeen(); len(urls) != 1 || urls[0] != convURL {
			t.Fatalf("落点=%v, want [%q]", urls, convURL)
		}
	})

	t.Run("关页后带同一 ID 重建回该对话页", func(t *testing.T) {
		gw, engine := newGW()
		req := chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "接着聊"})
		req.SiteConversationID = conv
		if _, err := gw.Chat(context.Background(), req); err != nil {
			t.Fatalf("首轮: %v", err)
		}
		gw.closeSession(context.Background(), req.SessionID)
		if _, err := gw.Chat(context.Background(), req); err != nil {
			t.Fatalf("重建轮: %v", err)
		}
		urls := engine.initURLsSeen()
		if len(urls) != 2 || urls[1] != convURL {
			t.Fatalf("重建落点=%v, want 第二跳为 %q", urls, convURL)
		}
	})

	t.Run("请求头等价于 body 字段", func(t *testing.T) {
		engine := newFakeEngine()
		gw := newTestGateway(t, engine)
		h := NewHandler(gw, nil)
		body := `{"channel_id":"deepseek-web","model":"deepseek-fast","messages":[{"role":"user","content":"你好"}]}`
		req := httptest.NewRequest(http.MethodPost, "/internal/provider/chat", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-uws-site-conversation-id", conv)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if urls := engine.initURLsSeen(); len(urls) != 1 || urls[0] != convURL {
			t.Fatalf("落点=%v, want [%q]", urls, convURL)
		}
	})
}

// T-31 S2 fail-closed：站点未登记模板 / ID 非法 / 与 new_session 互斥 ⇒ 400，且不得开页签。
func TestSiteConversationRestoreFailClosed(t *testing.T) {
	const conv = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	tests := []struct {
		name    string
		channel string
		model   string
		id      string
		newSess bool
	}{
		{name: "站点未登记模板", channel: "jimeng-web", model: "jimeng-image", id: conv},
		{name: "ID 非法", channel: "deepseek-web", model: "deepseek-fast", id: "c_0f1e2d3c4b5a6978"},
		{name: "与 new_session 互斥", channel: "deepseek-web", model: "deepseek-fast", id: conv, newSess: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			gw := newTestGateway(t, engine)
			req := chatReq(tc.channel, tc.model, Message{Role: "user", Content: "你好"})
			req.SiteConversationID = tc.id
			req.NewSession = tc.newSess
			_, err := gw.Chat(context.Background(), req)
			if err == nil {
				t.Fatalf("期望 fail-closed 拒绝")
			}
			if _, ok := err.(*BadRequestError); !ok {
				t.Fatalf("期望 BadRequestError，实际 %T: %v", err, err)
			}
			if urls := engine.initURLsSeen(); len(urls) != 0 {
				t.Fatalf("fail-closed 不得开页签，实际落点=%v", urls)
			}
		})
	}
}

// T-31 S2：POST /v1/sessions 接受 site_conversation_id（body / 头），落该对话页；
// 未登记站点一律 400（fail-closed，不落首页冒充恢复）。
func TestSessionsEndpointAcceptsSiteConversationID(t *testing.T) {
	const geminiURL = "https://gemini.google.com/app/0f1e2d3c4b5a6978"
	t.Run("body", func(t *testing.T) {
		h, fake := newTestHandler(t, nil)
		rec := doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{
			"channel_id": "gemini-web", "site_conversation_id": "c_0f1e2d3c4b5a6978",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if urls := fake.initURLsSeen(); len(urls) != 1 || urls[0] != geminiURL {
			t.Fatalf("落点=%v, want [%q]", urls, geminiURL)
		}
	})
	t.Run("头", func(t *testing.T) {
		h, fake := newTestHandler(t, nil)
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions",
			strings.NewReader(`{"channel_id":"gemini-web"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-uws-site-conversation-id", "0f1e2d3c4b5a6978")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if urls := fake.initURLsSeen(); len(urls) != 1 || urls[0] != geminiURL {
			t.Fatalf("落点=%v, want [%q]", urls, geminiURL)
		}
	})
	t.Run("未登记站点拒绝", func(t *testing.T) {
		h, fake := newTestHandler(t, nil)
		rec := doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{
			"channel_id": "jimeng-web", "site_conversation_id": "whatever",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if urls := fake.initURLsSeen(); len(urls) != 0 {
			t.Fatalf("fail-closed 不得开页签，实际落点=%v", urls)
		}
	})
}

// ---------- S3 站点侧栏列表（契约 §9） ----------

// sidebarEvaluator 模拟页内锚点扫描：返回给定 payload（fail=true 模拟读不到页面）。
func sidebarEvaluator(payload string, fail bool) func(ActionRequest) (ActionResult, error) {
	return func(req ActionRequest) (ActionResult, error) {
		if req.Value != sidebarAnchorsScript {
			return ActionResult{Success: true, ExtractedData: "null"}, nil
		}
		if fail {
			return ActionResult{}, errors.New("页面已关闭")
		}
		return ActionResult{Success: true, ExtractedData: payload}, nil
	}
}

// T-31 S3：枚举站点侧栏，只回「能按站点模板归一出 ID」的条目，去重且带标题。
func TestSiteConversationsEndpoint(t *testing.T) {
	payload := `[
	  {"href":"/app/0f1e2d3c4b5a6978","title":"第一条对话"},
	  {"href":"https://gemini.google.com/app/aaaaaaaabbbbbbbb","title":"第二条对话"},
	  {"href":"/app","title":"新对话"},
	  {"href":"https://accounts.google.com/ServiceLogin","title":"登录"},
	  {"href":"/app/0f1e2d3c4b5a6978","title":"重复项"}
	]`
	engine := newFakeEngine()
	engine.evaluateFunc = sidebarEvaluator(payload, false)
	gw := newTestGateway(t, engine)
	addLiveSession(gw, "s-sidebar", "gemini-web")
	h := NewHandler(gw, nil)

	rec := doJSON(t, h, http.MethodGet, "/v1/sessions/s-sidebar/conversations", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out ConversationsResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("不应有 error: %q", out.Error)
	}
	if out.Count != 2 || len(out.Conversations) != 2 {
		t.Fatalf("条数=%d %+v, want 2", out.Count, out.Conversations)
	}
	want := []SiteConversation{
		{ID: "c_0f1e2d3c4b5a6978", Title: "第一条对话"},
		{ID: "c_aaaaaaaabbbbbbbb", Title: "第二条对话"},
	}
	for i, w := range want {
		if out.Conversations[i] != w {
			t.Fatalf("第 %d 条=%+v, want %+v", i, out.Conversations[i], w)
		}
	}
	if out.SessionID != "s-sidebar" || out.Site != "gemini" {
		t.Fatalf("session/site=%q/%q", out.SessionID, out.Site)
	}
}

// T-31 S3 fail-closed：读不到侧栏 / 返回值无法解析 ⇒ 空数组 + error，不虚构。
func TestSiteConversationsFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		fail    bool
	}{
		{name: "读不到页面", payload: "", fail: true},
		{name: "返回值无法解析", payload: "not-json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluateFunc = sidebarEvaluator(tc.payload, tc.fail)
			gw := newTestGateway(t, engine)
			addLiveSession(gw, "s-bad", "deepseek-web")
			h := NewHandler(gw, nil)

			rec := doJSON(t, h, http.MethodGet, "/v1/sessions/s-bad/conversations", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var out ConversationsResult
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("解析响应: %v", err)
			}
			if out.Error == "" {
				t.Fatalf("期望 error 非空")
			}
			if out.Count != 0 || len(out.Conversations) != 0 {
				t.Fatalf("期望空数组，实际 %+v", out.Conversations)
			}
			if !strings.Contains(rec.Body.String(), `"conversations":[]`) {
				t.Fatalf("契约要求空数组（非 null）: %s", rec.Body.String())
			}
		})
	}
}

// T-31 S3：未知会话回 404（不假装是空列表）。
func TestSiteConversationsUnknownSession(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodGet, "/v1/sessions/nope/conversations", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// T-31 S1（契约 §9 字段级冻结）：`/v1/responses` 对外面共用同一回填点——
// 非流式顶层字段恒定出现（读不到为 null）；流式只在 response.completed 出现。
func TestResponsesAPICarriesSiteConversation(t *testing.T) {
	const conv = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	const pageURL = "https://chat.deepseek.com/a/chat/s/" + conv
	body := func(stream bool) string {
		return fmt.Sprintf(`{"model":"deepseek-fast","stream":%t,"input":"你好"}`, stream)
	}

	t.Run("非流式顶层字段", func(t *testing.T) {
		engine := newFakeEngine()
		engine.evaluateFunc = siteConvEvaluator(pageURL, false)
		gw := newTestGateway(t, engine)
		h := NewHandler(gw, nil)

		rec := doJSON(t, h, http.MethodPost, "/internal/provider/responses", map[string]any{
			"model": "deepseek-fast", "input": "你好",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			SiteConversationID *string `json:"site_conversation_id"`
			PageURL            *string `json:"page_url"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析响应: %v", err)
		}
		if derefString(out.SiteConversationID) != conv || derefString(out.PageURL) != pageURL {
			t.Fatalf("responses 回传=%v/%v, want %q/%q", out.SiteConversationID, out.PageURL, conv, pageURL)
		}
	})

	t.Run("流式只在 response.completed 出现", func(t *testing.T) {
		engine := newFakeEngine()
		engine.evaluateFunc = siteConvEvaluator(pageURL, false)
		gw := newTestGateway(t, engine)
		h := NewHandler(gw, nil)

		req := httptest.NewRequest(http.MethodPost, "/internal/provider/responses", strings.NewReader(body(true)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var completed map[string]any
		for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
			if !strings.Contains(frame, "response.completed") {
				continue
			}
			line := ""
			for _, l := range strings.Split(frame, "\n") {
				if strings.HasPrefix(l, "data: ") {
					line = strings.TrimPrefix(l, "data: ")
				}
			}
			if line == "" {
				continue
			}
			if err := json.Unmarshal([]byte(line), &completed); err != nil {
				t.Fatalf("解析完成帧: %v (%s)", err, line)
			}
			if resp, ok := completed["response"].(map[string]any); ok {
				delete(completed, "response")
				for k, v := range resp {
					completed[k] = v
				}
			}
		}
		if completed == nil {
			t.Fatalf("未收到 response.completed: %s", rec.Body.String())
		}
		if got := derefAnyString(completed["site_conversation_id"]); got != conv {
			t.Fatalf("response.completed site_conversation_id=%v, want %q", completed["site_conversation_id"], conv)
		}
		if got := derefAnyString(completed["page_url"]); got != pageURL {
			t.Fatalf("response.completed page_url=%v, want %q", completed["page_url"], pageURL)
		}
		// 冻结纪律：完成之前的帧不得带该字段。
		for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
			if strings.Contains(frame, "response.output_text.delta") && strings.Contains(frame, "site_conversation_id") {
				t.Fatalf("增量帧不得带站点会话身份: %s", frame)
			}
		}
	})
}

// T-31 S1（契约 §9 取值来源②，2026-10-01 修订）：L0 直发不驱动站点 SPA 路由，
// 页地址恒为通道首页（`/app`）⇒ 会话 ID 只能从直发响应体自报的帧里取；
// 页地址仍如实回传（这是真机 C1 之前恒为 null 的结构性根因）。
func TestChatDirectCarriesSiteConversationFromResponseBody(t *testing.T) {
	activateRecipeFixture(t, "gemini")
	const conv = "c_2d64e591ca5edc51"
	const pageURL = "https://gemini.google.com/app"
	ndjsonStream := `)]}'

177
[["wrb.fr",null,"[null,[\"c_2d64e591ca5edc51\",\"r_6ed42fdcf06117f5\"],{\"18\":\"r_6ed42fdcf06117f5\",\"44\":true}]"]]
200
[["wrb.fr",null,"[null,[\"c_2d64e591ca5edc51\",\"r_6ed42fdcf06117f5\"],null,null,[[\"rc_abc\",[\"Gemini direct answer\"]]]]"]]
90
[["wrb.fr",null,"[null,[\"c_2d64e591ca5edc51\",\"r_6ed42fdcf06117f5\"],{\"44\":true,\"46\":[\"c_2d64e591ca5edc51\",\"\"],\"52\":[]}]"]]
26
[["e",9,null,null,4839]]
`
	pollCount := 0
	var scripts []string
	e := newFakeEngine()
	e.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		scripts = append(scripts, req.Value)
		switch {
		case strings.Contains(req.Value, "__uws_auth_probe"):
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		case req.Value == "location.href":
			return ActionResult{Success: true, ExtractedData: fmt.Sprintf("%q", pageURL)}, nil
		case req.Value == "location.pathname":
			return ActionResult{Success: true, ExtractedData: `"/app"`}, nil
		case strings.Contains(req.Value, "StreamGenerate"):
			return ActionResult{Success: true, ExtractedData: `{"status":"streaming_started","ok":true}`}, nil
		case strings.Contains(req.Value, "chunks"):
			pollCount++
			if pollCount == 1 {
				return ActionResult{Success: true, ExtractedData: fmt.Sprintf(`{"chunks":[%q],"done":false}`, ndjsonStream)}, nil
			}
			return ActionResult{Success: true, ExtractedData: `{"chunks":[],"done":true}`}, nil
		default:
			return ActionResult{Success: true, ExtractedData: `{}`}, nil
		}
	}

	g := newTestGateway(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := g.Chat(ctx, chatReq("gemini-web", "gemini-flash", Message{Role: "user", Content: "2+3=?"}))
	if err != nil {
		t.Fatalf("Gemini 直发失败: %v", err)
	}
	if resp.Transport != transportInPageDirect {
		t.Fatalf("期望走 L0 直发，实际 transport=%q", resp.Transport)
	}
	if got := derefString(resp.SiteConversationID); got != conv {
		t.Fatalf("直发响应体自报 ID 未回填: got %q, want %q", got, conv)
	}
	if got := derefString(resp.PageURL); got != pageURL {
		t.Fatalf("页地址未如实回传: got %q, want %q", got, pageURL)
	}
	// L0 不驱动 SPA 路由 ⇒ 学到 ID 后必须把页签地址对齐到对话页，否则 recipe 的 from_url
	// 槽位下一轮读空、站点每轮新开对话（真机实测三轮三个 ID）。
	want := `history.replaceState(null, '', "/app/2d64e591ca5edc51")`
	for _, sc := range scripts {
		if strings.Contains(sc, want) {
			return
		}
	}
	t.Fatalf("未把页签地址对齐到会话页（期望脚本含 %s）；已执行脚本：%v", want, scripts)
}

// T-31 S1（契约 §9 取值来源③，2026-10-02 修订三）：DeepSeek 的会话 ID 由 recipe 的
// `session_mint` 生产（create 响应里），**不在响应流里** ⇒ 来源② 结构性读不到；回填来源是
// 启动响应的 echo 槽位（本轮请求体实际写进去的 `chat_session_id`）。学到 ID 后页签地址
// 仍要对齐到 `/a/chat/s/<uuid>`，让下一轮 `session_mint` 的 `re` 直接从地址取值、不再新建会话。
func TestChatDirectCarriesSiteConversationFromRecipeSlotEcho(t *testing.T) {
	activateRecipeFixture(t, "deepseek")
	const conv = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	const pageURL = "https://chat.deepseek.com/"
	stream := "data: {\"type\":\"text_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"DeepSeek direct answer\"}}\n\n" +
		"data: {\"type\":\"answer_finished\",\"finish_reason\":\"stop\"}\n\n"
	pollCount := 0
	var scripts []string
	e := newFakeEngine()
	e.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		scripts = append(scripts, req.Value)
		switch {
		case strings.Contains(req.Value, "__uws_auth_probe"):
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		case req.Value == "location.href":
			return ActionResult{Success: true, ExtractedData: fmt.Sprintf("%q", pageURL)}, nil
		case req.Value == "location.pathname":
			return ActionResult{Success: true, ExtractedData: `"/"`}, nil
		case strings.Contains(req.Value, "chat_session/create"):
			return ActionResult{Success: true, ExtractedData: fmt.Sprintf(
				`{"status":"streaming_started","ok":true,"httpStatus":200,"slots":{"chat_session_id":%q}}`, conv)}, nil
		case strings.Contains(req.Value, ".chunks.slice("):
			pollCount++
			if pollCount == 1 {
				return ActionResult{Success: true, ExtractedData: fmt.Sprintf(`{"chunks":[%q],"done":false}`, stream)}, nil
			}
			return ActionResult{Success: true, ExtractedData: `{"chunks":[],"done":true}`}, nil
		default:
			return ActionResult{Success: true, ExtractedData: `{}`}, nil
		}
	}

	g := newTestGateway(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := g.Chat(ctx, chatReq("deepseek-web", "deepseek-chat", Message{Role: "user", Content: "2+3=?"}))
	if err != nil {
		t.Fatalf("DeepSeek 直发失败: %v", err)
	}
	if resp.Transport != transportInPageDirect {
		t.Fatalf("期望走 L0 直发，实际 transport=%q", resp.Transport)
	}
	if got := derefString(resp.SiteConversationID); got != conv {
		t.Fatalf("recipe 槽位回显未回填: got %q, want %q", got, conv)
	}
	if got := derefString(resp.PageURL); got != pageURL {
		t.Fatalf("页地址未如实回传: got %q, want %q", got, pageURL)
	}
	want := `history.replaceState(null, '', "/a/chat/s/3fa85f64-5717-4562-b3fc-2c963f66afa6")`
	for _, sc := range scripts {
		if strings.Contains(sc, want) {
			return
		}
	}
	t.Fatalf("未把页签地址对齐到 DeepSeek 会话页（期望脚本含 %s）；已执行脚本：%v", want, scripts)
}

// 硬边界：站点模板未登记 / ID 形状不符 ⇒ 一律 null（不把垃圾串当 ID 回给调用方）。
func TestNormalizeSiteConversationIDFailClosed(t *testing.T) {
	cases := []struct {
		site, raw, want string
		ok              bool
	}{
		{"gemini", "c_2d64e591ca5edc51", "c_2d64e591ca5edc51", true},
		{"gemini", "2D64E591CA5EDC51", "c_2d64e591ca5edc51", true},
		{"gemini", "https://gemini.google.com/app/2d64e591ca5edc51", "c_2d64e591ca5edc51", true},
		{"gemini", "c_x", "", false},
		{"gemini", "c_2d64e591ca5edc5", "", false},
		{"gemini", "", "", false},
		{"claude", "c_2d64e591ca5edc51", "", false},
		{"deepseek", "3fa85f64-5717-4562-b3fc-2c963f66afa6", "3fa85f64-5717-4562-b3fc-2c963f66afa6", true},
	}
	for _, tc := range cases {
		got, ok := NormalizeSiteConversationID(tc.site, tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("NormalizeSiteConversationID(%q,%q)=(%q,%v), want (%q,%v)", tc.site, tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func derefAnyString(v any) string {
	s, _ := v.(string)
	return s
}
