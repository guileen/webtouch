"""Tier 2 时间系综采样纯逻辑单测（离线，无 Chrome/CDP）。

验证参考特征构建、结构漂移检测、reference 快照生成与 ComponentGene 装载，
复用与 Go 逐位对齐的 structural_simhash64。
"""

from __future__ import annotations

import json

import pytest

from benchmarks.tier2.sandbox import _sghash
from benchmarks.tier2.temporal_ensemble import _structural_hash, build_reference
from pipeline.component_gene import ComponentGene, structural_simhash64, ComponentFeature, ComponentAncestor


def _feat(anc_role_seq: list[tuple[str, str]]) -> dict:
    """构造一个 button 结构特征，祖先链为给定 [(tag, role), ...]。"""
    return {
        "tag": "button",
        "role": "",
        "attrs": [],
        "ancestors": [{"tag": t, "role": r} for t, r in anc_role_seq],
    }


def test_structural_hash_matches_component_gene() -> None:
    feat = _feat([("div", ""), ("nav", "navigation"), ("body", "")])
    want = structural_simhash64(ComponentFeature(
        tag="button", role="", attrs=[], ancestors=[ComponentAncestor("div", ""), ComponentAncestor("nav", "navigation"), ComponentAncestor("body", "")],
    ))
    assert _structural_hash(feat) == format(want, "016x")


def test_wrap_ghost_is_invariant() -> None:
    """无语义幽灵包装（role 空）不应改变结构哈希。"""
    bare = _feat([("div", ""), ("nav", "navigation")])
    wrapped = _feat([("div", ""), ("div", ""), ("nav", "navigation")])
    assert _structural_hash(bare) == _structural_hash(wrapped)


def test_semantic_ancestor_role_drift_changes_hash() -> None:
    """最近语义祖先 role 漂移（navigation → groups）应改变哈希（Hydration 瞬态）。"""
    a = _feat([("div", ""), ("nav", "navigation")])
    b = _feat([("div", ""), ("div", "groups"), ("nav", "navigation")])
    assert _structural_hash(a) != _structural_hash(b)


def test_build_reference_only_keeps_invariant(tmp_path) -> None:
    records = [
        {"semantic": "登录", "invariant": True, "struct_t0": {"tag": "button", "role": "", "attrs": [], "ancestors": [{"tag": "nav", "role": "navigation"}]}},
        {"semantic": "登录", "invariant": False, "struct_t0": {}},
        {"semantic": "立即购买", "invariant": True, "struct_t0": {"tag": "button", "role": "", "attrs": [], "ancestors": [{"tag": "section", "role": "region"}]}},
    ]
    ref = build_reference(records)
    # 同语义去重：只保留第一个 invariant。
    assert [r["semantic"] for r in ref] == ["登录", "立即购买"]

    p = tmp_path / "reference.json"
    p.write_text(json.dumps(ref, ensure_ascii=False), encoding="utf-8")
    gene = ComponentGene().load_reference(p)
    assert len(gene) == 2
    # 参考哈希应为非 0，且与 sghash 一致。
    login_struct = next(r["struct"] for r in ref if r["semantic"] == "登录")
    cf = ComponentFeature(
        tag=login_struct["tag"], role=login_struct["role"], attrs=[],
        ancestors=[ComponentAncestor(a["tag"], a["role"]) for a in login_struct["ancestors"]],
    )
    assert gene._by_semantic["登录"] == structural_simhash64(cf)
    assert gene._by_semantic["登录"] != 0


def test_drift_detail_flagged() -> None:
    feat_a = _feat([("nav", "navigation")])
    feat_b = _feat([("div", "groups"), ("nav", "navigation")])
    ha, hb = _structural_hash(feat_a), _structural_hash(feat_b)
    assert ha != hb
