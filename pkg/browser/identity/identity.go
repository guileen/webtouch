// Package identity 是「身份应用与设备指标」域能力层（复杂度治理·阶段 3 第二刀）。
//
// 分层：编排层（`package browser` 的 cdpSession——`applyTargetIdentity` 解析身份优先级
// 并按序落到目标 session）→ 域能力层（本包：身份 hook 注入、档案屏幕指标、生效日志）
// → 原语（CDP 通道；设备指标原语见 viewport.go）。
//
// 本包只向下依赖，不持有会话状态：Worker 订阅句柄与 `cfg.Viewport` 的写回都由宿主落账
// （域函数返回结果，宿主记账）。
package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"uws/pkg/fingerprint"
)

// Session 是身份/设备指标域向宿主索取的会话能力面——逐项对应实测出边，不发明能力。
type Session interface {
	// Call 走 browser 级 CDP 通道（不带 target session）。
	Call(ctx context.Context, method string, params, out any) error
	// CallSession 走目标 session 的 CDP 通道。
	CallSession(ctx context.Context, sessionID, method string, params, out any) error
	// On 订阅事件；回调收到原始 params（宿主事件类型不出包），返回解除函数。
	On(sessionID, event string, fn func(params json.RawMessage)) func()
	// EvalValue 页内求值并把 JSON 结果解到 out。
	EvalValue(ctx context.Context, expr string, out any) error
}

// InstallWorkerHooks 把同一份 hook 注入 Dedicated/Service Worker（宿主
// `installWorkerFingerprintHooks` 的逐行搬移）。
// Page.addScriptToEvaluateOnNewDocument 只覆盖 frame，不覆盖 Worker；这里用
// Target auto-attach 在 Worker 源码执行前暂停、写入脚本、再恢复。事件回调不能在
// CDP reader goroutine 内同步等响应，否则 reader 自己无法路由响应，故另起 goroutine。
//
// 返回新的订阅解除函数（宿主落账到会话，本包不持有状态）：`prevCancel` 是上一次的
// 解除函数，非空则先解除。
func InstallWorkerHooks(ctx context.Context, s Session, sessionID, script string, prevCancel func()) (func(), error) {
	if script == "" {
		return prevCancel, nil
	}
	if prevCancel != nil {
		prevCancel()
	}
	// Target.setAutoAttach 的作用域是当前页面 target；因此事件也从主页面
	// session 发出，而不是 browser 根 session。注册在根 session 会静默漏掉 Worker。
	cancel := s.On(sessionID, "Target.attachedToTarget", func(params json.RawMessage) {
		var event struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				Type string `json:"type"`
			} `json:"targetInfo"`
		}
		if err := json.Unmarshal(params, &event); err != nil || event.SessionID == "" {
			return
		}
		go func() {
			workerCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if event.TargetInfo.Type == "worker" || event.TargetInfo.Type == "service_worker" {
				if err := s.CallSession(workerCtx, event.SessionID, "Runtime.evaluate", map[string]any{"expression": script}, nil); err != nil {
					log.Printf("browser: 注入 Worker 身份 Hook 失败: %v", err)
				}
			}
			// waitForDebuggerOnStart 会暂停所有自动附着的子 target；非 Worker 也必须
			// 恢复，避免 iframe / 未来 Chrome target 类型被意外卡死。
			_ = s.CallSession(workerCtx, event.SessionID, "Runtime.runIfWaitingForDebugger", nil, nil)
		}()
	})
	if err := s.CallSession(ctx, sessionID, "Target.setAutoAttach", map[string]any{
		"autoAttach":             true,
		"waitForDebuggerOnStart": true,
		"flatten":                true,
	}, nil); err != nil {
		cancel()
		return nil, fmt.Errorf("cdp: enable worker fingerprint auto-attach: %w", err)
	}
	return cancel, nil
}

// ApplyScreenMetrics 把身份档案的屏幕落到 Emulation.setDeviceMetricsOverride（宿主
// `applyFingerprintScreenMetrics` 的逐行搬移）。
//
// 视口取整块屏幕（与自动化需要的坐标空间一致：截图与点击坐标都活在这个空间里），
// 同时把 screen.* 一起覆盖成档案值——只覆盖窗口不覆盖屏幕，就会出现「档案说 1440×900、
// screen.width 却报真屏 1536」这种自己撞自己的矛盾。
//
// 代价与配套：页面里的 screen.avail* 会跟着变成档案里的假显示器，而观看壳「原地放大」
// 靠读它夹进工作区——所以展开前必须先摘掉覆盖（见 viewer_window.go 的顺序，那里有注释）。
// 档案里的 color_depth 只是记账用：CDP 没有覆盖色彩深度的接口，本 Phase 不假装能做到。
func ApplyScreenMetrics(ctx context.Context, s Session, sessionID string, fp *fingerprint.Profile) error {
	screen := fp.Emulation.Screen
	metrics := map[string]any{
		"width":             screen.Width,
		"height":            screen.Height,
		"deviceScaleFactor": screen.DeviceScaleFactor,
		"mobile":            fp.Emulation.UserAgentMetadata.Mobile,
		"screenWidth":       screen.Width,
		"screenHeight":      screen.Height,
	}
	return s.CallSession(ctx, sessionID, "Emulation.setDeviceMetricsOverride", metrics, nil)
}

// LogApplied 把「这份身份到底生效了什么」写进日志：真机复跑的取证、以及
// 「档案瞄的 Chrome 大版本和手上这只不一样」这种能被抓的矛盾，都得看得见
// （宿主 `logFingerprintApplied` 的逐行搬移）。
func LogApplied(ctx context.Context, s Session, fp *fingerprint.Profile) {
	log.Printf("browser: 身份档案 %q 生效 ua=%q platform=%s tz=%s lang=%s screen=%dx%d@%.1f",
		fp.ProfileID, fp.Emulation.UserAgent, fp.Emulation.UserAgentMetadata.Platform,
		fp.Emulation.Locale.TimezoneID, fp.PrimaryLanguage(),
		fp.Emulation.Screen.Width, fp.Emulation.Screen.Height, fp.Emulation.Screen.DeviceScaleFactor)
	if fp.NeedsHookScript() {
		log.Printf("browser: Phase 3 原型链 Hook 与确定性噪声已注入 (hw_concurrency=%d, device_mem=%g, canvas=%v, audio=%v, webgl=%v, seed=%d)",
			fp.Emulation.Hardware.HardwareConcurrency, fp.Emulation.Hardware.DeviceMemory,
			fp.NoiseFeatures.Canvas2D, fp.NoiseFeatures.AudioContext, fp.NoiseFeatures.WebGLBuffer, fp.Seed)
	}
	if missing := fp.MissingNoiseFeatures(); len(missing) > 0 {
		log.Printf("browser: 身份档案点名了 %v，但相关噪声注入尚未实现——**没有**生效，别当成已做", missing)
	}
	var version struct {
		Product string `json:"product"`
	}
	if err := s.Call(ctx, "Browser.getVersion", nil, &version); err != nil || version.Product == "" {
		return
	}
	if !fp.MatchesChromeVersion(version.Product) {
		log.Printf("browser: ⚠️ 身份档案瞄准 Chrome %s，实际是 %s —— UA/Client Hints 与真实内核版本对不上，能被抓；换同版本 Chrome for Testing",
			fp.TargetPlatform.ChromeVersion, version.Product)
	}
}
