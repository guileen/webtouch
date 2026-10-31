"""匹配/分类集成测试（TDD-MATCH-02/04、TDD-ONNX-01/03，tdd-checklist.md §2）。

经真实 uws-engine 二进制（内置 mock scorer）驱动 gRPC 验证：
- TDD-MATCH-02：site_synonyms TENTATIVE 同义词以 0.2 权重扰动推入 L3；
- TDD-MATCH-04：negative_keywords（广告/弹窗容器）黑名单过滤；
- TDD-ONNX-01/03：决策核正样本 ≥0.85 执行、困难负样本 <0.50 → NOT_FOUND，
  黄金困难负样本集 AUC ≥0.90。
- TDD-ONNX 真实侧（A 案 ort-go CGO，2026-08-20）：引擎以真实 classifier.onnx
  artifact + --onnx-runtime-lib 启动，GetKnowledgeVersion 模型哈希与导出一致。

TDD-MATCH-05（L3 SimHash 物理校验）需要 DOM/候选特征输入通道（冻结契约无对应 RPC），
待 v1.5 契约或真实 CDP 会话接入后点亮，见 USER_ACTION.md。
"""

import hashlib
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
from pipeline.exporter.train_onnx import SEED_NEGATIVES, SEED_POSITIVES, train_classifier_onnx
from pipeline.llm_distiller.distiller import DistilledRule
from uws_service_pb2 import ActionIntentRequest, Empty, InitContextRequest
from uws_service_pb2_grpc import UwsEngineStub

COORD_KEY = "coordinator-test-key"
OPERATOR_KEY = "operator-test-key"
MONITOR_KEY = "monitor-test-key"
ROOT = Path(__file__).resolve().parents[2]


def _channel(port):
    return grpc.insecure_channel(f"127.0.0.1:{port}")


def _free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _wait_engine(port, proc):
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


def _onnx_lib_path():
    p = os.environ.get("ONNXRUNTIME_SHARED_LIBRARY_PATH")
    if p:
        return p
    p = ROOT / "build" / "ort" / "libonnxruntime.1.23.0.dylib"
    return str(p) if p.exists() else ""


