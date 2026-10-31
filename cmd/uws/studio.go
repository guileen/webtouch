package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"uws/pkg/webcontrol"
)

func runStudio(args []string) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Print(studioHelp())
		return nil
	}

	fs := flag.NewFlagSet("studio", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	defaultPort := 8080
	if envPort := os.Getenv("UWS_STUDIO_PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			defaultPort = p
		}
	}

	var (
		port       int
		engineAddr string
	)

	fs.IntVar(&port, "port", defaultPort, "Web 控制台监听端口 (默认 8080)")
	fs.StringVar(&engineAddr, "engine-addr", "", "UWS Engine gRPC 地址 (可选)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	addr := fmt.Sprintf(":%d", port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("bind port %d: %w", port, err)
	}

	opts := webcontrol.Options{
		Addr:            addr,
		EngineConnected: false,
		Logf: func(format string, v ...any) {
			fmt.Printf("[studio] "+format+"\n", v...)
		},
	}

	srv, err := webcontrol.NewServer(opts)
	if err != nil {
		_ = l.Close()
		return fmt.Errorf("create studio server: %w", err)
	}

	actualPort := l.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://127.0.0.1:%d", actualPort)

	fmt.Println()
	fmt.Printf("%s%s⚡ WebTouch Studio 云端交互控制台与流式伴生视窗已启动%s\n", ansiBold, ansiCyan, ansiReset)
	fmt.Printf("   控制看板地址: %s%s%s\n", ansiGreen, url, ansiReset)
	fmt.Printf("   流式伴生视窗: %s%s/api/viewer/stream%s (WebSocket)\n", ansiYellow, url, ansiReset)
	fmt.Printf("   系统健康检查: %s%s/api/status%s\n", ansiGray, url, ansiReset)
	fmt.Println()
	fmt.Printf("按 %sCtrl+C%s 退出控制台服务。\n\n", ansiBold, ansiReset)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(l)
	}()

	select {
	case <-sigCh:
		fmt.Println("\n正在关闭 WebTouch Studio...")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		return nil
	case err := <-errCh:
		return err
	}
}

func studioHelp() string {
	return `用法: webtouch studio [flags]

启动 WebTouch 云端 Web 交互控制面与流式远程伴生视窗服务。
提供交付看板（Figma 级 Diff、横向溢出瀑布流）与低延迟 WebSocket 流式画布逆向交互接管。

选项:
  --port <int>         Web 端口（默认 8080，支持 UWS_STUDIO_PORT）
  --engine-addr <host:port>
                       指定 UWS Engine gRPC 调度地址（默认自动探测）

示例:
  webtouch studio                       # 默认在 http://127.0.0.1:8080 启动控制台
  webtouch studio --port 9090           # 指定端口 9090
`
}
