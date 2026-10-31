package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"uws/pkg/browser/identity"
	"uws/pkg/browser/input"
	"uws/pkg/fingerprint"
)

// 身份/设备指标域适配（域逻辑在 `pkg/browser/identity`）：本文件只留「宿主注入 ＋
// 编排（身份解析并按序应用）＋ 状态落账」，域内不持有会话状态。形态同 `findBridge`
// （薄包装折叠进既有文件，不新开转发文件 ⇒ 条目数不增）。

// identityBridge 把宿主会话能力面暴露给身份域（`identity.Session` 的 4 项）。
type identityBridge struct{ s *cdpSession }

func (b identityBridge) Call(ctx context.Context, method string, params, out any) error {
	return b.s.client.Call(ctx, method, params, out)
}

func (b identityBridge) CallSession(ctx context.Context, sessionID, method string, params, out any) error {
	return b.s.client.CallSession(ctx, sessionID, method, params, out)
}

func (b identityBridge) On(sessionID, event string, fn func(params json.RawMessage)) func() {
	return b.s.client.On(sessionID, event, func(ev cdpEvent) { fn(ev.Params) })
}

func (b identityBridge) EvalValue(ctx context.Context, expr string, out any) error {
	return b.s.evalValue(ctx, expr, out)
}

// enableTargetDomains 在目标 session 上打开本会话依赖的 CDP 域，并启用「输入必达」
// 焦点仿真护栏。initTarget 与 SwitchTarget（新窗口接管）共用：新页若不带同一套域与
// 护栏，物理输入会因页面被判后台而挂起到超时。
func (s *cdpSession) enableTargetDomains(ctx context.Context, sessionID string) error {
	for _, m := range []string{"Runtime.enable", "Page.enable", "Network.enable"} {
		if err := s.client.CallSession(ctx, sessionID, m, nil, nil); err != nil {
			return fmt.Errorf("cdp: %s: %w", m, err)
		}
	}
	// 输入必达护栏 1/2（2026-09-12，GLOBAL #92）：专属小窗被用户窗口遮挡时，
	// Chrome 把页面当后台页处理，物理输入会挂起到超时。焦点仿真让渲染进程认为
	// 页面在焦点中——只改渲染层判定，不抬窗口、不动用户前台应用（守 R1）。
	// 失败不阻塞会话：少一层护栏，输入仍走下面的重试路径。
	if !s.headless && input.FocusEmulationEnabled() {
		_ = s.inputGuard().EnableFocusEmulation(ctx)
	}
	return nil
}

