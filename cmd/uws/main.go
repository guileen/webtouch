// Command webtouch 是面向用户/Agent 的 CLI 入口（run / crawl / parse-warc / api / search / config）。
// 自解释优先：`webtouch help`（或直接 `webtouch`）返回 skill 友好指南（agentGuide），
// 错误回落 agent 友好提示，让 Agent 无需通读大段 SKILL 就能自助排障。
package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed agent-guide.md
var agentGuideText string

func main() {
	if msg := deprecatedAliasNotice(os.Args[0]); msg != "" {
		fmt.Fprint(os.Stderr, msg)
	}
	args := os.Args[1:]
	prog := filepath.Base(os.Args[0])
	// 无参 / help / -h / --help：技能友好指南，Agent 可直接消费。
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(agentGuide())
		os.Exit(0)
	}
	// --version / -V / version：打印 CLI/引擎/知识库兼容基准版本（自解释防漂移）。
	if args[0] == "version" || args[0] == "--version" || args[0] == "-V" || args[0] == "-v" {
		runVersion()
		os.Exit(0)
	}
	switch args[0] {
	case "fetch":
		os.Exit(runFetch(args[1:], os.Stdout))
	case "config":
		if err := runConfig(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			fmt.Fprintln(os.Stderr, "提示: 运行 `"+prog+" help` 查看用法，或 `"+prog+" config get <key>` 查已存值（api_key 会脱敏）。")
			os.Exit(1)
		}
	case "daemon":
		if err := runDaemon(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			fmt.Fprintln(os.Stderr, "提示: `"+prog+" daemon status` 看引擎状态；改地址用 UWS_ENGINE_ADDR 或 `--engine-addr`。")
			os.Exit(1)
		}
	case "run", "crawl", "explore", "probe", "parse-warc", "search", "cc-fetch":
		if err := runCoordinator(args); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			os.Exit(1)
		}
	case "api", "gateway":
		// WebTouch 浏览器 provider 执行服务：100% 收口至 Go 原生实现（cmd/uws-gateway）。
		if err := runGateway(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			os.Exit(1)
		}
	case "chat", "-p", "--prompt", "-P":
		// UWS「义眼」原语：直接与视觉模型对话看图/对比（不产执行动作，只给认知反馈）。
		if err := runChat(wrapPromptArgs(args)); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			os.Exit(1)
		}
	case "viewer":
		// 观看小窗：status = 看引擎是否在推流；app = 拉起原生 Viewer 壳。
		if err := runViewer(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			fmt.Fprintln(os.Stderr, "提示: `"+prog+" viewer status` 看推流状态；`"+prog+" viewer app` 打开观看器。")
			os.Exit(1)
		}
	case "identity":
		// 浏览器身份（指纹）注册表与账号绑定：list/show/bind/unbind/resolve/use/check。
		if err := runIdentity(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			fmt.Fprintln(os.Stderr, "提示: `"+prog+" identity list` 看有哪些身份；`"+prog+" identity help` 看用法。")
			os.Exit(1)
		}
	case "vault":
		// 本地凭据库（age 加密、只落本机、不联网）：init/list/set/get/rm。
		if err := runVault(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			fmt.Fprintln(os.Stderr, "提示: `"+prog+" vault help` 看用法（init → set → get）。")
			os.Exit(1)
		}
	case "state":
		// State Bundle 迁移：导出/加密/导入均走受鉴权引擎，不打印 Cookie 或密钥。
		if err := runState(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			fmt.Fprintln(os.Stderr, "提示: `"+prog+" state help` 查看迁移步骤。")
			os.Exit(1)
		}
	case "request":
		// 纯 HTTP 伴侣能力：不启动浏览器，但只复用同一档案的 HTTP 可见身份。
		if err := runRequest(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			fmt.Fprintln(os.Stderr, "提示: `"+prog+" request help` 查看身份、Cookie 与 TLS 边界。")
			os.Exit(1)
		}
	case "bus":
		// 浏览器执行服务管理与 Agent Hub 客户端环境注入：status / env
		if err := runBus(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			fmt.Fprintln(os.Stderr, "提示: `"+prog+" bus help` 查看用法，或 `eval $("+prog+" bus env)` 注入环境变量。")
			os.Exit(1)
		}
	case "qa":
		// WebTouch QA Studio 前端交付验收工作台（多视口物理体检、洋红高亮 Diff 与验收报告）
		if err := runQA(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			os.Exit(1)
		}
	case "studio":
		// WebTouch Studio 云端 Web 交互控制面与流式伴生视窗
		if err := runStudio(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			os.Exit(1)
		}
	case "session":
		// 对外 session 生命周期：句柄进出 CLI，状态在本机 lease。
		if err := runSession(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			os.Exit(1)
		}
	case "inspect":
		// WebTouch 有界 Provider 入口：页面有界体检、脱敏与确定性证据包输出
		if err := runInspect(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			os.Exit(1)
		}
	case "commander":
		// 混合指挥演示桩：纯外发请求，不依赖引擎（引擎截图/AXTree RPC 接线为待办）。
		if err := execCoordinator(args); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			os.Exit(1)
		}
	default:
		// 2026-09-03：无子命令时第一个非命令词 = chat 目标文本（推荐最短写法），
		// 例如 `webtouch "发布微博：xxx"` / `webtouch "看看 x.com"`；等价 `webtouch chat "<文本>"`。
		if err := runChat(args); err != nil {
			fmt.Fprintln(os.Stderr, prog+": "+err.Error())
			os.Exit(1)
		}
	}
}

