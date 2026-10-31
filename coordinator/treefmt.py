"""两段式页面快照的格式化/摘要共用（2026-09-03 用户复盘定案）。

目标模式与 run/probe 的报告都消费同一份树文本：
  # 页面语义（AX 内容树…）
  …
  # 可交互节点（[ref] 行…）
  [1] a(link) "文本" href="https://…"

stdout 优先原则下，报告不能把整棵 500+ 行树截成“只有开头”，
否则 Agent 看不到交互 ref 行、还得再调一次 uws probe（两次工具调用）。
这里按块截断：交互 ref 块优先完整保留，语义内容块给头部摘要。
"""

from __future__ import annotations

import re
from dataclasses import dataclass

AX_HEADER = "# 页面语义（AX 内容树，供阅读；不带 ref 的行不可交互）"
REF_HEADER = "# 可交互节点"

# VLM 快照预算（2026-09-03 京东首页实测：93KB 全树 ≈45k~60k token/轮）。
# 交互 ref 行是决策入口，完整优先；正文只保留标题/链接/低优先摘要。
VLM_TREE_MAX_CHARS = 20_000
VLM_REF_MAX_ROWS = 260
_VLM_CONTENT_RESERVE = 4_000  # 至少留给正文摘要的字符数

_REF_ROW_RE = re.compile(r"^\s*\[(\d+)\](.*)$")
_ROLE_TOKEN_RE = re.compile(r"^\s*([a-zA-Z][\w-]*)\s*\(([^)]*)\)")
_QUOTED_RE = re.compile(r'"((?:[^"\\]|\\.)*)"')
_HREF_RE = re.compile(r'href="([^"]*)"')
_CONTENT_ROLE_RE = re.compile(r"^\s*-\s*([a-zA-Z][a-zA-Z0-9-]*)\b")

# 正文摘要保留规则：rank 越小越优先；行数上限按角色分开，防止单个角色霸占预算。
_CONTENT_RULES = {
    "heading": (0, 120),
    "title": (0, 40),
    "alert": (1, 40),
    "status": (1, 40),
    "dialog": (1, 30),
    "link": (2, 140),
    "listitem": (3, 90),
    "cell": (3, 90),
    "article": (4, 40),
    "region": (4, 30),
    "main": (4, 20),
    "text": (5, 100),
    "paragraph": (5, 60),
    "statictext": (5, 100),
}
_CONTENT_LINE_CAP = 200


@dataclass
class RefRow:
    """可交互 ref 行解析结果（node_id 为探针 BFS 编号，仅当前快照有效）。"""

    node_id: int
    tag: str = ""
    role: str = ""
    text: str = ""
    href: str = ""


def split_snapshot(tree_text: str) -> tuple[str, str]:
    """树文本 → (语义内容块, 交互 ref 块含块头)；找不到分界时整段视为内容。"""
    text = tree_text or ""
    idx = text.find(REF_HEADER)
    if idx < 0:
        return text, ""
    return text[:idx].rstrip(), text[idx:]


