"""attach 目标解析：uws-chrome-debug 持久调试 Chrome 或用户日常 Chrome（2026-08-31）。

`--attach` 取值（run/explore/api 通用）：
  debug     → 持久调试 Chrome：profile 默认 ~/.uws-chrome-debug，端口 9222；
              端点未就绪时自动拉起（与 uws-gateway 的 EnsureChrome 语义一致）。
  default   → 用户日常 Chrome：默认 profile，端口 9223；
              已在该端口监听则直接复用；运行中且无调试端口时先优雅退出，
              再带 --remote-debugging-port 重启（窗口/标签恢复、登录态保留）。
  <url>     → 显式 CDP 端点（如 http://127.0.0.1:9222），原样使用。

统一返回 http://127.0.0.1:<port> 形式端点；引擎侧自行解析 /json/version 得到
ws 端点，explorer 的原始 CDP 采集（/json/list）也直接消费该 http 端点。

安全边界：default 目标会重启用户日常 Chrome，非交互环境必须显式
UWS_ATTACH_CONFIRM=1 才放行；调试端口一律绑定 127.0.0.1。
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

DEBUG_PORT = 9222
DEFAULT_PORT = 9223
DEBUG_PROFILE_DIRNAME = ".uws-chrome-debug"
# 只有显式授权时，UWS 才能为不可达的 CDP 端点拉起可见 Chrome。
# 这个开关与 default 模式重启日常 Chrome 的 UWS_ATTACH_CONFIRM 是两个门禁：
# 前者授权「启动浏览器」，后者保留「重启日常浏览器」的既有确认语义。
CHROME_LAUNCH_PERMISSION_ENV = "UWS_ALLOW_CHROME_LAUNCH"
# 冷启动调试 Chrome 的默认窗口尺寸（UWS_WINDOW_SIZE 覆盖；与引擎 window.go 一致）。
# 这是页面的排版尺寸：小尺寸会把响应式页面按窄屏重排、展示不全（2026-09-22 现场反馈）。
DEFAULT_WINDOW_SIZE = "1440x900"
CHROME_READY_TIMEOUT = 15.0
QUIT_TIMEOUT = 15.0
# Chrome 单实例锁三件套（macOS 为指向 PID/临时 socket 的符号链接；残留会误判“已有实例”）。
STALE_LOCK_FILES = ("SingletonLock", "SingletonCookie", "SingletonSocket")


def chrome_bin() -> str | None:
    """返回可用的 Chrome/Chromium 可执行文件（UWS_CHROME_BIN 可覆盖）。"""
    if env := os.environ.get("UWS_CHROME_BIN", "").strip():
        if os.path.exists(env):
            return env
    candidates: list[str] = []
    if sys.platform == "darwin":
        candidates = [
            "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
            "/Applications/Chromium.app/Contents/MacOS/Chromium",
        ]
    elif sys.platform.startswith("linux"):
        candidates = [
            "/usr/bin/google-chrome",
            "/usr/bin/google-chrome-stable",
            "/usr/bin/chromium",
            "/usr/bin/chromium-browser",
        ]
    elif sys.platform == "win32":
        local = os.environ.get("LOCALAPPDATA", "")
        program_files = os.environ.get("PROGRAMFILES", r"C:\Program Files")
        candidates = [
            os.path.join(local, r"Google\Chrome\Application\chrome.exe"),
            os.path.join(program_files, r"Google\Chrome\Application\chrome.exe"),
            os.path.join(program_files, r"(x86)\Google\Chrome\Application\chrome.exe"),
        ]
    for path in candidates:
        if path and os.path.exists(path):
            return path
    return shutil.which("google-chrome") or shutil.which("google-chrome-stable") or shutil.which("chromium")


def default_profile_dir() -> Path:
    """用户日常 Chrome 的默认 profile 目录（UWS_CHROME_PROFILE 可覆盖）。"""
    if env := os.environ.get("UWS_CHROME_PROFILE", "").strip():
        return Path(env).expanduser()
    home = Path.home()
    if sys.platform == "darwin":
        return home / "Library/Application Support/Google/Chrome"
    if sys.platform.startswith("linux"):
        return home / ".config/google-chrome"
    if sys.platform == "win32":
        local = os.environ.get("LOCALAPPDATA", "")
        return Path(local) / "Google/Chrome/User Data"
    return home / ".config/google-chrome"


def debug_profile_dir() -> Path:
    """uws-chrome-debug 持久调试 profile（UWS_CHROME_PROFILE 可覆盖）。"""
    if env := os.environ.get("UWS_CHROME_PROFILE", "").strip():
        return Path(env).expanduser()
    return Path.home() / DEBUG_PROFILE_DIRNAME


def _endpoint(port: int) -> str:
    return f"http://127.0.0.1:{port}"


def probe_cdp(url: str, timeout: float = 1.2) -> bool:
    """探测 CDP HTTP 端点是否就绪（/json/version 且带 webSocketDebuggerUrl）。"""
    try:
        with urllib.request.urlopen(url.rstrip("/") + "/json/version", timeout=timeout) as resp:  # noqa: S310 - 本地调试端点
            if resp.status != 200:
                return False
            data = json.loads(resp.read())
            return bool(data.get("webSocketDebuggerUrl"))
    except (OSError, ValueError, urllib.error.URLError):
        return False


def wait_cdp(url: str, timeout: float = CHROME_READY_TIMEOUT) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if probe_cdp(url):
            return True
        time.sleep(0.3)
    return False


def chrome_launch_allowed() -> bool:
    """是否已显式授权 UWS 在 CDP 端点不可达时拉起 Chrome。"""
    return os.environ.get(CHROME_LAUNCH_PERMISSION_ENV, "").strip().lower() in (
        "1",
        "yes",
        "true",
        "y",
    )


def _launch_permission_error(url: str) -> RuntimeError:
    return RuntimeError(
        f"CDP 调试端点不可达：{url}。为避免打断当前工作，UWS 默认不会自动启动 Chrome。"
        f"请先手动启动并附着该端点；如明确授权本次由 UWS 启动，请设 "
        f"{CHROME_LAUNCH_PERMISSION_ENV}=1 后重试。"
    )


def is_chrome_running(profile_dir: Path) -> bool:
    """通过 profile 目录的 SingletonLock 判断 Chrome 是否在运行（macOS/Linux）。"""
    lock = profile_dir / "SingletonLock"
    return lock.exists()


def _chrome_app_name(bin_path: str) -> str:
    lowered = bin_path.lower()
    if "chromium" in lowered:
        return "Chromium"
    return "Google Chrome"


def quit_chrome(bin_path: str) -> None:
    """优雅退出日常 Chrome（macOS 用 AppleScript；其他平台提示手动关闭）。"""
    if sys.platform == "darwin":
        app = _chrome_app_name(bin_path)
        subprocess.run(
            ["osascript", "-e", f'tell application "{app}" to quit'],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        return
    raise RuntimeError(
        "日常 Chrome 正在运行但无调试端口：请先手动关闭 Chrome 后重试，"
        "或显式指定已开启调试端口的 URL（--attach http://127.0.0.1:<port>）。"
    )


def app_bundle(bin_path: str) -> str | None:
    """从可执行路径推导 macOS .app 包（用于 `open -g` 后台启动）；非 bundle 返回 None。"""
    marker = ".app/Contents/MacOS/"
    idx = bin_path.find(marker)
    if idx < 0:
        return None
    return bin_path[: idx + len(".app")]


def window_flags() -> list[str]:
    """uws 专属小窗的冷启动参数（UWS_WINDOW_SIZE=WxH；UWS_WINDOW_POS=X,Y）。

    首次拉起调试 Chrome 时直接给小窗尺寸，避免一上来就铺满屏幕压住用户窗口；
    运行期每个会话的专属小窗由引擎用 CDP Target.createTarget 建立（见 pkg/browser/window.go）。
    """
    size = os.environ.get("UWS_WINDOW_SIZE", "").strip() or DEFAULT_WINDOW_SIZE
    flags = [f"--window-size={size.replace('X', 'x').replace('x', ',').replace('*', ',')}"]
    pos = os.environ.get("UWS_WINDOW_POS", "").strip()
    if pos and pos.lower() not in ("auto", "none", "keep"):
        flags.append(f"--window-position={pos}")
    return flags


# 反节流启动参数（2026-09-12，GLOBAL #92）：调试 Chrome 常驻后台/被用户窗口遮挡时，
# macOS 与 Chrome 会节流渲染与定时器，CDP 物理输入会挂起到超时
# （Input.dispatchMouseEvent: context deadline exceeded）。这三条让后台窗口保持正常
# 节奏；只对**新启动**的调试 Chrome 进程生效（已在跑的实例须重启）。
ANTI_THROTTLE_FLAGS = [
    "--disable-backgrounding-occluded-windows",
    "--disable-renderer-backgrounding",
    "--disable-background-timer-throttling",
]


def launch_chrome(port: int, profile_dir: Path, *, restore: bool = False) -> None:
    """以远程调试端口启动 Chrome（仅在显式授权后；GUI 焦点须真机验收）。"""
    bin_path = chrome_bin()
    if not bin_path:
        raise RuntimeError("未找到 Chrome/Chromium（可设 UWS_CHROME_BIN 指定路径）")
    profile_dir.mkdir(parents=True, exist_ok=True)
    args = [
        f"--remote-debugging-port={port}",
        "--remote-allow-origins=*",
        f"--user-data-dir={profile_dir}",
        "--no-first-run",
        "--no-default-browser-check",
        *ANTI_THROTTLE_FLAGS,
        *window_flags(),
    ]
    if restore:
        args.append("--restore-last-session")
    args.append("about:blank")
    popen_kwargs: dict[str, object] = {
        "stdout": subprocess.DEVNULL,
        "stderr": subprocess.DEVNULL,
    }
    # 独立会话（Setsid）：避免 Chrome 随发起命令的 exec 会话回收被杀（引擎 daemon 同款）。
    if os.name == "posix":
        popen_kwargs["start_new_session"] = True
    # 直接执行 Chrome 二进制（不经 LaunchServices）：这是「同一 .app + 不同
    # user-data-dir 多实例」的可靠方式（`open -n` 会被 Chrome 多实例策略拒绝）。
    # 进程启动本身不等于已满足 R1；焦点行为必须在真机上另行验收。
    subprocess.Popen([bin_path, *args], **popen_kwargs)  # type: ignore[arg-type]


def _lock_owner_pid(profile_dir: Path) -> int | None:
    """从 SingletonLock 读 Chrome 主进程 PID（macOS: 符号链接 host.local-<pid>；
    Linux: 普通文件内含 PID）。无法解析返回 None。"""
    lock = profile_dir / "SingletonLock"
    try:
        raw = os.readlink(lock) if lock.is_symlink() else lock.read_text(
            encoding="utf-8", errors="ignore"
        )
    except OSError:
        return None
    raw = raw.strip().rsplit("-", 1)[-1] if "-" in raw else raw.strip()
    try:
        return int(raw)
    except ValueError:
        return None


def _pid_alive(pid: int) -> bool:
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return False
    return True


def clear_stale_debug_locks(profile_dir: Path) -> None:
    """调试 profile 的 Chrome 单实例锁清理：锁指向的主进程已死才删，绝不误伤活实例。

    2026-09-03 实测：Chrome 被会话回收后留下 stale SingletonLock，后续拉起误判“已有实例”
    直接退出。清理只在端口未监听且 owner PID 已死时执行（Agent 不应手工 rm）。
    """
    owner = _lock_owner_pid(profile_dir)
    if owner is not None and _pid_alive(owner):
        return
    for name in STALE_LOCK_FILES:
        path = profile_dir / name
        try:
            if path.is_symlink() or path.exists():
                path.unlink()
        except OSError:
            pass


def ensure_debug(port: int = DEBUG_PORT, *, profile_dir: Path | None = None) -> str:
    """确保 uws-chrome-debug 持久调试 Chrome 就绪，返回 CDP 端点 URL。"""
    url = _endpoint(port)
    if probe_cdp(url):
        return url
    if not chrome_launch_allowed():
        raise _launch_permission_error(url)
    profile = profile_dir or debug_profile_dir()
    if profile.exists():
        clear_stale_debug_locks(profile)
    launch_chrome(port, profile, restore=False)
    if not wait_cdp(url):
        raise RuntimeError(f"uws-chrome-debug 调试 Chrome 启动超时（{url}）")
    return url


def _confirm_default_restart() -> bool:
    """重启日常 Chrome 前的人工确认：非交互环境需 UWS_ATTACH_CONFIRM=1。"""
    env = os.environ.get("UWS_ATTACH_CONFIRM", "").strip().lower()
    if env in ("1", "yes", "true", "y"):
        return True
    if sys.stdin.isatty():
        try:
            answer = input(
                "检测到日常 Chrome 正在运行：附着需要先退出并带调试端口重启 "
                "（窗口/标签会恢复、登录态保留）。继续？[y/N] "
            )
        except EOFError:
            return False
        return answer.strip().lower() in ("y", "yes")
    return False


def ensure_default(port: int = DEFAULT_PORT, *, profile_dir: Path | None = None) -> str:
    """确保用户日常 Chrome 带调试端口运行，返回 CDP 端点 URL。"""
    url = _endpoint(port)
    if probe_cdp(url):
        return url
    if not chrome_launch_allowed():
        raise _launch_permission_error(url)
    profile = profile_dir or default_profile_dir()
    if not profile.exists():
        raise RuntimeError(f"未找到 Chrome 默认 profile：{profile}（可用 UWS_CHROME_PROFILE 指定）")
    bin_path = chrome_bin()
    if not bin_path:
        raise RuntimeError("未找到 Chrome/Chromium（可设 UWS_CHROME_BIN 指定路径）")
    if is_chrome_running(profile):
        if not _confirm_default_restart():
            raise RuntimeError(
                "日常 Chrome 正在运行但无调试端口：附着需要重启 Chrome。"
                "请在交互终端确认，或设 UWS_ATTACH_CONFIRM=1 自动确认；"
                "也可先手动关闭 Chrome 后重试。"
            )
        quit_chrome(bin_path)
        deadline = time.monotonic() + QUIT_TIMEOUT
        while time.monotonic() < deadline and is_chrome_running(profile):
            time.sleep(0.3)
        if is_chrome_running(profile):
            raise RuntimeError("日常 Chrome 退出超时，请手动关闭后重试")
    launch_chrome(port, profile, restore=True)
    if not wait_cdp(url):
        raise RuntimeError(f"日常 Chrome 带调试端口启动超时（{url}）")
    return url


def resolve_attach(
    value: str | None,
    *,
    default_port: int = DEFAULT_PORT,
    allow_launch: bool = True,
) -> str:
    """解析 --attach 取值，返回 http://127.0.0.1:<port> CDP 端点 URL。

    debug → uws-chrome-debug（9222）；default → 用户日常 Chrome（9223）；
    显式 URL 原样返回；其余取值报错。
    """
    raw = (value or "").strip()
    if raw in ("debug", ""):
        url = _endpoint(DEBUG_PORT)
        if allow_launch:
            return ensure_debug()
        if not probe_cdp(url):
            raise RuntimeError(f"只读观察不会启动浏览器；调试 Chrome 当前不可用（{url}）")
        return url
    if raw == "default":
        url = _endpoint(default_port)
        if allow_launch:
            return ensure_default(default_port)
        if not probe_cdp(url):
            raise RuntimeError(f"只读观察不会启动或重启浏览器；默认 Chrome 调试端点当前不可用（{url}）")
        return url
    if raw.startswith("http://") or raw.startswith("https://") or raw.startswith("ws://"):
        return raw.rstrip("/")
    raise ValueError("--attach 必须是 debug / default / 显式 CDP URL（如 http://127.0.0.1:9222）")
