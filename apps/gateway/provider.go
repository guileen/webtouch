package gateway

import (
	"fmt"
	"strings"
)

// ChatModel 是站点模型目录条目（移植 coordinator/app/providers/base.py）。
// Family 是物理层切换标签（版本无关的族名，如 "Flash" 同时匹配 "3.6 Flash"/"3.7 Flash"）。
type ChatModel struct {
	ID          string
	Label       string
	Family      string
	Description string
	Aliases     []string
	Searchable  bool // 该模型是否支持联网/智能搜索（DeepSeek 专家模式 = false）
	ImageGen    bool // 该模型是否支持生图（Imagen 3、即梦生图、豆包生图等）
	VideoGen    bool // 该模型是否支持生视频（Veo、即梦视频等）
}

// ChatProvider 是站点级行为契约（移植 coordinator/app/providers/base.py + deepseek.py + gemini.py）。
// 每个站点（deepseek/gemini/jimeng/doubao）的独有差异——语义映射、模型目录、模型切换标签——
// 收口在这里；共享编排（输入/发送/提取/轮询/done 语义/OpenAI 兼容形状）在 Gateway。
type ChatProvider struct {
	Site                string
	Semantics           map[string]string // input / send / answer / new_chat / sidebar
	Models              []ChatModel
	ModelSwitchSemantic string
	ThinkingSemantic    string
	SearchSemantic      string
	// 模式切换仅在新会话落地页可用（DeepSeek 三模式只在 chat 首页出现）；
	// 为 true 时 API 只在新会话建立时切换模式，多轮不重切。
	ModelOnNewChatOnly bool
	// MenuFamilies 是模型菜单里**应当出现**的档位关键字（大小写不敏感子串匹配，
	// 与切换时的 token 匹配同源），供模型菜单漂移自检比对（见 modelmenu.go）：
	// 缺任意一项即判漂移。空 = 该站点暂无菜单自检。**只登记菜单档位**——
	// 走独立 UI 的能力（Veo/Imagen）不在此列，否则会产生假漂移。
	MenuFamilies []string
	// 输入后等待页面框架启用发送按钮的沉降时间（Gemini Angular 实测 1.5s）。
	InputSettleSec float64
	// 点击「新对话」后等待 SPA 导航完成的沉降时间。
	NewChatSettleSec float64
}

// newProvider 构造站点 Provider；models 为空时用内置目录。
func newProvider(site string, models []ModelConfig) (*ChatProvider, error) {
	switch strings.ToLower(site) {
	case "deepseek":
		return &ChatProvider{
			Site: "deepseek",
			Semantics: map[string]string{
				"input": "chat_input", "send": "send_button", "answer": "chat_answer",
				"new_chat": "new_chat_button", "sidebar": "chat_sidebar_toggle",
				"retry": "chat_retry_button",
			},
			Models:              toChatModels(models, defaultModelConfigs("deepseek")),
			ModelSwitchSemantic: "deepseek_mode_switch",
			ThinkingSemantic:    "deepseek_thinking_switch",
			SearchSemantic:      "deepseek_search_switch",
			ModelOnNewChatOnly:  true,
			InputSettleSec:      1.5,
			NewChatSettleSec:    1.0,
		}, nil
	case "gemini":
		return &ChatProvider{
			Site: "gemini",
			Semantics: map[string]string{
				"input": "gemini_input", "send": "gemini_send", "answer": "gemini_answer",
				"new_chat": "gemini_new_chat", "sidebar": "",
			},
			Models:              toChatModels(models, defaultModelConfigs("gemini")),
			MenuFamilies:        []string{"Flash-Lite", "Flash", "Pro", "thinking"},
			ModelSwitchSemantic: "gemini_model_switch",
			ThinkingSemantic:    "gemini_thinking_switch",
			SearchSemantic:      "",
			ModelOnNewChatOnly:  false,
			InputSettleSec:      1.5,
			NewChatSettleSec:    1.5,
		}, nil
	case "jimeng":
		return &ChatProvider{
			Site: "jimeng",
			Semantics: map[string]string{
				"input": "jimeng_prompt_input", "send": "jimeng_generate_button", "answer": "jimeng_result_card",
				"new_chat": "jimeng_new_button", "sidebar": "",
			},
			Models:              toChatModels(models, defaultModelConfigs("jimeng")),
			ModelSwitchSemantic: "",
			ThinkingSemantic:    "",
			SearchSemantic:      "",
			ModelOnNewChatOnly:  false,
			InputSettleSec:      2.0,
			NewChatSettleSec:    2.0,
		}, nil
	case "doubao", "seedance":
		return &ChatProvider{
			Site: "doubao",
			Semantics: map[string]string{
				"input": "doubao_input", "send": "doubao_send", "answer": "doubao_answer",
				"new_chat": "doubao_new_chat", "sidebar": "",
			},
			Models:              toChatModels(models, defaultModelConfigs("doubao")),
			ModelSwitchSemantic: "",
			ThinkingSemantic:    "doubao_thinking_switch",
			SearchSemantic:      "doubao_search_switch",
			ModelOnNewChatOnly:  false,
			InputSettleSec:      1.5,
			NewChatSettleSec:    1.5,
		}, nil
	case "qianwen":
		// 千问对话走 §13 recipe 页内直发（`request.signer`＝`qwenSign`）；DOM 语义（输入/发送/取值）
		// 尚未接 ⇒ Semantics 留空，DOM 路径会显式失败，不会给出「看起来能聊」的假通路。
		return &ChatProvider{
			Site:                "qianwen",
			Semantics:           map[string]string{},
			Models:              toChatModels(models, defaultModelConfigs("qianwen")),
			ModelSwitchSemantic: "",
			ThinkingSemantic:    "",
			SearchSemantic:      "",
			ModelOnNewChatOnly:  false,
			InputSettleSec:      1.5,
			NewChatSettleSec:    1.5,
		}, nil
	case "perplexity", "claude":
		return &ChatProvider{
			Site: strings.ToLower(site),
			Semantics: map[string]string{
				"input": "chat_input", "send": "send_button", "answer": "chat_answer",
				"new_chat": "new_chat_button",
			},
			Models: toChatModels(models, nil),
		}, nil
	case "chatgpt":
		// ChatGPT（chatgpt.com，2026-10-02 真机 DOM 读数）：输入 = ProseMirror
		// contenteditable（aria-label「询问 ChatGPT」）；发送 = button[aria-label="发送"]
		// （输入非空后才出现）；回答 = [data-message-author-role="assistant"]。
		// 页内直发 recipe 被 Cloudflare JSD 哨兵挡（finalize 必须带挑战脚本产出的
		// proofofwork/turnstile；只带 prepare_token 会被 403「Unusual activity」）⇒
		// ChatGPT 只走 DOM 通路，由站点自己铸哨兵 token。
		return &ChatProvider{
			Site: "chatgpt",
			Semantics: map[string]string{
				"input": "chatgpt_input", "send": "chatgpt_send", "answer": "chatgpt_answer",
				"new_chat": "chatgpt_new_chat",
			},
			Models:           toChatModels(models, defaultModelConfigs("chatgpt")),
			InputSettleSec:   1.5,
			NewChatSettleSec: 1.5,
		}, nil
	default:
		return nil, fmt.Errorf("不支持的聊天站点 %q", site)
	}
}

