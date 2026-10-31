"""chat 目标模式（coordinator/goal.py）单元测试。

不启动真实引擎/VLM：注入 FakeEngine + 脚本化 vlm_ask，覆盖
成功链路（click→input→click→done）、付费护栏暂停、登录等待、
DSL 语法自动路由判定与站点/URL 推断。
"""

from __future__ import annotations

import pytest


@pytest.fixture(autouse=True)
def isolated_attach(monkeypatch):
    """Fake-engine tests never consult or launch a host browser."""
    monkeypatch.setattr("coordinator.goal.resolve_attach", lambda *args, **kwargs: "http://127.0.0.1:9222")

import os
from urllib.parse import quote_plus

from coordinator.engine_client import ActionResult, HealthStatus, InitResult
from coordinator.goal import (
    GoalOutcome,
    VLM_TREE_MAX_CHARS,
    _render_report,
    default_vlm_ask,
    infer_initial_url,
    is_dsl_prompt,
    parse_decision,
    parse_tree_nodes,
    run_goal,
)


class StageEngine:
    """按执行次数推进页面树的 FakeEngine（确定性“发布微博”场景）。"""

    def __init__(self, trees: list[str], base_url: str | None = None) -> None:
        self.trees = list(trees)
        self.base_url = base_url
        self.executed: list[tuple[str, int, str | None]] = []
        self.context_names: list[str | None] = []
        self.context_hints: list[dict[str, str]] = []
        self.initial_url: str | None = None
        self._idx = 0

    def initialize_context(self, task_id: str, initial_url: str | None = None, **kwargs: object) -> InitResult:
        self.initial_url = initial_url
        self.context_names.append(str(kwargs.get("context_name") or ""))
        return InitResult(
            ok=True,
            target_id="goal-target",
            context_id="ctx-goal",
            current_url=self.base_url or "",
        )

    def export_page_tree(self, target_id: str) -> str:
        return self.trees[min(self._idx, len(self.trees) - 1)]

    def capture_screenshot(self, target_id: str) -> bytes:
        return b"\x89PNG-fake-goal"

    def execute_action(
        self,
        intent_id: str,
        target_semantic: str,
        expected_action: str,
        value: str | None = None,
        context_hint: dict[str, str] | None = None,
        timeout_ms: int = 1500,
        node_id: int | None = None,
    ) -> ActionResult:
        self.executed.append((expected_action, node_id or 0, value))
        self.context_hints.append(dict(context_hint or {}))
        if self._idx + 1 < len(self.trees):
            self._idx += 1
        if self.base_url:
            current = self.base_url
        elif node_id == 4:
            current = "https://weibo.com/compose/ok"
        else:
            current = "https://weibo.com/compose"
        return ActionResult(
            success=True,
            context_state="STABLE",
            current_url=current,
        )

    def health_check(self) -> HealthStatus:
        return HealthStatus(ok=True)

    def attach_target(self, target_id: str) -> str:
        return "target-new"

    def close_context(self, context_name: str = "") -> None:
        return None


class EmptyThenNavEngine:
    """先给空白树；收到带 URL 的 initialize 后切换为真实页面树（模拟 VLM goto）。"""

    def __init__(self) -> None:
        self.initial_urls: list[str | None] = []
        self.context_names: list[str | None] = []
        self.navigated = False

    def initialize_context(self, task_id: str, initial_url: str | None = None, **kwargs: object) -> InitResult:
        self.initial_urls.append(initial_url)
        self.context_names.append(str(kwargs.get("context_name") or ""))
        if initial_url:
            self.navigated = True
        return InitResult(ok=True, target_id="goal-target", context_id="ctx-goal")

    def export_page_tree(self, target_id: str) -> str:
        if self.navigated:
            return '[1] link(link) "Example Domain"\n[2] heading(heading) "Example Domain"'
        return ""

    def capture_screenshot(self, target_id: str) -> bytes:
        return b"\x89PNG-fake-goal"

    def execute_action(
        self,
        intent_id: str,
        target_semantic: str,
        expected_action: str,
        value: str | None = None,
        context_hint: dict[str, str] | None = None,
        timeout_ms: int = 1500,
        node_id: int | None = None,
    ) -> ActionResult:
        return ActionResult(success=True, context_state="STABLE", current_url="")

    def health_check(self) -> HealthStatus:
        return HealthStatus(ok=True)

    def attach_target(self, target_id: str) -> str:
        return "target-new"

    def close_context(self, context_name: str = "") -> None:
        return None


