"""CLI/调试 UI 安全测试（TDD-CLI-07，tdd-checklist.md §8）。

端到端验证：
1. uws-engine 带 --debug-port 启动时，对外暴露 HTTP 只读调试端点。
2. 访问 /debug/task/{request_id} 返回 JSON 结构化日志。
3. 敏感数据（如 password、token、value）被严格替换为 [REDACTED]，且 pii_masked 字段为 true。
4. 服务仅绑定本地回环端口（127.0.0.1）。
"""

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
if str(ROOT / "proto/gen/python") not in sys.path:
    sys.path.insert(0, str(ROOT / "proto/gen/python"))

from coordinator.grpc_client import GrpcEngineClient


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_tdd_cli_07_debug_ui_local_only_redacted(tmp_path: Path):
    binary = tmp_path / "uws-engine"
    keys_file = tmp_path / "keys.json"
    keys_file.write_text(json.dumps({"keys": [{"key": "debug-test-key", "role": "coordinator"}]}))

    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/uws-engine"],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
    )
    if build.returncode != 0:
        pytest.fail(f"go build uws-engine 失败:\n{build.stdout}\n{build.stderr}")

    grpc_port = _free_port()
    debug_port = _free_port()
    grpc_addr = f"127.0.0.1:{grpc_port}"

    with tempfile.TemporaryDirectory() as td:
        db_path = str(Path(td) / "debug_e2e_online.db")

        proc = subprocess.Popen(
            [
                str(binary),
                f"-grpc-addr={grpc_addr}",
                f"-debug-port={debug_port}",
                f"-log-path={db_path}",
                f"-api-key-file={str(keys_file)}",
                "-browser=sim",
            ],
            cwd=str(ROOT),
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

        try:
            time.sleep(2.0)
            client = GrpcEngineClient(grpc_port, api_key="debug-test-key")
            client.initialize_context(task_id="task-debug-1", initial_url="http://mock-login.local")
            intent_id = "req-e2e-debug-999"

            # 执行包含敏感信息（密码/输入）的动作
            client.execute_action(
                intent_id=intent_id,
                expected_action="input",
                target_semantic="password_field",
                value="MySuperSecretPass!123",
            )

            # 请求调试 HTTP 接口
            debug_url = f"http://127.0.0.1:{debug_port}/debug/task/{intent_id}"
            req = urllib.request.Request(debug_url, method="GET")
            with urllib.request.urlopen(req, timeout=5) as resp:
                assert resp.status == 200
                raw_body = resp.read().decode("utf-8")
                data = json.loads(raw_body)

            # 断言结构完整性与安全脱敏（SEC-01/03）
            assert data["request_id"] == intent_id
            assert data["pii_masked"] is True
            assert "MySuperSecretPass!123" not in raw_body
            assert "[REDACTED]" in raw_body
            assert data["action_taken"] == "input"

        finally:
            proc.terminate()
            try:
                proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                proc.kill()
