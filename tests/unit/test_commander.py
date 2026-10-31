"""uws commander（混合指挥）解析契约单测（TDD-CLI-15，tdd-checklist.md §8）。"""

import json

from coordinator.cli import create_parser
from coordinator.commander import (
    build_request,
    parse_commander_reply,
    render_dsl,
)

TREE = '[12] button(button) "立即购买"\n[31] input(textbox) "验证码"\n'


def test_cli_parser_exposes_commander_subcommand():
    args = create_parser().parse_args(
        ["commander", "--tree-file", "x.txt", "--mock", "--goal", "点结算"]
    )
    assert args.command == "commander"
    assert args.tree_file == "x.txt"
    assert args.mock and not args.dry_run


def test_parse_single_action():
    reply = json.dumps({"action": "click", "node_id": 12, "reason": "结算"})
    assert parse_commander_reply(reply) == [
        {"action": "click", "node_id": 12, "reason": "结算"}
    ]


def test_parse_actions_list_and_order():
    reply = json.dumps(
        {"actions": [
            {"node_id": 31, "action": "extract", "reason": "验证码"},
            {"node_id": 12, "action": "click", "reason": "结算"},
            {"action": "done", "reason": "完成"},
        ]}
    )
    assert parse_commander_reply(reply) == [
        {"action": "extract", "node_id": 31, "reason": "验证码"},
        {"action": "click", "node_id": 12, "reason": "结算"},
        {"action": "done", "reason": "完成"},
    ]


def test_parse_markdown_fence_tolerated():
    reply = '```json\n{"commands":[{"node_id":52,"action":"click"}]}\n```'
    assert parse_commander_reply(reply) == [
        {"action": "click", "node_id": 52, "reason": ""}
    ]


def test_parse_malformed_or_unknown_action_returns_empty():
    assert parse_commander_reply("not json at all") == []
    assert parse_commander_reply(json.dumps({"action": "drag", "node_id": 12})) == []
    assert parse_commander_reply(json.dumps({"actions": [{"node_id": "abc", "action": "click"}]})) == []


def test_render_dsl_emits_node_refs_and_skips_invalid():
    dsl, report = render_dsl(
        [
            {"action": "click", "node_id": 12, "reason": "结算"},
            {"action": "extract", "node_id": 31, "reason": "验证码"},
            {"action": "click", "node_id": 999, "reason": "不存在"},
            {"action": "impossible", "reason": "看不清"},
        ],
        TREE,
    )
    assert dsl == "click:node:12\nextract:node:31"
    assert any("999 不在极简 AXTree" in line for line in report)
    assert any("impossible" in line for line in report)


def test_build_request_includes_tree_goal_and_thinking():
    payload = build_request(TREE, goal="点结算", model="m-test", thinking_disabled=True)
    assert payload["model"] == "m-test"
    assert payload["thinking"] == {"type": "disabled"}
    user = payload["messages"][1]["content"]
    assert "点结算" in user
    assert "[12] button(button)" in user


def test_build_request_omits_thinking_when_disabled():
    payload = build_request(TREE, goal="x", thinking_disabled=False)
    assert "thinking" not in payload
