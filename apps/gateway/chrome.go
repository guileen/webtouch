// 调试 Chrome 自动拉起（2026-08-26 用户要求「不再手动 attach」）。
//
// uws-gateway 启动时若 --attach-cdp 指定端点不可达，自动拉起持久调试
// Chrome（--remote-debugging-port=<port> --remote-allow-origins=*，
// user-data-dir 默认 ~/.uws-chrome-debug，与旧 scripts/start_*_attached.sh
// 同一持久 profile，登录态复用）。桌面 sidecar 与 CLI 均无需再手动开 Chrome。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultChromeProfile 是持久调试 Chrome 的默认 user-data-dir（与旧脚本一致）。
	DefaultChromeProfile = ".uws-chrome-debug"
	chromeReadyTimeout   = 15 * time.Second
	chromeProbeTimeout   = 1200 * time.Millisecond
	// ChromeLaunchPermissionEnv 必须显式为真，才允许端点不可达时启动 Chrome。
	ChromeLaunchPermissionEnv = "UWS_ALLOW_CHROME_LAUNCH"
)

var startChromeProcess = func(bin string, args []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release() // 网关退出不带走 Chrome（与旧脚本 nohup 语义一致）
}

// ChromeLaunchAllowed 报告本次是否授权自动拉起 Chrome（网关自愈宿主用；默认 fail-closed）。
func ChromeLaunchAllowed() bool { return chromeLaunchAllowed() }

func chromeLaunchAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ChromeLaunchPermissionEnv))) {
	case "1", "yes", "true", "y":
		return true
	default:
		return false
	}
}

func chromeLaunchPermissionError(attachCDP string) error {
	return fmt.Errorf(
		"CDP 调试端点不可达：%s。为避免打断当前工作，UWS 默认不会自动启动 Chrome；请先手动启动并附着该端点，或在明确授权本次启动时设 %s=1 后重试",
		attachCDP, ChromeLaunchPermissionEnv,
	)
}

// chromeBin 返回可用 Chrome/Chromium 可执行文件（UWS_CHROME_BIN 可覆盖）。
func chromeBin() string {
	if p := os.Getenv("UWS_CHROME_BIN"); p != "" {
		return p
	}
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// chromeWindowFlags 返回冷启动调试 Chrome 的小窗参数（UWS_WINDOW_SIZE=WxH；
// UWS_WINDOW_POS=X,Y）——避免首次拉起就铺满屏幕压住用户窗口（2026-09-11，
// 与 coordinator/attach.py window_flags 同口径）。
func chromeWindowFlags() []string {
	size := strings.TrimSpace(os.Getenv("UWS_WINDOW_SIZE"))
	if size == "" {
		size = "1440x900"
	}
	size = strings.NewReplacer("X", "x", "x", ",", "*", ",").Replace(size)
	flags := []string{"--window-size=" + size}
	switch pos := strings.ToLower(strings.TrimSpace(os.Getenv("UWS_WINDOW_POS"))); pos {
	case "", "auto", "none", "keep":
	default:
		flags = append(flags, "--window-position="+pos)
	}
	return flags
}

// chromeAntiThrottleFlags 是反节流启动参数（2026-09-12，GLOBAL #92）：调试 Chrome
// 常驻后台或被用户窗口遮挡时会被节流，CDP 物理输入会挂起到超时。只对**新启动**的
// 进程生效（已在跑的实例须重启）。
var chromeAntiThrottleFlags = []string{
	"--disable-backgrounding-occluded-windows",
	"--disable-renderer-backgrounding",
	"--disable-background-timer-throttling",
}

// chromeLaunchArgs 构造持久调试 Chrome 启动参数（纯函数，便于单测）。
func chromeLaunchArgs(port, profileDir, initialURL string) []string {
	args := []string{
		"--remote-debugging-port=" + port,
		"--remote-allow-origins=*",
		"--user-data-dir=" + profileDir,
		"--no-first-run",
		"--no-default-browser-check",
	}
	args = append(args, chromeAntiThrottleFlags...)
	args = append(args, chromeWindowFlags()...)
	if strings.TrimSpace(initialURL) == "" {
		initialURL = "about:blank"
	}
	return append(args, initialURL)
}

// ProbeChrome 探测 CDP HTTP 端点是否就绪（/json/version 且带 webSocketDebuggerUrl）。
func ProbeChrome(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: chromeProbeTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/json/version", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cdp http %d", resp.StatusCode)
	}
	var ver struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ver); err != nil {
		return err
	}
	if ver.WebSocketDebuggerURL == "" {
		return fmt.Errorf("cdp /json/version 无 webSocketDebuggerUrl")
	}
	return nil
}

func probeChrome(ctx context.Context, baseURL string) error {
	return ProbeChrome(ctx, baseURL)
}

// EnsureChrome 保证 attachCDP 指向的调试 Chrome 在运行：
//   - attachCDP 为空（引擎自建隔离 Chrome）→ 直接返回；
//   - 端点已就绪 → 复用（持久 profile 登录态）；
//   - 未就绪且 UWS_ALLOW_CHROME_LAUNCH=1 → 拉起持久调试 Chrome 并等待就绪；
//   - 未就绪且未授权 → fail-closed，不启动任何 Chrome 进程。
//
// 返回错误时调用方应中止启动，并按错误提示手动启动或显式授权。
func EnsureChrome(ctx context.Context, attachCDP, profileDir string, logger *log.Logger) error {
	if strings.TrimSpace(attachCDP) == "" {
		return nil
	}
	if logger == nil {
		logger = log.Default()
	}
	u, err := url.Parse(attachCDP)
	if err != nil {
		return fmt.Errorf("attach-cdp 非法: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("attach-cdp 缺少主机（应为 http://127.0.0.1:<port>）: %s", attachCDP)
	}
	port := u.Port()
	if port == "" {
		return fmt.Errorf("attach-cdp 缺少调试端口（应为 http://127.0.0.1:<port>）: %s", attachCDP)
	}
	if err := probeChrome(ctx, attachCDP); err == nil {
		logger.Printf("调试 Chrome 已在运行（%s），直接复用", attachCDP)
		return nil
	}
	if !chromeLaunchAllowed() {
		return chromeLaunchPermissionError(attachCDP)
	}
	if profileDir == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return fmt.Errorf("解析用户主目录失败: %w", herr)
		}
		profileDir = filepath.Join(home, DefaultChromeProfile)
	}
	bin := chromeBin()
	if bin == "" {
		return fmt.Errorf("未找到 Chrome/Chromium（可设 UWS_CHROME_BIN 指定路径），无法自动拉起调试浏览器")
	}
	if err := startChromeProcess(bin, chromeLaunchArgs(port, profileDir, "")); err != nil {
		return fmt.Errorf("启动调试 Chrome 失败: %w", err)
	}
	logger.Printf("已自动拉起调试 Chrome: %s（profile=%s，可用 UWS_CHROME_PROFILE/UWS_CHROME_BIN 覆盖）",
		attachCDP, profileDir)

	deadline := time.Now().Add(chromeReadyTimeout)
	for time.Now().Before(deadline) {
		if err := probeChrome(ctx, attachCDP); err == nil {
			logger.Printf("调试 Chrome 已就绪: %s", attachCDP)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("调试 Chrome 启动后 %v 内未就绪（%s）", chromeReadyTimeout, attachCDP)
}
