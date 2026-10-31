package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"uws/pkg/fingerprint"
)

// fingerprint_audit_test.go —— 指纹体系的「外站验收」（2026-09-13，Phase 2 → Phase 3 的门槛）。
//
// 为什么要有这一例：fingerprint_e2e_test.go 证的是「档案 → 浏览器自报身份」，那是自己对自己；
// 真实风控看的是**外部站点脚本眼里的我们**。本用例把一份身份档案真的开到
// bot.sannysoft / CreepJS / BrowserLeaks（外加出口 IP）上，把网站看到的事实抓回来，
// 落成可复核的证据（JSON + Markdown + 截图），并把「档案说了但浏览器没做到」钉成断言。
//
// 两道会话：先跑**裸奔基线**（不带档案），再跑**带档案**（默认档案从基线镜像派生：
// 同一只 Chrome 版本、同一套语言/时区、UA 去掉 HeadlessChrome 标记，另加一份指定的屏幕）。
// 于是「档案会话是否真的等于档案」与「相对裸奔多了/少了什么」两件事同时有账可查。
//
// 开关（都可能碰外网/桌面，默认关）：
//   UWS_RUN_EXTERNAL_AUDIT=1   允许走外网做验收（否则整例 Skip）
//   UWS_AUDIT_HEADED=1         有头跑（会真的在桌面上开一扇 Chrome 窗口；默认无头不打扰桌面）
//   UWS_AUDIT_PROFILE=<path>   指定档案文件（给了就不再镜像派生）
//   UWS_AUDIT_PROFILE_ID=<id>  指定身份注册表里的身份（用它的持久目录）
//   UWS_AUDIT_SCREEN=1440x900@2  档案里的屏幕（默认值；镜像档只换屏幕这一项设备事实）
//   UWS_AUDIT_SITES=sannysoft,creepjs,browserleaks,egress  只跑指定站点（默认全跑；基线固定跑 egress+sannysoft）
//   UWS_AUDIT_OUT=<dir>        证据目录（默认 <repo>/docs/evidence/fingerprint-audit/<日期>-<模式>）
//   UWS_AUDIT_PROFILE_JSON=<json>  直接给一段档案 JSON（调试派生逻辑用）

