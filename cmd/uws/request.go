package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"uws/pkg/fingerprint"
)

// runRequest executes one HTTP request with a fingerprint profile's
// HTTP-visible identity. It deliberately does not involve CDP or start Chrome:
// this is for stable API/resource requests after a browser has established the
// logical session and exported a State Bundle.
func runRequest(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(requestGuide())
		return nil
	}
	options, positional, err := parseRequestFlags(args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || options.profileFile == "" {
		return fmt.Errorf("用法: webtouch request <url> --profile-file <FingerprintProfile.json> [--state <bundle.json> --key-file <key>] [--method GET] [--header 'Name: value'] [--body <text>|--body-file <path>]")
	}
	endpoint, err := url.Parse(positional[0])
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return fmt.Errorf("request: URL 必须是完整的 http(s) 地址")
	}
	if options.body != "" && options.bodyFile != "" {
		return fmt.Errorf("request: --body 与 --body-file 只能二选一")
	}
	profile, err := fingerprint.Load(options.profileFile)
	if err != nil {
		return err
	}
	var bundle *fingerprint.StateBundle
	if options.stateFile != "" {
		bundle, _, err = readStateBundle(options.stateFile, options.keyFile)
		if err != nil {
			return fmt.Errorf("request: 读取 State Bundle: %w", err)
		}
	}
	hints, err := fingerprint.ParseHTTPClientHints(options.clientHints)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	client, err := fingerprint.NewIdentityHTTPClient(profile, bundle, fingerprint.HTTPClientOptions{
		Timeout: options.timeout, FollowRedirects: options.follow, ClientHints: hints,
	})
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	body, err := requestBody(options)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	req, err := client.NewRequest(ctx, options.method, endpoint.String(), body)
	if err != nil {
		return err
	}
	for _, raw := range options.headers {
		name, value, err := parseRequestHeader(raw)
		if err != nil {
			return err
		}
		req.Header.Add(name, value)
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()

	// Body is the command's data contract; status stays on stderr so pipelines
	// can consume JSON/binary responses without a wrapper parsing a banner.
	fmt.Fprintln(os.Stderr, fingerprint.HTTPStatusLine(response))
	if options.output == "" {
		_, err = io.Copy(os.Stdout, response.Body)
		return err
	}
	file, err := os.OpenFile(options.output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("request: 打开 --out: %w", err)
	}
	_, copyErr := io.Copy(file, response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("request: 写响应: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("request: 关闭 --out: %w", closeErr)
	}
	return nil
}

type requestFlagValues []string

func (v *requestFlagValues) String() string { return strings.Join(*v, ",") }
func (v *requestFlagValues) Set(value string) error {
	*v = append(*v, value)
	return nil
}

type requestOptions struct {
	profileFile string
	stateFile   string
	keyFile     string
	method      string
	body        string
	bodyFile    string
	output      string
	clientHints string
	timeout     time.Duration
	follow      bool
	headers     requestFlagValues
}

func parseRequestFlags(args []string) (requestOptions, []string, error) {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	options := requestOptions{method: http.MethodGet, clientHints: string(fingerprint.HTTPClientHintsLow), timeout: 30 * time.Second}
	fs.StringVar(&options.profileFile, "profile-file", "", "FingerprintProfile JSON file")
	fs.StringVar(&options.stateFile, "state", "", "State Bundle file (optional)")
	fs.StringVar(&options.keyFile, "key-file", "", "AES-256 key file for encrypted State Bundle")
	fs.StringVar(&options.method, "method", http.MethodGet, "HTTP method")
	fs.StringVar(&options.body, "body", "", "request body text")
	fs.StringVar(&options.bodyFile, "body-file", "", "request body file")
	fs.StringVar(&options.output, "out", "", "response output file (default stdout)")
	fs.StringVar(&options.clientHints, "client-hints", string(fingerprint.HTTPClientHintsLow), "none|low|full")
	fs.DurationVar(&options.timeout, "timeout", 30*time.Second, "request timeout")
	fs.BoolVar(&options.follow, "follow", false, "follow up to 10 redirects")
	fs.Var(&options.headers, "header", "extra contextual header, repeatable: 'Name: value'")
	if err := fs.Parse(normalizeRequestArgs(args)); err != nil {
		return requestOptions{}, nil, err
	}
	if options.timeout <= 0 {
		return requestOptions{}, nil, fmt.Errorf("request: --timeout 必须为正")
	}
	return options, fs.Args(), nil
}

// Keep public examples natural: positionals may appear before options. The
// set is intentionally closed so a body beginning with -- is not rewritten.
func normalizeRequestArgs(args []string) []string {
	needsValue := map[string]bool{
		"--profile-file": true, "--state": true, "--key-file": true, "--method": true,
		"--body": true, "--body-file": true, "--out": true, "--client-hints": true,
		"--timeout": true, "--header": true,
	}
	boolFlag := map[string]bool{"--follow": true}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if needsValue[arg] {
			flags = append(flags, arg)
			if index+1 < len(args) {
				index++
				flags = append(flags, args[index])
			}
			continue
		}
		if boolFlag[arg] {
			flags = append(flags, arg)
			continue
		}
		matched := false
		for name := range needsValue {
			if strings.HasPrefix(arg, name+"=") {
				flags = append(flags, arg)
				matched = true
				break
			}
		}
		if !matched && strings.HasPrefix(arg, "--follow=") {
			flags = append(flags, arg)
			matched = true
		}
		if !matched {
			positionals = append(positionals, arg)
		}
	}
	return append(flags, positionals...)
}

