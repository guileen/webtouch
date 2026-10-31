package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"uws/pkg/fingerprint"
)

// ── 会话执行 ────────────────────────────────────────────────────────

type auditRunConfig struct {
	session   string
	headless  bool
	outDir    string
	repoRoot  string
	sites     []string
	profile   *fingerprint.Profile
	profileID string
	selfURL   string
}

func auditRunSession(ctx context.Context, t *testing.T, cfg auditRunConfig) *auditRun {
	t.Helper()
	factory := &CDPFactory{Headless: cfg.headless, ExtraArgs: auditExtraArgs()}
	session, err := factory.Create(ctx, Config{
		TaskID:      "fingerprint-audit-" + cfg.session,
		Fingerprint: cfg.profile,
		ProfileID:   cfg.profileID,
		InitialURL:  cfg.selfURL,
	})
	if err != nil {
		t.Fatalf("[%s] 创建会话失败: %v", cfg.session, err)
	}
	defer func() { _ = session.Close() }()
	raw, ok := session.(*cdpSession)
	if !ok {
		t.Fatalf("[%s] 会话不是 *cdpSession", cfg.session)
	}

	if cfg.selfURL != "" {
		// 无头启动只会开 about:blank（InitialURL 只用于有头 app 窗口的 URL），而 UA-CH
		// 这类 API 只在**安全上下文**里存在——自测面必须显式导航到 127.0.0.1 本地页。
		if err := session.Navigate(ctx, cfg.selfURL); err != nil {
			t.Fatalf("[%s] 导航本地自测页失败: %v", cfg.session, err)
		}
	}

	run := &auditRun{Session: cfg.session, Self: map[string]any{}, BrowserBuild: auditBrowserBuild(ctx, raw)}

	// 自测面：先把「页面自己报的身份」原样抓下来（本地安全上下文页，不走外网）。
	if err := auditEvalJSON(ctx, raw, auditSelfScript, &run.Self); err != nil {
		t.Fatalf("[%s] 采集自报身份失败: %v", cfg.session, err)
	}
	if shot, _, _, err := raw.CaptureScreenshot(ctx); err == nil {
		name := fmt.Sprintf("%02d-%s-self-report.png", 0, cfg.session)
		if err := os.WriteFile(filepath.Join(cfg.outDir, name), shot, 0o644); err == nil {
			run.ShotSelfCheck = name
		}
	}

	for _, name := range cfg.sites {
		site, ok := auditSites[name]
		if !ok {
			t.Fatalf("未知站点 %q", name)
		}
		res := auditVisit(ctx, raw, site, cfg.outDir, cfg.session)
		run.Sites = append(run.Sites, res)
		switch name {
		case "egress":
			run.EgressIP = auditExtractIP(res)
		case "browserleaks":
			run.WebRTCIP = auditExtractWebRTCIP(res)
		}
	}
	return run
}

