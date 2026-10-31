"""站点启发式探索与导航图报告（uws explore，2026-08-28）。

ExploreDriver 用「启发式 + 确定性」方式了解一个网站：

  1. 从种子 URL 出发，用引擎逐页 InitializeContext（每页一个命名上下文，
     附着调试 Chrome 时共享登录态与指纹）；
  2. 每页经 CDP（默认 http://127.0.0.1:9222）采集：标题 / 正文摘要 /
     可交互元素（button/a/input/testid/role）/ 同域链接；
  3. 按 URL + testid + 文本关键词分类页面类型
     （landing / detail / player / auth / me / checkout / search / other）；
  4. BFS 同域链接（max_pages / max_depth 封顶），生成站点导航图
     （Mermaid）+ 页面清单 + 功能清单，输出 Markdown 报告。

纯规则、不调 VLM：结构发现与分类全部确定性启发式（M13/M16：厚工具、薄 LLM）。
CDP 不可用时降级为「仅 URL 级快照」并标注，不崩溃。
"""

from __future__ import annotations

import json
import re
import time
import urllib.parse
import urllib.request
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path

from coordinator.engine_client import EngineClient

# 静态资源后缀：同域链接中发现即跳过，避免探索进图片/字体/文档。
ASSET_EXTS = {
    ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico", ".avif",
    ".css", ".js", ".mjs", ".woff", ".woff2", ".ttf", ".otf", ".eot",
    ".mp4", ".webm", ".mp3", ".wav", ".pdf", ".zip", ".gz", ".webmanifest",
}


@dataclass
class ExploreOptions:
    max_pages: int = 12
    max_depth: int = 3
    settle_sec: float = 5.0
    attach: str = "http://127.0.0.1:9222"
    timeout_ms: int = 15000
    assets_dir: str = ""          # 截图输出目录（空 = 不截图）
    capture_shots: bool = True    # 是否每页截图（--no-shots 可关）


@dataclass
class Element:
    tag: str = ""
    text: str = ""
    href: str = ""
    testid: str = ""
    role: str = ""


@dataclass
class CollectResult:
    title: str = ""
    text: str = ""
    elements: list[Element] = field(default_factory=list)
    links: list[str] = field(default_factory=list)
    note: str = ""


@dataclass
class PageSnapshot:
    url: str
    depth: int
    page_type: str = "other"
    type_reason: str = ""
    title: str = ""
    elements: list[Element] = field(default_factory=list)
    links: list[str] = field(default_factory=list)
    note: str = ""
    screenshot: str = ""          # 相对报告位置的截图文件名（如 page-01-landing.png）


@dataclass
class SiteReport:
    seed_url: str
    pages: list[PageSnapshot] = field(default_factory=list)
    edges: list[tuple[str, str]] = field(default_factory=list)
    assets_rel: str = ""          # 报告引用的截图目录相对名（如 report.assets）


# ── 页面类型分类（确定性启发式，优先级从高到低）──────────────────────────────

_TYPE_RULES: list[tuple[str, str, callable]] = []


def _mk_checkout(url: str, title: str, text: str, elements: list[Element]) -> tuple[str, str]:
    low = url.lower()
    if any(k in low for k in ("/pay", "/checkout", "/payment", "/order", "/recharge", "/topup")):
        return "checkout", f"URL 含付费路径: {low}"
    t = text.lower()
    if any(k in t for k in ("模拟支付", "解锁与购买", "apple pay", "google pay", "adyen", "卡号", "解锁本集")):
        return "checkout", "正文含付费/解锁关键词"
    for el in elements:
        if el.testid and any(k in el.testid.lower() for k in ("checkout", "payment", "pay-modal")):
            return "checkout", f"testid 含付费语义: {el.testid}"
    return "", ""


