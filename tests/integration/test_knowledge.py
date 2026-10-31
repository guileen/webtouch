"""知识库热更新/回滚 gRPC 集成测试（TDD-KB-01..07，tdd-checklist.md §3.4）。

双层验证：
- 内置内存 fetcher（零配置 engine fixture）：KB-01（dry_run 不切换 + 未知版本
  fetch_failed）、KB-03（ONNX 未就绪 → sanity_check_failed 保旧版）。
- 真实 HTTP 对象存储 fetcher（http_engine fixture，pipeline/exporter 产物 +
  本地 HTTP 服务 + `--knowledge-base-url`）：KB-02（原子切换）、KB-04（内存超限
  RESOURCE_EXHAUSTED）、KB-05（版本兼容 + force 跳过）、KB-06（P0 自动回滚：
  连续 50×NOT_FOUND → 回滚 + HealthCheck 标记）、KB-07（回滚 RPC 全链路；
  ACTIVE→STAGING 一致性由 Go 实现层断言）。
"""

import http.server
import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
import time
from pathlib import Path

import grpc
import pytest

from pipeline.exporter.exporter import Exporter
from pipeline.llm_distiller.distiller import DistilledRule
from uws_service_pb2 import ActionIntentRequest, Empty, InitContextRequest, ReloadRequest
from uws_service_pb2_grpc import UwsEngineStub

OPERATOR_KEY = "operator-test-key"
MONITOR_KEY = "monitor-test-key"
COORD_KEY = "coordinator-test-key"

ROOT = Path(__file__).resolve().parents[2]


def _channel(port: int) -> grpc.Channel:
    return grpc.insecure_channel(f"127.0.0.1:{port}")


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _build_engine(binary: Path) -> None:
    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    env.setdefault("GOPATH", os.path.join(binary.parent, "gopath"))
    gomodcache = subprocess.check_output(["go", "env", "GOMODCACHE"], text=True).strip()
    env.setdefault("GOMODCACHE", gomodcache)
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/uws-engine"],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    if build.returncode != 0:
        pytest.fail(f"go build 失败:\n{build.stdout}\n{build.stderr}")


def _wait_ready(port: int, proc: subprocess.Popen) -> None:
    deadline = time.time() + 30
    last_err = None
    while time.time() < deadline:
        if proc.poll() is not None:
            break
        try:
            with _channel(port) as ch:
                stub = UwsEngineStub(ch)
                stub.HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),), timeout=1)
            return
        except Exception as exc:  # noqa: BLE001 - 启动竞态，重试直到就绪
            last_err = exc
            time.sleep(0.3)
    out, err = proc.communicate(timeout=5)
    pytest.fail(f"引擎未就绪: last_err={last_err}\nstdout={out}\nstderr={err}")


def _base_rules(extra_active: tuple[str, ...] = ()) -> list[DistilledRule]:
    rules = [
        DistilledRule(text_pattern="登录", action="click", confidence=0.95),
        DistilledRule(text_pattern="提交", action="click", confidence=0.9),
    ]
    for pattern in extra_active:
        rules.append(DistilledRule(text_pattern=pattern, action="click", confidence=0.8, status="ACTIVE"))
    return rules


def _build_artifacts(root: Path) -> None:
    """产出 v1.3.9 / v1.4.0 / v1.4.1 / v9.9.0 四版 artifact（exporter 真实产物）。"""
    exporter = Exporter(root)
    exporter.export(
        "v1.3.9",
        rules=_base_rules(),
        min_engine_version="v1.3.0",
        description="baseline",
    )
    exporter.export(
        "v1.4.0",
        rules=_base_rules(extra_active=("register_now",)),
        negative_keywords=["banner"],
        min_engine_version="v1.4.0",
        description="adds ACTIVE L2 rule",
    )
    exporter.export(
        "v1.4.1",
        rules=_base_rules(),
        min_engine_version="v9.9.9",
        description="incompatible version",
    )
    exporter.export(
        "v1.4.2",
        rules=_base_rules(),
        min_engine_version="v1.3.0",
        max_engine_version="v1.4.x",
        description="max version below engine",
    )
    exporter.export(
        "v9.9.0",
        rules=_base_rules(),
        min_engine_version="v1.4.0",
        deploy_metadata={"memory_estimate_mb": 500},
        description="oversized artifact",
    )


