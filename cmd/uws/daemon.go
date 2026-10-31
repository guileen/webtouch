package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"uws/pkg/engine"
)

// webtouch daemon 是引擎（cmd/uws-engine）的生命周期管理器。
// 用户/Agent 通过 `webtouch daemon start|stop|restart|status` 直接控制引擎；
// `webtouch run/crawl/api` 等子命令在真正执行前会先 ensureEngine（按需拉起）。
// 引擎以 Setsid 后台进程启动（不随 CLI 退出被 SIGHUP 杀），pid/元数据/日志落在
// ~/.uws/（可用 UWS_ENGINE_DIR 覆盖），地址默认 127.0.0.1:50051（UWS_ENGINE_ADDR 可覆盖）。

const daemonDefaultAddr = "127.0.0.1:50051"

// semverOf 返回去掉「+<sha>」后缀的语义版本，用于「是否漂移」判断：
// 仅当语义主版本（如 v1.5.4 vs v1.5.5）不同才算漂移，+sha 的构建指纹差异不算。
func semverOf(v string) string {
	if i := strings.IndexByte(v, '+'); i >= 0 {
		return v[:i]
	}
	return v
}

func versionDrifted(running, built string) bool {
	if running == "" || built == "" {
		return false
	}
	return semverOf(running) != semverOf(built)
}

// daemonMeta 记录被 webtouch CLI 托管的引擎进程元数据（pidfile 的伴生 JSON）。
type daemonMeta struct {
	PID           int    `json:"pid"`
	Addr          string `json:"addr"`
	Binary        string `json:"binary"`
	EngineVersion string `json:"engine_version,omitempty"`
	StartedAt     string `json:"started_at"`
}

// firstEnv 返回第一个非空环境变量。新名优先，旧名 UWS_* 仍有效。
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func daemonAddr() string {
	if v := firstEnv("WEBTOUCH_ENGINE_ADDR", "UWS_ENGINE_ADDR"); v != "" {
		return v
	}
	return daemonDefaultAddr
}

func daemonStateDir() string {
	// 默认目录仍是 ~/.uws。WEBTOUCH_STATE_DIR 只在显式设置时覆盖，避免静默拆成两套状态。
	if v := firstEnv("WEBTOUCH_STATE_DIR", "UWS_ENGINE_DIR"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".uws")
	}
	return ".uws"
}

func pidFilePath() string  { return filepath.Join(daemonStateDir(), "engine.pid") }
func metaFilePath() string { return filepath.Join(daemonStateDir(), "engine.json") }
func engineLogPath() string {
	if v := os.Getenv("UWS_ENGINE_LOG"); v != "" {
		return v
	}
	return filepath.Join(daemonStateDir(), "engine.log")
}

// engineBinary 解析引擎可执行文件：
// WEBTOUCH_ENGINE_BIN > UWS_ENGINE_BIN > 可执行文件旁的 webtouch-engine > uws-engine > PATH。
func engineBinary() (string, error) {
	if v := firstEnv("WEBTOUCH_ENGINE_BIN", "UWS_ENGINE_BIN"); v != "" {
		if info, err := os.Stat(v); err == nil && !info.IsDir() {
			return v, nil
		}
		return "", fmt.Errorf("WEBTOUCH_ENGINE_BIN/UWS_ENGINE_BIN 指向的文件不存在: %s", v)
	}
	seen := map[string]bool{}
	for _, cand := range engineBinarySearchList() {
		if seen[cand] {
			continue
		}
		seen[cand] = true
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand, nil
		}
	}
	for _, name := range []string{"webtouch-engine", "uws-engine"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("找不到 webtouch-engine（兼容名 uws-engine）：请设置 WEBTOUCH_ENGINE_BIN 或 UWS_ENGINE_BIN，或执行 make build")
}