TREES = [
    '[2] button(button) "开始写微博"\n[1] link(link) "微博"',
    '[3] textbox(textbox) "说点什么"\n[9] link(link) "返回"',
    '[4] button(button) "发布"\n[3] textbox(textbox) "hello"',
    '[5] div(text) "发布成功"',
]


def _publish_decisions() -> list[dict]:
    return [
        {"action": "click", "node_id": 2, "reason": "点开输入框"},
        {"action": "input", "node_id": 3, "text": "hello world", "reason": "输入微博正文"},
        {"action": "click", "node_id": 4, "reason": "点击发布"},
        {"action": "done", "text": "微博已发布：hello world", "reason": "发布完成"},
    ]


def test_infer_initial_url_prefers_explicit_then_site_hint() -> None:
    assert infer_initial_url("打开 https://example.com/a 看看") == "https://example.com/a"
    assert infer_initial_url("发布微博：今天天气不错") == "https://weibo.com/compose"
    assert infer_initial_url("登录 twitter 发推：hi") == "https://x.com/compose/post"
    assert infer_initial_url("随便逛逛") == ""


def test_infer_initial_url_search_phrase_and_bare_domain() -> None:
    assert infer_initial_url("google today news") == "https://www.google.com/search?q=today+news"
    assert infer_initial_url("用百度查今天天气") == "https://www.baidu.com/s?wd=" + quote_plus("今天天气")
    assert infer_initial_url("open google.com search today news") == (
        "https://www.google.com/search?q=today+news"
    )
    assert infer_initial_url("打开 example.com/a 看看") == "https://example.com/a"


def test_is_dsl_prompt_only_valid_dsl() -> None:
    assert is_dsl_prompt("click:发布")
    assert is_dsl_prompt("nav:https://weibo.com")
    assert is_dsl_prompt("input:说点什么=hello\nclick:发布")
    assert not is_dsl_prompt("发布微博：今天天气不错")
    assert not is_dsl_prompt("")


def test_parse_decision_tolerates_markdown_and_actions() -> None:
    raw = '```json\n{"action":"input","node_id":3,"text":"hi","reason":"填正文"}\n```'
    decision = parse_decision(raw)
    assert decision is not None
    assert decision["action"] == "input"
    assert decision["node_id"] == 3
    assert decision["text"] == "hi"
    assert parse_decision('{"action":"fly"}') is None
    assert parse_decision("not json at all") is None


def test_goal_success_click_input_done(tmp_path) -> None:
    engine = StageEngine(TREES)
    decisions = iter(_publish_decisions())

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return next(decisions)

    outcome = run_goal(
        "发布微博：hello world",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
    )
    assert outcome.status == "SUCCEEDED"
    assert outcome.reason == "微博已发布：hello world"
    assert engine.executed == [
        ("click", 2, None),
        ("input", 3, "hello world"),
        ("click", 4, None),
    ]
    assert len(outcome.trace) == 4
    assert outcome.url == "https://weibo.com/compose/ok"
    assert outcome.screenshot_path and os.path.exists(outcome.screenshot_path)
    assert engine.initial_url == "https://weibo.com/compose"


def test_goal_guard_pauses_payment_without_execution(tmp_path) -> None:
    engine = StageEngine(['[3] button(button) "立即购买"\n[1] link(link) "首页"'])

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return {"action": "click", "node_id": 3, "reason": "用户要求购买"}

    outcome = run_goal("帮我买这个套餐", engine, vlm_ask=ask, out_dir=str(tmp_path), wait_login=False)
    assert outcome.status == "PAUSED"
    assert "购买" in outcome.reason
    assert "需要确认" in outcome.reason or "confirmation_required" in outcome.reason
    assert engine.executed == []  # 护栏暂停 = 零动作外发


def test_goal_login_wait_returns_waiting_user(tmp_path) -> None:
    engine = StageEngine(['[1] input(textbox) "手机号"\n[2] button(button) "登录"'])

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return {"action": "login", "reason": "页面需要扫码登录"}

    outcome = run_goal(
        "登录账号发微博：hi",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
    )
    assert outcome.status == "WAITING_USER"
    assert "登录" in outcome.reason
    assert engine.executed == []


