"""tests 根级共享配置。

共享夹具规划见 docs/tdd-checklist.md §0.5 与 tests/fixtures/README.md：
Mock CDP / Mock VLM / Mock 破解服务 / Mock 博客站点 / 黄金样本集。
当前注入：proto/gen/python 到 sys.path（生成桩为扁平模块导入，无包命名空间）。
"""

import json
import os
import shutil
import socket
import subprocess
import tempfile
import time
from pathlib import Path

import sys

import grpc
import pytest

PROTO_PY = Path(__file__).resolve().parents[1] / "proto" / "gen" / "python"
if str(PROTO_PY) not in sys.path:
    sys.path.insert(0, str(PROTO_PY))

from uws_service_pb2 import Empty  # noqa: E402 - proto 桩路径注入后导入
from uws_service_pb2_grpc import UwsEngineStub  # noqa: E402

ROOT = Path(__file__).resolve().parents[1]

COORD_KEY = "coordinator-test-key"
OPERATOR_KEY = "operator-test-key"
MONITOR_KEY = "monitor-test-key"


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def channel(port: int) -> grpc.Channel:
    return grpc.insecure_channel(f"127.0.0.1:{port}")


@pytest.fixture(scope="session")
def engine():
    """构建并启动真实 uws-engine 二进制（一次性，供全部契约/集成用例共享）。"""
    keys = [
        {"key": COORD_KEY, "role": "coordinator"},
        {"key": OPERATOR_KEY, "role": "operator"},
        {"key": MONITOR_KEY, "role": "monitor"},
    ]
    tmp = Path(tempfile.mkdtemp(prefix="uws-engine-test-"))
    keyfile = tmp / "keys.json"
    keyfile.write_text(json.dumps({"keys": keys}, ensure_ascii=False))
    binary = tmp / "uws-engine"
    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    env.setdefault("GOPATH", os.path.join(tmp, "gopath"))
    gomodcache = subprocess.check_output(["go", "env", "GOMODCACHE"], text=True).strip()
    env.setdefault("GOMODCACHE", gomodcache)
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/uws-engine"],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    if build.returncode != 0:
        pytest.fail(f"go build 失败:\n{build.stdout}\n{build.stderr}")

    port = _free_port()
    proc = subprocess.Popen(
        [str(binary), "--grpc-addr", f"127.0.0.1:{port}", "--api-key-file", str(keyfile)],
        cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )

    deadline = time.time() + 30
    last_err = None
    while time.time() < deadline:
        if proc.poll() is not None:
            break
        try:
            with channel(port) as ch:
                stub = UwsEngineStub(ch)
                stub.HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),), timeout=1)
            break
        except Exception as exc:  # noqa: BLE001 - 启动竞态，重试直到就绪
            last_err = exc
            time.sleep(0.3)
    else:
        out, err = proc.communicate(timeout=5)
        pytest.fail(f"引擎未就绪: last_err={last_err}\nstdout={out}\nstderr={err}")
    if proc.poll() is not None:
        out, err = proc.communicate(timeout=5)
        pytest.fail(f"引擎进程提前退出: {out}\n{err}")

    yield {"port": port, "tmp": tmp}

    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
    shutil.rmtree(tmp, ignore_errors=True)
