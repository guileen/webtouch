"""PII 脱敏安全测试（TDD-SEC-01/03，tdd-checklist.md §5）。

双层验证：
- 实现层（Go）：pkg/engine/logstore_test.go（脱敏单元 + SQLite 往返）。
- 本文件（契约层）：真实 uws-engine 二进制 + `--log-path` SQLite 落库，
  断言 online_logs 中敏感字段值已替换 [REDACTED]、pii_masked=true、
  node_features/snapshot/decision_trace 全树无明文且仅含长度/类型元信息。

说明：规格（algorithm-kernel.md §4.4）写 snapshot_blob 为 zstd 压缩；当前实现
以明文 JSON 落库（体积小、便于审计），zstd 压缩待接入压缩依赖后补齐
（见 USER_ACTION.md），本用例按明文读取 blob 并断言脱敏语义。
"""

import json
import os
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import time
from pathlib import Path

import grpc
import pytest

from uws_service_pb2 import ActionIntentRequest, Empty, InitContextRequest
from uws_service_pb2_grpc import UwsEngineStub

ROOT = Path(__file__).resolve().parents[2]

COORD_KEY = "coordinator-test-key"
MONITOR_KEY = "monitor-test-key"


def _channel(port: int) -> grpc.Channel:
    return grpc.insecure_channel(f"127.0.0.1:{port}")


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="module")
def engine_with_logs():
    """构建并启动真实 uws-engine（内置知识库 + --log-path），返回 gRPC 端口与日志库路径。"""
    tmp = Path(tempfile.mkdtemp(prefix="uws-sec-log-"))
    log_path = tmp / "online_logs.db"
    keyfile = tmp / "keys.json"
    keyfile.write_text(json.dumps({
        "keys": [
            {"key": COORD_KEY, "role": "coordinator"},
            {"key": MONITOR_KEY, "role": "monitor"},
        ]
    }))
    binary = tmp / "uws-engine"
    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/uws-engine"],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    if build.returncode != 0:
        pytest.fail(f"go build 失败:\n{build.stdout}\n{build.stderr}")

    port = _free_port()
    proc = subprocess.Popen(
        [
            str(binary),
            "--grpc-addr", f"127.0.0.1:{port}",
            "--api-key-file", str(keyfile),
            "--log-path", str(log_path),
        ],
        cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )
    deadline = time.time() + 30
    last_err = None
    while time.time() < deadline:
        if proc.poll() is not None:
            break
        try:
            with _channel(port) as ch:
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

    yield {"port": port, "log_path": log_path}

    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
    shutil.rmtree(tmp, ignore_errors=True)


def _rows(log_path: Path, intent_id: str) -> list[dict]:
    con = sqlite3.connect(log_path)
    try:
        cur = con.execute(
            "SELECT request_id, node_features, snapshot_blob, snapshot_size, decision_trace, pii_masked "
            "FROM online_logs WHERE request_id = ?",
            (intent_id,),
        )
        cols = [d[0] for d in cur.description]
        return [dict(zip(cols, row)) for row in cur.fetchall()]
    finally:
        con.close()


