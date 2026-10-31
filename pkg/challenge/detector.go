package challenge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// DirectScriptEvaluator 是在页面运行时执行轻量 JS 表达式的原语接口。
type DirectScriptEvaluator interface {
	EvaluateScript(ctx context.Context, expr string) ([]byte, error)
}

// FastDOMProbeScript 是 0-Token 纯算极速 DOM/iframe 挑战探针脚本。
// 耗时 < 2ms，支持穿透主页面与嵌套容器，精准识别 PayPal、Cloudflare、Arkose、reCAPTCHA、hCaptcha。
const FastDOMProbeScript = `(() => {
	try {
		// 1. 探测 Paypal Security Challenge
		if (document.querySelector('#captcha-container, div[data-testid="security-challenge"], #security-challenge, iframe[src*="paypal.com/auth/challenge"], iframe[src*="paypalobjects.com/pa/service/recaptcha"]')) {
			const el = document.querySelector('#captcha-container, div[data-testid="security-challenge"], #security-challenge, iframe[src*="paypal.com/auth/challenge"], iframe[src*="paypalobjects.com/pa/service/recaptcha"]');
			return JSON.stringify({
				type: "paypal_security_challenge",
				provider: "PayPal",
				selector: el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + (el.className ? '.' + el.className.split(' ').join('.') : ''),
				reason: "DOM 命中 PayPal 安全质检容器或专属认证 iframe",
				confidence: 0.99
			});
		}

		// 2. 探测 Cloudflare Turnstile
		if (document.querySelector('iframe[src*="challenges.cloudflare.com"], .cf-turnstile, div[id^="cf-turnstile"], #cf-challenge-running, #challenge-stage, #turnstile-wrapper')) {
			const el = document.querySelector('iframe[src*="challenges.cloudflare.com"], .cf-turnstile, div[id^="cf-turnstile"], #cf-challenge-running, #challenge-stage, #turnstile-wrapper');
			return JSON.stringify({
				type: "cloudflare_turnstile",
				provider: "Cloudflare",
				selector: el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + (el.className ? '.' + el.className.split(' ').join('.') : ''),
				reason: "DOM 命中 Cloudflare Turnstile 质检盾或挑战容器",
				confidence: 0.99
			});
		}

		// 3. 探测 Arkose Labs (FunCaptcha)
		if (document.querySelector('iframe[src*="arkoselabs.com"], iframe[src*="client-api.arkoselabs.com"], #fc-iframe-wrap, div[id^="arkose"], div[data-arkose-challenge]')) {
			const el = document.querySelector('iframe[src*="arkoselabs.com"], iframe[src*="client-api.arkoselabs.com"], #fc-iframe-wrap, div[id^="arkose"], div[data-arkose-challenge]');
			return JSON.stringify({
				type: "arkose_labs_funcaptcha",
				provider: "Arkose Labs",
				selector: el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + (el.className ? '.' + el.className.split(' ').join('.') : ''),
				reason: "DOM 命中 Arkose Labs (FunCaptcha) 拼图/游戏质检容器",
				confidence: 0.98
			});
		}

		// 4. 探测 Google reCAPTCHA
		if (document.querySelector('iframe[src*="google.com/recaptcha"], iframe[src*="recaptcha/api2"], .g-recaptcha, #g-recaptcha-response')) {
			const el = document.querySelector('iframe[src*="google.com/recaptcha"], iframe[src*="recaptcha/api2"], .g-recaptcha, #g-recaptcha-response');
			return JSON.stringify({
				type: "google_recaptcha",
				provider: "Google",
				selector: el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + (el.className ? '.' + el.className.split(' ').join('.') : ''),
				reason: "DOM 命中 Google reCAPTCHA 交互式验证码",
				confidence: 0.98
			});
		}

		// 5. 探测 hCaptcha
		if (document.querySelector('iframe[src*="hcaptcha.com"], .h-captcha, iframe[data-hcaptcha-widget-id]')) {
			const el = document.querySelector('iframe[src*="hcaptcha.com"], .h-captcha, iframe[data-hcaptcha-widget-id]');
			return JSON.stringify({
				type: "hcaptcha",
				provider: "hCaptcha",
				selector: el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + (el.className ? '.' + el.className.split(' ').join('.') : ''),
				reason: "DOM 命中 hCaptcha 验证码组件",
				confidence: 0.98
			});
		}

		// 6. 泛化特征：页面包含显式验证码标题但无特定容器
		const title = (document.title || '').toLowerCase();
		if (title.includes('security challenge') || title.includes('verify you are human') || title.includes('just a moment...')) {
			return JSON.stringify({
				type: "generic_waf_challenge",
				provider: "WAF",
				selector: "title",
				reason: "网页标题命中人机质检特征: " + document.title,
				confidence: 0.85
			});
		}
	} catch(e) {
		return JSON.stringify({ error: String(e) });
	}
	return "null";
})()`

// ChallengeDetector 是人机验证与风控挑战态感知雷达。
type ChallengeDetector struct{}

// NewDetector 创建新的挑战探测器。
func NewDetector() *ChallengeDetector {
	return &ChallengeDetector{}
}

