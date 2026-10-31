package recipe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"uws/apps/gateway/recipes"
)

func TestRecipeDSL_ValidationAndSerialization(t *testing.T) {
	r := &Recipe{
		Site: "gemini",
		Match: MatchRule{
			Origin: "https://gemini.google.com",
			PathRe: "^/app",
		},
		Request: RequestSpec{
			Method:      "POST",
			URLTemplate: "/_/BardChatUi/data/StreamGenerate?bl={{bl}}",
			Headers:     map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		},
		Slots: []Slot{
			{Name: "bl", Producer: ProducerPageGlobal, Path: "WIZ_global_data.cfb2h"},
			{Name: "prompt", Producer: ProducerPrompt},
		},
		Response: ResponseSpec{
			Kind: "ndjson_wrb_fr",
		},
	}

	if err := r.Validate(); err != nil {
		t.Fatalf("有效 Recipe 校验失败: %v", err)
	}

	data, err := r.ToJSON()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	parsed, err := FromJSON(data)
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	if parsed.Site != "gemini" || parsed.Response.Kind != "ndjson_wrb_fr" {
		t.Fatalf("反序列化字段不匹配: %+v", parsed)
	}

	// 反例测试：缺少必填字段
	invalid := &Recipe{Site: ""}
	if err := invalid.Validate(); err == nil {
		t.Fatal("缺少 site 应该报错")
	}

	invalidSlot := &Recipe{
		Site:     "test",
		Request:  RequestSpec{Method: "POST", URLTemplate: "/api"},
		Response: ResponseSpec{Kind: "json"},
		Slots:    []Slot{{Name: "token", Producer: ProducerPageGlobal}}, // missing path
	}
	if err := invalidSlot.Validate(); err == nil {
		t.Fatal("page_global 缺少 path 应该报错")
	}
}

// TestRecipeDSL_AsyncProducersValidation 锁定 2026-10-01 DeepSeek 倒逼的三样扩展的校验口径。
func TestRecipeDSL_AsyncProducersValidation(t *testing.T) {
	base := func(slots []Slot) *Recipe {
		return &Recipe{
			Site:     "deepseek",
			Request:  RequestSpec{Method: "POST", URLTemplate: "/api/v0/chat/completion"},
			Response: ResponseSpec{Kind: "sse_data_json"},
			Slots:    slots,
		}
	}
	if err := base([]Slot{{Name: "authorization", Producer: ProducerLocalStorage, Key: "userToken", Decode: "json", Path: "value"}}).Validate(); err != nil {
		t.Fatalf("local_storage 槽位应合法: %v", err)
	}
	// local_storage 的 key 可缺省（回退到槽位名），decode 必须是 json 才有两层取值语义
	if err := base([]Slot{{Name: "device_id", Producer: ProducerLocalStorage}}).Validate(); err != nil {
		t.Fatalf("local_storage 缺 key 应回退到槽位名: %v", err)
	}
	if err := base([]Slot{{Name: "pow", Producer: ProducerPowChallenge, Endpoint: "/api/v0/chat/create_pow_challenge"}}).Validate(); err == nil {
		t.Fatal("pow_challenge 缺 chunk 应该报错")
	}
	if err := base([]Slot{{Name: "pow", Producer: ProducerPowChallenge, Chunk: "https://x/static/a.js"}}).Validate(); err == nil {
		t.Fatal("pow_challenge 缺 endpoint 应该报错")
	}
	if err := base([]Slot{{Name: "sid", Producer: ProducerSessionMint}}).Validate(); err == nil {
		t.Fatal("session_mint 缺 endpoint 应该报错")
	}
}

