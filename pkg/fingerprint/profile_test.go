package fingerprint

import (
	"strings"
	"testing"
)

// validProfileJSON 是规格 §2.1 里那份示例档案（Phase 2 的基准）。
const validProfileJSON = `{
  "$schema": "https://webtouch.dev/schemas/fingerprint-profile.v1.json",
  "profile_id": "fp_dev_mac_01",
  "version": "1.0.0",
  "created_at": "2026-09-12T14:55:00Z",
  "seed": 894120571,
  "target_platform": {
    "os": "macOS",
    "os_version": "14.5.0",
    "architecture": "arm64",
    "chrome_version": "152.0.7933.0"
  },
  "emulation": {
    "user_agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
    "user_agent_metadata": {
      "brands": [{"brand": "Chromium", "version": "152"}, {"brand": "Google Chrome", "version": "152"}],
      "fullVersion": "152.0.7933.0",
      "platform": "macOS",
      "platformVersion": "14.5.0",
      "architecture": "arm",
      "model": "",
      "mobile": false
    },
    "screen": {"width": 1440, "height": 900, "device_scale_factor": 2.0, "color_depth": 24},
    "locale": {"timezone_id": "Asia/Shanghai", "languages": ["zh-CN", "zh", "en"]},
    "hardware": {"hardware_concurrency": 8, "device_memory": 8}
  },
  "noise_features": {"canvas_2d": true, "audio_context": false, "webgl_buffer": false},
  "network": {"proxy_url": "socks5://127.0.0.1:10808"}
}`

func mustParse(t *testing.T, data string) *Profile {
	t.Helper()
	p, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("解析合法档案失败: %v", err)
	}
	return p
}

func TestParseAcceptsTheSpecExample(t *testing.T) {
	p := mustParse(t, validProfileJSON)
	if p.ProfileID != "fp_dev_mac_01" {
		t.Fatalf("profile_id 不对: %q", p.ProfileID)
	}
	if p.Seed != 894120571 {
		t.Fatalf("seed 丢失: %d", p.Seed)
	}
	if p.Emulation.Screen.Width != 1440 || p.Emulation.Screen.DeviceScaleFactor != 2.0 {
		t.Fatalf("screen 解析不对: %+v", p.Emulation.Screen)
	}
	if p.Network.ProxyURL != "socks5://127.0.0.1:10808" {
		t.Fatalf("proxy 解析不对: %q", p.Network.ProxyURL)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	// 手写档案里的拼写错误必须当场炸，而不是悄悄按默认值跑出一个「看起来像但不对」的身份。
	typo := strings.Replace(validProfileJSON, `"user_agent":`, `"user_agnet":`, 1)
	if _, err := Parse([]byte(typo)); err == nil {
		t.Fatal("未知字段（拼写错误）应当被拒绝")
	}
}

func TestParseRejectsTrailingJSON(t *testing.T) {
	if _, err := Parse([]byte(validProfileJSON + `{"extra":true}`)); err == nil {
		t.Fatal("合法档案后追加第二个 JSON 文档应当被拒绝")
	}
}

func TestValidateCatchesTheClassicContradiction(t *testing.T) {
	// UA 说 macOS、Client Hints 说 Windows —— 这类自相矛盾比不伪装还容易被抓。
	contradiction := strings.Replace(validProfileJSON, `"platform": "macOS"`, `"platform": "Windows"`, 1)
	_, err := Parse([]byte(contradiction))
	if err == nil {
		t.Fatal("UA 与 Client Hints 平台矛盾应当被拒绝")
	}
	if !strings.Contains(err.Error(), "自相矛盾") {
		t.Fatalf("错误信息没说清是矛盾: %v", err)
	}
}

func TestValidateCatchesMobileUserAgentMismatch(t *testing.T) {
	mobile := strings.Replace(validProfileJSON,
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Mobile Safari/537.36", 1)
	mobile = strings.Replace(mobile, `"platform": "macOS"`, `"platform": "Android"`, 1)
	if _, err := Parse([]byte(mobile)); err == nil || !strings.Contains(err.Error(), "mobile") {
		t.Fatalf("UA 与 mobile=false 矛盾应当被拒绝，got %v", err)
	}
}

func TestValidateRejectsBrokenFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"空身份名", func(s string) string { return strings.Replace(s, `"fp_dev_mac_01"`, `""`, 1) }, "profile_id"},
		{"非法身份名", func(s string) string { return strings.Replace(s, `"fp_dev_mac_01"`, `"../etc"`, 1) }, "profile_id"},
		{"空 UA", func(s string) string {
			return strings.Replace(s, `"user_agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"`, `"user_agent": ""`, 1)
		}, "user_agent"},
		{"假时区", func(s string) string { return strings.Replace(s, `"Asia/Shanghai"`, `"Mars/Olympus"`, 1) }, "时区"},
		{"零尺寸屏幕", func(s string) string { return strings.Replace(s, `"width": 1440`, `"width": 0`, 1) }, "screen"},
		{"桌面平台却自称手机", func(s string) string { return strings.Replace(s, `"mobile": false`, `"mobile": true`, 1) }, "mobile"},
		{"坏代理", func(s string) string { return strings.Replace(s, `socks5://127.0.0.1:10808`, `ftp://nope`, 1) }, "proxy_url"},
		{"不支持的内核架构", func(s string) string {
			return strings.Replace(s, `"architecture": "arm"`, `"architecture": "sparc"`, 1)
		}, "architecture"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mutate(validProfileJSON)))
			if err == nil {
				t.Fatalf("%s 应当被拒绝", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息里没提到 %q: %v", tc.want, err)
			}
		})
	}
}