// ── 页面自报身份采集脚本 ───────────────────────────────────────────────
//
// 每条探测各自 try/catch：一个 API 不存在不能让整份报告变空（宁缺一项，不要空白）。
const auditSelfScript = `(async () => {
  const out = { errors: [] };
  const T = async (k, f) => { try { out[k] = await f(); } catch (e) { out.errors.push(k + ': ' + ((e && e.message) ? e.message : String(e))); } };
  const N = navigator, S = screen;
  const fnv = (s) => { let h = 0x811c9dc5; for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 0x01000193) >>> 0; } return h.toString(16); };
  const g = (o, k) => { try { const d = Object.getOwnPropertyDescriptor(o, k); return (d && d.get) ? Function.prototype.toString.call(d.get) : null; } catch (e) { return 'err:' + e.message; } };
  await T('ua', () => N.userAgent);
  await T('appVersion', () => N.appVersion);
  await T('platform', () => N.platform);
  await T('vendor', () => N.vendor);
  await T('product', () => N.product);
  await T('webdriver', () => N.webdriver);
  await T('language', () => N.language);
  await T('languages', () => (N.languages || []).join(','));
  await T('hardwareConcurrency', () => N.hardwareConcurrency);
  await T('deviceMemory', () => (N.deviceMemory === undefined ? null : N.deviceMemory));
  await T('maxTouchPoints', () => N.maxTouchPoints);
  await T('pdfViewerEnabled', () => N.pdfViewerEnabled);
  await T('pluginsCount', () => N.plugins.length);
  await T('pluginNames', () => Array.from(N.plugins).map((p) => p.name).join(','));
  await T('mimeTypesCount', () => N.mimeTypes.length);
  await T('onLine', () => N.onLine);
  await T('uaDataPlatform', () => ((N.userAgentData || {}).platform || null));
  await T('uaDataMobile', () => (N.userAgentData || {}).mobile);
  await T('uaDataBrands', () => (((N.userAgentData || {}).brands) || []).map((b) => b.brand + ':' + b.version).join(', '));
  await T('uaDataHigh', async () => {
    if (!N.userAgentData || !N.userAgentData.getHighEntropyValues) return null;
    const h = await N.userAgentData.getHighEntropyValues(['architecture','bitness','model','platformVersion','uaFullVersion','fullVersionList','wow64']);
    return { architecture: h.architecture, bitness: h.bitness, model: h.model, platformVersion: h.platformVersion,
             uaFullVersion: h.uaFullVersion, wow64: (h.wow64 === undefined ? null : h.wow64),
             fullVersionList: (h.fullVersionList || []).map((b) => b.brand + ':' + b.version).join(', ') };
  });
  await T('screen', () => ({ width: S.width, height: S.height, availWidth: S.availWidth, availHeight: S.availHeight, colorDepth: S.colorDepth, pixelDepth: S.pixelDepth, orientation: ((S.orientation || {}).type || null) }));
  await T('windowMetrics', () => ({ innerWidth: window.innerWidth, innerHeight: window.innerHeight, outerWidth: window.outerWidth, outerHeight: window.outerHeight, screenX: window.screenX, screenY: window.screenY, devicePixelRatio: window.devicePixelRatio }));
  await T('tz', () => Intl.DateTimeFormat().resolvedOptions().timeZone);
  await T('tzOffset', () => new Date().getTimezoneOffset());
  await T('intlLocale', () => Intl.DateTimeFormat().resolvedOptions().locale);
  await T('notificationPermission', () => (typeof Notification === 'undefined' ? null : Notification.permission));
  await T('permissions', async () => {
    const q = async (name) => { try { const st = await navigator.permissions.query({ name: name }); return st.state; } catch (e) { return 'unsupported'; } };
    return { geolocation: await q('geolocation'), notifications: await q('notifications'), camera: await q('camera'), microphone: await q('microphone') };
  });
  await T('webgl', () => {
    const c = document.createElement('canvas');
    const gl = c.getContext('webgl') || c.getContext('experimental-webgl');
    if (!gl) return null;
    const dbg = gl.getExtension('WEBGL_debug_renderer_info');
    return { vendor: gl.getParameter(gl.VENDOR), renderer: gl.getParameter(gl.RENDERER),
             unmaskedVendor: (dbg ? gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL) : null),
             unmaskedRenderer: (dbg ? gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL) : null),
             version: gl.getParameter(gl.VERSION), shading: gl.getParameter(gl.SHADING_LANGUAGE_VERSION),
             maxTextureSize: gl.getParameter(gl.MAX_TEXTURE_SIZE),
             extCount: (gl.getSupportedExtensions() || []).length };
  });
  await T('webgl2', () => !!document.createElement('canvas').getContext('webgl2'));
  await T('webgpu', () => !!navigator.gpu);
  await T('canvas2dHash', () => {
    const c = document.createElement('canvas'); c.width = 240; c.height = 60;
    const x = c.getContext('2d');
    x.textBaseline = 'top'; x.font = '14px Arial';
    x.fillStyle = '#f60'; x.fillRect(0, 0, 120, 20);
    x.fillStyle = '#069'; x.fillText('UWS fingerprint audit', 2, 15);
    x.strokeStyle = 'rgba(102,204,0,0.7)'; x.arc(60, 30, 20, 0, Math.PI * 2); x.stroke();
    return fnv(c.toDataURL());
  });
  await T('audioHash', async () => {
    const OAC = window.OfflineAudioContext || window.webkitOfflineAudioContext;
    if (!OAC) return null;
    const ctx = new OAC(1, 44100, 44100);
    const osc = ctx.createOscillator(); osc.type = 'triangle'; osc.frequency.value = 10000;
    const comp = ctx.createDynamicsCompressor();
    osc.connect(comp); comp.connect(ctx.destination); osc.start(0); osc.stop(2);
    const buf = await ctx.startRendering();
    const d = buf.getChannelData(0);
    let sum = 0; for (let i = 4500; i < 5000; i++) sum += Math.abs(d[i]);
    return sum.toFixed(8);
  });
  await T('fonts', () => {
    const base = ['monospace', 'sans-serif', 'serif'];
    const probe = ['Helvetica','Arial','Times New Roman','Courier New','Menlo','Monaco','PingFang SC','Hiragino Sans GB','Songti SC','Helvetica Neue','Geneva','Verdana','Tahoma','Georgia','Impact','Comic Sans MS','Segoe UI','Roboto','Noto Sans CJK SC','Microsoft YaHei'];
    const span = document.createElement('span');
    span.style.fontSize = '72px'; span.style.position = 'absolute'; span.style.left = '-9999px';
    span.textContent = 'mmmmmmmmmmlli';
    document.body.appendChild(span);
    const measure = (family) => { span.style.fontFamily = family; return span.offsetWidth; };
    const widths = {};
    base.forEach((b) => { widths[b] = measure(b); });
    const hit = probe.filter((f) => base.some((b) => measure('"' + f + '",' + b) !== widths[b]));
    document.body.removeChild(span);
    return hit.join(',');
  });
  await T('nativeFns', () => ({
    ua: g(Navigator.prototype, 'userAgent'),
    platform: g(Navigator.prototype, 'platform'),
    hardwareConcurrency: g(Navigator.prototype, 'hardwareConcurrency'),
    languages: g(Navigator.prototype, 'languages'),
    webdriver: g(Navigator.prototype, 'webdriver'),
    userAgentData: g(Navigator.prototype, 'userAgentData')
  }));
  await T('iframe', () => {
    const f = document.createElement('iframe');
    f.style.display = 'none';
    document.body.appendChild(f);
    const n = f.contentWindow.navigator;
    const r = { ua: n.userAgent, platform: ((n.userAgentData || {}).platform || null), languages: (n.languages || []).join(','),
                hardwareConcurrency: n.hardwareConcurrency, webdriver: n.webdriver, pluginsCount: n.plugins.length };
    document.body.removeChild(f);
    return r;
  });
  await T('automationResidue', () => ({
    windowCdcKeys: Object.getOwnPropertyNames(window).filter((k) => /^(cdc_|\$cdc_|_phantom|__nightmare|_selenium|callSelenium|webdriver_|driver_)/i.test(k)).join(','),
    documentCdcKeys: Object.getOwnPropertyNames(document).filter((k) => /^\$?cdc_/i.test(k)).join(','),
    hasChrome: !!window.chrome,
    chromeKeys: (window.chrome ? Object.keys(window.chrome).join(',') : '')
  }));
  await T('secCtx', () => ({ isSecureContext: window.isSecureContext, origin: location.origin, protocol: location.protocol,
             hasUserAgentData: ('userAgentData' in N), uaDataKeys: (N.userAgentData ? Object.keys(N.userAgentData).join(',') : '') }));
  await T('errorStack', () => { try { return (new Error('audit').stack || '').split('\n').slice(0, 3).join(' | '); } catch (e) { return null; } });
  return JSON.stringify(out);
})()`

