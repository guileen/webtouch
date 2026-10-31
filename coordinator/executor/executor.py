"""调度执行器（spec/coordinator.md §5）：任务级状态机主循环。

语义要点（GLOBAL.md §5 裁决 #9/#11）：
- v1.4 同步契约下 ExecuteAction 返回 NAVIGATING 即视为本步完成（异步交接），
  稳定性等待由引擎在下一动作入口执行，协调器不做额外 wait_stable；
- DEADLINE_EXCEEDED 按 Fibonacci 1s/2s 最多重试 2 次（协调器侧统一语义）；
- FAILED_PRECONDITION / HealthCheck 连续失败触发崩溃恢复：只重放幂等步骤，
  显式标记 non_idempotent 的步骤绝不自动重放；
- 连续失败 >= 2 触发反思：回退最近 5 步内最后一个 STABLE 步骤重放。
"""

from __future__ import annotations

import re
import time as _time
from dataclasses import dataclass, field
from typing import Protocol

from coordinator.engine_client import ActionResult, EngineClient, EngineError
from coordinator.guard import guard_before_execute, page_cta_marker
from coordinator.models import ActionNode, TaskContext, TaskDefinition
from coordinator.planner import NoTemplateMatch, PlanValidationError, Planner, TemplateRegistry
from coordinator.reflector import FailureAction, Reflector
from coordinator.vlm import VlmSolver

HEALTH_FAILURE_THRESHOLD = 3
MAX_REFLECTIONS = 1
# VLM 挑战单任务求解次数上限（TDD-COORD-04）：求解成功但动作仍被拦截时
# 重试求解，超过上限置 PAUSED，避免无限重试。
MAX_VLM_SOLVES = 3
IDEMPOTENT_ACTIONS = {"input", "select", "focus", "hover", "scroll", "wait", "extract"}
# 条件表达式（TDD-PLAN-05）：`{{ var }}` + 运算符 + 字面量/列表。
# 支持 ==、!=、in、not in；RHS 可为 '字符串'、"字符串"、true/false、整数/浮点、
# ['a','b'] 列表（TDD 清单 §4.4：condition 分支语义与 loop 退出条件共用）。
CONDITION_RE = re.compile(
    r"^\s*\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}\s*"
    r"(==|!=|not\s+in|in)\s*"
    r"(.+?)\s*$"
)


class Clock(Protocol):
    def sleep(self, seconds: float) -> None: ...


class _RealClock:
    def sleep(self, seconds: float) -> None:
        _time.sleep(seconds)


def _parse_literal(raw: str) -> object:
    """解析条件表达式 RHS：字符串/布尔/数字/['a','b'] 列表；裸词按字符串处理。"""
    raw = raw.strip()
    if len(raw) >= 2 and raw[0] == raw[-1] and raw[0] in ("'", '"'):
        return raw[1:-1]
    low = raw.lower()
    if low == "true":
        return True
    if low == "false":
        return False
    if raw.startswith("[") and raw.endswith("]"):
        inner = raw[1:-1].strip()
        if not inner:
            return []
        return [_parse_literal(part) for part in _split_list(inner)]
    try:
        return int(raw)
    except ValueError:
        try:
            return float(raw)
        except ValueError:
            return raw


def _split_list(inner: str) -> list[str]:
    """按逗号拆分列表字面量（跳过引号内的逗号）。"""
    parts: list[str] = []
    buf: list[str] = []
    quote: str | None = None
    for ch in inner:
        if quote:
            buf.append(ch)
            if ch == quote:
                quote = None
        elif ch in ("'", '"'):
            quote = ch
            buf.append(ch)
        elif ch == ",":
            parts.append("".join(buf))
            buf = []
        else:
            buf.append(ch)
    if buf or parts:
        parts.append("".join(buf))
    return [p.strip() for p in parts]


def _eq(value: object, literal: object) -> bool:
    """类型感知相等：布尔与 "true"/"false" 字符串互认；数字跨 int/float 比较。"""
    if isinstance(literal, bool):
        if isinstance(value, bool):
            return value is literal
        if isinstance(value, str):
            return value.strip().lower() == str(literal).lower()
        return False
    if isinstance(value, bool):
        return isinstance(literal, str) and literal.strip().lower() == str(value).lower()
    if isinstance(literal, (int, float)):
        try:
            return float(value) == float(literal)
        except (TypeError, ValueError):
            return False
    return value == literal


