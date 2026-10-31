from __future__ import annotations

import json

import pytest

from benchmarks.bench_wstn_state_pipeline import run as run_state_pipeline_benchmark
from pipeline.wstn import S3B6Router
from pipeline.wstn_state import (
    CausalWSTNRecorder,
    PageSnapshot,
    PageStateEncoder,
    calibrate_hamming_threshold,
    evaluate_hamming_threshold,
)

LOGIN = """
<html><head><title>账户登录</title></head><body>
<main><h1>登录</h1><form aria-label="账户登录">
<label>邮箱 <input type="email" aria-label="邮箱"></label>
<label>密码 <input type="password" aria-label="密码"></label>
<button data-testid="submit-login">登录</button>
</form></main></body></html>
"""

LOGIN_MUTATED = """
<html><head><title>账户登录</title></head><body>
<div class="css-9a8b"><main><div><h1>登录账户</h1></div>
<form class="generated-123" aria-label="账户登录">
<label>邮箱 <input aria-label="邮箱" type="email"></label>
<label>密码 <input aria-label="密码" type="password"></label>
<button data-testid="renamed-login">立即登录</button>
</form></main></div><span class="clock">2026-09-14 18:00</span>
</body></html>
"""

CART = """
<html><head><title>购物车</title></head><body>
<main><h1>购物车</h1><section aria-label="订单商品">
<article><h2>旅行背包</h2><button>减少数量</button><button>增加数量</button></article>
</section><button data-testid="checkout">去结算</button></main>
</body></html>
"""


def _snapshot(name: str, html: str) -> PageSnapshot:
    return PageSnapshot(url=f"https://shop.test/{name}", html=html)


def test_page_state_encoder_is_robust_to_presentational_dom_drift() -> None:
    encoder = PageStateEncoder()
    original = encoder.encode(_snapshot("login", LOGIN))
    mutated = encoder.encode(_snapshot("login?attempt=2", LOGIN_MUTATED))

    assert original.hamming(mutated) <= 5
    assert original.roles_bitmap == mutated.roles_bitmap
    assert original.simhash == encoder.encode(_snapshot("login", LOGIN)).simhash


def test_page_state_encoder_separates_different_logical_pages() -> None:
    encoder = PageStateEncoder()
    login = encoder.encode(_snapshot("login", LOGIN))
    cart = encoder.encode(_snapshot("cart", CART))

    assert login.hamming(cart) > 5


def test_roc_threshold_is_selected_on_calibration_and_measured_on_holdout() -> None:
    calibration = [(0, 1, True), (4, 5, True), (0, 63, False), (3, 60, False)]
    threshold, metrics = calibrate_hamming_threshold(calibration, thresholds=range(9))
    holdout = evaluate_hamming_threshold(
        [(8, 9, True), (0, 31, False), (15, 48, False)], threshold
    )

    assert threshold == 1
    assert metrics.false_positive_rate == 0.0
    assert holdout.different_state_merge_rate == 0.0


def test_causal_recorder_emits_continuous_five_step_wstn() -> None:
    encoder = PageStateEncoder()
    states = [
        _snapshot("home", "<main><h1>商城首页</h1><button>开始搜索</button></main>"),
        _snapshot(
            "search",
            "<main><h1>商品搜索</h1><input aria-label='关键词'><button>搜索</button></main>",
        ),
        _snapshot(
            "results", "<main><h1>搜索结果</h1><a href='/product/1'>旅行背包</a></main>"
        ),
        _snapshot(
            "product/1", "<main><h1>旅行背包详情</h1><button>加入购物车</button></main>"
        ),
        _snapshot("cart", CART),
        _snapshot(
            "checkout", "<main><h1>确认订单</h1><button>提交订单</button></main>"
        ),
    ]
    recorder = CausalWSTNRecorder(encoder=encoder, threshold=5)
    actions = [
        ("click", "开始搜索"),
        ("input", "关键词"),
        ("click", "旅行背包"),
        ("click", "加入购物车"),
        ("click", "去结算"),
    ]

    for index, (action_type, semantic) in enumerate(actions):
        recorder.record(
            step_id=f"step-{index + 1}",
            action_type=action_type,
            target_semantic=semantic,
            before=states[index],
            after=states[index + 1],
            latency_ms=2.5 + index,
        )

    payload = recorder.to_dict()
    assert payload["schema_version"] == "webtouch.wstn.causal.v1"
    assert len(payload["transitions"]) == 5
    assert len(payload["graph"]["nodes"]) == 6
    assert len(payload["graph"]["edges"]) == 5
    assert all(row["causal_change"] for row in payload["transitions"])
    json.dumps(payload, allow_nan=False)

    graph = recorder.graph
    start_id = payload["transitions"][0]["before_state_id"]
    target_id = payload["transitions"][-1]["after_state_id"]
    route = S3B6Router(graph).find_path(graph.node(start_id), target_id)
    assert route.status == "FOUND"
    assert route.hops == 5


def test_causal_recorder_rejects_action_without_state_transition() -> None:
    snapshot = _snapshot("login", LOGIN)
    recorder = CausalWSTNRecorder(PageStateEncoder(), threshold=5)

    with pytest.raises(ValueError, match="did not cross a page-state boundary"):
        recorder.record(
            step_id="noop",
            action_type="click",
            target_semantic="登录",
            before=snapshot,
            after=snapshot,
        )


def test_page_state_benchmark_passes_disjoint_holdout_gate() -> None:
    result = run_state_pipeline_benchmark()

    assert result["tier1"]["holdout"]["different_state_merge_rate"] < 0.05
    assert result["tier1"]["holdout"]["same_state_split_rate"] < 0.05
    assert result["gates"]["tier1_cross_page_confusion"] == "PASS"
    assert result["gates"]["tier2_recorder_kernel"] == "PASS"
    assert result["gates"]["tier2_real_cdp_capture"] in ("NOT_RUN", "PASSED")
