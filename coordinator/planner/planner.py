"""规划器（spec/coordinator.md §4）：L1 精确模板 / L2 模糊模板 / L3 LLM 补全 / 计划校验。

生产 L2 建议 MiniLM/BGE 向量检索；本实现提供可插拔 Embedder 协议 +
确定性本地相似度降级（零外部依赖），真实向量模型按部署接入。
"""

from __future__ import annotations

import re
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any, Protocol

from coordinator.models import ActionNode, TaskContext

L2_THRESHOLD = 0.85
VALID_ACTIONS = {"click", "input", "select", "focus", "hover", "scroll", "wait", "extract", "drag"}
PLACEHOLDER_RE = re.compile(r"\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}")
PURE_PLACEHOLDER_RE = re.compile(r"^\s*\{\{.*\}\}\s*$")


class NoTemplateMatch(Exception):
    """L1/L2 均未命中且未启用/未配置 LLM。"""


class PlanValidationError(ValueError):
    """计划非法（环/死链/空 target/非法 action 等）。"""


def interpolate(text: str, variables: dict[str, Any]) -> str:
    """`{{ var }}` 变量插值；缺失变量视为计划错误（不静默留占位）。"""

    def _repl(match: re.Match[str]) -> str:
        key = match.group(1)
        if key not in variables:
            raise PlanValidationError(f"变量缺失: {key}")
        return str(variables[key])

    return PLACEHOLDER_RE.sub(_repl, text)


def validate_plan(steps: list[ActionNode]) -> list[str]:
    """计划校验：重复 ID / 非法 action / 空或纯占位 target / 死链 / 循环依赖。"""

    errors: list[str] = []
    ids = [s.step_id for s in steps]
    id_set = set(ids)
    if len(ids) != len(id_set):
        errors.append("重复 step_id")

    for s in steps:
        if s.action_type not in VALID_ACTIONS:
            errors.append(f"{s.step_id}: 非法 action_type '{s.action_type}'")
        ts = (s.target_semantic or "").strip()
        if not ts or PURE_PLACEHOLDER_RE.match(ts):
            errors.append(f"{s.step_id}: 空/纯占位 target_semantic")
        for dep in s.depend_on:
            if dep not in id_set:
                errors.append(f"{s.step_id}: 死链依赖 '{dep}'")

    # 环检测：DFS 三色标记（沿 depend_on 边）
    color: dict[str, int] = {}

    def _visit(node_id: str) -> bool:
        color[node_id] = 1
        node = next((s for s in steps if s.step_id == node_id), None)
        if node is None:
            return False
        for dep in node.depend_on:
            if color.get(dep, 0) == 1:
                return True
            if color.get(dep, 0) == 0 and _visit(dep):
                return True
        color[node_id] = 2
        return False

    for s in steps:
        if _visit(s.step_id):
            errors.append("存在循环依赖")
            break
    return errors


# ── DSL 规划（请求级自动分解，v0.1；append-only）────────────────────────
# 目标：用「少 token、无成对关键字」的紧凑 DSL 描述浏览器任务意图，
# 编译为真实可执行 ActionNode 序列（动词必须是引擎动作/宏）。
# 参考用户骨架 find(xx) and do(xx) or login else reply('fail') 的去括号化：
#   `a || b` = 失败回退（编译为 on_failure），`?guard` = 前置守卫，
#   `| $var` = 该步输出绑定到变量，`search:X` = 宏（input+click），
#   `nav:`/`reply:` = 计划级指令（不产 ActionNode，由调用方消费），
#   `node:<id>` = 确定性引用（VLM 指挥官返回的探针 BFS 编号，跳过语义派发）。
DSL_VALUE_VERBS = {"input", "select"}
DSL_SINGLE_VERBS = {"click", "focus", "hover", "scroll", "wait", "extract"}
# drag: 操作数整体即坐标值（引擎 ParseDragCoords 语义 x1,y1,x2,y2），target 用固定语义占位
DSL_COORD_VERBS = {"drag"}
DSL_DRAG_SEMANTIC = "drag"
# search 宏展开时的默认语义标签（LLM 可从 AXTree 改写；v0.1 用默认值）
DSL_SEARCH_INPUT_SEMANTIC = "search_box"
DSL_SEARCH_BUTTON_SEMANTIC = "search_button"