// engineBinaryCandidates 给出某个可执行文件目录下的查找顺序：同目录正式名、同目录兼容名，再向上一层 bin/。
func engineBinaryCandidates(exeDir string) []string {
	if exeDir == "" {
		return nil
	}
	names := []string{"webtouch-engine", "uws-engine"}
	dirs := []string{exeDir, filepath.Join(exeDir, "bin")}
	parent := filepath.Dir(exeDir)
	if parent != exeDir {
		dirs = append(dirs, filepath.Join(parent, "bin"))
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range dirs {
		for _, name := range names {
			cand := filepath.Join(dir, name)
			if seen[cand] {
				continue
			}
			seen[cand] = true
			out = append(out, cand)
		}
	}
	return out
}

func engineBinarySearchList() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			dirs = append(dirs, filepath.Dir(resolved))
		}
	}
	var out []string
	seenDir := map[string]bool{}
	for _, dir := range dirs {
		if dir == "" || seenDir[dir] {
			continue
		}
		seenDir[dir] = true
		out = append(out, engineBinaryCandidates(dir)...)
	}
	return out
}

// engineArgs 返回引擎启动参数（真实 Chrome 模式 + 在线日志落库 + 观看小窗推流；
// 可用 UWS_ENGINE_ARGS 追加覆盖，追加项在 flag 包语义下后出现者生效）。
//
// 观看小窗默认开（2026-09-15 用户定案：小窗常驻，真窗口默认不出现在屏幕上）：
// 引擎默认监听 ~/.uws/viewer.sock 供原生观看壳订阅；UWS_VIEWER_SOCKET=off 显式关闭。
func engineArgs(addr string) ([]string, error) {
	args := []string{
		"--browser", "cdp",
		"--grpc-addr", addr,
		"--log-path", filepath.Join(daemonStateDir(), "engine-onlinelogs.db"),
	}
	// Episode 采集默认开（algorithm-kernel.md §5.0.12，T-22「调试即采样」）：
	// 本机 append-only JSONL，裁剪 + 脱敏后落盘，不入交付仓；UWS_EPISODE_DIR=off 关闭。
	if dir := episodeDir(); dir != "" {
		args = append(args, "--episode-dir", dir)
	}
	if socket := viewerSocketPath(); socket != "" {
		args = append(args, "--viewer-socket", socket)
	}
	// learned 自进化默认开（2026-09-30 用户「自动学习是对的」＋ §5.0.9 条款 6 的落点
	// 自证写入门禁已落地 ⇒ 学到的定位可信）：学习存储落 <stateDir>/learned.json；
	// UWS_LEARN=off 关闭，其他非空值可覆盖路径（测试隔离用）。
	if p := learnedPath(); p != "" {
		args = append(args, "--vlm-learn-path", p)
	}
	if v := os.Getenv("UWS_ENGINE_ARGS"); v != "" {
		args = append(args, strings.Fields(v)...)
	}
	return args, nil
}

func portOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func readMeta() (daemonMeta, error) {
	var meta daemonMeta
	data, err := os.ReadFile(metaFilePath())
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

func writeMeta(meta daemonMeta) error {
	dir := daemonStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(metaFilePath(), data, 0o600); err != nil {
		return err
	}
	return os.WriteFile(pidFilePath(), []byte(strconv.Itoa(meta.PID)+"\n"), 0o600)
}

// loadRunningPID 读取当前托管引擎 pid（优先 meta，回退 pidfile）。
func loadRunningPID() (int, error) {
	if meta, err := readMeta(); err == nil && meta.PID > 0 {
		return meta.PID, nil
	}
	data, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, errors.New("引擎未运行（无 pid/元数据）")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, errors.New("引擎 pid 文件无效")
	}
	return pid, nil
}

func clearDaemonState() {
	_ = os.Remove(pidFilePath())
	_ = os.Remove(metaFilePath())
}

func killByPID(pid int) error {
	if pid <= 0 || !pidAlive(pid) {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	for i := 0; i < 20 && pidAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(100 * time.Millisecond)
	}
	clearDaemonState()
	return nil
}

