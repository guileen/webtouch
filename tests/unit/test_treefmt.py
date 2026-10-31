"""stdout 树摘要（coordinator/treefmt.py）单元测试。

2026-09-03 用户复盘：大页面（535 行）报告只给前 4000 字符，ref 交互行
根本没进 stdout，Agent 还得再调一次 probe。这里锁定「ref 交互块优先完整
保留、语义内容块给头部」的压缩策略。
"""

from __future__ import annotations

from coordinator.treefmt import (
    VLM_TREE_MAX_CHARS,
    parse_ref_hrefs,
    parse_ref_rows,
    split_snapshot,
    summarize_tree,
    trim_snapshot_for_vlm,
)


def _two_block_tree() -> str:
    content = "# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）\n- heading \"标题\""
    ref = (
        "# 可交互节点\n"
        '[1] a(link) "Scaling Up with Reducer and Context" href="https://react.dev/learn/scaling-up-with-reducer-and-context"\n'
        '[2] a(link) "Scaling Up with Reducer and Context" href="https://react.dev/learn/managing-state#scaling-up-with-reducer-and-context"'
    )
    return content + "\n\n" + ref


def test_split_snapshot_blocks() -> None:
    tree = _two_block_tree()
    content, ref = split_snapshot(tree)
    assert content.startswith("# 页面语义")
    assert ref.startswith("# 可交互节点")
    assert "href=" in ref


def test_summarize_tree_small_full() -> None:
    tree = _two_block_tree()
    shown, note = summarize_tree(tree, budget=100000)
    assert shown == tree.strip()
    assert note == ""


def test_summarize_tree_keeps_ref_block_under_budget() -> None:
    tree = _two_block_tree()
    content = "# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）\n" + "x" * 9000
    ref = "# 可交互节点\n[9] button(button) \"发布\""
    shown, note = summarize_tree(content + "\n\n" + ref, budget=8000)
    assert "[9] button(button) \"发布\"" in shown, "交互 ref 行必须完整保留"
    assert "stdout 已压缩" in note
    assert "…（语义树过长" in shown


def test_parse_ref_hrefs() -> None:
    rows = parse_ref_hrefs(_two_block_tree())
    assert rows == [
        (1, "Scaling Up with Reducer and Context", "https://react.dev/learn/scaling-up-with-reducer-and-context"),
        (2, "Scaling Up with Reducer and Context", "https://react.dev/learn/managing-state#scaling-up-with-reducer-and-context"),
    ]


def test_parse_ref_rows_extracts_role_and_href() -> None:
    tree = (
        '# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）\n'
        '- heading "标题"\n\n'
        '# 可交互节点\n'
        '[1] a(link) "Scaling Up with Reducer and Context" href="https://react.dev/learn/scaling-up-with-reducer-and-context"\n'
        '[9] button(button) "搜索"'
    )
    rows = parse_ref_rows(tree)
    assert [(r.node_id, r.tag, r.role, r.text, r.href) for r in rows] == [
        (1, "a", "link", "Scaling Up with Reducer and Context",
         "https://react.dev/learn/scaling-up-with-reducer-and-context"),
        (9, "button", "button", "搜索", ""),
    ]


def test_trim_snapshot_keeps_ref_and_headings_drops_noise() -> None:
    """VLM 快照降面：交互 ref 完整、标题/链接保留、正文噪声按预算裁剪。"""
    content_lines = ['# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）']
    content_lines.append('- heading "电煮锅 1-2L 600W"')
    for i in range(600):
        content_lines.append(
            f'- text "商品噪声描述第 {i} 条：这是一段很长的动态推荐内容，'
            "用来把 VLM 快照撑到预算之上以验证正文裁剪是否生效"
        )
    ref_lines = [
        '# 可交互节点',
        '[1] textbox(textbox) "搜索框"',
        '[2] button(button) "搜索"',
        '[3] link(link) "电煮锅 1-2L 600W" href="https://search.jd.com/Search?keyword=锅"',
    ]
    full = "\n".join(content_lines) + "\n\n" + "\n".join(ref_lines)
    assert len(full) > VLM_TREE_MAX_CHARS

    shown = trim_snapshot_for_vlm(full)
    assert len(shown) <= VLM_TREE_MAX_CHARS
    assert '[1] textbox(textbox) "搜索框"' in shown
    assert '[2] button(button) "搜索"' in shown
    assert 'href="https://search.jd.com/Search?keyword=锅"' in shown
    assert '电煮锅 1-2L 600W' in shown  # heading 优先保留
    assert "商品噪声描述第 290 条" not in shown  # 低优先正文被裁剪
    assert "已压缩" in shown


def test_trim_snapshot_ref_only_caps_rows() -> None:
    """旧格式（纯 [ref] 行、无两段分界）按行数预算截断且给出说明。"""
    rows = [f'[{i}] button(button) "候选{i}"' for i in range(1, 400)]
    full = "\n".join(rows)
    shown = trim_snapshot_for_vlm(full)
    assert len(shown) <= VLM_TREE_MAX_CHARS
    assert '[1] button(button) "候选1"' in shown
    assert '[399] button(button) "候选399"' not in shown
    assert "已压缩" in shown
