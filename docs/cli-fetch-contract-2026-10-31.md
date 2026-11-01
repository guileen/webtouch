# CLI 匿名读取契约

> 状态：`ACTIVE`

`webtouch fetch --url <http(s) URL> [--format markdown] [--timeout 毫秒]` 通过 Go HTTP 客户端一次性 GET 公共页面；默认 markdown，超时 25000 毫秒（1–120000）。只接收 HTML 或纯文本，最多读取 4 MiB，跟随最多 10 次 HTTP(S) 重定向；非 2xx 响应报错。HTML 使用现有 Go HTML 解析器提取正文、标题和链接，不执行 JavaScript，也不保证能读取动态页面、登录页或挑战页。

无 cookie jar、session、账号、vault、Chrome、环境代理或外部程序；URL 和重定向禁止 userinfo。仅支持 markdown；`screenshot` 和 `json` 返回 `unsupported_format`。不接受额外参数。

stdout 始终为单个 JSON 对象，契约 `webtouch.cli.fetch.v1`。成功：`ok: true`、`contract`、`backend: http`、`format: markdown`、`content`（字符串）、`status`、`url`（最终 URL）。失败：`ok: false`、`contract`、`backend: http`、`error: {code, message}`，退出码 1；成功退出 0。错误码：`invalid_argument`、`unsupported_format`、`fetch_failed`、`http_status`、`unsupported_content_type`、`output_too_large`、`invalid_html`。错误不回显 URL 或被拒参数。
