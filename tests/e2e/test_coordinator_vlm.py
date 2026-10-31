"""协调器 VLM 打码真实 CDP E2E（TDD-COORD-04，v1.5.1 引擎原语）。

执行条件：``UWS_E2E_CDP=1``（真实 Chrome + 非沙盒），与 test_engine_attach.py 同门控。

链路（对应 spec/coordinator.md §6.1 / USER_ACTION.md #9）：
  1. 本地挑战页：403 + CF 挑战头 + range 滑块（MockVlmSolver 坐标 200,200→300,200）；
  2. 引擎 --browser=cdp 启动真实 Chrome，InitializeContext(initial_url=挑战页)；
  3. ExecuteAction(登录) → UNAUTHENTICATED（引擎映射 CHALLENGE_INTERCEPTED）；
  4. CaptureScreenshot → PNG；MockVlmSolver 求解 → ExecuteAction drag 坐标直发；
  5. 滑块拖满 → /solve → location.reload() → 服务端转 200（挑战状态随 Document 响应清除）；
  6. 原步骤「登录」重试成功（物理点击真实生效）。
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

from coordinator.vlm import MockVlmSolver, VlmSolution
from uws_service_pb2 import (
    ActionIntentRequest,
    CaptureScreenshotRequest,
    Empty,
    InitContextRequest,
)
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


class _ChallengeSliderServer:
    """本地挑战页：先 403 + CF 挑战头 + 滑块；/solve 后转 200 + 登录按钮。

    滑块绝对定位在 (100,192) 起、宽 200px，初值 50（thumb 中心 200,200），
    与 MockVlmSolver 坐标 (200,200)→(300,200) 对齐。mouseup 时 value≥95 → fetch('/solve') + reload。
    """

    def __init__(self) -> None:
        self.solved = False
        self.clicks = 0
        self.lock = threading.Lock()

        class Handler(BaseHTTPRequestHandler):
            server_state = self

            def do_GET(self):  # noqa: N802
                if self.path == "/solve":
                    with self.server_state.lock:
                        self.server_state.solved = True
                    self.send_response(204)
                    self.end_headers()
                    return
                if self.path == "/clicked":
                    with self.server_state.lock:
                        self.server_state.clicks += 1
                    self.send_response(204)
                    self.end_headers()
                    return
                with self.server_state.lock:
                    solved = self.server_state.solved
                if solved:
                    body = """<!DOCTYPE html>
<html><body>
  <button id="login" onclick="fetch('/clicked')">登录</button>
</body></html>""".encode("utf-8")
                    self.send_response(200)
                    self.send_header("Content-Type", "text/html; charset=utf-8")
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                    return
                body = """<!DOCTYPE html>
<html><body>
  <div style="position:absolute;left:80px;top:180px;width:240px;height:60px;
       background:#fff;border:1px solid #ccc;z-index:1"></div>
  <input type="range" id="slider" min="0" max="100" value="50"
    style="position:absolute;left:100px;top:192px;width:200px;height:16px;z-index:2">
  <script>
    document.getElementById('slider').addEventListener('mouseup', function () {
      if (this.value >= 95) { fetch('/solve').then(function () { location.reload(); }); }
    });
  </script>
</body></html>""".encode("utf-8")
                self.send_response(403)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                # hasChallengeHeader 命中 "cf-chl"：引擎判定为 WAF/验证码挑战。
                self.send_header("CF-Chl-Bypass", "1")
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


def _start_engine() -> tuple[subprocess.Popen, int, str]:
    tmp = Path(tempfile.mkdtemp(prefix="uws-e2e-vlm-"))
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
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError("引擎提前退出")
        try:
            with grpc.insecure_channel(f"127.0.0.1:{port}") as ch:
                stub = UwsEngineStub(ch)
                stub.HealthCheck(Empty(), metadata=(("x-api-key", COORD_KEY),), timeout=1)
            return proc, port, str(tmp)
        except grpc.RpcError:  # noqa: PERF203 - 启动竞态重试
            time.sleep(0.3)
    raise RuntimeError("引擎未就绪")


def test_tdd_coord_04_vlm_visual_solve():
    """真实 CDP 全链路：挑战拦截 → 截图 → VLM 坐标 → drag 解锁 → 原步骤重试。"""
    server = _ChallengeSliderServer()
    engine, engine_port, _ = _start_engine()
    solver = MockVlmSolver(VlmSolution(x1=200, y1=200, x2=300, y2=200))
    try:
        with grpc.insecure_channel(f"127.0.0.1:{engine_port}") as ch:
            stub = UwsEngineStub(ch)
            init = stub.InitializeContext(
                InitContextRequest(
                    task_id="e2e-vlm-1",
                    initial_url=server.url,
                    viewport=dict(width=800, height=600),
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert init.ok and init.target_id

            # ① 挑战页上的普通动作 → UNAUTHENTICATED（CHALLENGE_INTERCEPTED）。
            with pytest.raises(grpc.RpcError) as ei:
                stub.ExecuteAction(
                    ActionIntentRequest(
                        intent_id="e2e-vlm-login-1",
                        target_semantic="登录",
                        expected_action="click",
                    ),
                    metadata=(("x-api-key", COORD_KEY),),
                )
            assert ei.value.code() == grpc.StatusCode.UNAUTHENTICATED

            # ② CaptureScreenshot → 真实 PNG。
            shot = stub.CaptureScreenshot(
                CaptureScreenshotRequest(target_id=init.target_id),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert shot.png.startswith(b"\x89PNG")
            assert shot.width > 0 and shot.height > 0

            # ③ Mock VLM 求解 → drag 坐标直发（bypass 语义匹配与挑战门禁）。
            solution = solver.solve(shot.png, "challenge_slider")
            drag = stub.ExecuteAction(
                ActionIntentRequest(
                    intent_id="e2e-vlm-drag-1",
                    target_semantic="challenge_slider",
                    expected_action="drag",
                    value=solution.coords(),
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert drag.success

            # ④ 滑块拖满 → /solve → reload 200（挑战状态清除）。
            deadline = time.time() + 10
            while time.time() < deadline and not server.solved:
                time.sleep(0.2)
            assert server.solved, "拖拽未触发 /solve"

            # ⑤ 原步骤「登录」重试：页面 reload 竞态下轮询直至物理点击真实生效。
            deadline = time.time() + 15
            attempt = 0
            clicked = False
            while time.time() < deadline:
                attempt += 1
                try:
                    act = stub.ExecuteAction(
                        ActionIntentRequest(
                            intent_id=f"e2e-vlm-login-retry-{attempt}",
                            target_semantic="登录",
                            expected_action="click",
                        ),
                        metadata=(("x-api-key", COORD_KEY),),
                    )
                    if act.success and server.clicks >= 1:
                        clicked = True
                        break
                except grpc.RpcError:
                    pass  # 页面仍在 reload/未稳定，下一轮重试
                time.sleep(0.4)
            assert clicked, "解锁后原步骤重试未成功（物理点击未生效）"
    finally:
        engine.terminate()
        engine.wait(timeout=5)
        server.close()
