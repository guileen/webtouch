package fingerprint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// HTTPClientHints controls which User-Agent Client Hints a pure HTTP request
// sends. A browser sends low-entropy hints by default; high-entropy fields are
// normally sent only after an origin opts in with Accept-CH, so they require an
// explicit caller choice here.
type HTTPClientHints string

const (
	HTTPClientHintsNone HTTPClientHints = "none"
	HTTPClientHintsLow  HTTPClientHints = "low"
	HTTPClientHintsFull HTTPClientHints = "full"
)

// HTTPClientOptions deliberately describes only transport behavior. Browser
// identity comes exclusively from Profile and StateBundle; callers cannot mix
// an arbitrary UA or Cookie header into this type.
type HTTPClientOptions struct {
	Timeout         time.Duration
	FollowRedirects bool
	ClientHints     HTTPClientHints
}

// IdentityHTTPClient is a pure Go HTTP client that carries the HTTP-visible
// part of a fingerprint identity. It is intentionally not a "Chrome HTTP
// client": Go's TLS, HTTP/2 and connection behavior remain Go's, so callers
// must use a real browser for endpoints that bind those transport properties.
type IdentityHTTPClient struct {
	client  *http.Client
	profile *Profile
	hints   HTTPClientHints
}

// NewIdentityHTTPClient constructs a client from one validated identity and,
// optionally, a StateBundle exported from the same identity. SessionStorage is
// browser-tab state and has no HTTP wire representation; only Cookie values are
// loaded into the jar. A mismatched bundle is rejected rather than leaking one
// identity's session through another identity's headers.
func NewIdentityHTTPClient(profile *Profile, bundle *StateBundle, options HTTPClientOptions) (*IdentityHTTPClient, error) {
	if profile == nil {
		return nil, fmt.Errorf("identity http: fingerprint profile 不能为空")
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	hints := options.ClientHints
	if hints == "" {
		hints = HTTPClientHintsLow
	}
	if hints != HTTPClientHintsNone && hints != HTTPClientHintsLow && hints != HTTPClientHintsFull {
		return nil, fmt.Errorf("identity http: client hints 必须是 none、low 或 full")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("identity http: 创建 Cookie jar: %w", err)
	}
	if bundle != nil {
		if err := bundle.Validate(); err != nil {
			return nil, err
		}
		if bundle.ProfileID != profile.ProfileID {
			return nil, fmt.Errorf("identity http: State Bundle profile_id %q 与档案 %q 不一致", bundle.ProfileID, profile.ProfileID)
		}
		if err := seedCookieJar(jar, bundle); err != nil {
			return nil, err
		}
	}
	transport, err := identityHTTPTransport(profile)
	if err != nil {
		return nil, err
	}
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Second
	}
	h := &IdentityHTTPClient{profile: profile, hints: hints}
	h.client = &http.Client{Transport: transport, Jar: jar, Timeout: options.Timeout}
	if !options.FollowRedirects {
		h.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	} else {
		h.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > 10 {
				return fmt.Errorf("identity http: 重定向超过 10 次")
			}
			if len(via) == 0 || !sameHTTPOrigin(req.URL, via[0].URL) {
				return fmt.Errorf("identity http: 不跟随跨 origin 重定向；Go client 无法重建浏览器 SameSite/发起页上下文，请使用浏览器")
			}
			h.prepareRequest(req)
			return nil
		}
	}
	return h, nil
}

// NewRequest creates a request with the profile's immutable HTTP identity
// fields. Context-specific headers such as Referer, Origin and Accept remain
// the caller's responsibility because a pure client cannot honestly infer the
// browser page that initiated an API request.
func (c *IdentityHTTPClient) NewRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	if c == nil || c.client == nil || c.profile == nil {
		return nil, fmt.Errorf("identity http: client 未初始化")
	}
	if strings.TrimSpace(method) == "" {
		return nil, fmt.Errorf("identity http: method 不能为空")
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("identity http: 创建请求: %w", err)
	}
	c.prepareRequest(req)
	return req, nil
}

// Do executes a request created by NewRequest. It remains public for callers
// that need to set contextual headers after identity headers are installed.
// Identity-bearing headers are reset immediately before sending, so a caller
// cannot accidentally mix another profile's Cookie, UA, language or UA-CH
// values into this client.
func (c *IdentityHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("identity http: client 未初始化")
	}
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("identity http: request 不能为空")
	}
	if req.Host != "" && req.Host != req.URL.Host {
		return nil, fmt.Errorf("identity http: 不允许覆写 Host")
	}
	c.prepareRequest(req)
	return c.client.Do(req)
}

