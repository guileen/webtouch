package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"uws/pkg/browser/modelbridge"

	"uws/pkg/browser/profiledir"
	"uws/pkg/fingerprint"
)

// SerializedSessionState 保存了 CDP 导出的 Cookie 和 Storage 状态。
type SerializedSessionState struct {
	CookiesJSON string                 `json:"cookies_json"`
	Origins     []StorageOriginContext `json:"origins"`
}

type StorageOriginContext struct {
	Origin         string           `json:"origin"`
	LocalStorage   []map[string]any `json:"localStorage"`
	SessionStorage []map[string]any `json:"sessionStorage"`
}

// maxInflightAge 是在途 XHR/Fetch 计入「网络忙碌」的上限时长：超过该时长的请求
// 视为挂起/长轮询，不再阻塞稳定判定（真实页面常见长轮询、SSE 长连接）。
const maxInflightAge = 30 * time.Second

// CDPFactory 创建真实 CDP 会话（启动 Chrome 或附着已有 Chrome）。
type CDPFactory struct {
	ChromePath    string        // 空 = 自动探测（macOS 默认安装路径 / PATH）
	Port          int           // 启动模式调试端口；0 = 自动挑选空闲端口
	Headless      bool          // 启动模式是否无头（默认 true）
	LaunchTimeout time.Duration // 启动等待上限；0 = 15s
	ProbeWasmPath string        // WASM 探针 .wasm 路径；空 = 自动探测
	// （build/probe_wasm_wasm/probe_wasm.wasm；缺失时页面候选降级为 JS 文本采集）

	// ProfileRoot 是持久身份 profile 的根目录（空 = UWS_PROFILE_ROOT 或
	// ~/.uws/profiles）。只有 Config.ProfileID 非空时才用得上，见 pkg/browser/profiledir。
	ProfileRoot string
	// ExtraArgs 追加到启动命令行末尾（空 = 不追加）。用途是**验收/实验**：例如
	// 「无头 + --disable-gpu 让 WebGL 整块消失」这类可探测面差异，得能不开分支就跑出来
	// （见 pkg/browser/fingerprint_audit_test.go）。生产路径不要用它做身份伪装——
	// 身份该走 Config.Fingerprint（档案），这里是给审计留的针孔。
	ExtraArgs []string

	next int64
}

