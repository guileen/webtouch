"""外部打码/破解服务异步回调与签名校验安全模块（TDD-SEC-04 / TDD-SEC-05）。

对应规范：
- spec/physical-patches.md §3（外部破解服务交互协议）
- docs/tdd-checklist.md TDD-SEC-04（签名防伪造）与 TDD-SEC-05（超时降级与人工升级）

协议规范：
1. 签名防伪造（TDD-SEC-04）：
   - 回调请求头必须包含 `X-Solver-Signature`；
   - 签名算法：HMAC-SHA256(secret, body_raw_bytes) 的十六进制表示（hex digest）；
   - 使用 constant-time 比对（hmac.compare_digest）防止时序攻击；
   - 缺失请求头、密钥为空、或哈希不匹配一律拒绝（抛 InvalidSignatureError），
     杜绝伪造回调污染任务状态机。

2. 异步回调与超时重试降级（TDD-SEC-05 / AsyncSolverClient）：
   - 同步 ACK 超时（默认 2s）；
   - 破解等待超时（默认 60s）；
   - 回调超时后最多重试 1 次（retry_count <= 1）；
   - 超过重试上限后，明确抛 SolverTimeoutError，由上层降级为 UNAUTHENTICATED 并升级人工接管，禁止无限重试。
"""

from __future__ import annotations

import hashlib
import hmac
import json
import time
from dataclasses import dataclass
from typing import Callable

from coordinator.vlm import VlmError, VlmSolution


class SolverSecurityError(VlmError):
    """打码安全错误基类。"""


class InvalidSignatureError(SolverSecurityError):
    """X-Solver-Signature 签名校验失败或缺失，回调被拒绝。"""


class SolverTimeoutError(SolverSecurityError):
    """破解超时（同步 ACK 超时或等待回调超时），需降级为 UNAUTHENTICATED。"""


def compute_signature(secret: str | bytes, body_bytes: bytes) -> str:
    """计算 HMAC-SHA256 签名（十六进制小写）。"""
    if isinstance(secret, str):
        secret_bytes = secret.encode("utf-8")
    else:
        secret_bytes = secret
    return hmac.new(secret_bytes, body_bytes, hashlib.sha256).hexdigest()


def verify_callback_signature(
    secret: str | bytes,
    body_bytes: bytes,
    signature_header: str | None,
) -> bool:
    """校验回调请求的 X-Solver-Signature 签名。

    :param secret: 预共享密钥（必须非空）
    :param body_bytes: 原始请求 Body 字节流
    :param signature_header: X-Solver-Signature 标头值
    :return: 校验成功返回 True
    :raises InvalidSignatureError: 密钥为空、签名头缺失或签名不匹配
    """
    if not secret:
        raise InvalidSignatureError("secret 不能为空")
    if not signature_header:
        raise InvalidSignatureError("缺失 X-Solver-Signature 请求头")

    expected = compute_signature(secret, body_bytes)
    # 处理可能的 sha256= 前缀（宽容解析）
    actual = signature_header.strip()
    if actual.lower().startswith("sha256="):
        actual = actual[7:].strip()

    if not hmac.compare_digest(expected.lower(), actual.lower()):
        raise InvalidSignatureError("X-Solver-Signature 校验失败，拒绝处理回调")
    return True


def parse_verified_callback(
    secret: str | bytes,
    body_bytes: bytes,
    signature_header: str | None,
) -> VlmSolution:
    """安全解析并校验回调 Payload。若验签通过则返回 VlmSolution，否则抛 InvalidSignatureError。"""
    verify_callback_signature(secret, body_bytes, signature_header)
    try:
        data = json.loads(body_bytes.decode("utf-8"))
    except Exception as exc:
        raise SolverSecurityError(f"invalid_json_payload: {exc}") from exc

    # 支持顶层或 solution 嵌套
    payload = data.get("solution", data)
    try:
        return VlmSolution(
            x1=int(payload["x1"]),
            y1=int(payload["y1"]),
            x2=int(payload["x2"]),
            y2=int(payload["y2"]),
            action=str(payload.get("action", "drag")),
        )
    except (KeyError, TypeError, ValueError) as exc:
        raise SolverSecurityError(f"invalid_solution_fields: {exc}") from exc


@dataclass
class SolverTask:
    task_id: str
    challenge_hint: str
    status: str = "PENDING"  # PENDING | COMPLETED | TIMEOUT | FAILED
    solution: VlmSolution | None = None
    created_at: float = 0.0
    attempts: int = 0


class AsyncSolverManager:
    """异步破解生命周期与超时重试管理器（TDD-SEC-05）。

    管控：
    - 同步 ACK 超时：2.0s
    - 破解等待总超时：60.0s
    - 最大重试次数：1 次（总尝试 2 次，attempts=2 后失败）
    - 超时抛 SolverTimeoutError，供上层捕获后返回 UNAUTHENTICATED 并升级人工接管。
    """

    def __init__(
        self,
        secret: str,
        ack_timeout_sec: float = 2.0,
        wait_timeout_sec: float = 60.0,
        max_retries: int = 1,
        time_fn: Callable[[], float] = time.time,
    ) -> None:
        self.secret = secret
        self.ack_timeout_sec = ack_timeout_sec
        self.wait_timeout_sec = wait_timeout_sec
        self.max_retries = max_retries
        self._time_fn = time_fn
        self._tasks: dict[str, SolverTask] = {}

    def create_task(self, task_id: str, challenge_hint: str) -> SolverTask:
        task = SolverTask(
            task_id=task_id,
            challenge_hint=challenge_hint,
            created_at=self._time_fn(),
            attempts=1,
        )
        self._tasks[task_id] = task
        return task

    def check_ack_timeout(self, elapsed_sec: float) -> bool:
        """检查同步 ACK 是否超时（> 2s 抛 SolverTimeoutError）。"""
        if elapsed_sec > self.ack_timeout_sec:
            raise SolverTimeoutError(
                f"打码服务同步 ACK 超时 ({elapsed_sec:.2f}s > {self.ack_timeout_sec}s)"
            )
        return True

    def handle_callback(
        self,
        task_id: str,
        body_bytes: bytes,
        signature_header: str | None,
    ) -> VlmSolution:
        """接收外部破解服务回调。先验签，再更新任务状态机。"""
        if task_id not in self._tasks:
            raise SolverSecurityError(f"未知 task_id: {task_id}")

        task = self._tasks[task_id]
        now = self._time_fn()
        if (now - task.created_at) > self.wait_timeout_sec:
            task.status = "TIMEOUT"
            raise SolverTimeoutError(
                f"回调已超时 (等待时长 {now - task.created_at:.1f}s > {self.wait_timeout_sec}s)"
            )

        # 验签（若失败则抛 InvalidSignatureError，状态机保持不变，防止被污染）
        solution = parse_verified_callback(self.secret, body_bytes, signature_header)
        task.solution = solution
        task.status = "COMPLETED"
        return solution

    def handle_timeout_or_retry(self, task_id: str) -> bool:
        """处理任务超时。若可重试（<= max_retries）则重置计时并返回 True，否则置 FAILED 并抛 SolverTimeoutError。"""
        if task_id not in self._tasks:
            raise SolverSecurityError(f"未知 task_id: {task_id}")

        task = self._tasks[task_id]
        if task.attempts <= self.max_retries:
            task.attempts += 1
            task.created_at = self._time_fn()
            task.status = "RETRYING"
            return True

        task.status = "FAILED"
        raise SolverTimeoutError(
            f"打码任务重试达上限 ({task.attempts} 次)，最终返回 UNAUTHENTICATED 并升级人工接管"
        )