@pytest.fixture(scope="module")
def http_engine():
    """真实 HTTP fetcher 引擎：exporter 产物目录 + 本地 HTTP 服务 + --knowledge-base-url。"""
    tmp = Path(tempfile.mkdtemp(prefix="uws-kb-artifacts-"))
    try:
        _build_artifacts(tmp)

        handler = lambda *args, **kwargs: http.server.SimpleHTTPRequestHandler(  # noqa: E731
            *args, directory=str(tmp), **kwargs
        )
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        base_url = f"http://127.0.0.1:{server.server_address[1]}"

        keys = [
            {"key": OPERATOR_KEY, "role": "operator"},
            {"key": MONITOR_KEY, "role": "monitor"},
            {"key": COORD_KEY, "role": "coordinator"},
        ]
        keyfile = tmp / "keys.json"
        keyfile.write_text(json.dumps({"keys": keys}, ensure_ascii=False))
        binary = tmp / "uws-engine"
        _build_engine(binary)

        port = _free_port()
        proc = subprocess.Popen(
            [
                str(binary),
                "--grpc-addr", f"127.0.0.1:{port}",
                "--api-key-file", str(keyfile),
                "--knowledge-base-url", base_url,
                "--knowledge-version", "v1.3.9",
            ],
            cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        _wait_ready(port, proc)
        if proc.poll() is not None:
            out, err = proc.communicate(timeout=5)
            pytest.fail(f"引擎进程提前退出: {out}\n{err}")

        yield {"port": port}

        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        server.shutdown()
        server.server_close()
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def _stub(engine) -> UwsEngineStub:
    return UwsEngineStub(_channel(engine["port"]))


def _kv(stub: UwsEngineStub):
    return stub.GetKnowledgeVersion(Empty(), metadata=(("x-api-key", MONITOR_KEY),))


def _reload(stub: UwsEngineStub, version: str, *, dry_run=False, force=False):
    return stub.ReloadKnowledge(
        ReloadRequest(version=version, dry_run=dry_run, force=force),
        metadata=(("x-api-key", OPERATOR_KEY),),
    )


def _ensure_version(stub: UwsEngineStub, version: str) -> None:
    if _kv(stub).version == version:
        return
    resp = _reload(stub, version)
    assert resp.ok, f"基线切换 {version} 失败: {resp.reason}"


# ── 内置 fetcher：KB-01 / KB-03（零配置 engine fixture） ─────────────────

def test_tdd_kb_01_dry_run_no_switch(engine):
    """dry_run 仅下载校验：版本与 schema/model 哈希均不变；未知版本 fetch 失败。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        before = _kv(stub)

        resp = _reload(stub, before.version, dry_run=True)
        assert resp.ok
        assert resp.active_version == before.version

        after = _kv(stub)
        assert (after.version, after.schema_hash, after.model_hash) == (
            before.version,
            before.schema_hash,
            before.model_hash,
        )

        missing = _reload(stub, "v9.9.9", dry_run=True)
        assert not missing.ok
        assert "artifact not found" in missing.reason
        assert _kv(stub).version == before.version


def test_tdd_kb_03_sanity_check_fail_keeps_old(engine):
    """内置知识库 ONNX 未就绪 → 正式切换返回 sanity_check_failed，旧版本保持有效。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        before = _kv(stub)

        resp = _reload(stub, before.version)  # 非 dry-run
        assert not resp.ok
        assert resp.reason == "sanity_check_failed"
        assert _kv(stub).version == before.version


# ── 真实 HTTP fetcher：KB-02 / KB-04 / KB-05 / KB-07 ─────────────────────

def test_tdd_kb_02_atomic_switch(http_engine):
    """正式切换：active/previous 正确，哈希与 GetKnowledgeVersion 一致。"""
    stub = _stub(http_engine)
    _ensure_version(stub, "v1.3.9")
    before = _kv(stub)

    resp = _reload(stub, "v1.4.0")
    assert resp.ok, resp.reason
    assert resp.active_version == "v1.4.0"
    assert resp.previous_version == "v1.3.9"
    assert resp.switch_time_ms >= 0

    after = _kv(stub)
    assert after.version == "v1.4.0"
    assert after.model_hash != before.model_hash  # exporter 占位模型按版本生成
    assert after.schema_hash == before.schema_hash  # SCHEMA 常量不变


def test_tdd_kb_04_memory_limit_reject(http_engine):
    """manifest 声明内存 500MB → 加载被拒 RESOURCE_EXHAUSTED，当前版本不变。"""
    stub = _stub(http_engine)
    _ensure_version(stub, "v1.3.9")

    resp = _reload(stub, "v9.9.0")
    assert not resp.ok
    assert "RESOURCE_EXHAUSTED" in resp.reason
    assert _kv(stub).version == "v1.3.9"


def test_tdd_kb_05_version_compatibility_force(http_engine):
    """min_engine_version 高于引擎 → 拒绝；force=true 跳过校验完成切换。"""
    stub = _stub(http_engine)
    _ensure_version(stub, "v1.3.9")

    resp = _reload(stub, "v1.4.1")
    assert not resp.ok
    assert resp.reason == "version_incompatible"
    assert _kv(stub).version == "v1.3.9"

    resp = _reload(stub, "v1.4.1", force=True)
    assert resp.ok, resp.reason
    assert resp.active_version == "v1.4.1"
    assert resp.previous_version == "v1.3.9"
    assert _kv(stub).version == "v1.4.1"


def test_tdd_kb_05b_max_engine_version_reject(http_engine):
    """max_engine_version 低于引擎（v1.4.x < v1.5.0）→ 拒绝；force=true 跳过。"""
    stub = _stub(http_engine)
    _ensure_version(stub, "v1.3.9")

    resp = _reload(stub, "v1.4.2")
    assert not resp.ok
    assert resp.reason == "version_incompatible"
    assert _kv(stub).version == "v1.3.9"

    resp = _reload(stub, "v1.4.2", force=True)
    assert resp.ok, resp.reason
    assert resp.active_version == "v1.4.2"
    assert _kv(stub).version == "v1.4.2"


def test_tdd_kb_07_rollback_consistency_l2_status(http_engine):
    """回滚 RPC 全链路：v1.4.0 → v1.3.9 切换成功，版本/哈希一致。

    ACTIVE→STAGING 一致性修复（新版本独有 ACTIVE 规则降级）由 Go 实现层
    （knowledge_test.go / m5_integration_test.go）断言；本用例验证真实
    对象存储产物下回滚路径的完整可执行性。
    """
    stub = _stub(http_engine)
    _ensure_version(stub, "v1.3.9")

    up = _reload(stub, "v1.4.0")
    assert up.ok, up.reason
    assert _kv(stub).version == "v1.4.0"

    back = _reload(stub, "v1.3.9")
    assert back.ok, back.reason
    assert back.active_version == "v1.3.9"
    assert back.previous_version == "v1.4.0"
    assert _kv(stub).version == "v1.3.9"


def test_tdd_kb_06_auto_rollback_p0(http_engine):
    """P0 自动回滚：连续 50 次 NOT_FOUND → 自动回滚到上一稳定版本（<1 分钟），
    HealthCheck 附带回滚标记（rolled_back + reason）。"""
    stub = _stub(http_engine)
    _ensure_version(stub, "v1.3.9")
    up = _reload(stub, "v1.4.0")
    assert up.ok, up.reason
    assert _kv(stub).version == "v1.4.0"

    with _channel(http_engine["port"]) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(
            InitContextRequest(task_id="kb6"),
            metadata=(("x-api-key", COORD_KEY),),
        )

        def not_found(i: int):
            with pytest.raises(grpc.RpcError) as ei:
                stub.ExecuteAction(
                    ActionIntentRequest(
                        intent_id=f"kb6-nf-{i}",
                        target_semantic="not_found",
                        expected_action="click",
                    ),
                    metadata=(("x-api-key", COORD_KEY),),
                )
            assert ei.value.code() == grpc.StatusCode.NOT_FOUND

        # 49 次连续 NOT_FOUND：版本不变；成功动作重置失明计数。
        for i in range(49):
            not_found(i)
        assert _kv(stub).version == "v1.4.0"

        stub.ExecuteAction(
            ActionIntentRequest(
                intent_id="kb6-ok",
                target_semantic="登录",
                expected_action="click",
            ),
            metadata=(("x-api-key", COORD_KEY),),
        )
        for i in range(49):
            not_found(100 + i)
        assert _kv(stub).version == "v1.4.0"

        # 重置后累计满 50 次 → 自动回滚到上一稳定版本 + 回滚标记。
        not_found(200)
        kv = _kv(stub)
        assert kv.version == "v1.3.9", f"应自动回滚到 v1.3.9: {kv.version}"

        hc = stub.HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),))
        assert hc.rolled_back, "HealthCheck 应附带回滚标记"
        assert hc.rollback_reason == "auto_rollback v1.4.0 → v1.3.9", hc.rollback_reason


