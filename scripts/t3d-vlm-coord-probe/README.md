# VLM 坐标兜底取证夹具（T-3 观察 ③ / T-1 后续复核）

用途：真机量出 VLM 兜底点击**到底歪在哪一环**——「模型瞄不准」还是「引擎换算错」。
结论与读数见 [`docs/evidence/vlm-coord-forensics-2026-09-30.md`](../../docs/evidence/vlm-coord-forensics-2026-09-30.md)。

## 为什么这样设计

1. **必须走 VLM 兜底**：夹具全部画在 canvas 上（DOM 无文字、无 `cursor:pointer` 叶子）⇒ 语义候选数恒为 0
   ⇒ 确定性路径 `no_match` ⇒ 强制走 VLM 坐标路径（`vlm: main-control click plan-point`）。
2. **要拿到「真实落点」而不只是「引擎以为的落点」**：页面在 `mousedown/mouseup/click` 上挂捕获监听，
   把 `clientX/clientY` + 当轮视口/DPR/画布几何 + 各目标真实中心，用 `new Image().src=/__probe?...`
   信标回传给本地 HTTPServer ⇒ 落点坐标进 HTTP 日志。CDP 派发的鼠标事件是可信事件，页面读到的就是真值。
   （不用 CDP 二次附着去读：`uws run` 结束会收走它自己的目标，信标不受影响。）
3. **两轴分开记**：
   - 换算误差 = 页面实测落点 − 引擎 `point_x/point_y`（恒 0 = 比例→CSS px→物理点击这一链保真）；
   - 模型估计误差 = 引擎落点 − 真实元素中心（VLM 瞄得准不准）。
   两者可并存，分别记录，不强行归单一根因。

## 夹具

| 文件 | 场景 | 目标 |
|---|---|---|
| `fixture-clean.html` | 干净页、四角＋中心 | `ALPHA` `BRAVO` `CHARLIE` `DELTA` `ECHO`（195×81） |
| `fixture-sizes.html` | 尺寸梯度 | `SETTINGS`（120×44）、`EDIT`（60×20） |
| `fixture-rows.html` | 同类行列表（12 行同形） | `ROW 03`、`ROW 10`（960×40） |
| `fixture-clutter.html` | 视觉杂波 ＋ 小按钮 | `RENAME`（72×22，另有 `EXPORT` 干扰） |
| `fixture-anchor.html` | 同杂波页 ＋ **带编号参考点**（列 A–F / 行 1–4 ＋ 四边百分刻度） | 同上；用于「给参考物有没有用」对照 |
| `fixture-ruler.html` | 同杂波页 ＋ **四边百分刻度**（无编号点） | 同上 |

每个夹具都自带信标；`read-page.mjs` 可旁路读 `window.__probe` 并按需截图（含 PNG 像素尺寸）。

## 复跑

```bash
# 1) 夹具 HTTP + 自建 Chrome（独立 profile，勿动用户 9222）
mkdir -p /tmp/wtx-vlm-probe && cp scripts/t3d-vlm-coord-probe/fixture-*.html /tmp/wtx-vlm-probe/
launchctl submit -l wtx-vlm-http -o /tmp/wtx-vlm-probe/http.log -e /tmp/wtx-vlm-probe/http.err \
  -- .venv/bin/python -m http.server 8791 --bind 127.0.0.1 --directory /tmp/wtx-vlm-probe
launchctl submit -l wtx-vlm-chrome -o /tmp/wtx-vlm-probe/chrome.log -e /tmp/wtx-vlm-probe/chrome.err \
  -- "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
     --remote-debugging-port=9224 --user-data-dir=/tmp/wtx-vlm-probe/profile \
     --no-first-run --no-default-browser-check --headless=new \
     --window-size=1440,900 --force-device-scale-factor=2 --hide-scrollbars about:blank
# 注：必须 launchd 托管；普通 nohup 起的进程会随发起它的会话结束被回收。

# 2) 逐目标跑点击（每目标一个新会话＝一次干净 page load；目标语义勿复用，免撞 learned 复用）
uws run --attach http://127.0.0.1:9224 \
  --target-url http://127.0.0.1:8791/fixture-clutter.html \
  --session probe-rename --plan 'click:RENAME'

# 3) 配对分析（引擎日志 plan-point 行 ↔ 页面信标落点）
python3 scripts/t3d-vlm-coord-probe/analyze.py /tmp/wtx-vlm-probe/http.err
```

## 读表须知

- `analyze.py` 按时间顺序配对：第 i 个带 click 的信标批 ↔ 第 i 条夹具页 `plan-point` 行。
  若有 VLM 点击未派发（落点在视口外等），批数会少于行数 ⇒ 表里会出现「未配对」提示，需人工对齐。
- 命中判定用「落点是否落在**该元素矩形**内」（`误差/半宽 ≤ 1`），比「看起来对不对」严格。
- 夹具文案避开「购买／支付／下单」，以免被既有 `confirmation_required` 拦成 `PAUSED`。

## 离线单臂探针（2026-09-30 起：分辨率 vs 参考线对照）

真机路径一次只能采一臂，做多因素对照太慢；`ask-offline.py` 离线复刻 `pkg/vlm` 的同一次调用
（同模型、同款提示词含能力清单、`temperature=0`、`thinking:disabled`），只换输入图与 `detail` ⇒ 单变量对照。
等价性已校验：`product/low` 臂对 `fixture-clutter` 的 `RENAME` 复跑得 `(0.548, 0.716)`，
与真机 `plan-point` 读数逐位一致。读数与结论见
[`docs/evidence/vlm-detail-resolution-ab-2026-09-30.md`](../../docs/evidence/vlm-detail-resolution-ab-2026-09-30.md)。

```bash
# 图：无头 Chrome 1440×813 @DPR2（2880×1626，与真机同规格）
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu \
  --hide-scrollbars --window-size=1440,813 --force-device-scale-factor=2 \
  --screenshot=/tmp/A-base.png "file://$PWD/scripts/t3d-vlm-coord-probe/fixture-clutter.html"
# 臂：DEEPSEEK_API_KEY 已在环境里（引擎同源）
python3 scripts/t3d-vlm-coord-probe/ask-offline.py /tmp/A-base.png product low  1440 813
python3 scripts/t3d-vlm-coord-probe/ask-offline.py /tmp/A-base.png product high 1440 813
TRUTH_X=216 TRUTH_Y=122 TARGET=ALPHA python3 scripts/t3d-vlm-coord-probe/ask-offline.py /tmp/A-clean.png product low 1440 813
# 只换答题约定（比例 / 像素 / 百分比 / 报真实图像尺寸）：ask-convention.py <png> <conv> <detail> [w h]
python3 scripts/t3d-vlm-coord-probe/ask-convention.py /tmp/A-base.png pixels low 1440 813
```
