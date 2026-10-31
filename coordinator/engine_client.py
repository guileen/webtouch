"""UWS 引擎客户端抽象（spec/coordinator.md §2 / proto/uws_service.proto 错误码表）。

协调器只通过公开 gRPC 语义与引擎交互；生产实现见 grpc_client.py，
测试注入 FakeEngine（tests/integration/test_coordinator.py）。
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol

# gRPC 状态码常量（与 proto 错误码映射表一致）
OK = 0
INVALID_ARGUMENT = 3
DEADLINE_EXCEEDED = 4
NOT_FOUND = 5
RESOURCE_EXHAUSTED = 8
FAILED_PRECONDITION = 9
ABORTED = 10
UNIMPLEMENTED = 12
INTERNAL = 13
UNAVAILABLE = 14
UNAUTHENTICATED = 16

# 物理层错误标记：标签页被关闭（用户手动关 Tab / Target.targetDestroyed）。
# API 层据此自动重建新标签页（v1.5.3 多标签会话）。
TARGET_DESTROYED = "target_destroyed"


class EngineError(Exception):
    """引擎侧错误（携带 gRPC 状态码，协调器据此决策）。"""

    def __init__(self, code: int, message: str = "", reason: str = "") -> None:
        super().__init__(message or f"engine error code={code}")
        self.code = code
        self.reason = reason or message


@dataclass
class HitElement:
    """动作实际命中元素的证据（引擎 `hit_element` 回执，v1.5.4 证据链）。

    extract 的返回值就来自这个节点：没有它，调用方无法判断「抽到的是目标本身，
    还是页面上同名的碎片节点」（2026-09-23 live dogfood 的假阳性形态）。
    """

    tag: str = ""
    text: str = ""
    selector: str = ""

    def describe(self) -> str:
        """单行证据：`h1` "Example Domains" ← css-path（无文本/选择器时省略对应段）。"""
        label = f'`{self.tag}` "{self.text}"' if self.text else f"`{self.tag}`"
        return f"{label} ← {self.selector}" if self.selector else label


@dataclass
class ActionResult:
    success: bool
    context_state: str = "STABLE"
    confidence: float = 0.0
    decision_trace_id: str = ""
    error_reason: str = ""
    extracted_data: str | None = None
    chat_stream_done: bool = False   # extract(prefer_sse) 时：SSE 流是否已结束
    chat_stream_active: bool = False  # extract(prefer_sse) 时：SSE 通道是否已接入
                                     # （引擎已捕获过聊天流，epoch>0；未接入=DOM 回退）
    current_url: str = ""
    step_id: str = ""
    intent_id: str = ""
    # ★v1.5.8（新窗口接管，2026-09-22 P0-1）：本次动作打开的新 target 与引擎当前
    # 实际作用的 target。两者相等 = 引擎已自动接管新页，协调器可无缝续作。
    new_target_id: str = ""
    active_target_id: str = ""
    # ★2026-09-23 证据回显：本次动作命中的元素（tag/text/selector）。
    hit_element: HitElement | None = None


@dataclass
class InitResult:
    ok: bool
    target_id: str = ""
    context_id: str = ""
    current_url: str = ""  # v1.5.6：初始化/续页时的真实当前页 URL
    error_reason: str = ""


@dataclass
class HealthStatus:
    ok: bool
    version: str = ""
    active_targets: int = 0
    max_targets: int = 0


class EngineClient(Protocol):
    """引擎客户端契约：只暴露 v1.4 gRPC 语义（attach 为协调器侧辅助）。"""

    def initialize_context(
        self, task_id: str, initial_url: str | None = None, **kwargs: object
    ) -> InitResult: ...

    def execute_action(
        self,
        intent_id: str,
        target_semantic: str,
        expected_action: str,
        value: str | None = None,
        context_hint: dict[str, str] | None = None,
        timeout_ms: int = 1500,
        node_id: int | None = None,
    ) -> ActionResult: ...

    def export_page_tree(self, target_id: str) -> str:
        """导出当前页面极简 AXTree（v1.5.5 ExportPageTree RPC），
        供 commander / uws run --plan 外发 VLM 指挥官做 node_id 确定性引用。
        """
        ...

    def capture_screenshot(self, target_id: str) -> bytes:
        """抓取当前 Target 屏幕截图（PNG 字节），供 VLM 求解（TDD-COORD-04）。

        生产 gRPC 客户端在 v1.5 追加 CaptureScreenshot RPC 前抛 UNIMPLEMENTED
        （见 USER_ACTION.md #9）；FakeEngine 测试夹具返回固定字节。
        """
        ...

    def health_check(self) -> HealthStatus: ...

    def attach_target(self, target_id: str) -> str:
        """NEW_WINDOW_OPENED 后切换新 Target；v1.4 无对应 RPC，生产客户端抛 UNIMPLEMENTED。"""
        ...

    def close_context(self, context_name: str = "") -> None:
        """关闭命名上下文（释放 Target，对应浏览器标签页随之关闭）。
        v1.5.3 追加 RPC；DELETE /v1/sessions/{id} 消费。幂等。"""
        ...
