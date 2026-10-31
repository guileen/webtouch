package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// runBus 处理 `uws bus` / `webtouch bus` 相关的总线管理与环境注入命令。
func runBus(args []string) error {
	if len(args) == 0 {
		return busStatus()
	}
	switch args[0] {
	case "status", "info":
		return busStatus()
	case "env":
		return busEnv(args[1:])
	case "help", "-h", "--help":
		fmt.Print(busGuide())
		return nil
	default:
		return fmt.Errorf("未知子命令 bus %s（支持 status | env | help）", args[0])
	}
}

func busGuide() string {
	return `uws bus —— WebTouch 浏览器执行服务管理

用法:
  uws bus status               查看浏览器运行时（Chrome 9222 / 引擎 50051 / WebTouch 服务 8000）与活跃会话
  uws bus env [--eval]         输出 Agent Hub 客户端模型 API 环境变量（支持 eval $(uws bus env)）

示例:
  eval $(uws bus env)          在当前 Shell 把 OpenAI SDK 指向 Agent Hub
`
}

// busEnv 输出 Agent Hub 的客户端模型 API 环境变量，供 shell eval 使用。
func busEnv(args []string) error {
	baseURL := os.Getenv("AHUB_MODELGW_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:18080/v1"
	}
	apiKey := os.Getenv("AHUB_GATEWAY_API_KEY")
	if apiKey == "" {
		apiKey = "agenthub-local"
	}

	fmt.Printf("export OPENAI_BASE_URL=%q\n", baseURL)
	fmt.Printf("export OPENAI_API_KEY=%q\n", apiKey)
	fmt.Printf("export WEBTOUCH_PROVIDER=%q\n", "webtouch")
	fmt.Printf("export WEBTOUCH_DEFAULT_MODEL=%q\n", "deepseek-chat")
	return nil
}

// probePort 快速检查本地 TCP 端口是否在监听。
func probePort(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// busStatus 输出当前三位一体运行拓扑的状态卡片。
func busStatus() error {
	fmt.Println("=== WebTouch 浏览器执行服务状态 ===")

	// 1. 探测 Chrome CDP 端口
	chromeAddr := "127.0.0.1:9222"
	chromeLive := probePort(chromeAddr, 300*time.Millisecond)
	if chromeLive {
		fmt.Printf("✓ 宿主 Chrome (CDP :9222)   : 运行中 (复用真实登录凭据)\n")
	} else {
		fmt.Printf("✗ 宿主 Chrome (CDP :9222)   : 未检测到监听 (需带 --remote-debugging-port=9222 启动)\n")
	}

	// 2. 探测 UWS 引擎端口
	engineAddr := "127.0.0.1:50051"
	engineLive := probePort(engineAddr, 300*time.Millisecond)
	if engineLive {
		fmt.Printf("✓ 物理执行引擎 (gRPC :50051) : 运行中\n")
	} else {
		fmt.Printf("✗ 物理执行引擎 (gRPC :50051) : 未检测到监听 (可运行 `uws daemon start`)\n")
	}

	// 3. 探测 HTTP 网关端口
	gatewayAddr := "127.0.0.1:8000"
	gatewayLive := probePort(gatewayAddr, 300*time.Millisecond)
	if gatewayLive {
		fmt.Printf("✓ 浏览器执行服务 (HTTP :8000) : 运行中 (WebTouch provider 就绪)\n")
		// 尝试获取活跃会话
		printGatewaySessions()
	} else {
		fmt.Printf("✗ 浏览器执行服务 (HTTP :8000) : 未检测到监听 (可运行 `./scripts/start_bus.sh` 一键拉起)\n")
	}

	fmt.Println("\n提示: 模型 API 由 Agent Hub 提供；可用 `uws bus env` 获取客户端地址。")
	return nil
}

func printGatewaySessions() {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get("http://127.0.0.1:8000/v1/sessions")
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	var res struct {
		Data []struct {
			SessionID string `json:"session_id"`
			Channel   string `json:"channel"`
			TargetID  string `json:"target_id"`
			Model     string `json:"model"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil || len(res.Data) == 0 {
		fmt.Println("  活跃会话: 当前无活跃 Tab 会话")
		return
	}

	fmt.Printf("  活跃会话 (%d 个):\n", len(res.Data))
	for _, s := range res.Data {
		targetBrief := s.TargetID
		if len(targetBrief) > 8 {
			targetBrief = targetBrief[:8] + "..."
		}
		fmt.Printf("    - 会话 [%s] 通道=%s 模型=%s Tab=%s\n", s.SessionID, s.Channel, s.Model, targetBrief)
	}
}