def _contains(value: object, literal: object) -> bool:
    """成员判定：列表 RHS 支持成员/子集；字符串 RHS 做子串匹配。"""
    if isinstance(literal, list):
        if isinstance(value, list):
            return any(v in literal for v in value)
        return value in literal
    if isinstance(literal, str):
        return literal in str(value)
    return False


@dataclass
class _RunState:
    completed: set[str] = field(default_factory=set)
    skipped: set[str] = field(default_factory=set)
    consecutive_failures: int = 0
    reflections: int = 0
    health_failures: int = 0
    steps_executed: int = 0
    # vlm_solves：本任务内 VLM 求解次数（求解成功但步骤仍被拦截时递增，封顶后 PAUSED）。
    vlm_solves: int = 0
    # session_epoch：每次崩溃恢复/REINIT 递增，重放步骤使用新 intent_id，
    # 避免新上下文吃到旧会话 60s 幂等缓存（R4）。
    session_epoch: int = 0


class Executor:
    def __init__(
        self,
        engine: EngineClient,
        planner: Planner | None = None,
        reflector: Reflector | None = None,
        clock: Clock | None = None,
        vlm_solver: VlmSolver | None = None,
        max_vlm_solves: int = MAX_VLM_SOLVES,
    ) -> None:
        self._engine = engine
        self._planner = planner or Planner(TemplateRegistry())
        self._reflector = reflector or Reflector()
        self._clock = clock or _RealClock()
        self._vlm_solver = vlm_solver
        self._max_vlm_solves = max_vlm_solves

    def run(self, task: TaskDefinition) -> TaskDefinition:
        task.status = "RUNNING"
        task.error_reason = ""
        state = _RunState()
        ctx = task.context

        if not self._initialize(task, ctx):
            return task
        self._refresh_page_risk(ctx)

        while True:
            if state.steps_executed >= task.max_steps:
                return self._fail(task, "max_steps_exceeded")

            state.health_failures = self._probe_health(state.health_failures)
            if state.health_failures >= HEALTH_FAILURE_THRESHOLD:
                self._recover_crash(task, ctx, state)
                state.health_failures = 0
                if task.status != "RUNNING":
                    return task

            remaining = [
                s
                for s in task.plan
                if s.step_id not in state.completed and s.step_id not in state.skipped
            ]
            ready = [
                s
                for s in remaining
                if all(d in state.completed or d in state.skipped for d in s.depend_on)
            ]
            if not ready:
                if not remaining:
                    task.status = "SUCCEEDED"
                else:
                    task.status = "FAILED"
                    task.error_reason = "deadlock_or_unresolvable_dependencies"
                return task

            step = ready[0]
            if step.condition and not self._eval_condition(step.condition, ctx):
                state.skipped.add(step.step_id)
                state.steps_executed += 1
                continue

            outcome = self._execute_with_recovery(step, task, ctx, state)
            state.steps_executed += 1

            if outcome == "ok":
                state.completed.add(step.step_id)
                state.consecutive_failures = 0
            elif outcome == "skipped":
                state.skipped.add(step.step_id)
                state.consecutive_failures = 0
            elif outcome == "failed_skip":
                state.skipped.add(step.step_id)
                state.consecutive_failures += 1
            elif outcome == "replan":
                state.consecutive_failures = 0
            elif outcome == "paused":
                return task
            else:  # failed
                state.consecutive_failures += 1
                if self._try_reflect(task, ctx, state):
                    continue
                return task

            if outcome == "failed_skip" and self._try_reflect(task, ctx, state):
                continue

    def _try_reflect(self, task: TaskDefinition, ctx: TaskContext, state: _RunState) -> bool:
        if (
            state.consecutive_failures < 2
            or state.reflections >= MAX_REFLECTIONS
            or not self._reflect(task, ctx, state)
        ):
            return False
        state.reflections += 1
        state.consecutive_failures = 0
        task.status = "RUNNING"
        task.error_reason = ""
        return True

    def _initialize(self, task: TaskDefinition, ctx: TaskContext) -> bool:
        try:
            init = self._engine.initialize_context(
                task_id=task.task_id,
                initial_url=ctx.variables.get("initial_url"),
                attach_cdp_url=ctx.variables.get("attach_cdp_url"),
                context_name=ctx.variables.get("context_name") or "",
                # 互斥键（P1 同站点跨 session 复用）：同键任务共享同窗、后来者不重导航。
                mutex_key=ctx.variables.get("mutex_key"),
                require_existing_session=bool(ctx.variables.get("require_existing_session")),
            )
        except EngineError as exc:
            return self._fail(task, f"initialize_context: {exc.reason or exc}") is None
        if not init.ok:
            return self._fail(task, f"initialize_context: {init.error_reason}") is None
        ctx.current_target_id = init.target_id
        ctx.current_state = "IDLE"
        if not ctx.variables.get("initial_url") and init.current_url:
            # 续接已有 session 时把引擎观察到的实际 URL 回传给 CLI；不把空请求
            # 错报为 about:blank，也不执行导航。
            ctx.variables["initial_url"] = init.current_url
        return True

    def _probe_health(self, health_failures: int) -> int:
        try:
            status = self._engine.health_check()
        except EngineError:
            return health_failures + 1
        return 0 if status.ok else health_failures + 1

    def _execute_with_recovery(
        self, step: ActionNode, task: TaskDefinition, ctx: TaskContext, state: _RunState
    ) -> str:
        """返回: ok | skipped | failed_skip | replan | failed | paused"""
        loop = step.loop or {}
        if loop:
            loop_max = self._loop_max(step, ctx)
            for iteration in range(loop_max):
                outcome = self._execute_once(step, task, ctx, state, iteration=iteration)
                if outcome != "ok":
                    return outcome
                exit_condition = loop.get("exit_condition")
                if exit_condition and self._eval_condition(str(exit_condition), ctx):
                    break
            return "ok"
        return self._execute_once(step, task, ctx, state, iteration=0)

    def _execute_once(
        self,
        step: ActionNode,
        task: TaskDefinition,
        ctx: TaskContext,
        state: _RunState,
        iteration: int = 0,
    ) -> str:
        while True:
            guard_reason = guard_before_execute(step, ctx)
            if guard_reason:
                task.failed_step_id = step.step_id
                task.status = "PAUSED"
                task.error_reason = guard_reason
                return "paused"
            try:
                response = self._execute_step(step, task, ctx, state, iteration=iteration)
                break
            except EngineError as exc:
                decision = self._reflector.classify(exc, step)
                if decision.action is FailureAction.RETRY:
                    self._clock.sleep(decision.wait_seconds)
                    continue
                if decision.action is FailureAction.REINIT:
                    self._recover_crash(task, ctx, state)
                    if task.status != "RUNNING":
                        return "failed"
                    continue
                if decision.action is FailureAction.REPLAN:
                    if self._replan(step, task, ctx, state):
                        return "replan"
                    if step.on_failure == "skip":
                        return "failed_skip"
                    # 原失败原因（如 not_found）不能丢：replan 无模板时也要让上层
                    # 知道“先是因为什么失败”，否则只剩 replan_failed 太笼统。
                    return self._fail_step(
                        task, step, f"{decision.reason} → replan 无可用模板"
                    )
                if decision.action is FailureAction.SKIP:
                    return "skipped"
                if decision.action is FailureAction.PAUSE:
                    task.failed_step_id = step.step_id
                    task.status = "PAUSED"
                    task.error_reason = decision.reason
                    return "paused"
                if decision.action is FailureAction.VLM:
                    outcome = self._solve_challenge(step, task, ctx, state, iteration)
                    if outcome == "retry":
                        continue  # 挑战已解除 → 重试当前步骤（原任务流无缝继续）
                    return outcome
                return self._fail_step(task, step, decision.reason)

        ctx.history.append(response)
        if response.decision_trace_id:
            ctx.decision_trace.append(response.decision_trace_id)
        if response.context_state == "NAVIGATED":
            ctx.navigation_counter += 1
            if response.current_url:
                ctx.visited_urls.add(response.current_url)
            self._refresh_page_risk(ctx)
        elif response.context_state == "NEW_WINDOW_OPENED":
            ctx.navigation_counter += 1
            self._refresh_page_risk(ctx)
            # v1.5.8（2026-09-22 P0-1）：引擎已默认接管新窗口/新标签页——此时
            # active_target_id == new_target_id，后续动作直接作用在新页（微信快捷登录
            # 的授权弹窗由此全自动点「允许」，无需人工切换 Tab）。协调器只需把钉扎的
            # Target 前移到新页，任务继续走完当前步骤。
            new_target = getattr(response, "new_target_id", "") or ""
            active_target = getattr(response, "active_target_id", "") or ""
            if new_target and active_target == new_target:
                ctx.current_target_id = active_target
                ctx.current_state = response.context_state
                return "ok"
            # 引擎未接管（开关关闭 / 接管失败）：保持 v1.5.5 的暂停语义，避免在旧页上
            # 误续动作（TDD 反模式 #12 落地形态）。
            task.status = "PAUSED"
            task.error_reason = (
                "new_window_opened：动作打开了新窗口/标签页（同属会话 "
                f"{ctx.variables.get('context_name') or 'default'}，原 Tab 仍保留）；"
                "引擎本次未接管（接管失败或 follow_new_target=0），请用 "
                "uws probe --attach debug --keep-open 确认页面，再决定继续操作的目标。"
            )
            return "paused"
        ctx.current_state = response.context_state
        # DSL `| $var` 运行时绑定：extract 成功结果写回任务变量（v0.1 打通）。
        # 非 extract 动作 extracted_data 为 None，不写（搜索宏绑定点击步时自然跳过）。
        if response.extracted_data is not None:
            var = task.bindings.get(step.step_id)
            if var:
                ctx.variables[var] = response.extracted_data
        return "ok"

    def _solve_challenge(
        self,
        step: ActionNode,
        task: TaskDefinition,
        ctx: TaskContext,
        state: _RunState,
        iteration: int,
    ) -> str:
        """CHALLENGE_INTERCEPTED → 截图 → VLM 坐标 → 原生拖拽解锁（TDD-COORD-04）。

        返回:
          "retry" — 求解 + 拖拽成功，调用方重试当前步骤；
          "paused" — 无求解器 / 求解耗尽 / 截图或拖拽失败，任务置 PAUSED。
        """
        if self._vlm_solver is None:
            task.failed_step_id = step.step_id
            task.status = "PAUSED"
            task.error_reason = "challenge_intercepted"
            return "paused"
        if state.vlm_solves >= self._max_vlm_solves:
            task.failed_step_id = step.step_id
            task.status = "PAUSED"
            task.error_reason = "challenge_solve_exhausted"
            return "paused"
        state.vlm_solves += 1
        try:
            screenshot = self._engine.capture_screenshot(ctx.current_target_id or "")
            solution = self._vlm_solver.solve(screenshot, step.target_semantic)
            drag = self._engine.execute_action(
                intent_id=(
                    f"intent-{task.task_id}-{step.step_id}-{state.session_epoch}-"
                    f"{iteration}-solve-{state.vlm_solves}"
                ),
                target_semantic="challenge_slider",
                expected_action="drag",
                value=solution.coords(),
                context_hint={
                    "step_id": step.step_id,
                    "solve": "vlm",
                    "context": ctx.variables.get("context_name") or "",
                    "target_id": ctx.current_target_id or "",
                },
                timeout_ms=step.timeout_ms,
            )
        except EngineError as exc:
            task.failed_step_id = step.step_id
            task.status = "PAUSED"
            task.error_reason = f"challenge_solve_failed: {exc.reason or exc}"
            return "paused"
        if not drag.success:
            task.failed_step_id = step.step_id
            task.status = "PAUSED"
            task.error_reason = drag.error_reason or "challenge_solve_failed"
            return "paused"
        return "retry"

    def _execute_step(
        self,
        step: ActionNode,
        task: TaskDefinition,
        ctx: TaskContext,
        state: _RunState,
        iteration: int = 0,
    ) -> ActionResult:
        # 同一步骤的重试复用同一 intent_id（命中引擎 60s 幂等/在途去重）；
        # 会话 epoch 与循环迭代保证跨恢复/跨迭代不误命中（R4）。
        response = self._engine.execute_action(
            intent_id=f"intent-{task.task_id}-{step.step_id}-{state.session_epoch}-{iteration}",
            target_semantic=step.target_semantic,
            expected_action=step.action_type,
            value=step.value,
            context_hint={
                "step_id": step.step_id,
                "context": task.context.variables.get("context_name") or "",
                "target_id": task.context.current_target_id or "",
            },
            timeout_ms=step.timeout_ms,
            node_id=step.node_id,
        )
        response.step_id = step.step_id
        return response

    def _replan(
        self, step: ActionNode, task: TaskDefinition, ctx: TaskContext, state: _RunState
    ) -> bool:
        try:
            plan = self._planner.replan(task.name, ctx, failed_step=step)
        except (NoTemplateMatch, PlanValidationError):
            return False
        index = next(
            (i for i, s in enumerate(task.plan) if s.step_id == step.step_id), len(task.plan)
        )
        task.plan = task.plan[:index] + plan.steps
        ids = {s.step_id for s in task.plan}
        state.completed &= ids
        state.skipped &= ids
        return True

    def _recover_crash(
        self, task: TaskDefinition, ctx: TaskContext, state: _RunState
    ) -> None:
        state.session_epoch += 1  # 新引擎会话：重放使用新 intent_id（R4）
        snapshot_url = self._last_navigated_url(ctx)
        try:
            init = self._engine.initialize_context(
                task_id=task.task_id,
                initial_url=snapshot_url,
                attach_cdp_url=ctx.variables.get("attach_cdp_url"),
                context_name=ctx.variables.get("context_name") or "",
                mutex_key=ctx.variables.get("mutex_key"),
            )
        except EngineError as exc:
            self._fail(task, f"recover: {exc.reason or exc}")
            return
        if not init.ok:
            self._fail(task, f"recover: {init.error_reason}")
            return
        ctx.current_target_id = init.target_id

        # 只重放已完成的幂等步骤；显式 non_idempotent（支付/发帖）绝不自动重放
        for step in task.plan:
            if step.step_id in state.completed and self._is_idempotent(step):
                try:
                    response = self._execute_step(step, task, ctx, state, iteration=0)
                except EngineError as exc:
                    self._fail(task, f"replay {step.step_id}: {exc.reason or exc}")
                    return
                ctx.history.append(response)

    @staticmethod
    def _last_navigated_url(ctx: TaskContext) -> str | None:
        for entry in reversed(ctx.history):
            if entry.context_state == "NAVIGATED" and entry.current_url:
                return entry.current_url
        return None

    def _refresh_page_risk(self, ctx: TaskContext) -> None:
        """导航后扫描页面付费 CTA 并写入 ctx.page_risk；探针/wasm 缺位时静默降级。"""
        target_id = ctx.current_target_id or ""
        if not target_id:
            return
        try:
            tree = self._engine.export_page_tree(target_id) or ""
        except Exception:  # noqa: BLE001 —— 探针缺位不应阻断执行，仅失去页面级护栏
            return
        ctx.page_risk = page_cta_marker(tree) or ctx.page_risk

    @staticmethod
    def _is_idempotent(step: ActionNode) -> bool:
        return not step.non_idempotent and step.action_type in IDEMPOTENT_ACTIONS

    def _reflect(
        self, task: TaskDefinition, ctx: TaskContext, state: _RunState
    ) -> bool:
        rollback_id = self._reflector.rollback_point(ctx.history, task.plan)
        if rollback_id is None:
            return False
        seen = False
        for step in task.plan:
            if step.step_id == rollback_id:
                seen = True
            if seen:
                state.completed.discard(step.step_id)
                state.skipped.discard(step.step_id)
        return True

    @staticmethod
    def _eval_condition(expr: str, ctx: TaskContext) -> bool:
        """解析并求值条件表达式；无法解析的安全默认不执行（False）。

        变量缺失时沿用 v1.4 既有语义：``==`` 为 False、``!=`` 为 True（安全默认）。
        布尔字面量兼容 ``True``/``"true"``/``'true'``；列表 RHS 支持子串/成员判定。
        """
        match = CONDITION_RE.match(expr)
        if not match:
            return False  # 无法解析的分支条件默认不执行（安全默认）
        variable, op, raw = match.group(1), match.group(2).replace(" ", ""), match.group(3)
        value = ctx.variables.get(variable, "")
        literal = _parse_literal(raw)
        if op == "==":
            return _eq(value, literal)
        if op == "!=":
            return not _eq(value, literal)
        if op == "in":
            return _contains(value, literal)
        if op == "notin":
            return not _contains(value, literal)
        return False

    @staticmethod
    def _loop_max(step: ActionNode, ctx: TaskContext) -> int:
        raw = (step.loop or {}).get("max_iterations", 1)
        if isinstance(raw, str) and raw.strip().startswith("{{"):
            key = raw.strip()[2:-2].strip()
            try:
                return int(ctx.variables.get(key, 1))
            except (TypeError, ValueError):
                return 1
        try:
            return int(raw)
        except (TypeError, ValueError):
            return 1

    @staticmethod
    def _fail(task: TaskDefinition, reason: str) -> str:
        task.status = "FAILED"
        task.error_reason = reason
        return "failed"

    @staticmethod
    def _fail_step(task: TaskDefinition, step: ActionNode, reason: str) -> str:
        """终结失败并记录失败步骤：Failure 报告需要知道“卡在哪一步”。"""
        task.failed_step_id = step.step_id
        return Executor._fail(task, reason)