const auditEgressScript = `(function () { return JSON.stringify({ url: location.href, text: (document.body ? document.body.innerText : '').trim().slice(0, 400) }); })()`

const auditSannysoftScript = `(function () { return JSON.stringify({
  title: document.title,
  rows: Array.from(document.querySelectorAll('table tr')).map((tr) => Array.from(tr.cells).map((td) => (td.innerText || '').trim().replace(/\s+/g, ' ')).join(' | ')).filter((s) => s.length > 0).slice(0, 90),
  failed: Array.from(document.querySelectorAll('.failed')).map((e) => (e.innerText || '').trim().replace(/\s+/g, ' ').slice(0, 120)).slice(0, 40),
  passedCount: document.querySelectorAll('.passed').length,
  text: (document.body ? document.body.innerText : '').slice(0, 2500)
}); })()`

const auditCreepjsScript = `(function () { return JSON.stringify({
  title: document.title,
  text: (document.body ? document.body.innerText : '').slice(0, 12000)
}); })()`

const auditBrowserleaksWebRTCScript = `(function () { return JSON.stringify({
  title: document.title,
  tables: Array.from(document.querySelectorAll('table')).map((t) => (t.innerText || '').trim().replace(/\s+/g, ' ').slice(0, 1200)).slice(0, 12),
  text: (document.body ? document.body.innerText : '').slice(0, 4000)
}); })()`

