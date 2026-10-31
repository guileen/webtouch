// Package fingerprint 承载「浏览器身份档案」（Fingerprint Profile）：一个虚拟身份
// 的全部可复现描述，纯数据、可版本化、可跨端下发。
//
// 定位（2026-09-12 方案定稿，docs/fingerprint-and-viewer-final-spec-2026-09-12.md §2）：
// 本包只做**解析、校验与派生**——档案文件长什么样、哪些组合自相矛盾、这份档案该变成
// 哪些浏览器启动参数与哪些 CDP 覆盖。真正把它们打到浏览器上的事归 pkg/browser（那里
// 才有 CDP 客户端），本包不依赖仓内任何其他包。
//
// 诚实边界（别在实现里假装能做到）：
//   - 本包让**配置层**跨端可复现（UA/UA-CH、时区、语言、屏幕、种子）；
//     物理层（真实 GPU、字体光栅化引擎）跨端永远做不到，见规格 §1 的 L4/L5。
//   - 校验里做的「矛盾检测」只覆盖档案自己内部的自洽（例如 UA 说 macOS 而
//     Client Hints 说 Windows）。与环境（出口 IP 的地理位置、真实字体）的自洽要靠
//     部署期保证，本包管不了。
package fingerprint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// SchemaV1 是 v1 档案的 $schema 标识。
const SchemaV1 = "https://webtouch.dev/schemas/fingerprint-profile.v1.json"

// Profile 是一份完整的浏览器身份档案（对应 fingerprint-profile.v1.json）。
type Profile struct {
	Schema         string         `json:"$schema"`
	ProfileID      string         `json:"profile_id"`
	Version        string         `json:"version"`
	CreatedAt      string         `json:"created_at"`
	Seed           uint32         `json:"seed"`
	TargetPlatform TargetPlatform `json:"target_platform"`
	Emulation      Emulation      `json:"emulation"`
	NoiseFeatures  NoiseFeatures  `json:"noise_features"`
	Network        Network        `json:"network"`
}

// TargetPlatform 是我们**瞄准**的平台（用于选版本、对账），不是浏览器自报的东西。
type TargetPlatform struct {
	OS            string `json:"os"`
	OSVersion     string `json:"os_version"`
	Architecture  string `json:"architecture"`
	ChromeVersion string `json:"chrome_version"`
}

type Emulation struct {
	UserAgent         string            `json:"user_agent"`
	UserAgentMetadata UserAgentMetadata `json:"user_agent_metadata"`
	Screen            Screen            `json:"screen"`
	Locale            Locale            `json:"locale"`
	Hardware          Hardware          `json:"hardware"`
}

// UserAgentMetadata 字段名与 CDP 的 `Emulation.UserAgentMetadata` 逐字对齐：它会
// 原样透传给 `Emulation.setUserAgentOverride`，所以这里不许改名。
type UserAgentMetadata struct {
	Brands          []Brand `json:"brands"`
	FullVersion     string  `json:"fullVersion"`
	Platform        string  `json:"platform"`
	PlatformVersion string  `json:"platformVersion"`
	Architecture    string  `json:"architecture"`
	Model           string  `json:"model"`
	Mobile          bool    `json:"mobile"`
}

type Brand struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

type Screen struct {
	Width             int     `json:"width"`
	Height            int     `json:"height"`
	DeviceScaleFactor float64 `json:"device_scale_factor"`
	ColorDepth        int     `json:"color_depth"`
}

type Locale struct {
	TimezoneID string   `json:"timezone_id"`
	Languages  []string `json:"languages"`
}

// Hardware 的零值表示「不覆盖，交给浏览器说实话」。
type Hardware struct {
	HardwareConcurrency int     `json:"hardware_concurrency"`
	DeviceMemory        float64 `json:"device_memory"`
	WebGL               WebGL   `json:"webgl"`
}

// WebGL 是 WebGL debug renderer 的稳定自报值。两个字段必须成对出现：只覆盖
// 其中一个会制造比原始环境更明显的矛盾。
type WebGL struct {
	Vendor   string `json:"vendor"`
	Renderer string `json:"renderer"`
}

type NoiseFeatures struct {
	Canvas2D     bool `json:"canvas_2d"`
	AudioContext bool `json:"audio_context"`
	WebGLBuffer  bool `json:"webgl_buffer"`
}

