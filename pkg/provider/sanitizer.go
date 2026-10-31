package provider

import (
	"net/url"
	"regexp"
	"strings"
)

var sensitiveQueryKeys = map[string]bool{
	"token":         true,
	"access_token":  true,
	"refresh_token": true,
	"id_token":      true,
	"auth":          true,
	"authorization": true,
	"secret":        true,
	"client_secret": true,
	"api_key":       true,
	"apikey":        true,
	"key":           true,
	"password":      true,
	"pwd":           true,
	"passwd":        true,
	"session":       true,
	"session_id":    true,
	"sid":           true,
	"sig":           true,
	"signature":     true,
	"credential":    true,
	"private_key":   true,
}

var sensitiveHeaderKeys = map[string]bool{
	"cookie":              true,
	"set-cookie":          true,
	"authorization":       true,
	"proxy-authorization": true,
	"x-auth-token":        true,
	"x-api-key":           true,
	"x-access-token":      true,
	"sec-websocket-key":   true,
}

var (
	reBearer        = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9_\-\.+=/]+`)
	reInlineSecret  = regexp.MustCompile(`(?i)\b(token|key|secret|session_id|apikey|api_key|password)=([^\s&"']+)`)
	reScriptTag     = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyleTag      = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reBase64Img     = regexp.MustCompile(`(?i)data:image\/[a-zA-Z0-9\+\-]+;base64,[A-Za-z0-9+/=]+`)
	rePasswordValue = regexp.MustCompile(`(?i)(type=["']password["'][^>]*?value=["'])([^"']*)(["'])`)
)

// Sanitizer 提供对 URL、请求头、日志与 DOM 的脱敏与提纯功能。
type Sanitizer struct{}

// NewSanitizer 创建脱敏器实例。
func NewSanitizer() *Sanitizer {
	return &Sanitizer{}
}

// SanitizeURL 清理 URL 中的鉴权凭据与敏感 Query 参数。
func (s *Sanitizer) SanitizeURL(rawURL string) (string, int) {
	redacted := 0
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, 0
	}

	// 1. 清理嵌入在 URL 中的 Basic Auth 用户名密码 (user:pass@host)
	if u.User != nil {
		u.User = nil
		redacted++
	}

	// 2. 清理敏感 Query 参数
	q := u.Query()
	modified := false
	for k, vals := range q {
		lowerK := strings.ToLower(strings.TrimSpace(k))
		if sensitiveQueryKeys[lowerK] {
			for i := range vals {
				vals[i] = "[REDACTED]"
				redacted++
			}
			q[k] = vals
			modified = true
		}
	}

	if modified {
		u.RawQuery = q.Encode()
	}

	return u.String(), redacted
}

// SanitizeHeaders 剥离敏感 HTTP 请求头。
func (s *Sanitizer) SanitizeHeaders(headers map[string]string) (map[string]string, int) {
	if headers == nil {
		return nil, 0
	}
	clean := make(map[string]string, len(headers))
	redacted := 0

	for k, v := range headers {
		lowerK := strings.ToLower(strings.TrimSpace(k))
		if sensitiveHeaderKeys[lowerK] {
			redacted++
			continue // 彻底剔除敏感头
		}
		clean[k] = v
	}

	return clean, redacted
}

// SanitizeText 清理控制台错误与日志中的内嵌凭据。
func (s *Sanitizer) SanitizeText(text string) (string, int) {
	redacted := 0
	out := reBearer.ReplaceAllStringFunc(text, func(m string) string {
		redacted++
		return "Bearer [REDACTED]"
	})
	out = reInlineSecret.ReplaceAllStringFunc(out, func(m string) string {
		redacted++
		parts := strings.SplitN(m, "=", 2)
		if len(parts) == 2 {
			return parts[0] + "=[REDACTED]"
		}
		return m
	})
	return out, redacted
}

// SanitizeDOM 剥离脚本、样式、Base64 超大内联图片及密码字段输入。
func (s *Sanitizer) SanitizeDOM(rawHTML string) (string, int) {
	redacted := 0

	out := reScriptTag.ReplaceAllStringFunc(rawHTML, func(m string) string {
		redacted++
		return "<!-- [REDACTED_SCRIPT] -->"
	})

	out = reStyleTag.ReplaceAllStringFunc(out, func(m string) string {
		redacted++
		return "<!-- [REDACTED_STYLE] -->"
	})

	out = reBase64Img.ReplaceAllStringFunc(out, func(m string) string {
		redacted++
		return "data:image/png;base64,[REDACTED_BASE64]"
	})

	out = rePasswordValue.ReplaceAllStringFunc(out, func(m string) string {
		redacted++
		return `type="password" value="[REDACTED]"`
	})

	return out, redacted
}