// ── 数据形状 ────────────────────────────────────────────────────────

type auditCheck struct {
	Name string `json:"name"`
	Want string `json:"want"`
	Got  string `json:"got"`
	OK   bool   `json:"ok"`
	Kind string `json:"kind"` // 硬：档案承诺必须兑现 / 软：本 Phase 未实现或环境相关 / 观察：只记账
}

type auditSiteResult struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	WaitedMS int64  `json:"waited_ms"`
	Err      string `json:"err,omitempty"`
	Raw      any    `json:"raw,omitempty"`
	Shot     string `json:"shot,omitempty"`
}

type auditRun struct {
	Session       string            `json:"session"` // baseline / profile
	Self          map[string]any    `json:"self"`
	Sites         []auditSiteResult `json:"sites"`
	EgressIP      string            `json:"egress_ip"`
	WebRTCIP      string            `json:"webrtc_public_ip,omitempty"`
	BrowserBuild  string            `json:"browser_build"`
	ShotSelfCheck string            `json:"shot_self_check,omitempty"`
}

type auditReport struct {
	StartedAt     string               `json:"started_at"`
	Mode          string               `json:"mode"`
	ChromeArgs    []string             `json:"chrome_args,omitempty"`
	ChromePath    string               `json:"chrome_path"`
	ChromeVersion string               `json:"chrome_version"`
	OSVersion     string               `json:"os_version"`
	ProfileSource string               `json:"profile_source"`
	Profile       *fingerprint.Profile `json:"profile"`
	Checks        []auditCheck         `json:"checks"`
	HardFails     int                  `json:"hard_fails"`
	Baseline      *auditRun            `json:"baseline"`
	Profiled      *auditRun            `json:"profiled"`
	DirectEgress  string               `json:"direct_egress_ip"`
	Notes         []string             `json:"notes"`
}

type auditSite struct {
	name    string
	url     string
	script  string
	waitFor string
	waitMax time.Duration
	settle  time.Duration
}

var auditSites = map[string]auditSite{
	"egress": {
		name: "egress", url: "https://api.ipify.org/?format=json", script: auditEgressScript,
		waitFor: "!!document.body && document.body.innerText.length > 0", waitMax: 20 * time.Second, settle: time.Second,
	},
	"sannysoft": {
		name: "sannysoft", url: "https://bot.sannysoft.com/", script: auditSannysoftScript,
		waitFor: "document.querySelectorAll('table tr').length > 5", waitMax: 25 * time.Second, settle: 4 * time.Second,
	},
	"creepjs": {
		name: "creepjs", url: "https://abrahamjuliot.github.io/creepjs/", script: auditCreepjsScript,
		waitFor: "/trust|bot|FP ID|fuzzy/i.test(document.body.innerText)", waitMax: 75 * time.Second, settle: 12 * time.Second,
	},
	"browserleaks": {
		name: "browserleaks", url: "https://browserleaks.com/webrtc", script: auditBrowserleaksWebRTCScript,
		waitFor: "document.querySelectorAll('table').length > 0", waitMax: 30 * time.Second, settle: 18 * time.Second,
	},
}

// ── 主用例 ──────────────────────────────────────────────────────────