func (c *IdentityHTTPClient) prepareRequest(req *http.Request) {
	for _, name := range identityHeaderNames {
		req.Header.Del(name)
	}
	c.applyIdentityHeaders(req.Header)
}

var identityHeaderNames = []string{
	"Cookie", "Host", "Content-Length", "Proxy-Authorization", "User-Agent", "Accept-Language",
	"Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform", "Sec-CH-UA-Full-Version",
	"Sec-CH-UA-Platform-Version", "Sec-CH-UA-Arch", "Sec-CH-UA-Model",
}

func (c *IdentityHTTPClient) applyIdentityHeaders(header http.Header) {
	p := c.profile
	header.Set("User-Agent", p.Emulation.UserAgent)
	if language := p.Overrides().AcceptLanguage; language != "" {
		header.Set("Accept-Language", language)
	}
	if c.hints == HTTPClientHintsNone {
		return
	}
	md := p.Emulation.UserAgentMetadata
	header.Set("Sec-CH-UA", formatCHBrands(md.Brands))
	if md.Mobile {
		header.Set("Sec-CH-UA-Mobile", "?1")
	} else {
		header.Set("Sec-CH-UA-Mobile", "?0")
	}
	header.Set("Sec-CH-UA-Platform", quoteCH(md.Platform))
	if c.hints != HTTPClientHintsFull {
		return
	}
	if md.FullVersion != "" {
		header.Set("Sec-CH-UA-Full-Version", quoteCH(md.FullVersion))
	}
	if md.PlatformVersion != "" {
		header.Set("Sec-CH-UA-Platform-Version", quoteCH(md.PlatformVersion))
	}
	if md.Architecture != "" {
		header.Set("Sec-CH-UA-Arch", quoteCH(md.Architecture))
	}
	if md.Model != "" {
		header.Set("Sec-CH-UA-Model", quoteCH(md.Model))
	}
}

func quoteCH(value string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

func formatCHBrands(brands []Brand) string {
	parts := make([]string, 0, len(brands))
	for _, brand := range brands {
		parts = append(parts, quoteCH(brand.Brand)+";v="+quoteCH(brand.Version))
	}
	return strings.Join(parts, ", ")
}

func sameHTTPOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	if !strings.EqualFold(left.Scheme, right.Scheme) || !strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return httpOriginPort(left.Scheme, left.Port()) == httpOriginPort(right.Scheme, right.Port())
}

// httpOriginPort follows browser origin normalization: an omitted HTTP/HTTPS
// port is the respective default, so https://example.test and
// https://example.test:443 are one origin.
func httpOriginPort(scheme, port string) string {
	if port != "" {
		return port
	}
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func identityHTTPTransport(profile *Profile) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("identity http: 默认 Transport 类型不支持")
	}
	transport := base.Clone()
	if strings.TrimSpace(profile.Network.ProxyURL) == "" {
		return transport, nil
	}
	proxyURL, err := url.Parse(profile.Network.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("identity http: 解析代理失败: %w", err)
	}
	switch proxyURL.Scheme {
	case "http", "https":
		transport.Proxy = http.ProxyURL(proxyURL)
	case "socks5", "socks5h":
		dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("identity http: 配置 SOCKS5 代理失败: %w", err)
		}
		transport.Proxy = nil
		transport.DialContext = dialContext(dialer)
	case "socks4":
		// The profile format accepts this because Chrome can launch with it, but
		// the standard Go client has no SOCKS4 dialer. Direct fallback would make
		// an identity silently change its egress, so reject it here.
		return nil, fmt.Errorf("identity http: SOCKS4 不能由纯 Go client 安全复用；请改用 HTTP/SOCKS5 本地中继或使用浏览器")
	default:
		return nil, fmt.Errorf("identity http: 不支持的代理 scheme %q", proxyURL.Scheme)
	}
	return transport, nil
}

