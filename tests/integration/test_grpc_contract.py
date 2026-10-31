"""gRPC 契约集成测试（TDD-API-01..07，tdd-checklist.md §7）。

CI 门禁命令：pytest tests/integration/test_grpc_contract.py
实现层：pkg/engine（Go）+ cmd/uws-engine；本文件启动真实引擎二进制做契约层验证。
验收以实现层（Go）为准。
"""

import json
import os
import shutil
import socket
import subprocess
import tempfile
import time
from pathlib import Path

import grpc
import pytest

from uws_service_pb2 import (
    ActionIntentRequest,
    CaptureScreenshotRequest,
    Empty,
    ExportPageTreeRequest,
    InitContextRequest,
    ReloadRequest,
    ViewportConfig,
)
from uws_service_pb2_grpc import UwsEngineStub

ROOT = Path(__file__).resolve().parents[2]

COORD_KEY = "coordinator-test-key"
OPERATOR_KEY = "operator-test-key"
MONITOR_KEY = "monitor-test-key"


def _channel(port: int) -> grpc.Channel:
    return grpc.insecure_channel(f"127.0.0.1:{port}")


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _openssl(*args: str) -> None:
    subprocess.run(
        ["openssl", *args],
        check=True,
        capture_output=True,
        text=True,
    )


def _generate_mtls_pki(dest: Path) -> dict[str, Path]:
    """本地生成测试 PKI：根 CA + 服务端证书（SAN 127.0.0.1）+ 合法客户端 + 伪造 CA 客户端。

    TDD-API-05 的证书夹具（2026-08-20 起本地自生成，不再依赖外部证书基建）。
    """
    dest.mkdir(parents=True, exist_ok=True)
    # 根 CA（自签名）
    _openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
             "-subj", "/CN=uws-test-ca",
             "-keyout", str(dest / "ca.key"), "-out", str(dest / "ca.crt"))
    # 服务端证书：SAN 必须含 127.0.0.1，否则 gRPC 客户端主机名校验失败
    (dest / "server.ext").write_text("subjectAltName=DNS:localhost,IP:127.0.0.1\n")
    _openssl("req", "-newkey", "rsa:2048", "-nodes",
             "-subj", "/CN=localhost",
             "-keyout", str(dest / "server.key"), "-out", str(dest / "server.csr"))
    _openssl("x509", "-req", "-in", str(dest / "server.csr"),
             "-CA", str(dest / "ca.crt"), "-CAkey", str(dest / "ca.key"),
             "-CAcreateserial", "-days", "2", "-extfile", str(dest / "server.ext"),
             "-out", str(dest / "server.crt"))
    # 合法客户端证书（根 CA 签发）
    _openssl("req", "-newkey", "rsa:2048", "-nodes",
             "-subj", "/CN=uws-client",
             "-keyout", str(dest / "client.key"), "-out", str(dest / "client.csr"))
    _openssl("x509", "-req", "-in", str(dest / "client.csr"),
             "-CA", str(dest / "ca.crt"), "-CAkey", str(dest / "ca.key"),
             "-CAcreateserial", "-days", "2",
             "-out", str(dest / "client.crt"))
    # 伪造 CA + 伪造客户端（根 CA 不认可，用于「无效证书被拒」）
    _openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
             "-subj", "/CN=uws-rogue-ca",
             "-keyout", str(dest / "rogue-ca.key"), "-out", str(dest / "rogue-ca.crt"))
    _openssl("req", "-newkey", "rsa:2048", "-nodes",
             "-subj", "/CN=rogue-client",
             "-keyout", str(dest / "rogue-client.key"), "-out", str(dest / "rogue-client.csr"))
    _openssl("x509", "-req", "-in", str(dest / "rogue-client.csr"),
             "-CA", str(dest / "rogue-ca.crt"), "-CAkey", str(dest / "rogue-ca.key"),
             "-CAcreateserial", "-days", "2",
             "-out", str(dest / "rogue-client.crt"))
    return {
        "ca": dest / "ca.crt",
        "server_cert": dest / "server.crt",
        "server_key": dest / "server.key",
        "client_cert": dest / "client.crt",
        "client_key": dest / "client.key",
        "rogue_cert": dest / "rogue-client.crt",
        "rogue_key": dest / "rogue-client.key",
    }