@dataclass
class ParsedPlan:
    """DSL 解析结果：steps 为真实可执行动作序列；其余为计划级指令（供调用方消费）。

    initial_url 来自 `nav:`，result 来自 `reply:`（可能含 `$var`），bindings 把
    `| $var` 的输出绑定到变量，preconditions 记录 `?guard:`。
    v0.1：executor 只消费 steps；initial_url/result/bindings 的运行时打通为下一项待办。
    """

    steps: list[ActionNode]
    initial_url: str | None = None
    result: str | None = None
    bindings: dict[str, str] = field(default_factory=dict)
    preconditions: list[str] = field(default_factory=list)


def _split_binding(line: str) -> tuple[str, str | None]:
    """拆出行尾 `| $var`；无则返回 (原行, None)。`|` 是 DSL 保留分隔符
    （绑定/回退），操作数 target 不允许裸 `|`（引擎 target_semantic 白名单
    只含字母/数字/下划线/连字符/空格/中文，解析期校验见 parse_plan_dsl）。"""
    m = re.search(r"\|\s*\$([^\s|]+)\s*$", line)
    if m:
        return line[: m.start()].rstrip(), m.group(1)
    return line, None


def _split_orfail(line: str) -> tuple[str, str | None]:
    """按首个 `||` 拆出主步骤与失败回退；无则 (原行, None)。"""
    parts = re.split(r"\s*\|\|\s*", line, maxsplit=1)
    if len(parts) == 2:
        return parts[0].strip(), parts[1].strip()
    return line.strip(), None


def _failure_action(fallback: str) -> str:
    """`|| <回退>` 映射到 ActionNode.on_failure（v0.1 规则）。

    reply:... → abort（失败即结束，消息是调用方侧计划元数据）；
    login... → replan（交由 Reflector 生成登录+重试子计划）；
    skip/retry/replan → 原样；其余 → replan。
    """
    fb = (fallback or "").strip().lower()
    if fb.startswith("reply:"):
        return "abort"
    if fb.startswith("login"):
        return "replan"
    if fb in ("skip", "retry", "replan"):
        return fb
    return "replan"


def _emit_step(
    nodes: list[ActionNode],
    counter: list[int],
    action: str,
    target: str,
    value: str | None = None,
    node_id: int | None = None,
) -> None:
    counter[0] += 1
    nodes.append(
        ActionNode(
            step_id=f"s{counter[0]}",
            action_type=action,
            target_semantic=target,
            value=value,
            node_id=node_id,
            # 线性计划：顺序依赖上一独立步（executor 依赖优先，语义明确）
            depend_on=[nodes[-1].step_id] if nodes else [],
        )
    )


def _parse_node_ref(operand: str, line: str) -> tuple[str, int | None]:
    """`node:<id>` 确定性引用 → (target_semantic, node_id)。

    探针 BFS 编号必须为正整数；语法上保留 `node:<id>` 可读性，执行层消费
    `ActionNode.node_id` 回查定位器（引擎接线为下一步待办，见 GLOBAL §5 #57）。
    """
    if not operand.lower().startswith("node:"):
        return operand, None
    raw = operand.split(":", 1)[1].strip()
    if not raw.isdigit() or int(raw) <= 0:
        raise PlanValidationError(f"DSL node: 引用必须为正整数: {line!r}")
    return operand, int(raw)


def _ensure_target_safe(target: str, line: str) -> None:
    """引擎 target_semantic 白名单不含 `|`（sanitize.go semanticRe）；裸 `|`
    在解析期报错而不是运行期 INVALID_ARGUMENT，提示改写语义（如 第1个结果）。"""
    if "|" in target:
        raise PlanValidationError(
            f"DSL target 含 `|`（保留分隔符），引擎语义白名单不允许: {line!r}；"
            "请改写操作数（如 click:第1个结果，而不是 click:结果|第1个）"
        )


