"""CLI 单元测试（TDD-CLI-05，tdd-checklist.md §8）。

构建真实 uws 二进制 + 隔离 HOME，验证配置持久化：
~/.uws/config.yaml 权限 0600、fallback_api_key 不打印明文、增量更新保留既有字段。
"""

from __future__ import annotations

import os
import stat
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


@pytest.fixture(scope="module")
def uws_binary(tmp_path_factory: pytest.TempPathFactory) -> Path:
    """构建 uws 二进制（复用 conftest 的 GOCACHE 约定）。"""
    binary = tmp_path_factory.mktemp("uws-cli") / "uws"
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


def _run_uws(binary: Path, home: Path, *args: str) -> subprocess.CompletedProcess[str]:
    env = dict(os.environ)
    env["HOME"] = str(home)
    env.setdefault("GOCACHE", "/tmp/uws-go-cache")
    return subprocess.run(
        [str(binary), *args],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
    )


def test_tdd_cli_05_config_persist_secure(
    uws_binary: Path, tmp_path: Path
) -> None:
    home = tmp_path / "home"
    home.mkdir()

    # 首次写入：~/.uws/config.yaml 落盘，权限 0600，内容包含设置值。
    first = _run_uws(uws_binary, home, "config", "set", "fallback_model", "deepseek-chat")
    assert first.returncode == 0, first.stderr
    cfg = home / ".uws" / "config.yaml"
    assert cfg.exists()
    assert stat.S_IMODE(cfg.stat().st_mode) == 0o600
    assert "deepseek-chat" in cfg.read_text()

    # 第二次写入 api_key：不打印明文；文件仍为 0600；既有字段保留。
    secret = "sk-super-secret-123456"
    second = _run_uws(uws_binary, home, "config", "set", "fallback_api_key", secret)
    assert second.returncode == 0, second.stderr
    assert secret not in second.stdout
    assert secret not in second.stderr
    content = cfg.read_text()
    assert secret in content
    assert "deepseek-chat" in content
    assert stat.S_IMODE(cfg.stat().st_mode) == 0o600

    # 未知键报错且不破坏现有配置。
    bad = _run_uws(uws_binary, home, "config", "set", "no_such_key", "x")
    assert bad.returncode != 0
    assert "deepseek-chat" in cfg.read_text()

    # get 子命令测试：读取已设置键与敏感键脱敏
    get_model = _run_uws(uws_binary, home, "config", "get", "fallback_model")
    assert get_model.returncode == 0
    assert "deepseek-chat" in get_model.stdout

    get_key = _run_uws(uws_binary, home, "config", "get", "fallback_api_key")
    assert get_key.returncode == 0
    assert secret not in get_key.stdout
    assert "<redacted>" in get_key.stdout


def test_uws_argv0_prints_deprecation(uws_binary: Path, tmp_path: Path) -> None:
    """二进制名叫 uws 时 stderr 提示弃用；改名为 webtouch 后不提示。stdout JSON/版本不受影响。"""
    home = tmp_path / "home"
    home.mkdir()
    as_uws = _run_uws(uws_binary, home, "version")
    assert as_uws.returncode == 0, as_uws.stderr
    assert "已弃用" in as_uws.stderr
    assert "webtouch CLI" in as_uws.stdout
    renamed = tmp_path / "webtouch"
    renamed.write_bytes(uws_binary.read_bytes())
    renamed.chmod(0o755)
    as_wt = _run_uws(renamed, home, "version")
    assert as_wt.returncode == 0, as_wt.stderr
    assert "已弃用" not in as_wt.stderr
    assert "webtouch CLI" in as_wt.stdout


def test_tdd_cli_08_uws_help_is_skill_friendly(uws_binary: Path, tmp_path: Path) -> None:
    """CLI 自解释：`uws help`（及无参）返回 Agent 友好手册，且与 SKILL 保证一致。"""
    home = tmp_path / "home"
    home.mkdir()
    res = _run_uws(uws_binary, home, "help")
    assert res.returncode == 0
    out = res.stdout
    for token in (
        "webtouch — 浏览器自动化底座",
        "ExecuteAction",
        "v1/chat/completions",
        "请求级",
        "错误速查",
        "反模式",
    ):
        assert token in out

    # 无参 = 同一自解释手册
    noarg = _run_uws(uws_binary, home)
    assert noarg.returncode == 0
    assert "Agent 自查手册" in noarg.stdout


def test_tdd_cli_08_python_help_subcommand(capsys) -> None:
    """python -m coordinator.cli help 同样输出自解释手册。"""
    from coordinator.cli import main

    assert main(["help"]) == 0
    captured = capsys.readouterr().out
    assert "webtouch — 浏览器自动化底座" in captured
    assert "错误速查" in captured


def test_engine_hint_maps_status_code_to_action() -> None:
    """引擎错误自动映射为 agent 友好对策（容错 + 自解释）。"""
    from coordinator.cli import _engine_hint
    from coordinator.engine_client import EngineError

    assert "重新规划" in _engine_hint(EngineError(5, "not found"))
    assert "重新 InitializeContext" in _engine_hint(EngineError(9))
    assert "WAF" in _engine_hint(EngineError(16))
    assert "对照" in _engine_hint(EngineError(14))  # 未登记状态码 → 通用提示