// TestDeepSeekRecipe_RealShape 锁定 DeepSeek 两态直发的 DSL 形态（真实抓包见
// docs/evidence/pilots/deepseek-direct-send-probe-2026-10-01.md §三）。
func TestDeepSeekRecipe_RealShape(t *testing.T) {
	data, err := recipes.FS.ReadFile("deepseek.json")
	if err != nil {
		t.Fatalf("读取内嵌 deepseek recipe 失败: %v", err)
	}
	r, err := FromJSON(data)
	if err != nil {
		t.Fatalf("deepseek recipe 校验失败: %v", err)
	}
	if r.Status != StatusDraft {
		t.Fatalf("未验收站点必须保持 DRAFT，实际 %s", r.Status)
	}
	if r.Request.URLTemplate != "/api/v0/chat/completion" {
		t.Fatalf("主请求应指向 completion，实际 %s", r.Request.URLTemplate)
	}
	if _, ok := r.Request.BodyBuilders["body"]; !ok {
		t.Fatal("DeepSeek 请求体应走 json_object 构造器")
	}
	byName := map[string]Slot{}
	for _, s := range r.Slots {
		byName[s.Name] = s
	}
	if got := byName["authorization"]; got.Producer != ProducerLocalStorage || got.Key != "userToken" || got.Decode != "json" || got.Path != "value" {
		t.Fatalf("authorization 槽位应是 localStorage 两层取值，实际 %+v", got)
	}
	if got := byName["chat_session_id"]; got.Producer != ProducerSessionMint || !got.Echo {
		t.Fatalf("chat_session_id 应走 session_mint 且回显，实际 %+v", got)
	}
	if got := byName["pow_response"]; got.Producer != ProducerPowChallenge || got.TargetPath != "/api/v0/chat/completion" || got.Chunk == "" {
		t.Fatalf("pow_response 槽位形态不对: %+v", got)
	}
	if _, ok := byName["model"]; ok {
		t.Fatal("真实请求体没有 model 字段，不得凭空声明 model 槽位")
	}

	script, err := BuildFetchScript(r, "2+3=? 只回一个数字", "deepseek-chat", "__q")
	if err != nil {
		t.Fatalf("构造直发脚本失败: %v", err)
	}
	for _, want := range []string{"producePowChallenge", "produceSessionMint", "localStorage.getItem", "pow-challenge", "expireAt", "json_object"} {
		if !strings.Contains(script, want) {
			t.Fatalf("直发脚本缺少 %q", want)
		}
	}
}

func TestBuildFetchScript_GeminiShape(t *testing.T) {

	r := &Recipe{
		Site: "gemini",
		Match: MatchRule{
			Origin: "https://gemini.google.com",
			PathRe: "^/app",
		},
		Request: RequestSpec{
			Method:      "POST",
			URLTemplate: "/_/BardChatUi/data/StreamGenerate?bl={{bl}}&f.sid={{fsid}}&_reqid={{reqid}}&rt=c",
			Headers: map[string]string{
				"Content-Type":  "application/x-www-form-urlencoded;charset=UTF-8",
				"X-Same-Domain": "1",
			},
			BodyTemplate: "f.req={{f.req}}&at={{at}}",
			BodyBuilders: map[string]BodyBuilder{
				"f.req": {
					Kind: "json_outer",
				},
			},
		},
		Slots: []Slot{
			{Name: "at", Producer: ProducerPageGlobal, Path: "WIZ_global_data.SNlM0e"},
			{Name: "fsid", Producer: ProducerPageGlobal, Path: "WIZ_global_data.FdrFJe"},
			{Name: "bl", Producer: ProducerPageGlobal, Path: "WIZ_global_data.cfb2h"},
			{Name: "reqid", Producer: ProducerCounter, Step: 100000},
			{Name: "conv", Producer: ProducerFromURL, Re: `^/app/([0-9a-f]{16})`, Fallback: nil},
		},
		Response: ResponseSpec{
			Kind: "ndjson_wrb_fr",
		},
	}

	script, err := BuildFetchScript(r, "你好", "gemini-flash", "test_queue_123")
	if err != nil {
		t.Fatalf("生成脚本失败: %v", err)
	}

	if !strings.Contains(script, "test_queue_123") {
		t.Error("脚本中应包含指定的 queueKey")
	}
	if !strings.Contains(script, "WIZ_global_data.SNlM0e") {
		t.Error("脚本中应包含槽位求值路径 WIZ_global_data.SNlM0e")
	}
	if !strings.Contains(script, "window.fetch") {
		t.Error("脚本中应包含原生 window.fetch 调用")
	}
	if !strings.Contains(script, "not_dispatched") {
		t.Error("脚本中应包含 slot_missing 时的 not_dispatched 防御")
	}
	// 错误体必须带上游来源：读流阶段中断与提交动作失败要能分辨（Gemini 502 network error 定位用）。
	if !strings.Contains(script, "stream_interrupted_after_headers") {
		t.Error("脚本中应包含读流中断的错误体阶段标注")
	}
	if !strings.Contains(script, "fetch_failed[") {
		t.Error("脚本中应包含 fetch 失败时按派发阶段标注错误体")
	}
}

