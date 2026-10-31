#!/usr/bin/env bash
# S1 取证：遮挡场景下物理输入必达（三组对照 + 前台应用采样 + 小窗截图）
#
#   bash scripts/verify_s1_occlusion.sh
#
# 对照组（同一代码、只切护栏开关）：
#   baseline    UWS_FOCUS_EMULATION=0 UWS_INPUT_RETRY_ATTEMPTS=1  真基线（复现遮挡挂起）
#   focus-only  UWS_FOCUS_EMULATION=1 UWS_INPUT_RETRY_ATTEMPTS=1  只开焦点仿真
#   full        UWS_FOCUS_EMULATION=1 UWS_INPUT_RETRY_ATTEMPTS=3  焦点仿真 + 超时重试
#
# 产出目录：docs/evidence/viewer/2026-09-12-s1-occlusion-input/
#   test-<组>-<日期>.log        每组多轮的 go test 全量输出（含 PASS/FAIL 与失败原因）
#   front-app-<组>-<日期>.log   每秒采样前台应用（R1：自动化不得改变用户前台应用）
#   windows-<组>-<日期>.log     抓取当刻窗口清单（含小窗 id/尺寸/坐标）
#   occlusion-<组>-<日期>.log   遮挡判定：目标主窗口是否覆盖住小窗矩形
#   NN-s1-<组>-windowN.png      小窗窗口级截图（穿透遮挡）
set -uo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
dir="$root/docs/evidence/viewer/2026-09-12-s1-occlusion-input"
mkdir -p "$dir"
PROBE=/tmp/macprobe
[ -x "$PROBE" ] || swiftc -O scripts/macprobe.swift -o "$PROBE"
export UWS_RUN_CHROME_E2E=1
export UWS_E2E_ATTACH_URL="${UWS_E2E_ATTACH_URL:-http://127.0.0.1:9222}"
export GOCACHE="${GOCACHE:-/tmp/uws-gocache}" GOTMPDIR=/tmp
day="$(date +%Y-%m-%d)"
runs="${UWS_S1_RUNS:-2}"

# 遮挡判定：小窗矩形是否被「前台应用」的某个普通窗口完全覆盖。
occlusion_report() { # snapshot_file front_app small_rect
  python3 - "$1" "$2" <<'PY'
import sys
snap, front = sys.argv[1], sys.argv[2]
wins = []
for line in open(snap, encoding='utf-8', errors='replace'):
    parts = line.rstrip('\n').split('\t')
    if len(parts) < 4:
        continue
    owner, size, pos = parts[1], parts[2], parts[3]
    try:
        w, h = (int(v) for v in size.split('x'))
        x, y = (int(v) for v in pos.split(','))
    except ValueError:
        continue
    wins.append((owner, x, y, w, h))
smalls = [w for w in wins if 480 <= w[3] <= 560 and 500 <= w[4] <= 600]
if not smalls:
    print("小窗未在窗口清单中出现（可能已被关闭）")
    raise SystemExit
for s in smalls:
    _, sx, sy, sw, sh = s
    cover = [w for w in wins if w[0] == front and w[2] > 200 and w[1] <= sx and w[2] <= sy
             and w[1] + w[3] >= sx + sw and w[2] + w[4] >= sy + sh]
    print(f"小窗 {sx},{sy} {sw}x{sh} 前台应用={front} 遮挡={'是' if cover else '否'}"
          + (f" 覆盖窗口={cover[0][1:]}" if cover else ""))
PY
}

run_arm() { # phase serial fe attempts
  local phase="$1" serial="$2" fe="$3" attempts="$4"
  local log="$dir/test-$phase-$day.log" flog="$dir/front-app-$phase-$day.log"
  local wlog="$dir/windows-$phase-$day.log" olog="$dir/occlusion-$phase-$day.log"
  : > "$log"; : > "$flog"; : > "$wlog"; : > "$olog"
  printf '=== phase=%s focus_emulation=%s retry_attempts=%s runs=%s ===\n' \
    "$phase" "$fe" "$attempts" "$runs" | tee -a "$log"
  ( while :; do printf '%s\t%s\n' "$(date '+%H:%M:%S')" "$("$PROBE" front 2>/dev/null || echo '?')" >> "$flog"; sleep 1; done ) &
  local sampler=$!
  local i
  for i in $(seq 1 "$runs"); do
    UWS_FOCUS_EMULATION="$fe" UWS_INPUT_RETRY_ATTEMPTS="$attempts" UWS_E2E_HOLD_MS=2500 \
      go test -count=1 ./pkg/browser -run TestCDPSessionOwnWindowKeepsFocus -v >>"$log" 2>&1 &
    local tp=$!
    sleep 2
    "$PROBE" list > "$wlog" 2>/dev/null
    occlusion_report "$wlog" "$("$PROBE" front 2>/dev/null || echo '?')" >> "$olog"
    local n=0 id owner size pos title
    while IFS=$'\t' read -r id owner size pos title; do
      [ "$owner" = "Google Chrome" ] || continue
      case "$size" in 5*x5*) ;; *) continue ;; esac
      n=$((n + 1))
      [ "$n" -le 2 ] && screencapture -x -l "$id" \
        "$dir/$(printf '%02d' "$serial")-s1-$phase-window$n.png" 2>/dev/null
    done < "$wlog"
    wait "$tp"
    printf 'run%s rc=%s 小窗截图=%s\n' "$i" "$?" "$n" >> "$log"
  done
  kill "$sampler" 2>/dev/null; wait "$sampler" 2>/dev/null
  printf 'phase=%s 通过=%s 失败=%s\n' "$phase" \
    "$(grep -c '^--- PASS' "$log")" "$(grep -c '^--- FAIL' "$log")" | tee -a "$log"
  grep -E '^--- (PASS|FAIL)|click_failed|context deadline|小窗应可见|未启用焦点仿真' "$log" | tail -12
}

run_arm baseline   1 0 1
sleep 1
run_arm focus-only 2 1 1
sleep 1
run_arm full       3 1 3
echo "证据目录：$dir"
