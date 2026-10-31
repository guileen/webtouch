// Command uws-gateway 是 WebTouch 浏览器 provider 执行服务入口（契约：
// docs/gateway-contract-2026-08-26.md）。
//
// 把 uws-engine（gRPC + CDP 网页接管）封装成 Agent Hub 可调用的浏览器执行契约；
// 保留站点管理端点及 Hub 专用 provider 执行契约，不提供客户端模型 API。默认仅监听 127.0.0.1。
//
// 分层边界：客户端模型 API 属 Agent Hub；本入口仅属 WebTouch 浏览器 provider 与站点身份执行层；
// 见 docs/README.md 与 docs/gateway-contract-2026-08-26.md。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"uws/apps/gateway"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8000", "HTTP 监听地址（默认仅本机）")
	channelsPath := flag.String("channels", "", "channels.json 路径；空 = 内置 deepseek-web/gemini-web")
	engineAddr := flag.String("engine-addr", "127.0.0.1:50051", "uws_web 通道默认引擎 gRPC 地址")
	attachCDP := flag.String("attach-cdp", "",
		"InitializeContext 附着的 CDP 调试端点（如 http://127.0.0.1:9222，共享已登录 Chrome；端点不可达时默认报错，设 UWS_ALLOW_CHROME_LAUNCH=1 才允许拉起；空 = 引擎自建隔离 Chrome）")
	chromeProfile := flag.String("chrome-profile", "",
		"自动拉起调试 Chrome 的 user-data-dir（默认 ~/.uws-chrome-debug；UWS_CHROME_PROFILE 可覆盖；已有运行实例时忽略）")
	apiKey := flag.String("api-key", "", "引擎 API Key（默认 uws_dev_key；可经 UWS_ENGINE_API_KEY 覆盖）")
	chatTimeout := flag.Duration("chat-timeout", gateway.DefaultChatTimeout, "单轮聊天总超时（等待流式回复结束）")
	videoTimeout := flag.Duration("video-timeout", gateway.DefaultVideoTimeout, "单轮视频生成总超时（Veo 等长任务）")
	authCheckTTL := flag.Duration("auth-check-ttl", 15*time.Minute, "已登录/未知状态复检间隔（默认 15 分钟；未登录/挑战需显式 auth reset）")
	flag.Parse()

	cfg := gateway.DefaultConfig(*engineAddr)
	if *channelsPath != "" {
		loaded, err := gateway.LoadConfig(*channelsPath)
		if err != nil {
			log.Fatalf("加载通道配置失败: %v", err)
		}
		cfg = *loaded
	}
	if err := cfg.Normalize(*engineAddr); err != nil {
		log.Fatalf("通道配置校验失败: %v", err)
	}

	key := *apiKey
	if key == "" {
		key = os.Getenv("UWS_ENGINE_API_KEY")
	}
	if key == "" {
		key = "uws_dev_key" // 引擎零配置开发内置 Key
	}

	logger := log.New(os.Stderr, "uws-gateway ", log.LstdFlags)
	if *attachCDP != "" {
		if *attachCDP == "none" {
			*attachCDP = ""
		} else {
			// 已就绪端点直接复用；不可达端点只在用户显式授权后才可拉起 Chrome。
			startupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			if err := gateway.EnsureChrome(startupCtx, *attachCDP, *chromeProfile, logger); err != nil {
				cancel()
				log.Fatalf("附着调试 Chrome 失败（请先手动启动端点，或设 UWS_ALLOW_CHROME_LAUNCH=1 显式授权启动；如需引擎自建隔离 Chrome 请指定 --attach-cdp=none）: %v", err)
			}
			cancel()
		}
	} else {
		// 未显式指定 --attach-cdp 时：宿主仍归网关管（2026-10-01「关窗 ≠ 断链路」）。
		// 已就绪 → 复用登录态；不可达但已授权拉起 → 纳管，由宿主守护循环自动拉起；
		// 不可达且未授权 → 退回引擎自建隔离 Chrome（保持旧行为，不致命）。
		const managedCDP = "http://127.0.0.1:9222"
		probeCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		probeErr := gateway.ProbeChrome(probeCtx, managedCDP)
		cancel()
		switch {
		case probeErr == nil:
			*attachCDP = managedCDP
			logger.Printf("探测到活跃调试 Chrome (%s)，自动复用登录态（纯净隔离模式请指定 --attach-cdp=none）", managedCDP)
		case gateway.ChromeLaunchAllowed():
			*attachCDP = managedCDP
			logger.Printf("调试 Chrome 不可达（%v），已纳管 %s：宿主守护循环将自动拉起（关窗 ≠ 断链路）", probeErr, managedCDP)
		default:
			logger.Printf("调试 Chrome 不可达（%v）且未授权拉起；退回引擎自建隔离 Chrome（设 %s=1 可让网关自愈宿主）",
				probeErr, gateway.ChromeLaunchPermissionEnv)
		}
	}
	gw, err := gateway.NewGateway(&cfg, gateway.Options{
		EngineAPIKey: key,
		AttachCDP:    *attachCDP,
		ChatTimeout:  *chatTimeout,
		VideoTimeout: *videoTimeout,
		AuthCheckTTL: *authCheckTTL,
		Logger:       logger,
	})
	if err != nil {
		log.Fatalf("初始化网关失败: %v", err)
	}
	// 会话生命周期（契约 §9）：空闲兜底回收「先删站点会话、再关 Tab」。
	// 阈值 UWS_SESSION_IDLE_TTL（默认 600s，<=0 关闭），回收周期 UWS_SESSION_REAP_SEC（默认 60s）。
	gw.StartSessionReaper(gateway.DefaultSessionReapInterval())
	// 浏览器宿主自愈（契约 §16）：周期探活 + 失联自动拉起；attach-cdp 为空时空操作。
	supervisorCtx, supervisorCancel := context.WithCancel(context.Background())
	defer supervisorCancel()
	gw.StartBrowserSupervisor(supervisorCtx)
	if st := gw.BrowserHostStatus(); st.Managed {
		logger.Printf("浏览器宿主纳管: %s（探活 %s）", st.Endpoint, gateway.BrowserHostIntervalFromEnv())
	}
	logger.Printf("会话空闲回收: ttl=%s reap=%s", gateway.DefaultSessionIdleTTL(), gateway.DefaultSessionReapInterval())
	logger.Printf("chat 超时预算: %s（--chat-timeout；整轮重试上限/退避/超时重试见 UWS_CHAT_RETRY_*）", *chatTimeout)
	// 启动回收：上次进程遗留的站点标签页（只关网关自己开过的页，见 tabreaper.go）。
	// 后台跑，不阻塞监听——回收失败只影响清理时效，不影响服务可用。
	go func() {
		reapCtx, reapCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer reapCancel()
		if n := gw.ReapOrphanTabs(reapCtx); n > 0 {
			logger.Printf("已回收上次遗留的站点标签页 %d 个", n)
		}
	}()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           gateway.NewHandler(gw, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		supervisorCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	logger.Printf("uws-gateway 已启动: http://%s (channels=%d, engine=%s, attach_cdp=%q)",
		*listen, len(cfg.Channels), *engineAddr, *attachCDP)
	logger.Printf("uws-gateway 构建标识: %s（make gateway-restart 可对齐运行版本）", gateway.BuildVersion)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
}
