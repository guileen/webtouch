"""attach 目标解析单元测试（TDD-CLI-18，2026-08-31）。

验证 --attach 三种取值（debug / default / 显式 URL）的解析与 Chrome
拉起/重启决策：全部通过注入桩驱动，不启动真实浏览器。
"""

from __future__ import annotations

from pathlib import Path

import pytest

from coordinator import attach


def test_explicit_url_passthrough() -> None:
    for url in ("http://127.0.0.1:9222", "https://127.0.0.1:9222/", "ws://127.0.0.1:9222/"):
        assert attach.resolve_attach(url) == url.rstrip("/")


def test_invalid_value_raises() -> None:
    with pytest.raises(ValueError, match="debug / default"):
        attach.resolve_attach("firefox")


def test_debug_reuses_running_endpoint(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(attach, "probe_cdp", lambda url, timeout=1.2: True)
    monkeypatch.setattr(attach, "launch_chrome", lambda *a, **k: pytest.fail("不应拉起 Chrome"))
    assert attach.resolve_attach("debug") == "http://127.0.0.1:9222"


def test_debug_launches_when_down(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple] = []
    monkeypatch.setenv(attach.CHROME_LAUNCH_PERMISSION_ENV, "1")
    monkeypatch.setattr(attach, "probe_cdp", lambda url, timeout=1.2: False)
    monkeypatch.setattr(
        attach, "wait_cdp", lambda url, timeout=15.0: (calls.append((url, timeout)) or True)
    )
    monkeypatch.setattr(
        attach,
        "launch_chrome",
        lambda port, profile_dir, restore=False: calls.append((port, profile_dir, restore)),
    )
    assert attach.resolve_attach("debug") == "http://127.0.0.1:9222"
    assert calls[0] == (attach.DEBUG_PORT, attach.debug_profile_dir(), False)


def test_debug_clears_stale_locks_of_dead_owner(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """2026-09-03：Chrome 异常退出后残留 Singleton*（owner PID 已死），ensure_debug
    应自动清理再拉起，而不是让下一次拉起误判“已有实例”直接退出。"""
    profile = tmp_path / "profile"
    profile.mkdir()
    # macOS 形态：SingletonLock 为指向 host.local-<pid> 的符号链接；PID 999999 视为已死。
    (profile / "SingletonLock").symlink_to("GLMBP.local-999999")
    (profile / "SingletonCookie").symlink_to("token")
    (profile / "SingletonSocket").symlink_to("/tmp/com.google.Chrome.stale/SingletonSocket")

    calls: list[tuple] = []
    monkeypatch.setenv(attach.CHROME_LAUNCH_PERMISSION_ENV, "true")
    monkeypatch.setattr(attach, "probe_cdp", lambda url, timeout=1.2: False)
    monkeypatch.setattr(attach, "wait_cdp", lambda url, timeout=15.0: True)
    monkeypatch.setattr(attach, "debug_profile_dir", lambda: profile)
    monkeypatch.setattr(
        attach,
        "launch_chrome",
        lambda port, profile_dir, restore=False: calls.append((port, profile_dir, restore)),
    )

    assert attach.resolve_attach("debug") == "http://127.0.0.1:9222"
    assert calls[0] == (attach.DEBUG_PORT, profile, False)
    assert not (profile / "SingletonLock").exists()
    assert not (profile / "SingletonCookie").exists()
    assert not (profile / "SingletonSocket").exists()


def test_debug_keeps_locks_of_live_owner(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """owner PID 存活时绝不清理锁（本机真实调试 Chrome 运行中也不误伤）。"""
    profile = tmp_path / "profile"
    profile.mkdir()
    lock = profile / "SingletonLock"
    lock.write_text("host-1", encoding="utf-8")  # PID 1 恒存活

    monkeypatch.setenv(attach.CHROME_LAUNCH_PERMISSION_ENV, "yes")
    monkeypatch.setattr(attach, "probe_cdp", lambda url, timeout=1.2: False)
    monkeypatch.setattr(attach, "wait_cdp", lambda url, timeout=15.0: True)
    monkeypatch.setattr(attach, "debug_profile_dir", lambda: profile)
    monkeypatch.setattr(attach, "launch_chrome", lambda *a, **k: None)

    attach.ensure_debug(profile_dir=profile)
    assert lock.exists()


def test_unreachable_debug_refuses_to_launch_without_explicit_permission(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv(attach.CHROME_LAUNCH_PERMISSION_ENV, raising=False)
    monkeypatch.setattr(attach, "probe_cdp", lambda url, timeout=1.2: False)
    monkeypatch.setattr(attach, "launch_chrome", lambda *a, **k: pytest.fail("不应拉起 Chrome"))

    with pytest.raises(RuntimeError, match="UWS_ALLOW_CHROME_LAUNCH=1"):
        attach.resolve_attach("debug")


@pytest.fixture
def isolated_default_profile(monkeypatch, tmp_path):
    monkeypatch.setattr(attach, "default_profile_dir", lambda: tmp_path)
    monkeypatch.setattr(attach, "chrome_bin", lambda: "/fake/chrome")


def test_default_launches_when_explicitly_authorized(monkeypatch: pytest.MonkeyPatch, isolated_default_profile) -> None:
    calls: list[tuple] = []
    monkeypatch.setenv(attach.CHROME_LAUNCH_PERMISSION_ENV, "1")
    monkeypatch.setattr(attach, "probe_cdp", lambda url, timeout=1.2: False)
    monkeypatch.setattr(attach, "is_chrome_running", lambda profile: False)
    monkeypatch.setattr(attach, "wait_cdp", lambda url, timeout=15.0: True)
    monkeypatch.setattr(
        attach,
        "launch_chrome",
        lambda port, profile_dir, restore=False: calls.append((port, profile_dir, restore)),
    )
    assert attach.resolve_attach("default") == "http://127.0.0.1:9223"
    port, profile, restore = calls[0]
    assert port == attach.DEFAULT_PORT
    assert profile == attach.default_profile_dir()
    assert restore is True


def test_unreachable_default_refuses_to_launch_without_explicit_permission(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv(attach.CHROME_LAUNCH_PERMISSION_ENV, raising=False)
    monkeypatch.setattr(attach, "probe_cdp", lambda url, timeout=1.2: False)
    monkeypatch.setattr(attach, "is_chrome_running", lambda profile: False)
    monkeypatch.setattr(attach, "launch_chrome", lambda *a, **k: pytest.fail("不应拉起 Chrome"))

    with pytest.raises(RuntimeError, match="UWS_ALLOW_CHROME_LAUNCH=1"):
        attach.resolve_attach("default")


def test_default_running_requires_confirm(monkeypatch: pytest.MonkeyPatch, isolated_default_profile) -> None:
    monkeypatch.setenv(attach.CHROME_LAUNCH_PERMISSION_ENV, "1")
    monkeypatch.setattr(attach, "probe_cdp", lambda url, timeout=1.2: False)
    monkeypatch.setattr(attach, "is_chrome_running", lambda profile: True)
    monkeypatch.setattr(attach, "_confirm_default_restart", lambda: False)
    with pytest.raises(RuntimeError, match="UWS_ATTACH_CONFIRM"):
        attach.resolve_attach("default")


def test_default_running_confirmed_restarts(monkeypatch: pytest.MonkeyPatch, isolated_default_profile) -> None:
    running = {"value": True}

    def fake_running(profile: Path) -> bool:
        return running["value"]

    calls: list[tuple] = []
    monkeypatch.setenv(attach.CHROME_LAUNCH_PERMISSION_ENV, "1")
    monkeypatch.setattr(attach, "probe_cdp", lambda url, timeout=1.2: False)
    monkeypatch.setattr(attach, "is_chrome_running", fake_running)
    monkeypatch.setattr(attach, "_confirm_default_restart", lambda: True)
    monkeypatch.setattr(
        attach, "quit_chrome", lambda bin_path: running.update(value=False)
    )
    monkeypatch.setattr(attach, "wait_cdp", lambda url, timeout=15.0: True)
    monkeypatch.setattr(
        attach,
        "launch_chrome",
        lambda port, profile_dir, restore=False: calls.append((port, profile_dir, restore)),
    )
    assert attach.resolve_attach("default") == "http://127.0.0.1:9223"
    assert calls and calls[0][0] == attach.DEFAULT_PORT


def test_env_overrides_bin_and_profile(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    fake_bin = tmp_path / "fake-chrome"
    fake_bin.touch()
    monkeypatch.setenv("UWS_CHROME_BIN", str(fake_bin))
    monkeypatch.setenv("UWS_CHROME_PROFILE", str(tmp_path / "profile"))
    assert attach.chrome_bin() == str(fake_bin)
    assert attach.debug_profile_dir() == tmp_path / "profile"
    assert attach.default_profile_dir() == tmp_path / "profile"


# ── 专属小窗（2026-09-11）：冷启动小窗参数与 macOS bundle 推导 ──────────────


def test_window_flags_default_and_override(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("UWS_WINDOW_SIZE", raising=False)
    monkeypatch.delenv("UWS_WINDOW_POS", raising=False)
    assert attach.window_flags() == ["--window-size=1440,900"]

    monkeypatch.setenv("UWS_WINDOW_SIZE", "390x844")
    monkeypatch.setenv("UWS_WINDOW_POS", "1200,24")
    assert attach.window_flags() == ["--window-size=390,844", "--window-position=1200,24"]

    # auto/none = 交给系统/不移动，不传固定坐标。
    monkeypatch.setenv("UWS_WINDOW_POS", "auto")
    assert attach.window_flags() == ["--window-size=390,844"]


def test_app_bundle_and_launch_direct_exec(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    assert (
        attach.app_bundle("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
        == "/Applications/Google Chrome.app"
    )
    assert attach.app_bundle("/usr/bin/google-chrome") is None

    bin_path = tmp_path / "fake-chrome"
    bin_path.touch()
    monkeypatch.setattr(attach, "chrome_bin", lambda: str(bin_path))
    popen: list[list[str]] = []
    monkeypatch.setattr(attach.subprocess, "Popen", lambda args, **kw: popen.append(list(args)))
    profile = tmp_path / "profile"
    attach.launch_chrome(9299, profile)
    assert popen and popen[0][0] == str(bin_path)
    joined = " ".join(popen[0])
    assert "--remote-debugging-port=9299" in joined
    assert "--window-size=1440,900" in joined
    # 反节流三件套（2026-09-12，GLOBAL #92）：后台/被遮挡时不节流，输入才不挂起。
    for flag in (
        "--disable-backgrounding-occluded-windows",
        "--disable-renderer-backgrounding",
        "--disable-background-timer-throttling",
    ):
        assert flag in joined
    assert joined.endswith("about:blank")
