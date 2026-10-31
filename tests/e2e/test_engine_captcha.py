"""引擎挑战拦截真实 CDP E2E（TDD-ENG-05）。

执行条件：``UWS_E2E_CDP=1``（真实 Chrome + 非沙盒）。

确定性验证：本地服务器返回 403 + cf-chl 挑战头（与真实 CF 挑战页同构），
引擎对普通动作返回 gRPC UNAUTHENTICATED（= CHALLENGE_INTERCEPTED）；
drag 坐标直发绕过挑战门禁（TDD-COORD-04 解锁原语）。
真实外部站点（chatgpt.com）的 CF 策略随环境变化（headless Chrome 可能被直接放行），
不作为确定性门禁；ENG-05 拦截分支以本地确定性 E2E 为准。
"""

import subprocess
import time
import grpc
import os
import shutil
import tempfile
import socket
import threading
import pytest
from http.server import BaseHTTPRequestHandler, HTTPServer

from uws_service_pb2 import ActionIntentRequest, InitContextRequest
from uws_service_pb2_grpc import UwsEngineStub

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "../.."))
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
    if env and os.path.exists(env):
        return env
    mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    if os.path.exists(mac):
        return mac
    for name in ("google-chrome", "chromium", "chromium-browser", "chrome"):
        p = shutil.which(name)
        if p:
            return p
    raise RuntimeError("未找到 Chrome")


def _start_engine() -> tuple[subprocess.Popen, int, str]:
    tmp = tempfile.mkdtemp(prefix="uws-e2e-engine-")
    binary = os.path.join(tmp, "uws-engine")
    import json

    with open(os.path.join(tmp, "keys.json"), "w") as f:
        json.dump({"keys": [{"key": COORD_KEY, "role": "coordinator"}]}, f)

    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    build = subprocess.run(
        ["go", "build", "-o", binary, "./cmd/uws-engine"],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    if build.returncode != 0:
        raise RuntimeError(f"go build 失败:\n{build.stdout}\n{build.stderr}")

    port = _free_port()
    proc = subprocess.Popen(
        [binary, "--grpc-addr", f"127.0.0.1:{port}",
         "--api-key-file", os.path.join(tmp, "keys.json"),
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
                from uws_service_pb2 import Empty

                stub.HealthCheck(Empty(), metadata=(("x-api-key", COORD_KEY),), timeout=1)
            return proc, port, tmp
        except grpc.RpcError as exc:
            last = exc
            time.sleep(0.3)
    raise RuntimeError(f"引擎未就绪: {last}")


class _ChallengeServer:
    """本地挑战页：403 + CF-Chl 头（hasChallengeHeader 命中 → challenge 状态）。"""

    def __init__(self) -> None:
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):  # noqa: N802
                body = """<!DOCTYPE html><html><body><h1>challenge</h1>
<input type="range" id="slider"
  style="position:absolute;left:100px;top:192px;width:200px;height:16px">
</body></html>""".encode("utf-8")
                self.send_response(403)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("CF-Chl-Bypass", "1")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):  # noqa: D401
                pass

        self.httpd = HTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.httpd.server_address[1]
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}/"

    def close(self) -> None:
        self.httpd.shutdown()


def test_tdd_eng_05_captcha_intercept():
    server = _ChallengeServer()
    engine, engine_port, engine_tmp = _start_engine()
    try:
        with grpc.insecure_channel(f"127.0.0.1:{engine_port}") as ch:
            stub = UwsEngineStub(ch)
            init = stub.InitializeContext(
                InitContextRequest(
                    task_id="e2e-captcha-1",
                    initial_url=server.url,
                    viewport=dict(width=1280, height=800),
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert init.ok

            # 挑战页上的普通动作 → gRPC UNAUTHENTICATED（= CHALLENGE_INTERCEPTED）。
            try:
                stub.ExecuteAction(
                    ActionIntentRequest(
                        intent_id="act-fail-1",
                        target_semantic="click anything",
                        expected_action="click",
                    ),
                    metadata=(("x-api-key", COORD_KEY),),
                )
                raise AssertionError("challenge 页面 ExecuteAction 应抛 UNAUTHENTICATED")
            except grpc.RpcError as exc:
                assert exc.code() == grpc.StatusCode.UNAUTHENTICATED, exc
                assert "challenge intercepted" in exc.details(), exc.details()

            # drag 坐标直发必须绕过挑战门禁（VLM 解锁原语）。
            res_drag = stub.ExecuteAction(
                ActionIntentRequest(
                    intent_id="act-drag",
                    target_semantic="bypass",
                    expected_action="drag",
                    value="200,200,300,200",
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert res_drag.success, "drag must be allowed even during challenge"

    finally:
        engine.terminate()
        try:
            engine.wait(timeout=10)
        except subprocess.TimeoutExpired:
            engine.kill()
        shutil.rmtree(engine_tmp, ignore_errors=True)
        server.close()
