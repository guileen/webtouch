"""Episode 消费端三级弹性录取单元测试（spec/algorithm-kernel.md §5.0.14 条款 4）。

取代 §5.0.13 条款 5 的「三项代数条件一刀切」：覆盖 Tier1 黄金（`w=1.0`）/
Tier2 弱监督（`w=0.5`，入库并排入离线补打分队列）/ Tier3 阻断隔离（`quarantine_reason`），
以及 quarantine 隔离区 append-only 语义、源文件不改写与分级计数。
"""

import json
from pathlib import Path

import pytest

from pipeline.episode_consumer import (
    MIN_CANDIDATES,
    REASON_CANDIDATES_BELOW_MIN,
    REASON_CANDIDATES_EMPTY,
    REASON_CANDIDATES_SOURCE_AFTER_NAV,
    REASON_GROUND_TRUTH_COUNT,
    REASON_INVALID_JSON,
    REASON_NOT_AN_OBJECT,
    REASON_SHADOW_SCORES_CARDINALITY,
    REASON_UNEXPECTED_ERROR,
    TIER1_GOLDEN,
    TIER2_WEAK,
    TIER3_QUARANTINE,
    TIER_WEIGHTS,
    EpisodeQuarantine,
    classify_episode_record,
)


def _cand(index: int, gt: bool) -> dict:
    return {"index": index, "selector_hash": f"h{index}", "is_ground_truth": gt}


_DEFAULT = object()


def _shadow(scores) -> dict:
    return {"model_version": "v1", "scores": list(scores), "top1": 0}


def _rec(cands=_DEFAULT, **over) -> dict:
    if cands is _DEFAULT:
        cands = [_cand(0, True), _cand(1, False)]
    rec = {
        "episode_id": "ep-1",
        "candidates": cands,
        "production": {"candidates_source": "page_projection"},
        "shadow_inference": None,
    }
    rec.update(over)
    return rec


def _write_jsonl(path: Path, lines) -> None:
    with open(path, "w", encoding="utf-8") as f:
        for ln in lines:
            f.write(ln + "\n")


# ── 三级判定 ──────────────────────────────────────────────────────────────────

def test_tier1_golden_requires_shadow_scores_with_matching_cardinality():
    rec = _rec(shadow_inference=_shadow([0.9, 0.1]))
    assert classify_episode_record(rec) == (TIER1_GOLDEN, None)


def test_tier2_weak_supervision_when_shadow_missing():
    # 影子未接线（无 lib / 无模型 / 无 active artifact）⇒ 降权入库，不隔离。
    assert classify_episode_record(_rec(shadow_inference=None)) == (TIER2_WEAK, None)
    assert classify_episode_record(_rec()) == (TIER2_WEAK, None)


def test_tier3_candidates_below_min_is_literal():
    # 门槛照用户原文逐字执行：len(candidates) >= 2（单候选页判 Tier3）。
    assert MIN_CANDIDATES == 2
    assert classify_episode_record(_rec(cands=None)) == (TIER3_QUARANTINE, REASON_CANDIDATES_EMPTY)
    assert classify_episode_record(_rec(cands=[])) == (TIER3_QUARANTINE, REASON_CANDIDATES_EMPTY)
    assert classify_episode_record(_rec(cands="null")) == (TIER3_QUARANTINE, REASON_CANDIDATES_EMPTY)
    assert classify_episode_record({"episode_id": "ep-1"}) == (TIER3_QUARANTINE, REASON_CANDIDATES_EMPTY)
    one = [_cand(0, True)]
    assert classify_episode_record(_rec(cands=one)) == (TIER3_QUARANTINE, REASON_CANDIDATES_BELOW_MIN)


def test_tier3_ground_truth_must_be_exactly_one():
    zero = _rec(cands=[_cand(0, False), _cand(1, False)])
    two = _rec(cands=[_cand(0, True), _cand(1, True)])
    assert classify_episode_record(zero) == (TIER3_QUARANTINE, REASON_GROUND_TRUTH_COUNT)
    assert classify_episode_record(two) == (TIER3_QUARANTINE, REASON_GROUND_TRUTH_COUNT)


def test_tier3_shadow_cardinality_mismatch():
    bad = _rec(shadow_inference=_shadow([0.1]))
    assert classify_episode_record(bad) == (TIER3_QUARANTINE, REASON_SHADOW_SCORES_CARDINALITY)
    no_scores = _rec(shadow_inference={"model_version": "v1", "top1": 0})
    assert classify_episode_record(no_scores) == (TIER3_QUARANTINE, REASON_SHADOW_SCORES_CARDINALITY)


def test_tier3_non_object_and_after_nav_source():
    assert classify_episode_record("not-an-object") == (TIER3_QUARANTINE, REASON_NOT_AN_OBJECT)
    # 来源不可回放（订正后不再产生，旧样本仍须隔离）。
    nav = _rec(production={"candidates_source": "page_projection_after_nav"})
    assert classify_episode_record(nav) == (TIER3_QUARANTINE, REASON_CANDIDATES_SOURCE_AFTER_NAV)


