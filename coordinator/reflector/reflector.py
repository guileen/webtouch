"""失败分类与反思（spec/coordinator.md §6 + guide §4）。

错误码 → 对策矩阵按 proto/uws_service.proto 末尾表实现；
DEADLINE_EXCEEDED 采用 Fibonacci 1s/2s 最多 2 次（GLOBAL.md §5 裁决 #2
统一协调器侧语义，与引擎侧契约的「重试 1 次」区分开）。
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum

from coordinator.engine_client import (
    ABORTED,
    DEADLINE_EXCEEDED,
    FAILED_PRECONDITION,
    INVALID_ARGUMENT,
    NOT_FOUND,
    RESOURCE_EXHAUSTED,
    UNAUTHENTICATED,
    ActionResult,
    EngineError,
)
from coordinator.models import ActionNode

FIBONACCI_WAITS = (1.0, 2.0)  # 最多 2 次重试
ABORTED_WAIT_SECONDS = 0.2
ABORTED_MAX_RETRIES = 3
REINIT_MAX_RETRIES = 1
# 凭据直填（`@vault:<名>`）解引用失败的引擎原话前缀（契约见 proto/uws_service.proto
# 错误码表 FAILED_PRECONDITION 行）：该子类归 PAUSE，不归 REINIT。
VAULT_UNRESOLVED_PREFIX = "vault_unresolved:"


class FailureAction(str, Enum):
    RETRY = "retry"
    REPLAN = "replan"
    SKIP = "skip"
    ABORT = "abort"
    REINIT = "reinit"
    PAUSE = "pause"
    VLM = "vlm"


@dataclass(frozen=True)
class FailureDecision:
    action: FailureAction
    wait_seconds: float = 0.0
    reason: str = ""


class Reflector:
    def __init__(self, max_deadline_retries: int = 2) -> None:
        self.max_deadline_retries = max_deadline_retries

    def classify(self, error: EngineError, step: ActionNode) -> FailureDecision:
        code = error.code
        if code == DEADLINE_EXCEEDED:
            step.retry_count += 1
            # R7 拍板（2026-08-20 续24，方案 A）：step.max_retries 是
            # on_failure=retry 的「局部语义重试预算」，只在 DEADLINE_EXCEEDED
            # 路径消费；受 Fibonacci 1s/2s 硬上限钳制（GLOBAL §5 #2 裁决，
            # FIBONACCI_WAITS 仅 2 档），默认 2 行为零变化。模板可调低
            # （max_retries=1/0）。ABORTED（物理遮挡自愈）与 REINIT（崩溃
            # 恢复）是不同失败语义，维持各自固定上限，不混用。
            cap = min(
                step.max_retries if step.max_retries is not None else self.max_deadline_retries,
                len(FIBONACCI_WAITS),
            )
            if step.retry_count <= cap:
                return FailureDecision(
                    FailureAction.RETRY,
                    wait_seconds=FIBONACCI_WAITS[step.retry_count - 1],
                    reason="deadline_exceeded",
                )
            return FailureDecision(FailureAction.ABORT, reason="deadline_exceeded_exhausted")
        if code == ABORTED:
            # 拓扑推演四（§5.0.8 契约保真）：若引擎内部已明确重试耗尽（如 after 3 attempts），
            # 直接 ABORT 并透传结构化错误原话，不再由协调器盲目二次放大重试。
            is_exhausted = "after" in (error.reason or "") and "attempt" in (error.reason or "")
            if is_exhausted:
                return FailureDecision(FailureAction.ABORT, reason=error.reason)
            step.retry_count += 1
            if step.retry_count <= ABORTED_MAX_RETRIES:
                return FailureDecision(
                    FailureAction.RETRY, wait_seconds=ABORTED_WAIT_SECONDS, reason=error.reason or "aborted"
                )
            reason = f"aborted_exhausted: {error.reason}" if error.reason else "aborted_exhausted"
            return FailureDecision(FailureAction.ABORT, reason=reason)
        if code == NOT_FOUND:
            return FailureDecision(FailureAction.REPLAN, reason="not_found")
        if code == FAILED_PRECONDITION:
            # 凭据直填失败是「前置条件不可满足」的子类：缺失的凭据不会因为重试或
            # 重建上下文而出现（2026-09-23 真机实测：走 REINIT 会白做一次上下文重建，
            # 且报告把原因显示成 reinit_exhausted，用户看不出真因）⇒ 暂停并透传引擎
            # 原话，与 RESOURCE_EXHAUSTED 同一口径。
            if error.reason.startswith(VAULT_UNRESOLVED_PREFIX):
                return FailureDecision(FailureAction.PAUSE, reason=error.reason)
            step.retry_count += 1
            if step.retry_count <= REINIT_MAX_RETRIES:
                return FailureDecision(FailureAction.REINIT, reason="target_crashed")
            return FailureDecision(FailureAction.ABORT, reason="reinit_exhausted")
        if code == RESOURCE_EXHAUSTED:
            # 保留引擎原话：RESOURCE_EXHAUSTED 既可能是 LLM 配额/Target 池满，
            # 也可能是防乱点熔断（2026-09-23 起熔断不再冒充 UNAUTHENTICATED）。
            # 泛化成 "resource_exhausted" 会让用户看不出真正原因，故透传。
            return FailureDecision(FailureAction.PAUSE, reason=error.reason or "resource_exhausted")
        if code == UNAUTHENTICATED:
            return FailureDecision(FailureAction.VLM, reason="challenge_intercepted")
        if code == INVALID_ARGUMENT:
            return FailureDecision(FailureAction.ABORT, reason="invalid_argument")
        if step.on_failure == "skip":
            return FailureDecision(FailureAction.SKIP, reason="policy_skip")
        if step.on_failure == "replan":
            return FailureDecision(FailureAction.REPLAN, reason="policy_replan")
        # 拓扑推演四：未识别未知错误兜底优先保留引擎原话，不再无脑覆盖为 unknown_error
        return FailureDecision(FailureAction.ABORT, reason=error.reason or "unknown_error")

    def should_reflect(self, consecutive_failures: int) -> bool:
        return consecutive_failures >= 2

    def rollback_point(
        self, history: list[ActionResult], steps: list[ActionNode]
    ) -> str | None:
        """最近 5 步内最后一个 STABLE 步骤；无则返回 None（全量重跑）。"""
        for entry in reversed(history[-5:]):
            if entry.context_state == "STABLE" and entry.step_id:
                return entry.step_id
        return None
