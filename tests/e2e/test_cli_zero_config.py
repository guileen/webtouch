"""CLI E2E 测试（TDD-CLI-06：零配置启动）。

验证当本地没有任何配置文件（~/.uws/config.yaml 不存在）且未配置外部知识库/打码服务时：
1. 引擎守护进程（cmd/uws-engine）能够以纯规则/内置内存模式启动，输出清晰可读告警，不发生崩溃（panic/exit code 非 0）；
2. CLI 命令（cmd/uws config get 等）能够正常运行，缺省值与空状态不崩溃。
"""

from __future__ import annotations

import os
import subprocess
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


@pytest.fixture(scope="module")
def uws_engine_binary(tmp_path_factory: pytest.TempPathFactory) -> Path:
    binary = tmp_path_factory.mktemp("uws-engine-bin") / "uws-engine"
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
    return binary


@pytest.fixture(scope="module")
def uws_cli_binary(tmp_path_factory: pytest.TempPathFactory) -> Path:
    binary = tmp_path_factory.mktemp("uws-cli-bin") / "uws"
    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/uws"],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
    )
    if build.returncode != 0:
        pytest.fail(f"go build uws 失败:\n{build.stdout}\n{build.stderr}")
    return binary


def test_tdd_cli_06_zero_config_start(
    uws_engine_binary: Path, uws_cli_binary: Path, tmp_path: Path
):
    """TDD-CLI-06: 零配置环境启动引擎与 CLI，输出可读告警，不崩溃。"""
    empty_home = tmp_path / "empty_home"
    empty_home.mkdir()

    env = dict(os.environ)
    env["HOME"] = str(empty_home)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")

    # 1. 启动引擎守护进程（零配置启动，默认 127.0.0.1:50099 隔离端口）
    test_port = "127.0.0.1:50099"
    proc = subprocess.Popen(
        [str(uws_engine_binary), "-grpc-addr", test_port, "-browser", "sim"],
        cwd=ROOT,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        # 等待 1 秒检查进程是否存活并在 stderr 输出了警告
        time.sleep(1.0)
        assert proc.poll() is None, f"引擎进程异常退出: returncode={proc.returncode}"
    finally:
        proc.terminate()
        try:
            stdout, stderr = proc.communicate(timeout=2.0)
        except subprocess.TimeoutExpired:
            proc.kill()
            stdout, stderr = proc.communicate()

    # 验证日志输出包含安全与零配置告警信息，无 panic
    combined_log = stdout + "\n" + stderr
    assert "panic" not in combined_log.lower()
    assert "警告: 未提供 --api-key-file" in combined_log
    assert "uws-engine" in combined_log
    assert "启动" in combined_log

    # 2. CLI 在零配置目录读取或执行 config get 不崩溃
    res = subprocess.run(
        [str(uws_cli_binary), "config", "get", "fallback_model"],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
    )
    assert res.returncode == 0
    assert "<unset>" in res.stdout
