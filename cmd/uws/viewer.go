package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// webtouch viewer —— 观看小窗（P2，2026-09-12）。
//
// 拓扑：引擎是 Unix socket 服务端（uws-engine --viewer-socket 或 UWS_VIEWER_SOCKET），
// 原生 Viewer 是客户端。`webtouch viewer status` 用一次 dial 判断引擎是否在推流；
// `webtouch viewer app` 拉起本机已构建的 Viewer 壳，并把同一个 socket 路径传给它。
//
// 该命令不驱动浏览器、不改变引擎状态：它只是「看一眼有没有人在推流 / 把观看器打开」。
//
// 2026-09-15 用户定案（默认体验）：小窗是常驻的，引擎**默认就开**推流
// （`~/.uws/viewer.sock`），不再需要显式开关；观看壳单实例，重复拉起只复用不叠窗。

// viewerSocketPath 解析观看小窗 socket 路径（默认启用）：
//
//	UWS_VIEWER_SOCKET 未设        → ~/.uws/viewer.sock
//	UWS_VIEWER_SOCKET=<路径>      → 用该路径
//	UWS_VIEWER_SOCKET=off/空/none/no/false/- → 显式关闭推流（返回 ""）
func viewerSocketPath() string {
	if v, ok := os.LookupEnv("UWS_VIEWER_SOCKET"); ok {
		if viewerSocketDisabled(v) {
			return ""
		}
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uws/viewer.sock"
	}
	return filepath.Join(home, ".uws", "viewer.sock")
}

// viewerSocketDisabled 识别「显式关闭推流」的哨兵值。
func viewerSocketDisabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off", "none", "no", "false", "-":
		return true
	default:
		return false
	}
}

// viewerShellLogPath 是常驻观看壳的日志路径（与 socket 同目录）。
func viewerShellLogPath(socketPath string) string {
	return filepath.Join(filepath.Dir(socketPath), "viewer.log")
}

// viewerShellLockPath 是观看壳的单实例锁路径（与 socket 同目录，锁由壳进程持有）。
func viewerShellLockPath(socketPath string) string {
	return filepath.Join(filepath.Dir(socketPath), "viewer.lock")
}

// viewerShellRunning 用与观看壳相同的 flock 约定判断壳是否已在运行。
func viewerShellRunning(socketPath string) bool {
	lockPath := viewerShellLockPath(socketPath)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true // 锁被别人持有 = 壳在跑
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func runViewer(args []string) error {
	if len(args) == 0 {
		return viewerStatus(viewerSocketPath())
	}
	switch args[0] {
	case "status":
		path := viewerSocketPath()
		if len(args) > 1 {
			path = args[1]
		}
		return viewerStatus(path)
	case "app", "open", "run":
		path := viewerSocketPath()
		for i := 1; i < len(args); i++ {
			if args[i] == "--socket" && i+1 < len(args) {
				path = args[i+1]
				i++
			}
		}
		return viewerApp(path)
	case "help", "-h", "--help":
		fmt.Print(viewerGuide())
		return nil
	default:
		return fmt.Errorf("未知子命令 viewer %s（支持 status | app）", args[0])
	}
}

func viewerGuide() string {
	return `webtouch viewer —— 观看小窗

  webtouch viewer status [socket]   查看引擎是否在推流（默认 ~/.uws/viewer.sock）
  webtouch viewer app [--socket P]  启动原生 Viewer 壳（单实例：已在跑则复用；
                               UWS_VIEWER_BIN / cmd/viewer/.build 下查找）

引擎侧默认推流（2026-09-15 起）：webtouch daemon 启动引擎时默认带
--viewer-socket ~/.uws/viewer.sock；关闭推流设 UWS_VIEWER_SOCKET=off。
`
}

func viewerStatus(path string) error {
	if strings.TrimSpace(path) == "" {
		fmt.Println("观看小窗推流：已显式关闭（UWS_VIEWER_SOCKET=off）。")
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		fmt.Printf("观看小窗 socket: %s（不存在）\n", path)
		fmt.Println("该 socket 不存在：引擎未在跑（或已用 UWS_VIEWER_SOCKET=off 关闭推流）。")
		return nil
	}
	conn, dialErr := net.DialTimeout("unix", path, 500*time.Millisecond)
	live := dialErr == nil
	if live {
		_ = conn.Close()
	}
	state := "未监听（陈旧 socket 文件）"
	if live {
		state = "监听中"
	}
	fmt.Printf("观看小窗 socket: %s（%s，mode=%s，size=%d）\n", path, state, info.Mode().Perm(), info.Size())
	if viewerShellRunning(path) {
		fmt.Println("观看壳：已在运行（单实例）。")
	} else if live {
		fmt.Println("观看壳未运行；`webtouch viewer app` 打开原生观看器。")
	}
	return nil
}

// viewerApp 拉起常驻的原生观看壳。The viewer is a Swift package built outside the
// Go build; we look in the documented places instead of shipping a second copy.
//
// 常驻语义（2026-09-15）：壳是**独立会话**的常驻进程——Agent 的任务 shell 结束、
// 终端关闭都不该把用户屏幕上的小窗一起带走（实测 `nohup ... &` 仍会随调用方进程组
// 清理而死）。所以这里 setsid 脱离会话、Release 不回收，日志落 ~/.uws/viewer.log；
// 单实例锁保证重复调用只复用。
func viewerApp(path string) error {
	if viewerShellRunning(path) {
		fmt.Printf("观看壳已在运行（单实例锁 %s）；本次不重复拉起。\n", viewerShellLockPath(path))
		return nil
	}
	binary, err := viewerBinary()
	if err != nil {
		return err
	}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "UWS_VIEWER_SOCKET="+path, "UWS_VIEWER_MODE=socket")
	logPath := viewerShellLogPath(path)
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
	} else {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动观看器失败: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // 壳的生命周期与调用者无关（Release 后 Pid 读回 -1，故先取）
	fmt.Printf("观看壳已启动（常驻，pid=%d，日志 %s）：%s（socket=%s）\n",
		pid, logPath, binary, path)
	return nil
}

func viewerBinary() (string, error) {
	if v := strings.TrimSpace(os.Getenv("UWS_VIEWER_BIN")); v != "" {
		if info, err := os.Stat(v); err == nil && !info.IsDir() {
			return v, nil
		}
		return "", fmt.Errorf("UWS_VIEWER_BIN 指向的文件不存在: %s", v)
	}
	roots := []string{}
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	// 允许在仓库任意子目录执行：向上找 cmd/viewer 的构建产物。
	for _, root := range roots {
		for dir := root; ; {
			for _, candidate := range []string{
				filepath.Join(dir, "cmd", "viewer", ".build", "release", "uws-viewer-prototype"),
				filepath.Join(dir, "cmd", "viewer", ".build", "debug", "uws-viewer-prototype"),
				filepath.Join(dir, "uws-viewer-prototype"),
			} {
				if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
					return candidate, nil
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("未找到观看器可执行文件；先 `swift build --package-path cmd/viewer`，或用 UWS_VIEWER_BIN 指定")
}