def _fill(engine, semantic: str, value: str, intent_id: str) -> None:
    with _channel(engine["port"]) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(
            InitContextRequest(task_id="sec-pii"),
            metadata=(("x-api-key", COORD_KEY),),
        )
        resp = stub.ExecuteAction(
            ActionIntentRequest(
                intent_id=intent_id,
                target_semantic=semantic,
                expected_action="input",
                value=value,
            ),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert resp.success, f"{semantic} 动作应成功: {resp}"


def _assert_no_plaintext(row: dict, secrets: list[str]) -> None:
    joined = " ".join([
        row["node_features"] or "",
        (row["snapshot_blob"] or b"").decode("utf-8", "replace"),
        row["decision_trace"] or "",
    ])
    for secret in secrets:
        assert secret not in joined, f"明文 {secret!r} 残留于 online_logs: {joined}"


def test_tdd_sec_01_password_redacted_in_logs(engine_with_logs):
    """input[type=password] 语义的值 → [REDACTED] + pii_masked=true；
    pwd/pass/ssn/phone/id_card 字段级扩展同样生效。"""
    cases = {
        "password_input": "mySecret123",
        "pwd": "pwd-secret",
        "pass": "pass-secret",
        "ssn_input": "110101199001011234",
        "phone_input": "13800138000",
        "id_card": "440101200001011234",
    }
    for i, (semantic, value) in enumerate(cases.items()):
        # intent_id 不得内嵌语义词，否则断言「语义明文不残留」会被 intent_id 误伤。
        intent_id = f"sec01-{i}"
        _fill(engine_with_logs, semantic, value, intent_id)

        rows = _rows(engine_with_logs["log_path"], intent_id)
        assert len(rows) == 1, f"{semantic} 应恰好落 1 条日志: {rows}"
        row = rows[0]
        assert row["pii_masked"] == 1, f"{semantic} pii_masked 应为 true"
        blob = (row["snapshot_blob"] or b"").decode("utf-8", "replace")
        assert "[REDACTED]" in blob, f"{semantic} 快照应含 [REDACTED]: {blob}"
        _assert_no_plaintext(row, [value, semantic])


def test_tdd_sec_03_snapshot_full_tree_redaction(engine_with_logs):
    """node_features / snapshot_blob 全树无明文；decision_trace 仅保留长度/类型元信息。"""
    value = "mySecret123"
    semantic = "password_input"
    intent_id = "sec03-1"
    _fill(engine_with_logs, semantic, value, intent_id)

    rows = _rows(engine_with_logs["log_path"], intent_id)
    assert len(rows) == 1
    row = rows[0]

    _assert_no_plaintext(row, [value, semantic])

    # node_features 仅元信息：不允许出现任何原始文本/语义字段。
    nf = json.loads(row["node_features"])
    assert isinstance(nf, list) and nf, "node_features 应为非空 JSON 数组"
    for item in nf:
        assert isinstance(item, dict)
        for k in item:
            assert k in {"candidate_count", "confidence"}, f"node_features 出现非元信息键: {k}"

    # decision_trace 仅长度/类型元信息：不得携带 target_semantic / value 原文。
    dt = json.loads(row["decision_trace"])
    assert isinstance(dt, dict)
    assert "target_semantic" not in dt and "value" not in dt and "semantic" not in dt
    assert "node_text_lengths" in dt and "confidence_type" in dt

    # snapshot_blob：合法 JSON，全树扫描无明文（zstd 压缩待依赖决策，当前明文落库）。
    snap = json.loads((row["snapshot_blob"] or b"").decode("utf-8"))
    snap_text = json.dumps(snap, ensure_ascii=False)
    assert value not in snap_text and semantic not in snap_text
    assert snap["value"] == "[REDACTED]" and snap["semantic"] == "[REDACTED]"


def test_r2_value_never_persisted_plaintext(engine_with_logs):
    """R2 回归：非敏感命名字段（username）填入手机号，快照仍不得落明文——
    PII 脱敏不依赖语义标签关键词，value 结构性禁止明文入库。"""
    semantic = "username_input"
    value = "13800138000"
    intent_id = "r2-phone-in-username"
    _fill(engine_with_logs, semantic, value, intent_id)

    rows = _rows(engine_with_logs["log_path"], intent_id)
    assert len(rows) == 1
    row = rows[0]
    _assert_no_plaintext(row, [value])
    assert row["pii_masked"] == 1, "含 value 输入即应 pii_masked=true"
    snap = json.loads((row["snapshot_blob"] or b"").decode("utf-8"))
    assert snap["value"] == "[REDACTED]"
    assert snap["semantic"] == semantic, "非敏感语义标签本身保留（审计可读）"
