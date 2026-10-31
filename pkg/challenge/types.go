package challenge

import (
	"fmt"
	"time"
)

// ChallengeType 标识具体的人机验证/风控挑战类型。
type ChallengeType string

const (
	// ChallengeTypePayPal 是 Paypal 专属安全挑战态（Security Challenge / Captcha 容器）。
	ChallengeTypePayPal ChallengeType = "paypal_security_challenge"

	// ChallengeTypeCloudflareTurnstile 是 Cloudflare 现代化无感/交互式 Turnstile 质检盾。
	ChallengeTypeCloudflareTurnstile ChallengeType = "cloudflare_turnstile"

	// ChallengeTypeCloudflareInterstitial 是 Cloudflare 5秒盾/等待质检页（Just a moment...）。
	ChallengeTypeCloudflareInterstitial ChallengeType = "cloudflare_interstitial_5s"

	// ChallengeTypeArkose 是 Arkose Labs (FunCaptcha) 旋转/益智/拼图游戏挑战。
	ChallengeTypeArkose ChallengeType = "arkose_labs_funcaptcha"

	// ChallengeTypeRecaptcha 是 Google reCAPTCHA (v2 Checkbox, v3, Enterprise)。
	ChallengeTypeRecaptcha ChallengeType = "google_recaptcha"

	// ChallengeTypeHCaptcha 是 hCaptcha 图像/点选质检盾。
	ChallengeTypeHCaptcha ChallengeType = "hcaptcha"

	// ChallengeTypeGenericWAF 是泛化 WAF/反爬拦截态（403/429/Bot Wall）。
	ChallengeTypeGenericWAF ChallengeType = "generic_waf_challenge"
)

// ChallengeSignal 记录探测到的挑战态确凿物理证据。
type ChallengeSignal struct {
	Type       ChallengeType     `json:"type"`
	Provider   string            `json:"provider"`          // 提供商（Paypal, Cloudflare, Arkose, Google, hCaptcha, WAF）
	Confidence float64           `json:"confidence"`        // 匹配置信度 [0.0, 1.0]
	Reason     string            `json:"reason"`            // 命中特征/根因
	Selector   string            `json:"selector"`          // 命中的容器选择器或 iframe src
	URL        string            `json:"url"`               // 页面或 iframe URL
	Headers    map[string]string `json:"headers,omitempty"` // 命中的关键挑战响应头
	DetectedAt time.Time         `json:"detected_at"`       // 探测时间
}

func (s *ChallengeSignal) String() string {
	if s == nil {
		return "none"
	}
	return fmt.Sprintf("[%s] provider=%s reason=%q conf=%.2f", s.Type, s.Provider, s.Reason, s.Confidence)
}

// ProviderAntiThrashing 是防乱点熔断器因「连续失败」自产信号的 Provider 标识。
const ProviderAntiThrashing = "Anti-thrashing"

// ProviderCircuitBreaker 是人工/外部策略主动拉起熔断的 Provider 标识。
const ProviderCircuitBreaker = "CircuitBreaker"

// IsHumanChallenge 报告该信号是否指向页面真实存在的人机验证/风控挑战。
// 熔断器自产的「连续失败」「人工拉起」信号返回 false —— 它们只是自动化熔断，
// 不是页面存在 CAPTCHA/WAF 的证据（TDD-ENG-05 误报修复，2026-09-23）。
func (s *ChallengeSignal) IsHumanChallenge() bool {
	if s == nil {
		return false
	}
	switch s.Provider {
	case ProviderAntiThrashing, ProviderCircuitBreaker:
		return false
	}
	return true
}

// CircuitState 表示物理防乱点熔断器的生命周期状态。
type CircuitState string

const (
	// CircuitClosed 正常闭合态：允许自动化物理点击与键盘操作。
	CircuitClosed CircuitState = "CLOSED"

	// CircuitOpen 熔断开路态：强制阻断一切自动化重试与物理操作，杜绝乱点被封。
	CircuitOpen CircuitState = "OPEN"

	// CircuitHalfOpen 半开探测态：试探性检查挑战是否已由人工或外部解密完成。
	CircuitHalfOpen CircuitState = "HALF_OPEN"
)

// HumanInputType 表示人工接管或用户在流式视窗上的物理动作类型。
type HumanInputType string

const (
	HumanInputClick      HumanInputType = "click"
	HumanInputMouseMove  HumanInputType = "mouse_move"
	HumanInputMouseDown  HumanInputType = "mouse_down"
	HumanInputMouseUp    HumanInputType = "mouse_up"
	HumanInputKeyDown    HumanInputType = "key_down"
	HumanInputKeyUp      HumanInputType = "key_up"
	HumanInputInsertText HumanInputType = "insert_text"
)

// HumanInputEvent 是从 Web 流式视窗逆向发往浏览器 CDP 的人类交互事件。
type HumanInputEvent struct {
	Type       HumanInputType `json:"type"`
	X          float64        `json:"x,omitempty"`
	Y          float64        `json:"y,omitempty"`
	Button     string         `json:"button,omitempty"` // left | right | middle
	ClickCount int            `json:"click_count,omitempty"`
	Key        string         `json:"key,omitempty"`  // Enter, Backspace, Tab 等按键名
	Text       string         `json:"text,omitempty"` // 插入的文本
}
