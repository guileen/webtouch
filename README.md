# WebTouch

给本机 Agent 用的浏览器执行与站点身份底座。它接管已安装的 Chrome，保管网站账号、Vault 与 Profile/指纹绑定，并用 CLI `webtouch` 把会话交给调用方。

调用方是 Agent Hub、脚本和 IDE Agent：本机拉起子进程，不把 WebTouch 当成公网 HTTP API。

## 它解决什么问题

已登录的网页模型和站点操作需要一份稳定的浏览器身份。会话、Cookie 和风控边界如果放进调用方进程，或者暴露成常驻 HTTP，身份和凭据就不再由执行层独占。

WebTouch 把这件事留在本机：

- 对外只走 CLI。命令主名是 `webtouch`。
- session 生命周期在本机 lease。CLI 只携带 `session_id`。
- 同一账号复用已经绑定的 Profile，不在创建会话时新建指纹。
- 客户端模型 API 与 CLI/Web 连续体归 Agent Hub。边界见 [Agent Hub 与 WebTouch](docs/2026-10-03-agenthub-model-gateway-webtouch-boundary.md)。

`uws` 是同一二进制的弃用别名。以 `uws` 启动时，stderr 会提示改用 `webtouch`，stdout 契约不变。

## 快速开始

```bash
git clone https://github.com/guileen/webtouch.git
cd webtouch
make build
./webtouch version

./webtouch session create --channel gemini-web
./webtouch session use --session wts_...
./webtouch session show --session wts_...
./webtouch session close --session wts_...
```

成功和失败都写 stdout JSON，契约名 `webtouch.cli.session.v1`。状态文件仍在 `~/.uws/cli-sessions.json`。字段与状态机见 [CLI session 契约](docs/cli-session-contract-2026-10-31.md)。

匿名公开页面可用 `./webtouch fetch --url https://example.com`；用法与依赖见 [fetch 契约](docs/cli-fetch-contract-2026-10-31.md)。

引擎按需由 CLI 拉起：

```bash
./webtouch daemon start
./webtouch daemon status
```

更多子命令、配置和 DSL 见 [CLI 与配置](docs/cli-and-config.md) 与 [五分钟上手](docs/quickstart.md)。`webtouch help` 是给 Agent 的自查手册。

`:8000` 的 `/internal/provider/*` 只在迁移期保留，响应带废弃头。它不是对外入口。

## 构建与安装

需要 Go，以及系统里已安装的 Chrome。Python 只在跑协调器测试或旧协调器命令时需要。

```bash
make build     # bin/webtouch、bin/webtouch-engine、bin/webtouch-gateway、bin/webtouch-lease
./webtouch version
```

`uws`、`uws-engine`、`uws-gateway`、`uws-lease` 是指向上述产物的软链。源码目录仍是 `cmd/uws`、`cmd/uws-engine`、`cmd/uws-gateway`、`cmd/uws-lease`。

```bash
make install   # build + WASM 探针 + 链到 /usr/local/bin
```

`make install` 依赖本机 Emscripten（`emcc`）来编译探针。只需要 CLI 与引擎时，`make build` 就够。

版本以仓库根的 `VERSION` 为准，当前是 v1.5.5。构建时把版本和 git sha 注入二进制。

## 架构

仓库里没有单独的架构 SVG。在线路径是：

```text
Agent / 脚本
    │  本机 exec webtouch
    ▼
CLI session lease（~/.uws/cli-sessions.json）
    │
    ├── webtouch-engine     内部 gRPC，驱动 Chrome CDP
    └── webtouch-gateway    迁移期内部 HTTP，不是对外契约
```

| 文档 | 读它的原因 |
| --- | --- |
| [系统地图](docs/system-map-2026-09-23.md) | 目录、调用链和边界 |
| [核心设计](WebTouchKeyDesign.md) | 产品边界与不变量 |
| [活规范地图](docs/README.md) | 五大领域各自的现行契约 |
| [算法内核](spec/algorithm-kernel.md) | 现役执行与验收口径 |
| [v2 架构 RFC](docs/webtouch-v2-architecture-rfc.md) | 目标态；未落地前不以它替换现役内核 |

`pipeline/`、`learning/`、`benchmarks/`、`archive/`、`docs/evidence/` 不在本仓库树内。

## 状态

| 项 | 现在 |
| --- | --- |
| 对外入口 | CLI `webtouch` |
| 弃用别名 | `uws`（同一二进制） |
| 本机状态目录 | `~/.uws`；Chrome profile 仍是 `~/.uws-chrome-debug` |
| 许可 | 专有软件。本仓库不授予开源许可，也没有 OSI 许可证 |
| QA Studio | `HOLD`。说明见 [QA Studio](docs/qa-studio/README.md) |

## 文档

- [CLI session 契约](docs/cli-session-contract-2026-10-31.md)
- [CLI 与配置](docs/cli-and-config.md)
- [五分钟上手](docs/quickstart.md)
- [内部 gRPC 参考](docs/api-reference.md)（引擎 IPC，不是对外 API）
- [贡献指南](docs/contributing.md)

## 参与贡献

环境、测试和目录边界见 [贡献指南](docs/contributing.md)。改行为前先改对应活规范，地图在 [docs/README.md](docs/README.md)。

## 许可

WebTouch 是专有软件。本仓库当前不授予开源许可，根目录也没有 `LICENSE`。第三方依赖继续遵守各自的许可证。