# engine 由 tests/conftest.py 提供：每个用例独立启动模拟引擎。


def test_tdd_api_01_lifecycle_precondition(engine):
    """未 InitializeContext 直接 ExecuteAction → FAILED_PRECONDITION。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(
                ActionIntentRequest(intent_id="i1", target_semantic="登录", expected_action="click"),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.FAILED_PRECONDITION

        resp = stub.InitializeContext(
            InitContextRequest(task_id="task-1"), metadata=(("x-api-key", COORD_KEY),)
        )
        assert resp.ok and resp.target_id and resp.context_id

        action = stub.ExecuteAction(
            ActionIntentRequest(intent_id="i1", target_semantic="登录", expected_action="click"),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert action.success


def test_tdd_api_02_idempotency_ttl(engine):
    """同 intent_id 在 60s TTL 内返回相同缓存结果（decision_trace_id 不变）。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(InitContextRequest(task_id="task-2"), metadata=(("x-api-key", COORD_KEY),))
        req = ActionIntentRequest(intent_id="idem-1", target_semantic="登录", expected_action="click")
        r1 = stub.ExecuteAction(req, metadata=(("x-api-key", COORD_KEY),))
        r2 = stub.ExecuteAction(req, metadata=(("x-api-key", COORD_KEY),))
        assert r1 == r2
        assert r1.decision_trace_id

        r3 = stub.ExecuteAction(
            ActionIntentRequest(intent_id="idem-2", target_semantic="登录", expected_action="click"),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert r3.decision_trace_id != r1.decision_trace_id


def test_tdd_api_03_execute_action_stream_order(engine):
    """双向流：请求与响应一一对应、顺序一致。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(InitContextRequest(task_id="task-3"), metadata=(("x-api-key", COORD_KEY),))

        def gen():
            for i in range(1, 6):
                yield ActionIntentRequest(
                    intent_id=f"s-{i}", target_semantic="登录", expected_action="click"
                )

        stream = stub.ExecuteActionStream(gen(), metadata=(("x-api-key", COORD_KEY),))
        received = [resp.intent_id for resp in stream]
        assert received == [f"s-{i}" for i in range(1, 6)]


def test_tdd_api_04_api_key_authz(engine):
    """无 Key/非法 Key → UNAUTHENTICATED；角色越权 → PERMISSION_DENIED。"""
    port = engine["port"]
    req = ActionIntentRequest(intent_id="a-1", target_semantic="登录", expected_action="click")
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)

        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(req)
        assert ei.value.code() == grpc.StatusCode.UNAUTHENTICATED

        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(req, metadata=(("x-api-key", "bad-key"),))
        assert ei.value.code() == grpc.StatusCode.UNAUTHENTICATED

        for key in (MONITOR_KEY, OPERATOR_KEY):
            with pytest.raises(grpc.RpcError) as ei:
                stub.ExecuteAction(req, metadata=(("x-api-key", key),))
            assert ei.value.code() == grpc.StatusCode.PERMISSION_DENIED

        with pytest.raises(grpc.RpcError) as ei:
            stub.ReloadKnowledge(
                ReloadRequest(version="v1.5.5", dry_run=True),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.PERMISSION_DENIED

        reload_resp = stub.ReloadKnowledge(
            ReloadRequest(version="v1.5.5", dry_run=True),
            metadata=(("x-api-key", OPERATOR_KEY),),
        )
        assert reload_resp.ok and reload_resp.active_version == "v1.5.5"


@pytest.fixture(scope="module")
def mtls_engine():
    """以 mTLS 模式启动真实引擎（--tls-cert/--tls-key/--tls-ca），TDD-API-05 专用。"""
    tmp = Path(tempfile.mkdtemp(prefix="uws-mtls-test-"))
    pki = _generate_mtls_pki(tmp / "pki")
    keyfile = tmp / "keys.json"
    keyfile.write_text(json.dumps({
        "keys": [
            {"key": COORD_KEY, "role": "coordinator"},
            {"key": OPERATOR_KEY, "role": "operator"},
            {"key": MONITOR_KEY, "role": "monitor"},
        ]
    }, ensure_ascii=False))
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
            str(binary), "--grpc-addr", f"127.0.0.1:{port}",
            "--api-key-file", str(keyfile),
            "--tls-cert", str(pki["server_cert"]),
            "--tls-key", str(pki["server_key"]),
            "--tls-ca", str(pki["ca"]),
        ],
        cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )

    ca_pem = pki["ca"].read_bytes()
    valid_creds = grpc.ssl_channel_credentials(
        root_certificates=ca_pem,
        private_key=pki["client_key"].read_bytes(),
        certificate_chain=pki["client_cert"].read_bytes(),
    )
    deadline = time.time() + 30
    last_err = None
    while time.time() < deadline:
        if proc.poll() is not None:
            break
        try:
            with grpc.secure_channel(f"127.0.0.1:{port}", valid_creds) as ch:
                stub = UwsEngineStub(ch)
                stub.HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),), timeout=1)
            break
        except Exception as exc:  # noqa: BLE001 - 启动竞态，重试直到就绪
            last_err = exc
            time.sleep(0.3)
    else:
        out, err = proc.communicate(timeout=5)
        pytest.fail(f"mTLS 引擎未就绪: last_err={last_err}\nstdout={out}\nstderr={err}")
    if proc.poll() is not None:
        out, err = proc.communicate(timeout=5)
        pytest.fail(f"mTLS 引擎进程提前退出: {out}\n{err}")

    yield {"port": port, "pki": pki}

    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
    shutil.rmtree(tmp, ignore_errors=True)


def _assert_transport_rejected(call):
    """握手/传输层拒绝：gRPC 侧表现为 UNAVAILABLE 或 DEADLINE_EXCEEDED。"""
    with pytest.raises(grpc.RpcError) as ei:
        call()
    assert ei.value.code() in (grpc.StatusCode.UNAVAILABLE, grpc.StatusCode.DEADLINE_EXCEEDED)


def test_tdd_api_05_mtls_requires_client_cert(mtls_engine):
    """开启 mTLS（--tls-ca）后：无证书连接在传输层被拒绝。"""
    port = mtls_engine["port"]
    # 纯明文（无 TLS）→ 拒绝
    _assert_transport_rejected(
        lambda: UwsEngineStub(grpc.insecure_channel(f"127.0.0.1:{port}"))
        .HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),), timeout=2)
    )
    # 仅服务端信任（无客户端证书）→ 握手拒绝
    ca_pem = mtls_engine["pki"]["ca"].read_bytes()
    server_only = grpc.ssl_channel_credentials(root_certificates=ca_pem)
    _assert_transport_rejected(
        lambda: UwsEngineStub(grpc.secure_channel(f"127.0.0.1:{port}", server_only))
        .HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),), timeout=2)
    )


def test_tdd_api_05_mtls_rejects_rogue_client(mtls_engine):
    """持有非受信 CA 签发的客户端证书 → 传输层拒绝（RequireAndVerifyClientCert）。"""
    port = mtls_engine["port"]
    pki = mtls_engine["pki"]
    rogue = grpc.ssl_channel_credentials(
        root_certificates=pki["ca"].read_bytes(),
        private_key=pki["rogue_key"].read_bytes(),
        certificate_chain=pki["rogue_cert"].read_bytes(),
    )
    _assert_transport_rejected(
        lambda: UwsEngineStub(grpc.secure_channel(f"127.0.0.1:{port}", rogue))
        .HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),), timeout=2)
    )


def test_tdd_api_05_mtls_valid_client(mtls_engine):
    """合法客户端证书通过 mTLS 握手；应用层鉴权仍生效（无 Key → UNAUTHENTICATED）。"""
    port = mtls_engine["port"]
    pki = mtls_engine["pki"]
    creds = grpc.ssl_channel_credentials(
        root_certificates=pki["ca"].read_bytes(),
        private_key=pki["client_key"].read_bytes(),
        certificate_chain=pki["client_cert"].read_bytes(),
    )
    with grpc.secure_channel(f"127.0.0.1:{port}", creds) as ch:
        stub = UwsEngineStub(ch)
        hc = stub.HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),), timeout=5)
        assert hc.ok and hc.version == "v1.5.5"
        with pytest.raises(grpc.RpcError) as ei:
            stub.HealthCheck(Empty(), timeout=5)
        assert ei.value.code() == grpc.StatusCode.UNAUTHENTICATED


def test_r6_grpc_client_mtls_support(mtls_engine):
    """R6 回归：生产 GrpcEngineClient 支持 mTLS（服务端 --tls-ca 场景）；
    明文客户端连 mTLS 服务端在传输层被拒（UNAVAILABLE）。"""
    from coordinator.engine_client import EngineError, UNAVAILABLE
    from coordinator.grpc_client import GrpcEngineClient

    pki = mtls_engine["pki"]
    client = GrpcEngineClient(
        port=mtls_engine["port"],
        api_key=MONITOR_KEY,
        tls_ca_path=str(pki["ca"]),
        tls_client_cert_path=str(pki["client_cert"]),
        tls_client_key_path=str(pki["client_key"]),
    )
    status = client.health_check()
    assert status.ok and status.version == "v1.5.5"

    plain = GrpcEngineClient(port=mtls_engine["port"], api_key=MONITOR_KEY)
    with pytest.raises(EngineError) as ei:
        plain.health_check()
    assert ei.value.code == UNAVAILABLE


def test_tdd_api_06_health_version_consistency(engine):
    """HealthCheck / GetKnowledgeVersion 的版本与哈希一致。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        hc = stub.HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),))
        kv = stub.GetKnowledgeVersion(Empty(), metadata=(("x-api-key", MONITOR_KEY),))
        assert hc.ok
        assert hc.version == kv.version == "v1.5.5"
        assert kv.schema_hash and kv.model_hash
        assert hc.max_targets > 0

        stub.InitializeContext(InitContextRequest(task_id="task-6"), metadata=(("x-api-key", COORD_KEY),))
        hc2 = stub.HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),))
        # 断言初始化前后增量为 1，而非依赖绝对计数
        assert hc2.active_targets == hc.active_targets + 1


