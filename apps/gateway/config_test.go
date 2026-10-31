package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig("127.0.0.1:50051")
	if len(cfg.Channels) != 6 {
		t.Fatalf("期望 6 个默认通道（含 qianwen-web / chatgpt-web），实际 %d", len(cfg.Channels))
	}
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got := cfg.Channel("deepseek-web").PageURL; got != "https://chat.deepseek.com/" {
		t.Fatalf("deepseek page_url=%q", got)
	}
	if got := cfg.Channel("gemini-web").PageURL; got != "https://gemini.google.com/app" {
		t.Fatalf("gemini page_url=%q", got)
	}
	if got := cfg.Channel("chatgpt-web").PageURL; got != "https://chatgpt.com/" {
		t.Fatalf("chatgpt page_url=%q", got)
	}
	if n := len(cfg.ModelTable()); n != 15 {
		t.Fatalf("全局模型表期望 15 条（含 qwen-chat / chatgpt 三档），实际 %d", n)
	}
}

// 目录声明的能力位必须与站点真实存在的开关一致：没有语义的站点不能声称支持，
// 否则 World Dash 会给出一个点了也没用的开关（2026-09-26 用户现场反馈）。
func TestModelTableCapabilitiesMatchSiteSwitches(t *testing.T) {
	cfg := DefaultConfig("127.0.0.1:50051")
	for _, m := range cfg.ModelTable() {
		if m.Channel == "gemini-web" && m.Search {
			t.Fatalf("gemini-web 页面没有智能搜索开关，%s 不应声称支持: %+v", m.ID, m)
		}
		if m.ID == "deepseek-reasoner" && m.Search {
			t.Fatalf("DeepSeek 专家模式没有智能搜索开关，不应声称支持: %+v", m)
		}
		if m.Channel == "deepseek-web" && m.ID == "deepseek-chat" && !m.Search {
			t.Fatalf("deepseek-chat 支持智能搜索，能力位不应为 false: %+v", m)
		}
		// ChatGPT 聊天模式 UI 未见独立的深度思考 / 智能搜索开关（2026-10-02 只读读数）
		// ⇒ 不得声称能力位，否则 World Dash 会给出点了没用的开关。
		if m.Channel == "chatgpt-web" && (m.Thinking || m.Search) {
			t.Fatalf("chatgpt-web 无独立思考/搜索开关，不应声称支持: %+v", m)
		}
	}
}