func TestExternalFingerprintAudit(t *testing.T) {
	if os.Getenv("UWS_RUN_EXTERNAL_AUDIT") != "1" {
		t.Skip("未设置 UWS_RUN_EXTERNAL_AUDIT=1：外站验收会走外网（并有头模式会开窗口），默认跳过")
	}
	requireCDP(t)

	headed := os.Getenv("UWS_AUDIT_HEADED") == "1"
	mode := "headless"
	if headed {
		mode = "headed"
	}
	outDir := auditOutDir(t, mode)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("建证据目录失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	repoRoot := auditRepoRoot(t)
	chromePath, _ := resolveChrome("")
	chromeVersion := auditLocalChromeVersion(chromePath)
	osVersion := auditLocalOSVersion()

	rep := &auditReport{
		StartedAt:     time.Now().Format(time.RFC3339),
		Mode:          mode,
		ChromeArgs:    auditExtraArgs(),
		ChromePath:    chromePath,
		ChromeVersion: chromeVersion,
		OSVersion:     osVersion,
	}
	rep.DirectEgress = auditDirectEgressIP(ctx)
	if rep.DirectEgress == "" {
		rep.Notes = append(rep.Notes, "Go 侧直连出口 IP 没读到（不影响判定：出口以浏览器实测为准）")
	}

	// 自测面必须跑在**安全上下文**里：about:blank 是不透明源，navigator.userAgentData
	// 在那里根本不存在（UA-CH 只在 secure context 暴露）——用 127.0.0.1 起一张本地页。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>uws fingerprint self-check</title><p>self check</p>"))
	}))
	defer server.Close()
	selfURL := server.URL

	// 1) 裸奔基线：不带档案，同一只 Chrome 在检测站点眼里是什么样。
	baseline := auditRunSession(ctx, t, auditRunConfig{
		session: "baseline", headless: !headed, outDir: outDir, repoRoot: repoRoot,
		sites: auditSelectedSites("baseline"), profile: nil, profileID: "", selfURL: selfURL,
	})
	rep.Baseline = baseline

	// 2) 档案：显式给的就用给的；没给就从基线镜像派生（同一只 Chrome + 指定屏幕）。
	profile, source, err := auditResolveProfile(baseline)
	if err != nil {
		t.Fatalf("准备身份档案失败: %v", err)
	}
	rep.Profile = profile
	rep.ProfileSource = source

	profiled := auditRunSession(ctx, t, auditRunConfig{
		session: "profile", headless: !headed, outDir: outDir, repoRoot: repoRoot,
		sites: auditSelectedSites("profile"), profile: profile,
		profileID: strings.TrimSpace(os.Getenv("UWS_AUDIT_PROFILE_ID")), selfURL: selfURL,
	})
	rep.Profiled = profiled

	// 3) 对账。
	rep.Checks = append(rep.Checks, auditProfileChecks(profile, profiled.Self)...)
	rep.Checks = append(rep.Checks, auditMirrorChecks(profile, baseline, profiled)...)
	rep.Checks = append(rep.Checks, auditEgressChecks(profile, rep.DirectEgress, baseline, profiled)...)
	for _, c := range rep.Checks {
		if !c.OK && c.Kind == "硬" {
			rep.HardFails++
		}
	}
	if !profile.MatchesChromeVersion(chromeVersion) {
		rep.Notes = append(rep.Notes, fmt.Sprintf("档案瞄的 Chrome 大版本（%s）与手上这只（%s）不一致：UA/Client Hints 会与真实内核对不上",
			profile.TargetPlatform.ChromeVersion, chromeVersion))
	}
	if missing := profile.MissingNoiseFeatures(); len(missing) > 0 {
		rep.Notes = append(rep.Notes, "档案点名但本 Phase 还没实现的噪声能力（Phase 3）："+strings.Join(missing, ", "))
	}

	if err := auditWriteEvidence(outDir, rep); err != nil {
		t.Fatalf("写证据失败: %v", err)
	}
	t.Logf("外站验收证据已落盘: %s（硬失败 %d 项）", outDir, rep.HardFails)

	if rep.HardFails > 0 {
		for _, c := range rep.Checks {
			if !c.OK && c.Kind == "硬" {
				t.Errorf("硬检查未过：%s（档案 %s，实际 %s）", c.Name, c.Want, c.Got)
			}
		}
	}
}