def _validate_drag_coords(value: str, line: str) -> str:
    """校验 DSL `drag:` 坐标操作数（镜像引擎 ParseDragCoords 容忍度）。

    `124,200,300,200` / `124 200 300 200` 均合法；必须恰好 4 个数字，
    否则解析期 PlanValidationError（不等到运行期 ABORTED）。
    """
    parts = [p for p in re.split(r"[\s,]+", value.strip()) if p]
    if len(parts) != 4:
        raise PlanValidationError(
            f"DSL drag: 坐标必须为 x1,y1,x2,y2 四个数字: {line!r}"
        )
    try:
        list(map(float, parts))
    except ValueError as exc:
        raise PlanValidationError(
            f"DSL drag: 坐标含非数字分量: {line!r}"
        ) from exc
    return value.strip()


def parse_plan_dsl(text: str, variables: dict[str, Any] | None = None) -> ParsedPlan:
    """把紧凑 DSL 编译为真实可执行 ActionNode 序列。

    文法（一行一意图；`:` 分隔动词/操作数，`=` 分离目标与值，`| $var` 绑定，
    `|| 回退` 失败处理，`?guard` 前置守卫，`#` 注释，空行忽略）：

        nav:首页                        # 计划级初始 URL（不产步）
        search:A醇精华                  # 宏 → input:search_box + click:search_button
        drag:124,200,300,200            # 原生拖拽（坐标直发引擎，target 语义固定为 drag）
        click:第1个搜索结果 || reply:无结果
        click:node:12 / extract:node:31 # 确定性引用（探针 BFS 编号，跳过语义派发）
        extract:成分表 | $成分
        reply:$成分                     # 计划级结果表达式（不产步）
        ?login:账号                     # 前置守卫（记录，不产步）
    """
    variables = variables or {}
    counter = [0]
    parsed = ParsedPlan(steps=[])
    for raw_line in (text or "").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        # 行尾绑定 `| $var`
        line, var = _split_binding(line)
        # 拆失败回退 `||`
        main, fallback = _split_orfail(line)
        if main.startswith("?"):
            parsed.preconditions.append(main[1:].strip())
            continue
        if main.startswith("nav:"):
            parsed.initial_url = main[4:].strip()
            continue
        if main.startswith("reply:"):
            parsed.result = main[6:].strip()
            continue

        # `search:X` 宏 → input + click
        if main.startswith("search:"):
            query = interpolate(main[len("search:") :].strip(), variables)
            before = len(parsed.steps)
            _emit_step(parsed.steps, counter, "input", DSL_SEARCH_INPUT_SEMANTIC, value=query)
            _emit_step(parsed.steps, counter, "click", DSL_SEARCH_BUTTON_SEMANTIC)
            emitted = parsed.steps[before:]
            if fallback:
                act = _failure_action(fallback)
                for n in emitted:
                    n.on_failure = act
            if var:
                parsed.bindings[emitted[-1].step_id] = var
            continue

        if ":" not in main:
            raise PlanValidationError(f"DSL 行缺少 `:` 动词分隔符: {raw_line!r}")
        verb, operand = main.split(":", 1)
        verb, operand = verb.strip(), operand.strip()
        if verb in DSL_VALUE_VERBS and "=" in operand:
            target, value = operand.split("=", 1)
            _ensure_target_safe(target.strip(), raw_line)
            _emit_step(
                parsed.steps, counter, verb,
                interpolate(target.strip(), variables),
                value=interpolate(value.strip(), variables),
            )
        elif verb in DSL_SINGLE_VERBS:
            _ensure_target_safe(operand, raw_line)
            target, node_id = _parse_node_ref(interpolate(operand, variables), raw_line)
            _emit_step(parsed.steps, counter, verb, target, node_id=node_id)
        elif verb in DSL_COORD_VERBS:
            coords = _validate_drag_coords(interpolate(operand, variables), raw_line)
            _emit_step(parsed.steps, counter, verb, DSL_DRAG_SEMANTIC, value=coords)
        else:
            raise PlanValidationError(
                f"非法 DSL 动词 `{verb}`（可用 "
                f"{sorted(DSL_SINGLE_VERBS | DSL_VALUE_VERBS | DSL_COORD_VERBS | {'search'})}）"
            )
        node = parsed.steps[-1]
        if fallback:
            node.on_failure = _failure_action(fallback)
        if var:
            parsed.bindings[node.step_id] = var
    errors = validate_plan(parsed.steps)
    if errors:
        raise PlanValidationError("; ".join(errors))
    return parsed


