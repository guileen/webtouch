package modelbridge

import (
	"encoding/base64"
	"testing"
)

// ── SSE 解析器单测（方案 1：干净文本来自网络层 JSON delta，无 DOM 角标污染） ──

func TestSSESinkDeepSeekDeltaShapes(t *testing.T) {
	s := NewSink(1)
	// DeepSeek Web 实测形态：text_delta / reasoning_delta + answer_finished。
	s.feed([]byte("data: {\"type\":\"delta\",\"delta\":{\"type\":\"reasoning_delta\",\"text\":\"先分析\"}}\n\n"))
	s.feed([]byte("data: {\"type\":\"delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n"))
	s.feed([]byte("data: {\"type\":\"delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"世界\"}}\n\n"))
	s.feed([]byte("data: {\"type\":\"answer_finished\",\"index\":0}\n\n"))

	snap := s.Snapshot()
	if snap.Content != "你好世界" {
		t.Fatalf("content 不符: %q", snap.Content)
	}
	if snap.Reasoning != "先分析" {
		t.Fatalf("reasoning 不符: %q", snap.Reasoning)
	}
	if !snap.Done {
		t.Fatal("answer_finished 后应标记完成")
	}
	if snap.Epoch != 1 {
		t.Fatalf("epoch 不符: %d", snap.Epoch)
	}
}

func TestSSESinkOpenAIChoicesShape(t *testing.T) {
	s := NewSink(1)
	s.feed([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"A\"}}]}\n\n"))
	s.feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"B\"},\"finish_reason\":\"stop\"}]}\n\n"))
	snap := s.Snapshot()
	if snap.Content != "AB" {
		t.Fatalf("content 不符: %q", snap.Content)
	}
	if !snap.Done {
		t.Fatal("finish_reason=stop 应标记完成")
	}
}

// ── DeepSeek Web 新版补丁协议（2026-08-25 实测）─────────────────────────
// 初始快照 {"v":{"response":{"fragments":[{"id":2,"type":"RESPONSE","content":"```"}]}}}
// 正文增量 {"p":"response/fragments/-1/content","o":"APPEND","v":"mark"}
// 收尾     {"p":"response/status","o":"SET","v":"FINISHED"} 或 BATCH quasi_status。
func TestSSESinkDeepSeekPatchFragments(t *testing.T) {
	s := NewSink(1)
	// 初始快照：首个 fragment 已含首段正文（"```"）。
	s.feed([]byte("data: {\"v\":{\"response\":{\"message_id\":12,\"fragments\":[{\"id\":2,\"type\":\"RESPONSE\",\"content\":\"```\"}]}}}\n\n"))
	// 首个增量带 p/o（fragments/-1 = 最后一个 fragment，确立补丁路径）。
	s.feed([]byte("data: {\"p\":\"response/fragments/-1/content\",\"o\":\"APPEND\",\"v\":\"mark\"}\n\n"))
	// 真实协议：之后增量是省略 p/o 的隐式形态 {"v":"..."}（沿上一补丁路径继续 APPEND）。
	s.feed([]byte("data: {\"v\":\"down\"}\n\n"))
	s.feed([]byte("data: {\"v\":\"\\n## 标题\\n\\n正文\"}\n\n"))
	// 收尾 status。
	s.feed([]byte("data: {\"p\":\"response/status\",\"o\":\"SET\",\"v\":\"FINISHED\"}\n\n"))

	snap := s.Snapshot()
	want := "```markdown\n## 标题\n\n正文"
	if snap.Content != want {
		t.Fatalf("新版补丁 content 不符:\n got %q\nwant %q", snap.Content, want)
	}
	if !snap.Done {
		t.Fatal("status FINISHED 应标记完成")
	}
	if snap.Epoch != 1 {
		t.Fatalf("epoch 不符: %d", snap.Epoch)
	}
}

