"""协调器爬虫 E2E（TDD-COORD-05，Mock 博客站真实 CDP，2026-08-20 续24）。

执行条件：``UWS_E2E_CDP=1``（真实 Chrome + 非沙盒），与 test_engine_attach.py
同门控。

user case 链路（对应 tdd-checklist §4 / docs/user-cases.md 场景二）：
  1. 本地 Mock 博客站：首页含 `main_article`（正文）+ `author_name`（作者）
     + 侧边栏 `ad_banner`（广告）+ 「下一页」链接；第二页含正文/作者 + 推广块；
  2. 引擎 --browser=cdp 启动真实 Chrome；CrawlDriver 进入种子页；
  3. 逐页 extract signals → Markdown + YAML Frontmatter；
  4. 「下一页」点击深入第二层（max_depth=2）；
  5. 验收：输出含正文与作者、不含侧边栏广告/推广文本，服务端确实收到 /page2.html。

TDD-COORD-08（WARC 离线）仍为骨架（依赖离线蒸馏产物，另期点亮）。
"""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import grpc
import pytest

from coordinator.crawler import CrawlDriver, CrawlOptions
from coordinator.grpc_client import GrpcEngineClient
from uws_service_pb2 import Empty
from uws_service_pb2_grpc import UwsEngineStub

ROOT = Path(__file__).resolve().parents[2]
COORD_KEY = "coordinator-test-key"

pytestmark = pytest.mark.skipif(
    os.environ.get("UWS_E2E_CDP") != "1",
    reason="需要 UWS_E2E_CDP=1 且真实 Chrome（非沙盒）",
)


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _chrome_binary() -> str:
    env = os.environ.get("CHROME_PATH")
    if env and Path(env).exists():
        return env
    mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    if Path(mac).exists():
        return mac
    for name in ("google-chrome", "chromium", "chromium-browser", "chrome"):
        p = shutil.which(name)
        if p:
            return p
    raise RuntimeError("未找到 Chrome（设置 CHROME_PATH）")


class _BlogServer:
    """两层 Mock 博客：首页（正文+作者+广告+下一页）→ 第二页（正文+作者+推广，无下一页）。"""

    INDEX = """<!DOCTYPE html>
<html><body>
  <h1>测试博客</h1>
  <article data-testid="main_article">首页正文：欢迎访问测试博客，这里是第一层内容。</article>
  <span data-testid="author_name">作者甲</span>
  <aside data-testid="ad_banner">广告：限时特惠买买买</aside>
  <a href="/page2.html" data-testid="next_link">下一页</a>
</body></html>"""

    PAGE2 = """<!DOCTYPE html>
<html><body>
  <h1>第二页</h1>
  <article data-testid="main_article">第二层正文：深入一层的文章内容。</article>
  <span data-testid="author_name">作者乙</span>
  <div data-testid="ad_banner">推广：点击了解更多</div>
</body></html>"""

    def __init__(self) -> None:
        self.hits: list[str] = []

        class Handler(BaseHTTPRequestHandler):
            blog = self
            protocol_version = "HTTP/1.0"  # 无 keep-alive：避免 shutdown 被挂起连接阻塞

            def do_GET(self):  # noqa: N802
                self.blog.hits.append(self.path)
                body = self.blog.PAGE2 if self.path == "/page2.html" else self.blog.INDEX
                payload = body.encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, *args):  # noqa: D401 - 静默
                pass

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.httpd.server_address[1]
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}/"

    def close(self) -> None:
        self.httpd.shutdown()


def _start_engine() -> tuple[subprocess.Popen, int]:
    tmp = Path(tempfile.mkdtemp(prefix="uws-e2e-crawl-"))
    binary = tmp / "uws-engine"
    keys = [{"key": COORD_KEY, "role": "coordinator"}]
    (tmp / "keys.json").write_text(json.dumps({"keys": keys}))
    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    env.setdefault("GOPATH", os.path.join(tmp, "gopath"))
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/uws-engine"],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    if build.returncode != 0:
        raise RuntimeError(f"go build 失败:\n{build.stdout}\n{build.stderr}")
    port = _free_port()
    log_path = Path("/tmp/uws-e2e-crawl-engine.log")
    logf = open(log_path, "w")
    proc = subprocess.Popen(
        [str(binary), "--grpc-addr", f"127.0.0.1:{port}",
         "--api-key-file", str(tmp / "keys.json"),
         "--browser", "cdp", "--chrome-path", _chrome_binary()],
        cwd=ROOT, stdout=logf, stderr=subprocess.STDOUT,
    )
    deadline = time.time() + 30
    while time.time() < deadline:
        if proc.poll() is not None:
            logf.close()
            raise RuntimeError("引擎提前退出")
        try:
            with grpc.insecure_channel(f"127.0.0.1:{port}") as ch:
                stub = UwsEngineStub(ch)
                stub.HealthCheck(Empty(), metadata=(("x-api-key", COORD_KEY),), timeout=1)
            return proc, port
        except grpc.RpcError:  # noqa: PERF203 - 启动竞态重试
            time.sleep(0.3)
    logf.close()
    raise RuntimeError(f"引擎未就绪（日志: {log_path}）")


def test_tdd_coord_05_crawl_mock_blog():
    """真实 CDP 全链路：种子页 → 提取正文/作者 → 下一页深入 → Markdown 输出、无广告。"""
    server = _BlogServer()
    engine, engine_port = _start_engine()
    try:
        client = GrpcEngineClient(port=engine_port, api_key=COORD_KEY)
        driver = CrawlDriver(
            client,
            CrawlOptions(
                signals=["main_article", "author_name"],
                negative_keywords=["广告", "推广"],
                max_depth=2,
            ),
        )
        records = driver.crawl(server.url)

        assert len(records) == 2, f"应爬取 2 层, got {len(records)}: {records}"
        assert records[0].fields.get("main_article", "").startswith("首页正文"), records[0]
        assert records[0].fields.get("author_name") == "作者甲"
        assert records[1].fields.get("main_article", "").startswith("第二层正文"), records[1]
        assert records[1].fields.get("author_name") == "作者乙"

        corpus = "\n".join(r.markdown for r in records)
        for forbidden in ("广告", "推广", "买买买", "了解更多"):
            assert forbidden not in corpus, f"输出不应包含广告文本: {forbidden}"
        assert "作者甲" in records[0].markdown and "作者乙" in records[1].markdown
        assert records[0].markdown.startswith("---") and records[1].markdown.startswith("---")

        # 真实导航证据：第二页确实被访问。
        deadline = time.time() + 10
        while time.time() < deadline and "/page2.html" not in server.hits:
            time.sleep(0.2)
        assert "/page2.html" in server.hits, f"未访问第二页, hits={server.hits}"
    finally:
        engine.terminate()
        engine.wait(timeout=5)
        server.close()