@pytest.fixture(scope="module")
def onnx_engine():
    """真实 ONNX（A 案）引擎：exporter 真实模型 artifact + 本地 HTTP 服务 +
    --knowledge-base-url + --onnx-runtime-lib。ORT 库缺失时整组 skip。"""
    lib = _onnx_lib_path()
    if not lib:
        pytest.skip("未找到 libonnxruntime（设置 ONNXRUNTIME_SHARED_LIBRARY_PATH 或先 make ort-lib）")
    tmp = Path(tempfile.mkdtemp(prefix="uws-onnx-artifacts-"))
    try:
        model = train_classifier_onnx(SEED_POSITIVES, SEED_NEGATIVES)
        Exporter(tmp).export(
            "v1.6.0",
            rules=[
                DistilledRule(text_pattern="登录", action="click", confidence=0.95),
                DistilledRule(text_pattern="提交", action="click", confidence=0.9),
            ],
            min_engine_version="v1.5.0",
            onnx_model_bytes=model,
            description="real onnx",
        )
        handler = lambda *args, **kwargs: http.server.SimpleHTTPRequestHandler(  # noqa: E731
            *args, directory=str(tmp), **kwargs
        )
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        base_url = f"http://127.0.0.1:{server.server_address[1]}"

        keys = [
            {"key": COORD_KEY, "role": "coordinator"},
            {"key": OPERATOR_KEY, "role": "operator"},
            {"key": MONITOR_KEY, "role": "monitor"},
        ]
        keyfile = tmp / "keys.json"
        keyfile.write_text(json.dumps({"keys": keys}, ensure_ascii=False))
        binary = tmp / "uws-engine"
        env = dict(os.environ)
        env.setdefault("GOCACHE", "/tmp/uws-go-cache")
        env.setdefault("GOPATH", os.path.join(tmp, "gopath"))
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
                "--knowledge-base-url", base_url,
                "--knowledge-version", "v1.6.0",
                "--onnx-runtime-lib", lib,
            ],
            cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        _wait_engine(port, proc)
        if proc.poll() is not None:
            out, err = proc.communicate(timeout=5)
            pytest.fail(f"引擎进程提前退出: {out}\n{err}")

        yield {"port": port, "model_hash": "sha256:" + hashlib.sha256(model).hexdigest()}

        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        server.shutdown()
        server.server_close()
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def test_tdd_match_02_l2_bm25_tentative_synonym(engine):
    """'Sign in' 不在 L1 规则内，但 site_synonyms 含 TENTATIVE 同义词 →
    L2 以 0.2 权重扰动（0.5 语义召回先验 + 0.2）推入候选池并成功执行。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(
            InitContextRequest(task_id="m2"), metadata=(("x-api-key", COORD_KEY),)
        )
        resp = stub.ExecuteAction(
            ActionIntentRequest(
                intent_id="m2-1", target_semantic="Sign in", expected_action="click"
            ),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert resp.success
        # 0.5 召回先验 + TENTATIVE 增益 0.2；若走 classifier 兜底会得到 0.90，故 0.7 可证明 L2 路径。
        assert resp.confidence == pytest.approx(0.7, abs=1e-6)


def test_tdd_match_04_negative_keywords_blacklist(engine):
    """页面节点含 negative_keywords（广告/弹窗容器）时，即使文本命中 L1 也不得进入候选池。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(
            InitContextRequest(task_id="m4"), metadata=(("x-api-key", COORD_KEY),)
        )

        # 基线：无黑名单上下文 → 命中 L1 登录规则。
        ok = stub.ExecuteAction(
            ActionIntentRequest(
                intent_id="m4-1", target_semantic="登录", expected_action="click"
            ),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert ok.success

        # 容器 zonal_keyword 命中黑名单（广告）→ 拦截，不进候选池、不兜底。
        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(
                ActionIntentRequest(
                    intent_id="m4-2",
                    target_semantic="登录",
                    expected_action="click",
                    context_hint={"zonal_keyword": "广告"},
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.NOT_FOUND

        # 语义本身含黑名单 token（banner_zone）→ 同样拦截。
        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(
                ActionIntentRequest(
                    intent_id="m4-3", target_semantic="banner_zone_login", expected_action="click"
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.NOT_FOUND


def test_tdd_onnx_01_hard_negative_separation(engine):
    """正样本 C≥0.85 直接执行；困难负样本 C<0.50 → NOT_FOUND（mock scorer 模拟 ONNX 决策）。"""
    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(
            InitContextRequest(task_id="onnx1"), metadata=(("x-api-key", COORD_KEY),)
        )

        pos = stub.ExecuteAction(
            ActionIntentRequest(
                intent_id="onnx1-p", target_semantic="login_button", expected_action="click"
            ),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert pos.success
        assert pos.confidence >= 0.85

        with pytest.raises(grpc.RpcError) as ei:
            stub.ExecuteAction(
                ActionIntentRequest(
                    intent_id="onnx1-n",
                    target_semantic="hard_negative_button",
                    expected_action="click",
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
        assert ei.value.code() == grpc.StatusCode.NOT_FOUND


def test_tdd_onnx_03_golden_regression_auc(engine):
    """黄金困难负样本回归集经真实引擎驱动：AUC ≥0.90（NOT_FOUND 负样本按 0 分计）。"""
    positives = [
        "login_button", "search_button", "submit_btn", "username_input", "password_input",
        "register_btn", "next_page_btn", "confirm_btn", "send_btn",
    ]
    negatives = [
        "hard_negative", "spam_button", "fake_login", "hidden_submit", "tracking_btn",
        "bait_link", "deco_btn", "ghost_btn", "noise_btn",
    ]

    port = engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        stub.InitializeContext(
            InitContextRequest(task_id="onnx3"), metadata=(("x-api-key", COORD_KEY),)
        )

        scores = []
        for i, sem in enumerate(positives):
            resp = stub.ExecuteAction(
                ActionIntentRequest(
                    intent_id=f"golden-p-{i}", target_semantic=sem, expected_action="click"
                ),
                metadata=(("x-api-key", COORD_KEY),),
            )
            assert resp.success, f"正样本 {sem} 应执行成功"
            assert resp.confidence >= 0.85
            scores.append((True, resp.confidence))

        for i, sem in enumerate(negatives):
            with pytest.raises(grpc.RpcError) as ei:
                stub.ExecuteAction(
                    ActionIntentRequest(
                        intent_id=f"golden-n-{i}", target_semantic=sem, expected_action="click"
                    ),
                    metadata=(("x-api-key", COORD_KEY),),
                )
            assert ei.value.code() == grpc.StatusCode.NOT_FOUND, f"负样本 {sem} 应 NOT_FOUND"
            scores.append((False, 0.0))

    assert _auc(scores) >= 0.90


def test_tdd_onnx_real_model_loaded_contract(onnx_engine):
    """真实 ONNX（A 案 ort-go CGO）契约层：引擎以真实模型 artifact + ORT 库启动，
    知识库版本/模型哈希就绪、HealthCheck 正常、语义动作仍可执行。"""
    port = onnx_engine["port"]
    with _channel(port) as ch:
        stub = UwsEngineStub(ch)
        kv = stub.GetKnowledgeVersion(Empty(), metadata=(("x-api-key", MONITOR_KEY),))
        assert kv.version == "v1.6.0"
        assert kv.model_hash == onnx_engine["model_hash"], "模型哈希应与 exporter 产物一致"
        hc = stub.HealthCheck(Empty(), metadata=(("x-api-key", MONITOR_KEY),))
        assert hc.ok

        stub.InitializeContext(
            InitContextRequest(task_id="onnx-contract"), metadata=(("x-api-key", COORD_KEY),)
        )
        resp = stub.ExecuteAction(
            ActionIntentRequest(
                intent_id="onnx-c-1", target_semantic="登录", expected_action="click"
            ),
            metadata=(("x-api-key", COORD_KEY),),
        )
        assert resp.success


def _auc(pairs):
    """Mann-Whitney U（秩和法）计算 ROC-AUC，与 pkg/classifier.AUC 同语义。"""
    sorted_pairs = sorted(pairs, key=lambda p: p[1])
    n_pos = sum(1 for p in sorted_pairs if p[0])
    n_neg = len(sorted_pairs) - n_pos
    if n_pos == 0 or n_neg == 0:
        return 0.0

    rank_sum = 0.0
    i = 0
    while i < len(sorted_pairs):
        j = i
        while j < len(sorted_pairs) and sorted_pairs[j][1] == sorted_pairs[i][1]:
            j += 1
        avg_rank = (i + j + 1) / 2  # 1-based 平均秩
        for k in range(i, j):
            if sorted_pairs[k][0]:
                rank_sum += avg_rank
        i = j

    f_pos, f_neg = float(n_pos), float(n_neg)
    return (rank_sum - f_pos * (f_pos + 1) / 2) / (f_pos * f_neg)
