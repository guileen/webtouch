"""Playwright 风格适配层（TDD-ADAPTER-01/02）。

把 page.fill / page.click / page.wait_for_navigation 等常见调用映射为
UWS 公开 gRPC 语义（ExecuteAction），只经 EngineClient 协议触达引擎，
不触碰任何内部状态（边界隔离）。
"""

from __future__ import annotations

import uuid

from coordinator.engine_client import ActionResult, EngineClient


class PlaywrightAdapter:
    """以 Playwright 的页面 API 风格驱动 UWS 引擎。"""

    def __init__(self, client: EngineClient, task_id: str | None = None) -> None:
        self._client = client
        self._task_id = task_id or f"adapter-{uuid.uuid4().hex[:12]}"

    def _intent_id(self) -> str:
        return str(uuid.uuid4())

    def fill(self, target_semantic: str, value: str, timeout_ms: int = 1500) -> ActionResult:
        """映射 page.fill(locator, value) → ExecuteAction(input)。"""
        return self._client.execute_action(
            intent_id=self._intent_id(),
            target_semantic=target_semantic,
            expected_action="input",
            value=value,
            timeout_ms=timeout_ms,
        )

    def click(self, target_semantic: str, timeout_ms: int = 1500) -> ActionResult:
        """映射 page.click(locator) → ExecuteAction(click)。"""
        return self._client.execute_action(
            intent_id=self._intent_id(),
            target_semantic=target_semantic,
            expected_action="click",
            timeout_ms=timeout_ms,
        )

    def wait_for_navigation(self, timeout_ms: int = 1500) -> ActionResult:
        """映射 page.wait_for_navigation() → ExecuteAction(wait, NAVIGATED)。"""
        return self._client.execute_action(
            intent_id=self._intent_id(),
            target_semantic="NAVIGATED",
            expected_action="wait",
            timeout_ms=timeout_ms,
        )
