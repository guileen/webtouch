"""引擎 E2E 测试（TDD-ENG-08/12/13，tdd-checklist.md §3.1/3.2/3.3，需要真实 Chromium + 非沙盒）。

执行条件：`UWS_E2E_CDP=1`（且环境可启动 Chrome 远程调试端口）。
ENG-08 附着模式已点亮：attach 本地 Chrome 新建标签页、共享登录态、不破坏原会话。
ENG-13 启动模式已点亮：点击 target=_blank → NEW_WINDOW_OPENED，原 Target 可继续执行。
ENG-12 无头检测依赖外网 bot.sannysoft.com，留待真实部署环境接线。
"""

import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import grpc
import pytest

from uws_service_pb2 import ActionIntentRequest, ContextState, Empty, InitContextRequest
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


class _ClickServer:
    """本地测试页：按钮点击后 fetch /clicked 计数（验证物理点击真实生效）。"""

    def __init__(self) -> None:
        self.clicks = 0
        self.lock = threading.Lock()

        class Handler(BaseHTTPRequestHandler):
            server_state = self

            def do_GET(self):  # noqa: N802
                if self.path == "/clicked":
                    with self.server_state.lock:
                        self.server_state.clicks += 1
                    self.send_response(204)
                    self.end_headers()
                    return
                body = """<!DOCTYPE html>
<html><body>
  <button id="login" onclick="fetch('/clicked')">登录</button>
  <input id="user" placeholder="用户名">
</body></html>""".encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):  # noqa: D401 - 静默
                pass

        self.httpd = HTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.httpd.server_address[1]
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}/"

    def close(self) -> None:
        self.httpd.shutdown()


class _NewWindowServer:
    """本地测试页：target=_blank 链接 + 普通按钮（验证 NEW_WINDOW_OPENED 后原 Target 可用）。"""

    def __init__(self) -> None:
        self.clicks = 0
        self.lock = threading.Lock()

        class Handler(BaseHTTPRequestHandler):
            server_state = self

            def do_GET(self):  # noqa: N802
                if self.path == "/clicked":
                    with self.server_state.lock:
                        self.server_state.clicks += 1
                    self.send_response(204)
                    self.end_headers()
                    return
                body = """<!DOCTYPE html>
<html><body>
  <button id="login" onclick="fetch('/clicked')">登录</button>
  <a href="/next" id="newtab" target="_blank">新标签</a>
</body></html>""".encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):  # noqa: D401 - 静默
                pass

        self.httpd = HTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.httpd.server_address[1]
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}/"

    def close(self) -> None:
        self.httpd.shutdown()


