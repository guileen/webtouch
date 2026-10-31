#!/usr/bin/env bash
# 过程存证：截图 + 环境元数据（用法见 docs/evidence/README.md）
#   evidence_shot.sh <阶段目录> <标签> [x,y,w,h] [--note "说明"] [--dry-run]
set -uo pipefail

dir="${1:-}"; tag="${2:-}"; region=""; note=""; dry=0
if [ -z "$dir" ] || [ -z "$tag" ]; then
  echo "用法: evidence_shot.sh <阶段目录> <标签> [x,y,w,h] [--note 说明] [--dry-run]"; exit 2
fi
shift 2
while [ $# -gt 0 ]; do
  case "$1" in
    --note) note="${2:-}"; shift 2 ;;
    --dry-run) dry=1; shift ;;
    *) region="$1"; shift ;;
  esac
done

root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$dir"

count=0
for f in "$dir"/*.png; do [ -f "$f" ] && count=$((count + 1)); done
n=$(printf '%02d' $((count + 1)))
out="$dir/$n-$tag.png"

if [ "$dry" = "1" ]; then
  echo "[dry-run] 将执行: screencapture -x ${region:+-R $region }$out"
else
  if [ -n "$region" ]; then screencapture -x -R "$region" "$out"; else screencapture -x "$out"; fi
fi

size="-"
if [ -f "$out" ]; then
  size=$(sips -g pixelWidth -g pixelHeight "$out" 2>/dev/null | awk '/pixelWidth|pixelHeight/{printf "%s", $2 (NR%2?"x":"")}')
  [ -n "$size" ] || size="-"
fi
front=$(osascript -e 'tell application "System Events" to get name of first application process whose frontmost is true' 2>/dev/null || echo "-")
engine=$("$root/bin/webtouch" daemon status 2>/dev/null | sed -n 's/.*引擎 运行中: addr=\([^ ]*\) pid=\([0-9]*\).*version=\([^ ]*\).*/\1 pid=\2 \3/p' | head -1)
[ -n "$engine" ] || engine="-"
chrome=$(curl -s --max-time 2 http://127.0.0.1:9222/json/version 2>/dev/null | sed -n 's/.*"Browser": "\([^"]*\)".*/\1/p')
[ -n "$chrome" ] || chrome="-"
ts=$(date '+%Y-%m-%d %H:%M:%S')

line="| $ts | $n-$tag | ${region:-全屏} | $size | 前台=$front | 引擎=$engine | Chrome=$chrome | ${note:-} |"
json=$(printf '{"ts":"%s","file":"%s","region":"%s","size":"%s","front_app":"%s","engine":"%s","chrome":"%s","note":"%s"}' \
  "$ts" "$n-$tag.png" "${region:-fullscreen}" "$size" "$front" "$engine" "$chrome" "$note")

if [ "$dry" = "1" ]; then
  echo "[dry-run] $line"
  echo "[dry-run] $json"
else
  echo "$line" >> "$dir/meta.md"
  echo "$json" >> "$dir/meta.jsonl"
  echo "已存证: $out"
fi
