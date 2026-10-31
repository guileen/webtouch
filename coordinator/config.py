"""UWS 配置加载（纯 stdlib，与 cmd/uws/config.go 的 YAML 子集同构）。

CLI ``uws config set`` 写出的 ~/.uws/config.yaml 是平铺的
``section:\\n  key: value`` 结构；协调器运行时只读不写，保持零依赖。
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import Mapping


def uws_config_dir() -> str:
    """配置目录：UWS_CONFIG_DIR 环境变量可覆盖（测试/容器化用）。"""
    return os.environ.get("UWS_CONFIG_DIR") or str(Path.home() / ".uws")


def uws_config_path() -> Path:
    return Path(uws_config_dir()) / "config.yaml"


def load_uws_config(path: str | os.PathLike[str] | None = None) -> dict[str, str]:
    """解析极简 YAML 子集，返回 ``"section.key" -> value`` 的平铺映射。

    与 cmd/uws/config.go 的 readConfig 语义对齐：注释/空行跳过、值去引号；
    文件不存在返回空 dict（零配置默认，调用方自行降级）。
    """
    p = Path(path) if path is not None else uws_config_path()
    values: dict[str, str] = {}
    if not p.exists():
        return values
    section = ""
    for raw in p.read_text(encoding="utf-8").splitlines():
        line = raw.rstrip("\n")
        trimmed = line.strip()
        if not trimmed or trimmed.startswith("#"):
            continue
        if not line.startswith((" ", "\t")) and trimmed.endswith(":"):
            section = trimmed[:-1].strip()
            continue
        if not section or ":" not in line:
            continue
        idx = line.index(":")
        key = line[:idx].strip()
        val = line[idx + 1 :].strip().strip('"').strip("'")
        if key:
            values[f"{section}.{key}"] = val
    return values


def config_int(values: Mapping[str, str], key: str, default: int) -> int:
    """读取整型配置；缺失或非法时返回 default（不抛错，安全降级）。"""
    raw = (values.get(key) or "").strip()
    try:
        return int(raw)
    except (TypeError, ValueError):
        return default
