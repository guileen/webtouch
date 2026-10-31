"""uws probe 单元测试（2026-09-03，TDD-CLI-19 对应）。

验证：附着调试 Chrome -> InitializeContext -> ExportPageTree + CaptureScreenshot ->
Markdown + PNG 输出（AXTree 报到 Agent 层）；默认快照后关上下文，--keep-open 保留。
不启动真实引擎/浏览器，注入 FakeEngine。
"""

from __future__ import annotations

import argparse
from pathlib import Path

import pytest

from coordinator.cli import run_probe
from coordinator.engine_client import InitResult


class ProbeFakeEngine:
    def __init__(self, *, ok: bool = True) -> None:
        self.ok = ok
        self.initial_url: str | None = None
        self.closed: list[str] = []

    def initialize_context(
        self, task_id: str, initial_url: str | None = None, **kwargs: object
    ) -> InitResult:
        self.initial_url = initial_url
        return InitResult(ok=self.ok, target_id="target-1", context_id="ctx-1")

    def export_page_tree(self, target_id: str) -> str:
        return '[1] button(button) "登录"\n[2] link(link) "ESA 控制台"\n'

    def capture_screenshot(self, target_id: str) -> bytes:
        return b"fake-png-bytes"

    def close_context(self, context_name: str = "") -> None:
        self.closed.append(context_name)


def _args(tmp_path: Path, **overrides: object) -> argparse.Namespace:
    base = dict(
        engine_addr="127.0.0.1:50051",
        api_key="",
        url="https://esa.console.aliyun.com",
        attach="http://127.0.0.1:9222",
        output=str(tmp_path / "esa-probe.md"),
        keep_open=False,
    )
    base.update(overrides)
    return argparse.Namespace(**base)


def test_probe_writes_tree_and_png(tmp_path: Path) -> None:
    engine = ProbeFakeEngine()

    rc = run_probe(_args(tmp_path), engine=engine)

    assert rc == 0
    assert engine.initial_url == "https://esa.console.aliyun.com"
    report = (tmp_path / "esa-probe.md").read_text(encoding="utf-8")
    assert "Target ID: `target-1`" in report
    assert "Context ID: `ctx-1`" in report
    assert "ESA 控制台" in report
    assert "[2] link(link)" in report
    assert (tmp_path / "esa-probe.png").read_bytes() == b"fake-png-bytes"
    assert engine.closed


def test_probe_default_stdout_merges_markdown_and_axtree(tmp_path: Path, monkeypatch, capsys) -> None:
    """TDD-CLI-24：无 --output 时 probe 不写报告文件，AXTree 随 Markdown 一起
    完整打印 stdout（含 Tab/Target ID），截图落 ~/.uws/artifacts/。"""
    monkeypatch.setenv("HOME", str(tmp_path))
    engine = ProbeFakeEngine()

    rc = run_probe(_args(tmp_path, output=""), engine=engine)

    assert rc == 0
    out = capsys.readouterr().out
    assert "Target ID: `target-1`" in out
    assert "Context ID: `ctx-1`" in out
    assert "## AXTree" in out
    assert "ESA 控制台" in out
    assert "[2] link(link)" in out
    shots = list((tmp_path / ".uws" / "artifacts").glob("probe-*.png"))
    assert len(shots) == 1
    assert shots[0].read_bytes() == b"fake-png-bytes"
    assert not (tmp_path / "esa-probe.md").exists()


def test_probe_keep_open_leaves_tab(tmp_path: Path) -> None:
    engine = ProbeFakeEngine()

    rc = run_probe(_args(tmp_path, keep_open=True), engine=engine)

    assert rc == 0
    assert engine.closed == []


def test_probe_init_failure_reports(tmp_path: Path) -> None:
    engine = ProbeFakeEngine(ok=False)

    with pytest.raises(RuntimeError, match="InitializeContext"):
        run_probe(_args(tmp_path), engine=engine)