// Create 实现 browser.SessionFactory。
func (f *CDPFactory) Create(ctx context.Context, cfg Config) (CDPSession, error) {
	if fp := cfg.Fingerprint; fp != nil {
		if err := fp.Validate(); err != nil {
			return nil, fmt.Errorf("cdp: fingerprint: %w", err)
		}
		if cfg.ProfileID != "" && cfg.ProfileID != fp.ProfileID {
			return nil, fmt.Errorf("cdp: profile_id %q 与 fingerprint.profile_id %q 不一致", cfg.ProfileID, fp.ProfileID)
		}
	}
	launchTimeout := f.LaunchTimeout
	if launchTimeout <= 0 {
		launchTimeout = 15 * time.Second
	}
	s := &cdpSession{
		id:        fmt.Sprintf("cdp-%d", f.next+1),
		cfg:       cfg,
		headless:  f.Headless,
		probePath: f.ProbeWasmPath,
		closedCh:  make(chan struct{}),
		inflight:  make(map[string]time.Time),
		lastMouse: pt{},
		mouseInit: false,
		chat:      modelbridge.NewStream(),
		capture:   newRecipeCaptureSink(),
	}
	var endpoint string
	if cfg.AttachCDPURL != "" {
		resolved, err := resolveBrowserEndpoint(ctx, cfg.AttachCDPURL)
		if err != nil {
			return nil, fmt.Errorf("cdp: attach: %w", err)
		}
		endpoint = resolved
		if fp := cfg.Fingerprint; fp != nil {
			log.Printf("browser: 附着模式起不了新进程，身份档案的启动参数（自动化标志/语言/代理）用不上，只应用 CDP 覆盖（UA/UA-CH/时区/语言/屏幕）")
			if fp.Network.ProxyURL != "" {
				// 最危险的缺口不是「少伪装一点」，而是「以为换了出口、其实还在本机」：
				// 档案绑了代理却在附着模式跑，流量走的是这台机器的真实 IP（2026-09-13 评审）。
				log.Printf("browser: ⚠️ 附着模式无法带 --proxy-server：身份档案 %q 绑定的代理 %s **没有生效**，本会话走本机真实出口",
					fp.ProfileID, fingerprint.RedactProxyURL(fp.Network.ProxyURL))
			}
		}
		// 附着对象的形态以**它自己**为准（2026-09-22 现场批次 #2）：引擎启动默认
		// headless=true，若附着到真 Chrome 时沿用这个默认，会话会被当成无头——
		// windowPolicy() 因 headless 退化成 tab（标签塞进别人的窗口）、
		// RaiseViewerWindow 直接报 ErrNoViewerWindow，「点小窗看真页面」整条链路就断了。
		// 所以这里双向判定；读不到 UA 时保留默认，不猜。
		if headless, known := browserHeadlessState(ctx, cfg.AttachCDPURL); known {
			s.headless = headless
		}
	} else {
		chrome, err := resolveChrome(f.ChromePath)
		if err != nil {
			return nil, err
		}
		port := f.Port
		if port == 0 {
			port, err = freePort()
			if err != nil {
				return nil, err
			}
		}
		profile, persistent, err := f.openProfile(cfg)
		if err != nil {
			return nil, fmt.Errorf("cdp: profile: %w", err)
		}
		// profile 里预置「关掉翻译」与「封死 WebRTC 真实 IP 泄漏」的偏好（见 seedChromePreferences）。
		// 无论有头还是无头均在启动前落盘，保证非代理 UDP 被彻底阻断。
		policy := fingerprint.WebRTCPolicyDisableNonProxied
		if fp := cfg.Fingerprint; fp != nil {
			if p := fp.EffectiveWebRTCPolicy(); p != "" {
				policy = p
			}
		}
		seedChromePreferences(profile, policy)
		// 注意：浏览器进程生命周期必须独立于 Create 的 ctx——
		// exec.CommandContext 会在 ctx 取消时杀掉进程，导致会话创建返回后
		// Chrome 立即退出（真实集成测试踩坑）。启动等待用独立超时 ctx。
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run",
			"--disable-crash-reporter", "--disable-component-update",
			"--disable-background-networking",
			// 反节流（2026-09-12，GLOBAL #92）：后台/被遮挡窗口默认会被节流，
			// 物理输入可能挂起到超时；有头启动模式下必须关掉。
			"--disable-backgrounding-occluded-windows",
			"--disable-renderer-backgrounding",
			"--disable-background-timer-throttling",
			"--remote-debugging-port="+strconv.Itoa(port),
			"--remote-debugging-address=127.0.0.1",
			"--user-data-dir="+profile)
		// 有头启动直接用 Chrome 的 app 窗口：没有标签栏、没有地址栏，只有页面本身
		// （2026-09-12 用户口径「我只需要页面那个载体本身」）。它也必须先打开
		// about:blank：身份注入只有在 initTarget 完成后才注册，若把 InitialURL 放进
		// --app，页面首帧脚本会先看到真实环境。随后由 CDP 导航到 InitialURL。
		launchArgs := []string{"about:blank"}
		if !f.Headless {
			windowPolicy := resolveWindowPolicy()
			s.appWindow = true
			launchArgs = []string{
				fmt.Sprintf("--window-size=%d,%d", windowPolicy.width, windowPolicy.height),
				"--app=" + appWindowURL("about:blank"),
			}
			launchArgs = append(launchArgs, chromeBubbleSuppressionArgs...)
		}
		// 身份档案里必须在**启动期**生效的那部分：自动化标志擦除、语言、代理。
		// navigator.webdriver / navigator.language 是渲染进程启动期定下的，连上 CDP
		// 之后再改已经晚了（见 pkg/fingerprint.ChromeArgs）。
		if fp := cfg.Fingerprint; fp != nil {
			args := fp.ChromeArgs()
			launchArgs = append(launchArgs, args...)
			log.Printf("browser: 身份档案 %q 启动参数 %v", fp.ProfileID, redactChromeArgs(args))
			if warn := fp.WebRTCLeakWarning(); warn != "" {
				// 绑了代理而 WebRTC 没人管：这事必须在日志里说出来（实测浏览器参数挡不住，
				// 见 pkg/fingerprint.EffectiveWebRTCPolicy 的注释与方案 §7.5）。
				log.Printf("browser: ⚠️ 身份档案 %q 绑了代理，但 %s", fp.ProfileID, warn)
			}
		}
		cmd.Args = append(cmd.Args, launchArgs...)
		if !f.Headless {
			cmd.Args = replaceHeadless(cmd.Args)
		}
		if len(f.ExtraArgs) > 0 {
			cmd.Args = append(cmd.Args, f.ExtraArgs...)
			log.Printf("browser: 追加验收用启动参数 %v", f.ExtraArgs)
		}
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			if !persistent {
				_ = os.RemoveAll(profile)
			}
			return nil, fmt.Errorf("cdp: launch chrome: %w", err)
		}
		s.chromeCmd = cmd
		s.profileDir = profile
		s.profilePersistent = persistent
		s.launched = true
		waitCtx, cancel := context.WithTimeout(ctx, launchTimeout)
		endpoint, err = waitForEndpoint(waitCtx, port)
		cancel()
		if err != nil {
			_ = cmd.Process.Kill()
			if !persistent {
				_ = os.RemoveAll(profile)
			}
			return nil, fmt.Errorf("cdp: wait devtools endpoint: %w", err)
		}
	}
	s.httpBase = wsBaseFromEndpoint(endpoint)

	client, err := newCDPClient(ctx, endpoint)
	if err != nil {
		s.teardownChrome()
		return nil, fmt.Errorf("cdp: connect %s: %w", endpoint, err)
	}
	s.client = client
	if s.launched {
		// 兜底记录：有头启动已改成「app 起始窗即会话窗」（见 initTarget），这条只在
		// app 窗口没按时出现、回落成另开一扇窗时才用得上——那时 Chrome 的起始窗是
		// 一块没人用的白板，会话窗就位后要关掉它。
		s.startupTargetID = s.startupPageTarget(ctx)
	}
	if err := s.initTarget(ctx); err != nil {
		s.Close()
		return nil, err
	}
	if s.launched && s.appWindow && strings.TrimSpace(cfg.InitialURL) != "" && cfg.InitialURL != "about:blank" {
		if err := s.Navigate(ctx, cfg.InitialURL); err != nil {
			s.Close()
			return nil, fmt.Errorf("cdp: navigate initial URL after fingerprint setup: %w", err)
		}
	}
	f.next++
	s.pointerHumanizer().StartDrift()
	return s, nil
}

