"""CLI 引擎守护进程生命周期 E2E 测试（uws daemon start/stop/restart/status + 按需拉起）。

验证 `uws`（Go CLI）能直接托管 uws-engine 守护进程，取代 launchd：
- 未运行时 status 提示未运行；
- start 后指定端口监听、status 显示运行中；
- restart 后仍监听；
- stop 后端口关闭、status 回到未运行；
- run 子命令在引擎未就绪时按需拉起（auto-start）。

默认跑在 127.0.0.1（本机），不依赖真实 Chrome 登录态。
"""

from __future__ import annotations

import os
import socket
import subprocess
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


@pytest.fixture(scope="module")
def engine_binary(tmp_path_factory: pytest.TempPathFactory) -> Path:
    b = tmp_path_factory.mktemp("uws-engine-daemon-bin") / "uws-engine"
    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    r = subprocess.run(
        ["go", "build", "-o", str(b), "./cmd/uws-engine"],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
    )
    if r.returncode != 0:
        pytest.fail(f"go build uws-engine 失败:\n{r.stdout}\n{r.stderr}")
    return b


@pytest.fixture(scope="module")
def cli_binary(tmp_path_factory: pytest.TempPathFactory) -> Path:
    b = tmp_path_factory.mktemp("uws-cli-daemon-bin") / "uws"
    env = dict(os.environ)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    r = subprocess.run(
        ["go", "build", "-o", str(b), "./cmd/uws"],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
    )
    if r.returncode != 0:
        pytest.fail(f"go build uws 失败:\n{r.stdout}\n{r.stderr}")
    return b


def _free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def _wait_port(port: int, want_open: bool, timeout: float = 8.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        s = socket.socket()
        s.settimeout(0.3)
        try:
            s.connect(("127.0.0.1", port))
            connected = True
        except OSError:
            connected = False
        finally:
            s.close()
        if connected == want_open:
            return True
        time.sleep(0.2)
    return False


def _run(cli_binary: Path, env: dict, *args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(cli_binary), *args],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
        timeout=40,
    )


def _env(cli_binary: Path, engine_binary: Path, home: Path, state: Path, port: int):
    env = dict(os.environ)
    env["HOME"] = str(home)
    env["GOCACHE"] = "/tmp/uws-go-cache"
    env["UWS_ENGINE_BIN"] = str(engine_binary)
    env["UWS_ENGINE_DIR"] = str(state)
    # 托管地址固定为本次测试端口，使 uws run 的按需拉起（ensureEngine）识别为默认托管地址。
    env["UWS_ENGINE_ADDR"] = f"127.0.0.1:{port}"
    # 项目 venv 置于最前，让 Go 的 pythonCommand() 解析到带依赖的 python3（run --help 走协调器）。
    env["PATH"] = f"{ROOT / '.venv' / 'bin'}:{env.get('PATH', '')}"
    return env


def test_tdd_cli_daemon_lifecycle_and_autostart(
    cli_binary: Path, engine_binary: Path, tmp_path: Path
) -> None:
    port = _free_port()
    addr = f"127.0.0.1:{port}"
    home = tmp_path / "home"
    home.mkdir()
    state = tmp_path / "state"
    state.mkdir()
    env = _env(cli_binary, engine_binary, home, state, port)

    # 1. 初始：未运行
    r = _run(cli_binary, env, "daemon", "status")
    assert r.returncode == 0, r.stderr
    assert "未运行" in r.stdout + r.stderr

    # 2. start：端口监听
    r = _run(cli_binary, env, "daemon", "start", "--engine-addr", addr)
    assert r.returncode == 0, r.stderr
    assert "引擎已启动" in r.stdout
    assert _wait_port(port, True), "引擎启动后端口应监听"

    # 3. status：运行中
    r = _run(cli_binary, env, "daemon", "status")
    assert r.returncode == 0, r.stderr
    assert "运行中" in r.stdout

    # 4. restart：仍监听（先停后起）
    r = _run(cli_binary, env, "daemon", "restart", "--engine-addr", addr)
    assert r.returncode == 0, r.stderr
    assert _wait_port(port, True)

    # 5. auto-start：stop 后，run --help 会按需拉起引擎
    r = _run(cli_binary, env, "daemon", "stop")
    assert r.returncode == 0, r.stderr
    assert _wait_port(port, False)

    r = _run(cli_binary, env, "run", "--engine-addr", addr, "--help")
    assert r.returncode == 0, r.stderr
    assert _wait_port(port, True), "run 子命令应按需拉起引擎"

    # 6. 清理：stop → 端口关闭、status 回到未运行
    r = _run(cli_binary, env, "daemon", "stop")
    assert r.returncode == 0, r.stderr
    assert _wait_port(port, False)
    r = _run(cli_binary, env, "daemon", "status")
    assert r.returncode == 0, r.stderr
    assert "未运行" in r.stdout
