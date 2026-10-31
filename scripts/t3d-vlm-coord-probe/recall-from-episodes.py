#!/usr/bin/env python3
"""从 ~/.uws/episodes/*.jsonl 量：候选池大小、GT 是否在池内、GT 位序、几何可得性。
只输出聚合统计，不打印任何页面文本原文。

⚠ 两条使用禁忌（2026-09-30 实测确认）：
  1) candidates[] 是**训练投影**（`pkg/browser/cdp_session_snapshot.go`：超 K=64 时按「距受体
     派发点欧氏距离最近」升序保留）⇒ 数组第 0 条就是受体附近那条，**GT 恒排最前是构造使然**，
     本脚本的「GT 在池内 / 位序 <K」**不能**当召回率用（会得到虚假高命中）。
  2) `page_candidate_count`（页面侧节点全集，**锚点序**）与 `cand_count`（引擎决策池）是**两个池**：
     真实页面上决策池 0 率 82–97%、中位 0（L1 需探针显式置信度、L2 需 KB 同义词、在线 ONNX 未接线），
     页面侧池才是那个中位 30–62 的池；谈「候选池大小」必须点名是哪个（案卷 §8.6）。
  3) engine_pool 是第一批遗留口径（池基本为空），与已登记的「空候选根因」同一件事；
     要谈现口径请只看 page_projection 分组。
"""
import glob, io, json, os, statistics as st
from collections import Counter


def episode_paths():
    """Episode 日志：UWS_EPISODES_DIR，否则 $HOME/.uws/episodes。"""
    root = os.environ.get('UWS_EPISODES_DIR') or os.path.join(os.path.expanduser('~'), '.uws', 'episodes')
    return sorted(glob.glob(os.path.join(root, 'episodes-*.jsonl')))


rows = []
for f in episode_paths():
    for line in io.open(f, encoding='utf-8'):
        line = line.strip()
        if not line:
            continue
        try:
            d = json.loads(line)
        except Exception:
            continue
        ts = (d.get('intent') or {}).get('target_semantic') or ''
        if ts.startswith('node:') or ts.startswith('ref:'):
            continue                      # 节点寻址不需要候选
        cands = d.get('candidates') or []
        if not cands:
            rows.append({'file': f[-13:-6], 'src': (d.get('production') or {}).get('candidates_source', '?'),
                         'pool': 0, 'gt_idx': None, 'geom': False, 'conf': False,
                         'state': (d.get('posterior') or {}).get('state', '?'),
                         'action': (d.get('intent') or {}).get('expected_action', '?')})
            continue
        gt = [c for c in cands if c.get('is_ground_truth')]
        pool = len(cands)
        rows.append({
            'file': f[-13:-6], 'src': (d.get('production') or {}).get('candidates_source', '?'),
            'pool': pool,
            'gt_idx': (min(c['index'] for c in gt) if gt else None),
            'geom': all((c.get('w') or 0) > 1 and (c.get('h') or 0) > 1 for c in cands),
            'gt_geom': bool(gt) and (gt[0].get('w') or 0) > 1 and (gt[0].get('h') or 0) > 1,
            'conf': any((c.get('confidence') or 0) > 0 for c in cands),
            'state': (d.get('posterior') or {}).get('state', '?'),
            'action': (d.get('intent') or {}).get('expected_action', '?'),
        })

