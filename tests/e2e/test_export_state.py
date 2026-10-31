import subprocess
import time
import grpc
import os
import shutil
import tempfile
import socket
import pytest
import json

from uws_service_pb2 import ActionIntentRequest, InitContextRequest, ExportStateRequest
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
        if p: return p
    raise RuntimeError("未找到 Chrome")


def _start_engine() -> tuple[subprocess.Popen, int, str]:
    tmp = tempfile.mkdtemp(prefix="uws-e2e-engine-")
    binary = os.path.join(tmp, "uws-engine")
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


def test_export_state():
    engine, engine_port, engine_tmp = _start_engine()
    try:
        with grpc.insecure_channel(f"127.0.0.1:{engine_port}") as ch:
            stub = UwsEngineStub(ch)
            init = stub.InitializeContext(
                InitContextRequest(
                    task_id="e2e-export-1",
                    initial_url="https://example.com",
                    viewport=dict(width=1280, height=800),
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert init.ok
            
            res = stub.ExportState(
                ExportStateRequest(
                    target_id=init.target_id,
                    target_origin="https://example.com"
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
            data = json.loads(res.cookies_json)
            # example.com might not have cookies by default, but it returns a valid JSON array.
            assert isinstance(data, list)
    finally:
        engine.terminate()
        try:
            engine.wait(timeout=10)
        except subprocess.TimeoutExpired:
            engine.kill()
        shutil.rmtree(engine_tmp, ignore_errors=True)