func TestLoadConfigAndNormalize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "channels.json")
	data := `{
	  "channels": [{
	    "id": "deepseek-web",
	    "type": "uws_web",
	    "site": "deepseek",
	    "models": [{"id": "deepseek-chat", "family": "通用模式", "thinking": true, "search": true}]
	  }]
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	ch := cfg.Channel("deepseek-web")
	if ch.EngineAddr != "127.0.0.1:50051" {
		t.Fatalf("engine_addr 未默认填充: %q", ch.EngineAddr)
	}
	if ch.PageURL != "https://chat.deepseek.com/" {
		t.Fatalf("page_url 未默认填充: %q", ch.PageURL)
	}
	if len(ch.Models) != 1 {
		t.Fatalf("models 应为 1 条（配置覆盖内置）: %d", len(ch.Models))
	}
}

func TestNormalizeValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"空通道", Config{}, "channels 不能为空"},
		{"重复 id", Config{Channels: []ChannelConfig{
			{ID: "a", Type: ChannelTypeWeb, Site: "deepseek", EngineAddr: "127.0.0.1:50051"},
			{ID: "a", Type: ChannelTypeWeb, Site: "gemini", EngineAddr: "127.0.0.1:50051"},
		}}, "重复"},
		{"未知类型", Config{Channels: []ChannelConfig{{ID: "x", Type: "weird"}}}, "未知 type"},
		{"未知站点", Config{Channels: []ChannelConfig{{ID: "x", Type: ChannelTypeWeb, Site: "unknown_site"}}}, "不支持的 site"},
		{"web 缺引擎地址", Config{Channels: []ChannelConfig{{ID: "x", Type: ChannelTypeWeb, Site: "deepseek"}}}, "engine_addr"},
		{"openai_http 缺 base_url", Config{Channels: []ChannelConfig{{ID: "x", Type: ChannelTypeOpenAIHTTP}}}, "base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Normalize("")
			if err == nil {
				t.Fatalf("期望报错 %q，实际 nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望错误含 %q，实际 %v", tc.want, err)
			}
		})
	}
}

func TestChannelByModel(t *testing.T) {
	cfg := DefaultConfig("127.0.0.1:50051")
	_ = cfg.Normalize("127.0.0.1:50051")
	ch, m, err := cfg.ChannelByModel("deepseek-chat")
	if err != nil || ch.ID != "deepseek-web" || m.ID != "deepseek-chat" {
		t.Fatalf("deepseek-chat 路由失败: ch=%v m=%v err=%v", ch, m, err)
	}
	ch, m, err = cfg.ChannelByModel("deepseek-fast") // 别名
	if err != nil || ch.ID != "deepseek-web" || m.ID != "deepseek-chat" {
		t.Fatalf("deepseek-fast 别名路由失败: ch=%v m=%v err=%v", ch, m, err)
	}
	ch, m, err = cfg.ChannelByModel("deepseek-reasoner") // 主 ID
	if err != nil || ch.ID != "deepseek-web" || m.ID != "deepseek-reasoner" {
		t.Fatalf("deepseek-reasoner 路由失败: ch=%v m=%v err=%v", ch, m, err)
	}
	ch, m, err = cfg.ChannelByModel("gemini-3.8-flash") // 别名
	if err != nil || ch.ID != "gemini-web" || m.ID != "gemini-flash" {
		t.Fatalf("gemini-3.8-flash 别名路由失败: ch=%v m=%v err=%v", ch, m, err)
	}
	ch, m, err = cfg.ChannelByModel("veo-3.1") // 别名
	if err != nil || ch.ID != "gemini-web" || m.ID != "gemini-veo" {
		t.Fatalf("veo-3.1 别名路由失败: ch=%v m=%v err=%v", ch, m, err)
	}
	ch, m, err = cfg.ChannelByModel("seedance-2.0") // 别名
	if err != nil || ch.ID != "jimeng-web" || m.ID != "jimeng-video" {
		t.Fatalf("seedance-2.0 别名路由失败: ch=%v m=%v err=%v", ch, m, err)
	}
	ch, m, err = cfg.ChannelByModel("chatgpt-latest")
	if err != nil || ch.ID != "chatgpt-web" || m.ID != "chatgpt-latest" {
		t.Fatalf("chatgpt-latest 路由失败: ch=%v m=%v err=%v", ch, m, err)
	}
	ch, m, err = cfg.ChannelByModel("gpt-6-pro") // 站点实测 model 字符串同为别名
	if err != nil || ch.ID != "chatgpt-web" || m.ID != "chatgpt-latest" {
		t.Fatalf("gpt-6-pro 别名路由失败: ch=%v m=%v err=%v", ch, m, err)
	}
	if _, _, err := cfg.ChannelByModel("nope-model"); err == nil {
		t.Fatal("未知模型应报错")
	}
}

// T-31：站点会话地址模板必须按站点分派，未登记站点 fail-closed（不猜地址/ID）。
func TestSiteConversationFromURL(t *testing.T) {
	tests := []struct {
		name    string
		site    string
		pageURL string
		wantID  string
		wantOK  bool
	}{
		{"gemini 完整地址", "gemini", "https://gemini.google.com/app/0f1e2d3c4b5a6978", "c_0f1e2d3c4b5a6978", true},
		{"gemini 裸路径大写", "gemini", "/app/0F1E2D3C4B5A6978", "c_0f1e2d3c4b5a6978", true},
		{"gemini 带 query", "gemini", "https://gemini.google.com/app/0f1e2d3c4b5a6978?hl=zh-CN", "c_0f1e2d3c4b5a6978", true},
		{"gemini 新对话无 ID", "gemini", "https://gemini.google.com/app", "", false},
		{"deepseek 对话页", "deepseek", "https://chat.deepseek.com/a/chat/s/3fa85f64-5717-4562-b3fc-2c963f66afa6", "3fa85f64-5717-4562-b3fc-2c963f66afa6", true},
		{"deepseek 首页无 ID", "deepseek", "https://chat.deepseek.com/", "", false},
		{"chatgpt 对话页", "chatgpt", "https://chatgpt.com/c/6abf5d65-8c84-83ed-a7bb-6cd5f5d73aa2", "6abf5d65-8c84-83ed-a7bb-6cd5f5d73aa2", true},
		{"chatgpt 首页无 ID", "chatgpt", "https://chatgpt.com/", "", false},
		{"chatgpt 自定义 GPT 路径未登记", "chatgpt", "https://chatgpt.com/g/g-abc123/c/6abf5d65-8c84-83ed-a7bb-6cd5f5d73aa2", "", false},
		{"未登记站点 jimeng", "jimeng", "https://jimeng.jianying.com/ai-tool/image/generate", "", false},
		{"站点名为空", "", "https://gemini.google.com/app/0f1e2d3c4b5a6978", "", false},
		{"地址为空", "gemini", "", "", false},
		{"站点不匹配路径", "deepseek", "https://gemini.google.com/app/0f1e2d3c4b5a6978", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SiteConversationFromURL(tc.site, tc.pageURL)
			if ok != tc.wantOK || got != tc.wantID {
				t.Fatalf("SiteConversationFromURL(%q, %q) = (%q, %v), want (%q, %v)",
					tc.site, tc.pageURL, got, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// T-31 S2 前置：由 ID 生成对话页地址；非法 ID / 未登记站点一律 false。
func TestSiteConversationPageURL(t *testing.T) {
	tests := []struct {
		name   string
		site   string
		convID string
		want   string
		wantOK bool
	}{
		{"gemini 规范写法", "gemini", "c_0f1e2d3c4b5a6978", "https://gemini.google.com/app/0f1e2d3c4b5a6978", true},
		{"gemini 裸 16hex", "gemini", "0f1e2d3c4b5a6978", "https://gemini.google.com/app/0f1e2d3c4b5a6978", true},
		{"gemini 传回页面地址幂等", "gemini", "https://gemini.google.com/app/0f1e2d3c4b5a6978", "https://gemini.google.com/app/0f1e2d3c4b5a6978", true},
		{"gemini ID 长度不对", "gemini", "c_0f1e", "", false},
		{"deepseek uuid", "deepseek", "3fa85f64-5717-4562-b3fc-2c963f66afa6", "https://chat.deepseek.com/a/chat/s/3fa85f64-5717-4562-b3fc-2c963f66afa6", true},
		{"chatgpt uuid", "chatgpt", "6abf5d65-8c84-83ed-a7bb-6cd5f5d73aa2", "https://chatgpt.com/c/6abf5d65-8c84-83ed-a7bb-6cd5f5d73aa2", true},
		{"chatgpt 传回页面地址幂等", "chatgpt", "https://chatgpt.com/c/6ABF5D65-8C84-83ED-A7BB-6CD5F5D73AA2", "https://chatgpt.com/c/6abf5d65-8c84-83ed-a7bb-6cd5f5d73aa2", true},
		{"chatgpt ID 长度不对", "chatgpt", "6abf5d65", "", false},
		{"深色站点传 gemini ID", "deepseek", "c_0f1e2d3c4b5a6978", "", false},
		{"未登记站点 jimeng", "jimeng", "abc", "", false},
		{"ID 为空", "gemini", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SiteConversationPageURL(tc.site, tc.convID)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("SiteConversationPageURL(%q, %q) = (%q, %v), want (%q, %v)",
					tc.site, tc.convID, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