func dialContext(dialer proxy.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if contextual, ok := dialer.(proxy.ContextDialer); ok {
		return contextual.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		type result struct {
			conn net.Conn
			err  error
		}
		completed := make(chan result, 1)
		go func() {
			conn, err := dialer.Dial(network, address)
			completed <- result{conn: conn, err: err}
		}()
		select {
		case result := <-completed:
			return result.conn, result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func seedCookieJar(jar http.CookieJar, bundle *StateBundle) error {
	var rawCookies []map[string]json.RawMessage
	if err := json.Unmarshal(bundle.Cookies, &rawCookies); err != nil {
		return fmt.Errorf("identity http: State Bundle cookies: %w", err)
	}
	for index, raw := range rawCookies {
		if raw == nil {
			return fmt.Errorf("identity http: State Bundle cookies[%d] 必须是对象", index)
		}
		if partitionKey, ok := raw["partitionKey"]; ok && !bytes.Equal(bytes.TrimSpace(partitionKey), []byte("null")) {
			return fmt.Errorf("identity http: State Bundle cookies[%d] 是 partitioned cookie；纯 HTTP jar 不支持，必须使用浏览器", index)
		}
		var cookie struct {
			Name     string  `json:"name"`
			Value    string  `json:"value"`
			URL      string  `json:"url"`
			Domain   string  `json:"domain"`
			Path     string  `json:"path"`
			Secure   bool    `json:"secure"`
			HTTPOnly bool    `json:"httpOnly"`
			Expires  float64 `json:"expires"`
			SameSite string  `json:"sameSite"`
		}
		data, err := json.Marshal(raw)
		if err != nil {
			return fmt.Errorf("identity http: 编码 cookies[%d]: %w", index, err)
		}
		if err := json.Unmarshal(data, &cookie); err != nil {
			return fmt.Errorf("identity http: 解析 cookies[%d]: %w", index, err)
		}
		if cookie.Name == "" {
			return fmt.Errorf("identity http: State Bundle cookies[%d] 缺少 name", index)
		}
		origin, err := cookieOrigin(cookie)
		if err != nil {
			return fmt.Errorf("identity http: State Bundle cookies[%d]: %w", index, err)
		}
		out := &http.Cookie{Name: cookie.Name, Value: cookie.Value, Path: cookie.Path, Secure: cookie.Secure, HttpOnly: cookie.HTTPOnly}
		if out.Path == "" {
			out.Path = "/"
		}
		// Chrome reports a leading dot for a domain cookie. Without it, preserve
		// host-only semantics instead of accidentally expanding the cookie to
		// every subdomain in the Go jar.
		if strings.HasPrefix(cookie.Domain, ".") {
			out.Domain = cookie.Domain
		}
		if cookie.Expires > 0 {
			out.Expires = time.Unix(int64(cookie.Expires), 0)
		}
		switch strings.ToLower(cookie.SameSite) {
		case "lax":
			out.SameSite = http.SameSiteLaxMode
		case "strict":
			out.SameSite = http.SameSiteStrictMode
		case "none", "no_restriction":
			out.SameSite = http.SameSiteNoneMode
		}
		jar.SetCookies(origin, []*http.Cookie{out})
	}
	return nil
}

func cookieOrigin(cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	URL      string  `json:"url"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
	Expires  float64 `json:"expires"`
	SameSite string  `json:"sameSite"`
}) (*url.URL, error) {
	if cookie.URL != "" {
		origin, err := url.Parse(cookie.URL)
		if err != nil || origin.Scheme == "" || origin.Host == "" {
			return nil, fmt.Errorf("url 非法")
		}
		return origin, nil
	}
	host := strings.TrimPrefix(strings.TrimSpace(cookie.Domain), ".")
	if host == "" {
		return nil, fmt.Errorf("缺少 domain 或 url")
	}
	// A secure URL is safe for jar seeding even for a non-secure cookie; that
	// cookie remains available to ordinary HTTP requests under the same host.
	return &url.URL{Scheme: "https", Host: host}, nil
}

// ParseHTTPClientHints parses the CLI/API spelling without exposing a loose
// stringly-typed switch to callers.
func ParseHTTPClientHints(raw string) (HTTPClientHints, error) {
	value := HTTPClientHints(strings.ToLower(strings.TrimSpace(raw)))
	if value == "" {
		value = HTTPClientHintsLow
	}
	if value != HTTPClientHintsNone && value != HTTPClientHintsLow && value != HTTPClientHintsFull {
		return "", fmt.Errorf("client hints 必须是 none、low 或 full")
	}
	return value, nil
}

// HTTPStatusLine is a small, credential-free diagnostic suitable for CLI
// stderr. It deliberately omits response headers, which often carry tokens.
func HTTPStatusLine(response *http.Response) string {
	if response == nil {
		return ""
	}
	return "HTTP " + strconv.Itoa(response.StatusCode) + " " + http.StatusText(response.StatusCode)
}