type Network struct {
	ProxyURL string `json:"proxy_url"`
	// WebRTCPolicy 是 WebRTC 的 IP 暴露策略（落到 Chrome 的
	// `--force-webrtc-ip-handling-policy=<value>`）。空 = 走 EffectiveWebRTCPolicy 的默认规则。
	//
	// 为什么需要它（2026-09-13 外站验收实测）：`--proxy-server` 只接管 TCP（HTTP/SOCKS5），
	// WebRTC 的 UDP 直接出网卡——BrowserLeaks 当场报出
	// `WebRTC IP doesn't match your Remote IP`（srflx 是本机真实出口，HTTP 出口是代理）。
	// 也就是「绑了代理」≠「全链路隔离」。
	WebRTCPolicy string `json:"webrtc_policy"`
}

// WebRTC IP 暴露策略取值（与 Chrome `--force-webrtc-ip-handling-policy` 逐字对齐）。
const (
	WebRTCPolicyDefault             = "default"
	WebRTCPolicyPublicInterfaceOnly = "default_public_interface_only"
	WebRTCPolicyPublicAndPrivate    = "default_public_and_private_interfaces"
	WebRTCPolicyDisableNonProxied   = "disable_non_proxied_udp"
)

// webRTCPolicies 是允许的取值（Chrome 的四个枚举 + 空）。
var webRTCPolicies = []string{
	WebRTCPolicyDefault,
	WebRTCPolicyPublicInterfaceOnly,
	WebRTCPolicyPublicAndPrivate,
	WebRTCPolicyDisableNonProxied,
}

// Overrides 是一份档案在 CDP 侧要落地的全部覆盖。空字段 = 不覆盖。
type Overrides struct {
	UserAgent         string
	AcceptLanguage    string
	Locale            string
	Timezone          string
	UserAgentMetadata *UserAgentMetadata
	Screen            *Screen
}