func TestChromeArgsCarryWhatOnlyLaunchTimeCanCarry(t *testing.T) {
	args := strings.Join(mustParse(t, validProfileJSON).ChromeArgs(), " ")
	for _, want := range []string{"--lang=zh-CN", "--disable-blink-features=AutomationControlled", "--proxy-server=socks5://127.0.0.1:10808"} {
		if !strings.Contains(args, want) {
			t.Errorf("启动参数缺少 %q（实际 %q）", want, args)
		}
	}
}

func TestRedactProxyURLHidesCredentials(t *testing.T) {
	got := RedactProxyURL("socks5://alice:secret@example.test:1080")
	if got != "socks5://REDACTED@example.test:1080" {
		t.Fatalf("代理展示未正确脱敏: %q", got)
	}
	if strings.Contains(got, "alice") || strings.Contains(got, "secret") {
		t.Fatalf("代理凭据泄露: %q", got)
	}
	bad := strings.Replace(validProfileJSON, "socks5://127.0.0.1:10808", "socks5://alice:%zz@example.test", 1)
	if _, err := Parse([]byte(bad)); err == nil || strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("坏代理的错误信息不应带凭据，got %v", err)
	}
}

func TestOverridesCarryTheCDPSideOfTheIdentity(t *testing.T) {
	o := mustParse(t, validProfileJSON).Overrides()
	if !strings.Contains(o.UserAgent, "Macintosh") {
		t.Fatalf("UA 没带上: %q", o.UserAgent)
	}
	if o.AcceptLanguage != "zh-CN,zh,en" {
		t.Fatalf("Accept-Language 不对: %q", o.AcceptLanguage)
	}
	if o.Locale != "zh-CN" || o.Timezone != "Asia/Shanghai" {
		t.Fatalf("语言/时区不对: %q %q", o.Locale, o.Timezone)
	}
	if o.UserAgentMetadata == nil || o.UserAgentMetadata.Platform != "macOS" {
		t.Fatalf("Client Hints 没带上: %+v", o.UserAgentMetadata)
	}
	if o.Screen == nil || o.Screen.Width != 1440 || o.Screen.Height != 900 {
		t.Fatalf("屏幕没带上: %+v", o.Screen)
	}
	// 覆盖是拷贝出来的：调用方改它不该动到档案本体。
	o.Screen.Width = 1
	if mustParse(t, validProfileJSON).Emulation.Screen.Width != 1440 {
		t.Fatal("Overrides 返回了档案内部结构的引用")
	}
}

func TestMatchesChromeVersionComparesMajorVersions(t *testing.T) {
	p := mustParse(t, validProfileJSON)
	if !p.MatchesChromeVersion("Chrome/152.0.7933.0") {
		t.Fatal("同一大版本应当算匹配")
	}
	if !p.MatchesChromeVersion("152.0.7933.0") {
		t.Fatal("不带产品名的写法也应当算匹配")
	}
	if p.MatchesChromeVersion("Chrome/128.0.6613.119") {
		t.Fatal("不同大版本应当算不匹配")
	}
	noPin := mustParse(t, strings.Replace(validProfileJSON, `"chrome_version": "152.0.7933.0"`, `"chrome_version": ""`, 1))
	if !noPin.MatchesChromeVersion("Chrome/128.0.0.0") {
		t.Fatal("档案没锁版本时不算不匹配")
	}
}

func TestMissingNoiseFeaturesIsHonestAccounting(t *testing.T) {
	// Phase 3 中 canvas_2d, audio_context, webgl_buffer 均已实现，不再报 missing。
	missing := mustParse(t, validProfileJSON).MissingNoiseFeatures()
	if len(missing) != 0 {
		t.Fatalf("Phase 3 已经支持 canvas_2d，不应再报 missing，实际 %v", missing)
	}
}

