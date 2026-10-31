package viewerhub

import (
	"context"

	"uws/pkg/browser"
)

// ApplyWindowIntent 是反向边的共享实现：引擎（pkg/engine/viewer.go）与联调驱动
// （cmd/uws-viewer-demo）都调它，保证「点小窗看真页面」在任何宿主下都是同一套行为。
//
// 它只做一件事：把 target 这个会话自己的真窗口抬到最前（2026-09-22 口径：不搬位置、
// 不缩放；收回时把抬前为救援屏外位置而搬过的窗口放回原位），然后把结果作为回执发给
// 观看壳。它不注入页面、不转发点击、不动别的会话，也不在没有用户手势的情况下动任何窗口。
//
// 返回回执只是为了调用方记日志；回执已经发给了所有观看壳。
func ApplyWindowIntent(ctx context.Context, hub *Hub, sessionID string, target browser.CDPSession, in Intent) IntentResult {
	res := IntentResult{
		Version: IntentProtocolVersion,
		Kind:    IntentResultKind,
		Session: in.Session,
		Intent:  in.Intent,
	}
	if hub == nil {
		return res
	}
	if target == nil || in.Session != sessionID {
		res.Reason = "engine: unknown viewer session"
		hub.PublishIntentResult(res)
		return res
	}
	// close 没有可搬动的东西：壳侧的 ✕ 是壳自己已经生效的事（它真丢掉了那块面板），
	// 引擎只需要把会话静默回收。所以这里不需要 ViewerWindowController，无头/附着会话
	// 也一律 ok=true —— 否则壳会以为「关不掉」。
	if in.Intent == IntentClose {
		res.OK = true
		hub.PublishIntentResult(res)
		return res
	}
	controller, ok := target.(browser.ViewerWindowController)
	if !ok {
		// 无头会话、以及「附着到没有可搬动窗口的浏览器」都走这里。壳会回落到自绘
		// 放大面板：点开至少不落空。
		res.Reason = "session has no movable window (headless or attached without one)"
		hub.PublishIntentResult(res)
		return res
	}
	if info, ok := target.(browser.BrowserProcessInfo); ok {
		res.BrowserPID = info.BrowserPID()
	}
	switch in.Intent {
	case IntentExpand:
		// 2026-09-22 口径：展开 = 把这扇真窗口抬到最前，不再按壳给的尺寸搬窗。壳发来的
		// `Size` 仍留在协议里（老壳会发），引擎忽略它；回执带回真窗口的**实际**矩形，
		// 壳据此把小窗「飞向它」的动画演到正确位置。
		//
		// 2026-09-24 口径（§4.4 第 3 条）：新壳带 `deferRaise` —— 先把矩形回给它、让动画
		// 先飞，激活留到动画末段的 IntentRaise。老壳不带，就地抬（老行为不变）。
		var (
			applied browser.ViewerWindowBounds
			err     error
		)
		if in.DeferRaise {
			applied, err = controller.PrepareViewerWindow(ctx)
		} else {
			applied, err = controller.RaiseViewerWindow(ctx)
		}
		if err != nil {
			res.Reason = err.Error()
			break
		}
		res.OK = true
		res.WindowBounds = &ViewerBoundsJSON{
			Left:   applied.Left,
			Top:    applied.Top,
			Width:  applied.Width,
			Height: applied.Height,
		}
	case IntentRaise:
		// 动画末段：壳说「现在抬」。只激活，不搬位置、不缩放、不回矩形（几何已经在
		// expand 那一趟定格，第二趟再读一次只会让壳多等一个往返）。
		if err := controller.ActivateViewerWindow(ctx); err != nil {
			res.Reason = err.Error()
			break
		}
		res.OK = true
	case IntentCollapse:
		if err := controller.RestoreViewerWindow(ctx); err != nil {
			res.Reason = err.Error()
			break
		}
		res.OK = true
	}
	hub.PublishIntentResult(res)
	return res
}