// chromeBubbleSuppressionArgs 关掉有头 app 窗口里的浏览器气泡。app 窗口没有地址栏，
// 这些提示没有正经宿主，会直接飘在页面上：2026-09-12 用户现场看到「窗口左上角浮着
// 一个谷歌翻译的小小小组件」——那是 Chrome 对英文页面的「翻译此页？」气泡。观看壳
// 要的是干净的页面载体本身，所以翻译、默认浏览器检查、崩溃恢复气泡一律关掉。
var chromeBubbleSuppressionArgs = []string{
	"--disable-features=Translate,TranslateUI",
	"--disable-translate",
	"--no-default-browser-check",
	"--disable-session-crashed-bubble",
	"--hide-crash-restore-bubble",
	"--noerrdialogs",
}

func redactChromeArgs(args []string) []string {
	const proxyPrefix = "--proxy-server="
	redacted := append([]string(nil), args...)
	for i, arg := range redacted {
		if strings.HasPrefix(arg, proxyPrefix) {
			redacted[i] = proxyPrefix + fingerprint.RedactProxyURL(strings.TrimPrefix(arg, proxyPrefix))
		}
	}
	return redacted
}

// seedChromePreferences 在临时 profile 里预置最小 Preferences：关掉「翻译此页？」气泡。
//
// 2026-09-12 真机复跑：app 窗口没有地址栏，翻译气泡没有地方可以依附，会直接飘在页面
// seedChromePreferences 在 profile 里预置最小 Preferences：
// 1. 关掉「翻译此页？」气泡（translate.enabled: false）；
// 2. 封死 WebRTC 非代理 UDP 泄漏（webrtc.ip_handling_policy: disable_non_proxied_udp）。
//
// 启动参数（--disable-features=Translate、--force-webrtc-ip-handling-policy）在部分 Chrome
// 版本上未能完全读入；Profile Preferences 文件是 Chrome 启动时必读的持久层，在这里焊死最稳。
// 写失败不阻塞会话：少一层护栏，页面照常打开。
func seedChromePreferences(profile string, policies ...string) {
	dir := filepath.Join(profile, "Default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("browser: 建 profile Default 目录失败: %v", err)
		return
	}
	policy := fingerprint.WebRTCPolicyDisableNonProxied
	if len(policies) > 0 && policies[0] != "" {
		policy = policies[0]
	}
	path := filepath.Join(dir, "Preferences")
	// 持久身份里这个文件是用户自己的：做最小合并，绝不整体覆盖已有的其他数据。
	if existing, err := os.ReadFile(path); err == nil {
		merged, changed, err := updateChromePreferences(existing, policy)
		if err != nil {
			log.Printf("browser: 解析已有 Preferences 失败（保持原样）: %v", err)
			return
		}
		if !changed {
			return
		}
		if err := os.WriteFile(path, merged, 0o600); err != nil {
			log.Printf("browser: 合并 Preferences 失败: %v", err)
		}
		return
	}
	prefs := fmt.Sprintf(`{"translate":{"enabled":false},"webrtc":{"ip_handling_policy":%q}}`, policy)
	if err := os.WriteFile(path, []byte(prefs), 0o600); err != nil {
		log.Printf("browser: 写 Preferences 失败: %v", err)
	}
}