def test_goal_vlm_impossible_with_login_reason_is_waiting(tmp_path) -> None:
    engine = StageEngine(['[1] div(text) "请先登录"'])

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return {"action": "impossible", "reason": "页面要求登录后才能继续"}

    outcome = run_goal("发微博：hi", engine, vlm_ask=ask, out_dir=str(tmp_path), wait_login=False)
    assert outcome.status == "WAITING_USER"


def test_goal_round_limit_reports_with_page(tmp_path) -> None:
    engine = StageEngine(['[1] button(button) "刷新"'])

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return {"action": "click", "node_id": 1, "reason": "反复刷新"}

    outcome = run_goal("循环目标", engine, vlm_ask=ask, out_dir=str(tmp_path), wait_login=False, max_rounds=3)
    assert outcome.status == "FAILED"
    assert "轮" in outcome.reason or "无进展" in outcome.reason
    # 同节点重复会先消耗一次“带提示换决策”的征询再熔断，允许 max_rounds+1 的处理轮。
    assert outcome.rounds <= 4
    assert len(engine.executed) <= 2  # 熔断前只执行过首两次点击，不烧满轮数


def test_goal_empty_page_vlm_goto_then_done(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr("coordinator.goal.time.sleep", lambda _s: None)
    engine = EmptyThenNavEngine()
    decisions = iter(
        [
            {"action": "goto", "text": "https://example.com", "reason": "空白页，先打开示例站"},
            {"action": "done", "text": "已打开 example.com", "reason": "页面加载完成"},
        ]
    )

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return next(decisions)

    outcome = run_goal(
        "随便看看",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
    )
    assert outcome.status == "SUCCEEDED"
    assert outcome.reason == "已打开 example.com"
    assert outcome.url == "https://example.com"
    assert engine.initial_urls == ["", "https://example.com"]
    assert outcome.screenshot_path and os.path.exists(outcome.screenshot_path)


def test_goal_empty_page_vlm_impossible_surfaces_reason(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr("coordinator.goal.time.sleep", lambda _s: None)
    engine = StageEngine([""])

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return {"action": "impossible", "reason": "页面空白，无法确定要打开的网站"}

    outcome = run_goal(
        "随便逛逛",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
        max_rounds=5,
    )
    assert outcome.status == "FAILED"
    assert "页面空白" in outcome.reason
    assert "多次快照仍无内容" not in outcome.reason


def test_goal_session_id_threads_through_init_and_actions(tmp_path) -> None:
    engine = StageEngine(TREES)

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return {"action": "done", "text": "页面已确认", "reason": "目标达成"}

    outcome = run_goal(
        "随便看看",
        engine,
        vlm_ask=ask,
        session_id="esa-wizard",
        out_dir=str(tmp_path),
        wait_login=False,
    )
    assert outcome.status == "SUCCEEDED"
    assert engine.context_names == ["esa-wizard"]
    assert all(h.get("context") == "esa-wizard" for h in engine.context_hints)


def test_goal_empty_page_goto_keeps_session(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr("coordinator.goal.time.sleep", lambda _s: None)
    engine = EmptyThenNavEngine()
    decisions = iter(
        [
            {"action": "goto", "text": "https://example.com", "reason": "先打开目标站"},
            {"action": "done", "text": "已打开", "reason": "完成"},
        ]
    )

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return next(decisions)

    outcome = run_goal(
        "随便看看",
        engine,
        vlm_ask=ask,
        session_id="esa-wizard",
        out_dir=str(tmp_path),
        wait_login=False,
    )
    assert outcome.status == "SUCCEEDED"
    assert engine.context_names == ["esa-wizard", "esa-wizard"]


def test_goal_done_with_empty_page_blurb_retries_snapshot(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr("coordinator.goal.time.sleep", lambda _s: None)
    engine = StageEngine(['[1] link(link) "结果标题"'])
    calls = {"n": 0}

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        calls["n"] += 1
        if calls["n"] == 1:
            return {
                "action": "done",
                "text": "页面内容为空，无法获取结果",
                "reason": "可能仍在加载",
            }
        return {"action": "done", "text": "已看到结果标题：结果标题", "reason": "页面已渲染"}

    outcome = run_goal(
        "google today news",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
    )
    assert outcome.status == "SUCCEEDED"
    assert outcome.reason == "已看到结果标题：结果标题"
    assert any("等待一帧重拍" in t for t in outcome.trace)


def test_parse_tree_nodes_ignores_href_suffix(tmp_path) -> None:
    tree = (
        '[1] a(link) "Scaling Up with Reducer and Context" '
        'href="https://react.dev/learn/scaling-up-with-reducer-and-context"\n'
        '[2] a(link) "Scaling Up with Reducer and Context" '
        'href="https://react.dev/learn/managing-state#scaling-up-with-reducer-and-context"'
    )
    nodes = parse_tree_nodes(tree)
    assert nodes == {
        1: "Scaling Up with Reducer and Context",
        2: "Scaling Up with Reducer and Context",
    }


def test_goal_repeat_same_node_hint_switches(tmp_path) -> None:
    engine = StageEngine(
        ['[1] link(link) "A"\n[2] link(link) "B"'],
        base_url="https://react.dev/learn/scaling-up-with-reducer-and-context",
    )
    calls: list[dict] = []

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        calls.append({"hint": kwargs.get("hint"), "target_id": kwargs.get("target_id")})
        if kwargs.get("hint"):
            return {"action": "click", "node_id": 2, "reason": "换选另一个同名候选"}
        if len(calls) <= 2:
            return {"action": "click", "node_id": 1, "reason": "点第一个"}
        return {"action": "done", "text": "完成", "reason": "目标达成"}

    outcome = run_goal("测试目标", engine, vlm_ask=ask, out_dir=str(tmp_path), wait_login=False)
    assert outcome.status == "SUCCEEDED"
    assert [a[0:2] for a in engine.executed] == [("click", 1), ("click", 2)]
    assert any(c["hint"] for c in calls), "第二次重复时应收到达提示的换决策征询"
    assert all(c["target_id"] == "goal-target" for c in calls)


def test_goal_repeat_same_node_stops_with_diagnosis(tmp_path) -> None:
    tree = (
        '[1] a(link) "Scaling Up with Reducer and Context" '
        'href="https://react.dev/learn/scaling-up-with-reducer-and-context"\n'
        '[2] a(link) "Scaling Up with Reducer and Context" '
        'href="https://react.dev/learn/managing-state#scaling-up-with-reducer-and-context"'
    )
    engine = StageEngine([tree], base_url="https://react.dev/learn/scaling-up-with-reducer-and-context")

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return {"action": "click", "node_id": 1, "reason": "想点官方章节"}

    outcome = run_goal(
        "查看官方状态管理方案",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
        max_rounds=6,
    )
    assert outcome.status == "FAILED"
    assert "无进展" in outcome.reason
    assert "href" in outcome.reason
    assert "node_id=1" in outcome.reason
    # 空转必须提前熔断，不能烧到 6 轮上限。
    assert outcome.rounds < 6
    assert engine.executed == [("click", 1, None)]


def test_goal_stagnant_different_nodes_stops_with_goto_diagnosis(tmp_path) -> None:
    """换着点同名节点、URL/页面不变：给一次提示，纠错点击后仍不变则熔断。"""
    tree = (
        '[1] a(link) "Scaling Up with Reducer and Context" href="https://react.dev/learn/scaling-up-with-reducer-and-context"\n'
        '[2] a(link) "Scaling Up with Reducer and Context" href="https://react.dev/learn/managing-state#scaling-up-with-reducer-and-context"\n'
        '[3] a(link) "Scaling Up with Reducer and Context" href="https://react.dev/learn/scaling-up-with-reducer-and-context#overview"'
    )
    engine = StageEngine([tree], base_url="https://react.dev/learn/managing-state")
    calls: list[dict] = []

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        calls.append({"hint": kwargs.get("hint")})
        if kwargs.get("hint"):
            return {"action": "click", "node_id": 1, "reason": "换 href 指向文章页的候选点一次"}
        n = len(calls)
        if n <= 3:
            return {"action": "click", "node_id": n, "reason": f"点第 {n} 个同名候选"}
        return {"action": "click", "node_id": 3, "reason": "再点一次试试"}

    outcome = run_goal(
        "查看官方状态管理方案",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
        max_rounds=8,
    )
    assert outcome.status == "FAILED"
    assert "goto" in outcome.reason
    assert "无进展" in outcome.reason or "未变化" in outcome.reason
    assert any("改用 goto" in t or "goto" in t for t in outcome.trace)
    assert any(c["hint"] for c in calls), "页面停滞时应收到达提示的换决策征询"
    # 最多 3 次物理点击（1→2→纠错点 1），第四轮点击被熔断不执行。
    assert len(engine.executed) == 3
    assert outcome.rounds < 8


def test_render_report_carries_target_id() -> None:
    outcome = GoalOutcome(
        status="SUCCEEDED",
        reason="已总结官方推荐",
        url="https://react.dev/learn/scaling-up-with-reducer-and-context",
        tree='[1] link(link) "A"',
        target_id="real-cdp-target-1",
    )
    report = _render_report(outcome, "了解 react 状态管理", session_id="react-docs")
    assert "Target ID: `real-cdp-target-1`" in report
    assert "Session ID: `react-docs`" in report


def test_goal_debug_flag_logs_intermediate_rounds(capsys, tmp_path) -> None:
    """--debug：每轮快照（URL/树规模/截图路径）、VLM 决策实时打到 stderr。"""
    engine = StageEngine(['[1] link(link) "结果标题"'])

    def ask(goal, url, tree, png, **kwargs):  # noqa: ANN001
        return {"action": "done", "text": "已找到 2L 600W 电煮锅", "reason": "结果页已渲染"}

    outcome = run_goal(
        "买个 2L 600W 电煮锅",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
        debug=True,
    )
    assert outcome.status == "SUCCEEDED"
    err = capsys.readouterr().err
    assert "[debug]" in err
    assert "快照" in err
    assert "VLM 决策" in err
    assert "截图=" in err
    assert any(p.name.startswith("goal-r1-") for p in tmp_path.iterdir())


def test_default_vlm_ask_debug_exposes_raw_and_usage(capsys, monkeypatch) -> None:
    """--debug 时 VLM 原始 content/usage 打印 stderr 并随决策返回（供上层记录）。"""

    def fake_send_request(payload, endpoint, api_key, timeout_s=60.0):
        return {
            "choices": [
                {"message": {"content": '{"action":"done","text":"ok","reason":"done"}'}}
            ],
            "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
        }

    monkeypatch.setenv("UWS_VLM_API_KEY", "debug-key")
    monkeypatch.setattr("coordinator.goal.send_request", fake_send_request)
    decision = default_vlm_ask(
        "目标",
        "https://example.com",
        '[1] link(link) "A"',
        b"\x89PNG-fake",
        debug=True,
    )
    assert decision is not None
    assert decision["action"] == "done"
    assert decision["_raw"] == '{"action":"done","text":"ok","reason":"done"}'
    assert decision["_usage"]["total_tokens"] == 15
    err = capsys.readouterr().err
    assert "原始 content" in err


class DynamicSearchEngine:
    """京东首页动态引擎：每轮 ref 行内容漂移（node_id 变化），语义稳定。"""

    def __init__(self, trees: list[str], base_url: str = "https://www.jd.com") -> None:
        self.trees = list(trees)
        self.base_url = base_url
        self.executed: list[tuple[str, int, str | None]] = []
        self._idx = 0

    def initialize_context(self, task_id: str, initial_url: str | None = None, **kwargs: object) -> InitResult:
        return InitResult(ok=True, target_id="jd-target", context_id="ctx-jd", current_url=self.base_url)

    def export_page_tree(self, target_id: str) -> str:
        return self.trees[min(self._idx, len(self.trees) - 1)]

    def capture_screenshot(self, target_id: str) -> bytes:
        return b"\x89PNG-fake-jd"

    def execute_action(
        self,
        intent_id: str,
        target_semantic: str,
        expected_action: str,
        value: str | None = None,
        context_hint: dict[str, str] | None = None,
        timeout_ms: int = 1500,
        node_id: int | None = None,
    ) -> ActionResult:
        self.executed.append((expected_action, node_id or 0, value))
        if self._idx + 1 < len(self.trees):
            self._idx += 1
        return ActionResult(success=True, context_state="STABLE", current_url=self.base_url)

    def health_check(self) -> HealthStatus:
        return HealthStatus(ok=True)

    def attach_target(self, target_id: str) -> str:
        return "target-new"

    def close_context(self, context_name: str = "") -> None:
        return None


def _jd_tree(search_node_id: int, submit_node_id: int, noise_tag: str) -> str:
    content = (
        '# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）\n'
        '- heading "京东秒杀"\n'
        f'- text "{noise_tag} 动态推荐内容"\n'
    )
    ref = (
        '# 可交互节点\n'
        f'[{search_node_id}] textbox(textbox) "搜索框"\n'
        f'[{submit_node_id}] button(button) "搜索"\n'
        f'[3] link(link) "猜你喜欢 {noise_tag}" href="https://www.jd.com/hot"'
    )
    return content + "\n\n" + ref


def test_goal_repeated_input_on_dynamic_home_auto_clicks_submit(tmp_path) -> None:
    """京东复现：动态首页 ref 树每轮变化 + VLM 重复 input——自动点搜索按钮，不再空转。"""
    engine = DynamicSearchEngine(
        [
            _jd_tree(1, 7, "A"),
            _jd_tree(9, 10, "B"),
        ]
    )
    seen_trees: list[str] = []
    asks = {"n": 0}

    def ask(goal, url, tree_text, png, **kwargs):  # noqa: ANN001
        asks["n"] += 1
        seen_trees.append(tree_text)
        nodes = parse_tree_nodes(tree_text)
        box = next(nid for nid, text in nodes.items() if text == "搜索框")
        if asks["n"] <= 2:
            return {"action": "input", "node_id": box, "text": "电煮锅 1-2L 600W", "reason": "在搜索框输入目标"}
        return {"action": "done", "text": "已在京东搜索电煮锅", "reason": "提交后由用户继续挑选"}

    outcome = run_goal(
        "帮我看看京东电煮锅 1-2L 600W",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
        max_rounds=6,
    )
    assert outcome.status == "SUCCEEDED"
    assert asks["n"] == 3
    assert engine.executed[0] == ("input", 1, "电煮锅 1-2L 600W")
    assert engine.executed[1] == ("click", 10, None), "第二轮应自动点击搜索按钮而不是再次 input"
    assert len(engine.executed) == 2
    assert any("自动提交" in t or "自动点击" in t for t in outcome.trace)
    # 两轮 VLM 看到的树必须不同（证明重复判据不依赖全树稳定）。
    assert seen_trees[0] != seen_trees[1]


def test_goal_repeated_input_without_submit_stops_with_diagnosis(tmp_path) -> None:
    """无提交候选时：给一次换决策提示，VLM 仍重复 input → 直接熔断给出诊断。"""
    tree = (
        '# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）\n'
        '- text "页面内容"\n\n'
        '# 可交互节点\n'
        '[2] textbox(textbox) "搜索框"'
    )
    engine = DynamicSearchEngine([tree])

    def ask(goal, url, tree_text, png, **kwargs):  # noqa: ANN001
        nodes = parse_tree_nodes(tree_text)
        box = next(nid for nid, text in nodes.items() if text == "搜索框")
        return {"action": "input", "node_id": box, "text": "电煮锅 1-2L 600W", "reason": "重复输入尝试提交"}

    outcome = run_goal(
        "买个电煮锅",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
        max_rounds=6,
    )
    assert outcome.status == "FAILED"
    assert "input" in outcome.reason or "输入" in outcome.reason
    assert "无进展" in outcome.reason or "重复" in outcome.reason
    assert engine.executed == [("input", 2, "电煮锅 1-2L 600W")], "重复 input 绝不二次执行"
    assert any("换决策" in t or "重复" in t for t in outcome.trace)


def test_goal_ask_receives_compacted_snapshot(tmp_path) -> None:
    """C：VLM 每轮收到预算内的压缩快照，不再全量塞 93KB 树。"""
    content = ['# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）']
    content.append('- heading "结果标题"')
    for i in range(400):
        content.append(f'- text "长正文第 {i} 段：" + "x" * 120')
    ref = (
        '# 可交互节点\n'
        '[1] link(link) "结果1" href="https://search.jd.com/Search?keyword=锅"\n'
        '[2] button(button) "下一页"'
    )
    engine = DynamicSearchEngine(["\n".join(content) + "\n\n" + ref])
    seen: dict[str, object] = {}

    def ask(goal, url, tree_text, png, **kwargs):  # noqa: ANN001
        seen["tree"] = tree_text
        return {"action": "done", "text": "看到结果标题", "reason": "目标达成"}

    outcome = run_goal(
        "看京东结果",
        engine,
        vlm_ask=ask,
        out_dir=str(tmp_path),
        wait_login=False,
    )
    assert outcome.status == "SUCCEEDED"
    shown = str(seen["tree"])
    assert len(shown) <= VLM_TREE_MAX_CHARS
    assert '[1] link(link) "结果1"' in shown
    assert "已压缩" in shown
