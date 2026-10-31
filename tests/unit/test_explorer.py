"""站点探索单元测试（TDD-CLI-17：uws explore 启发式探索 → 导航图报告）。"""

from __future__ import annotations

import pytest


@pytest.fixture(autouse=True)
def isolated_attach(monkeypatch):
    """Fake-engine tests never consult or launch a host browser."""
    monkeypatch.setattr("coordinator.cli.resolve_attach", lambda *args, **kwargs: "http://127.0.0.1:9222")

from coordinator.cli import create_parser, run_explore
from coordinator.engine_client import InitResult
from coordinator.explorer import (
    CollectResult,
    Element,
    ExploreDriver,
    ExploreOptions,
    PageSnapshot,
    SiteReport,
    classify_page,
    dedupe_links,
    normalize_link,
    render_markdown,
)

# ── 页面类型分类 ────────────────────────────────────────────────────────────

def test_classify_checkout_by_text_and_testid() -> None:
    ptype, reason = classify_page(
        "https://site.example/drama/d1",
        "",
        "解锁与购买 模拟支付",
        [],
    )
    assert ptype == "checkout" and "付费" in reason
    ptype2, _ = classify_page(
        "https://site.example/x", "", "", [Element(testid="checkout-modal")]
    )
    assert ptype2 == "checkout"


def test_classify_player_by_video_and_ep_marker() -> None:
    ptype, _ = classify_page("https://site.example/drama/d1", "", "", [Element(tag="video")])
    assert ptype == "player"
    ptype2, _ = classify_page("https://site.example/drama/d1", "", "EP 2 都市逆袭", [])
    assert ptype2 == "player"


def test_classify_detail_by_url_and_testid() -> None:
    ptype, _ = classify_page("https://site.example/drama/d1", "", "", [Element(testid="play-cta")])
    assert ptype == "detail"
    ptype2, reason = classify_page("https://site.example/drama/d1", "", "", [])
    assert ptype2 == "detail" and "{id}" in reason


def test_classify_auth_me_search_landing_other() -> None:
    assert classify_page("https://site.example/login", "", "", [])[0] == "auth"
    assert classify_page("https://site.example/me", "", "", [])[0] == "me"
    assert classify_page("https://site.example/search", "", "", [])[0] == "search"
    assert classify_page("https://site.example/", "", "", [Element(testid="hero-banner")])[0] == "landing"
    assert classify_page("https://site.example/", "", "", [])[0] == "landing"  # 根路径兜底
    # nav-avatar 是导航入口，不能把落地页误判成个人页
    assert classify_page("https://site.example/", "", "", [Element(testid="nav-avatar")])[0] == "landing"
    assert classify_page("https://site.example/about", "关于", "", [])[0] == "other"


# ── 链接归一化/去重 ─────────────────────────────────────────────────────────

def test_normalize_link_same_origin_only() -> None:
    base = "https://site.example/"
    assert normalize_link("https://site.example", "https://site.example") == "https://site.example/"
    assert normalize_link("https://site.example", "/") == "https://site.example/"
    assert normalize_link(base, "/drama/d1") == "https://site.example/drama/d1"
    assert normalize_link(base, "https://site.example/me#x") == "https://site.example/me"
    assert normalize_link(base, "https://other.example/x") is None
    assert normalize_link(base, "javascript:void(0)") is None
    assert normalize_link(base, "#top") is None
    assert normalize_link(base, "mailto:a@b.c") is None
    assert normalize_link(base, "/cover.png") is None
    assert normalize_link(base, "/a.css") is None


def test_dedupe_links_preserves_order() -> None:
    links = ["/a", "/b", "/a", "/c", "/b"]
    assert dedupe_links(links) == ["/a", "/b", "/c"]


# ── 报告渲染 ────────────────────────────────────────────────────────────────

def test_render_markdown_contains_nav_graph_and_sections() -> None:
    pages = [
        PageSnapshot(
            url="https://site.example/",
            depth=0,
            page_type="landing",
            type_reason="根路径",
            title="首页",
            elements=[Element(tag="button", text="Play", testid="hero-watch")],
            links=["https://site.example/drama/d1"],
        ),
        PageSnapshot(
            url="https://site.example/drama/d1",
            depth=1,
            page_type="detail",
            type_reason="URL 形态",
            elements=[Element(tag="button", text="立即播放", testid="play-cta")],
        ),
    ]
    report = SiteReport(seed_url="https://site.example/", pages=pages, edges=[(pages[0].url, pages[1].url)])
    md = render_markdown(report)
    assert "# 站点探索报告" in md
    assert "## 导航图" in md
    assert "```mermaid" in md and "graph LR" in md
    assert '"/ (landing)"' in md and '"/drama/d1 (detail)"' in md
    assert "## 页面清单" in md and "## 功能清单" in md
    assert "hero-watch" in md and "play-cta" in md


# ── 探索驱动 BFS（注入采集器，无浏览器依赖）────────────────────────────────

class _FakeEngine:
    def __init__(self) -> None:
        self.closed: list[str] = []
        self.shots: list[str] = []

    def initialize_context(self, task_id: str, initial_url: str | None = None, **kwargs: object) -> InitResult:
        return InitResult(ok=True, target_id="tgt", context_id="ctx")

    def capture_screenshot(self, target_id: str) -> bytes:
        self.shots.append(target_id)
        return b"\x89PNG-fake-shot"

    def close_context(self, context_name: str = "") -> None:
        self.closed.append(context_name)


