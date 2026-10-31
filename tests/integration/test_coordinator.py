"""协调器集成测试（TDD-COORD-01/02/03/06/07/09、TDD-PLAN-03；tdd-checklist.md §4）。

引擎侧用 FakeEngine 脚本化响应（确定性、零外部依赖）；另含一个真实引擎冒烟
用例（conftest `engine` 夹具），验证生产 gRPC 客户端接线。
"""

from __future__ import annotations

from collections import deque

import pytest

from coordinator.engine_client import (
    DEADLINE_EXCEEDED,
    FAILED_PRECONDITION,
    INTERNAL,
    NOT_FOUND,
    UNAUTHENTICATED,
    ActionResult,
    EngineError,
    HealthStatus,
    InitResult,
)
from coordinator.executor import Executor
from coordinator.models import ActionNode, TaskContext, TaskDefinition
from coordinator.planner import PlanValidationError, Planner, TemplateRegistry
from coordinator.vlm import MockVlmSolver

COORD_KEY = "coordinator-test-key"


class FakeEngine:
    """脚本化引擎：execute_action 按 target_semantic 弹出预设结果序列。"""

    def __init__(self) -> None:
        self.calls: list[tuple[str, dict]] = []
        self.script: dict[str, deque] = {}
        self.health_script: deque = deque()
        self.init_script: deque = deque()
        self.attach_calls: list[str] = []
        self.screenshot_calls: list[str] = []
        self.screenshot_bytes = b"fake-png"
        self.current_url = "https://example.com/login"

    def initialize_context(
        self, task_id: str, initial_url: str | None = None, **kwargs: object
    ) -> InitResult:
        self.calls.append(("init", {"task_id": task_id, "initial_url": initial_url}))
        outcome = self.init_script.popleft() if self.init_script else True
        if outcome is False:
            return InitResult(ok=False, error_reason="init_failed")
        return InitResult(ok=True, target_id=f"target-{len(self.calls)}", context_id="ctx")

    def execute_action(
        self,
        intent_id: str,
        target_semantic: str,
        expected_action: str,
        value: str | None = None,
        context_hint: dict[str, str] | None = None,
        timeout_ms: int = 1500,
        node_id: int | None = None,
    ) -> ActionResult:
        self.calls.append(
            (
                "execute",
                {
                    "target_semantic": target_semantic,
                    "expected_action": expected_action,
                    "value": value,
                    "intent_id": intent_id,
                    "node_id": node_id,
                },
            )
        )
        outcomes = self.script.get(target_semantic)
        if outcomes:
            outcome = outcomes.popleft()
            if isinstance(outcome, EngineError):
                raise outcome
            if not outcome.decision_trace_id:
                outcome.decision_trace_id = f"trace-{len(self.calls)}"
            return outcome
        return ActionResult(
            success=True,
            context_state="STABLE",
            decision_trace_id=f"trace-{len(self.calls)}",
            current_url=self.current_url,
        )

    def export_page_tree(self, target_id: str) -> str:
        self.calls.append(("export_tree", {"target_id": target_id}))
        # 通用夹具页面不含付费 CTA，避免触发执行护栏；付费 CTA 场景见
        # tests/unit/test_cli_run.py BillingPageFakeEngine / guard 测试。
        return "[1] button(button) \"提交\"\n[2] input(textbox) \"验证码\"\n"

    def health_check(self) -> HealthStatus:
        self.calls.append(("health", {}))
        outcome = self.health_script.popleft() if self.health_script else True
        return HealthStatus(ok=bool(outcome))

    def capture_screenshot(self, target_id: str) -> bytes:
        self.screenshot_calls.append(target_id)
        return self.screenshot_bytes

    def attach_target(self, target_id: str) -> str:
        self.attach_calls.append(target_id)
        return "target-new"

    def executed(self, semantic: str) -> list[tuple[str, dict]]:
        return [c for c in self.calls if c[0] == "execute" and c[1]["target_semantic"] == semantic]

    def drags(self) -> list[tuple[str, dict]]:
        return [
            c for c in self.calls if c[0] == "execute" and c[1]["expected_action"] == "drag"
        ]


class FakeClock:
    def __init__(self) -> None:
        self.now = 0.0

    def sleep(self, seconds: float) -> None:
        self.now += seconds


