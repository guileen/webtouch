#!/usr/bin/env python3
"""VLM 坐标兜底取证配对分析（T-3 观察 ③ / T-1 后续复核，2026-09-30）。

把「引擎日志里的 VLM 原始读数」与「夹具页经 Image 信标回传的真实落点」按序配对，
输出逐样本表并给出两条分轴结论：
  · 换算环节误差 = 页面实测落点 − 引擎换算落点（恒 0 才说明换算链保真）
  · 模型估计误差 = 引擎换算落点 − 页面真实元素中心（VLM 瞄得准不准）

用法：
  python3 analyze.py <probe-log> [engine-log]
    probe-log  夹具 HTTPServer 的日志（stderr 或 stdout 均可；含 /__probe? 行）
    engine-log 引擎日志，默认 ~/.uws/engine.log

配对方式：两侧都按时间顺序，第 i 个「带 click 的信标批」↔ 第 i 条 plan-point 行。
批的页别按 drawn 目标名判定（ROW * → rows；EDIT → sizes；RENAME → clutter；其余 → clean），
因为 HTTP 请求行里不含页面 URL；批内目标几何（中心/尺寸）即该轮真实几何。
注意：若某次 VLM 点击没能派发（如落点在视口外），批数会少于行数，配对会错位——
此时会打印「未配对（批内无该语义）」提示，需人工对齐再读结论。
"""
import io, json, os, re, statistics, sys, urllib.parse

CLASSIFY = (
    ('rows',    lambda names: any(n.startswith('ROW ') for n in names)),
    ('sizes',   lambda names: 'EDIT' in names),
    ('clutter', lambda names: 'RENAME' in names),
    ('clean',   lambda names: True),
)

PAT = re.compile(
    r'vlm: main-control click plan-point url=(\S+) semantic="([^"]+)" '
    r'x_ratio=([\d.eE+-]+) y_ratio=([\d.eE+-]+) curW=(\d+) curH=(\d+) '
    r'origin_x=(-?\d+) origin_y=(-?\d+) origin_w=(-?\d+) origin_h=(-?\d+) '
    r'point_x=(-?\d+) point_y=(-?\d+)')

# 只取夹具页（避免把引擎日志里他会话/他页面的 plan-point 行混进配对）
FIXTURE = re.compile(r'/(?:probe\d?|fixture-[a-z]+)\.html(?:\?|$)')


def classify(names):
    for label, pred in CLASSIFY:
        if pred(names):
            return label
    return 'clean'


# 配对两侧必须用同一排序：夹具页类固定顺序（同一次日志里出现多个夹具页时，
# 引擎侧若按 URL 出现序、夹具侧按页类序，就会整表错位——2026-09-30 实测踩到）。
PAGE_ORDER = ('clean', 'sizes', 'rows', 'clutter')


def page_class(url):
    for label in ('rows', 'sizes', 'clutter'):
        if label in url:
            return label
    return 'clean'


def load_vlm(log_path):
    """引擎侧读数，按页分组保序。"""
    out = {}
    for ln in io.open(log_path, encoding='utf-8', errors='replace'):
        m = PAT.search(ln)
        if not m or not FIXTURE.search(m.group(1)):
            continue
        url = m.group(1)
        out.setdefault(url, []).append(dict(
            sem=m.group(2), xr=float(m.group(3)), yr=float(m.group(4)),
            curW=int(m.group(5)), curH=int(m.group(6)),
            px=int(m.group(11)), py=int(m.group(12)), ts=ln[:19]))
    return out


def load_clicks(probe_logs):
    """夹具侧信标：每次 page load 开一批，批内 click 即真实落点。"""
    batches = []
    for f in probe_logs:
        if not os.path.exists(f):
            continue
        for ln in io.open(f, encoding='utf-8', errors='replace'):
            if '/__probe?' not in ln:
                continue
            try:
                q = urllib.parse.parse_qs(urllib.parse.urlparse(ln.split('"')[1]).query)
            except Exception:
                continue
            m = json.loads(q['m'][0])
            drawn = {t['name']: t for t in m.get('drawn', [])}
            kind = q.get('kind', [''])[0]
            cls = classify(set(drawn))
            if kind in ('ready', 'load'):
                batches.append(dict(cls=cls, names=set(drawn), innerW=m['innerW'], innerH=m['innerH'],
                                    dpr=m['dpr'], cvW=m['cvW'], cvH=m['cvH'], drawn=drawn, click=None))
            elif kind == 'event' and batches and batches[-1]['cls'] == cls:
                if q['type'][0] == 'click':
                    batches[-1]['click'] = (int(q['cx'][0]), int(q['cy'][0]))
    return batches


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 1
    probe_logs = [sys.argv[1]]
    engine_log = sys.argv[2] if len(sys.argv) > 2 else os.path.expanduser('~/.uws/engine.log')
    vlm = load_vlm(engine_log)
    clicks = {}
    for b in load_clicks(probe_logs):
        if b['click']:
            clicks.setdefault(b['cls'], []).append(b)

    print('| 序 | 页 | 目标语义 | 真实元素中心(css) | 元素尺寸 | VLM 比例 | 引擎换算落点 | 页面实测落点 | 换算误差 | 模型估计误差 | 误差/半宽 | 命中 |')
    print('|---|---|---|---|---|---|---|---|---|---|---|---|')
    runs = [v for cls in PAGE_ORDER for url, vs in vlm.items() if page_class(url) == cls for v in vs]
    flat = [b for cls in PAGE_ORDER for b in clicks.get(cls, [])]
    conv_all, dx_all, dy_all, hits, n = set(), [], [], 0, 0
    for i, v in enumerate(runs):
        b = flat[i] if i < len(flat) else None
        t = (b or {}).get('drawn', {}).get(v['sem'])
        if not t:
            print('| ? | ? | %s | 未配对（批内无该语义，两侧顺序可能已错位） |' % v['sem'])
            continue
        n += 1
        hit = b['click']
        conv = (hit[0] - v['px'], hit[1] - v['py'])
        conv_all.add(conv)
        dx, dy = v['px'] - t['cx'], v['py'] - t['cy']
        dx_all.append(dx); dy_all.append(dy)
        inside = abs(dx) <= t['w'] / 2 and abs(dy) <= t['h'] / 2
        hits += 1 if inside else 0
        print('| S%d | %s | %s | (%d, %d) | %d×%d | (%.3f, %.3f) | (%d, %d) | (%d, %d) | %+d, %+d | (%+.0f, %+.0f) | (%.2f, %.2f) | %s |' % (
            n, b['cls'], v['sem'], t['cx'], t['cy'], t['w'], t['h'], v['xr'], v['yr'], v['px'], v['py'],
            hit[0], hit[1], conv[0], conv[1], dx, dy,
            abs(dx) / (t['w'] / 2), abs(dy) / (t['h'] / 2), '是' if inside else '否'))
    print()
    print('样本数=%d' % n)
    print('换算环节误差集合 = %s → 换算链保真：%s' % (
        conv_all, '是（恒为 0）' if conv_all and conv_all <= {(0, 0)} else '否/不足'))
    if dx_all:
        print('模型估计误差 x: mean=%+.1f [%+.0f, %+.0f] | y: mean=%+.1f [%+.0f, %+.0f] ；命中 %d/%d' % (
            statistics.mean(dx_all), min(dx_all), max(dx_all),
            statistics.mean(dy_all), min(dy_all), max(dy_all), hits, n))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
