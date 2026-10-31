#!/usr/bin/env bash
# WebTouch Personal AI Bus 一键启动脚本
# 协同编排：调试 Chrome (9222) + 底层引擎 (50051) + 协议网关 (8000)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

CHROME_BIN="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
PROFILE_DIR="${HOME}/.uws-chrome-debug"

echo "=================================================="
echo "    🚀 WebTouch Personal AI Bus 一键拉起"
echo "=================================================="

# 1. 确保调试 Chrome 在运行（9222 端口）
if ! curl -s http://127.0.0.1:9222/json/version >/dev/null 2>&1; then
    echo ">>> [1/3] 正在启动调试 Chrome (端口 9222，配置目录: ${PROFILE_DIR})..."
    nohup "${CHROME_BIN}" \
        --remote-debugging-port=9222 \
        --remote-allow-origins="*" \
        --user-data-dir="${PROFILE_DIR}" \
        --no-first-run --no-default-browser-check \
        --disable-backgrounding-occluded-windows \
        --disable-renderer-backgrounding \
        --disable-background-timer-throttling \
        --window-size=360,540 about:blank >/dev/null 2>&1 &
    disown
    
    # 等待 Chrome 端口就绪
    for i in {1..20}; do
        if curl -s http://127.0.0.1:9222/json/version >/dev/null 2>&1; then
            echo ">>> Chrome 已就绪。"
            break
        fi
        sleep 0.5
    done
else
    echo ">>> [1/3] 调试 Chrome 已在运行 (http://127.0.0.1:9222)，直接复用登录态。"
fi

# 2. 确保引擎在运行（50051 端口）。正式名 webtouch-engine，uws-engine 为兼容软链。
if ! lsof -nP -iTCP:50051 -sTCP:LISTEN >/dev/null 2>&1; then
    echo ">>> [2/3] 正在启动 webtouch-engine (gRPC 127.0.0.1:50051)..."
    if [ ! -e ./bin/webtouch-engine ] && [ ! -e ./bin/uws-engine ]; then
        echo ">>> 正在编译 ./bin/webtouch-engine..."
        mkdir -p bin
        go build -o ./bin/webtouch-engine ./cmd/uws-engine
        ln -sfn webtouch-engine ./bin/uws-engine
    fi
    if [ -e ./bin/webtouch-engine ]; then
        ENGINE_BIN=./bin/webtouch-engine
    else
        ENGINE_BIN=./bin/uws-engine
    fi
    nohup "${ENGINE_BIN}" --browser cdp --grpc-addr 127.0.0.1:50051 --log-path "${HOME}/.uws/engine-onlinelogs.db" >/dev/null 2>&1 &
    disown
    sleep 1
    echo ">>> 引擎已就绪 (${ENGINE_BIN})。"
else
    echo ">>> [2/3] 引擎已在运行 (127.0.0.1:50051)。"
fi

# 3. 确保网关在运行（8000 端口）。正式名 webtouch-gateway，uws-gateway 为兼容名。
if ! lsof -nP -iTCP:8000 -sTCP:LISTEN >/dev/null 2>&1; then
    echo ">>> [3/3] 正在启动 webtouch-gateway (HTTP 127.0.0.1:8000)..."
    if [ ! -e ./bin/webtouch-gateway ] && [ ! -e ./bin/uws-gateway ] && [ ! -e ./webtouch-gateway ] && [ ! -e ./uws-gateway ]; then
        echo ">>> 正在编译 ./bin/webtouch-gateway..."
        mkdir -p bin
        go build -o ./bin/webtouch-gateway ./cmd/uws-gateway
        ln -sfn webtouch-gateway ./bin/uws-gateway
    fi
    if [ -e ./bin/webtouch-gateway ]; then
        GATEWAY_BIN=./bin/webtouch-gateway
    elif [ -e ./bin/uws-gateway ]; then
        GATEWAY_BIN=./bin/uws-gateway
    elif [ -e ./webtouch-gateway ]; then
        GATEWAY_BIN=./webtouch-gateway
    else
        GATEWAY_BIN=./uws-gateway
    fi
    nohup "${GATEWAY_BIN}" >/dev/null 2>&1 &
    disown
    sleep 1
    echo ">>> 网关已就绪 (${GATEWAY_BIN})。"
else
    echo ">>> [3/3] 网关已在运行 (127.0.0.1:8000)。"
fi

echo "=================================================="
echo "    ✅ WebTouch 浏览器执行服务已在线！"
echo "    - Agent Hub 模型 API: http://127.0.0.1:18080/v1"
echo "    - WebTouch 健康检查: http://127.0.0.1:8000/v1/health"
if [ -e ./webtouch ]; then
    CLI_BIN=./webtouch
elif [ -e ./uws ]; then
    CLI_BIN=./uws
else
    CLI_BIN=webtouch
fi
echo "    - SDK 环境变量: eval \$(${CLI_BIN} bus env)"
echo "    - 对外 CLI: ${CLI_BIN} session （uws 为弃用别名）"
echo "=================================================="
