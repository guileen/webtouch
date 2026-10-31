# CLI 对外契约与本机 session 生命周期

> 状态：`ACTIVE`（2026-10-31）
> 本文是 WebTouch **对外能力面**的事实源：对外只走 CLI；session 生命周期由本机 lease 持有。
> 跨项目职责仍以 [Agent Hub 边界](2026-10-03-agenthub-model-gateway-webtouch-boundary.md) 为准；浏览器执行细节仍以 [网关契约](gateway-contract-2026-08-26.md) 为准。二者若与本文的「谁是对外入口」冲突，以本文为准。

## 1. 定调

1. **对外契约 = CLI**。命令主名是 `webtouch`。`uws` 是同一二进制的弃用别名：以 `uws` 启动时向 stderr 打一行提示，stdout JSON 不变。调用方是本机子进程：Agent Hub、脚本、IDE Agent。成功与失败都写 stdout JSON，退出码 `0` / `1`。
2. **CLI 无状态，session 有状态**。短生命周期的 CLI 只携带 `session_id`。真实状态在本机 lease 文件（默认 `~/.uws/cli-sessions.json`，`UWS_CLI_SESSION_FILE` 可覆盖），不在调用方进程里。
3. **Agent Hub 仍是对外模型网关**。Hub → WebTouch 的目标形态是本机 `exec`（与已落地的 `uws vault get` 相同，二进制由 `AHUB_WEBTOUCH_BIN` 指定）。Hub 不把 `:8000` 当对外契约。
4. **内部 IPC 保留**。`uws-engine` gRPC（默认 `127.0.0.1:50051`）与 Chrome CDP 仍是执行器官。它们不是对外 API。
5. **HTTP `:8000` `/internal/provider/*` 退役**。迁移期默认仍响应，但必须带废弃头；`UWS_PROVIDER_HTTP=410` 时直接 `410`，且不得触达引擎。见 §6。

`session create` **不**为了分配句柄而弹出窗口（Focus Invariant）。它只分配句柄、绑定已有账号/Profile，并记录附着端点。物理 Chrome / engine 仍由既有 `daemon` 按需拉起；本命令不新建指纹、不启动第二份浏览器身份。

## 2. CLI schema 原则

| 命令 | 作用 |
| --- | --- |
| `webtouch session create [--channel --account --profile --site-conversation --attach --engine-addr --id]` | 分配 `session_id`，写入 lease |
| `webtouch session use --session <id>` | 复用同一 lease（空闲则唤醒） |
| `webtouch session show --session <id>` | 读当前句柄，不改变状态 |
| `webtouch session list [--all]` | 列未关闭 lease；`--all` 含已关闭 |
| `webtouch session close --session <id>` | 释放租约（幂等） |

- 句柄来源：`--session`，否则环境变量 `WEBTOUCH_SESSION`，再否则 `UWS_SESSION`。`create` 忽略环境变量，总是新建。
- 省略 `--id` 时分配 `wts_` + 16 位十六进制。禁止占用网关隐式槽 `default`。
- **stdout 默认是 JSON**，契约名 `webtouch.cli.session.v1`。`--text` 只给人类一行摘要。
- 凭据、`@vault:` 引用、私钥材料不得出现在 argv、lease 文件或错误 JSON 里。明文仍只允许走既有 vault 管道。
- 单条成功体至少包含：`ok`、`contract`、`session_id`、`state`、`channel`、`site_conversation_id`（没有则为 JSON `null`）、`runtime`、`lease_epoch`、`hub`。
- 失败体：`{"ok":false,"contract":"webtouch.cli.session.v1","error":{"code":"...","message":"..."}}`。`message` 不回显被拒绝的凭据原文。

`hub` 对象只有两个字段，供 Hub 记账：

```json
{"channel":"gemini-web","site_conversation_id":null}
```

## 3. 状态机

```
create
  │
  ▼
active ──use/touch──► active
  │                      ▲
  │ 空闲 ≥ IdleTTL       │ use
  ▼                      │
idle  （runtime=sleeping，可恢复）
  │
  ├─ use ────────────────► active（lease_epoch +1）
  ├─ 显式 close ─────────► closed（runtime=released，幂等）
  └─ 超过 LeaseTTL ──────► closed
```

| 策略 | 默认 | 含义 |
| --- | --- | --- |
| IdleTTL | 10 分钟 | 无活动：`active → idle`，`runtime=sleeping`。引擎可以休眠，lease 仍可 `use` 恢复。不删 Profile、不换指纹。 |
| LeaseTTL | 24 小时（滑动，对齐引擎 `sessions.json` 的 24h） | 自上次活动起超过该时限：`closed`，`runtime=released`。 |
| 崩溃回收 | `owner_pid != 0` 且进程已死 | 下次读/写把 lease 收成 `idle` 并清掉 owner，允许后续 `use` 接管。`owner_pid == 0` 表示权威就是 lease 文件本身，CLI 进程退出不算崩溃。 |
| close | 显式 | 立即 `closed`。再次 close 仍成功。未知 id → `session_not_found`。 |

`runtime` 三值：`ready`（可附着）、`sleeping`（空闲休眠，状态还在）、`released`（租约已释放）。本切片的 `sleeping` / `released` 是 lease 记录；不在 `create`/`close` 里杀 Chrome 或重启 engine。页签回收仍走网关既有空闲回收，直到 Hub 改为 exec 之后再把关页接到 CLI close。