def test_tdd_api_08_capture_screenshot_and_drag(engine):
    """v1.5.4 追加-only 契约：hit_element/page_url/url_changed；v1.5.3 context_name/CloseContext；v1.5.2 chat_stream_done；
    v1.5.1 起 CaptureScreenshot RPC + drag。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)

        # 未 InitializeContext → FAILED_PRECONDITION。
        with pytest.raises(grpc.RpcError) as ei:
            stub.CaptureScreenshot(
                CaptureScreenshotRequest(target_id="x"),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.FAILED_PRECONDITION

        init = stub.InitializeContext(
            InitContextRequest(task_id="task-8"), metadata=(("x-api-key", COORD_KEY),)
        )
        shot = stub.CaptureScreenshot(
            CaptureScreenshotRequest(target_id=init.target_id),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert shot.png and shot.width > 0 and shot.height > 0

        drag = stub.ExecuteAction(
            ActionIntentRequest(
                intent_id="drag-api-1",
                target_semantic="challenge_slider",
                expected_action="drag",
                value="100,200,300,200",
            ),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert drag.success
        assert drag.local_verification.observed_changes[0].startswith("drag:")

        # monitor 角色越权 → PERMISSION_DENIED。
        with pytest.raises(grpc.RpcError) as ei:
            stub.CaptureScreenshot(
                CaptureScreenshotRequest(target_id=init.target_id),
                metadata=(("x-api-key", MONITOR_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.PERMISSION_DENIED


def test_tdd_api_11_evidence_hit_element_echo(engine):
    """v1.5.4 证据链：ExecuteAction 响应回显实际命中元素（sim 会话合成证据）；
    page_url/url_changed 字段存在且不崩溃（sim 无页面 URL 时为空/false）。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(
            InitContextRequest(task_id="task-evidence"),
            metadata=(("x-api-key", COORD_KEY),),
        )
        resp = stub.ExecuteAction(
            ActionIntentRequest(
                intent_id="evidence-api-1",
                target_semantic="登录",
                expected_action="click",
            ),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert resp.success
        assert resp.hit_element.selector.startswith("sim:click:登录")
        assert resp.hit_element.tag
        assert resp.url_changed is False


def test_tdd_api_07_proto_backward_compat(tmp_path):
    """Proto 兼容性回归：重新生成桩与仓库内 proto/gen 完全一致（追加-only 契约）。"""
    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    go_out = tmp_path / "go"
    py_out = tmp_path / "py"
    go_out.mkdir()
    py_out.mkdir()
    proto_dir = ROOT / "proto"

    go_cmd = [
        "protoc", f"-I{proto_dir}",
        f"--go_out={go_out}", "--go_opt=module=uws",
        f"--go-grpc_out={go_out}", "--go-grpc_opt=module=uws",
        str(proto_dir / "uws_service.proto"),
    ]
    py_cmd = [
        str(ROOT / ".venv" / "bin" / "python"), "-m", "grpc_tools.protoc", f"-I{proto_dir}",
        f"--python_out={py_out}", f"--grpc_python_out={py_out}",
        str(proto_dir / "uws_service.proto"),
    ]
    for cmd in (go_cmd, py_cmd):
        try:
            res = subprocess.run(cmd, cwd=ROOT, env=env, capture_output=True, text=True, timeout=120)
        except FileNotFoundError:
            pytest.skip("protoc 或 grpc_tools 不可用")
        assert res.returncode == 0, f"重新生成失败: {res.stdout}\n{res.stderr}"

    def diff_dir(left: Path, right: Path, names: list[str]):
        for name in names:
            a = left / name
            b = right / name
            assert a.exists() and b.exists(), f"缺少生成文件: {name}"
            assert a.read_bytes() == b.read_bytes(), f"proto/gen 与重新生成不一致: {name}"

    diff_dir(go_out / "proto" / "v1", ROOT / "proto" / "gen" / "go" / "proto" / "v1",
            ["uws_service.pb.go", "uws_service_grpc.pb.go"])
    diff_dir(py_out, ROOT / "proto" / "gen" / "python",
            ["uws_service_pb2.py", "uws_service_pb2_grpc.py"])


def test_tdd_api_12_node_direct_and_export_page_tree(engine):
    """v1.5.5 追加-only 契约：ActionIntentRequest.node_id 确定性引用（跳过语义派发）
    + ExportPageTree RPC（极简 AXTree，混合指挥）。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        init = stub.InitializeContext(
            InitContextRequest(task_id="task-node"),
            metadata=(("x-api-key", COORD_KEY),),
        )

        tree = stub.ExportPageTree(
            ExportPageTreeRequest(target_id=init.target_id),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert "立即购买" in tree.tree

        resp = stub.ExecuteAction(
            ActionIntentRequest(
                intent_id="node-api-1",
                target_semantic="node:12",
                expected_action="click",
                node_id=12,
            ),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert resp.success
        assert resp.hit_element.selector == "sim:node:12"
