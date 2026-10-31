"""Tier 1.5 component_gene 跨语言对拍 + 规则级 StructHash 落地测试。"""

from __future__ import annotations

import sqlite3

from pipeline.component_gene import (
    ComponentAncestor,
    ComponentFeature,
    ComponentGene,
    feature_from_node,
    structural_phrases,
    structural_simhash64,
)
from pipeline.exporter.exporter import Exporter
from pipeline.llm_distiller.distiller import DistilledRule


# Go 侧 pkg/matcher.StructuralSimHash64 黄金值（跨语言逐位对拍）。
def test_structural_simhash64_matches_go_golden():
    feat = ComponentFeature(
        tag="button",
        role="button",
        attrs=["aria-label"],
        ancestors=[ComponentAncestor(tag="div", role="navigation")],
    )
    assert structural_phrases(feat) == [
        "tag:button",
        "role:button",
        "attr:aria-label",
        "sem-anc-role:navigation",
    ]
    assert structural_simhash64(feat) == 0x99CE76C794A892A9


def test_structural_simhash64_no_attrs_matches_go_golden():
    feat = ComponentFeature(tag="button", role="button")
    assert structural_simhash64(feat) == 0x980A324380A050CC


def test_feature_from_node_filters_whitelist():
    node = {
        "tag": "a",
        "role": "link",
        "attrs": {"aria-label": "x", "data-testid": "y", "class": "ignored", "id": "z"},
    }
    feat = feature_from_node(node)
    assert feat.tag == "a"
    assert feat.role == "link"
    assert feat.attrs == ["aria-label", "data-testid"]  # 仅白名单


def test_component_gene_bakes_struct_hash_into_rules():
    gene = ComponentGene()
    feat = ComponentFeature(tag="button", role="button")
    h = gene.calibrate("登录", feat)
    assert h == 0x980A324380A050CC

    rules = [
        DistilledRule(text_pattern="登录", action="click", confidence=0.9, semantic_label="登录"),
        DistilledRule(text_pattern="搜索", action="click", confidence=0.9, semantic_label="搜索"),
    ]
    baked = gene.apply(rules)
    assert baked[0].struct_hash == 0x980A324380A050CC  # 已校准回填
    assert baked[1].struct_hash == 0  # 未校准保持 0（引擎单通道降级）


def test_exporter_persists_struct_hash(tmp_path):
    exp = Exporter(tmp_path)
    rules = [
        DistilledRule(
            text_pattern="登录",
            action="click",
            confidence=0.9,
            semantic_label="登录",
            struct_hash=0x980A324380A050CC,
        )
    ]
    exp.export("v1.6.0", rules=rules, synonyms=[], negative_keywords=[])

    db = tmp_path / "v1.6.0" / "uws_l1.db"
    con = sqlite3.connect(db)
    try:
        row = con.execute(
            "SELECT struct_hash FROM uws_rules WHERE text_pattern = ?",
            ("登录",),
        ).fetchone()
    finally:
        con.close()
    assert row is not None
    # SQLite INTEGER 是有符号 int64；高位置位哈希以负数落库，读取后 reinterpret 为 uint64。
    stored = row[0]
    if stored < 0:
        stored += 1 << 64
    assert stored == 0x980A324380A050CC


def test_component_gene_load_reference(tmp_path):
    ref = tmp_path / "reference.json"
    ref.write_text(
        """[
          {"semantic": "登录", "struct": {"tag": "button", "role": "button",
            "attrs": ["aria-label"], "ancestors": [{"tag": "div", "role": "navigation"}]}}
        ]""",
        encoding="utf-8",
    )
    gene = ComponentGene().load_reference(ref)
    assert len(gene) == 1
    # 带 attrs + navigation 祖先的登录按钮 = 0x99CE76C794A892A9（Go 黄金）。
    assert gene._by_semantic["登录"] == 0x99CE76C794A892A9