func TestSSESinkDeepSeekPatchThinkingAndResponse(t *testing.T) {
	s := NewSink(1)
	// 思考 + 正文两个 fragment 的初始快照；思考增量走具体 id，正文走 -1。
	s.feed([]byte("data: {\"v\":{\"response\":{\"fragments\":[" +
		"{\"id\":1,\"type\":\"THINKING\",\"content\":\"先想\"}," +
		"{\"id\":2,\"type\":\"RESPONSE\",\"content\":\"答\"}]}}}\n\n"))
	s.feed([]byte("data: {\"p\":\"response/fragments/1/content\",\"o\":\"APPEND\",\"v\":\" 再想\"}\n\n"))
	// 隐式增量沿上一路径（thinking fragment id=1）继续。
	s.feed([]byte("data: {\"v\":\"继续想\"}\n\n"))
	s.feed([]byte("data: {\"p\":\"response/fragments/-1/content\",\"o\":\"APPEND\",\"v\":\"案\"}\n\n"))
	// 隐式增量沿 response fragment（-1 = 最后一个）继续。
	s.feed([]byte("data: {\"v\":\"完整\"}\n\n"))
	s.feed([]byte("data: {\"p\":\"response\",\"o\":\"BATCH\",\"v\":[{\"p\":\"quasi_status\",\"v\":\"FINISHED\"}]}\n\n"))

	snap := s.Snapshot()
	if snap.Content != "答案完整" {
		t.Fatalf("content 不符: %q", snap.Content)
	}
	if snap.Reasoning != "先想 再想继续想" {
		t.Fatalf("reasoning 不符: %q", snap.Reasoning)
	}
	if !snap.Done {
		t.Fatal("BATCH quasi_status=FINISHED 应标记完成")
	}
}

func TestSSESinkDeepSeekPatchSplitAcrossChunks(t *testing.T) {
	raw := `data: {"p":"response/fragments/-1/content","o":"APPEND","v":"跨"}` + "\n\n" +
		`data: {"v":" chunk"}` + "\n\n" +
		`data: {"v":" 拼接"}` + "\n\n" +
		`data: {"p":"response/status","o":"SET","v":"FINISHED"}` + "\n\n"
	s := NewSink(1)
	for i := 0; i < len(raw); i++ {
		s.feed([]byte(raw[i : i+1]))
	}
	snap := s.Snapshot()
	if snap.Content != "跨 chunk 拼接" {
		t.Fatalf("新版补丁跨 chunk 拼接失败: %q", snap.Content)
	}
	if !snap.Done {
		t.Fatal("新版补丁收尾应标记完成")
	}
}

func TestSSESinkSplitAcrossFeedBoundaries(t *testing.T) {
	raw := `data: {"type":"delta","delta":{"type":"text_delta","text":"引用角标污染不应出现"}}` + "\n\n" +
		"data: [DONE]\n\n"
	s := NewSink(1)
	// 逐字节喂入：chunk 边界可切断任意行/JSON 中间。
	for i := 0; i < len(raw); i++ {
		s.feed([]byte(raw[i : i+1]))
	}
	snap := s.Snapshot()
	if snap.Content != "引用角标污染不应出现" {
		t.Fatalf("跨 chunk 拼接失败: %q", snap.Content)
	}
	if !snap.Done {
		t.Fatal("[DONE] 应标记完成")
	}
}

func TestSSESinkDrainIncrements(t *testing.T) {
	s := NewSink(2)
	s.feed([]byte("data: {\"delta\":{\"content\":\"第一段\"}}\n\n"))
	s.feed([]byte("data: {\"delta\":{\"content\":\"第二段\"}}\n\n"))
	chunks := s.Drain()
	if len(chunks) != 2 {
		t.Fatalf("drain 应返回 2 个增量块，got %d", len(chunks))
	}
	got := ""
	for _, c := range chunks {
		got += c.Content
	}
	if got != "第一段第二段" {
		t.Fatalf("增量拼接不符: %q", got)
	}
	if len(s.Drain()) != 0 {
		t.Fatal("drain 后应清空（读后清零语义）")
	}
}

func TestSSESinkIgnoresNonJSONAndComments(t *testing.T) {
	s := NewSink(1)
	s.feed([]byte(": keep-alive comment\n\ndata: {\"delta\":{\"content\":\"有效\"}}\n\n"))
	if s.Snapshot().Content != "有效" {
		t.Fatalf("注释/心跳行不应污染内容: %q", s.Snapshot().Content)
	}
}