func TestSynthesizer_GeminiCaptureTrace(t *testing.T) {
	// 复用 2026-09-26 嗅探案卷真实样本
	trace := InteractionTrace{
		Site:          "gemini",
		Prompt:        "2+3=? 只回一个数字。",
		RenderedText:  "5",
		RequestURL:    "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=boq_assistant-bard-web-server_20260925.04_p0&f.sid=642958603962486721&hl=en&_reqid=3446439&rt=c",
		RequestMethod: "POST",
		RequestHeaders: map[string]string{
			"Content-Type":             "application/x-www-form-urlencoded;charset=UTF-8",
			"X-Same-Domain":            "1",
			"x-goog-ext-73010989-jspb": "[0]",
		},
		RequestBody:    "f.req=sample&at=AOvx0lJPfBfhycpNQDvcr4iRvfX7%3A1790427236188",
		ResponseStatus: 200,
		ResponseHeaders: map[string]string{
			"Content-Type": "application/json",
		},
		ResponseBody: `)]}'

177
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],null,null,[[\"rc_abc\",[\"5\"]]]]"]]
26
[["e",9,null,null,4839]]
`,
		PageSnapshot: PageContextSnapshot{
			URL:      "https://gemini.google.com/app/25096c6ba2166662",
			Pathname: "/app/25096c6ba2166662",
			Window: map[string]any{
				"WIZ_global_data": map[string]any{
					"SNlM0e": "AOvx0lJPfBfhycpNQDvcr4iRvfX7", // 目标前缀
					"FdrFJe": "642958603962486721",
					"cfb2h":  "boq_assistant-bard-web-server_20260925.04_p0",
				},
			},
		},
	}

	synth := NewSynthesizer()
	rec, err := synth.Synthesize(trace)
	if err != nil {
		t.Fatalf("因果推导失败: %v", err)
	}

	if rec.Site != "gemini" {
		t.Errorf("推导站点不符: %s", rec.Site)
	}
	if rec.Response.Kind != "ndjson_wrb_fr" {
		t.Errorf("响应结构探测不符: %s", rec.Response.Kind)
	}

	// 核验推导出的 slots
	slotMap := make(map[string]Slot)
	for _, s := range rec.Slots {
		slotMap[s.Name] = s
	}

	// 1. at 槽位
	atSlot, ok := slotMap["at"]
	if !ok || atSlot.Producer != ProducerPageGlobal || atSlot.Path != "WIZ_global_data.SNlM0e" {
		t.Errorf("at 槽位 BFS 推导不符: %+v", atSlot)
	}

	// 2. bl 槽位
	blSlot, ok := slotMap["bl"]
	if !ok || blSlot.Producer != ProducerPageGlobal || blSlot.Path != "WIZ_global_data.cfb2h" {
		t.Errorf("bl 槽位 BFS 推导不符: %+v", blSlot)
	}

	// 3. f.sid 槽位
	fsidSlot, ok := slotMap["f_sid"]
	if !ok || fsidSlot.Producer != ProducerPageGlobal || fsidSlot.Path != "WIZ_global_data.FdrFJe" {
		t.Errorf("f.sid 槽位 BFS 推导不符: %+v", fsidSlot)
	}

	// 4. reqid 槽位
	reqidSlot, ok := slotMap["reqid"]
	if !ok || reqidSlot.Producer != ProducerCounter {
		t.Errorf("reqid 槽位推导不符: %+v", reqidSlot)
	}
}

type fakeDirectEvaluator struct {
	evalFunc func(ctx context.Context, script string) ([]byte, error)
}

func (f *fakeDirectEvaluator) EvaluateScript(ctx context.Context, script string) ([]byte, error) {
	return f.evalFunc(ctx, script)
}

