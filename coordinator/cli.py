#!/usr/bin/env python3
"""CLI 入口与子命令分发（TDD-CLI-01 ~ TDD-CLI-17）。

为 Python 协调器与 uws Go 二进制提供命令行统一入口：
- run: 接管本地浏览器或模拟浏览器执行声明式任务（结果始终 stdout，--output 仅持久化副本）
- crawl: 在线多深度信号提取爬虫
- probe: 附着持久调试 Chrome 做一次页面快照（AXTree + 截图 → Markdown，报给 Agent 层）
- api: 网页转标准 REST API 服务

注：CC 语料工具链（cc-fetch / parse-warc / search）已随代码移交
`labs/world-replay/search-engine/`（se 包，2026-08-20），uws 保持义肢纯净。
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path

# 确保 proto 桩可被导入
_PROTO_DIR = Path(__file__).resolve().parent.parent / "proto" / "gen" / "python"
if _PROTO_DIR.exists() and str(_PROTO_DIR) not in sys.path:
    sys.path.insert(0, str(_PROTO_DIR))

from coordinator.attach import resolve_attach
from coordinator.crawler import CrawlDriver, CrawlOptions
from coordinator.engine_client import EngineClient, EngineError
from coordinator.evidence import (
    attach_vlm_diagnosis,
    collect_failure_evidence,
    render_failure_markdown,
)
from coordinator.executor import Executor
from coordinator.explorer import ExploreDriver, ExploreOptions, render_markdown
from coordinator.models import ActionNode, TaskContext, TaskDefinition
from coordinator.planner import parse_plan_dsl
from coordinator.treefmt import summarize_tree

# ── 自解释：CLI 是「常新事实源」，skill 极简并指向 `uws help`/`help` ────────────────
# Agent 手册唯一事实源 = cmd/uws/agent-guide.md（Go 二进制 embed + 本文件同源读取，
# 改中文案只改一处）；文件缺失（如脱离仓库安装）时回退到最小提示。
_AGENT_GUIDE_FILE = Path(__file__).resolve().parent.parent / "cmd" / "uws" / "agent-guide.md"
_AGENT_GUIDE_FALLBACK = """uws — Universal Web Surrogate 浏览器自动化底座（Agent 自查手册）