def _mk_player(url: str, title: str, text: str, elements: list[Element]) -> tuple[str, str]:
    low = url.lower()
    if any(k in low for k in ("/watch", "/player", "/play/")):
        return "player", f"URL 含播放路径: {low}"
    for el in elements:
        if el.tag == "video":
            return "player", "页面含 <video> 元素"
        if el.testid and any(k in el.testid.lower() for k in ("player", "video", "immersive")):
            return "player", f"testid 含播放语义: {el.testid}"
    if re.search(r"EP\s*\d", text) or "正在播放" in text:
        return "player", "正文含剧集/播放标记"
    return "", ""


def _mk_detail(url: str, title: str, text: str, elements: list[Element]) -> tuple[str, str]:
    path = urllib.parse.urlparse(url).path.strip("/")
    if len(path.split("/")) >= 2 and path.split("/")[0] in {"drama", "detail", "movie", "show", "novel"}:
        return "detail", f"URL 形态: /{path.split('/')[0]}/{{id}}"
    ids = {el.testid.lower() for el in elements if el.testid}
    if ids & {"play-cta", "detail-title", "chapter-item", "open-drawer", "detail-stage"}:
        return "detail", "页面含详情/选集 testid"
    return "", ""


def _mk_auth(url: str, title: str, text: str, elements: list[Element]) -> tuple[str, str]:
    low = url.lower()
    if any(k in low for k in ("/login", "/signin", "/auth", "/register", "/signup")):
        return "auth", f"URL 含登录路径: {low}"
    if any(k in text for k in ("登录", "第三方账号登录", "sign in", "log in")):
        return "auth", "正文含登录关键词"
    for el in elements:
        if el.testid and any(k in el.testid.lower() for k in ("login", "signin", "signup")):
            return "auth", f"testid 含登录语义: {el.testid}"
    return "", ""


def _mk_me(url: str, title: str, text: str, elements: list[Element]) -> tuple[str, str]:
    low = url.lower()
    if any(k in low for k in ("/me", "/profile", "/account", "/mine", "/user")):
        return "me", f"URL 含个人页路径: {low}"
    if any(k in text for k in ("观看历史", "我的收藏", "我的资产")):
        return "me", "正文含个人页关键词"
    ids = {el.testid.lower() for el in elements if el.testid}
    # nav-avatar 是导航入口（落地页也有），不能作为个人页证据
    if ids & {"user-center", "history-list", "profile-card"}:
        return "me", "页面含个人页 testid"
    return "", ""


def _mk_search(url: str, title: str, text: str, elements: list[Element]) -> tuple[str, str]:
    if "/search" in url.lower():
        return "search", "URL 含 /search"
    for el in elements:
        if el.tag == "input" and (el.testid or "search" in (el.text or "").lower()):
            return "search", "页面含搜索输入框"
        if el.testid and "search" in el.testid.lower():
            return "search", f"testid 含搜索语义: {el.testid}"
    return "", ""


def _mk_landing(url: str, title: str, text: str, elements: list[Element]) -> tuple[str, str]:
    path = urllib.parse.urlparse(url).path
    if path in ("", "/"):
        return "landing", "根路径"
    ids = {el.testid.lower() for el in elements if el.testid}
    if ids & {"hero-banner", "drama-card", "hero-watch", "home-grid", "top-nav"}:
        return "landing", "页面含首页/hero testid"
    return "", ""


def classify_page(url: str, title: str, text: str, elements: list[Element]) -> tuple[str, str]:
    """确定性页面类型分类：checkout > player > detail > auth > me > search > landing > other。"""
    for maker in (_mk_checkout, _mk_player, _mk_detail, _mk_auth, _mk_me, _mk_search, _mk_landing):
        ptype, reason = maker(url, title, text, elements)
        if ptype:
            return ptype, reason
    return "other", "未命中启发式规则"


# ── 链接归一化与去重（同域 BFS 边界）────────────────────────────────────────