// Parse 解析一份档案：未知字段一律拒绝（手写配置里的拼写错误必须当场炸，而不是
// 悄悄按默认值跑出一个「看起来像但不对」的身份）。
func Parse(data []byte) (*Profile, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var p Profile
	if err := decoder.Decode(&p); err != nil {
		return nil, fmt.Errorf("fingerprint: 解析档案失败: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("fingerprint: 解析档案失败: JSON 文档后不允许再有内容")
		}
		return nil, fmt.Errorf("fingerprint: 解析档案失败: JSON 结尾非法: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Load 从文件读一份档案。
func Load(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: 读档案失败: %w", err)
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Validate 校验档案自身是否自洽。这里挡的都是「一眼假」的组合——它们比不伪装更容易
// 被风控抓（规格 §1 的 L2：UA 说 macOS 而 Client Hints 说 Windows 是最典型的穿帮）。
func (p *Profile) Validate() error {
	if p == nil {
		return fmt.Errorf("fingerprint: 档案为空")
	}
	if err := ValidIdentity(p.ProfileID); err != nil {
		return fmt.Errorf("fingerprint: profile_id 非法: %w", err)
	}
	if p.Schema != "" && p.Schema != SchemaV1 {
		return fmt.Errorf("fingerprint: 不支持的 $schema %q（本实现只认 %s）", p.Schema, SchemaV1)
	}
	if p.CreatedAt != "" {
		if _, err := time.Parse(time.RFC3339, p.CreatedAt); err != nil {
			return fmt.Errorf("fingerprint: created_at 不是 RFC3339 时间: %w", err)
		}
	}
	if err := p.validatePlatform(); err != nil {
		return err
	}
	if err := p.validateScreen(); err != nil {
		return err
	}
	if err := p.validateLocale(); err != nil {
		return err
	}
	return p.validateHardware()
}

func (p *Profile) validatePlatform() error {
	ua := strings.TrimSpace(p.Emulation.UserAgent)
	if ua == "" {
		return fmt.Errorf("fingerprint: emulation.user_agent 不能为空")
	}
	if !strings.Contains(ua, "Mozilla/") {
		return fmt.Errorf("fingerprint: emulation.user_agent 不像浏览器 UA: %q", ua)
	}
	md := p.Emulation.UserAgentMetadata
	if md.Platform == "" {
		return fmt.Errorf("fingerprint: emulation.user_agent_metadata.platform 不能为空（否则 Client Hints 会自报空白）")
	}
	// 矛盾检测：UA 字符串里的平台必须与 Client Hints 的 platform 一致。
	if uaOS := osFromUserAgent(ua); uaOS != "" && uaOS != md.Platform {
		return fmt.Errorf(
			"fingerprint: 档案自相矛盾——user_agent 说 %s，而 user_agent_metadata.platform 说 %s",
			uaOS, md.Platform)
	}
	if md.Architecture != "" && !validArchitecture(md.Architecture) {
		return fmt.Errorf("fingerprint: user_agent_metadata.architecture %q 不是 CDP 认的取值（x86/arm/x86_64/arm64）", md.Architecture)
	}
	if md.Mobile && !isMobilePlatform(md.Platform) {
		return fmt.Errorf("fingerprint: user_agent_metadata.platform=%s 不该是 mobile（那是手机平台的事）", md.Platform)
	}
	if uaMobile := isMobileUserAgent(ua); uaMobile != md.Mobile {
		return fmt.Errorf("fingerprint: 档案自相矛盾——user_agent 的 mobile=%t，而 user_agent_metadata.mobile=%t", uaMobile, md.Mobile)
	}
	for _, b := range md.Brands {
		if strings.TrimSpace(b.Brand) == "" || strings.TrimSpace(b.Version) == "" {
			return fmt.Errorf("fingerprint: user_agent_metadata.brands 里有空项")
		}
	}
	return nil
}

func (p *Profile) validateScreen() error {
	s := p.Emulation.Screen
	if s.Width <= 0 || s.Height <= 0 {
		return fmt.Errorf("fingerprint: emulation.screen 必须是正的宽高（拿到 %dx%d）", s.Width, s.Height)
	}
	if s.DeviceScaleFactor <= 0 || math.IsNaN(s.DeviceScaleFactor) || math.IsInf(s.DeviceScaleFactor, 0) {
		return fmt.Errorf("fingerprint: emulation.screen.device_scale_factor 必须为正（拿到 %v）", s.DeviceScaleFactor)
	}
	if s.ColorDepth != 0 && (s.ColorDepth < 8 || s.ColorDepth > 64) {
		return fmt.Errorf("fingerprint: emulation.screen.color_depth %d 不像真值", s.ColorDepth)
	}
	return nil
}

func (p *Profile) validateLocale() error {
	loc := p.Emulation.Locale
	if loc.TimezoneID != "" {
		if _, err := time.LoadLocation(loc.TimezoneID); err != nil {
			return fmt.Errorf("fingerprint: 时区 %q 不是合法 IANA 时区: %w", loc.TimezoneID, err)
		}
	}
	for _, lang := range loc.Languages {
		if strings.TrimSpace(lang) == "" {
			return fmt.Errorf("fingerprint: locale.languages 里有空项")
		}
	}
	if policy := strings.TrimSpace(p.Network.WebRTCPolicy); policy != "" {
		ok := false
		for _, allowed := range webRTCPolicies {
			if policy == allowed {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("fingerprint: network.webrtc_policy %q 不是 Chrome 认的取值（%s）",
				policy, strings.Join(webRTCPolicies, "/"))
		}
	}
	if p.Network.ProxyURL != "" {
		parsed, err := url.Parse(p.Network.ProxyURL)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("fingerprint: network.proxy_url 不是合法代理地址")
		}
		switch parsed.Scheme {
		case "socks5", "socks5h", "socks4", "http", "https":
		default:
			return fmt.Errorf("fingerprint: network.proxy_url 的 scheme %q 不受支持（socks5/socks5h/socks4/http/https）", parsed.Scheme)
		}
		// Chrome 的 --proxy-server 不支持 URL 内嵌用户名/密码：它会把凭据当成主机名的
		// 一部分，代理连接失败，而 Chrome 不会因此降级成直连——真正的危害是「档案里写着
		// 代理、日志里也写着走了代理」，用户以为换了出口其实没有。这里 fail-closed：
		// 带凭据的代理必须先在本机收敛成本地中继（127.0.0.1 转发），档案里只填中继地址。
		// （2026-09-13 评审）
		if parsed.User != nil {
			return fmt.Errorf("fingerprint: network.proxy_url 不能内嵌用户名/密码（Chrome --proxy-server 不支持）。请在本机起一个带凭据的本地中继（例如 127.0.0.1 转发），档案里只填中继地址")
		}
	}
	return nil
}

func (p *Profile) validateHardware() error {
	h := p.Emulation.Hardware
	if h.HardwareConcurrency < 0 || h.HardwareConcurrency > 256 {
		return fmt.Errorf("fingerprint: emulation.hardware.hardware_concurrency 必须在 0..256（拿到 %d）", h.HardwareConcurrency)
	}
	if h.DeviceMemory < 0 || math.IsNaN(h.DeviceMemory) || math.IsInf(h.DeviceMemory, 0) {
		return fmt.Errorf("fingerprint: emulation.hardware.device_memory 必须为非负有限数（拿到 %v）", h.DeviceMemory)
	}
	if (strings.TrimSpace(h.WebGL.Vendor) == "") != (strings.TrimSpace(h.WebGL.Renderer) == "") {
		return fmt.Errorf("fingerprint: emulation.hardware.webgl.vendor 与 renderer 必须同时配置或同时留空")
	}
	return nil
}

// PrimaryLanguage 是主语言（Accept-Language 的第一项），没有就返回空串。
func (p *Profile) PrimaryLanguage() string {
	for _, lang := range p.Emulation.Locale.Languages {
		if trimmed := strings.TrimSpace(lang); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ChromeArgs 是这份档案必须在**启动时**就带上、连上 CDP 之后再改已经晚了的那部分。
//
// `--disable-blink-features=AutomationControlled` 是自动化标志擦除（L3）；语言要在
// 启动期给，否则 `navigator.language` 与 UA-CH 的语言会在第一帧就露馅；代理同理。
// 注意：附着模式（接管用户已有 Chrome）用不上这些——那时进程不是我们起的。
func (p *Profile) ChromeArgs() []string {
	if p == nil {
		return nil
	}
	args := []string{}
	if p.PrimaryLanguage() != "" {
		args = append(args, "--lang="+p.PrimaryLanguage())
	}
	args = append(args, "--disable-blink-features=AutomationControlled")
	if p.Network.ProxyURL != "" {
		if arg := chromeProxyArg(p.Network.ProxyURL); arg != "" {
			args = append(args, "--proxy-server="+arg)
		}
	}
	if policy := p.EffectiveWebRTCPolicy(); policy != "" {
		args = append(args, "--force-webrtc-ip-handling-policy="+policy)
	}
	return args
}

// EffectiveWebRTCPolicy 返回这份档案要下发的 WebRTC 策略。
// 规则：
//  1. 若档案显式指定了 webrtc_policy，优先使用显式值；
//  2. 若未显式指定但配置了代理（ProxyURL != ""），默认锁定为 disable_non_proxied_udp
//     （在 Profile Preferences 与启动参数中封死非代理 UDP，杜绝真实宽带 IP 泄漏）；
//  3. 无代理且无策略时返回空（不伪装、不打扰直连正常 WebRTC）。
func (p *Profile) EffectiveWebRTCPolicy() string {
	if p == nil {
		return ""
	}
	if policy := strings.TrimSpace(p.Network.WebRTCPolicy); policy != "" {
		return policy
	}
	if strings.TrimSpace(p.Network.ProxyURL) != "" {
		return WebRTCPolicyDisableNonProxied
	}
	return ""
}

// WebRTCLeakWarning 返回关于 WebRTC 的运行提示。
func (p *Profile) WebRTCLeakWarning() string {
	if p == nil || strings.TrimSpace(p.Network.ProxyURL) == "" {
		return ""
	}
	policy := p.EffectiveWebRTCPolicy()
	if policy == WebRTCPolicyDisableNonProxied {
		return "WebRTC 策略已锁定为 disable_non_proxied_udp：非代理 UDP 已被封死，真实宽带 IP 不会泄漏（音视频需走中继）。"
	}
	return "⚠️ 档案配置了代理但 WebRTC 策略放行了非代理 UDP（" + policy + "）：页面可能通过 STUN 拿到真实宽带出口，与代理出口不一致（BrowserLeaks 会标出）。"
}

// chromeProxyArg 把档案里的代理地址换成 Chrome 认的形式。
//
// socks5h 是 curl 一类工具的「让代理解析域名」写法，Chrome 的 --proxy-server 只认
// socks5://；Chromium 的 SOCKS5 客户端本来就把域名交给代理（ATYP=domain），不在本地
// 先解析，所以这里直接归一化，不引入第二种传输、也不假装能配置 DNS 行为。
// 用户名/密码一律丢弃：Validate 已经拒绝带凭据的地址，这里再兜一层，保证「日志里说走了
// 代理、实际却没走」不会从启动参数这条缝里溜过去。（2026-09-13 评审）
func chromeProxyArg(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme == "socks5h" {
		parsed.Scheme = "socks5"
	}
	parsed.User = nil
	return parsed.String()
}

// RedactProxyURL 只用于展示/日志：保留代理类型与地址，抹掉可能包含在 URL 中的
// 用户名/密码。真正传给 Chrome 的启动参数仍由 ChromeArgs 原样生成。
func RedactProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = url.User("REDACTED")
	return parsed.String()
}

// Overrides 是这份档案在 CDP 侧要落地的覆盖。
func (p *Profile) Overrides() Overrides {
	if p == nil {
		return Overrides{}
	}
	o := Overrides{
		UserAgent: p.Emulation.UserAgent,
		Timezone:  p.Emulation.Locale.TimezoneID,
		Locale:    p.PrimaryLanguage(),
	}
	if languages := p.Emulation.Locale.Languages; len(languages) > 0 {
		o.AcceptLanguage = strings.Join(languages, ",")
	}
	if p.Emulation.UserAgentMetadata.Platform != "" {
		metadata := p.Emulation.UserAgentMetadata
		o.UserAgentMetadata = &metadata
	}
	if p.Emulation.Screen.Width > 0 && p.Emulation.Screen.Height > 0 {
		screen := p.Emulation.Screen
		o.Screen = &screen
	}
	return o
}

// MatchesChromeVersion 判断这份档案瞄准的 Chrome 大版本是否就是手上这只浏览器。
//
// 不匹配不等于立刻失败（用户可能就是要先看看效果），但它意味着 UA/Client Hints 与
// 真实内核版本对不上——那是能被抓的矛盾，调用方**必须**把差异大声记进日志。
func (p *Profile) MatchesChromeVersion(actual string) bool {
	want := majorVersion(p.TargetPlatform.ChromeVersion)
	if want == "" {
		return true // 档案没锁版本：无从对账，不算不匹配
	}
	return want == majorVersion(actual)
}

// MissingNoiseFeatures 列出档案点名要、但尚未落地的能力（诚实记账）。
// 在 Phase 3 中，canvas_2d、audio_context、webgl_buffer 均已通过原型链 Hook 闭环落地。
func (p *Profile) MissingNoiseFeatures() []string {
	if p == nil {
		return nil
	}
	missing := []string{}
	// Phase 3 已落地 canvas_2d、audio_context、webgl_buffer。
	// 若未来新增未实现的噪声特征项，可在此追加检查。
	sort.Strings(missing)
	return missing
}

// ValidIdentity 校验身份名是否可用作目录名/键：`[A-Za-z0-9._-]`，1..64，且不能是
// "."/".."/以 "." 开头。这是**唯一**的判定处（`pkg/browser` 建目录时也调这里），
// 因为它将来会从配置或网络下发，路径拼接前必须挡住逃逸。
func ValidIdentity(id string) error {
	if id == "" {
		return fmt.Errorf("身份名为空")
	}
	if len(id) > 64 {
		return fmt.Errorf("身份名过长（%d > 64）", len(id))
	}
	if id == "." || id == ".." || strings.HasPrefix(id, ".") {
		return fmt.Errorf("身份名 %q 不是合法身份名", id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("身份名 %q 含非法字符 %q", id, r)
		}
	}
	return nil
}

// osFromUserAgent 从 UA 字符串里认出平台名（与 Client Hints 的 platform 同口径）。
func osFromUserAgent(ua string) string {
	switch {
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		return "macOS"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "CrOS"):
		return "Chrome OS"
	case strings.Contains(ua, "Linux"), strings.Contains(ua, "X11"):
		return "Linux"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		return "iOS"
	}
	return ""
}

func isMobilePlatform(platform string) bool {
	switch platform {
	case "Android", "iOS":
		return true
	}
	return false
}

func isMobileUserAgent(ua string) bool {
	return strings.Contains(ua, "Android") || strings.Contains(ua, "iPhone") ||
		strings.Contains(ua, "iPad") || strings.Contains(ua, "Mobile")
}

func validArchitecture(architecture string) bool {
	switch architecture {
	case "x86", "arm", "x86_64", "arm64":
		return true
	}
	return false
}

// majorVersion 取首个数字段：`152.0.7933.0`、`Chrome/152.0.7933.0`、`152` 都得到 `152`
// ——CDP `Browser.getVersion` 给的是带产品名的写法，不能按 `.` 切第一段就算了。
func majorVersion(version string) string {
	i := strings.IndexAny(version, "0123456789")
	if i < 0 {
		return ""
	}
	rest := version[i:]
	if j := strings.IndexAny(rest, ".:"); j >= 0 {
		return rest[:j]
	}
	return rest
}
