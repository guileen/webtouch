"""CrawlDriver 单元测试（TDD-COORD-05，2026-08-20 续24）。

确定性脚本化引擎：逐页 extract（缺失信号优雅跳过）、负向关键词过滤、
「下一页」深入与 max_depth 封顶、Markdown + YAML Frontmatter 输出。
"""

from __future__ import annotations

from collections import deque

from coordinator.crawler import CrawlDriver, CrawlError, CrawlOptions
from coordinator.engine_client import (
    NOT_FOUND,
    ActionResult,
    EngineError,
    InitResult,
)


class FakeCrawlEngine:
    """脚本化引擎：execute_action 按 target_semantic 弹出预设结果。"""

    def __init__(self, init_ok: bool = True) -> None:
        self.script: dict[str, deque] = {}
        self.init_ok = init_ok
        self.calls: list[tuple[str, str]] = []

    def initialize_context(
        self, task_id: str, initial_url: str | None = None, **kwargs: object
    ) -> InitResult:
        self.calls.append(("init", initial_url or ""))
        if not self.init_ok:
            return InitResult(ok=False, error_reason="init_failed")
        return InitResult(ok=True, target_id="t1", context_id="c1")

    def execute_action(
        self,
        intent_id: str,
        target_semantic: str,
        expected_action: str,
        value: str | None = None,
        context_hint: dict[str, str] | None = None,
        timeout_ms: int = 1500,
    ) -> ActionResult:
        self.calls.append((target_semantic, expected_action))
        outcomes = self.script.get(target_semantic)
        if outcomes:
            item = outcomes.popleft()
            if isinstance(item, EngineError):
                raise item
            return item
        return ActionResult(success=False)


def _extract(text: str) -> ActionResult:
    return ActionResult(success=True, extracted_data=text)


def test_crawl_depth_two_collects_signals_and_filters_ads():
    engine = FakeCrawlEngine()
    engine.script["main_article"] = deque(
        [_extract("首页正文：欢迎访问测试博客。"), _extract("第二层正文：深入一层的文章内容。")]
    )
    engine.script["author_name"] = deque([_extract("作者甲"), _extract("作者乙")])
    engine.script["横幅"] = deque([_extract("广告：限时特惠买买买")])
    engine.script["下一页"] = deque(
        [ActionResult(success=True, context_state="NAVIGATED"), EngineError(NOT_FOUND, "无下一页")]
    )

    driver = CrawlDriver(
        engine,
        CrawlOptions(
            signals=["main_article", "author_name", "横幅"],
            negative_keywords=["广告", "推广"],
            max_depth=2,
        ),
    )
    records = driver.crawl("http://mock.blog/")

    assert len(records) == 2
    assert records[0].fields["main_article"].startswith("首页正文")
    assert records[1].fields["main_article"].startswith("第二层正文")
    assert records[0].fields["author_name"] == "作者甲"
    assert records[1].fields["author_name"] == "作者乙"
    # 广告值被负向关键词过滤，不进字段、不进 Markdown，仅留审计。
    assert "横幅" not in records[0].fields
    assert records[0].ad_filtered == ["广告：限时特惠买买买"]
    corpus = "\n".join(r.markdown for r in records)
    assert "广告" not in corpus and "买买买" not in corpus
    assert "作者甲" in records[0].markdown and "作者乙" in records[1].markdown
    # 深层页面经「下一页」到达（引擎调用序列：extract×3 → click → extract×3）。
    assert ("下一页", "click") in engine.calls


def test_crawl_missing_signal_graceful():
    engine = FakeCrawlEngine()
    engine.script["main_article"] = deque([_extract("只有正文，没有作者")])
    engine.script["author_name"] = deque([EngineError(NOT_FOUND, "页面无作者节点")])
    driver = CrawlDriver(
        engine, CrawlOptions(signals=["main_article", "author_name"], max_depth=1)
    )
    records = driver.crawl("http://mock.blog/")
    assert len(records) == 1
    assert "author_name" not in records[0].fields
    assert "## author_name" not in records[0].markdown
    assert "## main_article" in records[0].markdown


def test_crawl_stops_when_no_next():
    engine = FakeCrawlEngine()
    engine.script["main_article"] = deque([_extract("首页正文")])
    engine.script["下一页"] = deque([EngineError(NOT_FOUND, "无下一页")])
    driver = CrawlDriver(
        engine, CrawlOptions(signals=["main_article"], max_depth=2)
    )
    records = driver.crawl("http://mock.blog/")
    assert len(records) == 1


def test_crawl_init_failure_raises():
    driver = CrawlDriver(FakeCrawlEngine(init_ok=False))
    try:
        driver.crawl("http://mock.blog/")
    except CrawlError as exc:
        assert "initialize_context" in str(exc)
    else:
        raise AssertionError("应抛 CrawlError")


def test_markdown_frontmatter_escaping():
    engine = FakeCrawlEngine()
    engine.script["main_article"] = deque([_extract('含 "引号" 与\n换行的正文')])
    driver = CrawlDriver(engine, CrawlOptions(signals=["main_article"], max_depth=1))
    records = driver.crawl("http://mock.blog/")
    md = records[0].markdown
    assert md.startswith("---")
    assert 'url: "http://mock.blog/"' in md
    assert "depth: 1" in md
    assert 'main_article: "含 \\"引号\\" 与\\n换行的正文"' in md
    assert "## main_article" in md