def test_tier3_captures_unexpected_exception():
    class _Exploding(dict):
        def get(self, *a, **k):  # 校验途中抛异常 ⇒ 不得逃逸，归 Tier3。
            raise RuntimeError("boom")

    tier, reason = classify_episode_record(_Exploding())
    assert (tier, reason) == (TIER3_QUARANTINE, REASON_UNEXPECTED_ERROR)


# ── 分级录取与计数 ────────────────────────────────────────────────────────────

def test_weights_table_is_declared():
    assert TIER_WEIGHTS == {TIER1_GOLDEN: 1.0, TIER2_WEAK: 0.5}


def test_consumer_splits_three_tiers_and_annotates_weight(tmp_path):
    src = tmp_path / "episodes-20260925.jsonl"
    golden = json.dumps(_rec(shadow_inference=_shadow([0.9, 0.1])), ensure_ascii=False)
    weak = json.dumps(_rec(episode_id="ep-2"), ensure_ascii=False)
    bad_gt = json.dumps(_rec(episode_id="ep-3", cands=[_cand(0, False), _cand(1, False)]), ensure_ascii=False)
    bad_one = json.dumps(_rec(episode_id="ep-4", cands=[_cand(0, True)]), ensure_ascii=False)
    _write_jsonl(src, [golden, weak, bad_gt, bad_one])
    before = src.read_text(encoding="utf-8")

    q = EpisodeQuarantine(tmp_path)
    accepted = q.consume_file(src)

    assert [r["episode_id"] for r in accepted] == ["ep-1", "ep-2"]
    assert [(r["_tier"], r["_weight"]) for r in accepted] == [
        (TIER1_GOLDEN, 1.0),
        (TIER2_WEAK, 0.5),
    ]
    stats = q.stats()
    assert stats["tier_counts"] == {TIER1_GOLDEN: 1, TIER2_WEAK: 1, TIER3_QUARANTINE: 2}
    assert stats["weights"] == {TIER1_GOLDEN: 1.0, TIER2_WEAK: 0.5}
    assert stats["weighted_accepted_total"] == pytest.approx(1.5)
    assert stats["accepted_episodes_total"] == 2
    assert stats["quarantined_episodes_total"] == 2
    assert stats["shadow_missing_total"] == 1  # Tier2 样本（影子未接线）计数保留
    assert stats["offline_scoring_queue"] == ["ep-2"]
    assert stats["reasons"] == {
        REASON_GROUND_TRUTH_COUNT: 1,
        REASON_CANDIDATES_BELOW_MIN: 1,
    }
    # 源文件不得被改写（隔离区是副本，append-only）。
    assert src.read_text(encoding="utf-8") == before

    qfile = tmp_path / "quarantine" / src.name
    assert qfile.read_text(encoding="utf-8").splitlines() == [bad_gt, bad_one]

    # 追加式：再消费一轮只 append，不覆盖上一轮隔离内容。
    q.consume_file(src)
    assert qfile.read_text(encoding="utf-8").splitlines() == [bad_gt, bad_one] * 2
    assert q.stats()["quarantined_episodes_total"] == 4
    assert q.stats()["tier_counts"][TIER1_GOLDEN] == 2


def test_invalid_json_line_is_quarantined(tmp_path):
    src = tmp_path / "episodes-20260926.jsonl"
    _write_jsonl(src, ["{not json", json.dumps(_rec(), ensure_ascii=False)])
    q = EpisodeQuarantine(tmp_path)
    accepted = q.consume_file(src)
    assert len(accepted) == 1
    assert q.stats()["reasons"] == {REASON_INVALID_JSON: 1}
    assert (tmp_path / "quarantine" / src.name).read_text(encoding="utf-8").splitlines() == ["{not json"]


def test_all_tier1_creates_no_quarantine_dir(tmp_path):
    src = tmp_path / "episodes-20260927.jsonl"
    _write_jsonl(src, [json.dumps(_rec(shadow_inference=_shadow([0.9, 0.1])), ensure_ascii=False)])
    q = EpisodeQuarantine(tmp_path)
    assert len(q.consume_file(src)) == 1
    assert not (tmp_path / "quarantine").exists()
    assert q.stats()["quarantined_episodes_total"] == 0
    assert q.stats()["offline_scoring_queue"] == []


def test_custom_quarantine_dir_and_blank_lines_ignored(tmp_path):
    src = tmp_path / "episodes-20260928.jsonl"
    _write_jsonl(src, [json.dumps(_rec(), ensure_ascii=False), "", "   "])
    out = tmp_path / "custom-quarantine"
    q = EpisodeQuarantine(tmp_path, quarantine_dir=out)
    assert len(q.consume_file(src)) == 1
    assert q.stats()["quarantine_dir"] == str(out)


def test_exception_during_consume_is_quarantined_not_raised(tmp_path):
    src = tmp_path / "episodes-20260929.jsonl"
    _write_jsonl(src, ['{"candidates": {"a": 1}}'])
    q = EpisodeQuarantine(tmp_path)
    assert q.consume_file(src) == []
    assert q.stats()["quarantined_episodes_total"] == 1
    assert list(q.stats()["reasons"].values()) == [1]
