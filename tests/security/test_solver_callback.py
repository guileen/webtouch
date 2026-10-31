"""外部破解回调安全与超时测试（TDD-SEC-04 / TDD-SEC-05，spec/physical-patches.md §3）。

测试覆盖：
- TDD-SEC-04: X-Solver-Signature HMAC-SHA256 验签（正常签名放行、缺失签名拒绝、错误签名拒绝、伪造载荷拒绝、防状态机污染）
- TDD-SEC-05: 破解超时降级（ACK 超时 2s、等待超时 60s、重试 1 次后升级 UNAUTHENTICATED 人工接管）
"""

import json
import time

import pytest
from coordinator.solver_callback import (
    AsyncSolverManager,
    InvalidSignatureError,
    SolverSecurityError,
    SolverTimeoutError,
    compute_signature,
    parse_verified_callback,
    verify_callback_signature,
)



def test_tdd_sec_04_callback_signature_verification():
    """TDD-SEC-04: 无 X-Solver-Signature 或 HMAC-SHA256 校验失败必须拒绝，状态机不被污染。"""
    secret = "test-pre-shared-secret-key-123456"
    payload = {"task_id": "solve_001", "solution": {"x1": 120, "y1": 250, "x2": 320, "y2": 250, "action": "drag"}}
    body_bytes = json.dumps(payload).encode("utf-8")
    valid_sig = compute_signature(secret, body_bytes)

    # 1. 正常有效签名通过
    assert verify_callback_signature(secret, body_bytes, valid_sig) is True
    assert verify_callback_signature(secret, body_bytes, f"sha256={valid_sig}") is True
    solution = parse_verified_callback(secret, body_bytes, valid_sig)
    assert solution.x1 == 120
    assert solution.y1 == 250
    assert solution.x2 == 320
    assert solution.coords() == "120,250,320,250"

    # 2. 缺失请求头必须拒绝
    with pytest.raises(InvalidSignatureError, match="缺失 X-Solver-Signature"):
        verify_callback_signature(secret, body_bytes, None)

    with pytest.raises(InvalidSignatureError, match="缺失 X-Solver-Signature"):
        verify_callback_signature(secret, body_bytes, "")

    # 3. 错误签名或篡改 Payload 必须拒绝
    fake_sig = "a" * 64
    with pytest.raises(InvalidSignatureError, match="校验失败"):
        verify_callback_signature(secret, body_bytes, fake_sig)

    tampered_body = json.dumps({"task_id": "solve_001", "solution": {"x1": 999, "y1": 999, "x2": 999, "y2": 999}}).encode("utf-8")
    with pytest.raises(InvalidSignatureError, match="校验失败"):
        verify_callback_signature(secret, tampered_body, valid_sig)

    # 4. 状态机防污染验证
    mgr = AsyncSolverManager(secret=secret)
    task = mgr.create_task("solve_001", "请完成滑块验证")
    assert task.status == "PENDING"
    assert task.solution is None

    # 伪造请求打入，抛异常且 task 状态保持 PENDING，绝不污染 solution
    with pytest.raises(InvalidSignatureError):
        mgr.handle_callback("solve_001", tampered_body, valid_sig)
    assert task.status == "PENDING"
    assert task.solution is None

    # 合法请求打入，状态更新为 COMPLETED
    mgr.handle_callback("solve_001", body_bytes, valid_sig)
    assert task.status == "COMPLETED"
    assert task.solution is not None
    assert task.solution.x1 == 120


def test_tdd_sec_05_solver_timeout_escalation():
    """TDD-SEC-05: 同步 ACK 超时(2s) / 等待(60s) / 重试 1 次后返回 UNAUTHENTICATED 并升级人工接管。"""
    secret = "secret-tdd-sec-05"
    simulated_time = 1000.0

    def mock_time():
        return simulated_time

    mgr = AsyncSolverManager(
        secret=secret,
        ack_timeout_sec=2.0,
        wait_timeout_sec=60.0,
        max_retries=1,
        time_fn=mock_time,
    )

    # 1. 测试 ACK 超时（>2s 抛 SolverTimeoutError）
    assert mgr.check_ack_timeout(1.5) is True
    with pytest.raises(SolverTimeoutError, match="同步 ACK 超时"):
        mgr.check_ack_timeout(2.1)

    # 2. 测试创建任务与 60s 等待超时
    task = mgr.create_task("task_002", "滑动拼图")
    assert task.attempts == 1

    # 推进 65 秒后回调，发生等待超时
    simulated_time += 65.0
    body = json.dumps({"x1": 10, "y1": 20, "x2": 30, "y2": 40}).encode("utf-8")
    sig = compute_signature(secret, body)
    with pytest.raises(SolverTimeoutError, match="回调已超时"):
        mgr.handle_callback("task_002", body, sig)
    assert task.status == "TIMEOUT"

    # 3. 超时重试 1 次机制（第 1 次允许重试，第 2 次超限并升级 UNAUTHENTICATED）
    can_retry_1 = mgr.handle_timeout_or_retry("task_002")
    assert can_retry_1 is True
    assert task.attempts == 2

    with pytest.raises(SolverTimeoutError, match="重试达上限.*升级人工接管"):
        mgr.handle_timeout_or_retry("task_002")
    assert task.status == "FAILED"