// updateChromePreferences 在既有 Preferences 上做最小改动：
// 1. 关掉 translate 气泡；
// 2. 将 webrtc.ip_handling_policy 设置为指定策略（杜绝非代理 UDP 泄漏）；
// 其余顶层字段原样保留，避免打乱已有用户数据。
func updateChromePreferences(data []byte, webrtcPolicy string) ([]byte, bool, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, false, err
	}
	changed := false

	// 1. 关掉翻译气泡
	needTranslate := true
	if raw, ok := top["translate"]; ok {
		var tr struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &tr); err == nil && tr.Enabled != nil && !*tr.Enabled {
			needTranslate = false
		}
	}
	if needTranslate {
		top["translate"] = json.RawMessage(`{"enabled":false}`)
		changed = true
	}

	// 2. 焊死 WebRTC UDP 策略
	if webrtcPolicy != "" {
		needWebRTC := true
		if raw, ok := top["webrtc"]; ok {
			var w struct {
				IPHandlingPolicy *string `json:"ip_handling_policy"`
			}
			if err := json.Unmarshal(raw, &w); err == nil && w.IPHandlingPolicy != nil && *w.IPHandlingPolicy == webrtcPolicy {
				needWebRTC = false
			}
		}
		if needWebRTC {
			wJSON, _ := json.Marshal(map[string]string{"ip_handling_policy": webrtcPolicy})
			top["webrtc"] = json.RawMessage(wJSON)
			changed = true
		}
	}

	if !changed {
		return data, false, nil
	}
	merged, err := json.Marshal(top)
	if err != nil {
		return nil, false, err
	}
	return merged, true, nil
}

// disableTranslatePref 保持兼容别名。
func disableTranslatePref(data []byte) ([]byte, bool, error) {
	return updateChromePreferences(data, "")
}

func replaceHeadless(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "--headless") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// resolveChrome 定位 Chrome 可执行文件。
func resolveChrome(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("cdp: chrome path %q not found", explicit)
	}
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("cdp: chrome not found (set ChromePath)")
}

// freePort 挑选一个空闲 TCP 端口。
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// waitForEndpoint 轮询 DevTools 端点直至就绪。
func waitForEndpoint(ctx context.Context, port int) (string, error) {
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		verCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		body, err := httpGet(verCtx, base+"/json/version")
		cancel()
		if err == nil {
			var ver struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			if json.Unmarshal(body, &ver) == nil && ver.WebSocketDebuggerURL != "" {
				return ver.WebSocketDebuggerURL, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// ── 持久身份 profile 的薄包装（2026-09-27 复杂度治理·阶段 1，单元 4）─────────────
//
// 纯逻辑已出包到 uws/pkg/browser/profiledir；这里保留同名入口，让 cmd/uws、pkg/qa 等
// 既有消费方零改动。新代码请直接用 profiledir。

// ProfileRootEnv 覆盖持久 profile 的根目录（测试、多环境部署用）。
const ProfileRootEnv = profiledir.RootEnv

// DefaultProfileRoot 返回持久 profile 根目录：~/.uws/profiles。
func DefaultProfileRoot() (string, error) { return profiledir.DefaultRoot() }

// ProfileDir 把身份名落成一个目录，并挡住会逃出根目录的名字。
func ProfileDir(root, id string) (string, error) { return profiledir.Dir(root, id) }

// ReapStaleTempProfiles 清理自起浏览器的临时 Profile 残留（口径见 profiledir.Reap）。
func ReapStaleTempProfiles(olderThan time.Duration) int { return profiledir.Reap(olderThan) }

// openProfile 为一次启动准备 user-data-dir，返回（目录，是否持久）。
func (f *CDPFactory) openProfile(cfg Config) (string, bool, error) {
	return profiledir.Prepare(f.ProfileRoot, cfg.ProfileID)
}
