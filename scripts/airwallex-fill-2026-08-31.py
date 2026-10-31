#!/usr/bin/env python3
"""Airwallex 表单 VLM 填写重跑（2026-08-31）。

附着 uws-chrome-debug（9222，登录态保留）→ 打开产品目录 →
「新产品」→ VLM input 兜底填写产品名称/描述/价格 → 逐步截图留证。

用法（webtouch 目录）：
  .venv/bin/python scripts/airwallex-fill-2026-08-31.py
"""

from __future__ import annotations

import pathlib
import sys
import time
from datetime import datetime

ROOT = pathlib.Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "proto/gen/python"))

from coordinator.attach import ensure_debug  # noqa: E402
from coordinator.engine_client import EngineError  # noqa: E402
from coordinator.grpc_client import GrpcEngineClient  # noqa: E402

OUT = pathlib.Path("/private/tmp/airwallex-evidence-2026-08-31")
OUT.mkdir(parents=True, exist_ok=True)

ENGINE_PORT = 50051
API_KEY = "uws_dev_key"
PRODUCTS_URL = "https://www.airwallex.com/app/billing/products"


def shot(client, target_id: str, name: str) -> pathlib.Path | None:
    try:
        png = client.capture_screenshot(target_id)
        path = OUT / f"{datetime.now():%H%M%S}-{name}.png"
        path.write_bytes(png)
        print(f"  shot {path.name} ({len(png)} bytes)")
        return path
    except Exception as exc:  # noqa: BLE001
        print(f"  shot {name} failed: {exc}")
        return None


def dump_tree(client, target_id: str, name: str) -> None:
    try:
        tree = client.export_page_tree(target_id)
        path = OUT / f"{datetime.now():%H%M%S}-{name}.tree.txt"
        path.write_text(tree, encoding="utf-8")
        print(f"  tree {path.name} ({len(tree)} chars)")
    except Exception as exc:  # noqa: BLE001
        print(f"  tree {name} failed: {exc}")


def act(client, ctx: str, step: int, semantic: str, action: str, value: str | None = None,
        timeout_ms: int = 4000):
    intent = f"intent-{ctx}-{step}"
    try:
        r = client.execute_action(intent, semantic, action, value=value, timeout_ms=timeout_ms)
        print(f"  [{step}] {action} {semantic!r} -> success={r.success} "
              f"conf={r.confidence:.3f} state={r.context_state} reason={r.error_reason}")
        return r
    except EngineError as exc:
        print(f"  [{step}] {action} {semantic!r} -> EngineError({exc.code}) {exc.message}")
        return exc


def main() -> int:
    cdp = ensure_debug()
    print(f"CDP endpoint: {cdp}")
    client = GrpcEngineClient(port=ENGINE_PORT, api_key=API_KEY)
    ctx = f"airwallex-{datetime.now():%H%M%S}"
    init = client.initialize_context(
        task_id=f"task-{ctx}",
        initial_url=PRODUCTS_URL,
        attach_cdp_url=cdp,
        context_name=ctx,
    )
    print("init:", init)
    target_id = init.target_id
    time.sleep(5)
    shot(client, target_id, "01-after-init")
    dump_tree(client, target_id, "01-after-init")

    step = 1
    # 尝试语义点击「新产品」（规则库未覆盖 → VLM 主控兜底）
    act(client, ctx, step, "新产品", "click", timeout_ms=6000)
    step += 1
    time.sleep(4)
    shot(client, target_id, "02-after-click-create")
    dump_tree(client, target_id, "02-after-click-create")

    # 填写产品名称 / 描述（VLM input 兜底）
    act(client, ctx, step, "产品名称", "input", value="auto-renewable week")
    step += 1
    time.sleep(2)
    act(client, ctx, step, "描述", "input", value="订阅有效期内，可阅读本书城所有短篇故事")
    step += 1
    time.sleep(2)
    shot(client, target_id, "03-after-name-desc")
    dump_tree(client, target_id, "03-after-name-desc")

    print(f"\n证据目录: {OUT}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