func TestVerifier_SelfProvingGate(t *testing.T) {
	v := NewVerifier()
	newRecipe := func() *Recipe {
		return &Recipe{
			Site:  "gemini",
			Match: MatchRule{Origin: "https://gemini.google.com"},
			Request: RequestSpec{
				Method:      "POST",
				URLTemplate: "/api/test",
			},
			Slots: []Slot{
				{Name: "prompt", Producer: ProducerPrompt},
			},
			Response: ResponseSpec{Kind: "json"},
		}
	}
	// 真实 Gemini NDJSON 形态（anti-XSSI + 长度前缀 + wrb.fr 帧 + e 结束帧）。
	answerFrames := ")]}'\n\n177\n" +
		`[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],null,null,[[\"rc_abc\",[\"Gemini direct answer\"]]]]"]]` + "\n\n26\n" +
		`[["e",9,null,null,4839]]` + "\n"

	t.Run("解出非空回答文本才置 ACTIVE", func(t *testing.T) {
		r := newRecipe()
		fakeSuccess := &fakeDirectEvaluator{
			evalFunc: func(ctx context.Context, script string) ([]byte, error) {
				if strings.Contains(script, "window.fetch") {
					return []byte(`{"status":"streaming_started","ok":true,"httpStatus":200}`), nil
				}
				return []byte(fmt.Sprintf(`{"chunks":[%s],"done":true}`, jsonQuote(answerFrames))), nil
			},
		}
		res, err := v.VerifyRecipe(context.Background(), fakeSuccess, r, "test")
		if err != nil {
			t.Fatalf("自证门禁应该通过: %v", err)
		}
		if r.Status != StatusActive || res.Status != StatusActive {
			t.Fatalf("自证通过后状态应为 ACTIVE，got recipe=%s result=%s", r.Status, res.Status)
		}
		if res.Answer != "Gemini direct answer" {
			t.Fatalf("激活凭证里的回答文本不符: %q", res.Answer)
		}
		if res.HTTPStatus != 200 {
			t.Fatalf("激活凭证里的 HTTP 状态不符: %d", res.HTTPStatus)
		}
	})

	t.Run("只收到非文本帧且流已结束 => DRIFTED", func(t *testing.T) {
		r := newRecipe()
		fakeEmpty := &fakeDirectEvaluator{
			evalFunc: func(ctx context.Context, script string) ([]byte, error) {
				if strings.Contains(script, "window.fetch") {
					return []byte(`{"status":"streaming_started","ok":true,"httpStatus":200}`), nil
				}
				return []byte(fmt.Sprintf(`{"chunks":[%s],"done":true}`, jsonQuote(")]}'\n\n26\n[[\"e\",9,null,null,4839]]\n"))), nil
			},
		}
		res, err := v.VerifyRecipe(context.Background(), fakeEmpty, r, "test")
		if err == nil {
			t.Fatal("未解出回答文本不应通过自证")
		}
		if r.Status != StatusDrifted || res.Answer != "" {
			t.Fatalf("状态应为 DRIFTED 且无回答文本，got %s / %q", r.Status, res.Answer)
		}
		if got := v.RecentDrifts(); len(got) == 0 || got[len(got)-1].Reason != "no_answer_text" {
			t.Fatalf("漂移原因应为 no_answer_text: %+v", got)
		}
	})

	t.Run("未派发（槽位缺失）=> DRIFTED", func(t *testing.T) {
		r := newRecipe()
		fakeFail := &fakeDirectEvaluator{
			evalFunc: func(ctx context.Context, script string) ([]byte, error) {
				return []byte(`{"status":"not_dispatched","error":"slot_missing: at"}`), nil
			},
		}
		if _, err := v.VerifyRecipe(context.Background(), fakeFail, r, "test"); err == nil {
			t.Fatal("未派发自证应该失败")
		}
		if r.Status != StatusDrifted {
			t.Fatalf("自证失败后状态应为 DRIFTED，got %s", r.Status)
		}
		drifts := v.RecentDrifts()
		if len(drifts) == 0 || drifts[len(drifts)-1].Site != "gemini" {
			t.Fatalf("漂移现场未记录: %+v", drifts)
		}
	})
}

// jsonQuote 把一段文本编码成 JSON 字符串字面量（嵌进 fake 的 chunks 数组）。
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func TestRegistry_DefaultRecipesLoading(t *testing.T) {
	reg := DefaultRegistry()
	list := reg.List()
	if len(list) < 5 {
		t.Fatalf("内嵌 Recipe 数量应至少为 5 (gemini, deepseek, chatgpt, claude, perplexity)，got %d", len(list))
	}

	sites := []string{"gemini", "deepseek", "chatgpt", "claude", "perplexity"}
	for _, s := range sites {
		rec, ok := reg.Get(s)
		if !ok || rec == nil {
			t.Errorf("缺少默认 Recipe: %s", s)
			continue
		}
		if rec.Status != StatusDraft {
			t.Errorf("默认 Recipe %s 状态应为 DRAFT，got %s", s, rec.Status)
		}
	}

	// URL 查找验证
	r, ok := reg.FindByURL("https://gemini.google.com/app/25096c6ba2166662")
	if !ok || r.Site != "gemini" {
		t.Fatalf("FindByURL gemini 失败: %+v", r)
	}

	r, ok = reg.FindByURL("https://chat.deepseek.com/")
	if !ok || r.Site != "deepseek" {
		t.Fatalf("FindByURL deepseek 失败: %+v", r)
	}

	r, ok = reg.FindByURL("https://www.perplexity.ai/search/123")
	if !ok || r.Site != "perplexity" {
		t.Fatalf("FindByURL perplexity 失败: %+v", r)
	}
}

