#!/bin/sh
# 生成 E4「候选块 A/B/C/D 批问」实验输入（2026-09-30 单变量对照，读数见
# docs/evidence/vlm-detail-resolution-ab-2026-09-30.md §6.4）。
# 输入＝ fixture-clutter.html 的 DPR2 截图（CSS 1440×813 ⇒ 2880×1626）。
# 真值：RENAME 中心 CSS(893,585)、EXPORT(893,650)，按钮 72×22 ⇒ device 半宽 72、半高 22。
# 用法：mk-candidates.sh <shot-2880x1626.png> <outdir>
set -e
SRC=$1
OUT=${2:-.}
crop() {  # $1=字母 $2=magick 裁剪几何（device px）
  magick "$SRC" -crop "$2" +repage -background white -gravity northwest \
    -splice 60x50 -fill black -pointsize 30 -annotate +8+4 "$1" "$OUT/blk-$1.png"
}
crop A 640x170+1466+1100   # 含 RENAME（按钮 y 1148..1192），不含 EXPORT（起于 1278）
crop B 640x170+1466+1210   # 含 EXPORT（1278..1322），不含 RENAME
crop C 640x170+480+400     # 纯噪声（左栏）
crop D 640x170+1680+300    # 纯噪声（右上）
magick montage "$OUT"/blk-A.png "$OUT"/blk-B.png "$OUT"/blk-C.png "$OUT"/blk-D.png \
  -tile 2x2 -geometry +6+6 -background white "$OUT/cand-abcd.png"
# 变体：整页上画四个互不重叠的候选框（同夹具，A 仍唯一含 RENAME）
magick "$SRC" -fill none -stroke '#e11d48' -strokewidth 6 \
  -draw "rectangle 1466,1030 2106,1270" -draw "rectangle 1466,1272 2106,1470" \
  -draw "rectangle 480,400 1120,640" -draw "rectangle 1680,200 2320,440" \
  -fill '#e11d48' -stroke none -pointsize 40 -gravity northwest \
  -draw "text 1478,1036 'A'" -draw "text 1478,1278 'B'" -draw "text 492,406 'C'" -draw "text 1692,206 'D'" \
  "$OUT/page-boxed.png"
