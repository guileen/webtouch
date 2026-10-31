#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""T-31 站点会话身份 · 真机验收探针（契约 §9：S1 捕获 / S2 恢复 / S3 列表）。

    python3 scripts/verify_t31_site_conversation.py [channel_id]

只读性质：只打网关 HTTP API（urllib），不碰站点 DOM、不直接驱动浏览器；但会真实发问——
站点侧会产生测试对话：探针收尾默认只关页（`?purge=0`，**不删站点对话**，避免误删用户历史）；
设 `T31_PURGE=1` 则收尾**先清账再关页**（按契约 §14 删除本跑观察到的站点对话，用于批量复跑不留残留）；
清账走 `POST /v1/purge`（同步核验，删不掉的自动落待删除列表重试），默认关闭以免误删非本跑的对话。
**收尾纪律（2026-10-02）**：本进程建过的网关会话在正常结束、异常退出、Ctrl-C / SIGTERM
时都会被收掉（只关页、不删站点对话）；不再把测试页签留在浏览器里。

前置不满足（网关不通 / 未登录）⇒ 退出码 2 = **阻塞**，不算失败；有判据失败 ⇒ 退出码 1。
环境变量：UWS_GATEWAY（默认 http://127.0.0.1:8000）、T31_CHANNEL、T31_MODEL、
T31_PROMPT、T31_GAP_SEC（同站两轮间隔秒，默认 gemini 6 / deepseek 21，遵守站点低频门）。
产物：docs/evidence/pilots/t31-acceptance-<时间戳>.log（逐条原始读数）。
"""
from __future__ import annotations

import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

import atexit
import signal

GW = os.environ.get("UWS_GATEWAY", "http://127.0.0.1:8000").rstrip("/")
CHAN = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("T31_CHANNEL", "gemini-web")
PROMPT = os.environ.get("T31_PROMPT", "只回答两个字：收到")
GAP = float(os.environ.get("T31_GAP_SEC", "21" if "deepseek" in CHAN else "6"))
ROOT = Path(__file__).resolve().parent.parent
STAMP = time.strftime("%Y%m%d-%H%M%S")
LOGF = ROOT / "docs" / "evidence" / "pilots" / f"t31-acceptance-{STAMP}.log"
LINES: list[str] = []
PASS: list[tuple[str, str]] = []
FAIL: list[tuple[str, str]] = []
NOTIMPL: list[tuple[str, str]] = []


# CREATED 记录本进程建过的网关会话；退出（含中断）时统一收尾，避免测试页签留在浏览器里。
CREATED: list[str] = []
# OBSERVED 记录本跑在站点侧看到的对话 ID；T31_PURGE=1 时收尾清账用（契约 §14）。
OBSERVED: list[str] = []


def observe(cid) -> None:
    if cid and cid not in ("<缺失>", None) and cid not in OBSERVED:
        OBSERVED.append(cid)


def track(sid: str) -> None:
    if sid and sid not in CREATED:
        CREATED.append(sid)


# T31_PURGE=1：收尾时清账（删除本进程会话的站点对话），批量复跑不留残留（契约 §14）。
PURGE_ON_EXIT = os.environ.get("T31_PURGE", "").strip().lower() in ("1", "true", "yes", "on")


def close_session(sid: str, purge: bool = False) -> tuple[int, object]:
    """关页（默认不删站点对话；purge=True 则按 §14 一并清账）；幂等；返回 (HTTP 码, 响应)。"""
    code, data = api("DELETE", f"/v1/sessions/{sid}" + ("" if purge else "?purge=0"))
    if sid in CREATED:
        CREATED.remove(sid)
    return code, data


def purge_observed() -> None:
    """按契约 §14 清掉本跑在站点侧留下的对话（同步核验；删不掉自动落待删除列表）。"""
    for cid in list(OBSERVED):
        code, data = api("POST", "/v1/purge",
                         {"channel_id": CHAN, "conversation_id": cid,
                          "reason": "t31_probe_leftover"})
        st = data.get("status") if isinstance(data, dict) else None
        log(f"-- 清账 {cid} -> HTTP {code} status={st} verified="
            f"{data.get('verified') if isinstance(data, dict) else None}")


def cleanup_sessions() -> None:
    if PURGE_ON_EXIT and OBSERVED:
        try:
            purge_observed()
        except Exception:  # 清账失败不掩盖主流程结果
            pass
    for sid in list(CREATED):
        try:
            close_session(sid, purge=PURGE_ON_EXIT)
        except Exception:  # 收尾失败不掩盖主流程结果
            if sid in CREATED:
                CREATED.remove(sid)


def _on_signal(signum, _frame):  # noqa: ANN001
    cleanup_sessions()
    raise SystemExit(128 + signum)


atexit.register(cleanup_sessions)
signal.signal(signal.SIGINT, _on_signal)
signal.signal(signal.SIGTERM, _on_signal)


def log(msg: str) -> None:
    print(msg)
    LINES.append(msg)


def api(method: str, path: str, body: dict | None = None, timeout: float = 240.0):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(GW + path, data=data, method=method)
    if data is not None:
        req.add_header("content-type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw, code = resp.read().decode("utf-8", "replace"), resp.status
    except urllib.error.HTTPError as exc:
        raw, code = exc.read().decode("utf-8", "replace"), exc.code
    except Exception as exc:  # 连接失败 / 超时
        return 0, {"_error": f"{type(exc).__name__}: {exc}"}
    try:
        return code, json.loads(raw)
    except json.JSONDecodeError:
        return code, {"_raw": raw}


def check(name: str, ok: bool, detail: str, notimpl: bool = False) -> None:
    log(f"  [{'PASS' if ok else ('NOT_IMPL' if notimpl else 'FAIL')}] {name}：{detail}")
    (PASS if ok else (NOTIMPL if notimpl else FAIL)).append((name, detail))


def auth_status_of(obj, chan: str) -> str:
    """从 /v1/auth/status 找出该通道的状态；找不到返回 unknown。"""
    found = ["unknown"]

    def walk(v) -> None:
        if isinstance(v, dict):
            if v.get("channel_id") == chan and isinstance(v.get("status"), str):
                found[0] = v["status"]
            for x in v.values():
                walk(x)
        elif isinstance(v, list):
            for x in v:
                walk(x)

    walk(obj)
    return found[0]


def find_model(obj) -> str | None:
    hits: list[str] = []

    def walk(v) -> None:
        if isinstance(v, dict):
            ident = v.get("id")
            ch = v.get("channel") or v.get("channel_id") or v.get("uws_channel")
            if isinstance(ident, str) and ch == CHAN:
                hits.append(ident)
            for x in v.values():
                walk(x)
        elif isinstance(v, list):
            for x in v:
                walk(x)

    walk(obj)
    if not hits:
        return None
    for m in hits:  # 优先站点默认档（flash 系），避开模型档位切换这条无关路径
        if "flash" in m.lower():
            return m
    return hits[0]


def show(prefix: str, data) -> None:
    log(f"{prefix}{json.dumps(data, ensure_ascii=False)[:700]}")


def chat(sid: str, model: str, tag: str):
    code, data = api(
        "POST",
        "/internal/provider/chat",
        {"channel_id": CHAN, "model": model, "session_id": sid,
         "messages": [{"role": "user", "content": PROMPT}]},
    )
    if not isinstance(data, dict):
        data = {"_raw": data}
    cid = data.get("site_conversation_id", "<缺失>")
    purl = data.get("page_url", "<缺失>")
    log(f"  · {tag} HTTP {code} site_conversation_id={json.dumps(cid, ensure_ascii=False)} "
        f"page_url={json.dumps(purl, ensure_ascii=False)}")
    if code != 200:
        show("    原始响应：", data)
        if "model_switch" in json.dumps(data, ensure_ascii=False):
            log("    提示：档位切换失败（既有路径，非 T-31 判据）；可用 T31_MODEL=<默认档模型> 避开")
    return code, data, cid


def run() -> int:
    log(f"== T-31 真机验收 {time.strftime('%Y-%m-%d %H:%M:%S')} · 通道={CHAN} · 网关={GW} ==")
    log(f"   判据 C1 捕获 / C2 恢复 / C3 列表 / C4 反向 / C5 fail-closed；产物 {LOGF}")
    code, _ = api("GET", "/v1/health", timeout=10)
    if code != 200:
        log(f"阻塞(2)：网关不可达（{GW}/v1/health -> HTTP {code}）。先 `uws daemon status` 再跑。")
        return 2
    _, ast = api("GET", "/v1/auth/status")
    st = auth_status_of(ast, CHAN)
    if st in ("logged_out", "challenge"):
        log(f"阻塞(2)：{CHAN} 状态={st}（/v1/auth/status）——先人工登录/处理挑战再跑。")
        show("  auth/status：", ast)
        return 2
    if st != "logged_in":
        log(f"   注意：{CHAN} 状态={st}（未探测）——首轮请求会做一次只读探测，锁定则判阻塞/失败。")
    _, models = api("GET", "/internal/provider/models")
    model = os.environ.get("T31_MODEL") or find_model(models)
    if not model:
        log(f"阻塞(2)：内部网页模型目录里找不到通道 {CHAN} 的模型（可用 T31_MODEL 指定）。")
        return 2
    log(f"   模型={model} 两轮间隔={GAP}s")

    sid1 = f"s-t31-{STAMP}"
    code, created = api("POST", "/v1/sessions", {"channel_id": CHAN, "session_id": sid1})
    track(sid1)
    if code != 200:
        log(f"阻塞(2)：建会话失败 HTTP {code}")
        show("  原始响应：", created)
        return 2

    _, d1, cid1 = chat(sid1, model, "C1 第 1 轮")
    time.sleep(GAP)
    _, _, cid2 = chat(sid1, model, "C1 第 2 轮")
    observe(cid1); observe(cid2)
    check("C1 捕获", cid1 not in ("<缺失>", None) and cid2 == cid1,
          f"第 1 轮={cid1}，第 2 轮={cid2}（要求两轮均非空且相同）")

    if cid1 in ("<缺失>", None):
        check("C2 恢复", False, "第 1 轮无 ID，恢复无从验证（先修 C1）")
    else:
        sid2 = f"{sid1}-r"
        code, _ = close_session(sid1)
        log(f"-- 关页 {sid1}?purge=0 -> HTTP {code}（只关页，不删站点对话）")
        code, resumed = api("POST", "/v1/sessions",
                            {"channel_id": CHAN, "session_id": sid2, "site_conversation_id": cid1})
        track(sid2)
        log(f"-- 按 ID 重建 {sid2} -> HTTP {code}")
        show("  重建响应：", resumed)
        time.sleep(GAP)
        _, _, cid3 = chat(sid2, model, "C2 恢复后 1 轮")
        observe(cid3)
        check("C2 恢复", code == 200 and cid3 == cid1,
              f"重建 HTTP {code}，恢复后 ID={cid3}，期望回到 {cid1}（不新开对话）")
        code, lst = api("GET", f"/v1/sessions/{sid2}/conversations")
        needle = [x for x in (cid1, cid1.split("_")[-1]) if x]
        body = json.dumps(lst, ensure_ascii=False)
        if code in (404, 501):
            check("C3 列表", False, f"HTTP {code} —— S3 未见实现", notimpl=True)
        elif isinstance(lst, dict) and {"sessions", "tabs"} <= set(lst):
            check("C3 列表", False,
                  "S3 未见实现：该路径当前落进 GET /v1/sessions 分支，返回的是本网关会话快照",
                  notimpl=True)
        else:
            check("C3 列表", code == 200 and any(n in body for n in needle),
                  f"HTTP {code}；要求含 {cid1}；返回 {body[:300]}")
        close_session(sid2)

    sid3 = f"{sid1}-n"
    code, _ = api("POST", "/v1/sessions", {"channel_id": CHAN, "session_id": sid3})
    track(sid3)
    time.sleep(GAP)
    _, _, cid4 = chat(sid3, model, "C4 不带 ID 新会话")
    observe(cid4)
    check("C4 反向", code == 200 and cid4 not in ("<缺失>", None, cid1),
          f"新会话 ID={cid4}，必须 ≠ 旧 ID {cid1}（不带 ID 不得落到旧对话）")
    close_session(sid3)

    scid_ok = ("site_conversation_id" in d1 and isinstance(d1.get("site_conversation_id"), (str, type(None)))
               and "page_url" in d1 and isinstance(d1.get("page_url"), (str, type(None))))
    check("C5 fail-closed", scid_ok,
          f"字段必须存在且为 string/null：{json.dumps({k: d1.get(k, '<缺失>') for k in ('site_conversation_id', 'page_url')}, ensure_ascii=False)}")

    log("-- 结论 --")
    for name, detail in PASS:
        log(f"  PASS {name}：{detail}")
    for name, detail in FAIL:
        log(f"  FAIL {name}：{detail}")
    for name, detail in NOTIMPL:
        log(f"  NOT_IMPL {name}：{detail}")
    log(f"  合计 PASS {len(PASS)} / FAIL {len(FAIL)} / NOT_IMPL {len(NOTIMPL)}；"
        f"探针在站点侧留下若干测试对话（只关页未删，如需清理按契约 §14）")
    return 1 if (FAIL or NOTIMPL) else 0


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] in ("-h", "--help"):
        print(__doc__)
        sys.exit(0)
    code = run()
    LOGF.parent.mkdir(parents=True, exist_ok=True)
    LOGF.write_text("\n".join(LINES) + "\n", encoding="utf-8")
    print(f"\n探针日志：{LOGF}")
    sys.exit(code)