func requestBody(options requestOptions) (io.Reader, error) {
	if options.bodyFile != "" {
		data, err := os.ReadFile(options.bodyFile)
		if err != nil {
			return nil, fmt.Errorf("request: 读取 --body-file: %w", err)
		}
		return bytes.NewReader(data), nil
	}
	if options.body != "" {
		return strings.NewReader(options.body), nil
	}
	return nil, nil
}

var protectedIdentityHeaders = map[string]bool{
	"cookie": true, "host": true, "content-length": true, "proxy-authorization": true,
	"user-agent": true, "accept-language": true,
	"sec-ch-ua": true, "sec-ch-ua-mobile": true, "sec-ch-ua-platform": true,
	"sec-ch-ua-full-version": true, "sec-ch-ua-platform-version": true,
	"sec-ch-ua-arch": true, "sec-ch-ua-model": true,
}

func parseRequestHeader(raw string) (string, string, error) {
	name, value, ok := strings.Cut(raw, ":")
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if !ok || name == "" {
		return "", "", fmt.Errorf("request: --header 必须写成 'Name: value'")
	}
	if protectedIdentityHeaders[strings.ToLower(name)] {
		return "", "", fmt.Errorf("request: --header %q 由指纹档案或 State Bundle 管理，不能手工覆盖", name)
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
			return "", "", fmt.Errorf("request: --header 名称非法")
		}
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", "", fmt.Errorf("request: --header 值不能含换行")
	}
	return http.CanonicalHeaderKey(name), value, nil
}

func requestGuide() string {
	return `webtouch request —— 带同一身份的纯 HTTP 请求

  webtouch request <url> --profile-file <FingerprintProfile.json> [--state <StateBundle.json> --key-file <key>]
              [--method GET] [--header 'Name: value'] [--body <text>|--body-file <path>]
              [--client-hints none|low|full] [--follow] [--out <response-file>]

用途：某个站点接口不必渲染/执行 JavaScript 时，可不启动浏览器，复用同一份档案的 User-Agent、
Accept-Language、低熵 Client Hints，以及 State Bundle 中同 profile_id 的 Cookies。响应体默认直出
stdout（适合管道）；HTTP 状态只写 stderr；--out 以 0600 写文件。--follow 默认关闭，防止凭证因
重定向被静默带到其他地址；开启后仅允许同 origin（scheme + host）跳转，带 --state 的 Cookie 也只在该
范围内随 jar 发送。跨 origin 跳转会被拒绝，因为 Go client 无法重建浏览器 SameSite / 发起页上下文。

--header 只用于请求上下文（例如 Authorization、Accept、Content-Type、Referer、Origin）；Cookie、UA、
语言、Client Hints 和 Host 不允许手工覆盖，避免拼出一份自相矛盾的身份。SessionStorage 没有 HTTP
线上的等价物；接口若把 token 放在那里，请在你确认后通过 Authorization 等上下文 header 显式传入。

边界：这不是 Chrome 的网络栈。Go client 的 TLS / HTTP2 / 连接复用指纹仍是 Go；需要浏览器级 TLS、
JS、WebRTC 或 partitioned Cookie 的接口必须留在真实 Chrome。代理按档案复用 HTTP/HTTPS/SOCKS5；
SOCKS4 会明确拒绝，绝不悄悄直连。
`
}
