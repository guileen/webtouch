#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""T-31 判据 4 后半：两个不同 site_conversation_id 并发不得互相污染（真机探针）。

用法：python3 scripts/verify_t31_concurrency.py   （网关默认 http://127.0.0.1:8000）
结构：造两条对话并核验在栏 → 顺序对照（应 PASS）→ 并发恢复+并发发问（应 PASS）→ 收尾清账。
收尾只清本进程创建的两条站点对话（契约 §14 只删工具记录过的 ID）。
判定：顺序 PASS 而并发 FAIL ⇒ 判据 4 后半不成立（见 Tasks.md `T-35`）。
"""
import json, threading, time, urllib.error, urllib.request
from pathlib import Path

GW = "http://127.0.0.1:8000"; CHAN = "gemini-web"
PROMPT = "只回答两个字：收到"; GAP = 6.0
STAMP = time.strftime("%Y%m%d-%H%M%S")
ROOT = Path("/Users/gl/hz/aliyun-terraform/labs/world-agent/webtouch")
LOGF = ROOT / "docs" / "evidence" / "pilots" / f"t31-concurrency-{STAMP}.log"
LINES, RES, SIDS = [], {"pass": [], "fail": []}, []


def log(m): print(m, flush=True); LINES.append(m)


def api(method, path, body=None, timeout=240.0):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(GW + path, data=data, method=method)
    if data is not None: req.add_header("content-type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw, code = r.read().decode("utf-8", "replace"), r.status
    except urllib.error.HTTPError as e:
        raw, code = e.read().decode("utf-8", "replace"), e.code
    except Exception as e:
        return 0, {"_error": f"{type(e).__name__}: {e}"}
    try: return code, json.loads(raw)
    except json.JSONDecodeError: return code, {"_raw": raw}


def create(sid, site_id=None):
    body = {"channel_id": CHAN, "session_id": sid}
    if site_id: body["site_conversation_id"] = site_id
    code, d = api("POST", "/v1/sessions", body); SIDS.append(sid)
    return code, d


def chat(sid, model, tag):
    code, d = api("POST", "/internal/provider/chat",
                  {"channel_id": CHAN, "model": model, "session_id": sid,
                   "messages": [{"role": "user", "content": PROMPT}]})
    if not isinstance(d, dict): d = {"_raw": d}
    log(f"    · {tag} HTTP {code} id={d.get('site_conversation_id')} url={d.get('page_url')}")
    return code, d.get("site_conversation_id")


def listed(sid, cid):
    code, lst = api("GET", f"/v1/sessions/{sid}/conversations")
    return code == 200 and cid in json.dumps(lst, ensure_ascii=False)


def check(name, ok, detail):
    (RES["pass"] if ok else RES["fail"]).append((name, detail))
    log(f"  [{'PASS' if ok else 'FAIL'}] {name}：{detail}")


def main():
    log(f"== T-31 并发串台 严格版 {time.strftime('%Y-%m-%d %H:%M:%S')} · {GW} ==")
    model = "gemini-3.5-flash-lite"

    sA, sB = f"s-t31c3-{STAMP}-A", f"s-t31c3-{STAMP}-B"
    create(sA); code, cA = chat(sA, model, "造A"); time.sleep(GAP)
    create(sB); code, cB = chat(sB, model, "造B"); time.sleep(GAP)
    exA, exB = listed(sA, cA), listed(sB, cB)
    check("D0 两条对话均在侧栏", exA and exB and cA != cB, f"A={cA}(在栏={exA}) B={cB}(在栏={exB})")
    if not (exA and exB and cA != cB): return 2
    for s in (sA, sB): api("DELETE", f"/v1/sessions/{s}?purge=0")
    time.sleep(3)

    # 对照：顺序恢复
    sA2, sB2 = f"s-t31c3-{STAMP}-A2", f"s-t31c3-{STAMP}-B2"
    create(sA2, cA); _, r1 = chat(sA2, model, "顺序A2"); time.sleep(GAP)
    create(sB2, cB); _, r2 = chat(sB2, model, "顺序B2"); time.sleep(GAP)
    check("D1 顺序恢复 A→A", r1 == cA, f"期望 {cA} 得 {r1}")
    check("D2 顺序恢复 B→B", r2 == cB, f"期望 {cB} 得 {r2}")
    for s in (sA2, sB2): api("DELETE", f"/v1/sessions/{s}?purge=0")
    time.sleep(3)

    # 复现：并发恢复 + 并发发问
    out, res = {}, {}
    sA3, sB3 = f"s-t31c3-{STAMP}-A3", f"s-t31c3-{STAMP}-B3"
    def mk(tag, sid, site): out[tag] = create(sid, site)[0]
    ta = threading.Thread(target=mk, args=("A", sA3, cA)); tb = threading.Thread(target=mk, args=("B", sB3, cB))
    t0 = time.time(); ta.start(); tb.start(); ta.join(); tb.join()
    log(f"    · 并发建会话 {time.time()-t0:.1f}s A={out['A']} B={out['B']}")
    def ask(tag, sid): res[tag] = chat(sid, model, f"并发{tag}3")[1]
    ta = threading.Thread(target=ask, args=("A", sA3)); tb = threading.Thread(target=ask, args=("B", sB3))
    t1 = time.time(); ta.start(); tb.start(); ta.join(); tb.join()
    log(f"    · 并发发问 {time.time()-t1:.1f}s")
    check("D3 并发 A→A", res["A"] == cA, f"期望 {cA} 得 {res['A']}")
    check("D4 并发 B→B", res["B"] == cB, f"期望 {cB} 得 {res['B']}")
    check("D5 两路未共落同一对话", res["A"] != res["B"], f"A={res['A']} B={res['B']}（相同即串台）")

    log("-- 收尾：只清本进程创建的两条站点对话 --")
    for sid, expect in ((sA3, cA), (sB3, cB)):
        code, d = api("DELETE", f"/v1/sessions/{sid}")
        s = json.dumps(d, ensure_ascii=False)
        ok = "deleted" in s or "已入待删除" in s
        log(f"    · purge {sid}（应删 {expect}）-> HTTP {code} {s[:220]}")
        check(f"D6 清账 {expect}", ok, s[:160])
    for s in SIDS:
        if s not in (sA3, sB3): api("DELETE", f"/v1/sessions/{s}?purge=0")

    log("-- 结论 --")
    log(f"  PASS {len(RES['pass'])} / FAIL {len(RES['fail'])}")
    for n, d in RES["pass"]: log(f"  PASS {n}：{d}")
    for n, d in RES["fail"]: log(f"  FAIL {n}：{d}")
    LOGF.write_text("\n".join(LINES) + "\n", encoding="utf-8"); log(f"  产物 {LOGF}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