// applyTargetIdentity 把会话身份落到目标 session：UA(+Client Hints)/Accept-Language/
// 语言/时区、指纹 hook 脚本（含 Worker）、视口策略、探针预置、预置 Cookie。
// initTarget 与 SwitchTarget 共用——新窗口接管后新页若不带同一份身份，就会出现
// 「同一会话两个页面两套指纹」的当场穿帮（TDD-ENG-01 探活一致性）。
//
// 复杂度治理·阶段 3 第二刀：本方法是**编排层**（身份解析优先级 ＋ 按序应用），按
// §A.3 分层口径留宿主；机制面（Worker hook、屏幕指标、生效日志、设备指标原语、
// 窗口视口读取）下沉 `pkg/browser/identity`。
func (s *cdpSession) applyTargetIdentity(ctx context.Context, sessionID string) error {
	// 身份覆盖：UA(+ Client Hints) / Accept-Language / 语言 / 时区（TDD-ENG-01 探活一致性）。
	//
	// 优先级（2026-09-12 Phase 2）：**档案优先**。档案是身份的事实源，而 cfg.UserAgent /
	// cfg.Timezone 是任务级的零散设置（引擎还会给 Timezone 注入一个默认 "UTC"）——让零散
	// 设置盖掉档案，等于档案一进门就作废。没有档案时行为与以前完全一致。
	ua, acceptLanguage, locale := s.cfg.UserAgent, "", ""
	timezone := s.cfg.Timezone
	var userAgentMetadata *fingerprint.UserAgentMetadata
	if fp := s.cfg.Fingerprint; fp != nil {
		overrides := fp.Overrides()
		if overrides.UserAgent != "" {
			ua = overrides.UserAgent
		}
		acceptLanguage = overrides.AcceptLanguage
		locale = overrides.Locale
		userAgentMetadata = overrides.UserAgentMetadata
		if overrides.Timezone != "" {
			timezone = overrides.Timezone
		}
	}
	if ua != "" {
		params := map[string]any{"userAgent": ua}
		if acceptLanguage != "" {
			params["acceptLanguage"] = acceptLanguage
		}
		if userAgentMetadata != nil {
			// 不给 metadata，就会出现「UA 说 macOS 而 Client Hints 说 Windows」的穿帮
			// ——那正是档案校验在源头上挡的那类矛盾。
			params["userAgentMetadata"] = *userAgentMetadata
		}
		if err := s.client.CallSession(ctx, sessionID, "Emulation.setUserAgentOverride", params, nil); err != nil {
			return fmt.Errorf("cdp: set UA: %w", err)
		}
	}
	if locale != "" {
		if err := s.client.CallSession(ctx, sessionID, "Emulation.setLocaleOverride",
			map[string]any{"locale": locale}, nil); err != nil {
			return fmt.Errorf("cdp: set locale: %w", err)
		}
	}
	if timezone != "" {
		if err := s.client.CallSession(ctx, sessionID, "Emulation.setTimezoneOverride",
			map[string]any{"timezoneId": timezone}, nil); err != nil {
			return fmt.Errorf("cdp: set timezone: %w", err)
		}
	}
	if fp := s.cfg.Fingerprint; fp != nil {
		if fp.NeedsHookScript() {
			if hookScript := fp.BuildHookScript(); hookScript != "" {
				if err := s.client.CallSession(ctx, sessionID, "Page.addScriptToEvaluateOnNewDocument",
					map[string]any{"source": hookScript}, nil); err != nil {
					return fmt.Errorf("cdp: install fingerprint hook script: %w", err)
				}
				// 对初始化时已有的空白页/初始页立即评估一次
				_ = s.client.CallSession(ctx, sessionID, "Runtime.evaluate",
					map[string]any{"expression": hookScript}, nil)
				if err := installWorkerFingerprintHooks(ctx, s, sessionID, hookScript); err != nil {
					return err
				}
			}
		}
		logFingerprintApplied(ctx, s, fp)
	}
	// 视口策略（2026-09-03 修复 USER_ACTION #27 / GLOBAL #84）：
	//   - 启动模式（独立 Chrome，headless/可见）：按配置/默认 1920x1080 覆盖，
	//     保证 Page.captureScreenshot 渲染尺寸与坐标空间一致；
	//   - 附着模式（接管用户/调试窗口）未显式指定视口：**不**强设 1920x1080——
	//     先清除可能残留的设备指标覆盖，再读窗口实际 innerWidth/innerHeight 写入
	//     cfg.Viewport，页面宽度=窗口宽度（修复“页面比窗口宽且无横向滚动条”）；
	//   - 附着模式显式指定视口（移动模拟等任务级需求）：仍按配置覆盖。
	switch {
	case s.cfg.AttachCDPURL != "" && !s.cfg.ViewportExplicit && s.cfg.Fingerprint == nil:
		_ = s.client.CallSession(ctx, sessionID, "Emulation.clearDeviceMetricsOverride", nil, nil)
		if err := adoptWindowViewport(ctx, s); err != nil {
			return fmt.Errorf("cdp: adopt window viewport: %w", err)
		}
	case s.cfg.ViewportExplicit && s.cfg.Viewport != nil && s.cfg.Viewport.Width > 0 && s.cfg.Viewport.Height > 0:
		// 任务级显式视口（移动模拟等）压过档案：调用方当场要的就是这个坐标空间。
		if err := applyViewportMetrics(ctx, s, sessionID); err != nil {
			return fmt.Errorf("cdp: set viewport metrics: %w", err)
		}
	case s.cfg.Fingerprint != nil && s.cfg.Fingerprint.Emulation.Screen.Width > 0:
		if err := applyFingerprintScreenMetrics(ctx, s, sessionID); err != nil {
			return fmt.Errorf("cdp: set fingerprint screen metrics: %w", err)
		}
	case s.cfg.Viewport != nil && s.cfg.Viewport.Width > 0 && s.cfg.Viewport.Height > 0:
		if err := applyViewportMetrics(ctx, s, sessionID); err != nil {
			return fmt.Errorf("cdp: set viewport metrics: %w", err)
		}
	}
	// 探针常驻预实例化（TDD-OPT-01）：Page.addScriptToEvaluateOnNewDocument
	// 在每个新文档解析前执行，WASM 探针提前驻留 window.__uwsProbe，采集路径
	// 不再每次实例化；注册失败不阻塞（collectProbeCandidates 保留兜底实例化）。
	if b64, err := s.probeWasmBase64(); err == nil {
		_ = s.installPreProbeScript(ctx, b64)
	}
	// 预置 Cookie（TDD-ENG-09）。
	for _, c := range s.cfg.Cookies {
		s.setCookie(ctx, c)
	}
	return nil
}

