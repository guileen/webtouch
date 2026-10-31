"""CLI E2E 测试（TDD-CLI-01：uws run --attach / CLI 执行闭环）。

端到端验证：
1. uws run 接收 --task-file 与 --output
2. 驱动 Coordinator Executor 逐步执行并在任务结束后落盘结构化 result.md
3. 退出码 0，Markdown 内容包含执行结果与步骤耗时摘要
"""

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
if str(ROOT / "proto/gen/python") not in sys.path:
    sys.path.insert(0, str(ROOT / "proto/gen/python"))

from coordinator.cli import create_parser, run_task


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_tdd_cli_01_run_task_and_output_result(tmp_path: Path):
    binary = tmp_path / "uws-engine"
    keys_file = tmp_path / "keys.json"
    keys_file.write_text(json.dumps({"keys": [{"key": "run-test-key", "role": "coordinator"}]}))

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
    grpc_addr = f"127.0.0.1:{grpc_port}"

    proc = subprocess.Popen(
        [
            str(binary),
            f"-grpc-addr={grpc_addr}",
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
        task_file = tmp_path / "my_task.json"
        task_file.write_text(
            json.dumps({
                "plan": [
                    {
                        "step_id": "step-1",
                        "action_type": "input",
                        "target_semantic": "search_input",
                        "value": "webtouch crawler test",
                    },
                    {
                        "step_id": "step-2",
                        "action_type": "click",
                        "target_semantic": "search_button",
                    },
                ]
            }),
            encoding="utf-8",
        )
        out_file = tmp_path / "result.md"

        parser = create_parser()
        args = parser.parse_args([
            "run",
            "--target-url", "http://mock-search.local",
            "--task-file", str(task_file),
            "--output", str(out_file),
            "--engine-addr", grpc_addr,
            "--api-key", "run-test-key",
        ])

        code = run_task(args)
        assert code == 0
        assert out_file.exists()

        content = out_file.read_text(encoding="utf-8")
        assert "# Task Execution Result:" in content
        assert "Status: **SUCCEEDED**" in content
        assert "step-1" in content
        assert "step-2" in content

    finally:
        proc.terminate()
        try:
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proc.kill()