// TestGeminiRecipeTemplateShape 锁定 gemini 请求体形状（回归自 2026-10-01 真站抓包两轮）。
//
// 事实：内层是 99 元素数组，非 null 位固定；会话 ID 落在 index 71（不是早期猜测的 index 2）；
// index 3/4/59 每轮现生成；index 17 首轮 [[0]] / 有会话 [[1]]。
func TestGeminiRecipeTemplateShape(t *testing.T) {
	r, ok := DefaultRegistry().Get("gemini")
	if !ok {
		t.Fatal("缺少 gemini recipe")
	}
	bb, ok := r.Request.BodyBuilders["f.req"]
	if !ok {
		t.Fatal("缺少 f.req 构造器")
	}
	outer, ok := bb.Template.([]any)
	if !ok || len(outer) != 2 || outer[0] != nil {
		t.Fatalf("外层模板应为 [null, {json_string: …}]，got %#v", bb.Template)
	}
	wrapper, ok := outer[1].(map[string]any)
	if !ok {
		t.Fatalf("外层第二位应为 json_string 包裹对象，got %#v", outer[1])
	}
	sparse, ok := wrapper["json_string"].(map[string]any)["sparse_array"].(map[string]any)
	if !ok {
		t.Fatalf("内层应为 sparse_array 结构，got %#v", wrapper)
	}
	if size, _ := sparse["size"].(float64); int(size) != 99 {
		t.Errorf("内层长度应为 99，got %v", sparse["size"])
	}
	set, ok := sparse["set"].(map[string]any)
	if !ok {
		t.Fatal("sparse_array 缺少 set")
	}
	// 抓包实测的非 null 位（index 71 仅在后续轮出现）
	want := []string{"0", "1", "3", "4", "6", "7", "10", "11", "17", "18", "27", "30",
		"41", "49", "53", "59", "61", "67", "68", "71", "79", "80", "91", "96", "98"}
	for _, k := range want {
		if _, ok := set[k]; !ok {
			t.Errorf("内层 index %s 缺失（实测非 null 位）", k)
		}
	}
	if len(set) != len(want) {
		t.Errorf("非 null 位数量应为 %d，got %d", len(want), len(set))
	}
	// 会话位：index 71 必须是「有会话才填」的分支
	convNode, ok := set["71"].(map[string]any)
	if !ok || convNode["if_slot"] != "conv" {
		t.Fatalf("index 71 应为 if_slot=conv 的两态分支，got %#v", set["71"])
	}
	convArr, ok := convNode["then"].([]any)
	if !ok || len(convArr) != 8 || convArr[0] != "{{conv}}" {
		t.Errorf("index 71 then 分支应为 8 元素且首位为会话 ID 占位，got %#v", convNode["then"])
	}
	if convNode["else"] != nil {
		t.Error("index 71 首轮分支应为 null")
	}
	for _, idx := range []string{"3", "4", "59"} {
		if _, ok := set[idx].(string); !ok {
			t.Errorf("index %s 应为槽位占位字符串，got %#v", idx, set[idx])
		}
	}
	// 生成脚本必须带上新模板机制与新生产者（页面侧求值靠它们）
	script, err := BuildFetchScript(r, "你好", "gemini-flash", "q")
	if err != nil {
		t.Fatalf("生成脚本失败: %v", err)
	}
	for _, marker := range []string{"resolveTemplateNode", "sparse_array", "fresh_blob", "fresh_hex", "if_slot"} {
		if !strings.Contains(script, marker) {
			t.Errorf("脚本缺少模板机制标记 %q", marker)
		}
	}
}

