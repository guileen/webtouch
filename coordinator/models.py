"""协调器侧数据模型（spec/coordinator.md §3）。

ActionNode 在规格字段之上补充 `non_idempotent`（指南 §5：崩溃恢复只重放
幂等步骤；支付/发帖等外部副作用动作显式标记，绝不自动重放）。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ActionNode:
    step_id: str
    action_type: str
    target_semantic: str
    value: str | None = None
    condition: str | None = None
    loop: dict[str, Any] | None = None
    timeout_ms: int = 1500
    retry_count: int = 0
    max_retries: int = 2
    on_failure: str = "abort"  # abort | retry | skip | replan
    depend_on: list[str] = field(default_factory=list)
    non_idempotent: bool = False
    node_id: int | None = None  # 确定性引用（探针 BFS 编号；VLM 指挥官返回；执行接线待引擎 RPC）


@dataclass
class TaskContext:
    """跨步骤运行时上下文（spec/coordinator.md §3.3）。"""

    variables: dict[str, Any] = field(default_factory=dict)
    history: list[Any] = field(default_factory=list)
    visited_urls: set[str] = field(default_factory=set)
    current_target_id: str | None = None
    current_state: str = "IDLE"
    navigation_counter: int = 0
    decision_trace: list[str] = field(default_factory=list)
    # 执行护栏（2026-09-03）：页面 AXTree 命中的付费 CTA 词；非空时后续
    # click/input/select 先暂停交用户确认（coordinator/guard.py）。
    page_risk: str = ""


@dataclass
class TaskDefinition:
    task_id: str
    name: str
    mode: str = "linear"
    template_ref: str | None = None
    plan: list[ActionNode] = field(default_factory=list)
    context: TaskContext = field(default_factory=TaskContext)
    max_steps: int = 50
    status: str = "PENDING"  # PENDING | RUNNING | PAUSED | SUCCEEDED | FAILED
    error_reason: str = ""
    # 失败证据（2026-09-03 反馈可消费化）：终结失败的步骤 id，供 uws run 生成
    # Failure 报告（原因 + 当前 URL + AXTree + 截图 + 可选 VLM 诊断），
    # 让上层 Agent 知道“卡在哪一步、页面现状是什么”，而不是只知道状态码。
    failed_step_id: str = ""
    # DSL 计划级元数据（v0.1 运行时打通）：bindings 把 `| $var` 的输出绑定到
    # 变量（executor 把 extract 结果写回 context.variables）；result 为 `reply:`
    # 结果表达式（可能含 `$var`，由调用方在任务结束后插值）。
    bindings: dict[str, str] = field(default_factory=dict)
    result: str | None = None