func startEngine(addr string) error {
	if portOpen(addr) {
		if meta, err := readMeta(); err == nil && meta.PID > 0 {
			return fmt.Errorf("引擎已在 %s 运行（pid=%d）", addr, meta.PID)
		}
		// 端口被占但 meta 缺失：可能是上次会话残留的孤儿/外部启动的引擎。
		if ver := healthVersion(addr); ver != "" {
			return fmt.Errorf("检测到未托管的 UWS 引擎运行于 %s（version=%s，可能是孤儿进程）。"+
				"请先 `webtouch daemon stop` 或 `webtouch daemon restart` 回收后再启动", addr, ver)
		}
		return fmt.Errorf("端口 %s 已被非 UWS 进程占用", addr)
	}
	// 清理残留（进程已死或端口未通）：若 meta 里 pid 仍存活但端口不通，先杀掉再重启。
	if pid, err := loadRunningPID(); err == nil && pidAlive(pid) {
		_ = killByPID(pid)
	} else {
		clearDaemonState()
	}

	bin, err := engineBinary()
	if err != nil {
		return err
	}
	args, err := engineArgs(addr)
	if err != nil {
		return err
	}
	logPath := engineLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开引擎日志失败: %w", err)
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("启动引擎失败: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // 交给 init 收养，CLI 退出不影响引擎
	_ = logFile.Close()

	meta := daemonMeta{
		PID: pid, Addr: addr, Binary: bin,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	if err := writeMeta(meta); err != nil {
		fmt.Fprintf(os.Stderr, "webtouch: 写引擎元数据失败（忽略，进程已启动）: %v\n", err)
	}

	// 轮询等待监听成功（冷启动/首次构建可能 >400ms）；进程退出则快速失败。
	waitDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(waitDeadline) {
		if portOpen(addr) {
			meta.EngineVersion = healthVersion(addr)
			if meta.EngineVersion == "" {
				// 读不到版本（如自定义 key file）；用当前构建版本兜底记录，仅供参考。
				meta.EngineVersion = buildVersion
			}
			_ = writeMeta(meta)
			fmt.Printf("引擎已启动: pid=%d addr=%s version=%s 日志=%s\n",
				pid, addr, meta.EngineVersion, logPath)
			if versionDrifted(meta.EngineVersion, buildVersion) {
				fmt.Fprintf(os.Stderr, "webtouch: 警告 引擎运行版本(%s) 与构建版本(%s) 不一致——可能 UWS_ENGINE_BIN 指向了旧二进制。建议 `make engine-restart`\n", meta.EngineVersion, buildVersion)
			}
			return nil
		}
		if !pidAlive(pid) {
			clearDaemonState()
			return fmt.Errorf("引擎进程已退出（pid=%d，日志: %s）", pid, logPath)
		}
		time.Sleep(200 * time.Millisecond)
	}
	clearDaemonState()
	return fmt.Errorf("启动引擎后 %s 未监听（pid=%d，日志: %s）", addr, pid, logPath)
}

func stopEngine() error {
	pid, err := loadRunningPID()
	if err != nil {
		// meta/pidfile 缺失：若端口确实有 UWS 引擎（孤儿），按端口回收；否则报未运行。
		if ver := healthVersion(daemonAddr()); ver != "" {
			orphan := pidByPort(daemonAddr())
			if orphan > 0 && pidAlive(orphan) {
				fmt.Fprintf(os.Stderr, "webtouch: 检测到未托管引擎（pid=%d version=%s），正在回收…\n", orphan, ver)
				if err := killByPID(orphan); err != nil {
					return fmt.Errorf("回收孤儿引擎 pid=%d 失败: %w", orphan, err)
				}
				return nil
			}
		}
		return err
	}
	if !pidAlive(pid) {
		clearDaemonState()
		return errors.New("引擎未运行（进程已退出，残留已清理）")
	}
	return killByPID(pid)
}

// ensureEngine 保证引擎在 addr 上运行：已监听则直接返回；未运行且 addr 是默认托管地址则拉起；
// addr 非默认托管地址且未监听则报错（不擅自为一个非托管地址起第二个引擎）。
func ensureEngine(addr string) error {
	if portOpen(addr) {
		// 版本一致性：run/crawl 绝不能静默跑在「与当前构建不一致」的旧引擎上（防源码/进程漂移）。
		if running := healthVersion(addr); versionDrifted(running, buildVersion) {
			msg := fmt.Sprintf("检测到引擎版本漂移：运行=%s 构建=%s（addr=%s）", running, buildVersion, addr)
			if addr == daemonAddr() {
				return fmt.Errorf("%s；请先 `make engine-restart`（= build + daemon restart）", msg)
			}
			fmt.Fprintf(os.Stderr, "webtouch: 警告 %s（外部引擎，未强制重启）\n", msg)
		}
		return nil
	}
	if addr != daemonAddr() {
		return fmt.Errorf("引擎在 %s 未运行，且不是默认托管地址 %s；请先 `webtouch daemon start`（或设置 WEBTOUCH_ENGINE_ADDR / UWS_ENGINE_ADDR）托管该地址", addr, daemonAddr())
	}
	return startEngine(addr)
}

func reportStatus() error {
	fmt.Printf("webtouch CLI 构建版本: %s（引擎基准: %s）\n", buildVersion, engine.EngineVersion)
	meta, err := readMeta()
	if err != nil {
		// meta 缺失但端口有引擎 → 孤儿/外部引擎，识别真实版本。
		if ver := healthVersion(daemonAddr()); ver != "" {
			pid := pidByPort(daemonAddr())
			fmt.Printf("引擎运行中（未托管/孤儿）: addr=%s pid=%d version=%s\n", daemonAddr(), pid, ver)
			if versionDrifted(ver, buildVersion) {
				fmt.Printf("[版本漂移] 运行 %s ≠ 构建 %s，请 `webtouch daemon restart`（或先 `make engine-restart`）\n", ver, buildVersion)
			}
			return nil
		}
		if pid, perr := loadRunningPID(); perr == nil && pidAlive(pid) {
			fmt.Printf("引擎运行中: pid=%d（元数据缺失，可能由外部/launchd 启动）\n", pid)
			return nil
		}
		fmt.Println("引擎未运行")
		return nil
	}
	if !pidAlive(meta.PID) {
		clearDaemonState()
		fmt.Println("引擎未运行（进程已退出，残留元数据已清理）")
		return nil
	}
	up := portOpen(meta.Addr)
	state := "运行中"
	listen := "监听"
	if !up {
		state = "进程存活但端口未监听"
		listen = "未监听"
	}
	runningVer := meta.EngineVersion
	if up {
		if v := healthVersion(meta.Addr); v != "" {
			runningVer = v
		}
	}
	fmt.Printf("引擎 %s: addr=%s pid=%d binary=%s started=%s version=%s %s\n",
		state, meta.Addr, meta.PID, meta.Binary, meta.StartedAt, runningVer, listen)
	if up && versionDrifted(runningVer, buildVersion) {
		fmt.Printf("[版本漂移] 运行 %s ≠ 构建 %s，请 `make engine-restart`（或 `webtouch daemon restart`）\n",
			runningVer, buildVersion)
	}
	return nil
}

// runDaemon 处理 `webtouch daemon <start|stop|restart|status>`（支持 --engine-addr/--grpc-addr 覆盖地址）。
func runDaemon(args []string) error {
	if len(args) == 0 {
		return errors.New("daemon: 用法 `webtouch daemon <start|stop|restart|status>`")
	}
	addr := daemonAddr()
	for i := 0; i < len(args); i++ {
		if (args[i] == "--engine-addr" || args[i] == "--grpc-addr") && i+1 < len(args) {
			addr = args[i+1]
			i++
		}
	}
	switch args[0] {
	case "start":
		return startEngine(addr)
	case "stop":
		return stopEngine()
	case "restart":
		if err := stopEngine(); err != nil {
			fmt.Fprintln(os.Stderr, "webtouch: "+err.Error())
		}
		return startEngine(addr)
	case "status":
		return reportStatus()
	default:
		return fmt.Errorf("daemon: 未知子命令 %q（支持 start、stop、restart、status）", args[0])
	}
}

// coordinatorEngineAddr 从 run/crawl/api 等子命令参数中解析引擎地址（--engine-addr，否则 WEBTOUCH_ENGINE_ADDR / UWS_ENGINE_ADDR）。
func coordinatorEngineAddr(args []string) string {
	addr := daemonAddr()
	for i := 0; i < len(args); i++ {
		if args[i] == "--engine-addr" && i+1 < len(args) {
			addr = args[i+1]
		}
	}
	return addr
}

// resolvedExeDir 解析 CLI 可执行文件的真实目录（软链部署以真实位置为准，
// 否则 /usr/local/bin 旁的相对目录判断会全部落空）。
func resolvedExeDir() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		return filepath.Dir(exe)
	}
	return ""
}