def normalize_link(base_url: str, href: str) -> str | None:
    """同域链接归一化；外域/静态资产/协议伪链接返回 None。"""
    if not href or not href.strip():
        return None
    raw = href.strip()
    if raw.startswith(("#", "javascript:", "mailto:", "tel:", "data:")):
        return None
    base = urllib.parse.urlparse(base_url)
    try:
        parsed = urllib.parse.urlparse(urllib.parse.urljoin(base_url, raw))
    except ValueError:
        return None
    if parsed.scheme not in ("http", "https"):
        return None
    if parsed.netloc != base.netloc:
        return None
    path = urllib.parse.unquote(parsed.path or "")
    if not path:
        path = "/"  # 根路径归一化（seed 无尾斜杠 vs 链接 "/" 视为同页）
    if path.lower().endswith(tuple(ASSET_EXTS)):
        return None
    parsed = parsed._replace(path=path, fragment="")
    return urllib.parse.urlunparse(parsed)


def dedupe_links(links: list[str]) -> list[str]:
    seen: set[str] = set()
    out: list[str] = []
    for link in links:
        if link and link not in seen:
            seen.add(link)
            out.append(link)
    return out


# ── CDP 结构采集（附着调试 Chrome；不可用则降级）────────────────────────────

_CDP_EVAL = """
JSON.stringify({
  title: document.title || '',
  text: (document.body ? document.body.innerText : '').slice(0, 2000),
  links: Array.from(document.querySelectorAll('a[href]')).map(a => a.href || '').slice(0, 300),
  elements: Array.from(document.querySelectorAll(
    'button, a[href], input:not([type=hidden]), textarea, select, ' +
    '[role="button"], [role="link"], [role="menuitem"], [onclick], [data-testid]'
  )).slice(0, 120).map(el => ({
    tag: el.tagName.toLowerCase(),
    text: (el.innerText || el.value || el.getAttribute('aria-label') || el.placeholder || '')
      .replace(/\\s+/g, ' ').trim().slice(0, 60),
    href: (el.getAttribute('href') || '').slice(0, 200),
    testid: el.getAttribute('data-testid') || '',
    role: el.getAttribute('role') || ''
  }))
})
"""


def collect_via_cdp(attach: str, current_url: str) -> CollectResult:
    """经 9222 调试端点找当前标签页并采集 DOM 结构（失败抛异常，由调用方降级）。"""
    root = attach if not attach.endswith("/") else attach[:-1]
    with urllib.request.urlopen(root + "/json/list", timeout=10) as resp:
        targets = json.loads(resp.read())
    candidates = [
        t for t in targets
        if t.get("type") == "page"
        and t.get("url", "").startswith(current_url)
    ]
    if not candidates:
        # SPA 规范化后 URL 可能略有出入：回退按同 host + 同 path 前缀匹配
        base = urllib.parse.urlparse(current_url)
        candidates = [
            t for t in targets
            if t.get("type") == "page"
            and urllib.parse.urlparse(t.get("url", "")).netloc == base.netloc
            and urllib.parse.urlparse(t.get("url", "")).path == base.path
        ]
    if not candidates:
        raise RuntimeError(f"attach 端点未找到当前页面标签: {current_url}")

    import websocket  # 延迟导入：无 CDP 环境不阻塞其他能力

    ws = websocket.create_connection(candidates[-1]["webSocketDebuggerUrl"], timeout=15)
    try:
        ws.send(json.dumps({
            "id": 1,
            "method": "Runtime.evaluate",
            "params": {"expression": _CDP_EVAL, "returnByValue": True},
        }))
        while True:
            msg = json.loads(ws.recv())
            if msg.get("id") != 1:
                continue
            value = msg.get("result", {}).get("result", {}).get("value")
            if not value:
                raise RuntimeError("CDP 采集返回空")
            data = json.loads(value)
            elements = [
                Element(
                    tag=el.get("tag", ""),
                    text=el.get("text", ""),
                    href=el.get("href", ""),
                    testid=el.get("testid", ""),
                    role=el.get("role", ""),
                )
                for el in data.get("elements", [])
            ]
            return CollectResult(
                title=data.get("title", ""),
                text=data.get("text", ""),
                elements=elements,
                links=data.get("links", []),
            )
    finally:
        ws.close()


# ── 探索驱动（BFS）──────────────────────────────────────────────────────────