def summarize_tree(tree_text: str, budget: int = 8000) -> tuple[str, str]:
    """stdout 友好摘要：(显示文本, 截断说明)。ref 交互块优先完整保留。"""
    tree = (tree_text or "").strip()
    if not tree:
        return "", ""
    if len(tree) <= budget:
        return tree, ""
    content, ref = split_snapshot(tree)
    ref_shown = ref
    ref_note = ""
    ref_keep = max(budget // 3, 2400)
    if len(ref) > ref_keep:
        ref_shown = ref[:ref_keep] + "\n…（ref 行过长已截断）"
        ref_note = "ref 行超长截断"
    content_budget = budget - len(ref_shown)
    content_budget = max(content_budget, 1200)
    if len(content) <= content_budget:
        content_shown = content
    else:
        content_shown = content[:content_budget] + "\n…（语义树过长，仅保留头部；链接行已带 href）"
    parts = [content_shown]
    if ref_shown:
        parts.append(ref_shown)
    notes = []
    if ref_note:
        notes.append(ref_note)
    notes.append(
        f"总 {len(tree.splitlines())} 行（语义 {len(content.splitlines())} / 交互 {len(ref.splitlines())}）"
        f"，共 {len(tree)} 字符，stdout 已压缩；全文可用 `uws probe --keep-open`"
    )
    return "\n".join(parts), "；".join(notes)


def parse_ref_hrefs(tree_text: str) -> list[tuple[int, str, str]]:
    """解析 ref 行 → [(node_id, 可见文本, href)]（无 href 为空串）。"""
    out: list[tuple[int, str, str]] = []
    for raw in (tree_text or "").splitlines():
        m = _REF_ROW_RE.match(raw)
        if not m:
            continue
        node_id = int(m.group(1))
        rest = m.group(2)
        tm = _QUOTED_RE.search(rest)
        text = tm.group(1) if tm else ""
        hm = _HREF_RE.search(rest)
        href = hm.group(1) if hm else ""
        out.append((node_id, text, href))
    return out


def parse_ref_rows(tree_text: str) -> list[RefRow]:
    """解析全部 ref 行（含 tag/role/text/href），供提交候选与稳定签名使用。"""
    rows: list[RefRow] = []
    for raw in (tree_text or "").splitlines():
        m = _REF_ROW_RE.match(raw)
        if not m:
            continue
        rest = m.group(2)
        rm = _ROLE_TOKEN_RE.match(rest)
        tag = rm.group(1) if rm else ""
        role = rm.group(2) if rm else ""
        tm = _QUOTED_RE.search(rest)
        text = tm.group(1) if tm else ""
        hm = _HREF_RE.search(rest)
        href = hm.group(1) if hm else ""
        rows.append(RefRow(node_id=int(m.group(1)), tag=tag, role=role, text=text, href=href))
    return rows


def _content_role(line: str) -> str:
    m = _CONTENT_ROLE_RE.match(line or "")
    return m.group(1).lower() if m else ""


def _digest_content(content: str, budget: int) -> tuple[str, int, int]:
    """正文块 → 按角色优先级挑选的摘要（保留原始行序），返回 (文本, 保留行数, 总语义行数)。"""
    if not content.strip():
        return "", 0, 0
    lines = content.splitlines()
    header = [line for line in lines if line.startswith("#")]
    semantic: list[tuple[str, str]] = []
    for line in lines:
        role = _content_role(line)
        if role in _CONTENT_RULES:
            semantic.append((role, line))
    if not semantic and not header:
        # 无法识别角色（异常树）：按字符硬截断保底。
        cut = content[:budget]
        return cut + ("\n…（正文已压缩）" if len(content) > budget else ""), 0, 0

    chosen: list[str] = list(header)
    used = sum(len(x) for x in chosen)
    per_role: dict[str, int] = {}
    for rank in range(6):
        for role, line in semantic:
            rule = _CONTENT_RULES[role]
            if rule[0] != rank:
                continue
            kept = per_role.get(role, 0)
            if kept >= rule[1]:
                continue
            shown = line if len(line) <= _CONTENT_LINE_CAP else line[:_CONTENT_LINE_CAP] + "…"
            if used + len(shown) > budget:
                continue
            chosen.append(shown)
            per_role[role] = kept + 1
            used += len(shown)
    return "\n".join(chosen), sum(per_role.values()), len(semantic)


def _limit_ref_block(ref: str, max_rows: int, max_chars: int) -> tuple[str, bool]:
    """ref 块截断：保留表头 + 前 max_rows 行，并受 max_chars 硬约束。返回 (文本, 是否截断)。"""
    text = (ref or "").strip()
    if not text:
        return "", False
    lines = text.splitlines()
    header: list[str] = []
    body: list[str] = []
    for line in lines:
        if _REF_ROW_RE.match(line):
            body.append(line)
        else:
            header.append(line)
    if len(body) <= max_rows and len(text) <= max_chars:
        return text, False
    keep = body[:max_rows]
    # 行本身可能超长（异常页）：收缩到 max_chars 内，至少保留 8 行。
    while keep and len("\n".join(header + keep)) > max_chars and len(keep) > 8:
        keep = keep[: len(keep) * 2 // 3]
    out = header + keep
    out.append(
        f"…（ref 行 {len(body)}→{len(keep)} 已压缩；node_id 只对上面展示的行有效）"
    )
    return "\n".join(out), True


def trim_snapshot_for_vlm(
    tree_text: str,
    *,
    max_chars: int = VLM_TREE_MAX_CHARS,
    max_ref_rows: int = VLM_REF_MAX_ROWS,
) -> str:
    """VLM 快照降面：ref 交互行完整优先 + 正文摘要，总长 ≤ max_chars。

    动态页（京东 1430~1757 行/93KB）不能全量进每轮 prompt：
    决策需要的是「可点/可填入口 + 标题/链接/正文摘要」，而不是整棵正文树。
    """
    tree = (tree_text or "").strip()
    if not tree:
        return ""
    content, ref = split_snapshot(tree)
    if not ref and parse_ref_rows(tree):
        # 旧格式/纯 ref 树：没有两段分界，整体当交互块处理。
        ref, content = tree, ""

    if not content:
        shown, _cut = _limit_ref_block(ref, max_rows=max_ref_rows, max_chars=max_chars)
        if len(shown) > max_chars:
            shown = shown[: max_chars - 120] + "\n…（ref 超长已压缩）"
        return shown

    ref_shown, _ref_cut = _limit_ref_block(
        ref, max_rows=max_ref_rows, max_chars=max_chars - _VLM_CONTENT_RESERVE
    )
    content_budget = max(max_chars - len(ref_shown) - 220, 800)
    content_shown, kept, total = _digest_content(content, content_budget)
    if content_shown and total > kept:
        content_shown += f"\n…（正文已压缩：保留 {kept}/{total} 语义行；标题/链接优先）"

    parts = [p for p in (content_shown, ref_shown) if p.strip()]
    out = "\n\n".join(parts)
    if len(out) <= max_chars:
        return out
    # 兜底：正文摘要按剩余预算重算（ref 行优先级最高）。
    content_budget2 = max(max_chars - len(ref_shown) - 220, 400)
    content_shown2, kept2, total2 = _digest_content(content, content_budget2)
    if content_shown2 and total2 > kept2:
        content_shown2 += f"\n…（正文已压缩：保留 {kept2}/{total2} 语义行；标题/链接优先）"
    parts = [p for p in (content_shown2, ref_shown) if p.strip()]
    out2 = "\n\n".join(parts)
    if len(out2) <= max_chars:
        return out2
    return out2[: max_chars - 120] + "\n…（VLM 快照已压缩到预算上限）"
