// Command uws-viewer-demo 是「真实 Chrome → 原生观看小窗」的本地联调 / 真机取证驱动。
//
// 它走的是产品同一条链路的最小切片：browser.CDPFactory（附着或启动真实 Chrome）
// → Page.startScreencast 帧流 → viewerhub（Unix socket，JSON lines）→ Viewer 壳。
// 与 uws-engine 的区别只有一点：不需要 gRPC 协调器，便于单独验证观看链路，以及
// 「多会话共用 Profile、但各有各的 tab/窗口/面板」的隔离性。
//
// 用法：
//
//	uws-viewer-demo --attach http://127.0.0.1:9333 --url https://example.com
//	uws-viewer-demo --then https://www.iana.org/help/example-domains --seconds 40
//
// 该命令不属于自动化产品路径，只用于演示与取证。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"uws/pkg/browser"
	"uws/pkg/fingerprint"
	"uws/pkg/viewerhub"
)

func main() {
	attach := flag.String("attach", "", "附着已有 Chrome（如 http://127.0.0.1:9333）；空 = 启动一个真实 Chrome")
	chrome := flag.String("chrome", "", "启动模式 Chrome 可执行文件路径（空 = 自动探测）")
	headed := flag.Bool("headed", true, "启动模式使用有头 Chrome（观看器需要真实渲染）")
	pageURL := flag.String("url", "https://example.com", "首个导航地址")
	then := flag.String("then", "", "可选：4 秒后导航到的第二个地址（演示页面切换）")
	socket := flag.String("socket", defaultSocket(), "观看小窗 Unix socket 路径")
	seconds := flag.Int("seconds", 40, "保持推流的秒数（0 = 直到 Ctrl-C）")
	agent := flag.String("agent", "UWS demo", "观看器上显示的 Agent 名")
	profileID := flag.String("profile-id", "", "持久浏览器身份 id（空 = 临时目录用完即焚；指定后落 ~/.uws/profiles/<id>，同一 id 每次都是同一台设备）")
	fingerprintPath := flag.String("fingerprint", "", "身份档案 JSON 路径（Phase 2）；不给 = 用浏览器真实身份（UA/UA-CH/时区/语言/屏幕）")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := viewerhub.NewHub(*socket, viewerhub.WithLogger(log.Printf))
	go func() {
		if err := hub.Serve(ctx); err != nil {
			log.Printf("观看 socket 未启用: %v", err)
		}
	}()
	log.Printf("观看小窗 socket: %s", *socket)

	profile, err := loadFingerprint(*fingerprintPath, profileID)
	if err != nil {
		log.Fatalf("身份档案不可用: %v", err)
	}

	factory := &browser.CDPFactory{ChromePath: *chrome, Headless: !*headed}
	session, err := factory.Create(ctx, browser.Config{
		TaskID:       "viewer-demo",
		ProfileID:    *profileID,
		Fingerprint:  profile,
		AttachCDPURL: *attach,
		InitialURL:   *pageURL,
		Viewport:     &browser.Viewport{Width: 1280, Height: 800, DeviceScaleFactor: 1},
	})
	if err != nil {
		log.Fatalf("创建 CDP 会话失败: %v", err)
	}
	defer func() { _ = session.Close() }()

	source, ok := session.(browser.ScreencastSource)
	if !ok {
		log.Fatal("该会话不支持 Page.startScreencast")
	}
	if err := session.Navigate(ctx, *pageURL); err != nil {
		log.Printf("导航失败（继续展示当前页）: %v", err)
	}

	spec := func() viewerhub.SessionSpec {
		url := pageURLOf(session)
		return viewerhub.SessionSpec{
			ID: session.ID(), AgentName: *agent, PageTitle: hostOf(url), URL: url,
			BrowserPID: sessionBrowserPID(session),
		}
	}
	publisher, err := viewerhub.NewSession(hub, spec(), source, spec)
	if err != nil {
		log.Fatalf("创建观看会话失败: %v", err)
	}
	// 输入路径的指针观察者：引擎在真实点击/移动时报告坐标，壳自己画光标。
	if sink, ok := session.(browser.PointerObserverSink); ok {
		sink.SetPointerObserver(publisher)
	}
	// 反向意图（2026-09-12）：观看壳把「用户点了小窗 / 用户切走了」写回同一根
	// socket，引擎据此搬动这个会话自己的真窗口（展开 = 真页面，不是放大的图片）。
	// 意图不是自动化输入：这里只改窗口几何，不注入页面、不代替用户点击。
	hub.SetIntentHandler(func(in viewerhub.Intent) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res := viewerhub.ApplyWindowIntent(ctx, hub, session.ID(), session, in)
		log.Printf("观看壳意图: %s ok=%v reason=%q bounds=%+v viewer=%s",
			in.Intent, res.OK, res.Reason, res.WindowBounds, in.Reason)
	})

	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); publisher.Pump(runCtx) }()

	publisher.PublishAction(browser.ViewerAction{Kind: "navigate", Summary: "已打开 " + hostOf(*pageURL)})
	go demoInteractions(runCtx, session, *then)

	if *seconds > 0 {
		timer := time.NewTimer(time.Duration(*seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	} else {
		<-ctx.Done()
	}

	cancel()
	wg.Wait()
	publisher.Close()
	_ = hub.Close()
	log.Print("观看推流结束")
}

// loadFingerprint 读身份档案。档案自带身份名时顺带把持久 profile 目录也钉到同一个名字上
// ——「同一份身份每次都落同一个目录」是这套东西的第一条不变式，不该要求用户手抄两遍。
func loadFingerprint(path string, profileID *string) (*fingerprint.Profile, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	profile, err := fingerprint.Load(path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(*profileID) == "" {
		*profileID = profile.ProfileID
		log.Printf("身份档案 %q：持久 profile 目录沿用档案里的身份名", profile.ProfileID)
	} else if strings.TrimSpace(*profileID) != profile.ProfileID {
		return nil, fmt.Errorf("--profile-id %q 与 fingerprint.profile_id %q 不一致", *profileID, profile.ProfileID)
	}
	return profile, nil
}

// demoInteractions plays two obvious, harmless interactions so the shell shows
// the page changing and the virtual cursor pressing on a real page.
func demoInteractions(ctx context.Context, session browser.CDPSession, then string) {
	time.Sleep(4 * time.Second)
	if strings.TrimSpace(then) != "" && ctx.Err() == nil {
		if err := session.Navigate(ctx, then); err != nil {
			log.Printf("第二次导航失败: %v", err)
		}
	}
	time.Sleep(3 * time.Second)
	if clicker, ok := session.(interface {
		ClickAt(x, y int) (string, bool, string)
	}); ok && ctx.Err() == nil {
		if _, _, message := clicker.ClickAt(360, 220); message != "" {
			log.Printf("演示点击: %s", message)
		}
	}
}

// sessionBrowserPID 报告承载这个会话真页面的浏览器进程（附着模式为 0）。
func sessionBrowserPID(session browser.CDPSession) int {
	if info, ok := session.(browser.BrowserProcessInfo); ok {
		return info.BrowserPID()
	}
	return 0
}

func pageURLOf(session browser.CDPSession) string {
	if p, ok := session.(interface{ PageURL() string }); ok {
		if url := p.PageURL(); url != "" {
			return url
		}
	}
	return ""
}

func hostOf(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "about:blank"
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return raw
}

func defaultSocket() string {
	if v := strings.TrimSpace(os.Getenv("UWS_VIEWER_SOCKET")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "viewer.sock"
	}
	return home + "/.uws/viewer.sock"
}
