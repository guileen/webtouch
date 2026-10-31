"""生产 gRPC 引擎客户端（对接 proto/gen/python 生成的桩）。

错误码 → EngineError 映射与 proto/uws_service.proto 末尾表一致；
NEW_WINDOW_OPENED 的 Attach（attach_target）在 v1.4 无对应 RPC，抛 UNIMPLEMENTED，
待真实 CDP 会话接入后按 M7 定案（USER_ACTION.md #4）。
传输层：默认明文（本地/内网）；配置 tls_ca_path（+ 客户端证书）后走 TLS/mTLS，
与服务端 --tls-cert/--tls-key/--tls-ca 对应（R6）。
"""

from __future__ import annotations

from pathlib import Path

import grpc

from coordinator.engine_client import (
    ABORTED,
    DEADLINE_EXCEEDED,
    FAILED_PRECONDITION,
    INVALID_ARGUMENT,
    INTERNAL,
    NOT_FOUND,
    RESOURCE_EXHAUSTED,
    UNAUTHENTICATED,
    UNAVAILABLE,
    UNIMPLEMENTED,
    ActionResult,
    EngineError,
    HealthStatus,
    HitElement,
    InitResult,
)
from uws_service_pb2 import (
    ActionIntentRequest,
    CaptureScreenshotRequest,
    CloseContextRequest,
    Empty,
    ExportPageTreeRequest,
    InitContextRequest,
)
from uws_service_pb2_grpc import UwsEngineStub

_CONTEXT_STATES = {
    0: "STABLE",
    1: "NAVIGATING",
    2: "NAVIGATED",
    3: "NEW_WINDOW_OPENED",
    4: "CHALLENGE_INTERCEPTED",
}
_GRPC_TO_CODE = {
    grpc.StatusCode.INVALID_ARGUMENT: INVALID_ARGUMENT,
    grpc.StatusCode.DEADLINE_EXCEEDED: DEADLINE_EXCEEDED,
    grpc.StatusCode.NOT_FOUND: NOT_FOUND,
    grpc.StatusCode.RESOURCE_EXHAUSTED: RESOURCE_EXHAUSTED,
    grpc.StatusCode.FAILED_PRECONDITION: FAILED_PRECONDITION,
    grpc.StatusCode.ABORTED: ABORTED,
    grpc.StatusCode.UNAUTHENTICATED: UNAUTHENTICATED,
    grpc.StatusCode.UNAVAILABLE: UNAVAILABLE,
}