// TestRecipeDSL_OptionalSlotFirstTurn 覆盖首轮两态：无会话 ID 时 conv 槽位应可缺失。
// 回归自 2026-10-01 真站自证现场：`/app` 无 ID ⇒ 曾被判 `slot_missing: conv` 而 fail-closed。
func TestRecipeDSL_OptionalSlotFirstTurn(t *testing.T) {
	r, ok := DefaultRegistry().Get("gemini")
	if !ok {
		t.Fatal("内置 gemini recipe 缺失")
	}
	var conv *Slot
	for i := range r.Slots {
		if r.Slots[i].Name == "conv" {
			conv = &r.Slots[i]
		}
	}
	if conv == nil || !conv.Optional {
		t.Fatalf("会话 ID 槽位应标 optional（首轮本就无 ID）: %+v", conv)
	}
	script, err := BuildFetchScript(r, "2+3=?", "", "__uws_optional_test")
	if err != nil {
		t.Fatalf("生成脚本失败: %v", err)
	}
	if !strings.Contains(script, "slot.optional") {
		t.Fatal("生成脚本缺少可选槽位分支：首轮会被误判为 slot_missing")
	}
}

// 契约 §9 取值来源③：只有 `producer: session_mint` 且 `echo: true` 的槽位才是
// 「站点会话 ID 槽位」。DeepSeek 命中，Gemini（走 from_url + 来源②）不命中。
func TestSessionEchoSlotNames(t *testing.T) {
	ds, ok := DefaultRegistry().Get("deepseek")
	if !ok {
		t.Fatal("内置 deepseek recipe 缺失")
	}
	names := ds.SessionEchoSlotNames()
	if len(names) != 1 || names[0] != "chat_session_id" {
		t.Fatalf("DeepSeek 会话槽位 = %v, want [chat_session_id]", names)
	}
	gm, ok := DefaultRegistry().Get("gemini")
	if !ok {
		t.Fatal("内置 gemini recipe 缺失")
	}
	if names := gm.SessionEchoSlotNames(); len(names) != 0 {
		t.Fatalf("Gemini 不应命中 session_mint 会话槽位（走 from_url + 来源②）: %v", names)
	}
	var nilRecipe *Recipe
	if names := nilRecipe.SessionEchoSlotNames(); len(names) != 0 {
		t.Fatalf("nil recipe 应为空: %v", names)
	}
}

// 契约 §13 request.signer：规范 2026-10-02 定（千问 qwenSign），实现 2026-10-02 接。
func TestRecipeDSL_SignerValidation(t *testing.T) {
	base := func(signer *SignerSpec) *Recipe {
		return &Recipe{
			Site:     "qianwen",
			Request:  RequestSpec{Method: "POST", URLTemplate: "https://chat2.qianwen.com/api/v2/chat", Signer: signer},
			Response: ResponseSpec{Kind: "sse_data_json"},
		}
	}
	if err := base(nil).Validate(); err != nil {
		t.Fatalf("未声明 signer 应合法: %v", err)
	}
	if err := base(&SignerSpec{Path: "__QIANWEN_CHAT_SDK__.qwenSign", URLFrom: "signedUrl", HeadersFrom: "signedHeader", TimeoutMs: 10000}).Validate(); err != nil {
		t.Fatalf("完整 signer 应合法: %v", err)
	}
	if err := base(&SignerSpec{URLFrom: "signedUrl"}).Validate(); err == nil {
		t.Fatal("signer 缺 path 应报错（fail-closed，不许静默跳过签名）")
	}
	if err := base(&SignerSpec{Path: "   "}).Validate(); err == nil {
		t.Fatal("signer 的 path 只有空白也应报错")
	}
}

