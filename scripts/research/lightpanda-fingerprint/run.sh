#!/usr/bin/env bash
# WebTouch · Lightpanda 指纹调研 demo 一键入口（研究用，不碰主线）
#
# 做三件事：① 用本地靶站记录「连接层 HTTP 头」；② 同一份 JS 探针分别打 Lightpanda
# 与官方 Chrome（独立 profile / 独立端口，绝不碰用户 9222 与 uws daemon）；
# ③ 生成逐项对照表。产出全部落 docs/evidence/research/lightpanda-fingerprint-<日期>/。
#
# 用法：
#   scripts/research/lightpanda-fingerprint/run.sh [--out DIR] [--skip-download] [--keep-running]
#
# 环境变量：
#   LP_BIN       Lightpanda 可执行文件路径（默认 $LP_CACHE/lightpanda-x86_64-macos）
#   LP_CACHE     二进制缓存目录（默认 /private/tmp/lightpanda-research）
#   CHROME_BIN   Chrome 可执行文件（默认自动探测）
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
out=""; keep=0; skip_dl=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --out) out="$2"; shift 2 ;;
    --keep-running) keep=1; shift ;;
    --skip-download) skip_dl=1; shift ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$out" ]] || out="$repo/docs/evidence/research/lightpanda-fingerprint-$(date +%Y-%m-%d)"
mkdir -p "$out"

LP_CACHE="${LP_CACHE:-/private/tmp/lightpanda-research}"
LP_BIN="${LP_BIN:-$LP_CACHE/lightpanda-x86_64-macos}"
LP_VER="${LP_VERSION:-1.0.0}"
CHROME_BIN="${CHROME_BIN:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
HEADER_PORT="${HEADER_PORT:-8799}"
LP_PORT="${LP_PORT:-9223}"
LP_ZH_PORT="${LP_ZH_PORT:-9224}"
LP_FULL_PORT="${LP_FULL_PORT:-9225}"
CHROME_PORT="${CHROME_PORT:-9333}"
CHROME_PROFILE="$LP_CACHE/chrome-probe-profile"

if [[ ! -x "$LP_BIN" ]]; then
  if [[ $skip_dl == 1 ]]; then echo "缺少 $LP_BIN（--skip-download 且文件不存在）" >&2; exit 1; fi
  echo "==> 下载 Lightpanda $LP_VER ($(uname -m))"
  mkdir -p "$LP_CACHE"
  curl -sL -o "$LP_BIN" "https://github.com/lightpanda-io/browser/releases/download/$LP_VER/lightpanda-$(uname -m)-macos"
  chmod +x "$LP_BIN"
fi
echo "==> Lightpanda: $("$LP_BIN" version)"

# 记录一条硬边界：Lightpanda 明确拒绝把自己伪装成 Chrome（UA 里不许出现 Mozilla）。
echo "==> 记录 --user-agent 伪装拒绝（预期 fatal）"
"$LP_BIN" serve --port 19226 --user-agent \
  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36" \
  >"$out/lightpanda-ua-refusal.txt" 2>&1 || true

pids=()
cleanup() {
  if [[ $keep == 1 ]]; then echo "==> --keep-running：保留进程 PID ${pids[*]:-}"; return; fi
  for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done
  pkill -f "remote-debugging-port=$CHROME_PORT" 2>/dev/null || true
}
trap cleanup EXIT

echo "==> 启动本地靶站 :$HEADER_PORT"
node "$here/header-server.mjs" --port "$HEADER_PORT" --dump "$out/http-headers.json" &
pids+=($!)
sleep 1

echo "==> 启动 Lightpanda CDP :$LP_PORT（默认身份）"
HOME="$LP_CACHE/home" XDG_CACHE_HOME="$LP_CACHE/home/.cache" LIGHTPANDA_DISABLE_TELEMETRY="${LIGHTPANDA_DISABLE_TELEMETRY:-true}" \
  "$LP_BIN" serve --host 127.0.0.1 --port "$LP_PORT" --log-level info >"$out/lightpanda.log" 2>&1 &
