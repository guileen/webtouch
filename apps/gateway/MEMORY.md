# apps/gateway 经验（M1，2026-08-26）

> 分层边界（2026-09-02）：本包属应用层（待外移），非 WebTouch 核心；核心禁止反向依赖。
> 见 `docs/README.md`（01-Bus 领域）。

- **HasField 语义**：proto optional bool（`chat_stream_done`）在 Go 里用
  `resp.ChatStreamDone != nil` 判断「SSE 通道是否已接入」（Python `HasField`），
  不能用 `GetChatStreamDone()` 的布尔值区分「未接入」与「已接入但未结束」。
- **target_destroyed 识别**：引擎在 error_reason/错误消息里带 `target_destroyed`
  字符串；网关 ChatError.Reason 已带语义前缀，`strings.Contains(reason, ...)`
  即可命中；恢复 = 关会话 → 重建 Tab → 整轮重试一次。
- **多轮基线**：第二轮聊天先读页面最后回答作基线，新回答必须「不同于基线」；
  测试 fake engine 需要每次 extract 返回不同文本（`incrementAnswers`），否则
  永远等到超时（Python _RecordingEngine 同款）。
- **bufconn 契约层测试**：`grpc.NewClient("passthrough:///bufnet", ...)`
  + `grpc.WithContextDialer` 连 bufconn，无需起真实引擎。
- **Tauri externalBin 命名**：macOS/Linux 要求 `binaries/<name>-<target-triple>`
  （如 `uws-engine-x86_64-apple-darwin`），否则 `tauri build`/cargo 报
  "resource path ... doesn't exist"；`prepare-binaries.sh` 用 `rustc -vV` 取 triple。
- **图标**：`iconutil` 对 Go `image/png` 生成的 iconset 偶发 "Invalid Iconset"，
  macOS 用 `sips -s format icns icon.png --out icon.icns` 直转稳定。
- **沙箱构建**：`go run`/`go build` 需要 `GOCACHE=/tmp/uws-go-cache`；cargo 需要
  沙箱外（~/.cargo 缓存写入）。
- **调试 Chrome 自动拉起（2026-08-26）**：`EnsureChrome`（chrome.go）在
  `--attach-cdp` 端点不可达时自动拉起持久调试 Chrome（profile 默认
  `~/.uws-chrome-debug`，`UWS_CHROME_BIN`/`UWS_CHROME_PROFILE` 可覆盖），
  探测 `/json/version` 就绪后复用；`cmd/uws-gateway` 启动时调用（20s 预算，
  失败 fatal）。判定「页面未打开」要区分三态：Tab 关闭=`target_destroyed`、
  会话未初始化=`FAILED_PRECONDITION`、标签页在但无目标按钮=`模型选择器按钮未找到: <nil>`
  （err=nil 仅 Found=false，见 gemini_model.go openGeminiPicker 5s 有界等待）。
- **桌面附着身份边界（2026-09-23）**：`--attach-cdp` 接入用户已运行的 Chrome 时，不能注入 ProviderProfile 的固定 UA/UA-CH/硬件/屏幕覆盖；它可能与实际 Chrome 版本冲突并增加网站风控信号。仅 Gateway 自管的隔离 Chrome 使用 ProviderProfile。登录探针失败只保存本地错误类别，不落盘原始页面文本/Target 详情。
