"""重试与反思器：错误分类、退避重试、回退与 VLM 视觉接管。"""
"""失败分类与反思（spec/coordinator.md §6）。"""

from coordinator.reflector.reflector import (
    FailureAction,
    FailureDecision,
    Reflector,
)

__all__ = ["FailureAction", "FailureDecision", "Reflector"]