// repoRootFromDir 从给定目录向上锚定仓库根（以 coordinator/cli.py 存在为判据），
// 找不到返回空串。
func repoRootFromDir(dir string) string {
	for p := dir; ; p = filepath.Dir(p) {
		if info, err := os.Stat(filepath.Join(p, "coordinator", "cli.py")); err == nil && !info.IsDir() {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
	}
}

// repoRootFromExe 按可执行文件真实位置定位仓库根：bin/webtouch 与
// /usr/local/bin 软链两种部署形态都能稳定命中。
func repoRootFromExe() string {
	if d := resolvedExeDir(); d != "" {
		return repoRootFromDir(d)
	}
	return ""
}

// pythonCommand 定位项目内的 venv Python（依赖最稳），回退系统 python3。
func pythonCommand() (string, error) {
	// 仓库根 = 可执行文件真实位置向上锚定（bin/webtouch、仓库根软链、
	// /usr/local/bin 软链均可），避免入口目录变化时误回退系统 python3。
	if root := repoRootFromExe(); root != "" {
		cand := filepath.Join(root, ".venv", "bin", "python")
		if info, serr := os.Stat(cand); serr == nil && !info.IsDir() {
			return cand, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		cand := filepath.Join(filepath.Dir(exe), ".venv", "bin", "python")
		if info, serr := os.Stat(cand); serr == nil && !info.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p, nil
	}
	return "", errors.New("找不到 python3 / .venv/bin/python")
}

// execCoordinator 把子命令（run/crawl/api/search/cc-fetch）交给 Python 协调器执行。
func execCoordinator(args []string) error {
	py, err := pythonCommand()
	if err != nil {
		return err
	}
	cmd := exec.Command(py, append([]string{"-m", "coordinator.cli"}, args...)...)
	// 以仓库根为工作目录：`python -m coordinator.cli` 依赖 cwd 在 sys.path 上
	// （符号链接部署时 cwd 可能任意，必须显式固定；bin/ 内产物向上锚定仓库根）。
	if root := repoRootFromExe(); root != "" {
		cmd.Dir = root
	} else if d := resolvedExeDir(); d != "" {
		cmd.Dir = d
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

// episodeDir 解析 Episode 采集目录（默认启用）：
//
//	UWS_EPISODE_DIR 未设     → <state>/episodes
//	UWS_EPISODE_DIR=<路径>   → 用该路径
//	UWS_EPISODE_DIR=off/空/none/no/false/- → 显式关闭采集（与观看小窗同一哨兵口径）
//
// learnedPath 解析自进化学习存储路径：默认 <stateDir>/learned.json（daemon 托管默认开），
// UWS_LEARN=off/none/false/no/0/- 显式关闭，其他非空值覆盖路径。
func learnedPath() string {
	if v, ok := os.LookupEnv("UWS_LEARN"); ok {
		if viewerSocketDisabled(v) {
			return ""
		}
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return filepath.Join(daemonStateDir(), "learned.json")
}

func episodeDir() string {
	if v, ok := os.LookupEnv("UWS_EPISODE_DIR"); ok {
		if viewerSocketDisabled(v) {
			return ""
		}
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return filepath.Join(daemonStateDir(), "episodes")
}
