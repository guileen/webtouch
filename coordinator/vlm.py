"""VLM 视觉打码求解器（spec/coordinator.md §6.1 / docs/tdd-checklist.md TDD-COORD-04）。

职责边界：协调器在收到 ``CHALLENGE_INTERCEPTED``（引擎映射 UNAUTHENTICATED）后，
挂起当前执行流 → 抓取截图 → 交由 VLM/打码服务计算轨迹坐标 → 下发物理拖拽解锁 →
原任务流无缝继续。

本模块定义求解抽象、确定性 Mock 与生产 HTTP 求解器：

- ``VlmSolution``：求解产物（拖拽/点击坐标 + 动作类型），``coords()`` 输出
  引擎 ExecuteAction drag 消费的 ``x1,y1,x2,y2`` 文本；
- ``VlmSolver``：协议接口，生产实现（DeepSeek-VL / 打码 API）后续接入；
- ``MockVlmSolver``：固定坐标的确定性求解器，供测试与本地演示使用。
- ``HttpVlmSolver``：纯 stdlib HTTP 求解器，POST base64 截图 + 挑战语义到
  打码/VLM 服务，解析 ``{x1,y1,x2,y2,action}`` 响应；
- ``build_vlm_solver``：从 ``~/.uws/config.yaml``（vlm 段）构造求解器工厂。

引擎侧截图 RPC 与原生拖拽原语已随 v1.5.1 追加（CaptureScreenshot RPC +
ExecuteAction drag 坐标直发，见 USER_ACTION.md #9）；``GrpcEngineClient.capture_screenshot``
已实现真实 PNG 抓取，生产 VLM/打码服务可替换 ``MockVlmSolver`` 接入。
"""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass
from typing import Mapping, Protocol

from coordinator.config import config_int


class VlmError(Exception):
    """VLM 求解服务调用失败（网络/协议/响应非法），调用方按 PAUSED 降级。"""


@dataclass(frozen=True)
class VlmSolution:
    """VLM 求解产物：一次轨迹（默认水平滑块拖拽）。"""

    x1: int
    y1: int
    x2: int
    y2: int
    action: str = "drag"  # drag | click（后续可扩展）

    def coords(self) -> str:
        """引擎 ExecuteAction drag 消费的坐标文本（x1,y1,x2,y2）。"""
        return f"{self.x1},{self.y1},{self.x2},{self.y2}"


class VlmSolver(Protocol):
    """VLM 求解抽象：输入截图与挑战语义，输出轨迹坐标。"""

    def solve(self, screenshot: bytes, challenge_hint: str) -> VlmSolution: ...


class MockVlmSolver:
    """确定性 Mock：固定坐标（水平滑块，如 x:100→300, y:200），供测试/本地演示。

    生产实现应替换为真实 VLM/打码服务（DeepSeek-VL 等），见 USER_ACTION.md #9。
    """

    def __init__(self, solution: VlmSolution | None = None) -> None:
        self._solution = solution or VlmSolution(x1=100, y1=200, x2=300, y2=200)
        self.calls: list[tuple[bytes, str]] = []

    def solve(self, screenshot: bytes, challenge_hint: str) -> VlmSolution:
        self.calls.append((screenshot, challenge_hint))
        return self._solution


class HttpVlmSolver:
    """生产 HTTP 打码/VLM 服务求解器（纯 stdlib，零外部依赖）。

    请求协议：POST JSON 到 ``endpoint``——
    ``{"image": "<base64 PNG>", "challenge_hint": "<目标语义>"}``；
    配置了 api_key 时携带 ``Authorization: Bearer <key>``。
    响应 JSON 须包含 ``x1/y1/x2/y2``（可选 ``action``，默认 ``drag``）；
    任何非 2xx、网络错误或非法响应一律抛 :class:`VlmError`（不静默失败）。
    """

    def __init__(
        self,
        endpoint: str,
        api_key: str = "",
        timeout_sec: float = 10.0,
        urlopen=None,
    ) -> None:
        if not endpoint:
            raise VlmError("vlm endpoint 未配置")
        self._endpoint = endpoint
        self._api_key = api_key
        self._timeout_sec = timeout_sec
        self._urlopen = urlopen  # 测试注入点；None 时用 urllib.request.urlopen

    def solve(self, screenshot: bytes, challenge_hint: str) -> VlmSolution:
        import urllib.error
        import urllib.request

        body = json.dumps(
            {
                "image": base64.b64encode(screenshot).decode("ascii"),
                "challenge_hint": challenge_hint,
            }
        ).encode("utf-8")
        req = urllib.request.Request(
            self._endpoint,
            data=body,
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        if self._api_key:
            req.add_header("Authorization", f"Bearer {self._api_key}")
        opener = self._urlopen or urllib.request.urlopen
        try:
            with opener(req, timeout=self._timeout_sec) as resp:
                payload = json.loads(resp.read().decode("utf-8"))
        except (OSError, urllib.error.HTTPError, ValueError) as exc:
            raise VlmError(f"http_solve_failed: {exc}") from exc
        try:
            return VlmSolution(
                x1=int(payload["x1"]),
                y1=int(payload["y1"]),
                x2=int(payload["x2"]),
                y2=int(payload["y2"]),
                action=str(payload.get("action", "drag")),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise VlmError(f"invalid_solver_response: {exc}") from exc


def build_vlm_solver(config: Mapping[str, str]) -> VlmSolver | None:
    """从配置构造 VLM 求解器（vlm 段）。

    - ``vlm.provider=mock`` → :class:`MockVlmSolver`（测试/本地演示）；
    - ``vlm.provider=http``（或未显式 provider 但配置了 endpoint）→
      :class:`HttpVlmSolver`；
    - 未配置 → ``None``（协调器保持既有 PAUSED 降级语义，行为兼容）。
    """
    provider = (config.get("vlm.provider") or "").strip().lower()
    endpoint = (config.get("vlm.endpoint") or "").strip()
    if provider == "mock":
        return MockVlmSolver()
    if provider == "http" or (provider == "" and endpoint):
        if not endpoint:
            raise VlmError("vlm.provider=http 但未配置 vlm.endpoint")
        return HttpVlmSolver(
            endpoint=endpoint,
            api_key=config.get("vlm.api_key", ""),
            timeout_sec=float(config_int(config, "vlm.timeout_sec", 10)),
        )
    return None
