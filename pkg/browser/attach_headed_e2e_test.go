package browser

import (
	"context"
	"os"
	"testing"
	"time"
)

// TDD-ENG-25（真机）：附着到真 Chrome 的会话必须被当成**有头**，并且拿到一扇可居前、
// 可激活的专属窗口 —— 这是「点小窗看真页面」在不变量层面的前提（2026-09-22 现场批次 #2）。
//
// 与 TestCDPSessionOwnWindowKeepsFocus 的关键差别：本用例用**引擎现场那套配置**
// （CDPFactory{Headless: true}，即守护进程默认），而不是测试里惯用的零值 false。
// 历史上正是这个差别让「附着=有头」在测试里成立、在现场不成立。
func TestAttachedHeadedChromeGetsItsOwnMovableWindow(t *testing.T) {
	if os.Getenv("UWS_RUN_CHROME_E2E") != "1" {
		t.Skip("未设置 UWS_RUN_CHROME_E2E=1，跳过会触碰真实 Chrome 的回归用例")
	}
	attachURL := os.Getenv("UWS_E2E_ATTACH_URL")
	if attachURL == "" {
		t.Skip("未设置 UWS_E2E_ATTACH_URL，跳过真机附着用例")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	f := &CDPFactory{Headless: true}
	sess, err := f.Create(ctx, Config{AttachCDPURL: attachURL})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sess.Close() }()
	s := sess.(*cdpSession)

	if s.headless {
		t.Fatalf("附着到真 Chrome 的会话被当成无头：windowPolicy 会退化成 tab，RaiseViewerWindow 会直接报 ErrNoViewerWindow")
	}
	if !s.ownWindow {
		t.Fatalf("有头会话应在自己的专属窗口里运行（ownWindow=true），当前 false")
	}
	if vis, err := s.evalResult(ctx, "(String(document.visibilityState))"); err != nil || vis != "visible" {
		t.Fatalf("专属窗口应可见（否则物理输入会挂起），visibilityState=%q err=%v", vis, err)
	}

	// 真窗口可抬到最前：现场「点小窗展开」走的正是这条路径（2026-09-22 新口径：只抬高，
	// 不搬位置、不缩放；窗口若整块停在屏外才会被救援搬进屏内）。
	bounds, err := s.RaiseViewerWindow(ctx)
	if err != nil {
		t.Fatalf("RaiseViewerWindow（抬到最前）失败：%v", err)
	}
	if bounds.Width <= 0 || bounds.Height <= 0 {
		t.Fatalf("RaiseViewerWindow 读回无效矩形：%+v", bounds)
	}
	t.Logf("EVIDENCE 抬到最前读回 left=%d top=%d %dx%d", bounds.Left, bounds.Top, bounds.Width, bounds.Height)

	// 不打扰用户：用例结束把窗口放回它原来的位置（没救援搬过就是空操作）。
	if err := s.RestoreViewerWindow(ctx); err != nil {
		t.Fatalf("RestoreViewerWindow: %v", err)
	}
}
