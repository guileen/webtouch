"""离线蒸馏/自进化集成测试（TDD-PIPE-03..06、TDD-LEARN-01..03、TDD-WATCH-01）。"""

import hashlib
import json
import sqlite3
from dataclasses import asdict

import pytest

from pipeline.aggregator.aggregator import Aggregator
from pipeline.cc_fetcher.topology import simhash64
from pipeline.exporter.exporter import Exporter, ExportError, VersionConflictError
from pipeline.exporter.training import HardNegativeMiner, Node
from pipeline.llm_distiller.distiller import LLMDistiller, TENTATIVE_STATUS, TENTATIVE_WEIGHT
from pipeline.watchdog import LLMBudgetWatchdog, TokenBucket


class _FakeClock:
    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        return self.now


def test_tdd_pipe_03_llm_distill_structured_output():
    calls = []

    def llm(pages):
        calls.append(list(pages))
        return {
            "distillation_data": {
                "rules": [{"text_pattern": "登录", "action": "click", "confidence": 0.95}],
                "new_synonyms": [
                    # LLM 即使返回 ACTIVE/1.0 也必须被压回 TENTATIVE/0.2（防投毒）
                    {"site_domain": "example.com", "synonym": "Sign in", "status": "ACTIVE", "base_weight": 1.0},
                    {"site_domain": "example.com", "synonym": "登 录"},
                ],
                "negative_keywords": ["广告", "弹窗"],
            }
        }

    d = LLMDistiller(llm, batch_size=2)
    result = d.distill(["页面A", "页面B", "页面C"])

    assert len(calls) == 2  # 3 页按 2 页/批提交
    assert len(result.rules) == 2
    assert result.rules[0].text_pattern == "登录"
    assert result.rules[0].action == "click"
    assert len(result.synonyms) == 4
    for s in result.synonyms:
        assert s.status == TENTATIVE_STATUS
        assert s.base_weight == TENTATIVE_WEIGHT
    assert set(result.negative_keywords) == {"广告", "弹窗"}
    assert result.dropped_count == 0

    # 缺失 distillation_data → 重试后仍失败 → 丢弃并计数，不崩溃
    bad_calls = []

    def bad_llm(pages):
        bad_calls.append(1)
        return {"nonsense": True}

    d2 = LLMDistiller(bad_llm, max_retries=1)
    r2 = d2.distill(["页面X", "页面Y"])
    assert r2.rules == [] and r2.synonyms == [] and r2.negative_keywords == []
    assert r2.dropped_count == 2
    assert len(bad_calls) == 2  # 1 批 ×（1 初试 + 1 重试）


def test_tdd_pipe_04_aggregate_determinism():
    from pipeline.llm_distiller.distiller import DistilledRule

    rules = [
        DistilledRule(text_pattern="登录", action="click", confidence=0.9),
        DistilledRule(text_pattern="登录", action="hover", confidence=0.95),
        DistilledRule(text_pattern="提交", action="click", confidence=0.8),
    ] * 100  # 流式重复输入

    a1 = Aggregator().add(rules).result()
    a2 = Aggregator().add(rules).result()

    def _serialize(res):
        return json.dumps(
            {"rules": [asdict(r) for r in res.rules], "conflicts": [asdict(c) for c in res.conflicts]},
            sort_keys=True,
            ensure_ascii=False,
        )

    # 相同输入两次运行产物完全一致（确定性）
    assert _serialize(a1) == _serialize(a2)
    # 同 text_pattern 不同 action 冲突被显式消解并记录（保留高置信 0.95 的 hover）
    assert {r.text_pattern for r in a1.rules} == {"登录", "提交"}
    conflict = [c for c in a1.conflicts if c.text_pattern == "登录"]
    assert len(conflict) == 1
    assert conflict[0].resolved_action == "hover"
    assert set(conflict[0].actions) == {"click", "hover"}


