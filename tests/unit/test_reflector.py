"""Reflector 重试上限语义测试（R7 拍板，2026-08-20 续24）。

- DEADLINE_EXCEEDED：局部重试预算 = step.max_retries（默认 2），受 Fibonacci
  1s/2s 硬上限（2 次）钳制——模板可调低（1/0），不可调高；
- ABORTED（物理遮挡自愈）/ REINIT（崩溃恢复）维持各自固定上限，语义不混用。
"""

from __future__ import annotations

from coordinator.engine_client import (
    ABORTED,
    DEADLINE_EXCEEDED,
    FAILED_PRECONDITION,
    EngineError,
)
from coordinator.models import ActionNode
from coordinator.reflector import FailureAction, Reflector


def _err(code: int) -> EngineError:
    return EngineError(code, "test")


def test_deadline_default_two_retries():
    """默认 max_retries=2：2 次 Fibonacci 重试后 ABORT（行为与裁决前一致）。"""
    r = Reflector()
    step = ActionNode("s1", "click", "login")
    assert r.classify(_err(DEADLINE_EXCEEDED), step).action is FailureAction.RETRY
    assert r.classify(_err(DEADLINE_EXCEEDED), step).action is FailureAction.RETRY
    assert r.classify(_err(DEADLINE_EXCEEDED), step).action is FailureAction.ABORT


def test_deadline_max_retries_one_and_zero():
    """模板调低预算：max_retries=1 → 只重试 1 次；max_retries=0 → 立即 ABORT。"""
    r = Reflector()
    step = ActionNode("s1", "click", "login", max_retries=1)
    assert r.classify(_err(DEADLINE_EXCEEDED), step).action is FailureAction.RETRY
    assert r.classify(_err(DEADLINE_EXCEEDED), step).action is FailureAction.ABORT

    step0 = ActionNode("s1", "click", "login", max_retries=0)
    assert r.classify(_err(DEADLINE_EXCEEDED), step0).action is FailureAction.ABORT


def test_deadline_max_retries_capped_by_fibonacci():
    """max_retries=5 也不能突破 Fibonacci 2 次硬上限（GLOBAL §5 #2 裁决）。"""
    r = Reflector()
    step = ActionNode("s1", "click", "login", max_retries=5)
    assert r.classify(_err(DEADLINE_EXCEEDED), step).action is FailureAction.RETRY
    assert r.classify(_err(DEADLINE_EXCEEDED), step).action is FailureAction.RETRY
    assert r.classify(_err(DEADLINE_EXCEEDED), step).action is FailureAction.ABORT


def test_aborted_and_reinit_keep_fixed_caps():
    """ABORTED（物理遮挡自愈 3 次）与 REINIT（崩溃恢复 1 次）不随 max_retries 改变。"""
    r = Reflector()
    step = ActionNode("s1", "click", "login", max_retries=0)
    # ABORTED 仍按固定上限 3 次重试。
    for _ in range(3):
        assert r.classify(_err(ABORTED), step).action is FailureAction.RETRY
    assert r.classify(_err(ABORTED), step).action is FailureAction.ABORT


def test_vault_unresolved_pauses_instead_of_reinit():
    """凭据直填失败（`vault_unresolved:` 前缀）走 PAUSE 透传原话，不耗 REINIT 预算。

    2026-09-23 真机实测：缺 `@vault:<名>` 条目时若按泛化 FAILED_PRECONDITION 处理，
    会白做一次上下文重建并把报告原因显示成 reinit_exhausted，用户看不出真因。
    """
    r = Reflector()
    step = ActionNode("s1", "input", "搜索实例名称")
    err = EngineError(
        FAILED_PRECONDITION,
        "vault_unresolved: 凭据直填失败 @vault:nothere: vault: 没有这一条",
    )
    decision = r.classify(err, step)
    assert decision.action is FailureAction.PAUSE
    assert "凭据直填失败" in decision.reason
    assert step.retry_count == 0  # 不消费 REINIT 预算

    step2 = ActionNode("s2", "click", "login", max_retries=5)
    assert r.classify(_err(FAILED_PRECONDITION), step2).action is FailureAction.REINIT
    assert r.classify(_err(FAILED_PRECONDITION), step2).action is FailureAction.ABORT


def test_resource_exhausted_pauses_with_engine_reason():
    """RESOURCE_EXHAUSTED（含防乱点熔断）→ PAUSE，且 reason 保留引擎原话。

    2026-09-23：防乱点熔断此前被引擎误报为 UNAUTHENTICATED，协调器于是走 VLM
    破解并最终 PAUSED「challenge_intercepted」——假验证码报警。引擎改报
    RESOURCE_EXHAUSTED 后，这里必须把引擎给的实话（如 anti-thrashing guard
    OPEN）原样带给用户，而不是泛化成 resource_exhausted。
    """
    from coordinator.engine_client import RESOURCE_EXHAUSTED

    r = Reflector()
    step = ActionNode("s1", "click", "login")
    decision = r.classify(
        EngineError(RESOURCE_EXHAUSTED, "anti-thrashing guard OPEN (not a human challenge): ..."),
        step,
    )
    assert decision.action is FailureAction.PAUSE
    assert "anti-thrashing" in decision.reason


def test_aborted_exhausted_preserves_engine_reason():
    """ABORTED 错误若内部已耗尽（如 after 3 attempts），直接 ABORT 且保留真实故障代号。"""
    r = Reflector()
    step = ActionNode("s1", "input", "search_box")
    err = EngineError(
        ABORTED,
        "input_failed:TARGET_NOT_EDITABLE: span \"Sign in\" (after 3 attempts)",
    )
    decision = r.classify(err, step)
    assert decision.action is FailureAction.ABORT
    assert "TARGET_NOT_EDITABLE" in decision.reason
    assert 'span "Sign in"' in decision.reason
    assert step.retry_count == 0  # 不耗协调器重试预算


def test_unknown_fallback_preserves_engine_reason():
    """未显式识别的状态码走第 110 行兜底时，必须透传引擎原话，不再覆写为 unknown_error。"""
    from coordinator.engine_client import INTERNAL

    r = Reflector()
    step = ActionNode("s1", "input", "search_box")
    err = EngineError(INTERNAL, "matcher: candidate pool exhausted")
    decision = r.classify(err, step)
    assert decision.action is FailureAction.ABORT
    assert decision.reason == "matcher: candidate pool exhausted"