func TestIsChatCompletionURL(t *testing.T) {
	cases := map[string]bool{
		"https://chat.deepseek.com/api/v0/chat/completion":     true,
		"https://chat.deepseek.com/api/v0/chat/completion?a=1": true,
		"https://api.openai.com/v1/chat/completions":           true,
		"https://chat.deepseek.com/api/v0/chat/other":          false,
		"https://example.com/chat/completionx":                 false,
		"not a url":                                            false,
	}
	for u, want := range cases {
		if got := IsChatCompletionURL(u); got != want {
			t.Fatalf("IsChatCompletionURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestIsGeminiStreamURL(t *testing.T) {
	cases := map[string]bool{
		"https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate":                 true,
		"https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=boq&f.sid=-1": true,
		"https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=PCck7e":                                         false,
		"https://chat.deepseek.com/api/v0/chat/completion":                                                               false,
		"not a url": false,
	}
	for u, want := range cases {
		if got := IsGeminiStreamURL(u); got != want {
			t.Fatalf("IsGeminiStreamURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestSSESinkGeminiNDJSON(t *testing.T) {
	// 2026-08-25 gemini.google.com 真实抓包形态：anti-XSSI 前缀 + 长度前缀帧。
	// 内容在 rc_<candidate> 的文本数组里；收尾 = {"44":true,"46":[...],"52":[]} 状态帧。
	stream := `)]}'

177
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],{\"18\":\"r_y\",\"21\":[\"tok\"],\"44\":true}]"]]
200
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],null,null,[[\"rc_abc\",[\"1\\n2\\n\"],null,null,null,null,null,null,[1]]]]"]]
120
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],null,null,[[\"rc_abc\",[\"1\\n2\\n3\"],null,null,null,null,null,null,[2]]]]"]]
90
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],{\"44\":true,\"46\":[\"c_x\",\"\"],\"52\":[]}]"]]
26
[["e",9,null,null,4839]]
`
	s := NewSink(1)
	s.feed([]byte(stream))
	snap := s.Snapshot()
	if snap.Content != "1\n2\n3" {
		t.Fatalf("Gemini NDJSON content 不符: %q", snap.Content)
	}
	if !snap.Done {
		t.Fatal("状态帧（46/52）后应标记完成")
	}
	chunks := s.Drain()
	var contents []string
	for _, c := range chunks {
		if !c.Done && c.Content != "" {
			contents = append(contents, c.Content)
		}
	}
	if len(contents) != 2 || contents[0] != "1\n2\n" || contents[1] != "3" {
		t.Fatalf("增量块不符: %+v", contents)
	}
}

func TestSSESinkGeminiNDJSONChunkedAndDedup(t *testing.T) {
	stream := `)]}'

150
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],null,null,[[\"rc_a\",[\"hello\"]]]]"]]
150
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],null,null,[[\"rc_a\",[\"hello\"]]]]"]]
155
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],null,null,[[\"rc_a\",[\"hello world\"]]]]"]]
70
[["e",9,null,null,70]]
`
	// 逐字节喂入：帧行可能在任意字节处被截断，必须跨 Feed 安全拼接。
	s := NewSink(1)
	for i := 0; i < len(stream); i++ {
		s.feed([]byte{stream[i]})
	}
	snap := s.Snapshot()
	if snap.Content != "hello world" {
		t.Fatalf("chunked NDJSON content 不符: %q", snap.Content)
	}
	// 重复帧不下发增量（同一候选累计文本无变化）。
	chunks := s.Drain()
	var got string
	for _, c := range chunks {
		got += c.Content
	}
	if got != "hello world" {
		t.Fatalf("重复帧应去重: %q", got)
	}
}

func TestPickGeminiOption(t *testing.T) {
	opts := []MenuOption{
		{Text: "3.5 Flash-Lite\n极速回答"},
		{Text: "3.6 Flash\n全方位帮助"},
		{Text: "3.1 Pro\n高级推理"},
	}
	cases := map[string]int{
		"3.5 Flash-Lite":    0,
		"3.6 Flash":         1,
		"3.1 Pro":           2,
		"Extended thinking": -1, // 开关不是模型单选，模型匹配不命中
		"thinking":          -1, // 同上
		"flash-lite":        0,  // 大小写不敏感（token 匹配）
		"Flash":             1,  // 家族词精确：Flash 不得命中 Flash-Lite
		"不存在的模型":            -1,
		"":                  -1,
	}
	for want, idx := range cases {
		if got := PickGeminiOption(opts, want); got != idx {
			t.Fatalf("PickGeminiOption(%q) = %d, want %d", want, got, idx)
		}
	}
}

func TestFindThinkingOption(t *testing.T) {
	opts := []MenuOption{
		{Text: "3.5 Flash-Lite\n极速回答"},
		{Text: "3.6 Flash\n全方位帮助"},
		{Text: "3.1 Pro\n高级推理"},
		{Text: "Extended thinking\nComplex problem solving", Selected: true},
	}
	if got := FindThinkingOption(opts); got != 3 {
		t.Fatalf("FindThinkingOption = %d, want 3", got)
	}
	if got := FindThinkingOption(opts[:3]); got != -1 {
		t.Fatalf("FindThinkingOption(models-only) = %d, want -1", got)
	}
}

func TestModelSwitchFamilyVerification(t *testing.T) {
	// 模型切换校验语义：按钮短名必须精确匹配目标模型的末 token（模型族名），
	// "Flash" 不得被误判为 "Flash-Lite"。
	cases := []struct {
		button string
		target string
		want   bool
	}{
		{"Flash-Lite", "3.5 Flash-Lite", true},     // 短名 = 族名
		{"3.5 Flash-Lite", "3.5 Flash-Lite", true}, // 按钮显示全名
		{"Flash", "3.6 Flash", true},
		{"Pro", "3.1 Pro", true},
		{"Flash-Lite", "3.6 Flash", false}, // 旧模型 ≠ 目标
		{"Flash", "3.5 Flash-Lite", false}, // Flash 不是 Flash-Lite
		{"Flash", "最新 Flash", true},
		{"Flash-Lite", "最新 Flash", false},
	}
	for _, c := range cases {
		family := lastToken(normalizeText(c.target))
		got := family != "" && hasWord(normalizeText(c.button), family)
		if got != c.want {
			t.Fatalf("button=%q target=%q: got %v, want %v", c.button, c.target, got, c.want)
		}
	}
}

// ── chatStream 状态机单测（轮次/武装顺序/收尾） ────────────────────────

func TestChatStreamPreFeedOrdering(t *testing.T) {
	c := NewStream()
	c.OnRequestStart("req-1")
	// 武装完成前到达的增量先进 preFeed（真实场景：dataReceived 可能早于
	// streamResourceContent 响应返回）。
	c.OnData("req-1", b64(t, `data: {"delta":{"content":"先"}}`+"\n\n"))
	c.OnBuffered("req-1", []byte(`data: {"delta":{"content":"缓冲"}}`+"\n\n"))
	c.OnData("req-1", b64(t, `data: {"delta":{"content":"后"}}`+"\n\n"))
	snap := c.Snapshot()
	// 顺序 = bufferedData（武装返回）→ preFeed 中先到的增量 → 后续增量。
	if snap.Content != "缓冲先后" {
		t.Fatalf("preFeed 乱序: %q", snap.Content)
	}
}

func TestChatStreamEpochResetAndFinish(t *testing.T) {
	c := NewStream()
	c.OnRequestStart("req-1")
	c.OnBuffered("req-1", []byte(`data: {"delta":{"content":"第一轮"}}`+"\n\n"))
	c.OnFinish("req-1")
	snap := c.Snapshot()
	if snap.Epoch != 1 || snap.Content != "第一轮" || !snap.Done {
		t.Fatalf("第一轮快照不符: %+v", snap)
	}

	// 第二轮：新请求必须重置 sink（旧回答不再是当前快照，多轮不串味）。
	c.OnRequestStart("req-2")
	snap = c.Snapshot()
	if snap.Epoch != 2 || snap.Content != "" || snap.Done {
		t.Fatalf("第二轮开始快照应清空: %+v", snap)
	}
	c.OnData("req-2", b64(t, `data: {"delta":{"content":"第二轮"}}`+"\n\n"))
	c.OnFinish("req-2")
	if got := c.Snapshot().Content; got != "第二轮" {
		t.Fatalf("第二轮内容不符: %q", got)
	}

	// loadingFailed 放弃在途轮次：不标记完成。
	c.OnRequestStart("req-3")
	c.OnFail("req-3")
	if c.Snapshot().Epoch != 3 || c.Snapshot().Done {
		t.Fatalf("失败轮次不应标记完成: %+v", c.Snapshot())
	}
}

func TestChatStreamArmFailedFlushesPreFeed(t *testing.T) {
	c := NewStream()
	c.OnRequestStart("req-1")
	c.OnData("req-1", b64(t, `data: {"delta":{"content":"X"}}`+"\n\n"))
	c.OnArmFailed("req-1")
	if got := c.Snapshot().Content; got != "X" {
		t.Fatalf("武装失败应放行 preFeed: %q", got)
	}
}

func TestChatStreamFailMarksFailedAndResets(t *testing.T) {
	// loadingFailed/中止：标记 Failed（快照不可信，extract 回退 DOM），
	// 但不标记 Done（不能当「已完成的空回答」）；新一轮开始必须清除。
	c := NewStream()
	c.OnRequestStart("req-1")
	c.OnBuffered("req-1", []byte(`data: {"delta":{"content":"半截"}}`+"\n\n"))
	c.OnFail("req-1")
	snap := c.Snapshot()
	if !snap.Failed || snap.Done {
		t.Fatalf("失败轮次应标记 Failed 且不标记 Done: %+v", snap)
	}
	if snap.Content != "半截" {
		t.Fatalf("失败轮次残留内容保留供审计: %q", snap.Content)
	}
	c.OnRequestStart("req-2")
	if c.Snapshot().Failed {
		t.Fatal("新一轮应清除 Failed 标记")
	}
}

func TestChatStreamBodyFallbackForGemini(t *testing.T) {
	// Gemini batchexecute NDJSON：dataReceived/streamResourceContent 均无数据，
	// 收尾后靠 Network.getResponseBody 兜底取全文（2026-08-25 真实抓包）。
	c := NewStream()
	c.OnRequestStart("req-1")
	if !c.NeedsBody("req-1") {
		t.Fatal("零增量轮次应标记需要 getResponseBody 兜底")
	}
	body := `)]}'

150
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],null,null,[[\"rc_a\",[\"完整回答\"]]]]"]]
70
[["e",9,null,null,70]]
`
	c.OnBody("req-1", []byte(body))
	snap := c.Snapshot()
	if snap.Content != "完整回答" || !snap.Done {
		t.Fatalf("body 兜底快照不符: %+v", snap)
	}

	// DeepSeek 路径（已有增量）：needsBody=false，onBody 不得覆盖。
	c.OnRequestStart("req-2")
	c.OnData("req-2", b64(t, `data: {"delta":{"content":"增量"}}`+"\n\n"))
	c.OnArmFailed("req-2") // 武装失败放行 preFeed，内容进入 sink
	if c.NeedsBody("req-2") {
		t.Fatal("已有增量不应触发 body 兜底")
	}
	c.OnBody("req-2", []byte("garbage"))
	if got := c.Snapshot().Content; got != "增量" {
		t.Fatalf("已有增量时 onBody 不应覆盖: %q", got)
	}
}

func b64(t *testing.T, s string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// ── CDP 事件接线测试（伪 WS 服务器：requestWillBeSent → dataReceived → loadingFinished） ──

// TestSSESinkGeminiConversationID：站点自报的会话 ID（内层 index 1[0]）必须被读到——
// 契约 §9 取值来源②；2026-10-01 真机帧 `[null,["c_<16hex>","r_<16hex>"],…]`。
// 关键回归点：收尾状态帧（state["46"]）会 markDone+return，取 ID 必须在它之前。
func TestSSESinkGeminiConversationID(t *testing.T) {
	const conv = "c_2d64e591ca5edc51"
	stream := `)]}'

177
[["wrb.fr",null,"[null,[\"c_2d64e591ca5edc51\",\"r_6ed42fdcf06117f5\"],{\"18\":\"r_6ed42fdcf06117f5\",\"21\":[\"tok\"],\"44\":true}]"]]
200
[["wrb.fr",null,"[null,[\"c_2d64e591ca5edc51\",\"r_6ed42fdcf06117f5\"],null,null,[[\"rc_abc\",[\"答案\"]]]]"]]
90
[["wrb.fr",null,"[null,[\"c_2d64e591ca5edc51\",\"r_6ed42fdcf06117f5\"],{\"44\":true,\"46\":[\"c_2d64e591ca5edc51\",\"\"],\"52\":[]}]"]]
26
[["e",9,null,null,4839]]
`
	s := NewSink(1)
	s.feed([]byte(stream))
	snap := s.Snapshot()
	if snap.ConversationID != conv {
		t.Fatalf("站点自报会话 ID 未取到: got %q, want %q", snap.ConversationID, conv)
	}
	if snap.Content != "答案" || !snap.Done {
		t.Fatalf("同轮其余快照字段回归: %+v", snap)
	}
}

// TestSSESinkGeminiConversationIDAbsent：帧里没有 ID / 为空 ⇒ 空串（不猜）。
func TestSSESinkGeminiConversationIDAbsent(t *testing.T) {
	stream := `)]}'

150
[["wrb.fr",null,"[null,null,null,null,[[\"rc_a\",[\"hi\"]]]]"]]
70
[["wrb.fr",null,"[null,[\"\",\"\"],null,null,[[\"rc_a\",[\"hi\"]]]]"]]
26
[["e",9,null,null,70]]
`
	s := NewSink(1)
	s.feed([]byte(stream))
	if got := s.Snapshot().ConversationID; got != "" {
		t.Fatalf("无 ID 帧不得给出读数: %q", got)
	}
}

// ── 通义千问（Qwen）Web SSE 形态（2026-10-02 实测帧，已脱敏）──────────────
// 帧＝ `data:{...}`，正文在 data.messages[].content（mime_type=multi_load/iframe，
// 按前缀快照递增），收尾 `event:complete`，会话 ID 在顶层 communication.sessionid。
func TestSSESinkQianwenFrames(t *testing.T) {
	s := NewSink(1)
	frames := []string{
		"data:{\"error_msg\":\"\",\"data\":{\"messages\":[{\"mime_type\":\"signal/post\",\"status\":\"complete\"}]},\"error_code\":0,\"communication\":{\"sessionid\":\"5b4c53d52f64458092e65a9de0ed0b62\",\"resid\":2}}\n\n",
		"data:{\"error_msg\":\"\",\"data\":{\"messages\":[{\"mime_type\":\"bar/progress\",\"status\":\"complete\"}]},\"error_code\":0}\n\n",
		"data:{\"error_msg\":\"\",\"data\":{\"messages\":[{\"mime_type\":\"multi_load/iframe\",\"content\":\"4\",\"status\":\"processing\"}]},\"error_code\":0}\n\n",
		"data:{\"error_msg\":\"\",\"data\":{\"messages\":[{\"mime_type\":\"multi_load/iframe\",\"content\":\"42\",\"status\":\"complete\"}]},\"error_code\":0}\n\n",
		"event:complete\n\n",
		"data:{\"error_msg\":\"\",\"data\":{\"messages\":[{\"mime_type\":\"survey/card\",\"status\":\"complete\"}]},\"error_code\":0}\n\n",
	}
	for _, f := range frames {
		s.feed([]byte(f))
	}
	snap := s.Snapshot()
	if snap.Content != "42" {
		t.Fatalf("千问正文应解出 42（前缀快照不得重复拼接），实际 %q", snap.Content)
	}
	if snap.ConversationID != "5b4c53d52f64458092e65a9de0ed0b62" {
		t.Fatalf("应从 communication.sessionid 取到会话 ID，实际 %q", snap.ConversationID)
	}
	if !snap.Done {
		t.Fatal("event:complete 后应标记完成")
	}
}

// 非问答型帧（无 multi_load/iframe）不得产出正文；未见过千问帧时
// `event:complete` 也不当完成信号（防跨站点误触）。
func TestSSESinkQianwenNonAnswerFrames(t *testing.T) {
	s := NewSink(1)
	s.feed([]byte("data:{\"data\":{\"messages\":[{\"mime_type\":\"bar/iframe\",\"status\":\"complete\"}]},\"error_code\":0}\n\n"))
	s.feed([]byte("event:complete\n\n"))
	snap := s.Snapshot()
	if snap.Content != "" {
		t.Fatalf("非正文帧不应产出正文，实际 %q", snap.Content)
	}
	if snap.Done {
		t.Fatal("未见过千问正文帧时 event:complete 不应标记完成")
	}
}