def test_tdd_pipe_05_export_completeness(tmp_path):
    from pipeline.llm_distiller.distiller import DistilledRule, SiteSynonym

    exporter = Exporter(tmp_path)
    rules = [
        DistilledRule(text_pattern="登录", action="click", confidence=0.95),
        DistilledRule(text_pattern="提交", action="click", confidence=0.9),
    ]
    syns = [SiteSynonym(site_domain="example.com", synonym="Sign in")]
    manifest = exporter.export("v1.4.0", rules, syns, ["广告", "弹窗"])

    # 产物齐全：SQLite + ONNX + manifest + checksums
    version_dir = tmp_path / "v1.4.0"
    assert set(manifest.files.keys()) == {"uws_l1.db", "classifier.onnx", "uws_l2_default.db"}
    for fname, meta in manifest.files.items():
        p = version_dir / fname
        assert p.exists(), fname
        assert p.stat().st_size == meta["size_bytes"]
        assert hashlib.sha256(p.read_bytes()).hexdigest() == meta["hash"].split(":", 1)[1]
    assert (version_dir / "manifest.json").exists()
    assert (version_dir / "checksums.sha256").exists()
    assert exporter.verify(manifest, version_dir) is True

    # SQLite 可被引擎语义读取：规则 active、同义词 TENTATIVE 0.2
    con = sqlite3.connect(version_dir / "uws_l1.db")
    row = con.execute(
        "SELECT text_pattern, action, status FROM uws_rules WHERE text_pattern='登录'"
    ).fetchone()
    assert (row[0], row[1], row[2]) == ("登录", "click", "active")
    row2 = con.execute(
        "SELECT site_domain, synonym, base_weight, status FROM site_synonyms WHERE synonym='Sign in'"
    ).fetchone()
    assert row2[2] == 0.2 and row2[3] == "TENTATIVE"
    con.close()

    # 版本语义化且唯一
    with pytest.raises(VersionConflictError):
        exporter.export("v1.4.0", rules)
    with pytest.raises(ExportError):
        exporter.export("1.4", rules)


def test_tdd_pipe_05_real_onnx_replaces_stub(tmp_path):
    """真实 ONNX 训练产物经 Exporter 落盘，替换确定性占位（USER_ACTION #1/#3）。"""
    import onnx
    import onnxruntime

    from pipeline.exporter.train_onnx import (
        SEED_NEGATIVES,
        SEED_POSITIVES,
        feature_vector,
        train_classifier_onnx,
    )
    from pipeline.exporter.exporter import _stub_onnx

    model_bytes = train_classifier_onnx(SEED_POSITIVES, SEED_NEGATIVES)
    assert model_bytes != _stub_onnx("v1.4.0", 2)  # 非占位

    exporter = Exporter(tmp_path)
    manifest = exporter.export("v1.4.0", onnx_model_bytes=model_bytes)
    onnx_path = tmp_path / "v1.4.0" / "classifier.onnx"
    assert onnx_path.stat().st_size == len(model_bytes) < 1_000_000

    model = onnx.load(onnx_path)
    onnx.checker.check_model(model)
    session = onnxruntime.InferenceSession(str(onnx_path), providers=["CPUExecutionProvider"])
    assert session.run(None, {"features": [feature_vector("登录", "登录")]})[1][0][1] > 0.5

    # knowledge_versions.model_hash 与产物一致
    con = sqlite3.connect(tmp_path / "v1.4.0" / "uws_l1.db")
    (model_hash,) = con.execute(
        "SELECT model_hash FROM knowledge_versions WHERE version='v1.4.0'"
    ).fetchone()
    con.close()
    assert model_hash == "sha256:" + hashlib.sha256(model_bytes).hexdigest()


def test_tdd_pipe_06_hard_negative_mining():
    nodes = [
        Node("p1", "登录 提交 表单 按钮", siblings=["s1", "s2"], positive=True),
        Node("p2", "注册 提交 表单 按钮", siblings=["s3"], positive=True),
        Node("s1", "登录 提交 表单 按钮 变体A"),
        Node("s2", "登录 提交 表单 按钮 变体B"),
        Node("s3", "注册 提交 表单 按钮 变体C"),
    ] + [Node(f"n{i}", f"干扰文本 标签{i} 随机值{i * 7}") for i in range(12)]

    miner = HardNegativeMiner()
    ds = miner.mine(nodes)

    # 每个正样本：≥1 兄弟负样本 + 恰好 3 个 SimHash 近邻负样本
    positives = [s for s in ds.samples if s.label == 1]
    assert len(positives) == 2
    for p in positives:
        sib = [s for s in ds.samples if s.label == 0 and s.kind == "sibling" and s.positive_id == p.node_id]
        neigh = [s for s in ds.samples if s.label == 0 and s.kind == "simhash_neighbor" and s.positive_id == p.node_id]
        assert len(sib) >= 1
        assert len(neigh) == 3
        # SimHash 近邻确实是池内汉明距最小的 3 个（排除自身）
        ph = simhash64(p.text)
        others = sorted(
            ((ph ^ simhash64(n.text)).bit_count(), n.node_id)
            for n in nodes
            if n.node_id != p.node_id
        )[:3]
        assert {s.node_id for s in neigh} == {nid for _, nid in others}

    # 抽样审计 100 条无违规
    report = miner.audit(ds, sample_size=100)
    assert report["checked"] == 2
    assert report["violations"] == []


def test_tdd_watch_01_daily_budget_fuse():
    clock = _FakeClock()
    w = LLMBudgetWatchdog(
        daily_limit=100,
        bucket=TokenBucket(capacity=1000, refill_per_sec=1000.0, clock=clock),
    )

    # 0~80%：离线蒸馏可用
    for _ in range(80):
        assert w.consume_offline(1) is True
    # 80%：熔断离线（LOW），在线兜底（HIGH）不受影响
    assert w.consume_offline(1) is False
    assert w.llm_online_allowed() is True
    assert w.consume_online(1) is True

    # 到 100%：切断所有 LLM 出口（在线退化为纯规则 + A*，不 panic）
    for _ in range(19):
        assert w.consume_online(1) is True
    assert w.consume_online(1) is False
    assert w.llm_online_allowed() is False
    assert w.llm_offline_allowed() is False