class Embedder(Protocol):
    def similarity(self, text_a: str, text_b: str) -> float: ...


class TokenOverlapEmbedder:
    """零依赖降级实现：字符 bigram Jaccard 相似度（确定性、无外部模型）。

    生产环境替换为 MiniLM/BGE 向量检索（spec/coordinator.md §4.1）。
    """

    def similarity(self, text_a: str, text_b: str) -> float:
        a = self._grams(text_a)
        b = self._grams(text_b)
        if not a or not b:
            return 0.0
        return len(a & b) / (len(a | b) or 1)

    @staticmethod
    def _grams(text: str) -> set[str]:
        text = "".join(text.split()).lower()
        if len(text) <= 1:
            return {text} if text else set()
        return {text[i : i + 2] for i in range(len(text) - 1)}


@dataclass
class TaskTemplate:
    template_id: str
    name: str
    description: str = ""
    parameters: list[str] = field(default_factory=list)
    steps: list[dict[str, Any]] = field(default_factory=list)

    def to_nodes(self, variables: dict[str, Any]) -> list[ActionNode]:
        nodes: list[ActionNode] = []
        for raw in self.steps:
            step = dict(raw)
            nodes.append(
                ActionNode(
                    step_id=str(step["step_id"]),
                    action_type=str(step["action_type"]),
                    target_semantic=interpolate(str(step.get("target_semantic", "")), variables),
                    value=(
                        interpolate(str(step["value"]), variables)
                        if step.get("value") is not None
                        else None
                    ),
                    condition=str(step["condition"]) if step.get("condition") else None,
                    loop=step.get("loop"),
                    timeout_ms=int(step.get("timeout_ms", 1500)),
                    max_retries=int(step.get("max_retries", 2)),
                    on_failure=str(step.get("on_failure", "abort")),
                    depend_on=[str(d) for d in step.get("depend_on", [])],
                    non_idempotent=bool(step.get("non_idempotent", False)),
                )
            )
        return nodes


class TemplateRegistry:
    def __init__(self) -> None:
        self._templates: dict[str, TaskTemplate] = {}

    def register(self, template: TaskTemplate | dict[str, Any]) -> None:
        if isinstance(template, TaskTemplate):
            item = template
        else:
            item = TaskTemplate(
                template_id=str(template["template_id"]),
                name=str(template.get("name", "")),
                description=str(template.get("description", "")),
                parameters=[str(p) for p in template.get("parameters", [])],
                steps=[dict(s) for s in template.get("steps", [])],
            )
        self._templates[item.template_id] = item

    def get(self, template_id: str) -> TaskTemplate | None:
        return self._templates.get(template_id)

    def all(self) -> list[TaskTemplate]:
        return list(self._templates.values())


@dataclass
class Plan:
    steps: list[ActionNode]
    source: str  # l1_template | l2_template | l3_llm | l3_replan
    template_id: str | None = None
    initial_url: str | None = None  # DSL `nav:` 指令（计划级，非步骤）
    result: str | None = None  # DSL `reply:` 指令（任务结果表达式，可能含 $var）


