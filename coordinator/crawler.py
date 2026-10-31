"""智能爬虫驱动（TDD-COORD-05，Mock 博客站 user case，2026-08-20 续24）。

CrawlDriver 是「爬取级」编排（与 Executor 任务级状态机分层）：消费 EngineClient
协议（生产 = GrpcEngineClient，测试 = FakeEngine），以 signals 为提取契约逐页执行：

  1. InitializeContext(initial_url=seed) 进入种子页；
  2. 对每个 signal 执行 extract（缺失信号优雅跳过，转瞬失败重试 1 次）；
  3. negative_keywords 过滤——提取值命中负向关键词（广告/推广）即剔除并记录审计；
  4. 点击「下一页」深入，max_depth 封顶；逐页输出 Markdown + YAML Frontmatter
     （url/depth/signals 字段），供 user case「正文 + 作者、不含侧边栏广告」验收。

当前 URL 跟踪受引擎契约限制（ActionResult 未回传 current_url）：深层记录 url 暂以
seed 标识 + depth 区分（见 USER_ACTION 后续项）。
"""

from __future__ import annotations

import json
import time
from collections.abc import Iterable
from dataclasses import dataclass, field

from coordinator.engine_client import ABORTED, NOT_FOUND, EngineClient, EngineError

EXTRACT_RETRIES = 2  # 深层页导航竞态下转瞬失败的重试次数（真实爬虫健壮性）
RETRY_BACKOFF_SEC = 0.5


class CrawlError(RuntimeError):
    """爬取级错误（引擎失败且重试耗尽）。"""


@dataclass
class CrawlOptions:
    signals: list[str] = field(default_factory=lambda: ["main_article", "author_name"])
    negative_keywords: list[str] = field(default_factory=lambda: ["广告", "推广"])
    max_depth: int = 2
    next_semantic: str = "下一页"


@dataclass
class PageRecord:
    url: str
    depth: int
    fields: dict[str, str]
    markdown: str
    ad_filtered: list[str] = field(default_factory=list)  # 命中的负向关键词值（审计）


class CrawlDriver:
    def __init__(self, engine: EngineClient, options: CrawlOptions | None = None) -> None:
        self._engine = engine
        self._opts = options or CrawlOptions()

    def crawl(self, seed_url: str) -> list[PageRecord]:
        """从种子 URL 开始逐层爬取，返回每页 PageRecord（深度递增，max_depth 封顶）。"""
        init = self._engine.initialize_context(
            task_id="crawl-" + _short_id(seed_url), initial_url=seed_url
        )
        if not init.ok:
            raise CrawlError(f"initialize_context: {init.error_reason or 'failed'}")

        records: list[PageRecord] = []
        for depth in range(1, self._opts.max_depth + 1):
            fields, filtered = self._extract_page(depth)
            records.append(self._render(seed_url, depth, fields, filtered))
            if depth >= self._opts.max_depth:
                break
            if not self._go_next(depth):
                break  # 无「下一页」→ 爬取结束
        return records

    def _extract_page(self, depth: int) -> tuple[dict[str, str], list[str]]:
        fields: dict[str, str] = {}
        filtered: list[str] = []
        for i, signal in enumerate(self._opts.signals):
            value = self._extract_once(signal, f"{depth}-{i}", depth)
            if value is None:
                continue  # 信号缺失优雅跳过
            if self._is_negative(value):
                filtered.append(value)
                continue
            fields[signal] = value
        return fields, filtered

    def _extract_once(self, signal: str, seq: str, depth: int) -> str | None:
        for attempt in range(EXTRACT_RETRIES):
            try:
                resp = self._engine.execute_action(
                    intent_id=f"crawl-{depth}-{seq}-{signal}-{attempt}",
                    target_semantic=signal,
                    expected_action="extract",
                )
            except EngineError as exc:
                # NOT_FOUND（规则层无此语义）/ ABORTED（物理层 no_match /
                # eval_error / target_destroyed 等）：均视为页面无此信号，
                # 优雅跳过（extract 语义缺失不是爬取失败）。
                if exc.code in (NOT_FOUND, ABORTED):
                    return None
                if attempt + 1 < EXTRACT_RETRIES:
                    time.sleep(RETRY_BACKOFF_SEC)
                    continue
                raise CrawlError(f"extract({signal}): {exc.reason or exc}") from exc
            if not resp.success:
                return None
            value = (resp.extracted_data or "").strip()
            return value or None
        return None

    def _go_next(self, depth: int) -> bool:
        for attempt in range(EXTRACT_RETRIES):
            try:
                resp = self._engine.execute_action(
                    intent_id=f"crawl-next-{depth}-{attempt}",
                    target_semantic=self._opts.next_semantic,
                    expected_action="click",
                )
            except EngineError:
                return False  # 没有「下一页」→ 停止
            if resp.success:
                return True
            if attempt + 1 < EXTRACT_RETRIES:
                time.sleep(RETRY_BACKOFF_SEC)
        return False

    def _is_negative(self, value: str) -> bool:
        return is_negative_keyword(value, self._opts.negative_keywords)

    @staticmethod
    def _render(url: str, depth: int, fields: dict[str, str], filtered: list[str]) -> PageRecord:
        return render_page_record(url, depth, fields, filtered)


def _short_id(url: str) -> str:
    """确定性短任务号：去除协议与标点，便于幂等/日志阅读。"""
    cleaned = "".join(ch for ch in url if ch.isalnum())
    return cleaned[-12:] or "seed"


def is_negative_keyword(value: str, keywords: Iterable[str]) -> bool:
    """负向关键词过滤（在线 crawl 与离线 parse-warc 共用同一规则）。"""
    low = value.lower()
    return any(kw.lower() in low for kw in keywords)


def render_page_record(
    url: str, depth: int, fields: dict[str, str], filtered: list[str]
) -> PageRecord:
    """Markdown + YAML Frontmatter 渲染（在线/离线同构的唯一实现）。"""
    front = ["---", f"url: {json.dumps(url, ensure_ascii=False)}", f"depth: {depth}"]
    for k, v in fields.items():
        front.append(f"{k}: {json.dumps(v, ensure_ascii=False)}")
    if filtered:
        front.append(f"ad_filtered: {len(filtered)}")
    front.append("---")
    body: list[str] = []
    for k, v in fields.items():
        body += [f"## {k}", "", v, ""]
    markdown = "\n".join(front) + "\n\n" + "\n".join(body).strip() + "\n"
    return PageRecord(
        url=url, depth=depth, fields=fields, markdown=markdown, ad_filtered=filtered
    )
