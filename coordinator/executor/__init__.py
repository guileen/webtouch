"""调度执行器：任务状态机、并发 Target 与动作循环（spec/coordinator.md §5）。"""
"""调度执行器（TDD-COORD-01..09，spec/coordinator.md §5）。"""

from coordinator.executor.executor import Executor

__all__ = ["Executor"]