共享手册文件未找到（cmd/uws/agent-guide.md）。请在仓库内运行本 CLI，或构建 Go 二进制后
执行 `uws help` 获取完整 Agent 手册（快速启动/子命令/网关/引擎/DSL/错误速查/反模式）。
"""

# 引擎状态码 → Agent 友好提示（容错 + 自解释）
ENGINE_HINTS = {
    4: "DEADLINE_EXCEEDED: 先退避 1s/2s 最多 2 次（网关已内置），仍失败则降级。",
    5: "NOT_FOUND: 可信节点不存在，页面结构可能大变 → 重新规划，勿裸重试。",
    9: "FAILED_PRECONDITION: 未初始化 / Target 崩溃 → 重新 InitializeContext。",
    10: "ABORTED: 节点脱离 DOM / 遮挡 → 等一帧立即重试（引擎已内置自愈）。",
    16: "UNAUTHENTICATED: WAF / 验证码 → 截图 → VLM 打码或人工接管。",
    8: "RESOURCE_EXHAUSTED: Target 池满 / 配额 → 降级阻塞。",
}


def _engine_hint(exc: BaseException) -> str:
    code = getattr(exc, "code", None)
    if code in ENGINE_HINTS:
        return "引擎提示: " + ENGINE_HINTS[code]
    return "提示: 对照 `uws help` 错误速查表；若是调用/权限问题，先确认引擎已启动与登录态。"


def agent_help() -> str:
    try:
        return _AGENT_GUIDE_FILE.read_text(encoding="utf-8")
    except OSError:
        return _AGENT_GUIDE_FALLBACK


def create_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="uws",
        description="Universal Web Surrogate 命令行工具",
        epilog="自解释手册：`uws help`（Go 二进制）或 `uws help`（本 CLI）会输出 Agent 友好指南（与 SKILL 保持一致）。",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    # uws help：自解释手册兜底
    help_parser = subparsers.add_parser("help", help="输出 Agent 友好自解释手册")
    help_parser.add_argument("topic", nargs="?", default="", help="（预留）主题")

    # uws run
    run_parser = subparsers.add_parser("run", help="执行声明式任务、只读观察或接管现有标签页")
    run_parser.add_argument("--attach", type=str, nargs="?", const="debug", default="debug", help="接管目标浏览器：debug=uws-chrome-debug（默认，自动拉起）/ default=用户日常 Chrome（自动带调试端口重启）/ 显式 CDP URL")
    run_parser.add_argument("--target-url", type=str, default="", help="目标初始网页 URL")
    run_parser.add_argument("--session", type=str, default=None, help="会话 ID（同一会话=同一标签页上下文，可跨命令续页；默认 default）")
    run_parser.add_argument("--mutex-key", type=str, default="", help="互斥键（如 login:example.com）：同键任务共享同一扇窗、后来者不重导航；默认不共享")
    run_parser.add_argument("--task-file", type=str, default="", help="任务定义 JSON 文件路径")
    run_parser.add_argument("--plan", type=str, default="", help="请求级 DSL 计划（如 'nav:首页\\nsearch:A醇精华\\nextract:成分表 | $成分\\nreply:$成分'）")
    run_parser.add_argument("--plan-file", type=str, default="", help="DSL 计划文件路径（多行，一行一意图）")
    run_parser.add_argument("--output", type=str, default="", help="可选：完整报告另存路径（结果始终打印 stdout，本参数仅持久化副本）")
    run_parser.add_argument("--screenshot", action="store_true", help="成功时将当前 Target 截图保存到本地并在回执中给出路径")
    run_parser.add_argument("--observe", action="store_true", help="只读观察指定的已有 session；不启动浏览器、不执行 DSL 动作")
    run_parser.add_argument("--engine-addr", type=str, default="127.0.0.1:50051", help="uws-engine gRPC 监听地址")
    run_parser.add_argument("--api-key", type=str, default="uws_dev_key", help="引擎 API Key (默认 uws_dev_key，与 uws daemon 默认一致)")

    # uws crawl
    crawl_parser = subparsers.add_parser("crawl", help="在线多深度智能爬取")
    crawl_parser.add_argument("url", help="起始种子 URL")
    crawl_parser.add_argument("--depth", type=int, default=2, help="爬取最大深度 (默认 2)")
    crawl_parser.add_argument("--concurrency", type=int, default=1, help="并发爬取工作协程数 (默认 1)")
    crawl_parser.add_argument("--signals", nargs="+", default=["title", "content", "author"], help="提取语义信号列表")
    crawl_parser.add_argument("--negative-keywords", nargs="+", default=["广告", "推广", "赞助"], help="负向过滤关键词")
    crawl_parser.add_argument("--format", choices=["md", "json"], default="md", help="输出格式 (默认 md)")
    crawl_parser.add_argument("--output-dir", type=str, default="", help="保存输出文件的目录")
    crawl_parser.add_argument("--engine-addr", type=str, default="127.0.0.1:50051", help="uws-engine gRPC 监听地址")
    crawl_parser.add_argument("--api-key", type=str, default="uws_dev_key", help="引擎 API Key (默认 uws_dev_key，与 uws daemon 默认一致)")

    # uws explore（站点启发式探索 → 导航图/功能报告）
    explore_parser = subparsers.add_parser(
        "explore",
        help="启发式探索站点：BFS 同域链接，分类页面类型，生成导航图与功能报告",
    )
    explore_parser.add_argument("url", help="种子 URL（同域链接会被 BFS 探索）")
    explore_parser.add_argument("--max-pages", type=int, default=12, help="最多探索页面数 (默认 12)")
    explore_parser.add_argument("--max-depth", type=int, default=3, help="BFS 最大深度 (默认 3)")
    explore_parser.add_argument("--settle-sec", type=float, default=5.0, help="每页加载稳定等待秒数 (默认 5)")
    explore_parser.add_argument("--attach", type=str, nargs="?", const="debug", default="debug", help="接管目标浏览器：debug=uws-chrome-debug（默认）/ default=用户日常 Chrome / 显式 CDP URL")
    explore_parser.add_argument("--output", type=str, default="", help="可选：Markdown 报告另存路径（结果始终打印 stdout，本参数仅持久化副本）")
    explore_parser.add_argument("--no-shots", action="store_true", help="不截取页面截图（报告不含图片）")
    explore_parser.add_argument("--engine-addr", type=str, default="127.0.0.1:50051", help="uws-engine gRPC 监听地址")
    explore_parser.add_argument("--api-key", type=str, default="uws_dev_key", help="引擎 API Key (默认 uws_dev_key，与 uws daemon 默认一致)")

    # uws probe（附着快照：AXTree + 截图 → Agent 层观察，DSL/chat 之外的首选“看一眼”原语）
    probe_parser = subparsers.add_parser(
        "probe",
        help="附着持久调试 Chrome 做一次页面快照：极简 AXTree + 截图 → Markdown 报告",
    )
    probe_parser.add_argument("--url", type=str, default="", help="初始 URL（空 = about:blank 新标签）")
    probe_parser.add_argument("--attach", type=str, nargs="?", const="debug", default="debug", help="接管目标浏览器：debug=uws-chrome-debug（默认）/ default=用户日常 Chrome / 显式 CDP URL")
    probe_parser.add_argument("--output", type=str, default="", help="可选：Markdown 报告另存路径（默认不写报告文件，AXTree 直接打印 stdout；截图默认存 ~/.uws/artifacts/，传本参数时存同目录 <报告名>.png）")
    probe_parser.add_argument("--keep-open", action="store_true", help="快照后保留标签页（人等登录/继续操作时用；默认快照后关闭）")
    probe_parser.add_argument("--engine-addr", type=str, default="127.0.0.1:50051", help="uws-engine gRPC 监听地址")
    probe_parser.add_argument("--api-key", type=str, default="uws_dev_key", help="引擎 API Key (默认 uws_dev_key，与 uws daemon 默认一致)")

    # uws api
    api_parser = subparsers.add_parser("api", help="[已废除] 网页转 REST API 已收敛至 Go 网关（请用 uws api / cmd/uws-gateway）")
    api_parser.add_argument("--target", choices=["llm_chat"], default="llm_chat", help="语义模板（当前支持 llm_chat）")
    api_parser.add_argument("--url", type=str, default="about:blank", help="目标网页 URL（如大模型对话页）")
    api_parser.add_argument("--attach", type=str, nargs="?", const="debug", default="debug", help="接管目标浏览器：debug=uws-chrome-debug（默认）/ default=用户日常 Chrome / 显式 CDP URL")
    api_parser.add_argument("--host", type=str, default="127.0.0.1", help="HTTP 监听地址（默认仅本机）")
    api_parser.add_argument("--port", type=int, default=8000, help="HTTP 监听端口")
    api_parser.add_argument("--chat-timeout-sec", type=int, default=60, help="单步等待超时秒数（等待流式回复结束）")
    api_parser.add_argument("--engine-addr", type=str, default="127.0.0.1:50051", help="uws-engine gRPC 监听地址")
    api_parser.add_argument("--api-key", type=str, default="uws_dev_key", help="引擎 API Key (默认 uws_dev_key，与 uws daemon 默认一致)")

    # uws commander（混合指挥：极简 AXTree → VLM 指挥官 → DSL 步骤）
    commander_parser = subparsers.add_parser(
        "commander",
        help="混合指挥：极简 AXTree → VLM 指挥官 → DSL 步骤（node: 确定性引用）",
    )
    commander_parser.add_argument("--tree-file", required=True, help="极简 AXTree 文本路径（'-' 读 stdin）")
    commander_parser.add_argument("--goal", default="把商品加入购物车并完成验证码", help="交给指挥官的任务")
    commander_parser.add_argument("--image", help="可选截图路径（base64 内联，detail=low）")
    commander_parser.add_argument("--endpoint", default="https://api.deepseek.com/chat/completions", help="OpenAI 兼容端点")
    commander_parser.add_argument("--model", default="deepseek-v4-flash-vision-exp", help="模型名")
    commander_parser.add_argument("--timeout", type=float, default=30.0, help="请求超时秒数")
    commander_parser.add_argument("--no-thinking", action="store_true", help="请求体省略 thinking:disabled")
    commander_group = commander_parser.add_mutually_exclusive_group()
    commander_group.add_argument("--dry-run", action="store_true", help="只打印请求，不真正外发")
    commander_group.add_argument("--mock", action="store_true", help="本地模拟指挥官，不发真实请求")

    # uws goal（内部：chat 目标模式的 Python 单一实现，uws Go 入口委派；不面向用户自命令）
    goal_parser = subparsers.add_parser(
        "goal",
        help="(内部) chat 目标模式：自然语言目标 → VLM 自决执行；DSL 语法有效时自动走 run",
    )
    goal_parser.add_argument("--prompt", required=True, help="自然语言目标（如：发布微博：今天天气不错）")
    goal_parser.add_argument("--attach", type=str, nargs="?", const="debug", default="debug", help="接管目标浏览器：debug（默认）/ default / 显式 CDP URL")
    goal_parser.add_argument("--session", type=str, default="default", help="会话 ID（与 uws run 同值可续同一页；默认 default）")
    goal_parser.add_argument("--mutex-key", type=str, default="", help="互斥键（如 login:example.com）：同键任务共享同一扇窗、后来者不重导航；默认不共享")
    goal_parser.add_argument("--output", type=str, default="", help="可选：Markdown 报告另存路径（结果始终打印 stdout，本参数仅持久化副本）")
    goal_parser.add_argument("--out-dir", type=str, default="", help="可选：截图保存目录（默认 ~/.uws/artifacts）")
    goal_parser.add_argument("--max-rounds", type=int, default=8, help="VLM 自决最大轮数（默认 8）")
    goal_parser.add_argument("--model", type=str, default="", help="VLM 模型名（默认 UWS_VLM_MODEL 或 deepseek-v4-flash-vision-exp）")
    goal_parser.add_argument("--debug", action="store_true", help="逐轮打印中间过程到 stderr：快照 URL/树规模/每轮截图、VLM 原始响应与决策、引擎动作结果，定位 VLM 卡点用")
    goal_parser.add_argument("--engine-addr", type=str, default="127.0.0.1:50051", help="uws-engine gRPC 监听地址")
    goal_parser.add_argument("--api-key", type=str, default="uws_dev_key", help="引擎 API Key (默认 uws_dev_key)")

    return parser


def _resolve_dsl_result(expr: str, variables: dict) -> str:
    """解析 DSL `reply:` 结果表达式：`$var` 取任务变量值，字面量原样返回。

    支持复合字符串内嵌 `$var`（如 `描述：$desc；$count`）；变量未绑定/缺失时
    替换为空串。整形恰为单个 `$var` 时与旧行为一致（缺失 → ""）。
    """
    expr = expr.strip()

    def _sub(match: re.Match) -> str:
        name = match.group(1)
        value = variables.get(name, "")
        return str(value if value is not None else "")

    # `\w`（Python3 默认 Unicode 感知）可覆盖中英文变量名（planner 允许 Unicode）。
    return re.sub(r"\$(\w+)", _sub, expr)


def _current_page_summary_lines(
    engine: EngineClient,
    task: TaskDefinition,
    *,
    capture_screenshot: bool = False,
    assets_dir: str | None = None,
) -> list[str]:
    """把“停在这里的页面”报清楚：当前 URL + AXTree 摘要 + 可选截图。

    成功结束也要给 Agent 停留页面摘要（含 node_id，可据此继续下一条 DSL 或用
    click:node 确定性引用）；失败/护栏暂停时 Failure/「需要确认」证据段已含树与
    截图，本摘要只负责正常收尾后的可续跑上下文。
    """
    url = str(task.context.variables.get("initial_url", ""))
    for entry in reversed(task.context.history):
        entry_url = getattr(entry, "current_url", "") or ""
        if entry_url:
            url = entry_url
            break
    target_id = task.context.current_target_id or ""
    lines = [f"- 当前 URL：`{url or '-'}`", f"- Target ID: `{target_id or '-'}`（同会话内续页定位用）"]
    if not target_id:
        lines.append("- 页面 AXTree：无活动 Target（任务结束/上下文未初始化）")
        return lines
    try:
        tree = (engine.export_page_tree(target_id) or "").strip()
        shown, note = summarize_tree(tree, budget=8000)
        summary = f"- 页面 AXTree 摘要：{len(tree.splitlines())} 行"
        if note:
            summary += f"；{note}"
        lines.append(summary)
        if shown:
            lines.extend(["", "```text", shown, "```"])
    except Exception as exc:  # noqa: BLE001 —— 摘要不可用不阻断结果
        lines.append(f"- 页面 AXTree 不可用：{exc}")

    if capture_screenshot:
        try:
            png = engine.capture_screenshot(target_id)
            if not png:
                raise RuntimeError("引擎返回空截图")
            if assets_dir:
                out_dir = Path(assets_dir)
            else:
                out_dir = Path.home() / ".uws" / "artifacts"
                try:
                    out_dir.mkdir(parents=True, exist_ok=True)
                except OSError:
                    out_dir = Path.cwd() / "uws-run-assets"
            out_dir.mkdir(parents=True, exist_ok=True)
            safe_target = re.sub(r"[^A-Za-z0-9_-]", "_", target_id)
            shot_path = out_dir / f"run-{os.getpid()}-{safe_target}.png"
            shot_path.write_bytes(png)
            lines.append(f"- 页面截图：`{shot_path}`")
        except Exception as exc:  # noqa: BLE001 —— 截图不可用不阻断成功回执
            lines.append(f"- 页面截图不可用：{exc}")
    return lines


def run_task(args: argparse.Namespace, engine: EngineClient | None = None) -> int:
    observe_only = bool(getattr(args, "observe", False))
    session_arg = getattr(args, "session", None)
    if observe_only:
        if not session_arg:
            print("错误: --observe 必须显式指定已有 --session，避免误观察 default/new tab。", file=sys.stderr)
            return 2
        if any((getattr(args, "target_url", ""), getattr(args, "plan", ""),
                getattr(args, "plan_file", ""), getattr(args, "task_file", ""),
                getattr(args, "mutex_key", ""))):
            print("错误: --observe 只接受 --session、--screenshot 和输出/连接参数；不得同时传入导航或动作计划。", file=sys.stderr)
            return 2

    if engine is None:
        from coordinator.grpc_client import GrpcEngineClient
        # 解析 host:port 或 port
        addr_str = args.engine_addr
        if ":" in addr_str:
            port = int(addr_str.split(":")[-1])
        else:
            port = int(addr_str)
        client = GrpcEngineClient(port=port, api_key=args.api_key)
    else:
        client = engine

    task_id = "cli-run-" + str(os.getpid())
    initial_url = args.target_url or ""
    session_id = session_arg or "default"
    plan: list[ActionNode] = []
    bindings: dict[str, str] = {}
    result: str | None = None

    if not observe_only and (args.plan or args.plan_file):
        # 请求级 DSL：parse_plan_dsl 编译为可执行 ActionNode 序列（node: 确定性引用）。
        dsl = args.plan
        if args.plan_file:
            dsl = Path(args.plan_file).read_text(encoding="utf-8")
        parsed = parse_plan_dsl(dsl)
        plan = parsed.steps
        bindings = parsed.bindings
        result = parsed.result
        if parsed.initial_url:
            initial_url = parsed.initial_url
    elif not observe_only and args.task_file and Path(args.task_file).exists():
        data = json.loads(Path(args.task_file).read_text(encoding="utf-8"))
        for item in data.get("plan", []):
            plan.append(ActionNode(**item))
    elif not observe_only:
        # 默认提取任务
        plan.append(ActionNode(
            step_id="step-1",
            action_type="extract",
            target_semantic="page_title_and_main_content",
        ))

    try:
        # 只读观察不能为了寻找会话而启动或重启 Chrome；仅使用已就绪的 CDP 端点。
        attach_url = resolve_attach(args.attach, allow_launch=not observe_only)
    except (RuntimeError, ValueError) as exc:
        print(f"观察失败：{exc}", file=sys.stderr)
        return 1
    context = TaskContext(
        variables={
            "initial_url": initial_url or None,
            "attach_cdp_url": attach_url,
            "context_name": session_id,
            "mutex_key": getattr(args, "mutex_key", "") or None,
            "require_existing_session": observe_only,
        }
    )
    task = TaskDefinition(
        task_id=task_id,
        name="CLI Run Task",
        plan=plan,
        context=context,
        bindings=bindings,
        result=result,
    )

    executor = Executor(client)
    executor.run(task)
    reported_url = str(task.context.variables.get("initial_url") or initial_url or "")

    out_md = [
        f"# Task Execution Result: {task.name}",
        f"- Status: **{task.status}**",
        f"- Task ID: `{task.task_id}`",
        f"- Session ID: `{session_id}`",
        f"- Target ID: `{task.context.current_target_id or '-'}`（同会话续页/定位用）",
        f"- Target URL: `{reported_url or '-'}`",
        f"- Attached Mode: `{attach_url}`",
        "",
        "## Execution History",
    ]
    for h in task.context.history:
        line = f"- Step `{getattr(h, 'step_id', 'unknown')}`: status={getattr(h, 'status', 'ok')}"
        he = getattr(h, "hit_element", None)
        if he is not None and (he.tag or he.text):
            # v1.5.4 证据链回显（2026-09-23）：动作实际命中了哪个节点。extract 的返回值
            # 就来自这个节点——不回显就无法分辨「抽到目标本身」与「抽到同名碎片」。
            line += f" · hit={he.describe()}"
        out_md.append(line)
    if observe_only:
        out_md.append("- Mode: `READ_ONLY_OBSERVE`（没有执行页面动作）")
    if result is not None:
        out_md.append("")
        out_md.append("## Result")
        out_md.append(_resolve_dsl_result(result, task.context.variables))

    ok = task.status == "SUCCEEDED"
    ev = None
    if ok:
        # 成功也把“停在这里的页面”报清楚：Agent 读完 AXTree 摘要即可继续下一条
        # DSL（单步模式 = 原 uws run --plan，不新增命令、不写计划文件）。
        out_md.append("")
        out_md.append("## 当前页面（任务结束点，可继续操作）")
        assets_dir = None
        if args.output:
            out = Path(args.output)
            assets_dir = str(out.with_name(out.stem + ".assets"))
        page_summary = _current_page_summary_lines(
            client, task,
            capture_screenshot=bool(getattr(args, "screenshot", False)),
            assets_dir=assets_dir,
        )
        out_md.extend(page_summary)
        if observe_only and any(line.startswith("- 页面 AXTree 不可用：") for line in page_summary):
            # 活跃上下文对象可能已经失效；不要把“初始化返回成功”误报成已成功观察。
            task.status = "FAILED"
            ok = False
            out_md[1] = "- Status: **FAILED**"
            out_md.extend(["", "## Observation Failure", "- 原因：会话 Target 已找到，但 AX 页面证据不可读取。", "- 未请求 VLM 诊断。"])
        observed_url = str(task.context.variables.get("initial_url") or "")
        if observe_only and observed_url.rstrip("/") in {"", "about:blank"}:
            out_md.append("- 观察提示：当前 session 没有可确认的网页内容；空白 Target 不代表原页面已成功续接。")
    elif observe_only:
        # session 不存在等初始化错误是确定性结果；不收集页面证据，也不请求 VLM 诊断。
        out_md.append("")
        out_md.append("## Observation Failure")
        out_md.append(f"- 原因：`{task.error_reason or task.status}`")
        out_md.append("- 未执行页面动作；未请求 VLM 诊断。")
    else:
        # 失败可消费化（2026-09-03）：原因 + 当前 URL + AXTree + 截图 + 可选 VLM
        # 自然语言诊断。确定性证据先收集；VLM 只做解释，不可用不阻塞。
        assets_dir = None
        if args.output:
            out = Path(args.output)
            assets_dir = str(out.with_name(out.stem + ".assets"))
        ev = collect_failure_evidence(task, client, assets_dir=assets_dir)
        if not ev.reason.startswith("confirmation_required"):
            # 护栏暂停（需用户确认）是确定性判断，不请 VLM 猜测、也不浪费余额。
            attach_vlm_diagnosis(ev)
        out_md.append("")
        out_md.extend(render_failure_markdown(ev))

    report = "\n".join(out_md) + "\n"
    # stdout 优先（2026-09-03 用户定案：tool for agent, stdin/stdout first，
    # files only for persistent）：完整报告永远直接打印 stdout，Agent 不需要再读文件；
    # --output 仅在需要留档时另存一份副本。
    print(report, end="")
    if args.output:
        Path(args.output).write_text(report, encoding="utf-8")
        print(f"报告已另存：{args.output}")
    if ev is not None and ev.screenshot_path:
        print(f"失败证据截图：{ev.screenshot_path}")

    return 0 if ok else 1


def run_crawl(args: argparse.Namespace, engine: EngineClient | None = None) -> int:
    if engine is None:
        from coordinator.grpc_client import GrpcEngineClient
        addr_str = args.engine_addr
        if ":" in addr_str:
            port = int(addr_str.split(":")[-1])
        else:
            port = int(addr_str)
        client = GrpcEngineClient(port=port, api_key=args.api_key)
    else:
        client = engine

    signals = [s.strip() for tok in args.signals for s in tok.split(",") if s.strip()]
    negative_keywords = [
        s.strip() for tok in args.negative_keywords for s in tok.split(",") if s.strip()
    ]
    opts = CrawlOptions(
        signals=signals,
        negative_keywords=negative_keywords,
        max_depth=args.depth,
    )
    driver = CrawlDriver(client, opts)

    try:
        pages = driver.crawl(args.url)
    except Exception as exc:  # noqa: BLE001 - CLI 顶层兜底
        print(f"uws crawl 失败: {exc}", file=sys.stderr)
        print("提示: 运行 `uws help` 查看用法与错误速查；多为引擎未就绪、种子页无法语义定位或连接被拦截。", file=sys.stderr)
        return 1

    if args.output_dir:
        out_dir = Path(args.output_dir)
        out_dir.mkdir(parents=True, exist_ok=True)
        for i, page in enumerate(pages, 1):
            if args.format == "md":
                file_path = out_dir / f"page_{i}.md"
                file_path.write_text(page.markdown, encoding="utf-8")
            else:
                file_path = out_dir / f"page_{i}.json"
                file_path.write_text(json.dumps({
                    "url": page.url,
                    "depth": page.depth,
                    "fields": page.fields,
                    "ad_filtered": page.ad_filtered,
                }, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"成功爬取 {len(pages)} 页，已保存至 {out_dir}")
    else:
        for page in pages:
            if args.format == "md":
                print(page.markdown)
            else:
                print(json.dumps(page.fields, ensure_ascii=False, indent=2))
    return 0


def run_explore(args: argparse.Namespace, engine: EngineClient | None = None) -> int:
    """启发式探索站点并输出导航图/功能报告（TDD-CLI-17）。"""
    if engine is None:
        from coordinator.grpc_client import GrpcEngineClient

        addr_str = args.engine_addr
        if ":" in addr_str:
            port = int(addr_str.split(":")[-1])
        else:
            port = int(addr_str)
        client = GrpcEngineClient(port=port, api_key=args.api_key)
    else:
        client = engine

    # 截图目录：有 --output 时放同目录 <报告名>.assets/，stdout 模式放 cwd/uws-explore-assets/
    if args.output:
        out_path = Path(args.output)
        assets_dir = str(out_path.with_name(out_path.stem + ".assets"))
        out_path.parent.mkdir(parents=True, exist_ok=True)
    else:
        assets_dir = "uws-explore-assets"

    options = ExploreOptions(
        max_pages=args.max_pages,
        max_depth=args.max_depth,
        settle_sec=args.settle_sec,
        attach=resolve_attach(args.attach),
        assets_dir=assets_dir,
        capture_shots=not args.no_shots,
    )
    driver = ExploreDriver(client, options)
    try:
        report = driver.explore(args.url)
    except Exception as exc:  # noqa: BLE001 - CLI 顶层兜底
        print(f"uws explore 失败: {exc}", file=sys.stderr)
        print("提示: 运行 `uws help` 查看用法；多为引擎未就绪、attach 端点不可达或种子页无法加载。", file=sys.stderr)
        return 1

    report.assets_rel = Path(assets_dir).name
    markdown = render_markdown(report)
    # stdout 优先：完整报告直接打印；--output 仅作持久化副本。
    print(markdown, end="")
    if args.output:
        Path(args.output).write_text(markdown, encoding="utf-8")
        print(f"探索完成：{len(report.pages)} 页 / {len(report.edges)} 条边，报告已另存：{args.output}")
    return 0


def run_probe(args: argparse.Namespace, engine: EngineClient | None = None) -> int:
    """附着持久调试 Chrome 做一次页面快照：极简 AXTree + 截图 → Markdown。

    这是 AXTree/截图“报到 Agent 层”的一级原语（2026-09-03）：Agent 需要观察当前页面
    （登录后确认、控制台结构、点哪一步前先看）时用本命令，禁止自降级写 gRPC 脚本。
    stdout 优先：AXTree 报告完整打印 stdout；--output 仅在需要留档时另存报告文件；
    截图是二进制产物必须落盘：--output 时存同目录 <报告名>.png，否则存 ~/.uws/artifacts/。
    默认快照后关闭上下文；--keep-open 保留标签页供人登录/继续操作。
    """
    if engine is None:
        from coordinator.grpc_client import GrpcEngineClient

        addr_str = args.engine_addr
        if ":" in addr_str:
            port = int(addr_str.split(":")[-1])
        else:
            port = int(addr_str)
        engine = GrpcEngineClient(port=port, api_key=args.api_key)

    task_id = f"cli-probe-{os.getpid()}"
    attach_url = resolve_attach(args.attach)
    init = engine.initialize_context(
        task_id,
        initial_url=args.url or None,
        context_name=task_id,
        attach_cdp_url=attach_url,
    )
    if not init.ok:
        raise RuntimeError(f"InitializeContext 失败: {init.error_reason or '未知原因'}")
    target_id = init.target_id
    try:
        tree = engine.export_page_tree(target_id)
        png = engine.capture_screenshot(target_id)
        if not png:
            raise RuntimeError("CaptureScreenshot 返回空 PNG，页面可能仍在加载或不可见")
    finally:
        if not args.keep_open:
            engine.close_context(context_name=task_id)

    if args.output:
        out_path = Path(args.output)
        out_path.parent.mkdir(parents=True, exist_ok=True)
        png_path = out_path.with_name(out_path.stem + ".png")
    else:
        artifacts = Path.home() / ".uws" / "artifacts"
        try:
            artifacts.mkdir(parents=True, exist_ok=True)
        except OSError:
            artifacts = Path.cwd() / "uws-probe-assets"
            artifacts.mkdir(parents=True, exist_ok=True)
        png_path = artifacts / f"probe-{os.getpid()}.png"
    png_path.write_bytes(png)
    markdown = "\n".join(
        [
            "# WebTouch Probe",
            "",
            f"- Task ID: `{task_id}`",
            f"- Context ID: `{init.context_id or task_id}`",
            f"- Target ID: `{target_id or '-'}`（真实 CDP target；--keep-open 时该 Tab 保留，可据此定位/继续）",
            f"- URL: `{args.url or 'about:blank'}`",
            f"- Attached: `{attach_url}`",
            f"- Screenshot: {png_path}",
            f"- Tab: {'kept open' if args.keep_open else 'closed after snapshot'}",
            "",
            "## AXTree（与 Markdown 合一输出；含 node_id，供 commander / DSL node: 引用）",
            "",
            "```",
            tree.rstrip("\n"),
            "```",
            "",
        ]
    )
    # stdout 优先：AXTree 随 Markdown 一起完整打印，Agent 直接消费，不再读文件。
    print(markdown, end="")
    if args.output:
        Path(args.output).write_text(markdown + "\n", encoding="utf-8")
        print(f"probe 完成：AXTree {len(tree.splitlines())} 行，截图 {png_path}，报告已另存：{args.output}")
    return 0


def run_api(args: argparse.Namespace, engine: EngineClient | None = None) -> int:
    """提示 Python API Server 已废除，WebTouch 只提供 Agent Hub 私有浏览器执行服务。"""
    print(
        "错误: Python 版 api_server 已正式废除并归档（M1 运行时收敛）。\n"
        "请用 `uws api` 或 `bin/uws-gateway` 启动 WebTouch 浏览器执行服务（或运行 `./scripts/start_bus.sh`）。\n"
        "客户端模型 API 请连接 Agent Hub；WebTouch 只接受 Hub 的内部 provider 契约，并负责多 Tab 物理隔离与站点风控。",
        file=sys.stderr,
    )
    return 1
def main(argv: list[str] | None = None) -> int:
    parser = create_parser()
    args = parser.parse_args(argv)
    if args.command == "help":
        print(agent_help())
        return 0
    try:
        if args.command == "run":
            return run_task(args)
        if args.command == "crawl":
            return run_crawl(args)
        if args.command == "explore":
            return run_explore(args)
        if args.command == "probe":
            return run_probe(args)
        if args.command == "api":
            return run_api(args)
        if args.command == "commander":
            from coordinator.commander import run_commander

            return run_commander(args)
        if args.command == "goal":
            from coordinator.goal import run_goal_cli

            return run_goal_cli(args)
        return 0
    except EngineError as exc:
        print(f"uws {args.command} 失败: {exc.reason or exc}", file=sys.stderr)
        print(_engine_hint(exc), file=sys.stderr)
        return 1
    except (OSError, ValueError) as exc:
        print(f"uws {args.command} 失败: {exc}", file=sys.stderr)
        print("提示: 检查 `uws help` 的参数与启动步骤；多为引擎未就绪或参数非法。", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 - CLI 顶层兜底（容错 + 自解释）
        print(f"uws {args.command} 失败: {exc}", file=sys.stderr)
        print("提示: 运行 `uws help` 查看用法与错误速查；若是调用/权限问题，先确认引擎与登录态。", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