def _task(plan: list[ActionNode], task_id: str = "task-1", variables: dict | None = None) -> TaskDefinition:
    return TaskDefinition(
        task_id=task_id,
        name="测试任务",
        plan=plan,
        context=TaskContext(variables=variables or {}),
    )


def test_tdd_coord_01_fibonacci_retry():
    engine = FakeEngine()
    engine.script["password_input"] = deque(
        [
            EngineError(DEADLINE_EXCEEDED, "timeout"),
            EngineError(DEADLINE_EXCEEDED, "timeout"),
            ActionResult(success=True, context_state="STABLE"),
        ]
    )
    engine.script["click_submit"] = deque(
        [
            ActionResult(
                success=True,
                context_state="NAVIGATED",
                current_url="https://example.com/home",
            )
        ]
    )
    clock = FakeClock()
    plan = [
        ActionNode("s1", "input", "username_input"),
        ActionNode("s2", "input", "password_input", depend_on=["s1"]),
        ActionNode("s3", "click", "click_submit", depend_on=["s2"]),
    ]
    task = _task(plan)

    Executor(engine, clock=clock).run(task)

    assert task.status == "SUCCEEDED"
    assert len(engine.executed("password_input")) == 3  # 1 初试 + 2 重试
    assert clock.now == 3.0  # Fibonacci 1s + 2s
    assert len(engine.executed("click_submit")) == 1  # 重试成功后继续后续步骤
    assert task.context.navigation_counter == 1
    assert "https://example.com/home" in task.context.visited_urls


def test_tdd_sched_10_max_steps_budget_exhausted():
    """T-10 调度预算耗尽：max_steps 是任务级硬预算，用尽即 FAILED
    `max_steps_exceeded`，剩余步骤不得再被派发到引擎（预算的拥有者是协调器，
    引擎只看到一个个 intent，无法替它收敛任务级预算）。"""
    engine = FakeEngine()
    plan = [
        ActionNode("s1", "click", "step_a"),
        ActionNode("s2", "click", "step_b"),
        ActionNode("s3", "click", "step_c"),
    ]
    task = _task(plan)
    task.max_steps = 2

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "FAILED"
    assert task.error_reason == "max_steps_exceeded"
    dispatched = [c[1]["target_semantic"] for c in engine.calls if c[0] == "execute"]
    assert dispatched == ["step_a", "step_b"]  # s3 从未被派发


def test_tdd_sched_10_deadline_retry_budget_exhausted():
    """T-10 重试预算耗尽：引擎持续 DEADLINE_EXCEEDED 时协调器只重试 2 次
    （Fibonacci 1s/2s），第 3 次失败即终结，不产生第 4 次物理提交。"""
    engine = FakeEngine()
    engine.script["submit"] = deque(
        [EngineError(DEADLINE_EXCEEDED, "timeout")] * 3
    )
    clock = FakeClock()
    task = _task([ActionNode("s1", "click", "submit")])

    Executor(engine, clock=clock).run(task)

    assert task.status == "FAILED"
    assert task.error_reason == "deadline_exceeded_exhausted"
    assert task.failed_step_id == "s1"
    assert len(engine.executed("submit")) == 3  # 1 初试 + 2 重试
    assert clock.now == 3.0  # Fibonacci 1s + 2s，预算用尽后不再退避