class GrpcEngineClient:
    def __init__(
        self,
        port: int,
        api_key: str,
        host: str = "127.0.0.1",
        *,
        tls_ca_path: str | None = None,
        tls_client_cert_path: str | None = None,
        tls_client_key_path: str | None = None,
    ) -> None:
        target = f"{host}:{port}"
        if tls_ca_path:
            creds = grpc.ssl_channel_credentials(
                root_certificates=Path(tls_ca_path).read_bytes(),
                private_key=(
                    Path(tls_client_key_path).read_bytes()
                    if tls_client_key_path
                    else None
                ),
                certificate_chain=(
                    Path(tls_client_cert_path).read_bytes()
                    if tls_client_cert_path
                    else None
                ),
            )
            self._channel = grpc.secure_channel(target, creds)
        else:
            self._channel = grpc.insecure_channel(target)
        self._stub = UwsEngineStub(self._channel)
        self._metadata = (("x-api-key", api_key),)

    def initialize_context(
        self, task_id: str, initial_url: str | None = None, **kwargs: object
    ) -> InitResult:
        request = InitContextRequest(task_id=task_id)
        if initial_url:
            request.initial_url = initial_url
        attach_url = kwargs.get("attach_cdp_url")
        if attach_url and isinstance(attach_url, str):
            request.attach_cdp_url = attach_url
        context_name = kwargs.get("context_name")
        if context_name and isinstance(context_name, str):
            request.context_name = context_name
        mutex_key = kwargs.get("mutex_key")
        if mutex_key and isinstance(mutex_key, str):
            request.mutex_key = mutex_key
        if kwargs.get("require_existing_session") is True:
            request.require_existing_session = True
        try:
            response = self._stub.InitializeContext(request, metadata=self._metadata)
        except grpc.RpcError as exc:
            raise _map_error(exc) from exc
        return InitResult(
            ok=response.ok,
            target_id=response.target_id,
            context_id=response.context_id,
            current_url=response.current_url,
            error_reason=response.error_reason,
        )

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
        request = ActionIntentRequest(
            intent_id=intent_id,
            target_semantic=target_semantic,
            expected_action=expected_action,
            timeout_ms=timeout_ms,
        )
        if value is not None:
            request.value = value
        if node_id is not None:
            request.node_id = node_id
        if context_hint:
            request.context_hint.update(context_hint)
        try:
            response = self._stub.ExecuteAction(request, metadata=self._metadata)
        except grpc.RpcError as exc:
            raise _map_error(exc) from exc
        return ActionResult(
            success=response.success,
            context_state=_CONTEXT_STATES.get(response.context_state, "STABLE"),
            confidence=response.confidence,
            decision_trace_id=response.decision_trace_id,
            error_reason=response.error_reason,
            extracted_data=response.extracted_data,
            chat_stream_done=bool(response.chat_stream_done),
            chat_stream_active=response.HasField("chat_stream_done"),
            intent_id=response.intent_id,
            # v1.5.4 证据链：动作后 page_url（导航确定性证据）映射为协调器
            # 侧的 current_url（失败报告/step 摘要的“当前 URL”来源）。
            current_url=response.page_url,
            # v1.5.8 新窗口接管：引擎回报的 new/active target（相等=已接管）。
            new_target_id=response.new_target_id,
            active_target_id=response.active_target_id,
            # v1.5.4 证据链（2026-09-23 回显）：命中元素 tag/text/selector。
            hit_element=_map_hit_element(response),
        )


    def export_page_tree(self, target_id: str) -> str:
        """调用 v1.5.5 ExportPageTree RPC，返回极简 AXTree 文本（混合指挥）。"""
        request = ExportPageTreeRequest(target_id=target_id or "")
        try:
            response = self._stub.ExportPageTree(request, metadata=self._metadata)
        except grpc.RpcError as exc:
            raise _map_error(exc) from exc
        return response.tree

    def health_check(self) -> HealthStatus:
        try:
            response = self._stub.HealthCheck(Empty(), metadata=self._metadata)
        except grpc.RpcError as exc:
            raise _map_error(exc) from exc
        return HealthStatus(
            ok=response.ok,
            version=response.version,
            active_targets=response.active_targets,
            max_targets=response.max_targets,
        )

    def attach_target(self, target_id: str) -> str:
        raise EngineError(UNIMPLEMENTED, "v1.4 无 AttachTarget RPC；待真实 CDP 会话接入后定案")

    def close_context(self, context_name: str = "") -> None:
        """调用 v1.5.3 CloseContext RPC，关闭命名上下文（标签页随之关闭）。
        幂等：上下文不存在也返回 ok。"""
        request = CloseContextRequest(context_name=context_name or "")
        try:
            self._stub.CloseContext(request, metadata=self._metadata)
        except grpc.RpcError as exc:
            raise _map_error(exc) from exc

    def capture_screenshot(self, target_id: str) -> bytes:
        """调用 v1.5.1 CaptureScreenshot RPC，返回 PNG 截图字节（VLM 求解输入）。

        USER_ACTION.md #9 已落地：引擎追加 CaptureScreenshot RPC 与 ExecuteAction
        drag 原生拖拽原语；本客户端同步接线。
        """
        request = CaptureScreenshotRequest(target_id=target_id)
        try:
            response = self._stub.CaptureScreenshot(request, metadata=self._metadata)
        except grpc.RpcError as exc:
            raise _map_error(exc) from exc
        return response.png


def _map_hit_element(response: object) -> HitElement | None:
    """把引擎 `hit_element` 回执映射为协调器证据对象；未回显时为 None。"""
    if not response.HasField("hit_element"):
        return None
    he = response.hit_element
    return HitElement(tag=he.tag, text=he.text, selector=he.selector)


def _map_error(error: grpc.RpcError) -> EngineError:
    code = _GRPC_TO_CODE.get(error.code(), INTERNAL)
    return EngineError(code=code, message=error.details() or str(error), reason=error.details() or "")
