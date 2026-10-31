package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// runGateway 处理 `uws api` 与 `uws gateway` 命令，将所有流量与调用 100% 收口至 Go 实现的 uws-gateway。
func runGateway(args []string) error {
	if len(args) == 0 {
		// 默认启动网关或输出状态
		return launchGateway(nil)
	}

	switch args[0] {
	case "status", "info":
		return busStatus()
	case "help", "-h", "--help":
		fmt.Print(gatewayGuide())
		return nil
	case "start", "run":
		return launchGateway(args[1:])
	default:
		// 直接携带参数启动网关，如 uws api --listen 127.0.0.1:8000 或 --port 8000
		return launchGateway(args)
	}
}

func gatewayGuide() string {
	return `uws api / uws gateway —— WebTouch 浏览器 provider 执行服务 (Go 原生实施源)

用法:
  uws api [start|run] [选项...] 启动浏览器执行服务（默认 127.0.0.1:8000）
  uws api status               检查浏览器运行时与活跃会话
  uws api help                 查看此帮助信息

选项（兼容与透传给 cmd/uws-gateway）:
  --listen <addr>              HTTP 监听地址（默认 127.0.0.1:8000）
  --port <port>                HTTP 监听端口（自动归一化为 --listen）
  --engine-addr <addr>         uws-engine gRPC 监听地址（默认 127.0.0.1:50051）
  --attach-cdp <url>           附着的 Chrome CDP 调试端点（如 http://127.0.0.1:9222；空=自动探测）
  --api-key <key>              引擎鉴权 Key（默认 uws_dev_key 或环境变量）
  --chat-timeout <duration>    单轮聊天超时时间（默认 60s）

核心端点:
  POST /internal/provider/chat       Agent Hub 专用网页模型执行契约
  POST /internal/provider/responses  Agent Hub 专用 Responses 执行契约
  GET  /internal/provider/models     Agent Hub 查询可用网页模型
  POST /v1/chat/completions    已退役；客户端模型 API 请使用 Agent Hub
  POST /v1/responses           已退役；客户端模型 API 请使用 Agent Hub
  GET  /v1/models              已退役；客户端模型目录请使用 Agent Hub
  GET  /v1/sessions            查看当前在各 Provider 绑定的物理 Tab 会话
  GET  /v1/health              浏览器执行服务健康检查
`
}

// normalizeGatewayArgs 将旧版 CLI 参数（如 --port、--host）平滑转换为 uws-gateway 标准参数。
func normalizeGatewayArgs(args []string) []string {
	var normalized []string
	var host string = "127.0.0.1"
	var port string = ""

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--host" && i+1 < len(args) {
			host = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--host=") {
			host = strings.TrimPrefix(arg, "--host=")
			continue
		}
		if arg == "--port" && i+1 < len(args) {
			port = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--port=") {
			port = strings.TrimPrefix(arg, "--port=")
			continue
		}
		// 忽略 Python 历史无用参数 --target
		if arg == "--target" && i+1 < len(args) {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--target=") {
			continue
		}
		normalized = append(normalized, arg)
	}

	if port != "" {
		normalized = append(normalized, fmt.Sprintf("--listen=%s:%s", host, port))
	}
	return normalized
}

// launchGateway 查找并拉起 Go 实现的 uws-gateway 二进制。
func launchGateway(rawArgs []string) error {
	binPath, err := gatewayBinary()
	if err != nil {
		return err
	}

	forwardArgs := normalizeGatewayArgs(rawArgs)
	cmd := exec.Command(binPath, forwardArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if status.Signaled() {
					return nil // 响应中断信号退出
				}
			}
		}
		return fmt.Errorf("uws-gateway 运行异常: %w", err)
	}
	return nil
}

// gatewayBinary 定位 uws-gateway 二进制产物，若未编译则尝试自动编译。
func gatewayBinary() (string, error) {
	if v := strings.TrimSpace(os.Getenv("UWS_GATEWAY_BIN")); v != "" {
		if info, err := os.Stat(v); err == nil && !info.IsDir() {
			return v, nil
		}
		return "", fmt.Errorf("环境变量 UWS_GATEWAY_BIN 指向的文件不存在: %s", v)
	}

	roots := []string{}
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}

	for _, root := range roots {
		for dir := root; ; {
			for _, candidate := range []string{
				filepath.Join(dir, "bin", "uws-gateway"),
				filepath.Join(dir, "uws-gateway"),
			} {
				if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
					return candidate, nil
				}
			}
			// 检查是否到达源码根目录（包含 cmd/uws-gateway）
			gatewaySourceDir := filepath.Join(dir, "cmd", "uws-gateway")
			if info, err := os.Stat(gatewaySourceDir); err == nil && info.IsDir() {
				// 尝试自动构建
				targetBin := filepath.Join(dir, "bin", "uws-gateway")
				_ = os.MkdirAll(filepath.Dir(targetBin), 0755)
				buildCmd := exec.Command("go", "build", "-o", targetBin, "./cmd/uws-gateway")
				buildCmd.Dir = dir
				buildCmd.Stderr = os.Stderr
				if err := buildCmd.Run(); err == nil {
					return targetBin, nil
				}
			}

			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	return "", fmt.Errorf("未找到 uws-gateway 二进制文件；请先在项目根目录下执行 `make build` 或 `go build -o bin/uws-gateway ./cmd/uws-gateway`")
}