// 判据（契约 §13）：无 signer 的脚本不得含签名调用；有 signer 的脚本必须
// 「取页内函数 → 覆盖 url → 并入签名头」，且失败路径是 not_dispatched。
func TestBuildFetchScript_SignerShape(t *testing.T) {
	noSigner := &Recipe{
		Site:     "plain",
		Request:  RequestSpec{Method: "POST", URLTemplate: "/api/x"},
		Response: ResponseSpec{Kind: "json"},
	}
	script, err := BuildFetchScript(noSigner, "hi", "", "__uws_signer_none")
	if err != nil {
		t.Fatalf("生成脚本失败: %v", err)
	}
	if strings.Contains(script, "signer_not_function") || strings.Contains(script, "signedUrl") {
		t.Fatal("未声明 signer 的脚本不应含签名调用")
	}

	withSigner := &Recipe{
		Site: "qianwen",
		Match: MatchRule{
			Origin: "https://www.qianwen.com",
		},
		Request: RequestSpec{
			Method:      "POST",
			URLTemplate: "https://chat2.qianwen.com/api/v2/chat?biz_id=ai_qwen",
			Headers:     map[string]string{"Content-Type": "application/json"},
			Signer: &SignerSpec{
				Path:        "__QIANWEN_CHAT_SDK__.qwenSign",
				URLFrom:     "signedUrl",
				HeadersFrom: "signedHeader",
				TimeoutMs:   10000,
			},
		},
		Slots:    []Slot{{Name: "prompt", Producer: ProducerPrompt}},
		Response: ResponseSpec{Kind: "sse_data_json"},
	}
	script, err = BuildFetchScript(withSigner, "你好", "", "__uws_signer_qw")
	if err != nil {
		t.Fatalf("生成脚本失败: %v", err)
	}
	for _, want := range []string{
		"__QIANWEN_CHAT_SDK__.qwenSign",
		"signer_not_function",
		"signer_timeout",
		"signedUrl",
		"signedHeader",
		`url = signedUrl`,
		`status: "not_dispatched", error: msg`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("带 signer 的脚本缺少 %q", want)
		}
	}
	// 覆盖 URL 必须发生在 fetch 之前，否则签名结果没被用上。
	if strings.Index(script, "url = signedUrl") > strings.Index(script, "await window.fetch(url, {") {
		t.Fatal("签名覆盖 URL 必须早于 window.fetch 派发")
	}
}

// 契约 §13 fetch_field（2026-10-02 千问后续轮 topic_id 倒逼）：值只在服务端、
// 页面无落点时的「派发前只读取数」生产者。
func TestRecipeDSL_FetchFieldValidation(t *testing.T) {
	base := func(slot Slot) *Recipe {
		return &Recipe{
			Site:     "qianwen",
			Request:  RequestSpec{Method: "POST", URLTemplate: "https://chat2.qianwen.com/api/v2/chat"},
			Response: ResponseSpec{Kind: "sse_data_json"},
			Slots:    []Slot{slot},
		}
	}
	ok := Slot{Name: "topic_existing", Producer: ProducerFetchField, Endpoint: "https://chat2-api.qianwen.com/api/v1/session/get", Path: "data.topic_id", Optional: true}
	if err := base(ok).Validate(); err != nil {
		t.Fatalf("完整 fetch_field 应合法: %v", err)
	}
	if err := base(Slot{Name: "t", Producer: ProducerFetchField, Path: "data.topic_id"}).Validate(); err == nil {
		t.Fatal("fetch_field 缺 endpoint 应报错")
	}
	if err := base(Slot{Name: "t", Producer: ProducerFetchField, Endpoint: "/api/x"}).Validate(); err == nil {
		t.Fatal("fetch_field 缺 path 应报错")
	}
}

// 判据：脚本里必须有 fetch_field 的取值实现与分支；未声明该 producer 时不得出现。
func TestBuildFetchScript_FetchFieldShape(t *testing.T) {
	r := &Recipe{
		Site: "qianwen",
		Match: MatchRule{
			Origin: "https://www.qianwen.com",
			PathRe: "^/chat",
		},
		Request: RequestSpec{
			Method:      "POST",
			URLTemplate: "https://chat2.qianwen.com/api/v2/chat?biz_id=ai_qwen",
			Headers:     map[string]string{"Content-Type": "application/json"},
			Signer:      &SignerSpec{Path: "__QIANWEN_CHAT_SDK__.qwenSign", URLFrom: "signedUrl", HeadersFrom: "signedHeader"},
			BodyBuilders: map[string]BodyBuilder{
				"body": {Kind: "json_object", Template: map[string]any{
					"session_id":  "{{session_id}}",
					"topic_id":    map[string]any{"if_slot": "topic_existing", "then": "{{topic_existing}}", "else": "{{topic_new}}"},
					"scene_param": map[string]any{"if_slot": "topic_existing", "then": "continue_chat", "else": "first_turn"},
					"prompt":      "{{prompt}}",
				}},
			},
		},
		Slots: []Slot{
			{Name: "session_id", Producer: ProducerFromURL, Re: `^/chat/([0-9a-f]{32})`, Prefix: ""},
			{Name: "topic_existing", Producer: ProducerFetchField, Endpoint: "https://chat2-api.qianwen.com/api/v1/session/get?ut={{x}}", Body: `{"session_id":"{{session_id}}"}`, Path: "data.topic_id", Optional: true},
			{Name: "topic_new", Producer: ProducerFreshBlob, Bytes: 24},
			{Name: "prompt", Producer: ProducerPrompt},
		},
		Response: ResponseSpec{Kind: "sse_data_json"},
	}
	script, err := BuildFetchScript(r, "只回一个数字，7乘6", "", "__uws_ff_qw")
	if err != nil {
		t.Fatalf("生成脚本失败: %v", err)
	}
	for _, want := range []string{"produceFetchField", "fetch_field_missing", "renderSlots", "if_slot"} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本缺少 %q", want)
		}
	}
	// 生产者实现随模板常驻（与 pow_challenge/session_mint 同一设计），
	// 「用不用」由 recipe JSON 决定；这里只锁 recipe 侧确实声明了 fetch_field 槽位。
	use := false
	for _, sl := range r.Slots {
		if sl.Producer == ProducerFetchField {
			use = true
		}
	}
	if !use {
		t.Fatal("该 recipe 应声明 fetch_field 槽位")
	}
}

