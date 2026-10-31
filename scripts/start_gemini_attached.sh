#!/usr/bin/env bash

# 开发辅助脚本（Python API 时代产物）。2026-08-26 起桌面/CLI 已不要求手动 attach：
# uws-gateway 的 --attach-cdp 端点不可达时会自动拉起持久调试 Chrome。
# 本脚本仅保留给需要直接跑 Python API 转换层的开发场景。

# 进入 webtouch 目录执行
cd "$(dirname "$0")" || exit 1

# 退出时仅清理后台服务；调试 Chrome 保持运行（登录态持久复用）
trap 'echo ">>> 正在关闭后台服务（调试 Chrome 保持运行）..."; kill ${ENGINE_PID:-} ${API_PID:-} ${HTTP_PID:-} 2>/dev/null' EXIT

echo "=========================================="
echo "    🚀 UWS + Gemini 接管模式一键启动"
echo "=========================================="

# 1. 持久调试配置目录（与 DeepSeek 共用；Gemini 首次需登录一次 Google 账号，
#    登录后模型选择器才会解锁全部 3 档模型 + Extended thinking 开关）
UWS_CHROME_PROFILE="${HOME}/.uws-chrome-debug"
CHROME_BIN="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
echo ">>> 调试 Chrome 持久配置目录: $UWS_CHROME_PROFILE"

# 2. 若 9222 未监听，拉起带远程调试的 Chrome（首次需在弹出窗口登录 Gemini）
if ! lsof -nP -iTCP:9222 -sTCP:LISTEN >/dev/null 2>&1; then
  echo ">>> 正在拉起调试 Chrome（--remote-debugging-port=9222）..."
  nohup "$CHROME_BIN" \
    --remote-debugging-port=9222 \
    "--remote-allow-origins=*" \
    --user-data-dir="$UWS_CHROME_PROFILE" \
    --no-first-run --no-default-browser-check \
    --disable-backgrounding-occluded-windows \
    --disable-renderer-backgrounding \
    --disable-background-timer-throttling \
    "https://gemini.google.com" > /dev/null 2>&1 &
  disown
  echo ">>> 等待 Chrome 完全启动..."
  sleep 4
else
  echo ">>> 9222 已有调试实例，直接复用"
fi

# 3. 检查并在后台启动 uws-engine (如果没有的话)
echo ">>> 正在拉起底层 uws-engine (Go)..."
go run ./cmd/uws-engine/main.go --browser cdp > /tmp/uws-engine-gemini.log 2>&1 &
ENGINE_PID=$!
sleep 2

# 4. 在后台启动 uws API (Python) —— Gemini 站点
echo ">>> 正在拉起 UWS 转换层 API (Python)..."
export PYTHONPATH="$(pwd):$(pwd)/proto/gen/python"
.venv/bin/python -m coordinator.cli api --target llm_chat --url "https://gemini.google.com" --attach http://127.0.0.1:9222 --api-key uws_dev_key > /tmp/uws-api-gemini.log 2>&1 &
API_PID=$!
sleep 2

# 5. 测试前端随仓库维护（dev/gemini_test.html，多会话/多 Tab 控制台），
#    启动时不再重新生成，避免脚本与仓库文件双份漂移。
if [ ! -f dev/gemini_test.html ]; then
  echo ">>> 缺少 dev/gemini_test.html，请先恢复该文件" >&2
  exit 1
fi

# 启动一个小 HTTP 服务器来避免 File:// 的 CORS 限制
python3 -m http.server 18080 > /dev/null 2>&1 &
HTTP_PID=$!
sleep 1

echo ">>> 正在打开 Gemini 页面及测试控制台..."
"$CHROME_BIN" --user-data-dir="$UWS_CHROME_PROFILE" "http://localhost:18080/dev/gemini_test.html" > /dev/null 2>&1 &
disown

echo "=========================================="
echo " ✅ 服务已全部就绪（Ctrl+C 仅关闭引擎/API/静态服务，调试 Chrome 保持运行）"
echo " 🌐 API 服务监听在: http://127.0.0.1:8000"
echo " 📋 测试页面已在您的 Chrome 新标签页打开"
echo " 🔐 首次使用请在调试 Chrome 窗口登录 Google 账号（游客态只能测 Flash-Lite）"
echo " 📄 若遇到问题请查看日志： /tmp/uws-engine-gemini.log 和 /tmp/uws-api-gemini.log"
echo "=========================================="

wait