class ExploreDriver:
    def __init__(
        self,
        engine: EngineClient,
        options: ExploreOptions | None = None,
        collector=None,
    ) -> None:
        """collector(attach, url) -> CollectResult；默认走 CDP，测试可注入桩。"""
        self._engine = engine
        self._opts = options or ExploreOptions()
        self._collector = collector or collect_via_cdp

    def explore(self, seed_url: str) -> SiteReport:
        report = SiteReport(seed_url=seed_url)
        queue: deque[tuple[str, int]] = deque([(seed_url, 0)])
        visited: set[str] = set()
        queued: set[str] = set()
        seq = 0

        while queue and len(report.pages) < self._opts.max_pages:
            url, depth = queue.popleft()
            key = normalize_link(seed_url, url) or url
            if key in visited:
                continue
            visited.add(key)

            ctx = f"explore-{seq}"
            init = self._engine.initialize_context(
                task_id=f"explore-{ctx}",
                initial_url=key,
                context_name=ctx,
                attach_cdp_url=self._opts.attach,
            )
            if not init.ok:
                report.pages.append(PageSnapshot(
                    url=key,
                    depth=depth,
                    note=f"初始化失败: {init.error_reason or 'unknown'}",
                ))
                seq += 1
                continue
            if self._opts.settle_sec > 0:
                time.sleep(self._opts.settle_sec)
            page = self._snapshot(ctx, key, depth)
            shot = self._capture_shot(seq, key, init.target_id)
            if shot:
                page.screenshot = shot
            report.pages.append(page)
            self._close(ctx)
            seq += 1

            if depth >= self._opts.max_depth:
                continue
            page = report.pages[-1]
            for link in dedupe_links(page.links):
                norm = normalize_link(key, link)
                if not norm or norm in visited or norm in queued:
                    continue
                if len(report.pages) >= self._opts.max_pages:
                    break
                queued.add(norm)
                report.edges.append((key, norm))
                queue.append((norm, depth + 1))
        return report

    def _capture_shot(self, seq: int, url: str, target_id: str) -> str:
        """经引擎 CaptureScreenshot 截当前页，存 assets_dir；失败返回空串（不致命）。"""
        if not self._opts.capture_shots or not self._opts.assets_dir:
            return ""
        try:
            png = self._engine.capture_screenshot(target_id)
        except Exception:  # noqa: BLE001 - 截图失败降级，不影响探索
            return ""
        if not png:
            return ""
        assets = Path(self._opts.assets_dir)
        assets.mkdir(parents=True, exist_ok=True)
        slug = _slug(url, seq)
        name = f"{seq:02d}-{slug}.png"
        (assets / name).write_bytes(png)
        return name

    def _snapshot(self, ctx: str, url: str, depth: int) -> PageSnapshot:
        note = ""
        try:
            collected = self._collector(self._opts.attach, url)
        except Exception as exc:  # noqa: BLE001 - CDP 降级不影响探索主流程
            note = f"CDP 结构采集不可用（{type(exc).__name__}）；仅 URL 级快照"
            collected = CollectResult()
        page_type, reason = classify_page(url, collected.title, collected.text, collected.elements)
        self_key = normalize_link(url, url) or url
        return PageSnapshot(
            url=url,
            depth=depth,
            page_type=page_type,
            type_reason=reason,
            title=collected.title,
            elements=collected.elements,
            links=dedupe_links([
                norm for l in collected.links
                if (norm := normalize_link(url, l)) is not None and norm != self_key
            ]),
            note=note,
        )

    def _close(self, ctx: str) -> None:
        try:
            self._engine.close_context(context_name=ctx)
        except Exception:  # noqa: BLE001, S110 - 关闭幂等，失败不致命
            pass


# ── 报告渲染（Markdown + Mermaid 导航图）───────────────────────────────────

def _label(url: str, page_type: str) -> str:
    path = urllib.parse.urlparse(url).path or "/"
    short = path if len(path) <= 40 else path[:37] + "..."
    return f"{short} ({page_type})"