pids+=($!)

echo "==> 启动 Lightpanda CDP :$LP_ZH_PORT（--locale zh-CN --timezone Asia/Shanghai）"
HOME="$LP_CACHE/home" XDG_CACHE_HOME="$LP_CACHE/home/.cache" LIGHTPANDA_DISABLE_TELEMETRY="${LIGHTPANDA_DISABLE_TELEMETRY:-true}" \
  "$LP_BIN" serve --host 127.0.0.1 --port "$LP_ZH_PORT" --locale zh-CN --timezone Asia/Shanghai \
  --user-agent-suffix " uws-research" --log-level info >"$out/lightpanda-zh.log" 2>&1 &
pids+=($!)

echo "==> 启动 Lightpanda CDP :$LP_FULL_PORT（--load-resources 全开，验指纹面是否受资源加载影响）"
HOME="$LP_CACHE/home" XDG_CACHE_HOME="$LP_CACHE/home/.cache" LIGHTPANDA_DISABLE_TELEMETRY="${LIGHTPANDA_DISABLE_TELEMETRY:-true}" \
  "$LP_BIN" serve --host 127.0.0.1 --port "$LP_FULL_PORT" --locale zh-CN --timezone Asia/Shanghai \
  --load-resources iframe --load-resources image --load-resources stylesheet --load-resources worker \
  --log-level info >"$out/lightpanda-full.log" 2>&1 &
pids+=($!)

echo "==> 启动 Chrome 对照组 :$CHROME_PORT（独立 profile，不动 9222）"
rm -rf "$CHROME_PROFILE"
"$CHROME_BIN" --headless=new --remote-debugging-port="$CHROME_PORT" --user-data-dir="$CHROME_PROFILE" \
  --no-first-run --no-default-browser-check --disable-blink-features=AutomationControlled \
  about:blank >"$out/chrome.log" 2>&1 &
pids+=($!)

for _ in $(seq 1 40); do
  curl -sf "http://127.0.0.1:$LP_PORT/json/version" >/dev/null 2>&1 && curl -sf "http://127.0.0.1:$CHROME_PORT/json/version" >/dev/null 2>&1 && break
  sleep 0.5
done

probe() { # label port settle
  echo "==> 探针 $1 (:$2)"
  node "$here/cdp-probe.mjs" --endpoint "http://127.0.0.1:$2" --url "http://127.0.0.1:$HEADER_PORT/" \
    --label "$1" --settle "$3" --out "$out/$1.json" --headers-dump "http://127.0.0.1:$HEADER_PORT/__dump" \
    --screenshot "$out/$1.png" 2>&1 | tee "$out/$1.probe.log"
}
probe lightpanda "$LP_PORT" 3000 || true
probe lightpanda-zh "$LP_ZH_PORT" 3000 || true
probe lightpanda-full "$LP_FULL_PORT" 3000 || true
probe chrome "$CHROME_PORT" 1500 || true

echo "==> 渲染保真度小检（背景色块 / 绝对定位 / 中文 / canvas）"
node "$here/render-check.mjs" --endpoint "http://127.0.0.1:$LP_FULL_PORT" --label lightpanda --out-prefix "$out/render-lightpanda" || true
node "$here/render-check.mjs" --endpoint "http://127.0.0.1:$CHROME_PORT" --label chrome --out-prefix "$out/render-chrome" || true

echo "==> 生成对照表"
node "$here/compare.mjs" --out "$out/compare.md" \
  "lightpanda=$out/lightpanda.json" "lightpanda-zh=$out/lightpanda-zh.json" \
  "lightpanda-full=$out/lightpanda-full.json" "chrome=$out/chrome.json" || true
echo "==> 产出目录：$out"
ls -la "$out"
