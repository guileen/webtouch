package gateway

import "testing"

func TestDeepSeekProviderModelResolution(t *testing.T) {
	p, err := newProvider("deepseek", nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"deepseek-chat":     "通用模式",
		"deepseek-v3":       "通用模式",
		"deepseek-fast":     "通用模式",
		"deepseek-vision":   "通用模式",
		"deepseek-reasoner": "深度思考",
		"deepseek-r1":       "深度思考",
		"deepseek-expert":   "深度思考",
		"通用模式":              "通用模式",
		"深度思考":              "深度思考",
		"unknown-model":     "", // 未收录 → 不切换（向后兼容）
	}
	for in, want := range cases {
		if got := p.resolveModel(in); got != want {
			t.Fatalf("resolveModel(%q)=%q, want %q", in, got, want)
		}
	}
	if got := p.defaultModel(); got != "通用模式" {
		t.Fatalf("defaultModel=%q", got)
	}
	if !p.ModelOnNewChatOnly {
		t.Fatal("DeepSeek 应仅新会话可切模式")
	}
	if p.SearchSemantic == "" || p.ThinkingSemantic == "" || p.ModelSwitchSemantic == "" {
		t.Fatalf("DeepSeek 开关语义缺失: %+v", p)
	}
}

func TestGeminiProviderModelResolution(t *testing.T) {
	p, err := newProvider("gemini", nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"gemini-flash":      "Flash",
		"gemini-3.8-flash":  "Flash",
		"gemini-3.7-flash":  "Flash",
		"gemini-3.6-flash":  "Flash",
		"gemini-3.1-pro":    "Pro",
		"3.5 Flash-Lite":    "Flash-Lite",
		"veo-3.1":           "", // Veo 不在物理选择器中（契约 §6）→ 不切换
		"imagen-3":          "", // Imagen 不在物理选择器中（契约 §6）→ 不切换
		"gemini-new-tier-x": "gemini-new-tier-x", // 未收录 → 透传（登录后新档位）
	}
	for in, want := range cases {
		if got := p.resolveModel(in); got != want {
			t.Fatalf("resolveModel(%q)=%q, want %q", in, got, want)
		}
	}
	if p.ModelOnNewChatOnly {
		t.Fatal("Gemini 任意轮可切模型")
	}
	if p.SearchSemantic != "" {
		t.Fatal("Gemini 无独立 search 开关语义")
	}
	if p.InputSettleSec != 1.5 || p.NewChatSettleSec != 1.5 {
		t.Fatalf("Gemini 沉降时间错误: input=%v new_chat=%v", p.InputSettleSec, p.NewChatSettleSec)
	}
}

func TestJimengAndDoubaoProviderModelResolution(t *testing.T) {
	// 验证即梦 AI（含底层 Seedance 2.0 别名）
	jm, err := newProvider("jimeng", nil)
	if err != nil {
		t.Fatalf("newProvider(jimeng): %v", err)
	}
	if got := jm.resolveModel("dreamina-image"); got != "Image" {
		t.Errorf("jimeng dreamina-image 别名解析错误: %q", got)
	}
	if got := jm.resolveModel("jimeng-video"); got != "Video" {
		t.Errorf("jimeng-video 解析错误: %q", got)
	}
	if got := jm.resolveModel("seedance-2.0"); got != "Video" {
		t.Errorf("seedance-2.0 别名解析错误: %q", got)
	}
	if got := jm.resolveModel("seedance"); got != "Video" {
		t.Errorf("seedance 别名解析错误: %q", got)
	}

	// 验证豆包 (Doubao 1.5 Pro)
	db, err := newProvider("doubao", nil)
	if err != nil {
		t.Fatalf("newProvider(doubao): %v", err)
	}
	if got := db.resolveModel("doubao"); got != "Pro" {
		t.Errorf("doubao 别名解析错误: %q", got)
	}
	if got := db.resolveModel("doubao-1.5-pro"); got != "Pro" {
		t.Errorf("doubao-1.5-pro 别名解析错误: %q", got)
	}
	if got := db.resolveModel("doubao-art"); got != "Image" {
		t.Errorf("doubao-art 别名解析错误: %q", got)
	}
}

// ChatGPT 走 DOM 通路（2026-10-02）：页内直发被 Cloudflare JSD 哨兵挡（finalize 只带
// prepare_token ⇒ 403「Unusual activity」），故语义必须指向 chatgpt_* 专用规则，
// 不能落回通用的 chat_input/chat_answer（那是 DeepSeek 的 textarea 选择器，匹配不到
// ChatGPT 的 ProseMirror contenteditable）。
func TestChatGPTProviderSemantics(t *testing.T) {
	p, err := newProvider("chatgpt", nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"input": "chatgpt_input", "send": "chatgpt_send",
		"answer": "chatgpt_answer", "new_chat": "chatgpt_new_chat",
	} {
		if got := p.Semantics[key]; got != want {
			t.Fatalf("chatgpt semantics[%q]=%q, want %q", key, got, want)
		}
	}
	if p.Site != "chatgpt" {
		t.Fatalf("site=%q", p.Site)
	}
	if p.InputSettleSec <= 0 {
		t.Fatal("输入后需要等待站点渲染出发送按钮（实测：空输入时页面上没有发送按钮）")
	}
	if len(p.Models) == 0 {
		t.Fatal("chatgpt 模型目录为空")
	}
}
