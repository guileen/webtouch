# WorkBuddy CDP 网络观察 hook

本目录提供两种互补的研究 hook：CDP observer 观察可附着浏览器的 Network 事件；Node preload hook 在 WorkBuddy CLI 的 JS release bundle 运行前，对 WebSearch/WebFetch API 调用加脱敏日志。两者都不触发浏览、领取、搜索或登录动作。

默认只观察 WorkBuddy 域名页面及请求，运行时长为 5 分钟：

```bash
WORKBUDDY_CDP_ENDPOINT=http://127.0.0.1:9222 node scripts/research/workbuddy-cdp-observer/observe.mjs
```

如果 WorkBuddy 的浏览器使用其它本机 CDP 端口，可改 `WORKBUDDY_CDP_ENDPOINT`；其它服务域名可用逗号分隔的 `WORKBUDDY_HOST_SUFFIXES` 指定；`WORKBUDDY_OBSERVER_MS` 可调整观察时长。

输出仅含脱敏后的域名、路径、方法、资源类型、请求头名称、Authorization 是否使用及其 scheme、Cookie 头是否存在、HTTP 状态和耗时。查询串与片段会被移除，路径中的数字、UUID 和长不透明段会归一化；请求/响应正文、Cookie 值、Authorization 值、请求 ID、页面标题和页面文本均不会输出。日志默认只写 stdout，使用前不要把终端输出公开分享。

限制：只有浏览器通过本机 CDP 暴露时才能观察；WorkBuddy 若使用内嵌不可调试 WebView 或独立远端浏览器，本探针不会看到其流量。它是本地研究 hook，不是 WebTouch 生产 API。

## WorkBuddy CLI JS hook

WorkBuddy 的 CLI release bundle 中，`WebSearchTool` 和 `WebFetchTool` 使用 Node 全局 `fetch` 调用服务端 API。`workbuddy-fetch-hook.cjs` 只在 argv 指向 WorkBuddy CLI bundle 时包装全局 `fetch`，并只记录这两个 endpoint 的 host、脱敏 path、请求头名称、认证 scheme、body 字段名、HTTP 状态和耗时；不记录 query、prompt、URL 值、token、Cookie 或响应正文。应用包本身保持原样，避免改写签名资源。

```bash
scripts/research/workbuddy-cdp-observer/launch-workbuddy-with-hook.sh
```

这会用 hook 环境启动 WorkBuddy。客户端会过滤任务进程的 `NODE_OPTIONS`，因此仅看到主进程启动并不能证明搜索 hook 已加载。实际入口还包括 `cli/bin/codebuddy`。研究 hook 在 daemon 中只为这类 CLI 子进程补上 preload，在 CLI 中包装 `fetch`；其它进程不安装。已运行的 CLI 或 daemon 可通过临时、仅 loopback 的 Node Inspector 加载同一 hook：先核对目标 PID 与实际 JS argv，只接受本应用 CLI/daemon，加载后立即关闭 Inspector，不重启任务。

加载记录和请求元数据同时写 stderr（`[WB_OBS]`）及本机临时目录 `/private/tmp/workbuddy-observer/metadata.jsonl`（目录 0700、文件 0600）。`observer_ready` 表示 hook 已加载；只有 `tool_request` 才表示观察到真实工具请求。进程结束即卸载，运行中可恢复原始 fetch/spawn；日志无凭据值、正文或 URL 参数。

这个 JS hook 覆盖 WorkBuddy 服务端 `WebSearch`/`WebFetch` 工具请求；浏览器自动化 Chromium 的页面网络仍需 CDP hook，因为 `agent-browser` 的实际执行器是独立原生二进制。