// DetectStatic 纯静态快速探测：依据 URL、标题、HTTP 状态码及响应头秒级判断。
func (d *ChallengeDetector) DetectStatic(pageURL, title string, httpStatus int, headers map[string]string) *ChallengeSignal {
	now := time.Now()
	urlLower := strings.ToLower(pageURL)
	titleLower := strings.ToLower(title)

	// 1. Paypal Security Challenge 静态识别
	if strings.Contains(urlLower, "paypal.com/auth/challenge") ||
		strings.Contains(urlLower, "paypal.com/signin/challenge") ||
		(strings.Contains(urlLower, "paypal.com") && strings.Contains(titleLower, "security challenge")) {
		return &ChallengeSignal{
			Type:       ChallengeTypePayPal,
			Provider:   "PayPal",
			Confidence: 0.98,
			Reason:     "URL或标题命中 PayPal Security Challenge 认证挑战特征",
			URL:        pageURL,
			DetectedAt: now,
		}
	}

	// 2. Cloudflare 5秒盾 / Turnstile
	if strings.Contains(titleLower, "just a moment...") || strings.Contains(titleLower, "attention required! | cloudflare") {
		return &ChallengeSignal{
			Type:       ChallengeTypeCloudflareInterstitial,
			Provider:   "Cloudflare",
			Confidence: 0.99,
			Reason:     "标题命中 Cloudflare 5秒等待质检盾（Just a moment...）",
			URL:        pageURL,
			DetectedAt: now,
		}
	}
	if strings.Contains(urlLower, "challenges.cloudflare.com") {
		return &ChallengeSignal{
			Type:       ChallengeTypeCloudflareTurnstile,
			Provider:   "Cloudflare",
			Confidence: 0.99,
			Reason:     "URL 包含 challenges.cloudflare.com Turnstile 端点",
			URL:        pageURL,
			DetectedAt: now,
		}
	}

	// 3. 响应头与 HTTP 状态码特征匹配
	if httpStatus == http.StatusForbidden || httpStatus == http.StatusTooManyRequests || httpStatus == http.StatusServiceUnavailable {
		for k, v := range headers {
			kl := strings.ToLower(k)
			vl := strings.ToLower(v)
			if strings.Contains(kl, "cf-mitigated") || strings.Contains(kl, "cf-challenge") ||
				strings.Contains(vl, "challenge") || strings.Contains(vl, "turnstile") {
				return &ChallengeSignal{
					Type:       ChallengeTypeCloudflareTurnstile,
					Provider:   "Cloudflare",
					Confidence: 0.95,
					Reason:     "HTTP 状态码异常且命中 Cloudflare 挑战响应头: " + k,
					URL:        pageURL,
					Headers:    headers,
					DetectedAt: now,
				}
			}
		}
		if strings.Contains(titleLower, "access denied") || strings.Contains(titleLower, "security check") || strings.Contains(titleLower, "verification") {
			return &ChallengeSignal{
				Type:       ChallengeTypeGenericWAF,
				Provider:   "WAF",
				Confidence: 0.88,
				Reason:     "HTTP 异常状态码并伴随 WAF 安全拦截标题",
				URL:        pageURL,
				Headers:    headers,
				DetectedAt: now,
			}
		}
	}

	// 4. Arkose Labs / reCAPTCHA / hCaptcha URL
	if strings.Contains(urlLower, "arkoselabs.com") || strings.Contains(urlLower, "client-api.arkoselabs.com") {
		return &ChallengeSignal{
			Type:       ChallengeTypeArkose,
			Provider:   "Arkose Labs",
			Confidence: 0.95,
			Reason:     "URL 包含 Arkose Labs 挑战服务域名",
			URL:        pageURL,
			DetectedAt: now,
		}
	}
	if strings.Contains(urlLower, "google.com/recaptcha") || strings.Contains(urlLower, "recaptcha/api2") {
		return &ChallengeSignal{
			Type:       ChallengeTypeRecaptcha,
			Provider:   "Google",
			Confidence: 0.95,
			Reason:     "URL 包含 Google reCAPTCHA 资源路径",
			URL:        pageURL,
			DetectedAt: now,
		}
	}
	if strings.Contains(urlLower, "hcaptcha.com") {
		return &ChallengeSignal{
			Type:       ChallengeTypeHCaptcha,
			Provider:   "hCaptcha",
			Confidence: 0.95,
			Reason:     "URL 包含 hCaptcha 资源路径",
			URL:        pageURL,
			DetectedAt: now,
		}
	}

	return nil
}

// Detect 综合探测：先进行 0-Token 静态分析，若未确诊则注入极速 DOM/iframe JS 探针。
func (d *ChallengeDetector) Detect(
	ctx context.Context,
	eval DirectScriptEvaluator,
	pageURL, title string,
	httpStatus int,
	headers map[string]string,
) *ChallengeSignal {
	// 静态特征直接确诊
	if sig := d.DetectStatic(pageURL, title, httpStatus, headers); sig != nil && sig.Confidence >= 0.90 {
		return sig
	}

	// 若未确诊且支持页内 JS 评估，执行极速 DOM 探针
	if eval != nil {
		probeCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		resBytes, err := eval.EvaluateScript(probeCtx, FastDOMProbeScript)
		if err == nil && len(resBytes) > 0 {
			var rawStr string
			if json.Unmarshal(resBytes, &rawStr) == nil && rawStr != "null" && rawStr != "" {
				var probeSig struct {
					Type       ChallengeType `json:"type"`
					Provider   string        `json:"provider"`
					Selector   string        `json:"selector"`
					Reason     string        `json:"reason"`
					Confidence float64       `json:"confidence"`
				}
				if json.Unmarshal([]byte(rawStr), &probeSig) == nil && probeSig.Type != "" {
					return &ChallengeSignal{
						Type:       probeSig.Type,
						Provider:   probeSig.Provider,
						Confidence: probeSig.Confidence,
						Reason:     probeSig.Reason,
						Selector:   probeSig.Selector,
						URL:        pageURL,
						Headers:    headers,
						DetectedAt: time.Now(),
					}
				}
			}
		}
	}

	// 降级兜底静态低置信度特征
	return d.DetectStatic(pageURL, title, httpStatus, headers)
}
