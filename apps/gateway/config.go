// Package gateway 实现 WebTouch 浏览器模型 provider 执行服务（契约：
// docs/gateway-contract-2026-08-26.md）。web 通道（deepseek-web / gemini-web）
// 直连 uws-engine gRPC，把网页聊天界面封装成 Agent Hub 内部调用的执行契约。
//
// 分层边界（2026-09-02 定稿）：本包属「应用层」（网页 LLM API 化网关），
// 不在 WebTouch 核心（内核/调度/Agent/界面/定制化/存储）之内；目标为外移成
// 独立应用组件（Phase D），外移前核心代码禁止反向依赖本包。
// 见 docs/README.md（01-Bus 领域）与 docs/gateway-contract-2026-08-26.md。
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

// 通道类型（channels.json schema，契约 §7）。
const (
	// ChannelTypeWeb 是 uws_web：引擎 gRPC + CDP 接管网页聊天站点。
	ChannelTypeWeb = "uws_web"
	// ChannelTypeOpenAIHTTP 是 openai_http：外部 OpenAI 兼容 API 直连（M3 落地，M1 仅登记）。
	ChannelTypeOpenAIHTTP = "openai_http"
)

// ModelConfig 是通道内模型目录条目（契约 §7 models）。
type ModelConfig struct {
	ID          string   `json:"id"`
	Family      string   `json:"family,omitempty"`
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
	Thinking    *bool    `json:"thinking,omitempty"`
	Search      *bool    `json:"search,omitempty"`
	ImageGen    bool     `json:"image_gen,omitempty"`
	VideoGen    bool     `json:"video_gen,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

// ChannelConfig 是 channels.json 的单通道条目（契约 §7）。
// M1 仅实现 uws_web；openai_http 条目允许登记，路由层返回 501。
type ChannelConfig struct {
	ID         string        `json:"id"`
	Type       string        `json:"type"`
	Site       string        `json:"site,omitempty"`
	AccountID  string        `json:"account_id,omitempty"`
	EngineAddr string        `json:"engine_addr,omitempty"`
	PageURL    string        `json:"page_url,omitempty"`
	BaseURL    string        `json:"base_url,omitempty"`
	APIKeyEnv  string        `json:"api_key_env,omitempty"`
	Models     []ModelConfig `json:"models,omitempty"`
}

// Config 是 channels.json 根结构（契约 §7）。
type Config struct {
	Channels []ChannelConfig `json:"channels"`
}

// DefaultPageURL 返回站点默认初始页面（可被 channels.json page_url 覆盖）。
func DefaultPageURL(site string) string {
	switch strings.ToLower(site) {
	case "gemini":
		return "https://gemini.google.com/app"
	case "jimeng":
		return "https://jimeng.jianying.com/ai-tool/image/generate"
	case "doubao", "seedance":
		return "https://www.doubao.com/chat/"
	case "qianwen":
		return "https://www.qianwen.com/"
	case "chatgpt":
		return "https://chatgpt.com/"
	default:
		return "https://chat.deepseek.com/"
	}
}

// 站点会话地址模板（契约 §9「站点会话身份」，T-31）：把站点服务端对话 ID
// 与浏览器地址互转。**未登记的站点一律 fail-closed**（返回 false）——不猜地址、不猜 ID。
var (
	// Gemini 对话页：/app/<16hex>，对外规范写法 c_<16hex>。
	geminiConvPathRe = regexp.MustCompile(`^/app/([0-9a-fA-F]{16})/?$`)
	geminiConvIDRe   = regexp.MustCompile(`^(?:c_)?([0-9a-fA-F]{16})$`)
	// DeepSeek 对话页：/a/chat/s/<uuid36>，ID 原样。
	deepseekConvPathRe = regexp.MustCompile(`^/a/chat/s/([0-9a-fA-F-]{36})/?$`)
	deepseekConvIDRe   = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
	// 千问对话页：/chat/<32hex>，ID 原样 32hex（契约 §13 实例：站点自报 session_id 同形）。
	qianwenSiteConvPathRe = regexp.MustCompile(`^/chat/([0-9a-fA-F]{32})/?$`)
	qianwenSiteConvIDRe   = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	// ChatGPT 对话页：/c/<uuid36>，ID 原样（2026-10-02 真机抓包页地址坐实；
	// 自定义 GPT 路径 /g/<slug>/c/<uuid> 未登记 ⇒ fail-closed）。
	chatgptConvPathRe = regexp.MustCompile(`^/c/([0-9a-fA-F-]{36})/?$`)
	chatgptConvIDRe   = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
)

// SiteConversationFromURL 从页面地址（完整 URL 或裸路径）提取站点服务端对话 ID。
// 返回规范写法：Gemini ⇒ c_<16hex>，DeepSeek ⇒ 原样 uuid。
// 站点未登记模板、或地址不是该站点的对话页时返回 ("", false)。
func SiteConversationFromURL(site, pageURL string) (string, bool) {
	path := sitePagePath(pageURL)
	if path == "" {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(site)) {
	case "gemini":
		if m := geminiConvPathRe.FindStringSubmatch(path); m != nil {
			return "c_" + strings.ToLower(m[1]), true
		}
	case "deepseek":
		if m := deepseekConvPathRe.FindStringSubmatch(path); m != nil {
			return strings.ToLower(m[1]), true
		}
	case "qianwen":
		if m := qianwenSiteConvPathRe.FindStringSubmatch(path); m != nil {
			return strings.ToLower(m[1]), true
		}
	case "chatgpt":
		if m := chatgptConvPathRe.FindStringSubmatch(path); m != nil {
			return strings.ToLower(m[1]), true
		}
	}
	return "", false
}

// NormalizeSiteConversationID 归一化并校验**站点自报**的会话 ID（响应体来路，契约 §9 取值来源②）。
// 与 SiteConversationFromURL / SiteConversationPageURL 同一套站点模板：
// 未登记站点或形状不符一律 ("", false) —— fail-closed，不把猜值/垃圾串当 ID 回给调用方。
func NormalizeSiteConversationID(site, raw string) (string, bool) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", false
	}
	if cid, ok := SiteConversationFromURL(site, id); ok { // 兼容直接喂页面地址/裸路径
		return cid, true
	}
	switch strings.ToLower(strings.TrimSpace(site)) {
	case "gemini":
		if m := geminiConvIDRe.FindStringSubmatch(id); m != nil {
			return "c_" + strings.ToLower(m[1]), true
		}
	case "deepseek":
		if deepseekConvIDRe.MatchString(id) {
			return strings.ToLower(id), true
		}
	case "qianwen":
		if qianwenSiteConvIDRe.MatchString(id) {
			return strings.ToLower(id), true
		}
	case "chatgpt":
		if chatgptConvIDRe.MatchString(id) {
			return strings.ToLower(id), true
		}
	}
	return "", false
}

// SiteConversationPageURL 由站点会话 ID（或该对话的页面地址，幂等）生成对话页地址。
// 站点未登记模板、或 ID 非法时返回 ("", false)——恢复动作必须 fail-closed，不落通道首页凑数。
func SiteConversationPageURL(site, convID string) (string, bool) {
	id := strings.TrimSpace(convID)
	if id == "" {
		return "", false
	}
	if cid, ok := SiteConversationFromURL(site, id); ok {
		id = cid
	}
	switch strings.ToLower(strings.TrimSpace(site)) {
	case "gemini":
		if m := geminiConvIDRe.FindStringSubmatch(id); m != nil {
			return "https://gemini.google.com/app/" + strings.ToLower(m[1]), true
		}
	case "deepseek":
		if deepseekConvIDRe.MatchString(id) {
			return "https://chat.deepseek.com/a/chat/s/" + strings.ToLower(id), true
		}
	case "qianwen":
		if qianwenSiteConvIDRe.MatchString(id) {
			return "https://www.qianwen.com/chat/" + strings.ToLower(id), true
		}
	case "chatgpt":
		if chatgptConvIDRe.MatchString(id) {
			return "https://chatgpt.com/c/" + strings.ToLower(id), true
		}
	}
	return "", false
}

// sitePagePath 从完整 URL 或裸路径取路径部分（丢弃 query / fragment）。
func sitePagePath(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Path
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// DefaultConfig 返回内置默认通道（deepseek-web + gemini-web + jimeng-web + doubao-web + qianwen-web + chatgpt-web），
// engineAddr 为 CLI --engine-addr 默认值；未提供 channels.json 时使用。
func DefaultConfig(engineAddr string) Config {
	thinking := true
	search := true
	noSearch := false
	return Config{
		Channels: []ChannelConfig{
			{
				ID: "deepseek-web", Type: ChannelTypeWeb, Site: "deepseek",
				EngineAddr: engineAddr, PageURL: DefaultPageURL("deepseek"),
				Models: []ModelConfig{
					{ID: "deepseek-chat", Family: "通用模式", Label: "全新统一模型", Description: "日常对话、图片理解与多模态通用应答（默认关深度思考，含智能搜索）", Aliases: []string{"deepseek-v3", "deepseek-fast", "deepseek-vision"}, Thinking: &thinking, Search: &search},
					{ID: "deepseek-reasoner", Family: "深度思考", Label: "深度思考 (R1)", Description: "R1 专家级深度推理与思维链（默认开深度思考；专家模式无智能搜索开关）", Aliases: []string{"deepseek-r1", "deepseek-expert"}, Thinking: &thinking, Search: &noSearch},
				},
			},
			{
				ID: "gemini-web", Type: ChannelTypeWeb, Site: "gemini",
				EngineAddr: engineAddr, PageURL: DefaultPageURL("gemini"),
				Models: []ModelConfig{
					{ID: "gemini-3.5-flash-lite", Family: "Flash-Lite", Label: "3.5 Flash-Lite", Description: "极速文本回答（站点搜索默认开启、不可关）", Aliases: []string{"gemini-flash-lite"}, Thinking: &thinking},
					{ID: "gemini-flash", Family: "Flash", Label: "3.8 Flash", Description: "全方位多模态与生图理解（主力 Flash；搜索默认开启、不可关）", Aliases: []string{"gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-2.5-flash"}, Thinking: &thinking, ImageGen: true},
					{ID: "gemini-3.1-pro", Family: "Pro", Label: "3.1 Pro", Description: "高级复杂推理与全能助手（搜索默认开启、不可关；深度思考＝Extended thinking 开关）", Aliases: []string{"gemini-pro", "gemini-3-pro", "gemini-2.5-pro"}, Thinking: &thinking, ImageGen: true},
					{ID: "gemini-veo", Family: "Veo", Label: "Veo 3.1", Description: "Google Veo 动态视频生成", Aliases: []string{"veo-3.1", "veo-3", "veo", "veo-2", "gemini-video"}, VideoGen: true},
					{ID: "gemini-imagen", Family: "Imagen", Label: "Imagen 3", Description: "Google Imagen 3 高清画作生成", Aliases: []string{"imagen-3", "imagen"}, ImageGen: true},
				},
			},
			{
				ID: "jimeng-web", Type: ChannelTypeWeb, Site: "jimeng",
				EngineAddr: engineAddr, PageURL: DefaultPageURL("jimeng"),
				Models: []ModelConfig{
					{ID: "jimeng-image", Family: "Image", Label: "即梦生图", Description: "即梦 AI 每日免费算力生图", Aliases: []string{"jimeng-v2", "dreamina-image", "seedance-image"}, ImageGen: true},
					{ID: "jimeng-video", Family: "Video", Label: "即梦视频 (Seedance 2.0)", Description: "即梦 AI 动态视频与 Seedance 2.0 运镜生成", Aliases: []string{"seedance-2.0", "seedance-2", "seedance", "dreamina-video", "seedance-video"}, VideoGen: true},
				},
			},
			{
				ID: "doubao-web", Type: ChannelTypeWeb, Site: "doubao",
				EngineAddr: engineAddr, PageURL: DefaultPageURL("doubao"),
				Models: []ModelConfig{
					{ID: "doubao-pro", Family: "Pro", Label: "豆包 Pro (1.5 Pro)", Description: "字节跳动中文对话、推理与搜索", Aliases: []string{"doubao", "doubao-1.5-pro"}, Thinking: &thinking, Search: &search},
					{ID: "doubao-image", Family: "Image", Label: "豆包绘图", Description: "豆包多风格生图", Aliases: []string{"doubao-art"}, ImageGen: true},
				},
			},
			{
				// ChatGPT（chatgpt.com）：接站第二站（2026-10-02）。**硬约束：只走「聊天」模式，
				// 绝不点「工作」**——额度几乎都耗在工作模式（用户 2026-10-02 口径）。
				// 模型名（默认「最新」＝GPT-6）与**思考强度**是两个参数：强度实测为 5 档滑杆
				// （不同模型档位数可能不同），**默认取中/高档**；智能搜索默认开启、不可关
				// （⇒ 能力位仍不声明：能力位＝「有可开关」，站点无可关的开关）。
				ID: "chatgpt-web", Type: ChannelTypeWeb, Site: "chatgpt",
				EngineAddr: engineAddr, PageURL: DefaultPageURL("chatgpt"),
				Models: []ModelConfig{
					{ID: "chatgpt-latest", Family: "Latest", Label: "最新 (GPT-6)", Description: "模型选择器当前项「最新」＝GPT-6（实测站点请求体 model=gpt-6-pro；搜索默认开、不可关；仅聊天模式）", Aliases: []string{"gpt-6-pro", "gpt-latest", "chatgpt-pro"}},
					{ID: "chatgpt-5.6-sol", Family: "GPT-5.6 Sol", Label: "GPT-5.6 Sol", Description: "模型菜单项 GPT-5.6 Sol（请求体 model 字符串未实测；搜索默认开、不可关）", Aliases: []string{"gpt-5.6-sol", "gpt-5.6"}},
					{ID: "chatgpt-5.5", Family: "GPT-5.5", Label: "GPT-5.5", Description: "模型菜单项 GPT-5.5（站点标注 2026-10-14 下线；请求体 model 字符串未实测）", Aliases: []string{"gpt-5.5"}},
				},
			},
			{
				// 千问（阿里通义）：接站第一站。通道先落「会话身份 + 清账」两项（SOP 第 1/7/8 条），
				// 对话走 §13 recipe 直发（`request.signer`＝qwenSign），DOM 语义（输入/发送/取值）尚未接。
				ID: "qianwen-web", Type: ChannelTypeWeb, Site: "qianwen",
				EngineAddr: engineAddr, PageURL: DefaultPageURL("qianwen"),
				Models: []ModelConfig{
					{ID: "qwen-chat", Family: "Qwen", Label: "通义千问", Description: "阿里通义千问网页端（页内直发；DOM 语义未接）", Aliases: []string{"qwen", "qianwen", "tongyi"}, Thinking: &thinking, Search: &search},
				},
			},
		},
	}
}

// LoadConfig 读取并解析 channels.json。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 channels.json(%s): %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 channels.json(%s): %w", path, err)
	}
	return &cfg, nil
}

// Normalize 补齐默认值并校验配置；engineAddr 是 uws_web 通道未显式配置时的默认引擎地址。
func (c *Config) Normalize(engineAddr string) error {
	if len(c.Channels) == 0 {
		return errors.New("channels 不能为空")
	}
	seen := make(map[string]bool, len(c.Channels))
	for i := range c.Channels {
		ch := &c.Channels[i]
		if strings.TrimSpace(ch.ID) == "" {
			return fmt.Errorf("channels[%d]: id 必填", i)
		}
		if seen[ch.ID] {
			return fmt.Errorf("channel %q 重复", ch.ID)
		}
		seen[ch.ID] = true
		switch ch.Type {
		case ChannelTypeWeb:
			if ch.Site == "" {
				return fmt.Errorf("channel %q: uws_web 需要 site", ch.ID)
			}
			if ch.Site != "deepseek" && ch.Site != "gemini" && ch.Site != "jimeng" && ch.Site != "doubao" && ch.Site != "seedance" && ch.Site != "perplexity" && ch.Site != "chatgpt" && ch.Site != "claude" && ch.Site != "qianwen" {
				return fmt.Errorf("channel %q: 不支持的 site %q（支持 deepseek|gemini|jimeng|doubao|qianwen|perplexity|chatgpt|claude）", ch.ID, ch.Site)
			}
			if ch.EngineAddr == "" {
				ch.EngineAddr = engineAddr
			}
			if ch.EngineAddr == "" {
				return fmt.Errorf("channel %q: uws_web 需要 engine_addr（或 CLI --engine-addr）", ch.ID)
			}
			if ch.PageURL == "" {
				ch.PageURL = DefaultPageURL(ch.Site)
			}
			if len(ch.Models) == 0 {
				ch.Models = defaultModelConfigs(ch.Site)
			}
		case ChannelTypeOpenAIHTTP:
			if ch.BaseURL == "" {
				return fmt.Errorf("channel %q: openai_http 需要 base_url", ch.ID)
			}
		default:
			return fmt.Errorf("channel %q: 未知 type %q（支持 uws_web|openai_http）", ch.ID, ch.Type)
		}
	}
	return nil
}

// Channel 按 id 查找通道。
func (c *Config) Channel(id string) *ChannelConfig {
	for i := range c.Channels {
		if c.Channels[i].ID == id {
			return &c.Channels[i]
		}
	}
	return nil
}

// ModelEntry 是全局模型表条目（契约 §6：id / channel / capabilities）。
type ModelEntry struct {
	ID          string
	Channel     string
	Label       string
	Description string
	Thinking    bool
	Search      bool
}

// ModelTable 聚合全部通道的模型目录（按通道、id 排序）。
func (c *Config) ModelTable() []ModelEntry {
	var out []ModelEntry
	for i := range c.Channels {
		ch := &c.Channels[i]
		for _, m := range ch.Models {
			out = append(out, ModelEntry{
				ID:          m.ID,
				Channel:     ch.ID,
				Label:       m.Label,
				Description: m.Description,
				Thinking:    m.Thinking != nil && *m.Thinking,
				Search:      m.Search != nil && *m.Search,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Channel != out[j].Channel {
			return out[i].Channel < out[j].Channel
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ChannelByModel 解析模型名（精确 id 或别名）→ 所属通道；未收录返回错误。
// 解析顺序：精确 model id → 通道配置内模型表（含别名），契约 §6。
func (c *Config) ChannelByModel(model string) (*ChannelConfig, *ModelConfig, error) {
	key := normKey(model)
	for i := range c.Channels {
		ch := &c.Channels[i]
		for j := range ch.Models {
			m := &ch.Models[j]
			if normKey(m.ID) == key {
				return ch, m, nil
			}
			for _, alias := range m.Aliases {
				if normKey(alias) == key {
					return ch, m, nil
				}
			}
		}
	}
	return nil, nil, fmt.Errorf("未知模型 %q", model)
}

// defaultModelConfigs 返回站点内置模型目录（Provider 同源）。
func defaultModelConfigs(site string) []ModelConfig {
	thinking := true
	search := true
	noSearch := false
	switch strings.ToLower(site) {
	case "gemini":
		return []ModelConfig{
			{ID: "gemini-3.5-flash-lite", Family: "Flash-Lite", Label: "3.5 Flash-Lite", Description: "极速文本回答", Aliases: []string{"gemini-flash-lite"}, Thinking: &thinking},
			{ID: "gemini-flash", Family: "Flash", Label: "3.8 Flash", Description: "全方位多模态与生图理解（主力 Flash）", Aliases: []string{"gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-2.5-flash"}, Thinking: &thinking, ImageGen: true},
			{ID: "gemini-3.1-pro", Family: "Pro", Label: "3.1 Pro", Description: "高级复杂推理与全能助手", Aliases: []string{"gemini-pro", "gemini-3-pro", "gemini-2.5-pro"}, Thinking: &thinking, ImageGen: true},
			{ID: "gemini-veo", Family: "Veo", Label: "Veo 3.1", Description: "Google Veo 动态视频生成", Aliases: []string{"veo-3.1", "veo-3", "veo", "veo-2", "gemini-video"}, VideoGen: true},
			{ID: "gemini-imagen", Family: "Imagen", Label: "Imagen 3", Description: "Google Imagen 3 高清画作生成", Aliases: []string{"imagen-3", "imagen"}, ImageGen: true},
		}
	case "jimeng":
		return []ModelConfig{
			{ID: "jimeng-image", Family: "Image", Label: "即梦生图", Description: "即梦 AI 每日免费算力生图", Aliases: []string{"jimeng-v2", "dreamina-image", "seedance-image"}, ImageGen: true},
			{ID: "jimeng-video", Family: "Video", Label: "即梦视频 (Seedance 2.0)", Description: "即梦 AI 动态视频与 Seedance 2.0 运镜生成", Aliases: []string{"seedance-2.0", "seedance-2", "seedance", "dreamina-video", "seedance-video"}, VideoGen: true},
		}
	case "doubao", "seedance":
		return []ModelConfig{
			{ID: "doubao-pro", Family: "Pro", Label: "豆包 Pro (1.5 Pro)", Description: "字节跳动中文对话、推理与搜索", Aliases: []string{"doubao", "doubao-1.5-pro"}, Thinking: &thinking, Search: &search},
			{ID: "doubao-image", Family: "Image", Label: "豆包绘图", Description: "豆包多风格生图", Aliases: []string{"doubao-art"}, ImageGen: true},
		}
	case "qianwen":
		return []ModelConfig{
			{ID: "qwen-chat", Family: "Qwen", Label: "通义千问", Description: "阿里通义千问网页端（页内直发；DOM 语义未接）", Aliases: []string{"qwen", "qianwen", "tongyi"}, Thinking: &thinking, Search: &search},
		}
	case "chatgpt":
		return []ModelConfig{
			{ID: "chatgpt-latest", Family: "Latest", Label: "Latest", Description: "模型选择器当前项 Latest（实测站点请求体 model=gpt-6-pro；仅聊天模式）", Aliases: []string{"gpt-6-pro", "gpt-latest", "chatgpt-pro"}},
			{ID: "chatgpt-5.6-sol", Family: "GPT-5.6 Sol", Label: "GPT-5.6 Sol", Description: "模型菜单项 GPT-5.6 Sol（站点请求体 model 字符串未实测）", Aliases: []string{"gpt-5.6-sol", "gpt-5.6"}},
			{ID: "chatgpt-5.5", Family: "GPT-5.5", Label: "GPT-5.5", Description: "模型菜单项 GPT-5.5（站点标注 2026-10-14 下线；请求体 model 字符串未实测）", Aliases: []string{"gpt-5.5"}},
		}
	default:
		return []ModelConfig{
			{ID: "deepseek-chat", Family: "通用模式", Label: "全新统一模型", Description: "日常对话、图片理解与多模态通用应答（默认关深度思考，含智能搜索）", Aliases: []string{"deepseek-v3", "deepseek-fast", "deepseek-vision"}, Thinking: &thinking, Search: &search},
			{ID: "deepseek-reasoner", Family: "深度思考", Label: "深度思考 (R1)", Description: "R1 专家级深度推理与思维链（默认开深度思考；专家模式无智能搜索开关）", Aliases: []string{"deepseek-r1", "deepseek-expert"}, Thinking: &thinking, Search: &noSearch},
		}
	}
}

// normKey 归一化字符串用于模型名/别名/族名匹配（Python providers norm_key 同构）。
func normKey(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "")
}
