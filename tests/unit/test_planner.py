"""规划器单元测试（TDD-PLAN-01/02/04，tdd-checklist.md §4.4）。"""

import pytest

from coordinator.models import ActionNode
from coordinator.planner import (
    NoTemplateMatch,
    ParsedPlan,
    Planner,
    PlanValidationError,
    TemplateRegistry,
    parse_plan_dsl,
    validate_plan,
)


def _login_template() -> dict:
    return {
        "template_id": "login_generic_v2",
        "name": "通用登录流程",
        "description": "登录并进入后台",
        "parameters": ["username", "password", "login_button_text"],
        "steps": [
            {
                "step_id": "input_user",
                "action_type": "input",
                "target_semantic": "username_input",
                "value": "{{ username }}",
                "depend_on": [],
            },
            {
                "step_id": "input_pwd",
                "action_type": "input",
                "target_semantic": "password_input",
                "value": "{{ password }}",
                "depend_on": ["input_user"],
            },
            {
                "step_id": "click_submit",
                "action_type": "click",
                "target_semantic": "{{ login_button_text }}",
                "depend_on": ["input_pwd"],
                "on_failure": "replan",
            },
        ],
    }


def test_tdd_plan_01_l1_exact_template_instantiation():
    registry = TemplateRegistry()
    registry.register(_login_template())
    llm_called: list[int] = []

    def llm(query, snippets):
        llm_called.append(1)
        return {"steps": []}

    planner = Planner(registry, llm=llm)
    plan = planner.plan(
        "帮我登录",
        {"username": "alice", "password": "s3cret", "login_button_text": "登录"},
        template_id="login_generic_v2",
    )

    assert plan.source == "l1_template"
    assert plan.template_id == "login_generic_v2"
    assert [s.step_id for s in plan.steps] == ["input_user", "input_pwd", "click_submit"]
    assert plan.steps[0].value == "alice"
    assert plan.steps[1].value == "s3cret"
    assert plan.steps[2].target_semantic == "登录"
    assert plan.steps[2].on_failure == "replan"
    assert not llm_called  # L1 精确命中全程不调用 LLM

    # 缺参数必须报错而不是静默插值
    with pytest.raises(PlanValidationError):
        planner.plan("帮我登录", {"username": "alice"}, template_id="login_generic_v2")


def test_tdd_plan_02_l2_fuzzy_embedding_top1():
    registry = TemplateRegistry()
    registry.register(_login_template())
    registry.register(
        {
            "template_id": "ecommerce_extract_products_v1",
            "name": "商品列表分页提取",
            "description": "遍历商品列表页并提取价格",
            "parameters": ["max_pages"],
            "steps": [
                {
                    "step_id": "extract",
                    "action_type": "extract",
                    "target_semantic": "product_card_list",
                    "depend_on": [],
                }
            ],
        }
    )

    class FakeEmbedder:
        def similarity(self, text_a: str, text_b: str) -> float:
            if "登录" in text_a and text_b == "登录并进入后台":
                return 0.92
            return 0.0

    llm_called: list[int] = []

    def llm(query, snippets):
        llm_called.append(1)
        return {"steps": []}

    planner = Planner(registry, embedder=FakeEmbedder(), llm=llm)
    plan = planner.plan(
        "帮我登录", {"username": "a", "password": "b", "login_button_text": "登录"}
    )
    assert plan.source == "l2_template"
    assert plan.template_id == "login_generic_v2"

    # 低于阈值不得误命中，也不得调用 LLM
    with pytest.raises(NoTemplateMatch):
        planner.plan("随便看看", {"max_pages": "3"})
    assert not llm_called


def test_tdd_plan_04_plan_validation_rejects_cycles_deadlinks():
    bad = [
        ActionNode("a", "click", "A", depend_on=["b"]),
        ActionNode("b", "click", "B", depend_on=["a"]),  # 环
        ActionNode("c", "input", "", depend_on=["a"]),  # 空 target_semantic
        ActionNode("d", "click", "{{ var }}", depend_on=["missing"]),  # 纯占位 + 死链
        ActionNode("e", "teleport", "E"),  # 非法 action_type
    ]
    joined = "\n".join(validate_plan(bad))
    assert "循环依赖" in joined
    assert "死链" in joined
    assert "空/纯占位" in joined
    assert "非法 action_type" in joined

    good = [
        ActionNode("s1", "input", "username_input", value="alice"),
        ActionNode("s2", "click", "登录", depend_on=["s1"]),
    ]
    assert validate_plan(good) == []