// toChatModels 把通道配置模型目录转为 Provider 模型目录；cfg 为空时回退内置。
func toChatModels(cfg []ModelConfig, fallback []ModelConfig) []ChatModel {
	if len(cfg) == 0 {
		cfg = fallback
	}
	out := make([]ChatModel, 0, len(cfg))
	for _, m := range cfg {
		out = append(out, ChatModel{
			ID:          m.ID,
			Label:       m.Label,
			Family:      m.Family,
			Description: m.Description,
			Aliases:     m.Aliases,
			Searchable:  m.Search == nil || *m.Search,
			ImageGen:    m.ImageGen,
			VideoGen:    m.VideoGen,
		})
	}
	return out
}

// modelSearchable 返回指定模型（id/别名）是否支持联网/智能搜索；
// 未收录模型保守返回 true（允许物理层尝试，真实不支持由物理层显式报错）。
func (p *ChatProvider) modelSearchable(model string) bool {
	key := normKey(model)
	for _, m := range p.Models {
		if normKey(m.ID) == key || normKey(m.Label) == key || normKey(m.Family) == key {
			return m.Searchable
		}
		for _, alias := range m.Aliases {
			if normKey(alias) == key {
				return m.Searchable
			}
		}
	}
	return true
}

// resolveModel 把 API model 参数解析为物理层切换标签（family）；空 = 不切换。
func (p *ChatProvider) resolveModel(model string) string {
	raw := strings.TrimSpace(model)
	if raw == "" {
		return ""
	}
	switch p.Site {
	case "deepseek":
		return resolveDeepSeekModel(p, raw)
	case "gemini":
		return resolveGeminiModel(p, raw)
	default:
		key := normKey(raw)
		for _, m := range p.Models {
			if normKey(m.ID) == key || normKey(m.Label) == key || normKey(m.Family) == key {
				return m.Family
			}
			for _, alias := range m.Aliases {
				if normKey(alias) == key {
					return m.Family
				}
			}
		}
		return raw
	}
}

// defaultModel 返回站点默认模式族名（模型目录首项）：DeepSeek 等
// 「仅新会话可切」站点的默认模式（快速模式），用于未锁定会话的模式比较。
func (p *ChatProvider) defaultModel() string {
	if p.ModelOnNewChatOnly && len(p.Models) > 0 {
		return p.Models[0].Family
	}
	return ""
}

// resolveDeepSeekModel：id/别名/显示名/族名 → 物理层模式标签；未收录返回 ""（不切换）。
func resolveDeepSeekModel(p *ChatProvider, raw string) string {
	key := normKey(raw)
	for _, m := range p.Models {
		if normKey(m.ID) == key || normKey(m.Label) == key || normKey(m.Family) == key {
			return m.Family
		}
		for _, alias := range m.Aliases {
			if normKey(alias) == key {
				return m.Family
			}
		}
	}
	return ""
}

// resolveGeminiModel：id/别名/显示名 → 族名；未收录字符串直接透传（登录后新档位）。
// 契约 §6 / Tasks.md：Gemini 选择器实测仅 3 档（Flash-Lite / Flash / Pro），Veo/Imagen
// 为生成式能力，不在此选择器中；物理层返回空（不切换选择器），避免误触不存在菜单导致 model_switch_failed。
func resolveGeminiModel(p *ChatProvider, raw string) string {
	key := normKey(raw)
	for _, m := range p.Models {
		if normKey(m.ID) == key || normKey(m.Label) == key {
			if m.VideoGen || (m.ImageGen && (m.Family == "Imagen" || m.ID == "gemini-imagen")) {
				return ""
			}
			return m.Family
		}
		for _, alias := range m.Aliases {
			if normKey(alias) == key {
				if m.VideoGen || (m.ImageGen && (m.Family == "Imagen" || m.ID == "gemini-imagen")) {
					return ""
				}
				return m.Family
			}
		}
	}
	return raw
}