// wrapPromptArgs 把顶层 `-p/--prompt "问题"` 归一为 `chat "问题"` 形参，
// 便于与 `webtouch chat ...` 共享解析逻辑。
func wrapPromptArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	switch args[0] {
	case "-p", "--prompt", "-P":
		out := make([]string, 0, len(args))
		out = append(out, args[1:]...)
		return out
	default:
		return args[1:]
	}
}

// deprecatedAliasNotice 在 argv0 仍是弃用名 uws 时返回一行 stderr 提示。
// 正式入口 webtouch 不提示。提示只走 stderr，避免污染 session 的 stdout JSON。
func deprecatedAliasNotice(argv0 string) string {
	base := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	if base == "uws" {
		return "注意: 命令名 uws 已弃用，请改用 webtouch（同一二进制）。\n"
	}
	return ""
}

func usage() {
	fmt.Fprintln(os.Stderr, `webtouch — WebTouch 命令行（uws 为弃用别名，同一二进制）

用法:
  webtouch "目标文本"                推荐入口（= chat 目标模式）：一句自然语言目标，
                                VLM 自决执行；登录/付费处停下等你；结束给状态和页面
                                （DSL 语法有效自动走确定性 run --plan）
  webtouch chat "目标" --img <图>    义眼模式：看图/对比/评审（显式视觉输入时自动分流）
  webtouch config set <key> <value>   持久化配置到 ~/.uws/config.yaml（api_key 不打印明文）
  webtouch daemon <start|stop|restart|status>   直接管理引擎守护进程（默认 127.0.0.1:50051）
  webtouch run --plan "<DSL>"         确定性 DSL（熟悉目标站点后的自动化；语法有效也会被目标模式自动路由）
  webtouch crawl --depth 2 ...        在线智能爬虫（python3 -m coordinator.cli crawl ...）
  webtouch explore <url> ...          启发式探索站点，生成导航图/功能报告（python3 -m coordinator.cli explore ...）
  webtouch probe [--url <URL>] ...    附着持久调试 Chrome 快照：AXTree + 截图 → Markdown 报告
  webtouch parse-warc <file> ...      离线 WARC 解析（python3 -m coordinator.cli parse-warc ...）
  webtouch api [start|status|help]   浏览器执行服务（迁移期；对外契约是 CLI，见 session）
  webtouch session create|use|show|list|close
                                 本机 session 生命周期（stdout JSON；--session 复用句柄）
  webtouch qa --url <url> [选项]     前端交付验收工作台（多视口物理体检、洋红高亮 Diff 与交付验收单）
  webtouch commander --tree-file ...  混合指挥：AXTree → VLM 指挥官 → DSL 步骤（node: 引用）
  webtouch chat "问题" --img <图> ... 义眼评审（返回结构化判断）；-p/--prompt 是顶层别名
  webtouch search --corpus <dir> ...  CC 语料本地检索（python3 -m coordinator.cli search ...）
	  webtouch cc-fetch --crawl ... ...   CC WET 语料下载（python3 -m coordinator.cli cc-fetch ...）`)
}

// runCoordinator 处理需要引擎的子命令：先确保引擎就绪（按需拉起），再把控制交给 Python 协调器。
func runCoordinator(args []string) error {
	addr := coordinatorEngineAddr(args)
	if err := ensureEngine(addr); err != nil {
		fmt.Fprintln(os.Stderr, "提示: 运行 `webtouch daemon status` 查看引擎状态，或 `webtouch daemon start` 手动拉起。")
		return err
	}
	if err := execCoordinator(args); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// agentGuide 返回嵌入的共享 Agent 手册（cmd/uws/agent-guide.md，与 Python
// coordinator.cli help 同源，改中文案只改一处）。
func agentGuide() string { return agentGuideText }