# ── R1：知识库热更→匹配管线接线（真实 HTTP artifact 双层验证） ─────────

def test_r1_hot_reload_changes_matching(http_engine):
    """热更改变匹配行为：matcher 随 KB active 版本重建（USER_ACTION.md 审查 R1）。

    - v1.3.9 无 register_now 规则 → classifier mock 0.90 执行；
    - v1.4.0 含 register_now 规则（conf 0.8）+ banner 黑名单 → 命中规则 0.80、
      banner_login 被黑名单拦截 NOT_FOUND；
    - 回滚 v1.3.9 → 行为还原（register_now 回 0.90、banner_login 放行）。
    """
    stub = _stub(http_engine)
    _ensure_version(stub, "v1.3.9")

    with _channel(http_engine["port"]) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(
            InitContextRequest(task_id="r1-hot"),
            metadata=(("x-api-key", COORD_KEY),),
        )

        def run(semantic: str, intent_id: str):
            return stub.ExecuteAction(
                ActionIntentRequest(
                    intent_id=intent_id, target_semantic=semantic, expected_action="click"
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )

        # 热更前：register_now / banner_login 均走 classifier 0.90。
        pre = run("register_now", "r1-pre-reg")
        assert pre.success
        assert pre.confidence == pytest.approx(0.90, abs=1e-6)
        pre_ban = run("banner_login", "r1-pre-ban")
        assert pre_ban.success
        assert pre_ban.confidence == pytest.approx(0.90, abs=1e-6)

        # 热更 v1.4.0：register_now 规则（0.8）+ banner 黑名单。
        resp = _reload(stub, "v1.4.0")
        assert resp.ok, resp.reason

        post = run("register_now", "r1-post-reg")
        assert post.success
        assert post.confidence == pytest.approx(0.80, abs=1e-6)
        with pytest.raises(grpc.RpcError) as ei:
            run("banner_login", "r1-post-ban")
        assert ei.value.code() == grpc.StatusCode.NOT_FOUND

        # 回滚 v1.3.9：行为还原。
        back = _reload(stub, "v1.3.9")
        assert back.ok, back.reason
        rolled = run("register_now", "r1-roll-reg")
        assert rolled.success
        assert rolled.confidence == pytest.approx(0.90, abs=1e-6)
        roll_ban = run("banner_login", "r1-roll-ban")
        assert roll_ban.success
        assert roll_ban.confidence == pytest.approx(0.90, abs=1e-6)
