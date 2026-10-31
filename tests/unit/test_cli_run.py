"""`uws run --plan` DSL 运行时消费（v0.1 打通）单元测试。

不启动真实引擎：注入 FakeEngine 验证 run_task 把 DSL（nav/search/node:/$var/reply）
编译为 TaskDefinition 并消费 initial_url/bindings/result。
"""

from __future__ import annotations

import pytest


@pytest.fixture(autouse=True)
def isolated_attach(monkeypatch):
    """Fake-engine tests never consult or launch a host browser."""
    monkeypatch.setattr("coordinator.cli.resolve_attach", lambda *args, **kwargs: "http://127.0.0.1:9222")

import argparse

from coordinator.cli import run_task
from coordinator.engine_client import (
    NOT_FOUND,
    ActionResult,
    EngineError,
    HealthStatus,
    HitElement,
    InitResult,
)


class PlanFakeEngine:
    def __init__(self) -> None:
        self.executed: list[tuple[str, str, int | None]] = []
        self.drag_calls: list[tuple[str, str]] = []
        self.variables: dict = {}
        self.initial_url: str | None = None

    def initialize_context(self, task_id: str, initial_url: str | None = None, **kwargs: object) -> InitResult:
        self.initial_url = initial_url
        return InitResult(ok=True, target_id="target-1", context_id="ctx")

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
        self.executed.append((expected_action, target_semantic, node_id))
        if expected_action == "drag":
            self.drag_calls.append((target_semantic, value or ""))
        if expected_action == "extract" and target_semantic == "成分表":
            return ActionResult(success=True, context_state="STABLE", extracted_data="水,甘油")
        return ActionResult(success=True, context_state="STABLE")

    def health_check(self) -> HealthStatus:
        return HealthStatus(ok=True)

    def attach_target(self, target_id: str) -> str:
        return "target-new"

    def export_page_tree(self, target_id: str) -> str:
        return "[1] link(link) \"Example Domain\"\n"


class PlanFailEngine(PlanFakeEngine):
    """单击“不存在的选项”必然 NOT_FOUND：验证失败证据（原因+AXTree+截图）落报告。"""

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
        if target_semantic == "不存在的选项":
            raise EngineError(NOT_FOUND, "no_match")
        return super().execute_action(
            intent_id,
            target_semantic,
            expected_action,
            value=value,
            context_hint=context_hint,
            timeout_ms=timeout_ms,
            node_id=node_id,
        )

    def capture_screenshot(self, target_id: str) -> bytes:
        return b"\x89PNG\r\n\x1a\nfake-screenshot"

    def export_page_tree(self, target_id: str) -> str:
        return "[1] button(button) \"不存在的选项\"\n"


class BillingPageFakeEngine(PlanFakeEngine):
    """页面 AXTree 出现付费 CTA：任何后续点击都应在执行前被护栏暂停。"""

    def export_page_tree(self, target_id: str) -> str:
        return "[1] button(button) \"立即购买\"\n[2] button(button) \"下一步\"\n"


def _args(**overrides: object) -> argparse.Namespace:
    base = dict(
        engine_addr="127.0.0.1:50051",
        api_key="",
        target_url="",
        attach="",
        task_file="",
        plan="",
        plan_file="",
        output="/tmp/uws-result.md",
    )
    base.update(overrides)
    return argparse.Namespace(**base)


def test_run_task_plan_consumes_dsl() -> None:
    engine = PlanFakeEngine()
    dsl = "nav:https://example.com\nclick:node:12\nextract:成分表 | $成分\nreply:$成分"

    rc = run_task(_args(plan=dsl, output="/tmp/uws-result.md"), engine=engine)

    assert rc == 0
    assert engine.initial_url == "https://example.com"
    assert ("click", "node:12", 12) in engine.executed
    assert ("extract", "成分表", None) in engine.executed
    # extract 结果经 bindings 写回变量，供 reply:$成分 插值（结果写入 result.md）。
    import pathlib
    out = pathlib.Path("/tmp/uws-result.md").read_text(encoding="utf-8")
    assert "水,甘油" in out


def test_run_task_report_echoes_hit_element(tmp_path) -> None:
    """2026-09-23 证据回显：报告每步带出引擎回执的命中节点。

    extract 的假阳性形态是「报 SUCCEEDED，抽到的却是同名碎片」——调用方只有看到
    命中节点（tag/text/selector）才能识别；此前报告只有 `status=ok`，无从判断。
    """

    class HitFakeEngine(PlanFakeEngine):
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
            self.executed.append((expected_action, target_semantic, node_id))
            return ActionResult(
                success=True,
                context_state="STABLE",
                extracted_data="Example Domains",
                hit_element=HitElement(tag="h1", text="Example Domains", selector="main > h1"),
            )

    out = tmp_path / "uws-hit.md"
    rc = run_task(
        _args(plan="extract:Example Domains | $x\nreply:$x", output=str(out)),
        engine=HitFakeEngine(),
    )

    assert rc == 0
    report = out.read_text(encoding="utf-8")
    assert 'hit=`h1` "Example Domains"' in report
    assert "main > h1" in report


def test_resolve_dsl_result_composite_interpolates() -> None:
    """`reply:` 复合字符串应插值内嵌 `$var`（v1.5.5 `reply:$var` 插值的通用形态）。

    既有实现只处理「整串恰为单个 $var」；复合串（如 `描述：$desc；$count`）
    应逐 token 替换，未绑定变量替换为空串。
    """
    from coordinator.cli import _resolve_dsl_result

    variables = {"成分": "水,甘油", "物": "精华"}
    assert _resolve_dsl_result("成分=$成分 物=$物", variables) == "成分=水,甘油 物=精华"
    assert _resolve_dsl_result("$成分", variables) == "水,甘油"
    assert _resolve_dsl_result("纯字面量", variables) == "纯字面量"
    assert _resolve_dsl_result("缺失=$无此变量", variables) == "缺失="


