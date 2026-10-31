package recipe

import (
	"strings"
	"testing"

	"uws/apps/gateway/recipes"
)

// ChatGPT 模板是「按真机实测重写」的产物（契约 §13，2026-10-02）：两态 body、哨兵两步、
// 正文轮询三件事必须有机器可核验的落点——模板存在 ≠ 站点可用，但模板写错必须当场红。
func TestChatGPTRecipeWiring(t *testing.T) {
	data, err := recipes.FS.ReadFile("chatgpt.json")
	if err != nil {
		t.Fatalf("读取内嵌 chatgpt.json 失败: %v", err)
	}
	r, err := FromJSON(data)
	if err != nil {
		t.Fatalf("chatgpt.json 未通过校验（DRAFT 也不许结构不合法）: %v", err)
	}
	if r.Status != StatusDraft {
		t.Fatalf("未跑第 9 条自证回放前必须保持 DRAFT，实际 %q", r.Status)
	}
	if r.Response.Kind != ResponseKindSSEHandoffPoll || r.Response.Poll == nil {
		t.Fatalf("响应分帧必须声明轮询补正文: kind=%q poll=%v", r.Response.Kind, r.Response.Poll)
	}
	if !strings.Contains(r.Response.Poll.URLTemplate, "{{conversation_id}}") {
		t.Fatalf("轮询地址必须带 {{conversation_id}}: %q", r.Response.Poll.URLTemplate)
	}
	byName := map[string]Slot{}
	for _, s := range r.Slots {
		byName[s.Name] = s
	}
	for _, want := range []struct{ name, producer string }{
		{"access_token", ProducerFetchField},
		{"sentinel", ProducerFetchField},
		{"parent_message_id", ProducerFetchField},
		{"conversation_id", ProducerFromURL},
		{"create_time", ProducerNowEpoch},
	} {
		s, ok := byName[want.name]
		if !ok {
			t.Fatalf("缺少槽位 %q", want.name)
		}
		if s.Producer != want.producer {
			t.Fatalf("槽位 %q 的 producer 应为 %q，实际 %q", want.name, want.producer, s.Producer)
		}
	}
	if got := byName["parent_message_id"]; got.Select != SelectLastAssistantMessageID || got.Fallback != "client-created-root" {
		t.Fatalf("parent_message_id 必须取「最后一条助手消息 id」并以 client-created-root 兜底: %+v", got)
	}
	tmpl, ok := r.Request.BodyBuilders["body"].Template.(map[string]any)
	if !ok {
		t.Fatalf("body_builders.body.template 形状异常: %T", r.Request.BodyBuilders["body"].Template)
	}
	node, ok := tmpl["conversation_id"].(map[string]any)
	if !ok {
		t.Fatalf("body 模板必须显式声明 conversation_id 的两态节点: %#v", tmpl["conversation_id"])
	}
	if node["omit_if_empty"] != "conversation_id" {
		t.Fatalf("首轮必须整键不落 conversation_id（omit_if_empty）: %#v", node)
	}
	if byName["sentinel"].Body == "" || !strings.Contains(byName["sentinel"].Body, "{{prepare_token}}") {
		t.Fatalf("哨兵 finalize 必须引用上一步的 prepare_token: %+v", byName["sentinel"])
	}
}

func TestBuildFetchScriptSupportsPollAndOmit(t *testing.T) {
	r := &Recipe{
		Site:   "chatgpt",
		Status: StatusDraft,
		Match:  MatchRule{Origin: "https://chatgpt.com"},
		Request: RequestSpec{
			Method:      "POST",
			URLTemplate: "https://chatgpt.com/backend-api/f/conversation",
			BodyBuilders: map[string]BodyBuilder{
				"body": {Kind: "json_object", Template: map[string]any{
					"conversation_id": map[string]any{"omit_if_empty": "conversation_id"},
				}},
			},
		},
		Slots: []Slot{{Name: "create_time", Producer: ProducerNowEpoch}},
		Response: ResponseSpec{Kind: ResponseKindSSEHandoffPoll, Poll: &PollSpec{
			URLTemplate: "https://chatgpt.com/backend-api/conversation/{{conversation_id}}",
		}},
	}
	script, err := BuildFetchScript(r, "ping", "gpt-6-pro", "q")
	if err != nil {
		t.Fatalf("BuildFetchScript: %v", err)
	}
	for _, want := range []string{
		`hasOwnProperty.call(node, "omit_if_empty")`,
		"async function runAnswerPoll",
		`case "now_epoch":`,
		"poll_conversation_id_missing",
		"__uws_poll_answer",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("生成的页内脚本缺少 %q", want)
		}
	}
}

func TestPollChunkMarkers(t *testing.T) {
	if ans, ok := PollAnswerFromChunk(`{"__uws_poll_answer":"收到"}`); !ok || ans != "收到" {
		t.Fatalf("PollAnswerFromChunk = (%q, %v)", ans, ok)
	}
	if _, ok := PollAnswerFromChunk("data: {\"type\":\"stream_handoff\"}"); ok {
		t.Fatal("协议帧不得被误判成正文")
	}
	if _, ok := PollAnswerFromChunk(`{"__uws_poll_answer":""}`); ok {
		t.Fatal("空正文不算解出答案")
	}
	if msg, ok := PollErrorFromChunk(`{"__uws_poll_error":"poll_timeout"}`); !ok || msg != "poll_timeout" {
		t.Fatalf("PollErrorFromChunk = (%q, %v)", msg, ok)
	}
}

func TestRecipeValidateNewProducers(t *testing.T) {
	base := func(slots []Slot, resp ResponseSpec) *Recipe {
		return &Recipe{
			Site:     "x",
			Request:  RequestSpec{Method: "POST", URLTemplate: "https://e/x"},
			Slots:    slots,
			Response: resp,
		}
	}
	if err := base([]Slot{{Name: "t", Producer: ProducerNowEpoch}}, ResponseSpec{Kind: "json"}).Validate(); err != nil {
		t.Fatalf("now_epoch 应合法: %v", err)
	}
	if err := base([]Slot{{Name: "p", Producer: ProducerFetchField, Endpoint: "https://e/a", Path: "a"}},
		ResponseSpec{Kind: "json"}).Validate(); err != nil {
		t.Fatalf("fetch_field+path 应合法: %v", err)
	}
	if err := base([]Slot{{Name: "p", Producer: ProducerFetchField, Endpoint: "https://e/a"}},
		ResponseSpec{Kind: "json"}).Validate(); err == nil {
		t.Fatal("fetch_field 缺 path/select 必须报错")
	}
	if err := base([]Slot{{Name: "p", Producer: ProducerFetchField, Endpoint: "https://e/a", Select: "nope"}},
		ResponseSpec{Kind: "json"}).Validate(); err == nil {
		t.Fatal("未知 select 必须报错")
	}
	if err := base(nil, ResponseSpec{Kind: ResponseKindSSEHandoffPoll}).Validate(); err == nil {
		t.Fatal("sse_handoff_poll 缺 poll 规格必须报错")
	}
	if err := base(nil, ResponseSpec{Kind: "json", Poll: &PollSpec{URLTemplate: "https://e/c"}}).Validate(); err != nil {
		t.Fatalf("非 poll 分帧下的 poll 字段应被忽略: %v", err)
	}
}