def render_mermaid(report: SiteReport) -> str:
    """Mermaid 导航图：节点 = 已探索页面，边 = 同域链接发现。"""
    index = {page.url: f"N{i}" for i, page in enumerate(report.pages)}
    lines = ["```mermaid", "graph LR"]
    for page in report.pages:
        node = index[page.url]
        label = _label(page.url, page.page_type).replace('"', "'")
        lines.append(f'  {node}["{label}"]')
    for src, dst in report.edges:
        if src in index and dst in index:
            lines.append(f"  {index[src]} --> {index[dst]}")
        else:
            # 未展开节点（超页数上限）：画成菱形占位
            lines.append(f"  {index.get(src, '?')} -. {_label(dst, 'unvisited')} .-")
    lines.append("```")
    return "\n".join(lines)


def render_markdown(report: SiteReport) -> str:
    lines: list[str] = []
    lines.append(f"# 站点探索报告：{report.seed_url}")
    lines.append("")
    lines.append(f"- 页面数: {len(report.pages)} ｜ 发现边: {len(report.edges)}")
    lines.append(f"- 引擎: UWS ｜ 模式: 附着 CDP ｜ 生成时间: {time.strftime('%Y-%m-%d %H:%M')}")
    lines.append("")

    lines.append("## 导航图")
    lines.append("")
    lines.append(render_mermaid(report))
    lines.append("")

    lines.append("## 页面清单")
    lines.append("")
    for i, page in enumerate(report.pages, 1):
        lines.append(f"### {i}. {_label(page.url, page.page_type)}")
        lines.append("")
        if page.screenshot and report.assets_rel:
            lines.append(f"![{_label(page.url, page.page_type)}]({report.assets_rel}/{page.screenshot})")
            lines.append("")
        lines.append(f"- URL: `{page.url}`")
        lines.append(f"- 深度: {page.depth} ｜ 类型: **{page.page_type}**（{page.type_reason or '启发式未命中'}）")
        if page.title:
            lines.append(f"- 标题: {page.title}")
        if page.note:
            lines.append(f"- 备注: {page.note}")
        if page.elements:
            lines.append("- 关键元素:")
            for el in page.elements[:20]:
                testid = f" testid={el.testid}" if el.testid else ""
                href = f" → {el.href}" if el.href else ""
                lines.append(f"  - `{el.tag}`「{el.text or '(无文本)'}」{testid}{href}")
        if page.links:
            lines.append("- 同域链接:")
            for link in page.links[:15]:
                lines.append(f"  - {link}")
        lines.append("")

    types: dict[str, list[str]] = {}
    for page in report.pages:
        for el in page.elements:
            if not (el.text or el.testid):
                continue
            item = el.testid if el.testid else el.text
            if item not in types.setdefault(page.page_type, []):
                types[page.page_type].append(item)
    lines.append("## 功能清单（按页面类型聚合）")
    lines.append("")
    if not types:
        lines.append("（结构采集不可用，无功能清单）")
    for ptype, items in types.items():
        lines.append(f"### {ptype}")
        for item in items[:30]:
            lines.append(f"- {item}")
        lines.append("")

    lines.append("## 启发式说明与限制")
    lines.append("")
    lines.append("- 页面类型 = URL 路径 + testid + 正文关键词的确定性规则（不调 VLM）。")
    lines.append("- 导航图仅覆盖同域链接可达的页面；SPA 内部路由若不可直接以 URL 打开则不会被发现。")
    lines.append("- 未登录/游客态可能缺失登录后页面；iframe 与 Shadow DOM 内元素暂不采集。")
    return "\n".join(lines).rstrip() + "\n"


def _slug(url: str, seq: int) -> str:
    """从 URL 路径取稳定文件名片段（去标点，超长截断）。"""
    path = urllib.parse.urlparse(url).path.strip("/")
    seg = (path.split("/")[-1] if path else "home")
    clean = re.sub(r"[^A-Za-z0-9_-]+", "-", seg).strip("-") or f"page-{seq}"
    return clean[:40]