错误码：`invalid_argument`、`vault_ref_rejected`、`profile_unbound`、`profile_mismatch`、`session_exists`、`session_not_found`、`session_closed`、`lease_busy`。

## 4. 身份红线

- `--account` 必须能在身份注册表里 **Resolve 到已有 Profile**。没绑定 → `profile_unbound`，不得静默新建指纹。
- `--profile` 必须是已存在的档案。与该账号已绑定档案不一致 → `profile_mismatch`。
- 同一账号已有未关闭 lease 时，新 lease 必须复用同一 `profile_id`。解析结果若指向另一份档案，拒绝创建。
- 不传账号也不传档案：允许，身份由调用方稍后附着的 Chrome 决定（`attach-cdp`）。这不是新指纹。
- lease 文件模式 `0600`。只记录句柄、通道、账号引用、Profile id、站点对话 id、附着端点。不含 Cookie、密码、API Key。

## 5. 与 Hub 的字段边界

| 字段 | 谁持有 | Hub 账本 |
| --- | --- | --- |
| `session_id` | WebTouch lease | 只在一次 exec 链的进程内存里带着回传，**不当**站点身份落账 |
| `hub.channel` | 两边用同一个通道名 | 可落账 |
| `hub.site_conversation_id` | WebTouch 从页面学到后写入 lease，经 CLI JSON 交出 | 可落账；没有则为 `null`，禁止猜 |
| `account_ref` / `profile_id` | 只在 WebTouch | **禁止**写入 Hub 对话账本 |

Hub 连续体继续只保存历史、channel 与站点会话引用。WebTouch 持有真实浏览器 session。

非流式 provider 轮次若命中一条已知 CLI lease，迁移期 HTTP 会把响应里的 `site_conversation_id` 写回该 lease。流式回写仍是后续项。

## 6. HTTP 退役（迁移期）

`/internal/provider/models|chat|responses`：

1. 每一跳都带 `Deprecation: true`、`X-WebTouch-Contract: deprecated`、`X-WebTouch-Successor: cli`、`Link: <urn:webtouch:cli:session>; rel="successor-version"`。
2. 环境变量 `UWS_PROVIDER_HTTP` 为 `410` / `retired` / `gone` 时，返回 **410**，错误 `type=provider_http_retired`，正文指向 `webtouch session`。此模式不得调用引擎。
3. 迁移期默认仍执行原内部逻辑，避免现网 Hub 在改为 exec 之前断链。这是兼容窗口，**不是**对外契约。
4. 请求里的 `session_id` 若能在 CLI lease 中找到：先 `use`（关闭的 lease → **409** `session_lease_closed`，不进引擎）。找不到的 id（含隐式 `default`）维持旧网关会话行为。
5. 客户端模型面（`/v1/models`、`/v1/chat*`、`/v1/responses`、`/v1/text`）维持已有 `410 gateway_moved`，归 Agent Hub。

## 7. 剩余迁移（follow-up）

- Agent Hub 把 `/internal/provider/*` 换成 `exec` CLI，并用 stdout 的 `hub` 字段记账。本仓不改 Hub。
- CLI `close` 随后再调用 engine `CloseContext` 关页；当前只释放 lease。
- 流式响应把 `site_conversation_id` 写回 lease。
- 迁移窗口结束后，把 `/internal/provider/*` 的默认行为从「废弃头 + 仍响应」改为 410，并删除 Hub 侧的 HTTP 客户端。

## 8. 名称退役（2026-10-31）

对外命令与文档主名是 `webtouch`。下面这些名字本轮**故意保留**，避免一夜拆断本机状态和内部 import。

| 仍用旧名 | 原因 |
| --- | --- |
| 命令 `uws` | 同一 `bin/webtouch` 的软链。argv0 为 `uws` 时 stderr 提示弃用。 |
| `uws-engine` / `uws-gateway` / `uws-lease` | 正式产物是 `webtouch-engine` / `webtouch-gateway` / `webtouch-lease`，旧名是同目录软链。 |
| Go module `uws` | import 路径 `uws/pkg/...`、`uws/cmd/uws`。改成 `github.com/guileen/webtouch` 会碰到每个 Go 文件、proto 生成与 ldflags，另立迁移，不在本轮做。源码目录 `cmd/uws*` 同样保留。 |
| proto 包 `uws.v1`、文件 `uws_service.proto` | 内部 gRPC 线协议，不是对外命令名。 |
| 环境变量 `UWS_*` | 继续有效。同义覆盖：`WEBTOUCH_ENGINE_BIN`、`WEBTOUCH_ENGINE_ADDR`、`WEBTOUCH_GATEWAY_BIN`、`WEBTOUCH_STATE_DIR`。 |
| `~/.uws/` 与 `~/.uws-chrome-debug` | 见下方迁移计划。launchd label `com.guileen.uws-gateway` 也不改。 |

### `~/.uws` 迁移计划（未执行）

1. 本轮默认读写路径不变：`~/.uws/cli-sessions.json`、`sessions.json`、vault、engine pid、learned。不自动创建 `~/.webtouch`，也不复制。
2. 以后若改默认目录，规则必须是：已有 `~/.uws` 就继续用它；只有 `~/.uws` 与 `~/.webtouch` 都不存在时才新建 `~/.webtouch`。禁止静默复制 vault 或 session。
3. 真正改名只能走尚未实现的显式命令，并在一个发布周期内保留 `~/.uws` 到新目录的只读跳转或符号链接。
4. Chrome profile `~/.uws-chrome-debug` 同样不在本轮改名。改名等于换浏览器身份。
