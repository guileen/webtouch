# 5 分钟快速上手 WebTouch

WebTouch 的对外入口是命令 `webtouch`。`uws` 是同一二进制的弃用别名。本机 session 在 `~/.uws/`（本轮不改目录）。日常操作不要从 gRPC 或 `:8000` 开始。

更全的参数见 [`cli-and-config.md`](cli-and-config.md)。session 字段见 [`cli-session-contract-2026-10-31.md`](cli-session-contract-2026-10-31.md)。

## 1. 构建

```bash
git clone https://github.com/guileen/webtouch.git
cd webtouch
make build
./webtouch version
```

`make build` 产出 `bin/webtouch`、`bin/webtouch-engine`、`bin/webtouch-gateway`、`bin/webtouch-lease`，并留下 `uws` / `uws-engine` / `uws-gateway` / `uws-lease` 软链。源码目录仍是 `cmd/uws`、`cmd/uws-engine` 等。Go module 路径仍是 `uws`。

## 2. 引擎

```bash
./webtouch daemon start
./webtouch daemon status
```

引擎默认 gRPC `127.0.0.1:50051`。状态文件在 `~/.uws/`。地址可用 `WEBTOUCH_ENGINE_ADDR` 或 `UWS_ENGINE_ADDR` 覆盖。

## 3. 对外 session

```bash
./webtouch session create --channel gemini-web
./webtouch session use --session wts_...
./webtouch session close --session wts_...
```

成功与失败都写 stdout JSON（`webtouch.cli.session.v1`）。Agent Hub 应本机 exec 这条 CLI。`:8000 /internal/provider/*` 是迁移期内部入口，不是对外示例。

## 4. 一句目标或一条 DSL

```bash
./webtouch "打开示例站并读出标题"
./webtouch run --plan "nav:https://example.com/"
./webtouch help
```

`webtouch help` 是给 Agent 的自查手册。以 `uws` 启动时，同样的程序会在 stderr 提示改用 `webtouch`。

## 5. 内部 gRPC

引擎线协议在 [`proto/uws_service.proto`](../proto/uws_service.proto)，服务名仍是 `uws.v1.UwsEngine`。这是内部 IPC，不是安装后的使用方式。
