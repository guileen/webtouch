package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"uws/pkg/fingerprint"
)

// e2eProfileJSON 故意不带代理：带代理的话所有流量都会走它，本地 httptest 就打不开了。
// 代理那条线的证据是「启动参数里必须有 --proxy-server」，由 pkg/fingerprint 的单测守。
const e2eProfileJSON = `{
  "profile_id": "fp_e2e_mac",
  "target_platform": {"os": "macOS", "os_version": "14.5.0", "architecture": "arm64", "chrome_version": ""},
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
  "network": {}
}`

// TestFingerprintProfileIsAppliedToARealChrome 是 Phase 2 的验收判据：档案里写的每一项，
// 都要在真 Chrome 自报的身份里对得上（CreepJS / bot.sannysoft 的外网站验收是后续的真机项，
// 这里先用本机页面把「档案 → 浏览器」这条链钉死）。
func TestFingerprintProfileIsAppliedToARealChrome(t *testing.T) {
	requireCDP(t)

	profile, err := fingerprint.Parse([]byte(e2eProfileJSON))
	if err != nil {
		t.Fatalf("档案解析失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := (&CDPFactory{Headless: true}).Create(ctx, Config{
		TaskID:      "fingerprint-e2e",
		Fingerprint: profile,
		InitialURL:  "about:blank",
	})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	defer func() { _ = session.Close() }()

	// 1) 启动参数：只有启动期能定下的那些（自动化标志擦除、语言）必须已经在命令行上。
	raw := session.(*cdpSession)
	if raw.chromeCmd == nil {
		t.Fatal("启动模式应当有 chromeCmd")
	}
	args := strings.Join(raw.chromeCmd.Args, " ")
	for _, want := range []string{"--disable-blink-features=AutomationControlled", "--lang=zh-CN"} {
		if !strings.Contains(args, want) {
			t.Errorf("启动参数缺少 %q；实际：%s", want, args)
		}
	}

	// 2) 页面自报的身份：用 127.0.0.1 起一张本地页（安全上下文，UA-CH 才可用）。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><title>fingerprint probe</title>ok"))
	}))
	defer server.Close()
	if err := session.Navigate(ctx, server.URL); err != nil {
		t.Fatalf("导航失败: %v", err)
	}

	const expr = `JSON.stringify({
		ua: navigator.userAgent,
		webdriver: navigator.webdriver,
		platform: (navigator.userAgentData || {}).platform || "",
		brands: ((navigator.userAgentData || {}).brands || []).map(function(b){return b.brand + ":" + b.version}),
		tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
		lang: navigator.language,
		langs: navigator.languages,
		sw: screen.width,
		sh: screen.height,
		dpr: devicePixelRatio,
		w: window.innerWidth,
		h: window.innerHeight
	})`
	var payload string
	if err := raw.evalValue(ctx, expr, &payload); err != nil {
		t.Fatalf("读取页面自报身份失败: %v", err)
	}
	var got struct {
		UA        string   `json:"ua"`
		Webdriver bool     `json:"webdriver"`
		Platform  string   `json:"platform"`
		Brands    []string `json:"brands"`
		TZ        string   `json:"tz"`
		Lang      string   `json:"lang"`
		Langs     []string `json:"langs"`
		SW        int      `json:"sw"`
		SH        int      `json:"sh"`
		DPR       float64  `json:"dpr"`
		W         int      `json:"w"`
		H         int      `json:"h"`
	}
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("解析页面自报身份失败: %v（原始 %s）", err, payload)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"navigator.userAgent", got.UA, profile.Emulation.UserAgent},
		{"navigator.webdriver", got.Webdriver, false},
		{"navigator.userAgentData.platform", got.Platform, profile.Emulation.UserAgentMetadata.Platform},
		{"Intl 时区", got.TZ, profile.Emulation.Locale.TimezoneID},
		{"navigator.language", got.Lang, profile.PrimaryLanguage()},
		{"screen.width", got.SW, profile.Emulation.Screen.Width},
		{"screen.height", got.SH, profile.Emulation.Screen.Height},
		{"devicePixelRatio", got.DPR, profile.Emulation.Screen.DeviceScaleFactor},
		{"window.innerWidth", got.W, profile.Emulation.Screen.Width},
		{"window.innerHeight", got.H, profile.Emulation.Screen.Height},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s = %v，档案里写的是 %v", check.name, check.got, check.want)
		}
	}
	if len(got.Langs) == 0 || got.Langs[0] != "zh-CN" {
		t.Errorf("navigator.languages = %v，档案里写的是 %v", got.Langs, profile.Emulation.Locale.Languages)
	}
	// Client Hints 的品牌必须来自档案，而不是浏览器自己的真实品牌串。
	if len(got.Brands) == 0 || !strings.Contains(strings.Join(got.Brands, ","), "Chromium:152") {
		t.Errorf("navigator.userAgentData.brands = %v，没带上档案里的品牌串", got.Brands)
	}
}
