# CLI 匿名读取契约（ACTIVE · 2026-10-31）

`webtouch fetch --url <http(s) URL> [--format markdown|json|screenshot] [--timeout 毫秒] [--output 路径]` 是一次性匿名读取，固定调用 `moli fetch`，不使用 engine。默认 format 为 markdown、超时 25000 毫秒；超时必须为 1–120000。截图必须指定 `--output`，其他格式禁止指定。URL 不允许 userinfo 和 `@vault:`；不接受额外参数。

仅允许匿名临时状态：不接 session、账号、profile、cookie、vault，不导入或同步 Chrome 状态；不提供任意参数透传、eval、serve 或 CDP。可执行文件优先取 `WEBTOUCH_MOLI_BIN`，其次 `MOLI_BIN`，否则从 PATH 查找 `moli`；不自动安装。固定参数为 `fetch --dump <format> --timeout <毫秒> [--layout] -- <URL>`，截图加 `--layout`。后端必须支持这些参数。

成功和失败均在 stdout 输出单个 JSON 对象，契约 `webtouch.cli.fetch.v1`，含 `ok`、`contract`、`backend: moli`。成功时含 `format`；markdown 的 `content` 是字符串，json 的 `content` 是解析后的 JSON 值，截图的 `artifact` 是调用者给出的路径，文件内容必须有 PNG 签名。结果不声称 HTTP 状态、最终 URL 或页面真实性：后端成功不保证页面不是登录页、挑战页或空壳。截图成功后才写文件，可能覆盖指定文件。

失败返回非零退出码和 `error: {code,message}`，不回显后端 stderr、请求 URL 或被拒绝的参数：`invalid_argument`（参数错误）、`backend_missing`（可执行文件不可用）、`backend_timeout`、`backend_failed`（启动或非零退出）、`output_too_large`（后端输出超过 16 MiB）、`invalid_backend_output`（空结果、无效 JSON 或 PNG）、`output_write_failed`。失败退出码统一为 1；帮助写 stdout 文本并退出 0。后端 stderr 丢弃。外层进程执行超时与后端超时相同，取消时终止子进程；不提供服务进程。该 MVP 未验证后端版本、HTTP 状态或挑战识别。