def test_r4_retry_reuses_intent_id_and_epoch_bump_on_reinit():
    """R4 回归：单步重试复用同一 intent_id（命中引擎幂等/在途去重）；
    崩溃恢复（REINIT）后重放步骤使用新会话 epoch 的 intent_id，
    避免新上下文吃到旧会话 60s 幂等缓存。"""
    engine = FakeEngine()
    engine.script["submit"] = deque(
        [
            EngineError(DEADLINE_EXCEEDED, "timeout"),
            EngineError(DEADLINE_EXCEEDED, "timeout"),
            ActionResult(success=True, context_state="STABLE"),
        ]
    )
    # 前 3 次健康探测失败 → 触发崩溃恢复（recover 重放幂等步骤）
    engine.health_script = deque([False, False, False])
    plan = [
        ActionNode("s1", "input", "username"),
        ActionNode("s2", "click", "submit", depend_on=["s1"]),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    submit_calls = engine.executed("submit")
    assert len(submit_calls) == 3  # 1 初试 + 2 重试
    assert {c[1]["intent_id"] for c in submit_calls} == {"intent-task-1-s2-0-0"}

    username_calls = engine.executed("username")
    assert len(username_calls) == 2  # 首次 + 崩溃恢复重放
    assert [c[1]["intent_id"] for c in username_calls] == [
        "intent-task-1-s1-0-0",
        "intent-task-1-s1-1-0",  # 新会话 epoch=1
    ]


def test_r4_loop_iterations_use_distinct_intent_ids():
    """R4 回归：同一 loop 步骤的每次迭代使用独立 intent_id（不误命中幂等缓存）。"""
    engine = FakeEngine()
    plan = [
        ActionNode("s1", "click", "next_page", loop={"max_iterations": 3}),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    ids = [c[1]["intent_id"] for c in engine.executed("next_page")]
    assert ids == ["intent-task-1-s1-0-0", "intent-task-1-s1-0-1", "intent-task-1-s1-0-2"]


def test_tdd_coord_02_not_found_replan():
    engine = FakeEngine()
    engine.script["click_submit"] = deque([EngineError(NOT_FOUND, "no trusted node")])
    llm_calls: list[int] = []

    def llm(query, snippets):
        llm_calls.append(1)
        return {
            "steps": [
                {"action_type": "click", "target_semantic": "submit_alternative", "depends_on": []},
                {
                    "action_type": "wait",
                    "target_semantic": "NAVIGATED",
                    "value": "3000",
                    "depends_on": ["submit_alternative"],
                },
            ]
        }

    planner = Planner(TemplateRegistry(), llm=llm)
    plan = [
        ActionNode("s1", "input", "username_input"),
        ActionNode("s2", "click", "click_submit", depend_on=["s1"], on_failure="replan"),
    ]
    task = _task(plan)

    Executor(engine, planner=planner, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    assert llm_calls  # NOT_FOUND → 调用了 planner 局部重规划，不死循环
    assert len(engine.executed("click_submit")) == 1  # 失败步骤不盲目重试
    assert len(engine.executed("submit_alternative")) == 1
    assert len(engine.executed("NAVIGATED")) == 1


def test_tdd_coord_03_crash_replay_idempotent_only():
    engine = FakeEngine()
    engine.script["input_user"] = deque(
        [
            ActionResult(success=True, context_state="STABLE"),
            ActionResult(success=True, context_state="STABLE"),  # 崩溃恢复重放
        ]
    )
    engine.script["click_pay"] = deque([ActionResult(success=True, context_state="STABLE")])
    engine.script["input_next"] = deque(
        [
            EngineError(FAILED_PRECONDITION, "target crashed"),
            ActionResult(success=True, context_state="STABLE"),
        ]
    )
    plan = [
        ActionNode("s1", "input", "input_user"),
        ActionNode("s2", "click", "click_pay", depend_on=["s1"], non_idempotent=True),
        ActionNode("s3", "input", "input_next", depend_on=["s2"]),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    assert len(engine.executed("input_user")) == 2  # 幂等步骤恢复后重放
    assert len(engine.executed("click_pay")) == 1  # 非幂等（支付）绝不重放
    assert len(engine.executed("input_next")) == 2  # 崩溃失败 1 次 + 恢复后成功
    inits = [c[1] for c in engine.calls if c[0] == "init"]
    assert len(inits) == 2  # 重新初始化环境


def test_tdd_coord_06_crash_recovery_snapshot_and_full_rerun():
    # 场景一：存在 NAVIGATED 快照 → 恢复时导航至快照 URL，重放幂等步骤
    engine = FakeEngine()
    engine.health_script = deque([True, False, False, False, True])
    engine.script["nav_link"] = deque(
        [
            ActionResult(
                success=True,
                context_state="NAVIGATED",
                current_url="https://example.com/app",
            )
        ]
    )
    plan = [
        ActionNode("s1", "click", "nav_link"),
        ActionNode("s2", "input", "username_input", depend_on=["s1"]),
        ActionNode("s3", "extract", "product_card_list", depend_on=["s2"]),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    inits = [c[1] for c in engine.calls if c[0] == "init"]
    assert len(inits) == 2
    assert inits[1]["initial_url"] == "https://example.com/app"
    assert len(engine.executed("username_input")) == 2  # 幂等步骤重放
    assert len(engine.executed("nav_link")) == 1  # click 非幂等不重放
    assert task.context.navigation_counter == 1

    # 场景二：无 NAVIGATED 快照 → 全量重跑
    engine2 = FakeEngine()
    engine2.health_script = deque([True, False, False, False, True])
    plan2 = [
        ActionNode("s1", "input", "username_input"),
        ActionNode("s2", "extract", "product_card_list", depend_on=["s1"]),
        ActionNode("s3", "focus", "search_box", depend_on=["s2"]),
    ]
    task2 = _task(plan2, task_id="task-2")

    Executor(engine2, clock=FakeClock()).run(task2)

    assert task2.status == "SUCCEEDED"
    inits2 = [c[1] for c in engine2.calls if c[0] == "init"]
    assert len(inits2) == 2
    assert inits2[1]["initial_url"] is None
    assert len(engine2.executed("username_input")) == 2
    assert len(engine2.executed("product_card_list")) == 2


def test_tdd_coord_07_reflection_rollback_stable():
    engine = FakeEngine()
    engine.script["step_d"] = deque(
        [
            EngineError(NOT_FOUND, "drift"),
            ActionResult(success=True, context_state="STABLE"),
        ]
    )
    engine.script["step_e"] = deque(
        [
            EngineError(NOT_FOUND, "drift"),
            ActionResult(success=True, context_state="STABLE"),
        ]
    )

    def llm(query, snippets):
        return {"nonsense": True}  # 重规划失败 → 触发反思回退

    planner = Planner(TemplateRegistry(), llm=llm)
    plan = [
        ActionNode("a", "click", "step_a"),
        ActionNode("b", "click", "step_b", depend_on=["a"]),
        ActionNode("c", "click", "step_c", depend_on=["b"]),
        ActionNode("d", "click", "step_d", depend_on=["c"], on_failure="skip"),
        ActionNode("e", "click", "step_e", depend_on=["d"], on_failure="skip"),
    ]
    task = _task(plan)

    Executor(engine, planner=planner, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    assert len(engine.executed("step_c")) == 2  # 回退最近 STABLE 步骤重放
    assert len(engine.executed("step_d")) == 2
    assert len(engine.executed("step_e")) == 2
    assert len(task.context.history) >= 5
    assert len(task.context.decision_trace) >= 5  # 反思收集最近 5 步 decision_trace


def test_tdd_coord_09_new_window_attach():
    engine = FakeEngine()
    engine.script["open_link"] = deque(
        [ActionResult(success=True, context_state="NEW_WINDOW_OPENED")]
    )
    plan = [
        ActionNode("s1", "click", "open_link"),
        ActionNode("s2", "input", "username_input", depend_on=["s1"]),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    # v1.5.5+ 契约：新窗口/新标签与任务同属会话，但引擎尚无「活动 Tab 自动切换」，
    # executor 暂停交 Agent/用户确认，不再静默自动 Attach 后继续（反模式 #12）。
    assert task.status == "PAUSED"
    assert "新窗口" in task.error_reason or "new_window" in task.error_reason
    assert engine.attach_calls == []


def test_tdd_coord_09b_new_window_takeover_continues():
    """v1.5.8（2026-09-22 P0-1）：引擎回报 new==active target（已接管）→ 无缝续作。

    现场根因：微信快捷登录的授权弹窗必须人工点「允许」，就是因为引擎不接管新 Tab。
    """
    engine = FakeEngine()
    engine.script["open_link"] = deque(
        [
            ActionResult(
                success=True,
                context_state="NEW_WINDOW_OPENED",
                new_target_id="popup-1",
                active_target_id="popup-1",
            )
        ]
    )
    plan = [
        ActionNode("s1", "click", "open_link"),
        ActionNode("s2", "input", "username_input", depend_on=["s1"]),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    assert task.context.current_target_id == "popup-1"


def test_tdd_coord_09c_new_window_not_taken_over_pauses():
    """引擎未接管（开关关闭/接管失败）时保持暂停语义，绝不在旧页上误续动作。"""
    engine = FakeEngine()
    engine.script["open_link"] = deque(
        [
            ActionResult(
                success=True,
                context_state="NEW_WINDOW_OPENED",
                new_target_id="popup-1",
                active_target_id="",
            )
        ]
    )
    plan = [
        ActionNode("s1", "click", "open_link"),
        ActionNode("s2", "input", "username_input", depend_on=["s1"]),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "PAUSED"
    assert engine.attach_calls == []


def test_tdd_coord_04_vlm_solve_unlocks_and_continues():
    """CHALLENGE_INTERCEPTED → 截图 → Mock VLM 坐标 → 原生拖拽 → 原任务无缝继续。"""
    engine = FakeEngine()
    engine.script["login_btn"] = deque(
        [
            EngineError(UNAUTHENTICATED, "challenge_intercepted"),
            ActionResult(success=True, context_state="STABLE"),
        ]
    )
    solver = MockVlmSolver()
    plan = [
        ActionNode("s1", "click", "login_btn"),
        ActionNode("s2", "input", "username_input", depend_on=["s1"]),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock(), vlm_solver=solver).run(task)

    assert task.status == "SUCCEEDED"
    assert engine.screenshot_calls == ["target-1"]  # 已抓取当前 Target 截图
    assert solver.calls == [(b"fake-png", "login_btn")]  # 截图 + 挑战语义交给 VLM
    drags = engine.drags()
    assert len(drags) == 1
    assert drags[0][1]["value"] == "100,200,300,200"  # Mock 坐标下发生物理拖拽
    assert drags[0][1]["target_semantic"] == "challenge_slider"
    assert drags[0][1]["intent_id"] == "intent-task-1-s1-0-0-solve-1"
    assert len(engine.executed("login_btn")) == 2  # 解锁后重试原步骤成功
    assert len(engine.executed("username_input")) == 1  # 后续步骤继续执行


def test_tdd_coord_04_vlm_solve_exhausted_pauses():
    """求解成功但动作仍被拦截：最多求解 3 次后置 PAUSED，不死循环。"""
    engine = FakeEngine()
    engine.script["login_btn"] = deque(
        [EngineError(UNAUTHENTICATED, "challenge_intercepted")] * 10
    )
    solver = MockVlmSolver()
    plan = [ActionNode("s1", "click", "login_btn")]
    task = _task(plan)

    Executor(engine, clock=FakeClock(), vlm_solver=solver).run(task)

    assert task.status == "PAUSED"
    assert task.error_reason == "challenge_solve_exhausted"
    assert len(engine.drags()) == 3  # 求解次数封顶
    assert len(engine.executed("login_btn")) == 4  # 1 初试 + 3 次解锁后重试


def test_vlm_max_solves_override():
    """max_vlm_solves 可配置覆盖（vlm.max_solves）：1 次求解后即 PAUSED。"""
    engine = FakeEngine()
    engine.script["login_btn"] = deque(
        [EngineError(UNAUTHENTICATED, "challenge_intercepted")] * 10
    )
    plan = [ActionNode("s1", "click", "login_btn")]
    task = _task(plan)

    Executor(engine, clock=FakeClock(), vlm_solver=MockVlmSolver(), max_vlm_solves=1).run(task)

    assert task.status == "PAUSED"
    assert task.error_reason == "challenge_solve_exhausted"
    assert len(engine.drags()) == 1
    assert len(engine.executed("login_btn")) == 2  # 1 初试 + 1 次解锁后重试


def test_tdd_coord_04_vlm_no_solver_paused():
    """未配置 VLM 求解器：保持既有降级语义——任务置 PAUSED，不拖拽不重试。"""
    engine = FakeEngine()
    engine.script["login_btn"] = deque(
        [EngineError(UNAUTHENTICATED, "challenge_intercepted")]
    )
    plan = [ActionNode("s1", "click", "login_btn")]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "PAUSED"
    assert task.error_reason == "challenge_intercepted"
    assert engine.drags() == []
    assert len(engine.executed("login_btn")) == 1


def test_tdd_coord_04_vlm_drag_failure_paused():
    """截图或拖拽执行失败：任务置 PAUSED 并给出可观测原因。"""
    engine = FakeEngine()
    engine.script["login_btn"] = deque(
        [EngineError(UNAUTHENTICATED, "challenge_intercepted")]
    )
    engine.script["challenge_slider"] = deque(
        [EngineError(INTERNAL, "drag failed")]
    )
    plan = [ActionNode("s1", "click", "login_btn")]
    task = _task(plan)

    Executor(engine, clock=FakeClock(), vlm_solver=MockVlmSolver()).run(task)

    assert task.status == "PAUSED"
    assert task.error_reason == "challenge_solve_failed: drag failed"
    assert len(engine.drags()) == 1


def test_tdd_plan_03_llm_completion_json_schema():
    registry = TemplateRegistry()

    def llm_ok(query, snippets):
        return {
            "steps": [
                {
                    "action_type": "input",
                    "target_semantic": "username_input",
                    "value": "alice",
                    "depends_on": [],
                },
                {
                    "action_type": "click",
                    "target_semantic": "submit_btn",
                    "depends_on": ["username_input"],  # 语义依赖 → 重映射为 step_id
                },
            ]
        }

    planner = Planner(registry, llm=llm_ok)
    plan = planner.plan("全新任务", {"username": "alice"}, allow_llm=True)
    assert plan.source == "l3_llm"
    assert [s.action_type for s in plan.steps] == ["input", "click"]
    assert plan.steps[1].depend_on == [plan.steps[0].step_id]

    def llm_bad(query, snippets):
        return {
            "steps": [
                {"action_type": "teleport", "target_semantic": "x", "depends_on": []},
                {"action_type": "click", "target_semantic": "y", "depends_on": ["missing"]},
            ]
        }

    with pytest.raises(PlanValidationError):
        Planner(registry, llm=llm_bad).plan("全新任务", {}, allow_llm=True)


def test_tdd_plan_05_condition_branch_bool_and_string():
    """条件分支语义：`{{ has_captcha }} == true` 为真执行、为假跳过，主流程继续。"""
    plan = [
        ActionNode("s1", "click", "captcha_ok", condition="{{ has_captcha }} == true"),
        ActionNode("s2", "click", "submit_btn"),
    ]

    # 布尔 True → 分支执行。
    engine = FakeEngine()
    task = _task(plan, variables={"has_captcha": True})
    Executor(engine, clock=FakeClock()).run(task)
    assert task.status == "SUCCEEDED"
    assert len(engine.executed("captcha_ok")) == 1
    assert len(engine.executed("submit_btn")) == 1

    # 布尔 False → 分支跳过，不报错、主流程继续。
    engine2 = FakeEngine()
    task2 = _task(plan, variables={"has_captcha": False})
    Executor(engine2, clock=FakeClock()).run(task2)
    assert task2.status == "SUCCEEDED"
    assert engine2.executed("captcha_ok") == []
    assert len(engine2.executed("submit_btn")) == 1

    # 字符串 "true" 与布尔字面量等价。
    engine3 = FakeEngine()
    task3 = _task(plan, variables={"has_captcha": "true"})
    Executor(engine3, clock=FakeClock()).run(task3)
    assert len(engine3.executed("captcha_ok")) == 1


def test_tdd_plan_05_condition_in_and_safe_defaults():
    """in/not in 列表语义与安全默认：缺失变量/非法表达式不得破坏执行流。"""
    plan = [
        ActionNode("s1", "click", "review_entry", condition="{{ status }} in ['pending','review']"),
        ActionNode("s2", "click", "next_step"),
    ]

    engine = FakeEngine()
    task = _task(plan, variables={"status": "review"})
    Executor(engine, clock=FakeClock()).run(task)
    assert len(engine.executed("review_entry")) == 1
    assert task.status == "SUCCEEDED"

    engine2 = FakeEngine()
    task2 = _task(plan, variables={"status": "done"})
    Executor(engine2, clock=FakeClock()).run(task2)
    assert engine2.executed("review_entry") == []
    assert task2.status == "SUCCEEDED"

    # 缺失变量：== 安全默认不执行；!= 安全默认执行（沿用 v1.4 既有语义）。
    engine3 = FakeEngine()
    plan3 = [
        ActionNode("s1", "click", "optional", condition="{{ missing }} == 'x'"),
        ActionNode("s2", "click", "fallback", condition="{{ missing }} != 'x'"),
    ]
    task3 = _task(plan3, variables={})
    Executor(engine3, clock=FakeClock()).run(task3)
    assert engine3.executed("optional") == []
    assert len(engine3.executed("fallback")) == 1

    # 无法解析的条件表达式 → 安全默认不执行该分支，不阻塞主流程。
    engine4 = FakeEngine()
    plan4 = [
        ActionNode("s1", "click", "bad_cond", condition="{{ x }} >>> 5"),
        ActionNode("s2", "click", "ok_step"),
    ]
    task4 = _task(plan4, variables={"x": 1})
    Executor(engine4, clock=FakeClock()).run(task4)
    assert engine4.executed("bad_cond") == []
    assert task4.status == "SUCCEEDED"


def test_tdd_plan_05_loop_max_iterations_and_exit_condition():
    """循环语义：max_iterations 封顶安全退出；exit_condition 满足时提前退出。"""
    plan = [
        ActionNode(
            "s1",
            "click",
            "next_page",
            loop={"max_iterations": 5, "exit_condition": "{{ page }} == 'done'"},
        ),
        ActionNode("s2", "click", "finish"),
    ]

    # 退出条件恒假 → 恰好执行 max_iterations 次后安全退出（不无限翻页）。
    engine = FakeEngine()
    task = _task(plan, variables={"page": "p1"})
    Executor(engine, clock=FakeClock()).run(task)
    assert task.status == "SUCCEEDED"
    assert len(engine.executed("next_page")) == 5
    assert len(engine.executed("finish")) == 1

    # 退出条件初始即真 → 执行 1 次后提前退出。
    engine2 = FakeEngine()
    task2 = _task(plan, variables={"page": "done"})
    Executor(engine2, clock=FakeClock()).run(task2)
    assert task2.status == "SUCCEEDED"
    assert len(engine2.executed("next_page")) == 1

    # max_iterations 支持 {{ var }} 插值。
    engine3 = FakeEngine()
    plan3 = [
        ActionNode("s1", "click", "collect", loop={"max_iterations": "{{ pages }}"}),
        ActionNode("s2", "click", "done"),
    ]
    task3 = _task(plan3, variables={"pages": 2})
    Executor(engine3, clock=FakeClock()).run(task3)
    assert len(engine3.executed("collect")) == 2
    assert task3.status == "SUCCEEDED"


def test_tdd_plan_05_loop_failure_handling():
    """循环中途失败不吞错：按失败语义处理（此处 NOT_FOUND → replan 失败 → skip），
    循环立即终止而非继续执行剩余迭代。"""
    engine = FakeEngine()
    engine.script["next_page"] = deque(
        [
            ActionResult(success=True, context_state="STABLE"),
            EngineError(NOT_FOUND, "page gone"),
        ]
    )
    plan = [
        ActionNode(
            "s1", "click", "next_page", loop={"max_iterations": 3}, on_failure="skip"
        ),
        ActionNode("s2", "click", "finish"),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    assert len(engine.executed("next_page")) == 2  # 第 1 次成功，第 2 次失败即终止
    assert task.status == "SUCCEEDED"  # on_failure=skip → 继续后续步骤
    assert len(engine.executed("finish")) == 1


def test_coordinator_real_engine_grpc_client_smoke(engine):
    from coordinator.executor import Executor
    from coordinator.grpc_client import GrpcEngineClient

    client = GrpcEngineClient(port=engine["port"], api_key=COORD_KEY)
    task = _task([ActionNode("s1", "click", "登录")], task_id="coord-smoke")

    Executor(client, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    assert task.context.current_target_id


def test_tdd_plan_08_node_id_threading():
    """混合指挥：executor 把 ActionNode.node_id 透传给引擎（node: 确定性引用）。"""
    engine = FakeEngine()
    plan = [
        ActionNode("s1", "click", "node:12", node_id=12),
        ActionNode("s2", "extract", "node:31", node_id=31, depend_on=["s1"]),
    ]
    task = _task(plan)

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    assert [c[1]["node_id"] for c in engine.executed("node:12")] == [12]
    assert [c[1]["node_id"] for c in engine.executed("node:31")] == [31]


def test_tdd_plan_09_extract_binding_writeback():
    """DSL `| $var` 运行时打通：extract 成功结果写回 TaskContext.variables。"""
    engine = FakeEngine()
    engine.script["成分表"] = deque(
        [ActionResult(success=True, context_state="STABLE", extracted_data="水,甘油,烟酰胺")]
    )
    plan = [ActionNode("s1", "extract", "成分表")]
    task = _task(plan)
    task.bindings = {"s1": "成分"}
    task.result = "$成分"

    Executor(engine, clock=FakeClock()).run(task)

    assert task.status == "SUCCEEDED"
    assert task.context.variables["成分"] == "水,甘油,烟酰胺"