def test_driver_bfs_same_origin_capped_and_classified() -> None:
    collector = {
        "https://site.example/": CollectResult(
            title="首页",
            text="热播",
            elements=[Element(tag="button", text="Play", testid="hero-watch")],
            links=["https://site.example/drama/d1", "https://other.example/x"],
        ),
        "https://site.example/drama/d1": CollectResult(
            title="详情",
            text="立即播放",
            elements=[Element(tag="button", text="立即播放", testid="play-cta")],
            links=[],
        ),
    }
    engine = _FakeEngine()
    driver = ExploreDriver(
        engine,
        ExploreOptions(max_pages=4, max_depth=2, settle_sec=0, attach="http://127.0.0.1:9222"),
        collector=lambda attach, url: collector[url],
    )
    report = driver.explore("https://site.example/")
    assert len(report.pages) == 2
    assert [p.page_type for p in report.pages] == ["landing", "detail"]
    assert report.edges == [("https://site.example/", "https://site.example/drama/d1")]
    assert engine.closed == ["explore-0", "explore-1"]


def test_driver_normalizes_root_and_filters_pseudo_links() -> None:
    collector = {
        "https://site.example/": CollectResult(
            title="首页",
            elements=[Element(tag="a", text="我的", testid="nav-avatar")],
            links=["https://site.example", "javascript:void(0)", "/me"],
        ),
        "https://site.example/me": CollectResult(
            title="我的",
            elements=[Element(tag="p", testid="profile-nickname")],
            links=[],
        ),
    }
    driver = ExploreDriver(
        _FakeEngine(),
        ExploreOptions(max_pages=3, max_depth=2, settle_sec=0),
        collector=lambda attach, url: collector[url],
    )
    report = driver.explore("https://site.example")
    assert len(report.pages) == 2
    assert report.pages[0].url == "https://site.example/"
    assert report.pages[0].page_type == "landing"
    assert report.pages[0].links == ["https://site.example/me"]
    assert report.pages[1].url == "https://site.example/me"
    assert report.pages[1].page_type == "me"


def test_driver_captures_shots_into_assets(tmp_path) -> None:
    collector = {
        "https://site.example/": CollectResult(
            title="首页",
            elements=[Element(tag="button", text="Play", testid="hero-watch")],
            links=["https://site.example/me"],
        ),
        "https://site.example/me": CollectResult(title="我的", links=[]),
    }
    assets = tmp_path / "report.assets"
    engine = _FakeEngine()
    driver = ExploreDriver(
        engine,
        ExploreOptions(max_pages=3, max_depth=2, settle_sec=0, assets_dir=str(assets)),
        collector=lambda attach, url: collector[url],
    )
    report = driver.explore("https://site.example/")
    assert len(engine.shots) == 2
    pngs = sorted(p.name for p in assets.glob("*.png"))
    assert pngs == ["00-home.png", "01-me.png"]
    assert report.pages[0].screenshot == "00-home.png"
    assert len(engine.closed) == 2  # 截图在关闭上下文前完成

    report.assets_rel = "report.assets"
    md = render_markdown(report)
    assert "![image](report.assets/00-home.png)" in md or "![" in md


def test_driver_fallback_without_cdp(monkeypatch) -> None:
    def boom(attach: str, current_url: str) -> CollectResult:
        raise RuntimeError("no cdp")

    engine = _FakeEngine()
    driver = ExploreDriver(
        engine,
        ExploreOptions(max_pages=2, max_depth=1, settle_sec=0),
        collector=boom,
    )
    report = driver.explore("https://site.example/")
    assert len(report.pages) == 1
    assert report.pages[0].note.startswith("CDP 结构采集不可用")
    assert report.pages[0].page_type == "landing"  # 根路径兜底分类仍可用


# ── CLI 接线（TDD-CLI-17）───────────────────────────────────────────────────

def test_run_explore_cli_wiring_writes_report(tmp_path, monkeypatch) -> None:
    def fake_collect(attach: str, current_url: str) -> CollectResult:
        return CollectResult(
            title="首页",
            text="",
            elements=[Element(tag="button", text="Play", testid="hero-watch")],
            links=[],
        )

    monkeypatch.setattr("coordinator.explorer.collect_via_cdp", fake_collect)
    engine = _FakeEngine()
    out = tmp_path / "explore.md"
    parser = create_parser()
    args = parser.parse_args([
        "explore", "https://site.example/",
        "--max-pages", "2", "--max-depth", "1", "--settle-sec", "0",
        "--output", str(out),
    ])
    ret = run_explore(args, engine=engine)
    assert ret == 0
    content = out.read_text(encoding="utf-8")
    assert "# 站点探索报告" in content
    assert "## 导航图" in content
    assert "hero-watch" in content
    assets = tmp_path / "explore.assets"
    assert list(assets.glob("*.png"))  # 每页截图已落盘
    assert "![" in content  # 报告内嵌截图引用


def test_explore_parser_help_text() -> None:
    parser = create_parser()
    args = parser.parse_args(["explore", "https://site.example/"])
    assert args.command == "explore"
    assert args.url == "https://site.example/"
    assert args.max_pages == 12