print('总样本（语义寻址）=%d' % len(rows))
print('按候选来源：', dict(Counter(r['src'] for r in rows)))
print('按后验状态：', dict(Counter(r['state'] for r in rows)))
print('按动作：', dict(Counter(r['action'] for r in rows)))
print()
pooled = [r for r in rows if r['pool'] > 0]
print('【池非空】N=%d（占 %.0f%%）' % (len(pooled), 100.0 * len(pooled) / max(1, len(rows))))
sizes = sorted(r['pool'] for r in pooled)
if sizes:
    q = lambda p: sizes[min(len(sizes) - 1, int(p * len(sizes)))]
    print('  池大小：min=%d P50=%d P75=%d P90=%d P99=%d max=%d mean=%.1f'
          % (sizes[0], q(.5), q(.75), q(.9), q(.99), sizes[-1], st.mean(sizes)))
    print('  池大小分布桶：', dict(sorted(Counter(
        ('1' if s == 1 else '2-3' if s <= 3 else '4-8' if s <= 8 else '9-16' if s <= 16 else '17-64' if s <= 64 else '>64')
        for s in sizes).items())))
    withgt = [r for r in pooled if r['gt_idx'] is not None]
    print('【GT 在池内】%d / %d = %.0f%%' % (len(withgt), len(pooled), 100.0 * len(withgt) / len(pooled)))
    for K in (1, 2, 4, 8, 16, 64):
        hit = sum(1 for r in withgt if r['gt_idx'] < K)
        print('  池内位序 < %-2d（≈前 %-2d 块）：%3d / %d = %5.1f%%' % (K, K, hit, len(pooled), 100.0 * hit / len(pooled)))
    print('【几何可得】池内每个候选都有 w/h>1 的样本：%d / %d = %.0f%%'
          % (sum(1 for r in pooled if r['geom']), len(pooled), 100.0 * sum(1 for r in pooled if r['geom']) / len(pooled)))
    print('【GT 有几何】%d / %d = %.0f%%'
          % (sum(1 for r in pooled if r.get('gt_geom')), len(pooled), 100.0 * sum(1 for r in pooled if r.get('gt_geom')) / len(pooled)))
    print('【候选带显式置信度】%d / %d = %.0f%%'
          % (sum(1 for r in pooled if r['conf']), len(pooled), 100.0 * sum(1 for r in pooled if r['conf']) / len(pooled)))
    print()
    print('按来源分组的 GT 池内率 / 池大小中位：')
    for src in sorted(set(r['src'] for r in pooled)):
        g = [r for r in pooled if r['src'] == src]
        wg = [r for r in g if r['gt_idx'] is not None]
        print('  %-26s N=%-4d GT在池内 %5.1f%%  池大小中位=%d' %
              (src, len(g), 100.0 * len(wg) / len(g), sorted(r['pool'] for r in g)[len(g) // 2]))

# ── GT 位序直方：证明上面的「锚点投影」判断（现口径）────────────────────
hist = Counter(r['gt_idx'] for r in pooled if r['gt_idx'] is not None and r['src'] == 'page_projection')
print()
print('GT 位序直方（page_projection，前 8 桶）：', dict(sorted(hist.items())[:8]))
print('※ GT 恒在 index=0 ＝锚点投影的构造结果；量召回必须另取「决策时刻的池 + 排序」。')

# ── 决策池 vs 页面侧候选集：两个「池」不是一回事（2026-09-30 追加）────────
# page_candidate_count = 页面侧只读快照的交互节点数（**锚点序**截断到 64）；
# cand_count           = 引擎决策池（candidatesFor 出的匹配器候选）条数。
# 真实公开页上后者几乎恒空（L1 需探针显式置信度、L2 需 KB 同义词背书；在线 ONNX 未接线）。
by_day = {}
dec_pool_pos, dec_pool_zero = Counter(), Counter()
for f in episode_paths():
    d0 = os.path.basename(f)[len('episodes-'):-len('.jsonl')]
    for line in io.open(f, encoding='utf-8'):
        line = line.strip()
        if not line:
            continue
        try:
            d = json.loads(line)
        except Exception:
            continue
        p = d.get('production') or {}
        if (p.get('candidates_source') or '') != 'page_projection':
            continue
        c = p.get('cand_count') or 0
        page = p.get('page_candidate_count') or 0
        e = by_day.setdefault(d0, {'n': 0, 'pool0': 0, 'pool': [], 'page': []})
        e['n'] += 1
        e['pool'].append(c)
        e['page'].append(page)
        if c == 0:
            e['pool0'] += 1
        (dec_pool_pos if c > 0 else dec_pool_zero)[p.get('decision') or '?'] += 1

print()
print('决策池 vs 页面侧候选集（只看 page_projection）：')
for day in sorted(by_day):
    e = by_day[day]
    pl = sorted(e['pool'])
    pg = sorted(e['page'])
    med = lambda xs: xs[min(len(xs) - 1, len(xs) // 2)]
    print('  %s n=%-4d 决策池 cand_count：0 率=%3.0f%% 中位=%d 最大=%d | 页面侧 page_candidate_count：中位=%d 最大=%d'
          % (day, e['n'], 100.0 * e['pool0'] / e['n'], med(pl), pl[-1], med(pg), pg[-1]))
print('  决策池非空时的 decision：', dict(dec_pool_pos.most_common(6)))
print('  决策池为空时的 decision：', dict(dec_pool_zero.most_common(6)))
print('※ 「页面侧池大、决策池恒空」＝T-30 的候选来源应是页面侧节点全集，且其今日**只有锚点序**')
print('  （锚点＝已选中目标 ⇒ 用它排「该问哪几块」是循环依赖）；相关性排序器是缺口，见案卷 §8.6。')