class Planner:
    def __init__(
        self,
        registry: TemplateRegistry,
        embedder: Embedder | None = None,
        llm: Callable[[str, list[str]], dict[str, Any]] | None = None,
    ) -> None:
        self._registry = registry
        self._embedder: Embedder = embedder or TokenOverlapEmbedder()
        self._llm = llm

    def plan(
        self,
        task_name: str,
        variables: dict[str, Any],
        template_id: str | None = None,
        allow_llm: bool = False,
    ) -> Plan:
        # L1 精确模板：template_id 命中直接实例化（全程不调用 LLM）
        if template_id:
            template = self._registry.get(template_id)
            if template is not None:
                return self._instantiate(template, variables, "l1_template")

        # L2 模糊模板：Embedding 相似度 > 0.85 命中 Top1
        best: TaskTemplate | None = None
        best_score = 0.0
        for template in self._registry.all():
            if not template.description:
                continue
            score = self._embedder.similarity(task_name, template.description)
            if score > best_score:
                best, best_score = template, score
        if best is not None and best_score >= L2_THRESHOLD:
            return self._instantiate(best, variables, "l2_template")

        # L3 LLM 补全（仅显式开启；低于阈值不误命中、不自动调 LLM）
        if allow_llm:
            return self._plan_with_llm(task_name, variables, [])
        raise NoTemplateMatch(f"未命中模板: {task_name}")

    def replan(
        self, task_name: str, context: TaskContext, failed_step: ActionNode
    ) -> Plan:
        """局部重规划：以失败步骤为锚点，LLM 生成替代子计划（spec §6.1 NOT_FOUND）。"""
        if self._llm is None:
            raise NoTemplateMatch("未配置 LLM，无法重规划")
        snippets = [f"{failed_step.step_id}: {failed_step.action_type} {failed_step.target_semantic}"]
        raw = self._llm(task_name, snippets)
        return self._plan_from_llm(raw, context.variables, source="l3_replan")

    def _instantiate(
        self, template: TaskTemplate, variables: dict[str, Any], source: str
    ) -> Plan:
        nodes = template.to_nodes(variables)
        errors = validate_plan(nodes)
        if errors:
            raise PlanValidationError("; ".join(errors))
        return Plan(steps=nodes, source=source, template_id=template.template_id)

    def _plan_with_llm(
        self, task_name: str, variables: dict[str, Any], snippets: list[str]
    ) -> Plan:
        if self._llm is None:
            raise NoTemplateMatch("未配置 LLM")
        raw = self._llm(task_name, snippets)
        return self._plan_from_llm(raw, variables, source="l3_llm")

    def _plan_from_llm(
        self, raw: dict[str, Any] | str, variables: dict[str, Any], source: str
    ) -> Plan:
        # DSL 字符串计划：去括号化、无成对关键字；仍在同一校验门禁内。
        if isinstance(raw, str):
            parsed = parse_plan_dsl(raw, variables)
            errors = validate_plan(parsed.steps)
            if errors:
                raise PlanValidationError("; ".join(errors))
            return Plan(
                steps=parsed.steps,
                source=source,
                initial_url=parsed.initial_url,
                result=parsed.result,
            )
        if not isinstance(raw, dict) or not isinstance(raw.get("steps"), list):
            raise PlanValidationError("LLM 输出缺少 steps 列表")
        nodes: list[ActionNode] = []
        for index, step in enumerate(raw["steps"], start=1):
            if not isinstance(step, dict):
                raise PlanValidationError("LLM 步骤不是对象")
            action_type = str(step.get("action_type", ""))
            if action_type not in VALID_ACTIONS:
                raise PlanValidationError(f"非法 action_type: {action_type}")
            target = interpolate(str(step.get("target_semantic", "")), variables).strip()
            if not target or PURE_PLACEHOLDER_RE.match(target):
                raise PlanValidationError("空/纯占位 target_semantic")
            nodes.append(
                ActionNode(
                    step_id=str(step.get("step_id") or f"llm_{index}"),
                    action_type=action_type,
                    target_semantic=target,
                    value=str(step["value"]) if step.get("value") is not None else None,
                    depend_on=[str(d) for d in step.get("depends_on", [])],
                )
            )
        # LLM 常以目标语义而非 step_id 声明依赖：做一次语义重映射
        by_semantic = {s.target_semantic: s.step_id for s in nodes}
        for node in nodes:
            node.depend_on = [by_semantic.get(dep, dep) for dep in node.depend_on]
        errors = validate_plan(nodes)
        if errors:
            raise PlanValidationError("; ".join(errors))
        return Plan(steps=nodes, source=source)
