"""Playwright 适配层单元测试（TDD-ADAPTER-01/02，tdd-checklist.md §7.1）。

FakeEngine 充当 mock gRPC stub，验证 page.fill/click/wait_for_navigation 到
ExecuteAction 的映射；边界隔离用源码静态审计（只允许 EngineClient 协议方法）。
"""

from __future__ import annotations

import inspect
import re
from dataclasses import dataclass, field

from coordinator.engine_client import ActionResult, EngineClient
from coordinator.playwright_adapter import PlaywrightAdapter


@dataclass
class FakeEngine:
    """记录调用的 mock 引擎（实现 EngineClient 协议）。"""

    calls: list[dict[str, object]] = field(default_factory=list)
    result: ActionResult = field(default_factory=lambda: ActionResult(success=True))

    def initialize_context(self, task_id: str, initial_url: str | None = None, **kwargs: object) -> object:
        self.calls.append(
            {"method": "initialize_context", "task_id": task_id, "initial_url": initial_url, "kwargs": kwargs}
        )
        return object()

    def execute_action(
        self,
        intent_id: str,
        target_semantic: str,
        expected_action: str,
        value: str | None = None,
        context_hint: dict[str, str] | None = None,
        timeout_ms: int = 1500,
    ) -> ActionResult:
        self.calls.append(
            {
                "method": "execute_action",
                "intent_id": intent_id,
                "target_semantic": target_semantic,
                "expected_action": expected_action,
                "value": value,
                "context_hint": context_hint,
                "timeout_ms": timeout_ms,
            }
        )
        return self.result

    def health_check(self) -> object:
        self.calls.append({"method": "health_check"})
        return object()

    def attach_target(self, target_id: str) -> str:
        self.calls.append({"method": "attach_target", "target_id": target_id})
        return target_id


def _adapter() -> tuple[PlaywrightAdapter, FakeEngine]:
    engine = FakeEngine()
    return PlaywrightAdapter(engine), engine


def test_tdd_adapter_01_api_mapping() -> None:
    page, engine = _adapter()

    page.fill("username_input", "alice")
    page.click("login_submit_button")
    page.wait_for_navigation(timeout_ms=2000)

    actions = [c for c in engine.calls if c["method"] == "execute_action"]
    assert len(actions) == 3

    fill = actions[0]
    assert fill["target_semantic"] == "username_input"
    assert fill["expected_action"] == "input"
    assert fill["value"] == "alice"

    click = actions[1]
    assert click["target_semantic"] == "login_submit_button"
    assert click["expected_action"] == "click"
    assert click["value"] is None

    nav = actions[2]
    assert nav["target_semantic"] == "NAVIGATED"
    assert nav["expected_action"] == "wait"
    assert nav["timeout_ms"] == 2000

    # 每个动作都有独立 intent_id，可被幂等缓存消费。
    ids = {a["intent_id"] for a in actions}
    assert len(ids) == 3


def test_tdd_adapter_02_boundary_isolation() -> None:
    """静态审计：适配层只经 EngineClient 协议方法触达引擎，不触碰内部状态。"""
    module = inspect.getmodule(PlaywrightAdapter)
    assert module is not None
    source = inspect.getsource(module)

    for forbidden in ("uws_service_pb2", "UwsEngineStub", "grpc.", "pkg/", "pkg."):
        assert forbidden not in source, f"适配层不应引用内部实现: {forbidden}"

    protocol_methods = {"initialize_context", "execute_action", "health_check", "attach_target"}
    calls = re.findall(r"self\._client\.(\w+)\(", source)
    assert calls, "适配层必须通过 EngineClient 触达引擎"
    assert set(calls) <= protocol_methods