// 千问 recipe 形状锁（2026-10-02 接站）：契约 §13 千问条目 + SOP 十条。
func TestQianwenRecipe_RealShape(t *testing.T) {
	data, err := recipes.FS.ReadFile("qianwen.json")
	if err != nil {
		t.Fatalf("读取内嵌 qianwen recipe 失败: %v", err)
	}
	r, err := FromJSON(data)
	if err != nil {
		t.Fatalf("qianwen recipe 校验失败: %v", err)
	}
	if r.Status != StatusDraft {
		t.Fatalf("未过自证回放必须保持 DRAFT，实际 %s", r.Status)
	}
	if r.Match.Origin != "https://www.qianwen.com" {
		t.Fatalf("match.origin 必须取实际站（tongyi.com 会重定向）: %s", r.Match.Origin)
	}
	if r.Request.Signer == nil || r.Request.Signer.Path != "__QIANWEN_CHAT_SDK__.qwenSign" {
		t.Fatalf("必须走页面自带签名器 qwenSign: %+v", r.Request.Signer)
	}
	byName := map[string]Slot{}
	for _, s := range r.Slots {
		byName[s.Name] = s
	}
	if got := byName["ut"]; got.Producer != ProducerCookie || got.Key != "b-user-id" {
		t.Fatalf("ut 槽位应取 cookie b-user-id（session/get 不带 ut 会 400 code:10001）: %+v", got)
	}
	if got := byName["session_from_url"]; got.Producer != ProducerFromURL || !strings.Contains(got.Re, "chat/") || !got.Optional {
		t.Fatalf("session_from_url 应来自页面 URL /chat/<32hex>（新页无 ID 时可缺省）: %+v", got)
	}
	if got := byName["session_new"]; got.Producer != ProducerFreshHex || got.Length != 32 {
		t.Fatalf("首轮会话 ID 应客户端自铸 32hex（站点无建会话接口，§十三实测）: %+v", got)
	}
	if got := byName["topic_existing"]; got.Producer != ProducerFetchField || got.Path != "data.topic_id" || !got.Optional {
		t.Fatalf("后续轮 topic_id 应走 fetch_field 只读取数且可缺省: %+v", got)
	}
	if got := byName["req_id"]; got.Producer != ProducerFreshHex || got.Length != 32 {
		t.Fatalf("req_id 必须 32 位 hex（非 32hex 即 403 签名错误）: %+v", got)
	}
	if got := byName["topic_new"]; got.Producer != ProducerFreshBlob || got.Bytes != 24 {
		t.Fatalf("首轮 topic_id 应客户端自铸 24 字节 base64url=32 字符: %+v", got)
	}

	script, err := BuildFetchScript(r, "只回一个数字，7乘6", "", "__uws_qw_1")
	if err != nil {
		t.Fatalf("构造直发脚本失败: %v", err)
	}
	for _, want := range []string{"__QIANWEN_CHAT_SDK__.qwenSign", "produceFetchField", "data.topic_id", "continue_chat", "first_turn", "if_slot"} {
		if !strings.Contains(script, want) {
			t.Fatalf("千问直发脚本缺少 %q", want)
		}
	}
}