def test_tdd_learn_01_synonym_state_machine():
    from learning.online_learner.synonyms import (
        ACTIVATE_HIT_COUNT,
        ACTIVE_WEIGHT,
        CONSECUTIVE_FALLBACK_POISON_THRESHOLD,
        TENTATIVE_WEIGHT,
        SynonymStateMachine,
        SynonymStatus,
    )

    m = SynonymStateMachine()
    s = m.register("example.com", "Sign in")
    assert s.status is SynonymStatus.TENTATIVE
    assert s.weight == TENTATIVE_WEIGHT

    # hit_count < 5：保持 TENTATIVE（权重 0.2）
    for _ in range(ACTIVATE_HIT_COUNT - 1):
        s = m.record_hit_success("example.com", "Sign in")
    assert s.hit_count == ACTIVATE_HIT_COUNT - 1
    assert s.status is SynonymStatus.TENTATIVE
    assert s.weight == TENTATIVE_WEIGHT

    # 第 5 次成功命中 → ACTIVE（权重 1.0）
    s = m.record_hit_success("example.com", "Sign in")
    assert s.status is SynonymStatus.ACTIVE
    assert s.weight == ACTIVE_WEIGHT
    assert s.in_recall

    # 连续导致 LLM 兜底 → POISONED 物理隔离，不再参与召回
    m2 = SynonymStateMachine()
    s2 = m2.register("shop.com", "Buy now")
    for _ in range(CONSECUTIVE_FALLBACK_POISON_THRESHOLD):
        s2 = m2.record_fallback("shop.com", "Buy now")
    assert s2.status is SynonymStatus.POISONED
    assert s2.weight == 0.0
    assert not s2.in_recall


def test_tdd_learn_02_dynamic_prior_graduation():
    from learning.online_learner.priors import can_graduate, can_promote, dynamic_prior

    # 拓扑孤岛（兄弟 ≤1 且非高危）：α/β=0.1，1 次命中即可毕业
    assert dynamic_prior(siblings=0, high_risk=False) == (0.1, 0.1)
    assert can_graduate(hits=1, siblings=0, high_risk=False) is True

    # 高危区域：α=3/β=2，需连续 9 次命中
    assert dynamic_prior(siblings=1, high_risk=True) == (3.0, 2.0)
    assert can_graduate(hits=8, siblings=1, high_risk=True) is False
    assert can_graduate(hits=9, siblings=1, high_risk=True) is True

    # 密集列表（兄弟 >5）按高危先验处理
    assert dynamic_prior(siblings=6, high_risk=False) == (3.0, 2.0)

    # 默认（兄弟 2~5 且非高危）：拉普拉斯 α=1/β=1
    assert dynamic_prior(siblings=3, high_risk=False) == (1.0, 1.0)

    # 转 ACTIVE：激活 ≥20、score ≥0.85、5 天无衰退，三者缺一不可
    assert can_promote(activation_count=19, score=0.95, no_decay_days=6) is False
    assert can_promote(activation_count=20, score=0.84, no_decay_days=6) is False
    assert can_promote(activation_count=20, score=0.95, no_decay_days=4) is False
    assert can_promote(activation_count=20, score=0.95, no_decay_days=6) is True


def test_tdd_learn_03_shadow_eval_replay_hash():
    from learning.shadow_eval.evaluator import CandidateRule, ShadowEvaluator
    from learning.snapshot_store.store import Snapshot, SnapshotStore

    store = SnapshotStore()
    store.save(Snapshot(rule_id="r1", ast_semantic_hash="hash-a"))
    evaluator = ShadowEvaluator(store)

    # 快照哈希一致 + 动态先验通过 → 毕业
    ok = CandidateRule(
        rule_id="r1", ast_semantic_hash="hash-a", hits=1, siblings=0, high_risk=False
    )
    verdict = evaluator.evaluate(ok)
    assert verdict.graduated is True
    assert verdict.reason == "graduated"

    # 物理同构不匹配（ast_semantic_hash 不一致）→ 不得毕业
    mismatch = CandidateRule(
        rule_id="r1", ast_semantic_hash="hash-b", hits=1, siblings=0, high_risk=False
    )
    assert evaluator.evaluate(mismatch).reason == "hash_mismatch"

    # 快照缺失 → 不得毕业
    missing = CandidateRule(
        rule_id="r-missing", ast_semantic_hash="hash-a", hits=1, siblings=0, high_risk=False
    )
    assert evaluator.evaluate(missing).reason == "snapshot_missing"