func auditVisit(ctx context.Context, s *cdpSession, site auditSite, outDir, tag string) auditSiteResult {
	res := auditSiteResult{Name: site.name, URL: site.url}
	started := time.Now()
	if err := s.Navigate(ctx, site.url); err != nil {
		// 头一次外网导航偶发 unreachable（DNS/预连接还没热），隔 2 秒重试一次：
		// 一次抖动不该把整站记成「采集失败」。
		time.Sleep(2 * time.Second)
		if err2 := s.Navigate(ctx, site.url); err2 != nil {
			res.Err = "导航失败（重试后仍失败）: " + err2.Error() + "（首次: " + err.Error() + "）"
			res.WaitedMS = time.Since(started).Milliseconds()
			return res
		}
	}
	deadline := time.Now().Add(site.waitMax)
	for time.Now().Before(deadline) {
		if site.waitFor != "" {
			if ok, err := s.evalBool(ctx, site.waitFor); err == nil && ok {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if site.settle > 0 {
		time.Sleep(site.settle)
	}
	res.WaitedMS = time.Since(started).Milliseconds()

	payload, err := s.evalResult(ctx, site.script)
	if err != nil {
		res.Err = "采集脚本失败: " + err.Error()
	} else if strings.HasPrefix(strings.TrimSpace(payload), "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			res.Err = "采集结果不是合法 JSON: " + err.Error()
		} else {
			res.Raw = obj
			if title, ok := obj["title"].(string); ok {
				res.Title = title
			}
		}
	}
	if shot, _, _, err := s.CaptureScreenshot(ctx); err == nil {
		name := fmt.Sprintf("%02d-%s-%s.png", auditSiteOrder(site.name), tag, site.name)
		if err := os.WriteFile(filepath.Join(outDir, name), shot, 0o644); err == nil {
			res.Shot = name
		}
	}
	return res
}

func auditSiteOrder(name string) int {
	switch name {
	case "egress":
		return 1
	case "sannysoft":
		return 2
	case "creepjs":
		return 3
	case "browserleaks":
		return 4
	default:
		return 9
	}
}

// ── 档案解析与派生 ──────────────────────────────────────────────────

func auditResolveProfile(baseline *auditRun) (*fingerprint.Profile, string, error) {
	if path := strings.TrimSpace(os.Getenv("UWS_AUDIT_PROFILE")); path != "" {
		p, err := fingerprint.Load(path)
		if err != nil {
			return nil, "", err
		}
		return p, "文件 " + path, nil
	}
	if raw := strings.TrimSpace(os.Getenv("UWS_AUDIT_PROFILE_JSON")); raw != "" {
		// 直接喂一段档案 JSON（调试/对照实验用）。解析失败必须当场炸——静默回落到镜像档
		// 会让人以为「策略生效了」，其实跑的是另一份身份。
		p, err := fingerprint.Parse([]byte(raw))
		if err != nil {
			return nil, "", fmt.Errorf("UWS_AUDIT_PROFILE_JSON 解析失败: %w", err)
		}
		return p, "UWS_AUDIT_PROFILE_JSON", nil
	}
	if id := strings.TrimSpace(os.Getenv("UWS_AUDIT_PROFILE_ID")); id != "" {
		reg, err := fingerprint.OpenRegistry("")
		if err != nil {
			return nil, "", err
		}
		p, err := reg.Load(id)
		if err != nil {
			return nil, "", err
		}
		return p, "身份注册表 " + reg.Root + "/" + id, nil
	}
	return auditMirrorProfile(baseline), "镜像派生（基线真值 + 指定屏幕）", nil
}

// auditMirrorProfile 从基线观测派生一份「本机自洽」的档案：同一只 Chrome 的版本与品牌串、
// 同一套语言/时区、UA 去掉 HeadlessChrome 标记，屏幕按 UWS_AUDIT_SCREEN 指定。
//
// 这样派生的意义：档案**不该**与手上这只浏览器硬碰硬（版本对不上、语言对不上都是破绽），
// 它该做的是「换一个设备事实（屏幕），其余如实；Phase 3 注入确定性渲染噪声与硬件隐蔽」。
func auditMirrorProfile(baseline *auditRun) *fingerprint.Profile {
	self := baseline.Self
	ua := auditStr(auditPick(self, "ua"))
	ua = strings.Replace(ua, "HeadlessChrome/", "Chrome/", 1)

	platform := auditStr(auditPick(self, "uaDataPlatform"))
	if platform == "" {
		// UA-CH 缺失时的兜底（例如非安全上下文）：从 UA 里认平台，别造出空 platform
		// ——空 platform 会被 Validate 当场拒掉（那正是它该做的事）。
		if strings.Contains(ua, "Macintosh") {
			platform = "macOS"
		} else if strings.Contains(ua, "Windows") {
			platform = "Windows"
		} else {
			platform = "Linux"
		}
	}
	brands := auditStr(auditPick(self, "uaDataBrands"))
	full := ""
	if high, ok := auditPick(self, "uaDataHigh").(map[string]any); ok {
		full = auditStr(high["uaFullVersion"])
	}
	platformVersion := ""
	architecture := ""
	if high, ok := auditPick(self, "uaDataHigh").(map[string]any); ok {
		platformVersion = auditStr(high["platformVersion"])
		architecture = auditStr(high["architecture"])
	}
	if architecture == "" {
		architecture = auditArchFallback()
	}
	// 品牌串**照抄基线**：GREASE 品牌（Not?A_Brand/24 这类）的版本号是浏览器构建常量，
	// 手改成大版本号就是一处可被测出的穿帮（本次验收实测：真 Chrome 152 报的是
	// `Not?A_Brand:24`，而按大版本号改会变成 `:152`）。
	brandList := auditParseBrands(brands)
	if len(brandList) == 0 {
		major := auditMajor(full)
		if major == "" {
			major = auditMajor(ua)
		}
		brandList = []fingerprint.Brand{{Brand: "Chromium", Version: major}, {Brand: "Google Chrome", Version: major}}
	}

	langs := auditStr(auditPick(self, "languages"))
	languageList := []string{}
	for _, l := range strings.Split(langs, ",") {
		if l = strings.TrimSpace(l); l != "" {
			languageList = append(languageList, l)
		}
	}
	width, height, dpr := auditParseScreen(os.Getenv("UWS_AUDIT_SCREEN"))

	profile := &fingerprint.Profile{
		ProfileID: "fp_audit_mirror",
		TargetPlatform: fingerprint.TargetPlatform{
			OS:            "macOS",
			OSVersion:     platformVersion,
			Architecture:  architecture,
			ChromeVersion: full,
		},
		Emulation: fingerprint.Emulation{
			UserAgent: ua,
			UserAgentMetadata: fingerprint.UserAgentMetadata{
				Brands:          brandList,
				FullVersion:     full,
				Platform:        platform,
				PlatformVersion: platformVersion,
				Architecture:    architecture,
				Mobile:          false,
			},
			Screen: fingerprint.Screen{Width: width, Height: height, DeviceScaleFactor: dpr, ColorDepth: 24},
			Locale: fingerprint.Locale{
				TimezoneID: auditStr(auditPick(self, "tz")),
				Languages:  languageList,
			},
		},
		NoiseFeatures: fingerprint.NoiseFeatures{
			Canvas2D:     true,
			AudioContext: true,
			WebGLBuffer:  true,
		},
		Seed: 20260913,
	}
	return profile
}

func auditParseBrands(s string) []fingerprint.Brand {
	out := []fingerprint.Brand{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.LastIndex(part, ":")
		if idx <= 0 {
			continue
		}
		out = append(out, fingerprint.Brand{Brand: part[:idx], Version: part[idx+1:]})
	}
	return out
}

func auditParseScreen(v string) (int, int, float64) {
	width, height, dpr := 1440, 900, 2.0
	v = strings.TrimSpace(v)
	if v == "" {
		return width, height, dpr
	}
	scale := 1.0
	if idx := strings.Index(v, "@"); idx > 0 {
		if f, err := strconv.ParseFloat(v[idx+1:], 64); err == nil && f > 0 {
			scale = f
		}
		v = v[:idx]
	}
	parts := strings.SplitN(v, "x", 2)
	if len(parts) == 2 {
		if w, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil && w > 0 {
			width = w
		}
		if h, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil && h > 0 {
			height = h
		}
	}
	return width, height, scale
}

// auditArchFallback 在 UA-CH 拿不到 architecture 时按宿主架构兜底：档案里写错架构
// （Intel 机上自称 arm）是能被 WebGL/驱动串对出来的穿帮。
func auditArchFallback() string {
	if runtime.GOARCH == "arm64" {
		return "arm"
	}
	return "x86"
}

func auditMajor(s string) string {
	re := regexp.MustCompile(`(\d+)\.`)
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return ""
}

// ── 对账 ────────────────────────────────────────────────────────────

func auditProfileChecks(p *fingerprint.Profile, self map[string]any) []auditCheck {
	checks := []auditCheck{}
	add := func(kind, name, want string, got any, ok bool) {
		checks = append(checks, auditCheck{Name: name, Want: want, Got: auditStr(got), OK: ok, Kind: kind})
	}
	eq := func(kind, name, want string, got any) {
		add(kind, name, want, got, auditStr(got) == want)
	}

	eq("硬", "navigator.userAgent", p.Emulation.UserAgent, auditPick(self, "ua"))
	add("硬", "navigator.webdriver", "false", auditPick(self, "webdriver"), auditStr(auditPick(self, "webdriver")) == "false")
	if md := p.Emulation.UserAgentMetadata; md.Platform != "" {
		eq("硬", "userAgentData.platform", md.Platform, auditPick(self, "uaDataPlatform"))
	}
	major := auditMajor(p.Emulation.UserAgentMetadata.FullVersion)
	if major == "" {
		major = auditMajor(p.Emulation.UserAgent)
	}
	brands := auditStr(auditPick(self, "uaDataBrands"))
	if major != "" {
		add("硬", "userAgentData.brands 含 Chromium:"+major, "yes", brands, strings.Contains(brands, "Chromium:"+major))
	}
	if p.Emulation.UserAgentMetadata.FullVersion != "" {
		eq("硬", "userAgentData.fullVersion", p.Emulation.UserAgentMetadata.FullVersion, auditNested(self, "uaDataHigh", "uaFullVersion"))
	}
	if p.Emulation.UserAgentMetadata.PlatformVersion != "" {
		eq("硬", "userAgentData.platformVersion", p.Emulation.UserAgentMetadata.PlatformVersion, auditNested(self, "uaDataHigh", "platformVersion"))
	}
	if p.Emulation.UserAgentMetadata.Architecture != "" {
		eq("硬", "userAgentData.architecture", p.Emulation.UserAgentMetadata.Architecture, auditNested(self, "uaDataHigh", "architecture"))
	}
	if p.Emulation.Locale.TimezoneID != "" {
		eq("硬", "Intl 时区", p.Emulation.Locale.TimezoneID, auditPick(self, "tz"))
	}
	if len(p.Emulation.Locale.Languages) > 0 {
		eq("硬", "navigator.language", p.Emulation.Locale.Languages[0], auditPick(self, "language"))
		eq("硬", "navigator.languages[0]", p.Emulation.Locale.Languages[0], strings.Split(auditStr(auditPick(self, "languages")), ",")[0])
	}
	if p.Emulation.Screen.Width > 0 {
		eq("硬", "screen.width", strconv.Itoa(p.Emulation.Screen.Width), auditNested(self, "screen", "width"))
		eq("硬", "screen.height", strconv.Itoa(p.Emulation.Screen.Height), auditNested(self, "screen", "height"))
		eq("硬", "window.innerWidth", strconv.Itoa(p.Emulation.Screen.Width), auditNested(self, "windowMetrics", "innerWidth"))
		eq("硬", "window.innerHeight", strconv.Itoa(p.Emulation.Screen.Height), auditNested(self, "windowMetrics", "innerHeight"))
		eq("硬", "devicePixelRatio", strconv.FormatFloat(p.Emulation.Screen.DeviceScaleFactor, 'f', -1, 64), auditNested(self, "windowMetrics", "devicePixelRatio"))
		eq("软", "screen.availWidth", strconv.Itoa(p.Emulation.Screen.Width), auditNested(self, "screen", "availWidth"))
		eq("软", "screen.availHeight", strconv.Itoa(p.Emulation.Screen.Height), auditNested(self, "screen", "availHeight"))
	}
	// 子 iframe 必须继承同一份身份（风控常见的「换个 iframe 再看一次」）。
	eq("硬", "iframe.navigator.userAgent", p.Emulation.UserAgent, auditNested(self, "iframe", "ua"))
	if p.Emulation.UserAgentMetadata.Platform != "" {
		eq("硬", "iframe.userAgentData.platform", p.Emulation.UserAgentMetadata.Platform, auditNested(self, "iframe", "platform"))
	}
	add("硬", "自动化残留（window.cdc_*/document.$cdc_*）", "空", auditNested(self, "automationResidue", "windowCdcKeys"), auditStr(auditNested(self, "automationResidue", "windowCdcKeys"))+auditStr(auditNested(self, "automationResidue", "documentCdcKeys")) == "")
	add("观察", "window.chrome 存在", "true", auditNested(self, "automationResidue", "hasChrome"), auditStr(auditNested(self, "automationResidue", "hasChrome")) == "true")
	add("软", "navigator.plugins.length", ">0（无头常见 0）", auditPick(self, "pluginsCount"), auditStr(auditPick(self, "pluginsCount")) != "0")
	if p.Emulation.Hardware.HardwareConcurrency > 0 {
		eq("软", "navigator.hardwareConcurrency（Phase 3）", strconv.Itoa(p.Emulation.Hardware.HardwareConcurrency), auditPick(self, "hardwareConcurrency"))
	}
	if p.Emulation.Hardware.DeviceMemory > 0 {
		eq("软", "navigator.deviceMemory（Phase 3）", strconv.FormatFloat(p.Emulation.Hardware.DeviceMemory, 'f', -1, 64), auditPick(self, "deviceMemory"))
	}
	add("软", "页面是安全上下文", "true", auditNested(self, "secCtx", "isSecureContext"), auditStr(auditNested(self, "secCtx", "isSecureContext")) == "true")
	add("软", "navigator.userAgentData 可用", "true（仅安全上下文暴露）", auditNested(self, "secCtx", "hasUserAgentData"), auditStr(auditNested(self, "secCtx", "hasUserAgentData")) == "true")
	add("软", "WebGL 可用", "true（无头 + --disable-gpu 会没有）", auditNested(self, "webgl", "renderer"), auditStr(auditNested(self, "webgl", "renderer")) != "")
	innerW := auditNum(auditNested(self, "windowMetrics", "innerWidth"))
	innerH := auditNum(auditNested(self, "windowMetrics", "innerHeight"))
	outerW := auditNum(auditNested(self, "windowMetrics", "outerWidth"))
	outerH := auditNum(auditNested(self, "windowMetrics", "outerHeight"))
	add("软", "window.outerWidth ≥ innerWidth", "true（窗口比视口小的组合不可能）", fmt.Sprintf("outer=%v inner=%v", outerW, innerW), outerW > 0 && innerW > 0 && outerW >= innerW)
	add("软", "window.outerHeight ≥ innerHeight", "true", fmt.Sprintf("outer=%v inner=%v", outerH, innerH), outerH > 0 && innerH > 0 && outerH >= innerH)

	// 原生性：CDP 的覆盖应当仍是原生 getter；出现 JS 植入会被 toString 抓（Phase 3 的注入必须回到这条线上）。
	native := func(key string) {
		got := auditStr(auditNested(self, "nativeFns", key))
		add("软", "原生 getter @"+key, "[native code]", got, strings.Contains(got, "[native code]"))
	}
	native("ua")
	native("platform")
	native("hardwareConcurrency")
	native("webdriver")
	return checks
}

func auditMirrorChecks(profile *fingerprint.Profile, baseline, profiled *auditRun) []auditCheck {
	checks := []auditCheck{}
	add := func(name string, want, got string, ok bool) {
		checks = append(checks, auditCheck{Name: name, Want: want, Got: got, OK: ok, Kind: "观察"})
	}
	b, p := baseline.Self, profiled.Self

	// Phase 3：如果档案启用了对应噪声，跨档（基线 vs 档案）应当产生分歧；未开启时应当一致保持稳定。
	bCanvas, pCanvas := auditStr(auditPick(b, "canvas2dHash")), auditStr(auditPick(p, "canvas2dHash"))
	if profile != nil && profile.NoiseFeatures.Canvas2D {
		add("canvas2d 哈希：基线 vs 档案（Phase 3 噪声注入：跨档分歧）", "分歧", fmt.Sprintf("%s vs %s", bCanvas, pCanvas), bCanvas != pCanvas)
	} else {
		add("canvas2d 哈希：基线 vs 档案（同档无噪声：保持稳定）", bCanvas, pCanvas, bCanvas == pCanvas)
	}

	bAudio, pAudio := auditStr(auditPick(b, "audioHash")), auditStr(auditPick(p, "audioHash"))
	if profile != nil && profile.NoiseFeatures.AudioContext {
		add("audio 哈希：基线 vs 档案（Phase 3 噪声注入：跨档分歧）", "分歧", fmt.Sprintf("%s vs %s", bAudio, pAudio), bAudio != pAudio)
	} else {
		add("audio 哈希：基线 vs 档案（同档无噪声：保持稳定）", bAudio, pAudio, bAudio == pAudio)
	}

	add("WebGL 渲染器：基线 vs 档案", auditStr(auditNested(b, "webgl", "unmaskedRenderer")), auditStr(auditNested(p, "webgl", "unmaskedRenderer")), auditStr(auditNested(b, "webgl", "unmaskedRenderer")) == auditStr(auditNested(p, "webgl", "unmaskedRenderer")))
	add("已装字体集：基线 vs 档案", auditStr(auditPick(b, "fonts")), auditStr(auditPick(p, "fonts")), auditStr(auditPick(b, "fonts")) == auditStr(auditPick(p, "fonts")))
	return checks
}

func auditEgressChecks(p *fingerprint.Profile, direct string, baseline, profiled *auditRun) []auditCheck {
	checks := []auditCheck{}
	baselineIP, profileIP := baseline.EgressIP, profiled.EgressIP
	proxied := p != nil && p.Network.ProxyURL != ""
	kind := "观察"
	want := "与裸奔基线出口一致（没绑代理时同机直连；本机出口可能变，故只记账）"
	ok := baselineIP != "" && profileIP == baselineIP
	if proxied {
		kind = "硬"
		want = "与基线出口不同（档案绑了 " + fingerprint.RedactProxyURL(p.Network.ProxyURL) + "）"
		ok = profileIP != "" && baselineIP != "" && profileIP != baselineIP
	}
	checks = append(checks, auditCheck{Name: "档案会话出口 IP", Want: want, Got: profileIP, OK: ok, Kind: kind})
	checks = append(checks, auditCheck{Name: "裸奔基线出口 IP", Want: "能读到（非空）", Got: baselineIP, OK: baselineIP != "", Kind: "观察"})
	if direct != "" {
		// Go 的直连出口与 Chrome 的出口本就可以不同（Chrome 走 macOS 系统代理、Go 只认
		// HTTPS_PROXY 环境变量），所以它只是参考值，不参与判定。
		checks = append(checks, auditCheck{Name: "Go 直连出口 IP（参考）", Want: "—", Got: direct, OK: true, Kind: "观察"})
	}
	// WebRTC 出口：2026-09-13 实测——`--proxy-server` 管不到 WebRTC 的 UDP，绑代理的会话在
	// BrowserLeaks 上会直接暴露**本机真实出口**（HTTP 出口是代理、WebRTC 出口是另一个 IP）。
	// 未绑代理时这条只是记账；绑了代理（要的就是隔离）时它是硬判据。
	webrtc := profiled.WebRTCIP
	if proxied {
		ok := webrtc == "" || (profileIP != "" && webrtc == profileIP)
		checks = append(checks, auditCheck{Name: "WebRTC 出口 IP（绑代理时不许是真机出口）",
			Want: "空（无候选）或 = 代理出口 " + profileIP, Got: webrtc, OK: ok, Kind: "硬"})
	} else {
		checks = append(checks, auditCheck{Name: "WebRTC 出口 IP（未绑代理，记账）",
			Want: "记录；有值即说明页面能拿到本机真实出口（绑代理后会变成泄漏）", Got: webrtc, OK: true, Kind: "观察"})
	}
	return checks
}

// auditExtractWebRTCIP 从 BrowserLeaks /webrtc 的表格里抽出「Public IP Address」那一格。
// 表格是 `... Public IP Address 1.2.3.4 ...` 的扁串；没有候选时该格是 `-`。
func auditExtractWebRTCIP(res auditSiteResult) string {
	obj, ok := res.Raw.(map[string]any)
	if !ok {
		return ""
	}
	tables, _ := obj["tables"].([]any)
	for _, t := range tables {
		text, _ := t.(string)
		idx := strings.Index(text, "Public IP Address")
		if idx < 0 {
			continue
		}
		tail := text[idx+len("Public IP Address"):]
		if ip := auditIPRe.FindString(tail); ip != "" {
			return ip
		}
	}
	return ""
}