def test_run_task_plan_drag_passes_coords() -> None:
    """TDD-PLAN-10 运行时：`uws run --plan "drag:..."` 把坐标 value
    透传给引擎 ExecuteAction（expected_action=drag，target 固定语义）。"""
    engine = PlanFakeEngine()
    dsl = "nav:https://example.com\ndrag:124,200,300,200"

    rc = run_task(_args(plan=dsl), engine=engine)

    assert rc == 0
    assert engine.initial_url == "https://example.com"
    assert engine.drag_calls == [("drag", "124,200,300,200")]


def test_run_task_failure_attaches_evidence_report(tmp_path, monkeypatch) -> None:
    """TDD-CLI-20：步骤失败时输出 Failure 报告（失败步骤/原因/AXTree/截图），
    让上层 Agent 知道卡在哪一步、页面现状是什么；VLM 未配置时显式说明。"""
    monkeypatch.setenv("UWS_VLM_API_KEY", "")
    monkeypatch.delenv("DEEPSEEK_API_KEY", raising=False)
    engine = PlanFailEngine()
    out = tmp_path / "uws-result.md"

    rc = run_task(_args(plan="click:不存在的选项", output=str(out)), engine=engine)

    assert rc == 1
    report = out.read_text(encoding="utf-8")
    assert "## Failure" in report
    assert "not_found → replan 无可用模板" in report
    assert "step-s1-failed.png" in report
    assert "不存在的选项" in report
    assert "VLM 诊断未启用" in report
    shot = tmp_path / "uws-result.assets" / "step-s1-failed.png"
    assert shot.read_bytes().startswith(b"\x89PNG")


def test_run_success_prints_current_page_summary(capsys) -> None:
    """TDD-CLI-22：uws run --plan 正常结束时把“停在这里的页面”报清楚
    （当前 URL + AXTree 摘要，含 node_id），Agent 可直接继续下一条 DSL。"""
    engine = PlanFakeEngine()

    rc = run_task(_args(plan="click:站点管理", output=""), engine=engine)

    assert rc == 0
    out = capsys.readouterr().out
    assert "## 当前页面（任务结束点，可继续操作）" in out
    assert "Example Domain" in out
    assert "[1] link(link)" in out


def test_run_task_with_output_still_prints_full_report_to_stdout(tmp_path, capsys) -> None:
    """TDD-CLI-23：即使传了 --output，完整报告仍直接打印 stdout
    （--output 只作持久化副本；Agent 消费 stdout，不需要再读文件）。"""
    engine = PlanFakeEngine()
    out = tmp_path / "uws-result.md"

    rc = run_task(_args(plan="click:站点管理", output=str(out)), engine=engine)

    assert rc == 0
    printed = capsys.readouterr().out
    assert "# Task Execution Result:" in printed
    assert "## 当前页面（任务结束点，可继续操作）" in printed
    assert "Example Domain" in printed
    assert printed.index("# Task Execution Result:") < printed.index("报告已另存")
    assert "# Task Execution Result:" in out.read_text(encoding="utf-8")


def test_run_task_billing_semantic_pauses_before_execute(tmp_path) -> None:
    """护栏：DSL 语义命中购买/支付类词 → 执行前 PAUSED + 「需要确认」报告，零动作外发。"""
    engine = PlanFakeEngine()
    out = tmp_path / "uws-billing.md"

    rc = run_task(_args(plan="click:立即购买套餐", output=str(out)), engine=engine)

    assert rc == 1
    assert engine.executed == []  # 未执行任何 ExecuteAction
    report = out.read_text(encoding="utf-8")
    assert "## 需要确认（任务已暂停）" in report
    assert "立即购买" in report
    assert "必须由用户确认" in report
    assert "VLM" not in report  # 护栏暂停不浪费 VLM 余额


def test_run_task_page_cta_pauses_before_click(tmp_path) -> None:
    """护栏：页面 AXTree 出现“立即购买”CTA → 后续点击暂停并提示确认。"""
    engine = BillingPageFakeEngine()
    out = tmp_path / "uws-page-risk.md"

    rc = run_task(_args(plan="click:下一步", output=str(out)), engine=engine)

    assert rc == 1
    assert engine.executed == []
    report = out.read_text(encoding="utf-8")
    assert "## 需要确认（任务已暂停）" in report
    assert "页面出现付费提示「立即购买」" in report


class MutexFakeEngine(PlanFakeEngine):
    """记录 initialize_context 收到的 kwargs，验证互斥键透传。"""

    def __init__(self) -> None:
        super().__init__()
        self.kwargs: dict = {}

    def initialize_context(self, task_id: str, initial_url: str | None = None, **kwargs: object) -> InitResult:
        self.kwargs = dict(kwargs)
        return super().initialize_context(task_id, initial_url, **kwargs)


def test_run_task_forwards_mutex_key() -> None:
    """互斥键（P1 同站点跨 session 复用）：`--mutex-key` 必须透传到 InitializeContext；
    未声明时不得凭空造一个（未声明 = 保持按会话隔离）。"""
    engine = MutexFakeEngine()
    rc = run_task(
        _args(plan="nav:https://example.com", mutex_key="login:example.com"),
        engine=engine,
    )

    assert rc == 0
    assert engine.kwargs.get("mutex_key") == "login:example.com"

    plain = MutexFakeEngine()
    run_task(_args(plan="nav:https://example.com"), engine=plain)
    assert plain.kwargs.get("mutex_key") is None