def test_parse_plan_dsl_search_segment_returns_ingredient():
    """TDD-PLAN (append-only)：DSL 分解「搜索A类精华并返回成分表」为真实动作序列。

    nav:/reply: 是计划级指令（不产 ActionNode）；search: 宏展开为 input+click；
    `| $成分` 绑定 extract 输出；步依赖线性串联。
    """
    dsl = """\
nav:首页
search:A类精华
click:第1个搜索结果
extract:成分表 | $成分
reply:$成分
"""
    parsed = parse_plan_dsl(dsl)
    assert isinstance(parsed, ParsedPlan)
    assert parsed.initial_url == "首页"
    assert parsed.result == "$成分"
    assert parsed.bindings == {parsed.steps[-1].step_id: "成分"}
    # 宏展开：input(search_box) + click(search_button) + click(结果) + extract(成分表)
    assert [s.action_type for s in parsed.steps] == ["input", "click", "click", "extract"]
    assert parsed.steps[0].value == "A类精华"
    assert parsed.steps[0].target_semantic == "search_box"
    assert parsed.steps[1].target_semantic == "search_button"
    assert parsed.steps[2].target_semantic == "第1个搜索结果"
    assert parsed.steps[3].target_semantic == "成分表"
    # 线性依赖
    assert parsed.steps[1].depend_on == [parsed.steps[0].step_id]
    assert parsed.steps[2].depend_on == [parsed.steps[1].step_id]
    assert parsed.steps[3].depend_on == [parsed.steps[2].step_id]
    # 合法计划
    assert validate_plan(parsed.steps) == []


def test_parse_plan_dsl_fallback_login_and_reply():
    """`|| login` → on_failure=replan；`|| reply:...` → on_failure=abort；
    `?guard` 记录为前置守卫（不产步）。"""
    dsl = """\
?login:账号
search:A醇精华 || login:账号
click:第1个搜索结果 || reply:没有可点结果
"""
    parsed = parse_plan_dsl(dsl)
    assert parsed.preconditions == ["login:账号"]
    assert [s.action_type for s in parsed.steps] == ["input", "click", "click"]
    assert all(s.on_failure == "replan" for s in parsed.steps[:2])  # 宏内两步
    assert parsed.steps[2].on_failure == "abort"


def test_plan_from_llm_accepts_dsl_string():
    """L3 LLM 直接输出 DSL 字符串时走同一校验门禁，结果为真实 ActionNode。"""
    registry = TemplateRegistry()

    def llm(query, snippets):
        return "search:A类精华\nclick:第1个结果 || reply:未找到\nextract:成分表\nreply:$成分"

    planner = Planner(registry, llm=llm)
    plan = planner.plan("搜索A类精华并返回成分表", {}, allow_llm=True)
    assert plan.source == "l3_llm"
    assert plan.result == "$成分"
    assert [s.action_type for s in plan.steps] == ["input", "click", "click", "extract"]


def test_parse_plan_dsl_rejects_pipe_in_target():
    """`|` 是保留分隔符，target 含裸 `|` 应在解析期报错（引擎白名单不允许），
    而不是运行期 INVALID_ARGUMENT；value 里的 `|`（非 target）不受限。"""
    with pytest.raises(PlanValidationError, match="保留分隔符"):
        parse_plan_dsl("click:搜索结果|第1个")
    # value 含 `|` 合法（引擎 value 字段无字符白名单）
    parsed = parse_plan_dsl("input:搜索框=精华|乳霜")
    assert parsed.steps[0].value == "精华|乳霜"


def test_parse_plan_dsl_node_ref_compiles_to_node_id():
    plan = parse_plan_dsl("click:node:12\nextract:node:31")
    assert len(plan.steps) == 2
    assert plan.steps[0].action_type == "click"
    assert plan.steps[0].node_id == 12
    assert plan.steps[0].target_semantic == "node:12"
    assert plan.steps[1].action_type == "extract"
    assert plan.steps[1].node_id == 31
    assert plan.steps[1].depend_on == ["s1"]
    # 编译产物必须能过 validate_plan（确定性引用步骤 target 非空）
    assert validate_plan(plan.steps) == []


def test_parse_plan_dsl_node_ref_rejects_non_positive():
    with pytest.raises(PlanValidationError, match="node:"):
        parse_plan_dsl("click:node:abc")
    with pytest.raises(PlanValidationError, match="node:"):
        parse_plan_dsl("click:node:0")
    with pytest.raises(PlanValidationError, match="node:"):
        parse_plan_dsl("click:node:")


def test_parse_plan_dsl_drag_verb_compiles_to_coord_step():
    """TDD-PLAN-10：DSL `drag:x1,y1,x2,y2` 编译为 ActionNode
    （target 固定语义 + value 坐标），编译产物过 validate_plan。"""
    plan = parse_plan_dsl("drag:124,200,300,200")
    assert [s.action_type for s in plan.steps] == ["drag"]
    step = plan.steps[0]
    assert step.target_semantic == "drag"
    assert step.value == "124,200,300,200"
    assert validate_plan(plan.steps) == []


def test_parse_plan_dsl_drag_tolerates_separators_and_rejects_bad_coords():
    """坐标分隔容忍（逗号/空格混合，镜像引擎 ParseDragCoords）；
    非 4 数字 / 非数字 / 空操作数在解析期报错。"""
    plan = parse_plan_dsl("drag:124 200 300 200\ndrag:124, 200, 300, 200")
    assert [s.value for s in plan.steps] == ["124 200 300 200", "124, 200, 300, 200"]
    with pytest.raises(PlanValidationError, match="drag"):
        parse_plan_dsl("drag:124,200,300")
    with pytest.raises(PlanValidationError, match="drag"):
        parse_plan_dsl("drag:abc,200,300,200")
    with pytest.raises(PlanValidationError, match="drag"):
        parse_plan_dsl("drag:")