def _start_chrome() -> tuple[subprocess.Popen, str, str]:
    port = _free_port()
    profile = tempfile.mkdtemp(prefix="uws-e2e-cdp-")
    proc = subprocess.Popen(
        [_chrome_binary(),
         "--headless=new", "--disable-gpu", "--no-first-run",
         "--disable-crash-reporter", "--disable-component-update",
         "--disable-background-networking",
         f"--remote-debugging-port={port}",
         "--remote-debugging-address=127.0.0.1",
         f"--user-data-dir={profile}",
         "about:blank"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    deadline = time.time() + 15
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                return proc, f"http://127.0.0.1:{port}", profile
        except OSError:
            time.sleep(0.2)
    proc.kill()
    raise RuntimeError("Chrome DevTools 端点未就绪")


def _start_engine() -> tuple[subprocess.Popen, int, str]:
    tmp = Path(tempfile.mkdtemp(prefix="uws-e2e-engine-"))
    binary = tmp / "uws-engine"
    keys = [{"key": COORD_KEY, "role": "coordinator"}]
    (tmp / "keys.json").write_text(json.dumps({"keys": keys}))
    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/uws-engine"],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    if build.returncode != 0:
        raise RuntimeError(f"go build 失败:\n{build.stdout}\n{build.stderr}")
    port = _free_port()
    proc = subprocess.Popen(
        [str(binary), "--grpc-addr", f"127.0.0.1:{port}",
         "--api-key-file", str(tmp / "keys.json"),
         "--browser", "cdp", "--chrome-path", _chrome_binary()],
        cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    deadline = time.time() + 30
    last = None
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError("引擎提前退出")
        try:
            with grpc.insecure_channel(f"127.0.0.1:{port}") as ch:
                stub = UwsEngineStub(ch)
                stub.HealthCheck(Empty(), metadata=(("x-api-key", COORD_KEY),), timeout=1)
            return proc, port, str(tmp)
        except grpc.RpcError as exc:  # noqa: PERF203 - 启动竞态重试
            last = exc
            time.sleep(0.3)
    raise RuntimeError(f"引擎未就绪: {last}")


def _page_targets(cdp_url: str) -> set[str]:
    raw = json.loads(subprocess.check_output(
        ["curl", "-s", cdp_url + "/json/list"], text=True))
    return {t["id"] for t in raw if t.get("type") == "page"}


def _wait_targets_settle(cdp_url: str, timeout: float = 5.0) -> set[str]:
    """等待 Chrome target 列表稳定（启动期存在瞬态 target，早于稳定会误判）。"""
    deadline = time.time() + timeout
    prev: set[str] | None = None
    while time.time() < deadline:
        cur = _page_targets(cdp_url)
        if cur == prev:
            return cur
        prev = cur
        time.sleep(0.3)
    return prev or _page_targets(cdp_url)


def test_tdd_eng_08_attach_cdp_url():
    """附着本地 Chrome：新建标签页（不破坏原会话）、共享登录态、物理点击真实生效。"""
    clicks = _ClickServer()
    chrome, cdp_url, profile = _start_chrome()
    engine, engine_port, engine_tmp = _start_engine()
    try:
        # 记录附着前的 page target 基线（等列表稳定，剔除启动瞬态 target）。
        before_ids = _wait_targets_settle(cdp_url)

        with grpc.insecure_channel(f"127.0.0.1:{engine_port}") as ch:
            stub = UwsEngineStub(ch)
            init = stub.InitializeContext(
                InitContextRequest(
                    task_id="e2e-attach-1",
                    attach_cdp_url=cdp_url,
                    initial_url=clicks.url,
                    viewport=dict(width=1280, height=800),
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert init.ok and init.target_id and init.context_id

            action = stub.ExecuteAction(
                ActionIntentRequest(intent_id="e2e-attach-click-1",
                                    target_semantic="登录", expected_action="click"),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert action.success

        # 物理点击真实生效：页面按钮 onclick fetch('/clicked')。
        deadline = time.time() + 5
        while time.time() < deadline:
            if clicks.clicks >= 1:
                break
            time.sleep(0.2)
        assert clicks.clicks >= 1, "附着会话点击未真实生效"

        # 不破坏原会话：attach 新建了标签页，原 target 仍在。
        after_ids = _page_targets(cdp_url)
        assert before_ids <= after_ids, "原 target 不应被销毁"
        assert len(after_ids) > len(before_ids), "attach 应新建标签页"
    finally:
        engine.terminate()
        try:
            engine.wait(timeout=10)
        except subprocess.TimeoutExpired:
            engine.kill()
        shutil.rmtree(engine_tmp, ignore_errors=True)
        chrome.kill()
        try:
            chrome.wait(timeout=10)
        except subprocess.TimeoutExpired:
            pass
        shutil.rmtree(profile, ignore_errors=True)
        clicks.close()


def test_tdd_eng_12_headless_detection():
    pytest.skip("skeleton: TDD-ENG-12 依赖外网 bot.sannysoft.com（有头/无头检测页），待真实部署环境接线")


def test_tdd_eng_13_new_window_opened():
    """启动模式：点击 target=_blank → NEW_WINDOW_OPENED，原 Target 仍可继续执行。"""
    srv = _NewWindowServer()
    engine, engine_port, engine_tmp = _start_engine()
    try:
        with grpc.insecure_channel(f"127.0.0.1:{engine_port}") as ch:
            stub = UwsEngineStub(ch)
            init = stub.InitializeContext(
                InitContextRequest(
                    task_id="e2e-new-window-1",
                    initial_url=srv.url,
                    viewport=dict(width=1280, height=800),
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert init.ok and init.target_id

            opened = stub.ExecuteAction(
                ActionIntentRequest(intent_id="e2e-new-window-1",
                                    target_semantic="新标签", expected_action="click"),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert opened.success
            assert opened.context_state == ContextState.NEW_WINDOW_OPENED

            # 原 Target 仍可用：后续点击真实生效。
            again = stub.ExecuteAction(
                ActionIntentRequest(intent_id="e2e-new-window-2",
                                    target_semantic="登录", expected_action="click"),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert again.success
            assert again.context_state == ContextState.STABLE

        deadline = time.time() + 5
        while time.time() < deadline:
            if srv.clicks >= 1:
                break
            time.sleep(0.2)
        assert srv.clicks >= 1, "原 target 点击未真实生效"
    finally:
        engine.terminate()
        try:
            engine.wait(timeout=10)
        except subprocess.TimeoutExpired:
            engine.kill()
        shutil.rmtree(engine_tmp, ignore_errors=True)
        srv.close()