// installWorkerFingerprintHooks 宿主侧簿记（域逻辑在 `identity.InstallWorkerHooks`）：
// `workerHookCancel` 归会话（close 路径依赖它解除浏览器级 Worker 自动附着监听），
// 域函数返回新句柄、此处落账 ⇒ 域内不持有会话状态。
func installWorkerFingerprintHooks(ctx context.Context, s *cdpSession, sessionID, script string) error {
	prev := s.workerHookCancel
	s.workerHookCancel = nil
	cancel, err := identity.InstallWorkerHooks(ctx, identityBridge{s}, sessionID, script, prev)
	if err != nil {
		return err
	}
	s.workerHookCancel = cancel
	return nil
}

// applyFingerprintScreenMetrics 宿主侧薄包装（域逻辑在 `identity.ApplyScreenMetrics`）。
func applyFingerprintScreenMetrics(ctx context.Context, s *cdpSession, sessionID string) error {
	return identity.ApplyScreenMetrics(ctx, identityBridge{s}, sessionID, s.cfg.Fingerprint)
}

// logFingerprintApplied 宿主侧薄包装（域逻辑在 `identity.LogApplied`）。
func logFingerprintApplied(ctx context.Context, s *cdpSession, fp *fingerprint.Profile) {
	identity.LogApplied(ctx, identityBridge{s}, fp)
}

// applyViewportMetrics 按 cfg.Viewport 应用 Emulation.setDeviceMetricsOverride
// （启动模式与附着模式的显式任务级视口共用）；设备指标原语在 `identity.SetDeviceMetrics`。
func applyViewportMetrics(ctx context.Context, s *cdpSession, sessionID string) error {
	vp := s.cfg.Viewport
	if vp == nil || vp.Width <= 0 || vp.Height <= 0 {
		return nil
	}
	scale := vp.DeviceScaleFactor
	if scale <= 0 {
		scale = 1
	}
	return identity.SetDeviceMetrics(ctx, identityBridge{s}, sessionID, vp.Width, vp.Height, scale, false)
}

// SetViewport 实现 ViewportSetter 接口，通过 CDP Emulation.setDeviceMetricsOverride 动态切换视口。
func (s *cdpSession) SetViewport(ctx context.Context, width, height int, scale float64, mobile bool) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("cdp: invalid viewport dimensions %dx%d", width, height)
	}
	if scale <= 0 {
		scale = 1
	}
	s.mu.Lock()
	sessionID := s.sessionID
	if s.cfg.Viewport == nil {
		s.cfg.Viewport = &Viewport{}
	}
	s.cfg.Viewport.Width = width
	s.cfg.Viewport.Height = height
	s.cfg.Viewport.DeviceScaleFactor = scale
	s.mu.Unlock()

	return identity.SetDeviceMetrics(ctx, identityBridge{s}, sessionID, width, height, scale, mobile)
}

// adoptWindowViewport 附着模式未显式指定视口时，读取页面实际布局视口
// （window.innerWidth/innerHeight，兜底 outerWidth/outerHeight）写入 cfg.Viewport，
// 使截图/坐标/裁剪都按真实窗口尺寸工作；读取在 `identity.AdoptWindowViewport`，写回归宿主。
func adoptWindowViewport(ctx context.Context, s *cdpSession) error {
	dsf := 1.0
	if s.cfg.Viewport != nil && s.cfg.Viewport.DeviceScaleFactor > 0 {
		dsf = s.cfg.Viewport.DeviceScaleFactor
	}
	w, h, err := identity.AdoptWindowViewport(ctx, identityBridge{s}, dsf)
	if err != nil {
		return err
	}
	s.cfg.Viewport = &Viewport{Width: w, Height: h, DeviceScaleFactor: dsf}
	return nil
}
