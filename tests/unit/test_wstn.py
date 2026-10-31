from __future__ import annotations

import json
from pathlib import Path

import pytest

from pipeline.wstn import (
    ActionEdge,
    RouteRequest,
    RouterParams,
    S3B6Router,
    StateNode,
    WSTNGraph,
)


def _node(state_id: str, simhash: int) -> StateNode:
    return StateNode(state_id, simhash, f"https://example.test/{state_id}", 1)


def _edge(source: str, target: str, *, weight: float = 1.0) -> ActionEdge:
    return ActionEdge(source, target, "click", f"open-{target}", weight=weight)


def _routing_graph() -> WSTNGraph:
    graph = WSTNGraph()
    for node in (
        _node("start", 0),
        _node("decoy", 0b1),
        _node("gold-1", 0b1111),
        _node("gold-2", 0b11),
        _node("target", 0),
    ):
        graph.add_node(node)
    graph.add_edge(_edge("start", "decoy", weight=3.0))
    graph.add_edge(_edge("start", "gold-1", weight=2.0))
    graph.add_edge(_edge("gold-1", "gold-2", weight=2.0))
    graph.add_edge(_edge("gold-2", "target", weight=2.0))
    graph.add_edge(_edge("gold-2", "gold-1", weight=0.5))
    return graph


def test_graph_round_trip_is_deterministic() -> None:
    graph = _routing_graph()
    payload = graph.to_json()
    restored = WSTNGraph.from_json(payload)

    assert restored.to_json() == payload
    assert restored.node_count == 5
    assert restored.edge_count == 5


def test_s3_wavefront_survives_attractive_dead_end_and_cycle() -> None:
    graph = _routing_graph()
    route = S3B6Router(graph).find_path(graph.node("start"), "target")
    greedy = S3B6Router(
        graph, RouterParams(start_k=1, branch_k=1, max_hops=5)
    ).find_path(graph.node("start"), "target")

    assert route.status == "FOUND"
    assert route.state_path == ("start", "gold-1", "gold-2", "target")
    assert len(set(route.state_path)) == len(route.state_path)
    assert greedy.status == "FALLBACK"


def test_versioned_consumer_contract_routes_exact_state_ids() -> None:
    graph = _routing_graph()
    request = RouteRequest("start", "target", target_intent="完成流程")
    result = S3B6Router(graph).route(request)

    assert request.to_dict()["schema_version"] == "wstn.route.v1"
    assert result.to_dict()["schema_version"] == "wstn.route.v1"
    assert result.to_dict()["message_type"] == "route_result"
    assert result.status == "FOUND"


def test_request_limits_are_authoritative_over_router_defaults() -> None:
    graph = _routing_graph()
    router = S3B6Router(graph, RouterParams(start_k=1, branch_k=1, max_hops=5))

    assert router.route(RouteRequest("start", "target")).status == "FOUND"
    assert (
        router.route(
            RouteRequest("start", "target", start_k=1, branch_k=1, max_hops=5)
        ).status
        == "FALLBACK"
    )


def test_schema_examples_cover_the_frozen_top_level_contract() -> None:
    schema_path = Path(__file__).resolve().parents[2] / "spec/wstn-routing-v1.schema.json"
    schema = json.loads(schema_path.read_text(encoding="utf-8"))

    for definition_name in ("route_request", "route_result"):
        definition = schema["$defs"][definition_name]
        example = definition["examples"][0]
        assert definition["additionalProperties"] is False
        assert set(definition["required"]) == set(example)
        assert example["schema_version"] == "wstn.route.v1"
        assert example["extensions"] == {}
        json.dumps(example, allow_nan=False)


def test_unknown_or_unreachable_target_falls_back() -> None:
    graph = _routing_graph()
    router = S3B6Router(graph)

    unknown = router.find_path(graph.node("start"), "missing")
    assert unknown.status == "FALLBACK"
    assert unknown.to_dict()["potential"] is None
    json.dumps(unknown.to_dict(), allow_nan=False)
    graph.find_edge("gold-2", "target").annihilate()
    assert router.find_path(graph.node("start"), "target").status == "FALLBACK"


def test_decoupled_credit_leaves_single_corridor_stable() -> None:
    graph = WSTNGraph()
    for node in (
        _node("entry", 0b111111),
        _node("hub", 0b1111),
        _node("chosen", 0b11),
        _node("alternate", 0b11111111),
        _node("target", 0),
    ):
        graph.add_node(node)
    graph.add_edge(_edge("entry", "hub", weight=1.0))
    graph.add_edge(_edge("hub", "chosen", weight=2.0))
    graph.add_edge(_edge("hub", "alternate", weight=0.5))
    graph.add_edge(_edge("chosen", "target", weight=2.0))
    route = S3B6Router(graph).find_path(graph.node("entry"), "target")
    corridor = graph.find_edge("entry", "hub")
    chosen = graph.find_edge("hub", "chosen")

    assert route.status == "FOUND"
    for _ in range(50):
        graph.reinforce(route, mode="decouple")

    assert corridor.weight == pytest.approx(1.0)
    assert chosen.weight > 2.0
    assert corridor.success_count == 50


def test_annihilated_primary_edge_switches_to_backup_next_query() -> None:
    graph = WSTNGraph()
    for node in (
        _node("start", 0b111111),
        _node("primary", 0b1),
        _node("backup", 0b1111),
        _node("target", 0),
    ):
        graph.add_node(node)
    graph.add_edge(_edge("start", "primary", weight=3.0))
    graph.add_edge(_edge("primary", "target", weight=3.0))
    graph.add_edge(_edge("start", "backup", weight=1.0))
    graph.add_edge(_edge("backup", "target", weight=1.0))
    router = S3B6Router(graph)

    first = router.find_path(graph.node("start"), "target")
    graph.find_edge("primary", "target").annihilate()
    second = router.find_path(graph.node("start"), "target")

    assert first.state_path == ("start", "primary", "target")
    assert second.status == "FOUND"
    assert second.state_path == ("start", "backup", "target")
    assert len(set(second.state_path)) == len(second.state_path)


def test_contract_rejects_invalid_edges() -> None:
    with pytest.raises(ValueError, match="unsupported action_type"):
        ActionEdge("a", "b", "hover", "bad")
    with pytest.raises(ValueError, match=r"\[0, 10\]"):
        _edge("a", "b", weight=11.0)


def test_result_contract_rejects_discontinuous_action_chain() -> None:
    from pipeline.wstn import RouteResult

    with pytest.raises(ValueError, match="not continuous"):
        RouteResult(
            status="FOUND",
            state_path=("a", "b"),
            edges=(_edge("a", "c"),),
            confidence=0.8,
            potential=0.2,
        )
