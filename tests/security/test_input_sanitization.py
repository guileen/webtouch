"""输入净化安全测试（TDD-SEC-02/06、TDD-MATCH-03，tdd-checklist.md §5）。

实现层：pkg/engine/sanitize.go validateIntent（Go，边界值见 TDD-SEC-06 勾选）；
本文件经真实引擎 gRPC（tests/conftest.py session 级 engine 夹具）点亮安全层。
validateIntent 在生命周期检查之前执行——非法输入无需 InitializeContext
即返回 INVALID_ARGUMENT；合法输入在未初始化时返回 FAILED_PRECONDITION
（证明通过了输入边界）。
"""

import grpc
import pytest

from uws_service_pb2 import ActionIntentRequest, Empty
from uws_service_pb2_grpc import UwsEngineStub

COORD_KEY = "coordinator-test-key"
MONITOR_KEY = "monitor-test-key"


def _channel(port: int) -> grpc.Channel:
    return grpc.insecure_channel(f"127.0.0.1:{port}")


def test_tdd_sec_02_invalid_semantic_rejected(engine):
    """超长（>120）或含 XSS 标签的 target_semantic → INVALID_ARGUMENT。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        for bad in ("<script>alert(1)</script>", "a" * 121):
            with pytest.raises(grpc.RpcError) as ei:
                stub.ExecuteAction(
                    ActionIntentRequest(
                        intent_id="sec02", target_semantic=bad, expected_action="click"
                    ),
                    metadata=(("x-api-key", COORD_KEY),),
                )
            assert ei.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_tdd_sec_06_parameter_boundaries(engine):
    """target_semantic ≤120 白名单；input ≤256 KiB，其他 value ≤4096，禁 javascript: 前缀；
    context_hint 键 ≤64 / 值 ≤256 → INVALID_ARGUMENT。
    合法请求通过边界（未初始化 → FAILED_PRECONDITION，非 INVALID_ARGUMENT）。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        bad_reqs = [
            ActionIntentRequest(intent_id="s1", target_semantic="a" * 121, expected_action="click"),
            ActionIntentRequest(intent_id="s2", target_semantic="login<x>", expected_action="click"),
            ActionIntentRequest(
                intent_id="s3", target_semantic="登录", expected_action="input", value="v" * (256 * 1024 + 1)
            ),
            ActionIntentRequest(
                intent_id="s3-click", target_semantic="登录", expected_action="click", value="v" * 4097
            ),
            ActionIntentRequest(
                intent_id="s4",
                target_semantic="登录",
                expected_action="input",
                value="javascript:alert(1)",
            ),
            ActionIntentRequest(
                intent_id="s5",
                target_semantic="登录",
                expected_action="click",
                context_hint={"k" * 65: "v"},
            ),
            ActionIntentRequest(
                intent_id="s6",
                target_semantic="登录",
                expected_action="click",
                context_hint={"k": "v" * 257},
            ),
        ]
        for req in bad_reqs:
            with pytest.raises(grpc.RpcError) as ei:
                stub.ExecuteAction(req, metadata=(("x-api-key", COORD_KEY),))
            assert ei.value.code() == grpc.StatusCode.INVALID_ARGUMENT

        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(
                ActionIntentRequest(intent_id="s7", target_semantic="登录", expected_action="click"),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.FAILED_PRECONDITION

        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(
                ActionIntentRequest(intent_id="long-input", target_semantic="composer",
                                    expected_action="input", value="读" * 20000),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.FAILED_PRECONDITION


def test_r7_semantic_length_counts_runes(engine):
    """R7 回归：target_semantic 上限按字符（rune）而非字节计数——
    60 个汉字（180 字节）应通过输入边界（未初始化 → FAILED_PRECONDITION，
    而非被字节长度误拒为 INVALID_ARGUMENT）。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(
                ActionIntentRequest(
                    intent_id="r7", target_semantic="汉" * 60, expected_action="click"
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.FAILED_PRECONDITION


def test_tdd_match_03_sql_injection_safe(engine):
    """SQL 注入 payload 在 API 输入边界被拦截（INVALID_ARGUMENT，
    满足「防注入层拦截」语义）；引擎健康不受影响、规则库不被破坏。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        payload = "'; DROP TABLE uws_rules;--"
        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(
                ActionIntentRequest(
                    intent_id="m3", target_semantic=payload, expected_action="click"
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.INVALID_ARGUMENT

        health = stub.HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),), timeout=5)
        assert health.ok