func TestValidIdentity(t *testing.T) {
	for _, id := range []string{"fp_dev_mac_01", "a", "A.b-c_1"} {
		if err := ValidIdentity(id); err != nil {
			t.Errorf("合法身份名 %q 被拒: %v", id, err)
		}
	}
	for _, id := range []string{"", ".", "..", ".hidden", "a/b", `a\b`, "身份", strings.Repeat("x", 65)} {
		if err := ValidIdentity(id); err == nil {
			t.Errorf("非法身份名 %q 应当被拒", id)
		}
	}
}

func TestValidateRejectsProxyCredentials(t *testing.T) {
	// Chrome 的 --proxy-server 不支持 URL 内嵌用户名/密码；放行它等于「档案里写着代理、
	// 实际却没走代理」，所以档案层 fail-closed（2026-09-13 评审）。
	bad := strings.Replace(validProfileJSON, "socks5://127.0.0.1:10808", "socks5://user:pass@127.0.0.1:10808", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "用户名/密码") {
		t.Fatalf("带凭据的代理地址应当被拒绝，got %v", err)
	}
}

func TestSocks5hIsAcceptedAndNormalizedForChrome(t *testing.T) {
	h := strings.Replace(validProfileJSON, "socks5://127.0.0.1:10808", "socks5h://127.0.0.1:10808", 1)
	args := strings.Join(mustParse(t, h).ChromeArgs(), " ")

	if !strings.Contains(args, "--proxy-server=socks5://127.0.0.1:10808") {
		t.Fatalf("socks5h 应当归一化成 Chrome 认的 socks5://，实际 %q", args)
	}
	if strings.Contains(args, "socks5h") {
		t.Fatalf("Chrome 不认 socks5h://，不该原样下发: %q", args)
	}
}

func TestProxyAloneEnforcesDisableNonProxiedUDP(t *testing.T) {
	// 当档案配置了代理时，默认下发 disable_non_proxied_udp：
	// 启动参数与 Profile Preferences 双保险切断非代理 UDP，杜绝公网真实 IP 泄漏。
	p := mustParse(t, validProfileJSON) // 带 socks5://127.0.0.1:10808
	if got := p.EffectiveWebRTCPolicy(); got != WebRTCPolicyDisableNonProxied {
		t.Fatalf("绑代理时默认 WebRTC 策略应为 %q，实际 %q", WebRTCPolicyDisableNonProxied, got)
	}
	if args := strings.Join(p.ChromeArgs(), " "); !strings.Contains(args, "--force-webrtc-ip-handling-policy="+WebRTCPolicyDisableNonProxied) {
		t.Fatalf("启动参数未包含双保险 WebRTC 策略: %q", args)
	}
	if warn := p.WebRTCLeakWarning(); !strings.Contains(warn, "disable_non_proxied_udp") || !strings.Contains(warn, "不会泄漏") {
		t.Fatalf("提示应当说明非代理 UDP 已封堵，实际 %q", warn)
	}
}

func TestWebRTCPolicyIsExplicitAndForwardedVerbatim(t *testing.T) {
	explicit := strings.Replace(validProfileJSON, `"proxy_url"`, `"webrtc_policy": "default_public_interface_only", "proxy_url"`, 1)
	p := mustParse(t, explicit)
	if got := p.EffectiveWebRTCPolicy(); got != WebRTCPolicyPublicInterfaceOnly {
		t.Fatalf("显式策略应当原样返回，实际 %q", got)
	}
	if args := strings.Join(p.ChromeArgs(), " "); !strings.Contains(args, "--force-webrtc-ip-handling-policy="+WebRTCPolicyPublicInterfaceOnly) {
		t.Fatalf("显式策略没落到启动参数: %q", args)
	}

	// 没代理也没策略：不动 WebRTC（不伪装、也不打断真实使用），也没有告警可念。
	bare := strings.Replace(validProfileJSON, `"proxy_url": "socks5://127.0.0.1:10808"`, `"proxy_url": ""`, 1)
	bp := mustParse(t, bare)
	if got := bp.EffectiveWebRTCPolicy(); got != "" {
		t.Fatalf("没代理没策略时不该下发 WebRTC 策略，实际 %q", got)
	}
	if args := strings.Join(bp.ChromeArgs(), " "); strings.Contains(args, "--force-webrtc-ip-handling-policy") {
		t.Fatalf("没代理没策略时不该出现 WebRTC 参数: %q", args)
	}
	if warn := bp.WebRTCLeakWarning(); warn != "" {
		t.Fatalf("没绑代理时不该有 WebRTC 告警，实际 %q", warn)
	}
}

func TestInvalidWebRTCPolicyIsRejected(t *testing.T) {
	bad := strings.Replace(validProfileJSON, `"proxy_url"`, `"webrtc_policy": "block-everything", "proxy_url"`, 1)
	if _, err := Parse([]byte(bad)); err == nil || !strings.Contains(err.Error(), "webrtc_policy") {
		t.Fatalf("非法 webrtc_policy 应当被拒，got %v", err)
	}
}
