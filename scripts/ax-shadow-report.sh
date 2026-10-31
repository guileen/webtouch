#!/bin/sh
# AX 影子观测汇总（2026-09-23）：量化「VLM 视觉调用有多少次本来能从页面语义树读到」。
#
# 数据源：引擎日志里的 [AX-SHADOW] 行（pkg/engine/ax_shadow.go 在每次 VLM 调用前
# 采样一次；只观测不改行为）。用法：
#   scripts/ax-shadow-report.sh [engine.log 路径]
# 默认 ~/.uws/engine.log。
set -eu

LOG="${1:-$HOME/.uws/engine.log}"
if [ ! -f "$LOG" ]; then
  echo "找不到日志：$LOG" >&2
  exit 1
fi

grep -o '\[AX-SHADOW\].*' "$LOG" 2>/dev/null | awk '
/AX-SHADOW/ {
  total++
  ax = "unknown"
  if (match($0, /ax=[a-z]+/)) { ax = substr($0, RSTART + 3, RLENGTH - 3) }
  axc[ax]++
  hit = ($0 ~ /hit=true/ && $0 !~ /hit=false/)
  if (hit) { hits++ }
  if (hit && $0 ~ /ref_hit=true/) { refhits++ }
  reason = "?"
  if (match($0, /reason=[^ ]+/)) { reason = substr($0, RSTART + 7, RLENGTH - 7) }
  rc[reason]++
  lines = ""
  if (match($0, /lines=[0-9]+/)) { lines = substr($0, RSTART + 6, RLENGTH - 6) }
  if (match($0, /ref_lines=[0-9]+/)) { refs += substr($0, RSTART + 10, RLENGTH - 10) }
  if (match($0, /ax=ok lines=[0-9]+/)) { sumlines += substr($0, RSTART + 12, RLENGTH - 12); sized++ }
  samples[total] = $0
}
END {
  printf "AX 影子观测：VLM 调用前采样 %d 次\n", total
  if (total == 0) { print "（暂无数据：跑一次会让 VLM 兜底的动作后再看）"; exit }
  printf "- 页面语义树规模：平均 %.0f 行，其中可交互 ref 行平均 %.1f 行（样本 %d）\n", sumlines / sized, refs / sized, sized
  printf "- AX 树可用性："
  for (k in axc) printf "%s=%d ", k, axc[k]
  printf "\n- 目标语义仅靠 AX 树即可读到：%d 次（%.0f%%）——这些是可省的视觉调用\n", hits, 100 * hits / total
  printf "- 其中命中可交互 ref 行（可直接 click:node）：%d 次\n", refhits
  printf "- 触发原因分布："
  for (k in rc) printf "%s=%d ", k, rc[k]
  printf "\n- 最近 3 条样例：\n"
  start = total - 2; if (start < 1) start = 1
  for (i = start; i <= total; i++) print "  " samples[i]
}'
