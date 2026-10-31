# uws-gateway 网关契约（M0，2026-08-26）

> **历史背景（2026-09-02）**：按 WebTouch 分层定稿（`docs/evidence/archive/web-touch-architecture-2026-09-02.md`
> §2.2/§5-R1），统一 AI 网关属**应用层**，当前混在核心仓（`apps/gateway`、`cmd/uws-gateway`、
> `coordinator/app/api_server.py`、`coordinator/app/providers/`）；Python 服务已归档，OpenAI 兼容门面现由 Agent Hub 提供。

> **职责迁移（2026-10-03 用户定案）**：Agent Hub 是面向客户端的统一模型 API 与模型目录所有者。WebTouch 仅运行浏览器 provider 执行服务，并继续管理网站、账号、Vault、账号到 Profile/指纹绑定、站点会话和风控状态。WebTouch 的 `/v1/models`、`/v1/text`、`/v1/chat*`、`/v1/responses` 客户端模型门面已退役；调用返回 `410 gateway_moved` 并指向 Agent Hub。Hub 只通过 `/internal/provider/*` 调用 WebTouch。本文后续关于 OpenAI 请求/响应形状、模型目录、流式和错误语义，约束 Hub 到 WebTouch 的内部执行契约，不构成 WebTouch 客户端模型 API。唯一跨项目边界见 [`Agent Hub 模型网关与 WebTouch 浏览器边界`](../../agent-hub/docs/2026-10-03-agenthub-model-gateway-webtouch-boundary.md)。
>
> 浏览器 provider 执行契约：Agent Hub 负责客户端协议与 provider 路由；WebTouch 将 Hub 指令转换为网页动作并返回结果。WebTouch 不提供通用模型 API。
> 状态：浏览器执行与站点管理仍由 `cmd/uws-gateway` + `apps/gateway` 提供；客户端模型 API 由 Agent Hub 提供。

## 1. 定位

WebTouch 浏览器 provider 执行服务仅承接 Hub 已选定的网页模型通道：

- `uws_web`（DeepSeek / Gemini 网页接管，经 uws-engine gRPC + CDP）；
- 官方 API provider 与跨供应商模型路由由 Agent Hub 管理，不在 WebTouch 执行服务内配置。

## 2. 进程与端口

- `uws-gateway` 默认监听 `127.0.0.1:8000`（与现有 api_server 对齐）；
- web 通道依赖 `uws-engine` gRPC `127.0.0.1:50051`；
- 配置：`channels.json`（v1 用 JSON，零第三方依赖；YAML 后置）。

## 3. 端点

| 方法 / 路径 | 说明 |
|---|---|
| `GET /internal/provider/models` | Agent Hub 查询网页模型与能力列表 |
| `POST /internal/provider/chat` | Agent Hub 发起网页模型轮次（内部沿用 Chat Completions 请求/响应形状） |
| `POST /internal/provider/responses` | Agent Hub 发起网页模型 Responses 轮次 |
| `POST /v1/sessions` | 新建会话（web 通道 = 新 Tab；body 支持 `channel_id` 或 `model` 解析通道；带 `site_conversation_id` 时直接落到该站点会话页，见 §9） |
| `GET /v1/sessions` | **本网关**会话列表（含通道 / 锁定模型）；站点侧对话见下一行 |
| `GET /v1/sessions/{id}/conversations` | **站点**侧栏对话列表（`id` ＋ 标题；只读，见 §9「站点会话身份」） |
| `DELETE /v1/sessions/{id}` | 关闭会话（**关前先删该会话，再关 Tab**；`?purge=0` 显式只关页，见 §9 / §14） |
| `GET` · `POST` `/v1/purge` | 待删除列表：列出 / 删除一条（见 §14） |
| `POST /v1/purge/drain` · `DELETE` · `POST` `/v1/purge/{cid}` | 立即清账一轮 / 人工销账 / 人工重试（见 §14） |
| `GET /v1/auth/status` | 各站点登录状态（桌面提醒用） |
| `POST /v1/auth/reset` | 用户完成人工登录/挑战处理后清除指定网页通道的本地锁定；不访问网页 |
| `GET /v1/diag/model-menu` | 模型菜单漂移自检（`?channel=&session=&deep=`；只读，见 §6「模型菜单漂移自检」） |
| `GET /v1/health` | 健康检查 |

## 4. 请求子集与动作协议（OpenAI 兼容 + Markdown-First 隐蔽动作）

标准字段：

- `model`（必填）：全局模型 id 或别名；
- `messages: [{role, content, name, tool_call_id}]`；
- `tools: [{type: "function", function: {name, description, parameters}}]`（Codex / Agent 动作声明）；
- `instructions`: 针对 Responses API 或会话级指令；
- `stream: bool`；
- `temperature` / `max_tokens`：尽力映射；web 通道不支持时忽略并在日志记录。

隐蔽 Markdown 动作执行规范（Stealth Action Polyfill）：
- **防风控去机械化**：网关底层将 tools 编译为自然程序员「本地终端协同排查」指令，规避 "tool"/"function" 等高危词；
- **下行指令提取**：模型在 Markdown 中以 ````run:<action>```` 给出命令，网关正则提纯为 OpenAI `tool_calls` 或 `function_call`，正文保留思考过程；
- **上行结果回显**：Agent 返回的工具结果自动转义为 `> 运行输出 (<action>):\n```\n...\n```` 贴回网页，维持自然对话并确保 100% 命中服务端 KVCache。
- **文本形态兜底结构化（ACTIVE，2026-10-03 派单 d8d0b0ac3）**：模型常不照约定，把调用写成 JSON 或站点原生 DSML；网关此前只认 `run:` 块 ⇒ 这些会漏成纯 content（agent-hub 导航步实测命中率 **1/10**，且唯一「命中」的还被塞成自造键 `{"input":…}`）。现由 `ParseToolCallsFromText`（`apps/gateway/tool_text.go`）在隐蔽块未命中后再解析两类形态并摘除原片段：① **JSON**——裸 schema 键对象、`{"name","arguments"}` 对象/数组、`{"tool_calls":[…]}`，容忍模型回显的 `tools=[…] tool_choice=…` 头；② **DSML/XML**——`<｜tool_calls｜><｜invoke name=…><｜parameter name=…>`（容忍 ｜/| 竖线变体与截断未闭合）。另：隐蔽块里被写成 `k=v` 自由文本的参数按声明 schema 重整（不再退化成 `{"input":…}`）。**只在请求声明了 tools、且能绑定到声明工具**（`parameters.properties` 键名校验；命名形态只认 `arguments`、不认 `parameters`——后者是工具定义回显）时生效，避免误判普通正文。真机 A/B（同一导航提示词连打 10 次，`deepseek-chat`）：schema 合法 `tool_calls` **1/10 → 7/10**（7 次全含 `intent`/`anchors`/`reason` 且 `intent` 合法）；余 3 次是模型只回显 `tools=…` 头或裸工具名、**无参数可结构化**。单测 `apps/gateway/tool_text_test.go`。

扩展字段（web 通道专用，body 内或 `x-uws-*` 头）：

- `thinking: "on" | "off"`；
- `search: "on" | "off"`；
- `new_session: bool` / `session_id: string`。

网页文本输入遵循 [`../spec/security-dx.md`](../spec/security-dx.md) §2.2 的 256 KiB UTF-8 边界，可承载多轮历史；不得为适配旧 4096 字节限制而静默截断或拆成多次模型提问。

强制网页通道通过内部请求体的 `channel_id` 指定；不再提供 `/v1/{channel_id}/...` 客户端模型路由。

## 5. 响应与错误

- Chat Completions 非流式：OpenAI `chat.completion`（choices / message / tool_calls / finish_reason）；
- Chat Completions 流式：SSE `chat.completion.chunk` + `data: [DONE]`；
- Responses API 非流式：`response` 对象（`status: completed`，`output: [message, function_call]`）；
- Responses API 流式：`response.created`、`response.output_text.delta`、`response.function_call_arguments.delta`、`response.completed`；
- 错误：OpenAI 风格 `error` 对象（`type` / `message` / `param` / `code`）。

### /v1/chat 失败分类（机器可判；ACTIVE，2026-10-03 用户定案「错误码要能分辨、内部至少重试一次」）

- **动机**：此前 HTTP 层恒为 502、`error.code` 恒为 `null`，调用方只能读文案/`semantic`/日志里的 `dur`，
  分不出「重试就好」与「绑定已死必须清」。本条款把它变成契约。
- **机器键＝`error.code`**（闭合枚举）；`error.semantic` 保留站点/步骤级细节（如 `chatgpt_send`）。
  另附 `error.retryable`（bool：该失败类是否已纳入网关的自动整轮重放）与
  `error.action`（`retry|reset_session|relogin|abort`，调用方该做什么）。
- **分类是唯一事实源**：HTTP 状态、错误体、SSE `error` 帧、整轮重试判据共读一份 `classifyChatError`
  （`apps/gateway/handler_write.go`）；同源由 `TestChatErrorStatusMatchesWriteChatError` 守护。
  分类走 `errors.As` 穿透 `fmt.Errorf("%w；…")` 包装（如模型切换错误里拼的菜单自检现场）——
  包装不得把「页签已毁」这类可重试失败降级成 `internal_error`/500。

| code | HTTP | retryable | action | 触发 / 调用方该做什么 |
| --- | --- | --- | --- | --- |
| `session_binding_dead` | 409 | 是 | `reset_session` | 引擎找不到会话原 Target（页签被关/Chrome 重启/空闲回收）；网关已自动清死绑定并重放一次，仍失败则 `DELETE /v1/sessions/<id>` 后再发 |
| `page_not_ready` | 503 | 是 | `retry` | 标签页被销毁 / 菜单未渲染 / 登录探测未就绪（提交前）；`Retry-After: 2`，可原样重发 |
| `engine_unavailable` | 503 | 是 | `retry` | **网关→引擎这一段不可用**（gRPC `code=14 UNVAILABLE`；含 `InitializeContext` 建会话与动作阶段），请求已到网关、是上游接入面不可用；`Retry-After: 2` |
| `upstream_timeout` | 504 | 否 | `abort` | 本轮预算内未完成；不能证明未发送（T-26），默认不重放，须先核对原页面，或显式 `UWS_CHAT_RETRY_ON_TIMEOUT=1` |
| `submission_outcome_unknown` | 502 | 否 | `abort` | 提交结果未知（T-26 硬边界）；须先核对原页面，禁止自动重发 |
| `provider_error` | 502 | 否 | `abort` | 站点/引擎业务失败（空回答、provider 5xx…）；细节看 `error.semantic` |
| `login_required` / `human_verification_required` | 401 | 否 | `relogin` | 游客态/风控挑战，需用户在该 Profile 手动登录 |
| `authentication_check_failed` | 503 | 否 | `relogin` | 登录态未知（保护账号，拒绝提交） |
| `invalid_request` / `unknown_model` / `not_implemented` | 400/404/501 | 否 | `abort` | 请求/路由错误 |

- **整轮重试**：默认 `UWS_CHAT_RETRY_ATTEMPTS=1`（内部至少重放一次，共 2 次尝试），只覆盖**提交前**可安全重放的类
  （`page_not_ready`、`session_binding_dead`、`engine_unavailable`）；重试前先 `CloseContext` 清死绑定再重建页签。
  「提交结果未知」与「本轮超时」默认不重放（T-26 不变量）。
- **代码落点**：`apps/gateway/handler_write.go`（分类 + 写出）、`apps/gateway/handler_chat.go`（SSE `error` 帧）、
  `apps/gateway/preflight.go`（重试判据）、`apps/gateway/chat_retry.go`（重试重建 + 清死绑定）。

### 页内登录探针返回契约

登录探针必须返回 JavaScript 对象 `{status, composer, login, challenge}`，由 CDP `returnByValue` 编码一次并经引擎原样传递；不得在脚本内先 `JSON.stringify`，否则 Go 会收到 JSON 字符串而非对象。结构无效仍归 `unknown` 并阻止发送；修复编码不放宽未登录 / 挑战锁。回归必须执行真实探针脚本再模拟 CDP 编码，不能只让假引擎直接返回理想 JSON。

### 网页动作重试与提交边界（ACTIVE）

- 父请求取消后不再派发动作或轮询。只读 `extract` 可有界退避；写动作仅在引擎明确返回 `physical_action_not_dispatched` 时自动重试，普通超时、ABORTED 或 `physical_event_dispatched_outcome_unknown` 均不能换 intent 再次点击。
- 等待答案只观察，不点击网页重试/重新生成按钮；无新文本不证明模型未收到问题。
- 预算耗尽时，SSE 未 done 或 DOM 尚未稳定的文字只是部分结果，不可作为成功完成；流式也不得补发 `finish_reason=stop`。返回明确等待超时，保留提交结果未知语义，不自动重问。
- 发送开始后，包括答案观察期的 Target 丢失或请求取消，返回 `submission_outcome_unknown`，不自动建新 Tab 重放整轮。发送前 Target 丢失仍可重建一次。非流式与流式必须使用同一规则。
- 本节不承诺撤回已经发送的问题；上层恢复前须核对原页面/回执。模型调用数不能只凭成功回合数推断，隐藏重发不能绕开上层预算。
- 页内直发与 DOM 共用提交边界：仅脚本明确回报 `not_dispatched`（尚未调用 fetch）才可回退 DOM。求值失联、HTTP/网络错误、队列丢失、解析错误、空响应或部分流中断均不回退重发；只有明确完成且非空的流可成功结束。直发消费错误必须传到调用者，取消须释放本请求的队列/reader，不触碰其他会话。

## 6. 模型命名与路由

全局模型表（v1 内置，通道配置可覆盖）：

| 模型 id | 通道 | 能力 |
|---|---|---|
| `deepseek-chat` / `deepseek-reasoner` | `deepseek-web` | 全新统一多模态模型 / R1 深度思考链（`reasoning_content`），双开关均支持 search |
| `gemini-3.5-flash-lite` / `gemini-flash` (3.8 Flash) / `gemini-3.1-pro` / `gemini-veo` (Veo 3.1) / `gemini-imagen` (Imagen 3) | `gemini-web` | thinking / image_gen / video_gen；**搜索默认开、不可关**（故 `capabilities.search=false` 表示「无开关」，不是「无搜索」）；选择器实测 3 档＝3.5 Flash-Lite／3.8 Flash／3.1 Pro，**Extended thinking 是深度思考开关而非模型**（2026-10-02 用户校准），Veo/Imagen 不在此选择器 |
| `jimeng-image` / `jimeng-video` (Seedance 2.0) | `jimeng-web` | image_gen / video_gen |
| `doubao-pro` (1.5 Pro) / `doubao-image` | `doubao-web` | thinking / search / image_gen |
| `qwen-chat` | `qianwen-web` | thinking / search（能力位声明；DOM 语义未接） |
| `chatgpt-latest` (Latest) / `chatgpt-5.6-sol` (GPT-5.6 Sol) / `chatgpt-5.5` (GPT-5.5) | `chatgpt-web` | 仅聊天模式（禁工作模式）；思考/搜索不声称（UI 未见独立开关） |
| （外部通道配置别名） | `openai_http` | 透传 |

解析顺序：精确 `model` id → 通道配置内模型表 → 未收录返回 404 + 可用模型列表提示。

Gemini 模型选择器兼容旧 `data-test-id=bard-mode-menu-button` 与当前 `button[aria-label^="Open mode picker, currently "]`。请求的模型族与可见按钮当前短名精确相同（Flash 不等于 Flash-Lite）时，只读确认即完成配置；否则走菜单真实切换并用同一选择器回读，不能将按钮结构变化误报为账号未登录。

模型菜单的读取有三条硬约束（2026-09-26 真实页 DOM 观测）：

1. 菜单容器属性是 `data-test-id="gem-mode-menu"`（无连字符的 `data-testid` 写法在线上已不存在），选择器须同时容忍两种拼法；**不要用泛化的 `gem-menu[role="menu"]` 兜底**——同站还有别的下拉菜单，泛化匹配会把无关项当模型项点击；
2. 菜单**懒渲染** —— 未打开时 DOM 里只有空的 `gem-popover` 占位，`gem-menu` / `gem-menu-item` 都不存在。任何「不开菜单就直接读 DOM」的判定都会误报「菜单不存在」，必须真实点击打开后再读；
3. 菜单项读取必须**收口到模型菜单容器**：整文档查找会撞上同站其它下拉菜单的 `gem-menu-item`，把无关项当模型项点击（线上模式菜单与其它菜单可能同时可见）。容器存在时**只读容器**——容器存在但还没渲染出可见项时返回空、由调用方继续轮询，绝不回退整文档，否则会把其它菜单项混进来；仅当页面确实找不到容器时才降级整文档（旧页面兼容）。

模型 / 思考开关 / DeepSeek 模式这类**多阶段物理切换语义**走独立的动作预算，不与单次点击共用默认 8 秒（`pkg/browser/action_budget.go`）：一次切换含「开菜单（有界等待 ≤5s）→ 拟人轨迹点击 → 等菜单项（≤5s）→ 拟人轨迹点击 → 800ms 回读」两段有界等待与两次真实鼠标轨迹，失败还会整轮重开一次，默认预算会被这条合法序列自身吃穿；而超时点落在 `disp.mark()` 之后，会被如实归类成不可重放的 `physical_event_dispatched_outcome_unknown`，把「很可能已切换成功」报成「结果未知」（WD-DEBATE-R1 现场第三次失败即此形态）。放宽的只是时长上界：派发前 / 后的中断语义、`indeterminateGrace=15s` 重放门禁与 `canRetryAction` 判据均不变。

### 模型菜单漂移自检（ACTIVE，2026-09-26 用户定案）

站点会改版、档位会变（菜单项增删改名、按钮与容器结构变化），因此「菜单/选择器发生变化」不是一次性修 bug 的对象，而是必须内生的**可重复检查**。自检只读页面、**不发送消息、不切换模型、不改会话状态**；深自检会开合一次菜单（页面自身 `element.click()`，无需物理鼠标）。

| 触发 | 入口 | 说明 |
|---|---|---|
| 失败即查 | `ensureModel` 切换失败时自动执行一次深自检 | 结论拼进 `model_error`，失败现场自带「当时菜单长什么样」 |
| 定时 / 按需 | `GET /v1/diag/model-menu?channel=gemini-web[&session=s-…][&deep=1]` | `deep` 默认 1（真实开菜单采样）；`deep=0` = 纯只读、不点击、可高频轮询 |

漂移判据（对站点 Provider 的 `MenuFamilies` 期望档位关键字做大小写不敏感子串比对，与切换时的 token 匹配同源）：

1. 选择器按钮缺失 ⇒ `picker_missing`（疑似改版）；
2. 深自检开不出菜单 ⇒ `menu_not_opened`（懒渲染已开却仍读不到项）；
3. 期望档位在菜单项里全不命中 ⇒ `family_missing:<F>`；
4. 菜单有项但一个期望档位都不命中 ⇒ `menu_unknown_shape`。菜单里出现期望外的新档位只记 `unexpected_items`，**不判漂移**（增档位不算坏）。

边界：自检**只负责发现漂移，不做自动适配**——自动重写选择器/模板属 recipe 路线（见 `USER_ACTION.md` 的 R4 候选），未排期前不得声称已自适应。v1 仅支持 Gemini 模式菜单（`gem-menu` 懒渲染 + `data-test-id="gem-mode-menu"` 容器），其它站点返回显式 `unsupported`，不得退化成泛化 `[role=menu]` 兜底（见上文三条硬约束）。

### Gemini Veo 3.1 视频生成与多模态生成契约（ACTIVE，2026-10-31 用户定案）

1. **模型与路由**：
   - 模型标识：`gemini-veo`（别名：`veo-3.1`, `veo-3`, `veo`, `veo-2`, `gemini-video`），由 `gemini-web` 通道承载；
   - 物理定位：Veo 3.1 属于提示词驱动的生成式能力，**不在页面下拉菜单物理档位中**；`resolveGeminiModel` 统一返回空，保持当前/默认模型（Flash/Pro），严禁触发模型下拉菜单物理点击导致 `model_switch_failed`。
2. **长耗时预算（Timeout Budget）**：
   - 视频生成耗时显著长于普通文本对话（通常 60s ~ 180s）；
   - 当请求模型标记为 `VideoGen: true`（如 `gemini-veo`）时，网关单轮超时预算由常规的 `chatTimeout`（默认 90s）自动放宽至 `videoTimeout`（默认 300s，可通过环境变量 `UWS_VIDEO_TIMEOUT` 或命令行 `-video-timeout` 覆盖），避免在视频渲染期间遭遇上游 504 `upstream_timeout`。
3. **提示词规范化自适应（Prompt Adaptation）**：
   - 若用户传入的 Prompt 未包含显式视频生成触发词（如 "generate a video", "create a video", "生成视频", "制作视频" 等），网关执行层自动附加结构化提示引导前缀（如 `Generate a high-quality video using Veo: <prompt>`），确保 Gemini Web 准确唤起 Veo 视频生成管道而非输出文本小说。
4. **视频结果提取与多模态交付（Output Extraction & Delivery）**：
   - 页面渲染识别：Gemini 网页端在生成视频完成后，会在模型回答容器中挂载视频组件（`<video src="...">`、`video > source[src]` 或 Google 用户存储媒体链接）；
   - 交付与格式化：
     ① 文本透传：在 `message.content` 中保留模型伴随文本，并以标准 Markdown 附带视频链接与视频标记：`\n\n[Generated Video](<video_url>)`；
     ② 结构化与落盘：支持提取到视频直链后，经本地下载管道落盘至 `~/.uws/artifacts/videos/`，向调用方提供稳定的离线 MP4 产物。

## 7. 通道 schema（channels.json）

```json
{
  "channels": [{
    "id": "deepseek-web",
    "type": "uws_web",
    "site": "deepseek",
    "account_id": "deepseek-primary",
    "engine_addr": "127.0.0.1:50051",
    "models": [
      {"id": "deepseek-fast", "family": "快速模式", "thinking": true, "search": true},
      {"id": "deepseek-expert", "family": "专家模式", "thinking": true, "search": false},
      {"id": "deepseek-vision", "family": "识图模式", "thinking": true, "search": true}
    ]
  }, {
    "id": "deepseek-api",
    "type": "openai_http",
    "base_url": "https://api.deepseek.com",
    "api_key_env": "DEEPSEEK_API_KEY",
    "models": [{"id": "deepseek-chat"}, {"id": "deepseek-reasoner"}]
  }]
}
```

`account_id` 是 WebTouch 身份注册表里的稳定账号标识，不是用户名、密码或 Vault 明文。配置后，网关必须通过 `pkg/fingerprint.Registry.Resolve(account_id)` 取得已绑定的 Profile；没有绑定时 fail-closed，不能静默生成另一份指纹。此项只适用于 WebTouch 自己启动的隔离浏览器；`--attach-cdp` 已由指定 Chrome Profile 决定身份，不能同时声明 `account_id`。省略 `account_id` 保留旧的按站点稳定 Profile 行为。

## 8. 参数映射（web 通道）

| OpenAI 字段 / 扩展 | 映射 |
|---|---|
| `messages` 最后一条 user | 网页输入（上下文靠 session / Tab） |
| `stream` | 引擎 SSE 捕获 / DOM 兜底（复用现有协议） |
| `thinking` | `gemini_thinking_switch` / `deepseek_thinking_switch` |
| `search` | DeepSeek 智能搜索 `deepseek_search_switch`（专家模式无此开关 → 显式报错）。当前引擎未实现 Gemini 搜索语义，故 gemini 型号目录能力位 `capabilities.search=false`、下发该字段不改变页面（2026-09-26 与站点真实开关对齐） |
| `model` | 模式切换（deepseek_mode / gemini_model）；仅新会话可切的站点自动提升 `new_session` |
| `temperature` / `max_tokens` | 不适用，忽略并记录 |
| `close_after` | 一次性会话：本轮回答交付后**立即收口**（先删站点会话再关 Tab；失败轮不收口，见 §9） |

## 9. 会话语义

- 多 Tab = 多 session，引擎 `context_name` 路由（引擎 v1.5.3 已有）；
- 会话锁定通道 + 模型；换模型自动重建 Tab（沿用 2026-08-26 修复语义）；
- `POST /v1/sessions` 带 `model` 时按模型解析通道并在创建后尽力应用模式
  （DeepSeek 三模式 / Gemini 模型切换）；失败不阻塞会话创建（页面已打开），
  下一次 chat 由 `modelChangedRequiresNewSession` / `ensureModel` 兜底兑现；
- `intent_id` 幂等与在途去重由引擎保证（已有）。

### 会话生命周期与收口（ACTIVE，2026-09-30 用户定案）

- **归属**：谁开会话谁负责收口。网关只关**自己创建**的 Tab（引擎 `CloseContext`，幂等）；
  不碰用户自己的窗口 / 标签页，也不删用户真实历史会话（§14 硬边界不变）。
  一 session 一 Tab 本身是必要的隔离手段，**缺陷从来不是"一 Tab 一会话"，而是"没有人负责关"**。
- **寿命三态**（显式优先，兜底在后）：
  1. `ephemeral`（一次性）：请求带 `close_after: true`（或头 `x-uws-close-after: true`）
     ⇒ 本轮回答交付后**立即收口**；失败轮不收口（留给重试与兜底），避免把可重试的上下文关掉。
  2. `task`（默认）：调用方在任务结束时显式 `DELETE /v1/sessions/{id}` 收口。
  3. **空闲兜底**：任何 web 会话空闲 ≥ `UWS_SESSION_IDLE_TTL`（默认 600 秒，`--session-idle-ttl` 覆盖，
     `<=0` 关闭）即自动收口 ——「用完没人关」不再堆垃圾页。回收协程周期 `UWS_SESSION_REAP_SEC`（默认 60 秒）。
- **接力清账**：跨对话接力（总结 → 新对话，新对话已成功开轮）后旧对话必须清账，窗口资源同步回收 ——
  见 §14「接力后的旧会话清账」（2026-10-02 用户定案）。
- **关前删会话（2026-10-03 派单 d27954eeb 收紧）**：`DELETE /v1/sessions/{id}` 默认**只解绑（关页）**；
  删除必须**显式 `?purge=1`**。只有站点实现了会话删除（当前 Gemini / 千问）才真删，其余站点返回 `skipped`
  并**只关页**，**不谎报删除**（§14 判据不变）。未登记的站点 = 有意不删，不做猜测式清理。
- **清账目标只认「会话自己的绑定对话」（2026-10-03 派单 d27954eeb；防再次误删）**：
  目标是 `Session.OwnConversationID` —— 只在站点**回传过真实 ID** 时写入（聊天响应体自报 / recipe
  `echo` 槽位回显 / S2 恢复入参 `site_conversation_id`），**禁止**用「页签当前地址」推断。
  删除前必须过一致性核对，任何一条不满足即 `status:"skipped"` ＋ `reason`，**只解绑不删**：
  ① `no_binding`（本会话没绑定过对话）；② `page_unreadable`（读不到页签当前对话）；
  ③ `mismatch`（绑定对话 ≠ 页签当前对话，响应带 `conversation_id` 与 `page_conversation_id` 两侧值）。
  删除前把**目标 id ＋ 侧栏标题**写进网关日志（可审计）。**为什么改成这样**：旧实现用
  `ConversationIDForSession`（读 `location.pathname`）当目标，而站点 SPA 会在删除/跳转后自行导航到
  **别的**对话 ⇒ 关会话会删掉「此刻页签所在的那条」；2026-10-03 实测误删一条不可恢复的真实对话
  （标题「简单的回应」，`c_16257c46d38d16d1`）。**取舍**：宁可漏删（有 `POST /v1/purge` 显式 ID 的补删
  通道）也绝不删错。内部收口路径（空闲回收 `reapIdleSessions`、一次性会话 `finishOneShot`）仍会尝试清账，
  但同样受这三条核对约束。
- **收口动作** = `purgeSessionOnClose` → `closeSession`，在持有该会话锁、无在途请求时执行；
  有在途请求的会话（锁被占）跳过，下一轮回收再试。
- **关页结果必须可分辨（T-34，2026-10-02 修）**：`DELETE /v1/sessions/{id}` 响应除
  `status:"deleted"`（**本网关会话已出账**，幂等）外，另报页签是否真的关掉：
  `tab_closed:true|false`；关不掉时附 `tab_error` 与 `tab_retained:true`，**账本条目保留**
  （`~/.uws/gateway-tabs.json`），由启动回收 `ReapOrphanTabs`（attach-cdp 模式）补关。
  旧行为（先出账、后关页、且忽略 `CloseContext` 错误）会让没关成的页签变成**永久孤儿**：
  账本里查不到它，启动回收也看不见。口径：**关页失败不谎报成功，也不丢回收线索**；
  `status:deleted` 只承诺「网关会话没了」，浏览器页签状态一律看 `tab_closed`。
- **同站点低频门与风控禁言熔断（账号安全，2026-10-03 用户定案收紧）**：
  同一站点**串行**（同站绝不并发），且同一站点两轮之间不小于站点最小间隔（默认 DeepSeek **60s**、其余 **15s**；`UWS_SITE_MIN_INTERVAL_SEC` 统一覆盖，`<=0` 关闭）。
  拟人化频控（`HumanPacer`）单会话两轮基准最小间隔 **15s**，每分钟上限下调至 **2 轮**。
  **风控/禁言感知强熔断（Circuit Breaker）**：若响应或页面特征包含“已被禁言/账号异常/限制发言”或“操作过于频繁/429”，
  归类为不可重放错误（`account_banned` / `rate_limited`），**绝不自动重试**；站点门禁自动进入熔断隔离（禁言锁 24h，限频锁 10m），
  冷却期内请求全部 fail-closed 拦截以保护真实账号，并向用户告警推荐切换官方 API 通道。
- **Gemini 临时会话**：页面自带 `button[aria-label="Temporary chat"]`（文本 `Turn on temporary chat`）；
  是否可作自动化默认**未验证**（`T-27`），默认**不开启**，不擅自改默认行为。
- **未实现（登记，不冒充现实现）**：进程重启后**上一进程遗留的会话页**（内存映射已丢）尚无启动对账回收，
  作 `T-29` 的执行项；当前靠「关闭 tab 后引擎释放上下文 + 父进程收尾」兜底。

### 会话绑定失活导致 chat 502（运维最短恢复；ACTIVE，2026-10-02 派单 d1f5493c6 实证）

**症状**：`GET /v1/models` 200、`POST /v1/chat/completions` 502（**毫秒级**返回，`t≈0.01s`），错误文本＝`initialize_context(<sid>): engine error code=13: acquire target: cdp: bound session target <tid> cannot be adopted; refusing a different target: 目标 <tid> 已不存在`。

**根因（页签被关 ＋ fail-closed，无自愈）**：引擎把「会话名 → Target」落盘到 `~/.uws/sessions.json`（0600、TTL 24h，`pkg/engine/session_store.go`）；同名会话在内存无活跃上下文时按该绑定续页并置 `cfg.RequireReuseTarget=true`（`pkg/engine/service_context.go`），接管失败即硬失败（`pkg/browser/cdp_session_core.go` 注释明写「持久 session 绑定则必须失败关闭」，防无 URL 的续页落到别的页签）。触发场景＝**那扇窗被关掉**（用户关窗／Chrome 重启／网关与引擎的页签回收）；而运行期没有 API 能清这条死绑定——`CloseContext` 在无内存上下文时提前返回、不删绑定（`pkg/engine/service_page.go`）。⇒ 只要同名会话再被请求，chat 会一直 502，直到人工清理。

**最短恢复（一条命令）**：`scripts/gateway-session-recover.sh`（＝删死绑定 → 重启引擎 → 可选 `--smoke`；`--dry-run` 只报不改）。手工等价三步：① 备份 `~/.uws/sessions.json`；② 删掉 `target_id` 不在 `curl -s http://127.0.0.1:9222/json/list` 里的条目（**CDP 不可达时必须中止**，否则会把活绑定全判死）；③ `webtouch daemon restart`，再重跑 chat。

**本轮恢复前后读数**：before `http=502 t=0.011s`；after `http=200 t=5.049s`（`model=deepseek-chat`、`uws_transport=in_page_direct`、`site_conversation_id=37673e63-4984-4c25-b7cd-87286d8b4bef`）。冒烟产生的那条站点会话已按 §14 口径删除（站点 API `POST /api/v0/chat_session/delete` → 200，复核列表已不含该 id），网关待删队列里的占位条目也已 `DELETE /v1/purge/<id>` 清掉。

**两条防复发已落地（ACTIVE，2026-10-03 派单 df506dd8f 真机实证）**：
① **fail-closed 分支加自愈**（`pkg/browser/cdp_session_core.go`）——调用方**显式给了 `initial_url`** 时允许「回退新开窗口」；**无 `initial_url` 时行为不变**（仍失败关闭，不偷偷落到别的页签）。
② **`CloseContext` 无内存上下文也删绑定**（`pkg/engine/service_page.go`）——把「删绑定」提到 `bc == nil` 提前返回之前，引擎刚重启（内存无上下文、落盘绑定还在）时 `DELETE /v1/sessions/<sid>` 也能清掉死绑定，不必重启引擎。
单测：`TestInitTargetRequireReuseFallsBackOnlyWithInitialURL`（有 `initial_url` 回退 / 无则维持失败关闭）、`TestCloseContextDeletesBindingWithoutInMemoryContext`；`go test ./pkg/browser/... ./pkg/engine/...` 全绿。
真机 A/B（同一条最小 `POST /v1/chat/completions`，`model=deepseek-chat`，会话各自预置一条 `target_id=DEADTARGET…`（32hex 不存在）的绑定）：旧构建 `http=502 t=5.157s`（`bound session target … cannot be adopted; refusing a different target`）→ 新构建 `http=200 t=4.905s`，引擎日志逐字 `browser: 会话续页目标 DEADTARGET… 不可用（目标 DEADTARGET… 已不存在），改为新开窗口`，新页签随即被 `rememberSession` 覆写成活绑定；②在「网关有内存会话 ＋ 引擎刚重启」路径上 `DELETE /v1/sessions/<sid>` 后落盘绑定确认消失（同路径下无内存上下文时 `CloseContext` 走到 `bc == nil`）。测试会话与页签已按 §14 清账，绑定表还原。
**第三处缺口已收口（2026-10-03 派单 d613c7908）**：`closeSession`（`apps/gateway/gateway.go`）在网关**没有**内存会话时只走 `closeLedgerTab`（CDP 账本）、**不通知引擎** ⇒ 网关重启过时 `DELETE /v1/sessions/<sid>` 清不掉落盘绑定。加固＝`closeSessionWithPurge`（`apps/gateway/lifecycle.go`，只有**显式删除**走这条）在「内存里没有该会话」时补一次 `dropEngineBindings`（对每个已登记引擎做幂等 `CloseContext`）。**刻意不放进 `closeSession` 本体**：首次建会话（`runChatRound` 的 `isNew` 分支）也调它，而那条路径必须保留绑定以支持「网关重启后续页接管原窗口」。真机验证：建 `gf-gap3-01`（gemini-flash `http=200 t=22.3s`）→ **只重启网关** → `DELETE /v1/sessions/gf-gap3-01` → `~/.uws/sessions.json` 里该绑定确认消失（加固前会留下）。单测 `TestCloseSessionDropsEngineBindingWithoutMemorySession`。

**根因二·同一 session_id 跨通道复用（ACTIVE，2026-10-03 派单 d613c7908 真机复现并加固）**：`runChatRound` 的 `isNew` 原先只看「有没有会话／模型是否变化」，**不看通道**；而 agent-hub 三角色共用 `session_id=default`（router/navigator＝`deepseek-chat`、synthesizer＝`gemini-flash`）⇒ gemini-flash 请求会**复用上一条 DeepSeek 页**，引擎把 `gemini_model_switch`／`gemini_answer` 打在 `chat.deepseek.com` 页上（日志逐字 `vlm: impossible url=https://chat.deepseek.com/a/chat/s/… semantic="gemini_model_switch"` 与 `extract … semantic="gemini_answer" … failed: no_match|url=https://chat.deepseek.com/…`）⇒ 白等一轮，网关内部预算耗尽即回 `502 Bad Gateway`。加固＝新增 `requiresNewSession`（`apps/gateway/chat.go`）：`sess.ChannelID != req.ChannelID` 同样判新会话，走既有 close+重建路径。真机 A/B（同一条最小 chat，先 `deepseek-chat` 再 `gemini-flash`，同一 sid）：加固前 `http=200 t=51.27s`（其中约 35s 打在错页上）→ 加固后 `http=200 t=14.88s`，且该窗口内 `gemini_*` 语义不再出现在 `chat.deepseek.com` 上。单测 `TestRequiresNewSessionOnChannelChange`。

**仍存的相邻项（登记，不在本轮范围）**：① 交替通道会持续「拆页 + 重建页」（冷建页约 15–22s、热复用约 5s），根治要靠调用方**按通道分 session_id**（如 `ahub-gemini`／`ahub-deepseek`）或网关按 `(session, channel)` 分桶——本单只做网关侧"别把语义打到错站点"，不改 agent-hub。② `POST /v1/purge` 的 `NormalizeConversationID` 不接受 DeepSeek 的「带连字符 uuid」形态（只接受 `/app/<16hex>`、`<16hex>`、`c_<16hex>`、`/chat/<32hex>`），且 `SupportsPurge("deepseek")=false` ⇒ DeepSeek 测试会话清账当前必须走站点 API。③ `scripts/gateway-session-recover.sh` 仍是「删死绑定 + 重启引擎」的最后兜底（fix① 之后它已不是必需，但清表仍靠它）。

### chat 502 抖动：超时预算与整轮重试（ACTIVE，2026-10-03 派单 d1957d57b）

**症状**：`gemini-flash` 对同一 prompt 间歇 `502`；失败轮耗时贴近预算值，成功轮也在 30–85s 摆动（与上一节「会话绑定失活」的毫秒级 502 不同）。
**定性（N=6 连打：同一 synth-prompt＋固定 session_id，旧构建 `fcda47b6`·60s 预算）**：成功率 5/6，逐次 51.8／69.6(502)／84.7／44.0／31.1／31.0 秒。失败逐字 `submission_outcome_unknown: engine error code=4: context deadline exceeded` —— 是**本网关自己的超时闸**（`chatTimeout`），**不是站点业务码**（`gemini` 登录态 `logged_in`）；冷建页只占约 15–22s，成功轮本身也要 31–85s ⇒ 判「通道本身就慢 ＋ 60s 预算切掉一部分」，**不是「冷建页预算不够」**。脚本 `/tmp/rc/jitter.sh`，日志逐字见 `~/.uws/gateway.log` 的 `chat 失败 … dur=…`。
**加固一（抬预算）**：`DefaultChatTimeout` 60s → **120s**（`apps/gateway/gateway.go` 导出常量；`cmd/uws-gateway` 的 `--chat-timeout` 旗标默认值改为引用它——**否则旗标自带的 60s 默认会经 `Options.ChatTimeout` 强制覆盖**，本单首轮即踩此坑）。启动日志新增 `chat 超时预算: 2m0s` 供核对。
**加固二（有界整轮重试 ＋ 逐次观测）**：整轮编排抽到 `apps/gateway/chat_retry.go` 的 `runChatWithRetry`；每次尝试落 `chat 失败 sid=… attempt=n/N http=<状态码> dur=…s retryable=… err=…`（状态码与 `writeChatError` 同源，经 `chatErrorStatus`；并记 dur 与 err 原文），重试成功落 `chat 重试成功`。可配常量：`UWS_CHAT_RETRY_ATTEMPTS`（额外次数，默认 1＝共 2 次）、`UWS_CHAT_RETRY_BACKOFF_MS`（退避基数，默认 2500，第 n 次 sleep n×基数）、`UWS_CHAT_RETRY_ON_TIMEOUT`（默认 0）。默认只重试**提交前就绪类失败**。
**T-26 边界（硬）**：默认**不对「本轮超时类失败」重放**——超时不能证明写未发生，重放可能让站点重复答一轮，违反 `chat_submission.go` 的 fail-closed 不变量；确需超时重试者显式 `UWS_CHAT_RETRY_ON_TIMEOUT=1`（自担重复风险）。守卫单测 `TestDefaultChatTimeoutCoversSlowChannel`／`TestChatRetryOnTimeoutDefaultsOff`／`TestIsRoundTimeoutMarkers`／`TestChatRetryEnvOverrides`/`TestChatErrorStatusMatchesWriteChatError`＋既有 `TestPostSubmissionInterruptionNeverResubmits`／`TestDirectFailureNeverFallsBackOrCompletes`。
**A/B 诚实标注（「after」被通道级退化污染）**：改动后同口径 N=6 ＝ **1/6**，逐次 175.5／63.9／91.8／171.7／86.5／112.8 秒；失败以 `submission_outcome_unknown: TypeError: network error` 为主（页内 `fetch` 抛错，**网关代码不会产生此串**）。同一时段 agent-hub 的 `ahub-gemini`（非本单改动对象）以同一条 `TypeError: network error` 连续失败，而页内对照探针 `GET /app → 200` 正常 ⇒ 判**站点侧对生成请求的瞬时退化/过载**（agent-hub synthesizer ＋ 本单 jitter 同时打同一通道），**非本改动引入**。正向信号：run 6 `http=200` 耗时 **112.8s**（旧 60s 预算下的产出更小）。⇒ 「抬预算」有单次支撑但 A/B 未净，待通道空闲复跑记入本节。
**复跑仍不净（2026-10-03 01:13–01:26，派单 d8d0b0ac3）**：agent-hub 已空闲（网关日志无 `ahub-*` 请求）时同口径 N=6 ＝ **0/6**，逐次 42.1／67.4／129.5／41.3／42.2／42.1s，失败仍以 `TypeError: network error` 为主；把 prompt 换成最小「只回答两个字：收到」也 **502 @35.9s**；页内对照 `GET /app → 200`、`/v1/auth/status` gemini＝`logged_in`。⇒ **通道级退化与调用方无关且非本改动引入**：失败发生在 36–42s，**早于 60s/120s 两档预算**，预算改动不可能造成该错误；退化始于约 00:57 并持续 >25 分钟，疑为站点侧对生成请求的风控/限流。**干净 A/B 至今未取得**，需通道恢复后再跑（判据：先跑最小 prompt 得 200 再计 N≥5）。

### gemini-web 502 `network error`：定位到环节（ACTIVE，2026-10-03 派单 da22dd2c5）

**症状**：`gemini-flash` 间歇 `502`，错误体逐字 `submission_outcome_unknown: TypeError: network error`；同一时刻 `deepseek-chat` 同句 200。
**环节定位（CDP `Network` 域铁证，非推断）**：对受控页 target 开 `Network.enable` 后跑真 gateway chat，抓到 `StreamGenerate` 逐字时序——`SENT` → **+2.1s `RESP status=200`**（响应头已到，`server-timing` 有值）→ **+32.2s `FAIL errorText='net::ERR_CONNECTION_CLOSED'`**（`canceled=False`、`blocked=None`、无 `loadingFinished`）。⇒ 响应头以 200 返回后，**连接在发出后约 30.0s 被服务端关闭**、正文流中断。**失败发生在响应链路，不在提交动作**。
**判「本线是否可加固」＝不可加固（站点/CF 侧）**，三条判据：① 响应头 200 已收到、流被服务端断开（CDP `ERR_CONNECTION_CLOSED`、非 `canceled`）⇒ 不是 CDP 提交失败；② 断点 +30.0s **早于**本网关 `chatTimeout` 120s（启动日志 `chat 超时预算: 2m0s`）⇒ 与 `719f58b4`／`20b9c44d` 的预算改动**无因果**；③ 页内 `window.fetch` 为 native、`WIZ_global_data.SNlM0e`／`FdrFJe` 均有值、`/v1/auth/status` gemini＝`logged_in` ⇒ 页状态与鉴权正常。**不硬凑兜底**：`dispatched=true` 时中断**不证明写未发生** ⇒ 不重试（守 `chat_submission.go` fail-closed 与 `T-26`）；提交前页内健康哨与失败后 close+rebuild 均**未落地**（前者在近全失败下无收益且多耗一次站点请求，后者对站点侧中断无疗效）。
**已落地加固＝错误可观测性（不是兜底）**：`apps/gateway/recipe/script.go` 页内脚本两个 `catch` 把**阶段＋上游来源**写进错误体——读流阶段中断 → `stream_interrupted_after_headers: … (httpStatus=<码>)`；派发阶段失败 → `fetch_failed[after_dispatch|before_dispatch]: …`。改前错误体只有裸 `TypeError: network error`，**无法区分「提交动作失败」与「响应链路中断」**，定位必须先做外部抓包。真机验证：重启后同口径失败轮逐字为 `stream_interrupted_after_headers: TypeError: network error (httpStatus=200)`（网关日志 01:57–02:00）。单测 `TestBuildFetchScript_GeminiShape` 增两条断言。
**`GET /app` 疑点澄清**：前单探针的 `/app` 是**页内相对路径 fetch**（`location.origin + "/app"` ⇒ `https://gemini.google.com/app`），**不是网关路由**；它回 200 只证明「页面上下文与静态资源可达」，与 `StreamGenerate` 是否被中断**无因果** ⇒ **不能**当通道可用性判据。网关无 `/app` 路由，打 `:8000/app` 才 404（正常）。
**提示词假说对照（2026-10-03 02:1x，用户现场提出「是提示词问题」）**：同一会话、同一时间窗内**交替**发两种提示词各 3 次——「只回答两个字：收到」**0/3**；「请用一句话说明今天适合做什么。」**1/3**（唯一成功 `99.5s`）。⇒ 本对照**不支持提示词为主导变量**（0/3 vs 1/3 在 n=3 下不可区分）；失败逐字与提示词无关（同为 `stream_interrupted_after_headers`），与「站点侧按时间/负载抖动」一致。**该对照功效不足**（n 小、通道本身抖动大），若要定论需在通道稳定期加样本。
**L1 真键复测（2026-10-03 04:0x–04:1x，派单 `d3bed1b68`；判「L0 失败降级 L1 能否救 gemini-web」）**：健康窗口内（前 2 次即成功）同句最小 prompt 两条路径对打——
① **L1 真键**（CDP `Input.insertText`＋`Enter` 可信输入事件驱动站点自己的 UI，非 JS 注入 value＋click）：**5/5**，`4088／6250／4231／4157／4204 ms`，回答首字逐字 `收到`；站点该对话 `user-query=5 / model-response=5` ⇒ **无重复提交**。
② **同窗 L0 XHR 对照**：**5/5**（另加 3 次早期探测亦 200，合计 8/8），`19277／21765／25086／28201／37930 ms` —— **同窗口 L0 也全通**，不是「L0 独挂」。
⇒ **落「换路径无收益，维持本线不可加固」**：健康窗口两者成功率同为 5/5（L1 仅快 4–8×，属延迟收益不是可靠性收益）；退化窗口两者同挂（02:1x L1 DOM 3/3 失败 vs 03:2x L1 真键 5/5 成功，说明差异来自**时间窗**而非提交路径）。
③ **双提交判据（部分取得，如实标注）**：本次健康窗口**未出现 L0 失败**（6 次探测全 200）⇒ 规定的「先让 L0 失败再走 L1」链路**未能构造**。但有一条等价读数：`gf-attr-01` 那条对话（`c_a706d3ba560e7ab7`）在 6 次成功 L0 ＋ 4 次失败 L0 之后，被一次真键 L1 完整加载，站点侧渲染出 `user-query=7 / model-response=7` ＝ **6 次成功 L0 ＋ 1 次 L1，4 次失败 L0 贡献 0 条用户消息** ⇒ 依据「发出后被掐的请求不在站点留用户回合」这一读数，**重放的重复提交风险低于原判**；但该读数只有 1 条、且计数经「真键提交触发对话完整加载」间接取得 ⇒ **`T-26` 默认不重放的口径本轮不动**，需退化窗口里补「L0 失败→L1 同对话」的正规链路再判。

**归因二段（2026-10-03 03:2x，派单 `d43a91548`；区分「受控页副作用」与「站点策略性掐流」）**：三个对照落地如下——
① **拦截对照（已排除）**：本栈只 enable `Runtime/Page/Network`（`pkg/browser/cdp_session_fingerprint.go:40`），**从不开 `Fetch`/请求拦截**；CDP `Network` 事件显示请求正常送出后由服务端关连接（`ERR_CONNECTION_CLOSED`），**无暂停未 continue 的请求** ⇒ 「我们自己拦截/卡住请求」被证伪。
② **可见性对照（操作未生效，A/B 作废）**：`Page.bringToFront` 前后两段，页签状态**实测完全相同**（`visibilityState=visible`／`hidden=false`／`hasFocus=true`）⇒ 该变量没被真正改变，A/B 不成立；但读数表明「置前台」并非变量：A 段 4/5（12834／20143／35889／41758✗／35359 ms）、B 段 2/5（37817✗／39102✗／38868✗／46400／38258 ms）——**同一前台状态下成功率随时间漂移**。
③ **人工基线（代理，真键输入）**：同一受控页、同一 profile、同一 prompt，用 CDP `Input.insertText`＋`Enter` 真键输入 ⇒ **约 13.0s 出答案**（页内最后一条助手消息逐字 `收到`，`users=7/assistants=7`）。**同一页签、同一账号、同一句**，差异只在「提交路径」（我们的页内 XHR vs 站点自己的 UI）。局限：这是真键输入的**代理**，不是用户本人手点。
④ **时间形态**：失败耗时**恒定聚在发出后约 30s**（网关侧 dur 37.8–41.8s＝导航/就绪 + 30s），且**与提示词内容无关**（极简 0/3 vs 正常 1/3，错误体逐字相同）⇒ 是站点侧对**长连接生成请求**的策略/限流特征，不是内容触发。
**落三态**：**站点侧对页内自动化 `StreamGenerate` 策略性掐流**（非受控页副作用、非本栈拦截）。**「本线不可加固」结论复核后维持**（③ 的差异说明「换提交路径」可能有救，但 L1 DOM 在同一时段也曾 3/3 失败 ⇒ 该救法未证实；结论待通道健康窗口用 L1 N≥5 复测后再动）。实验开关已完全还原（`direct_enabled=[deepseek,gemini,qianwen]`），自建会话与站点对话均已清账（逐字日志见 §9 上文/网关日志 02:26:36 `purge: 关会话清账 target=c_a706d3ba560e7ab7 title="简短确认" … verified`）。

**真机读数（最小 prompt，固定 session_id，N=5／段）**：加固前 **2/5**（`62.4／66.7／47.9／19.9／14.1s`）；加固后 **1/5**（`54.0／47.4／44.5／46.0／19.7s`）。⇒ 通道**间歇可用但未恢复**，「先证可用再计」的干净 A/B **仍不成立**（判据未满足，不谎报）。

### 站点会话身份（`site_conversation_id`；`DRAFT` · `T-31` 待实现，2026-10-01 用户现场指令）

- **两个 ID 必须分开叫**：`session_id` ＝ **本网关**的页签会话（引擎 `context_name`，`POST /v1/sessions` 建的那个）；`site_conversation_id` ＝ **站点服务端**的对话（Gemini `/app/<16hex>` ⇒ `c_<16hex>`，DeepSeek `/a/chat/s/<uuid36>`）。页签随时可回收重建，站点对话是持久的——**接着聊靠后者，不靠前者**。
- **响应回传（S1）**：chat 响应带 `site_conversation_id` 与 `page_url`；尚无服务端对话（如新对话首轮）时回 `null`，**不得**用当前页地址或猜值填充。
  - **字段级冻结（2026-10-01；实现必须逐条对齐）**：
    - **位置**：`site_conversation_id` / `page_url` 是**响应体顶层字段**，与 `session_id` / `context_id` / `target_id` 同级；chat 与 responses 两条对外面共用同一回填点，不得各写一套。
    - **非流式**：字段**恒定出现**；读不到为 JSON `null`（不省略、不用 `""` 代替）。
    - **流式（SSE）**：只在**完成帧**（带 `finish_reason` 的那一帧）出现；`data: [DONE]` 之前的帧不得带该字段。
    - **取值**：`page_url` ＝ 回合结束时该页签 `location.href` 原文；`site_conversation_id` 取**按序第一个非空**的来源：① 由 `page_url` 按 §13 站点模板归一化（Gemini `c_<16hex>`、DeepSeek 原样 uuid36）；② **本次 L0 直发响应体**里站点自报的会话 ID（Gemini：`wrb.fr` 帧内层 `[null,["c_<16hex>","r_…"], …]` 的 index `1[0]`）；③ **本轮 L0 直发实际使用的站点会话槽位回显**（recipe 中 `producer: session_mint` 且 `echo: true` 的槽位，如 DeepSeek `chat_session_id`；由页内执行器在发起请求时算出并随启动响应回显，值 ＝ 本轮请求体里真正写进去的那个 ID）。地址不是该站点对话页（含通道首页）⇒ 落到来源 ②；②③ 都读不到 ⇒ `site_conversation_id` 为 `null`，`page_url` 仍如实回传。**禁止**用当前页地址或猜值填充。
      - **2026-10-01 修订（用户现场指令 ＋ 真机原文）**：原稿的单一来源「值必须来自页面真实地址」被真机证伪——**L0 页内直发不驱动站点 SPA 路由**，页地址恒为通道首页 ⇒ 单一来源结构性拿不到 ID；而真机抓包证同一条 `StreamGenerate` **响应体自带会话 ID**（逐字帧见 [`evidence/pilots/site-conversation-continuity-2026-10-01.md`](evidence/pilots/site-conversation-continuity-2026-10-01.md) §七）。字段位置、`null` 语义、SSE 帧位等**其余冻结条目不变**。
      - **2026-10-01 修订二（同批真机收口）**：**L0 学到 ID 后必须把该页签地址对齐到该对话页**（`history.replaceState`，只改地址：不重载、不渲染、不改站点状态；已在对话页则幂等跳过）。理由＝请求侧与回传侧必须共用同一事实源：recipe 的会话槽位（Gemini `conv`）来源是 `from_url`，L0 不驱动 SPA 路由时页地址恒 `/app` ⇒ **下一轮请求不带会话位、站点每轮新开一个对话**（真机实测连问三轮拿到三个 ID、侧栏越堆越多）。对齐后两轮同 ID、侧栏该对话 1 条。
      - **2026-10-02 修订三（DeepSeek 覆盖，本线程）**：DeepSeek 的会话 ID 由 recipe 的 `session_mint` 生产（`POST /api/v0/chat_session/create` 响应 `data.biz_data.chat_session.id`），**不在响应流里** ⇒ 来源② 结构性读不到，故增加来源③（`echo` 槽位回显）并把 `ExecuteInPageDirectStream` 启动响应的 `slots` 接到身份回填。回传侧与请求侧仍是同一事实源（同一个槽位值写进请求体 `chat_session_id`）；地址对齐（来源①）在 DeepSeek 上同样生效（`/a/chat/s/<uuid36>`），下一轮 `session_mint` 的 `re` 直接从地址取值、不再新建会话。真机取证见 §十二。
    - **两条链路都要回填**：L0 直发（`uws_transport: in_page_direct`）与 L1 DOM；任一路径缺失即判 S1 未完成。
    - **禁止**：拿 `session_id` / `context_id` 冒充站点 ID；用通道 `page_url` 凑数；省略字段。
- **按 ID 恢复（S2）**：`POST /v1/sessions` 与 chat 请求可带 `site_conversation_id`（body）或头 `x-uws-site-conversation-id`；命中则新建 / 重建的页签**直接落该对话地址**，不落通道首页（`channels.json` 的 `page_url` 只作默认入口）。
  - **命名禁令**：body 的 `conversation_id` 已被占用为「本网关会话键」别名（`handler_chat.go`），恢复入参**不得**复用该名。
  - 站内地址模板按 recipe 家族维护（同 §13）：Gemini `/app/<hex>`、DeepSeek `/a/chat/s/<uuid>`；未登记模板的站点 fail-closed 拒绝恢复，不猜地址。
- **列表（S3）**：`GET /v1/sessions/{id}/conversations` 在**该会话所在页**枚举站点侧栏，返回 `id` ＋ 标题；读不到回空数组 ＋ `error`，**不虚构**。用途：调用方选对话续聊；§14 清账由「侧栏布尔校验」升级为「列表核对」。
- **记忆归属（硬边界）**：站点对话 ID 由**调用方**记忆（Agent Hub 账本），网关**不落盘**会话账本——网关只提供「给我 ID，我落到那一页」的能力；两处各存一份必然漂移（引擎 `~/.uws/sessions.json` 的 `url` 字段已注明「只作诊断用，不参与判定」）。
- **清账不受影响**：本能力不改 §14 判据与硬边界（不删用户真实历史对话、不提供批量清空）。

## 10. 登录状态

- 首次实际请求、以及上一次确认已超过 `--auth-check-ttl`（默认 15 分钟）时，网关经 CDP
  在当前页面做**只读** DOM 探测；探测不会输入、点击或发网络请求。
- 仅 `logged_in` 放行模型/开关/输入/发送/页内直发。`logged_out` 或 `challenge` 返回 HTTP 401
  （`login_required` / `human_verification_required`）；页面不明确或探测失败返回 HTTP 503
  （`authentication_check_failed`）。三种情况都 fail-closed，绝不提交用户内容。
- 明确 `logged_out` / `challenge` 会写入 `~/.uws/gateway-auth-state.json`（仅状态码、检查时间与提示，权限 `0600`），跨重启保持锁定，不会按 TTL 自动重探；页面不明确或探测失败的 `unknown` 在 TTL 内阻止会话创建和网页动作，到期后只允许一次只读复检，仍未知则跨重启持续锁定，直到人工确认页面状态并显式 reset。登录成功状态不跨重启信任。
- 用户在相应 Provider Profile 中手动完成登录或处理挑战后，显式调用
  `POST /v1/auth/reset`，body 为 `{"channel_id":"deepseek-web"}` 这类网页通道 ID。该端点只清本地状态；下一次聊天请求会进行一次只读检查，不会自动登录、点击或发送。必须先完成页面人工处理，再清除锁定。
- `GET /v1/auth/status` 只返回缓存状态（含 channel ID、复检次数与是否需人工 reset；初始为 `not_checked`），不创建标签页、不导航、不触发检查。默认 TTL 是 15 分钟，可通过 `--auth-check-ttl` 调整。

## 11. 移植范围（Python → Go）

| Python 现状 | Go 目标 |
|---|---|
| `api_server.py`（877 行） | `apps/gateway` handler + sessions |
| `engine_client.py` + `grpc_client.py`（295 行） | `apps/gateway` engine client（复用 proto） |
| `providers/base.py` + `deepseek.py` + `gemini.py`（208 行） | `apps/gateway/channels`（模型表 + 语义映射） |
| 对应测试 | Go 单测；Python 测试保留作行为规格对照（服务端仍用） |

新增代码位置：`cmd/uws-gateway/main.go` + `apps/gateway/`（handler / channels / sessions / config）。

## 12. 测试策略

- Go 单测：路由 / 参数映射 / 模型解析 / 错误语义；
- 集成：伪引擎 gRPC（复用现有测试基建）；
- 真实 Chrome E2E：沙箱外，复用现有门控；
- Python 侧 api_server 测试保留为服务端行为规格。

## 13. 站点 recipe（嗅探 → 模板 → 回放；DRAFT，2026-09-26 首跑取证）

- **定位（2026-10-01 目标扩界）**：面向在线 Agent AI 对话与 AI 搜索服务（Gemini、DeepSeek、ChatGPT、Claude 及 AI 搜索站点），将「一次物理交互因果 Trace」转化为**声明式 Recipe**，实现 In-Page Direct 秒级直发与自动降级；`in_page_direct.go` 现有站点硬编码为过渡形态，最终统一由 Recipe 执行器驱动。
- **首跑证据**：[`evidence/pilots/sniff-recipe-first-capture-2026-09-26.md`](evidence/pilots/sniff-recipe-first-capture-2026-09-26.md)（Gemini 一条完整链路 + 槽位对照表：`at`/`f.sid`/`bl` 三者逐字命中 `window.WIZ_global_data.SNlM0e` / `FdrFJe` / `cfb2h`）。
- **结构**：骨架＝稳定部分，槽位＝每次新取。

```json
{
  "site": "gemini",
  "match": {"origin": "https://gemini.google.com", "path_re": "^/app"},
  "request": {
    "method": "POST",
    "url_template": "…/StreamGenerate?bl={{bl}}&f.sid={{f.sid}}&hl=en&_reqid={{reqid}}&rt=c",
    "headers": {"Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
                "X-Same-Domain": "1", "x-goog-ext-73010989-jspb": "[0]"},
    "body_template": "f.req={{f.req}}&at={{at}}",
    "body_builders": {"f.req": {"kind": "json_outer",
      "template": [null, "json_string([[{{message}},0,null,null,null,null,0],[{{hl}}],null,{{conv}}])"]}}
  },
  "slots": [
    {"name": "at",    "producer": "page_global", "path": "WIZ_global_data.SNlM0e", "scope": "page_load"},
    {"name": "f.sid", "producer": "page_global", "path": "WIZ_global_data.FdrFJe", "scope": "page_load"},
    {"name": "bl",    "producer": "page_global", "path": "WIZ_global_data.cfb2h",   "scope": "site_build"},
    {"name": "reqid", "producer": "counter", "seed": "last_sent_reqid", "step": 100000, "scope": "page_load"},
    {"name": "conv",  "producer": "from_url", "re": "^/app/([0-9a-f]{16})", "fallback": null, "scope": "conversation"},
    {"name": "uuid",  "producer": "fresh_uuid", "scope": "request"}
  ],
  "response": {"kind": "xhr_stream", "frames": "unverified"},
  "drift": {"probe": "recipe_probe", "triggers": ["on_failure", "scheduled"]}
}
```

- **槽位生产者类型**（首跑只证到 `static` / `page_global` / `from_url` / `counter` / `fresh_uuid`；`from_last_response`、`session_mint` 属后续轮，**未证实**）。
- **槽位可选性（2026-10-01 实测补条款）**：`optional: true` 表示该槽位在首轮可合法缺失（Gemini 会话 ID 即此形态），缺失时按 `null` 传入并交给 body 构造器决定形态；它与「必需槽位缺失即 fail-closed」是两件事，后者是安全边界，**不得**为让请求发出去而放宽。
- **因果合成推导（Interaction-to-Recipe Synthesizer）**：
  1. **参数化挖掘**：比对物理交互发送的 Input Prompt，在请求 URL 或 Body 中自动定位参数槽位；
  2. **槽位自解推导**：对未知 Token/Nonce，在 `PageContextSnapshot`（`window` 对象树、Cookie、URL）中通过广度优先搜索（BFS）自动建立 `producer` 映射；
  3. **流式对齐**：将页面渲染的最终输出文本与网络 Chunk 子串模糊对齐，确定 SSE / NDJSON 分帧格式与提取路径。
- **四条硬要求**：
  1. **两态**：同一站点的首轮（无会话 ID）与后续轮（回填会话 ID）必须是 recipe 内的一条分支，不是两套代码；
  2. **槽位自解**：页面态/token 一律运行时现取，recipe 文本内**禁止**落明文 token（同 §5 secret 口径）；
  3. **自证回放门禁**：recipe 生成后必须在页面上下文内执行一次真实 `window.fetch` 回放，HTTP 200 且文本正确方可标记 `ACTIVE`；
  4. **双轨自愈兜底**：直发未命中、验证失败或线上漂移时，fail-closed 自动降级为 WebPage DOM 物理交互，绝不阻塞任务。
- **落地**：抓存与合成器落 `apps/gateway/`；响应捕获落 `pkg/browser/modelbridge/`；任务 `Tasks.md` `T-25`。
- **嗅探抓存（S1，2026-10-02 落地）**：`requestWillBeSent` 解析面扩到 `method`/`headers`/`postData`/`postDataEntries`（仍只认 XHR/Fetch）；候选＝XHR/Fetch ＋ 非安全方法，响应 `text/event-stream` 兜底升级；凭据头脱敏（只留键名）；原始快照原子落盘 `~/.uws/recipes/captures/`（`UWS_RECIPE_CAPTURES` 可覆盖，空串＝关停；目录 0700/文件 0600，临时文件＋rename）。**纯被动旁路**：不改页面、不注入 JS、不暂停请求。S1 落的是**原始快照**而非 recipe，为的是抓错/切错两段证据分开。代码落点 `pkg/browser/recipe_capture.go` ＋ `cdp_session_events.go` 三处接线；任务 `Tasks.md` `T-25`。
- **嗅探编译（S2，2026-10-02 落地；产物恒 `DRAFT`）**：把 S1 的原始快照编译为 §13 schema 的 recipe 骨架＋槽位。入口 `CompileCaptureRecipe(SniffOptions{Site, Page *recipe.PageContextSnapshot}, captures []browser.RecipeCapture) (*SniffResult, error)`，产物＝`Recipe` ＋ `Notes []SniffNote{Where,Reason}` ＋ `Main`（选中的主请求），**恒 `DRAFT`**（不写 `status`、不激活、不放行直发）。
  1. **主请求选择**：响应 `text/event-stream` 者优先（权重 `1<<20`），否则 `PostData` 更长者；落选者留痕（一次抓存可含多份请求）。
  2. **`match` 只写页面路由**：`SniffOptions.Page.URL` 缺失即 **fail-closed 报错**；`path_re` 取页面 pathname、`origin` 取页面 origin——**不得**把抓到的 API 路径写进 `match`（见「DSL 扩展」第 4 条）。
  3. **切槽语义**：query 与 urlencoded 体把值**整体**换成 `{{slot}}`（保留原始编码形态，渲染端 `encodeURIComponent` 下即「只编码一次」）；JSON 体只换**字符串叶子**（数字叶子保留字面量，避免 `String(val)` 改类型）。
  4. **头部三分类**：浏览器托管头（`user-agent`/`referer`/`origin`/`sec-*`/`accept*`）静默不收；功能面白名单（`content-type`/`x-same-domain`/`x-requested-with`/`x-platform`/`x-device-id`/`x-deviceid`/`prod_id`/`x-goog-ext-*`）接收并对动态值切槽；凭据类头与其余未知头**不收**并留痕。
  5. **凭据边界**：token/csrf/xsrf/auth/secret/sig/password/cookie/apikey 命名的值、凭据头、以及 S1 已脱敏的 `[REDACTED]`，一律**丢弃该字段并留痕**，**不得**降级为 `static`（承「槽位自解」硬要求第 2 条）。
  6. **producer 两级判定**：页面态回溯优先（BFS 找 `window` 字符串/前缀、`localStorage`、`cookie`、页面路径 ⇒ `from_url`）→ 形状/命名启发兜底（数字且名含 reqid/req_id/counter/seq/ts/nonce ⇒ `counter`；UUID ⇒ `fresh_uuid`；≥16 位纯十六进制 ⇒ `fresh_hex`）。**凡走启发分支必须留 `SniffNote` 写明依据**（编译输入不含页面态，可复盘）。
  7. **落地与验证**：`apps/gateway/recipe_sniff.go` ＋ `recipe_sniff_test.go`（五例：有/无页面态、事件流优先、缺页面 URL 报错、凭据不落明文）；`go test ./apps/gateway/ -count=1` 整包绿。**边界（未接线）**：无诊断路由、无 `~/.uws/recipes/captures/` 读盘入口、未与 `recipe.Synthesizer` 合并——编译出 `DRAFT` 只是「可复盘的骨架」，不等于可回放。
- **边界**：本 schema 严格限定在浏览器「页面内直发」（in-page fetch，复用真实安全上下文）；不搞脱机伪造。
- **激活门禁与验证产物（2026-10-01 增补，`T-25` 落地条款）**：
  - **验证入口**：`POST /v1/diag/recipe/verify`，入参 `{"site":"gemini","session_id":"s-xxx","prompt":"…"}`（可选 `timeout_ms`、`persist`）；在**该会话所在的真实页面上下文**内跑一次 `window.fetch` 回放。
  - **通过判据（取代 2026-09-26 的「收到数据块」）**：HTTP 200 **且**响应帧经 `pkg/browser/modelbridge` 解码器解出**非空回答文本**。「有 chunk」不再算通过。
  - **状态迁移**：通过 ⇒ `ACTIVE`；失败 ⇒ `DRIFTED` 并记录漂移原因（只报不改）。`DRAFT`/`DRIFTED`/`DISABLED` 一律 fail-closed 回退 DOM，不因模板存在而直发。
  - **验证产物落盘**：通过的 recipe 写入 `~/.uws/recipes/active/<site>.json`（`UWS_RECIPE_ACTIVE_DIR` 可覆盖目录），启动时覆盖内嵌同名模板 ⇒ 激活跨重启有效；未通过的站点不写。产物含 `verified_at` 与实测回答，作为「何时验过、验过什么」的唯一凭证。
  - **两态验证**：同一 recipe 需在 `/app`（首轮，会话槽位为空）与 `/app/<16hex>`（后续轮，槽位回填）各跑通一次。
  - **代码落点**：`apps/gateway/recipe/verifier.go`（文本判据）、`apps/gateway/recipe/overlay.go`（产物加载/落盘）、`apps/gateway/handler_recipe.go`（验证路由）。
- **逐层降级阶梯（2026-10-01 用户定案）**：对外链路按「先试最有把握、最省资源的一档，不行才降」的顺序走，只准相邻降级：
  1. **L0 页内直发**：站点 recipe 为 `ACTIVE` 且 `CanDirectTransport(site)` 为真 ⇒ 一次页内 `window.fetch`（Gemini 实测 3.6–5.6s；DeepSeek 实测 473ms／771ms，比 DOM 快 8–20 倍）；
  2. **L1 真实网页执行（并同趟嗅探）**：L0 不可用（未激活／已漂移／**派发前**失败）⇒ 走一次真实 DOM 物理交互。这一层**同时承担两件事**——
     - **对本次任务交付**：正常交互出答案，任务不因直发不可用而失败；
     - **顺便嗅探**（用户 2026-10-01 口径：降级到网页时「顺便嗅探一下」，网页执行的**目的**就是嗅探出 XHR）：同趟旁路采样这次交互真正发出的功能面请求与响应，落成因果 Trace ⇒ 喂合成器 ⇒ 页内自证 ⇒ 覆盖写 recipe 产物 ⇒ 下次同类请求回到 L0；
  3. **嗅探不单独成档**：`requestWillBeSent` 只能观测**真实发出**的请求，因此没有「先单独嗅探一次」的档位——嗅探必须伴随一次真实页面执行；
  4. **降级边界（承 §5 提交边界）**：只有**派发前**失败（`not_dispatched`：未激活、槽位缺失、脚本构造失败）才降到 L1；一旦 `fetch` 已派发，网络错误／队列丢失／解析失败**一律报错不重发**，不回落 DOM——否则同一问题会在站点留下两份提交（用户可见的重复消息）；
  5. **可观测性**：每次回答标明实际走的档位（`transport: in_page_direct | dom`），「为什么慢」可直接归因；
  6. **代码落点**：`apps/gateway/chat_direct.go`（L0 入口与 `errDirectNotDispatched` 降级判据）、`chat.go`（DOM 兜底）、采样与合成接线待办见 `Tasks.md` `T-25`。
- **recipe DSL 扩展（2026-10-01，DeepSeek 实测倒逼；首批生产者只覆盖 Gemini 形状）**：
  1. **`local_storage` 生产者**：从 `localStorage` 取值，支持**两层语义**——`key` 取原始串，`decode:"json"` 先 `JSON.parse` 再按 `path` 下钻（`userToken` ⇒ `{"value":"Bearer …"}` ⇒ `value`）；凭据类槽位**只能**走它，禁止落成 `static`（同「槽位自解」硬要求）；
  2. **异步生产者（`await`）**：主请求前需要「先请求、再计算」的前置步骤（DeepSeek PoW：`create_pow_challenge` ⇒ 页面内 worker 解 `DeepSeekHashV1` ⇒ base64 成 `x-ds-pow-response`）。槽位求解循环必须支持 `await`；**优先复用站点自带能力**（直接跑站点自己的 worker chunk，随站点版本自动跟新，不自行逆向 wasm）；
  3. **`session_mint` 生产者**：首轮不存在会话 ID 的站点需要「先建会话再发」（DeepSeek `chat_session_id:null` ⇒ 422）。生产者支持 `re`（页面 URL 已有会话 ID 就直接取）＋ `endpoint`（没有则调站点建会话来建，从 `id_path` 取新 ID）；**新建的会话 ID 必须进 §14 待删除列表**（清账纪律不因直发而破）；
  4. **`match` 只匹配页面路径**：`path_re` 写页面路由（如 `^/a/chat/s/`），API 路径只出现在 `request.url_template`；合成器不得把抓到的 API 路径写进 `match`（语义错位会让所有页面都匹配不上）。
  5. **会话 ID 回读（`T-31`，`DRAFT`）**：`from_url` 目前只把地址里的会话 ID 填进请求；新增**回读通道**——回合结束把该值（连同页地址）交给调用方（见 §9「站点会话身份」）。与槽位同源同评：值优先取页面真实地址、其次取本站点自报的 L0 直发响应帧（2026-10-01 修订，见 §9「站点会话身份」取值条），两者都读不到回 `null`，且与请求里实际使用的值不一致时以读数为准并报 `drift`。**学到 ID 后须把页签地址对该对话页（同 §9 修订二）**，否则「填进请求」这一步下一轮仍取空——回读与回填必须闭环到同一事实源。


- **站点接入清单（SOP，2026-10-02 用户「继续接各家，要求一样」定案）**：接一家站点＝下面 10 条**逐条有证据**，
  缺任一条不得宣称「已支持」；顺序即依赖顺序（前条是后条的前件）：
  1. **通道登记**：`channels.json`（或 `DefaultConfig`）加 `uws_web` 条目 = `id`（如 `qianwen-web`）＋ `site`（＝recipe 的 `site`）＋ `page_url`；
  2. **登录态判据**：先证「免登录可用」还是「必须登录」——**必须登录属账号类动作，归用户本人**，不得代跑；
  3. **请求形状**：抓包逐字落 URL/query/头/体，**每个动态值都必须归到 producer**（页内可取 / 计算产物 / 每次新生成）；
  4. **签名器**：站点自带签名函数 ⇒ 走 `request.signer`（见下条，**2026-10-02 实现已接**）；无 ⇒ 按 `pow_challenge` / `session_mint` / `fetch_field` 类生产者处理；
  5. **两态**：首轮（无会话 ID）与后续轮（回填）**必须是 recipe 内的一条分支**；首轮 ID 由站点分配还是客户端自铸，须有实测判据；
  6. **响应分帧**：帧格式 ＋ **完成标志** ＋ 取值路径（例：`event:complete` ＋ 最后一条 `multi_load/iframe` 且 `status=="complete"`）；
  7. **会话身份（§9）**：把站点对话页路径模板与 ID 归一规则登记进 `SiteConversationFromURL` / `NormalizeSiteConversationID`，未登记站点一律 fail-closed；
  8. **清账落点（§14）**：定位列表端点与删除端点（或页内可读的列表缓存）；**删除端点不存在时要明说「本站不可清账」**，不得假装已清；
  9. **自证回放**：`POST /v1/diag/recipe/verify` 两态各跑通（HTTP 200 ＋ 非空回答）才允许置 `ACTIVE`；
  10. **残留自清洁**：验收/探针脚本收尾清账（`T31_PURGE` 范式）＋ 页签回收（§9 `tabreaper`），站点与浏览器两侧都不留测试残留。
- **`fetch_field`（新增生产者，2026-10-02 千问后续轮 `topic_id` 倒逼；实现已接）**：**值只在服务端、页内无落点**时的「派发前只读取数」——
  `{"name":"topic_existing","producer":"fetch_field","endpoint":"…/session/get?…&ut={{ut}}","method":"POST","body":"{\"session_id\":\"{{session_from_url}}\"}","headers":{…最小头集…},"path":"data.topic_id","optional":true}`；
  语义＝先用**已求解槽位**渲染 endpoint/body（`{{slot}}`），发一次页内 `fetch`（`credentials: include`），按 `path` 取字段；`optional: true` 时取不到返回空（交给 `if_slot` 两态分支），非 optional 取不到即 `not_dispatched`。
  `headers` 可覆盖默认头集（只读取数常需最小头，避免跨域预检被拒）。`cookie` 生产者同步支持 `key` 指定 cookie 名（缺省仍取槽位名）。

- **`request.signer`（2026-10-02 定规范、同日实现已接）**：站点把签名放在**页面自带的异步函数**里时（千问 `window.__QIANWEN_CHAT_SDK__.qwenSign(url)` 即此形态），
  recipe 只声明调用路径与取值位置，**签名值一律不落 recipe 文本**：
  `"signer": {"path": "__QIANWEN_CHAT_SDK__.qwenSign", "url_from": "signedUrl", "headers_from": "signedHeader", "timeout_ms": 10000}`；
  语义＝① 先用模板＋槽位拼出「待签 URL」；② `await getPath(window, path)(url)`（以路径父对象为 `this` 调用）；③ 用 `url_from` 覆盖实际请求 URL、并把 `headers_from` 的键值并入请求头；④ **任一环失败即 `not_dispatched`（fail-closed，可无损降级 DOM）**。Eval 只调站点自带函数，不重实现签名算法。
  **2026-10-02 实现判据**（`apps/gateway/recipe/{dsl.go,script.go}` + `recipe_test.go`）：`request.signer.path` 为空即 `Validate` 报错；未声明 signer 的 recipe **脚本里不出现签名段**（`signerSnippet` 只在声明时注入）；声明后脚本含「取父对象 → 非函数即 `signer_not_function` → `signer_timeout` → `signedUrl` 覆盖 → `signedHeader` 并入 → 失败 `not_dispatched`」，且覆盖 URL 早于 `window.fetch`（三条单测锁定）。

- **实例：千问（`qianwen`，2026-10-02 **自证回放通过并 `ACTIVE`**；`~/.uws/recipes/active/qianwen.json` 为激活产物）**：页面 origin `https://www.qianwen.com`（`tongyi.com` 重定向；`chat.qwen.ai` 是国际站，未选）；
  补全＝**一条** `POST https://chat2.qianwen.com/api/v2/chat`（**跨域绝对 origin**，与页面同源策略无关，`fetch` 直发即可）；
  对话页 `/chat/<32hex>`；签名走 `signer`（10 个头由 `qwenSign` 产出，`ut`/`nonce`/`timestamp` 由 `signedUrl` 自动补）；
  body 动态值＝`req_id`（32hex，同时镜像到 `x-chat-id`/`x-wpk-reqid`/`x-chat-biz.chatId`）、`session_id`（32hex）、`topic_id`（32 位 base64url）、
  `scene_param`（`first_turn`/`continue_chat`）、`parent_req_id`（首轮 `"0"`）、`prompt`；**首轮两 ID 可客户端自铸**（实测 200，⇒ 不需要 `session_mint`）；
  清账：列表落点＝`localStorage["qw_cache_sidebar:<uid>:first-page-sessions"]`（含 `sessionId`/`summary`/`sessionType`/`createTime`），
  详情＝`POST https://chat2-api.qianwen.com/api/v1/session/get`（**必须带 `ut`**，否则 400 `code:10001`）；**删除＝`POST https://chat2-api.qianwen.com/api/v1/session/delete/batch`**，body `{"session_ids":["<32hex>"]}`，响应 `data.delete_success:true`（2026-10-02 定位并实删核验；清账判据＝2xx ＋ `delete_success` ＋ `page/list` 不再列出）；
  `topic_id` 来源**已解**（2026-10-02）：**只在服务端**——`POST https://chat2-api.qianwen.com/api/v1/session/get?biz_id=ai_qwen&…&ut=<b-user-id>` body `{"session_id":"<32hex>"}` ⇒ 200 `data.topic_id`（实测 `QWt0pic…`）；页内 localStorage/全局确无落点 ⇒ 新增 `fetch_field` 生产者承接（见上条），recipe 用 `if_slot` 表达两态：
  ① `session_from_url` 命中（URL `/chat/<32hex>`）**且** `session/get` 回带 `topic_id` ⇒ `scene_param=continue_chat`、`topic_id` 用服务端值；
  ② 否则（新页 URL 无 ID＝真实首轮态 / 服务端无该会话记录）⇒ 客户端自铸 `session_new`(32hex)＋`topic_new`(24B→32 位 base64url)、`scene_param=first_turn`（§十三.4 实测 200）。
  **实测**：新标签页落在 `https://www.qianwen.com/chat`（无 ID）；`POST /v1/diag/recipe/verify` ⇒ HTTP 200 ＋ 解出回答（`elapsed_ms≈2.8s`，`bytes≈9.8KB`），站点侧新建会话已按 §14 清账（`POST /v1/purge{channel_id:qianwen-web}` ⇒ `deleted`/`verified`）。
  实测与案卷：[`evidence/pilots/qianwen-guest-direct-send-2026-10-01.md`](evidence/pilots/qianwen-guest-direct-send-2026-10-01.md)、[`qianwen-sse-frames-2026-10-01.md`](evidence/pilots/qianwen-sse-frames-2026-10-01.md)。

- **实例：ChatGPT（`chatgpt`，2026-10-02 **接入中**：SOP 第 1／7 条已落地，第 2–6／8 条已取证，第 9／10 条未做）**：页面 origin `https://chatgpt.com`；对话页 `/c/<uuid36>`（ID 原样，已登记进三处会话身份函数）。
  **模式硬约束＝只用 composer 上方的「聊天」，绝不点「工作」**（额度主要耗在工作模式，2026-10-02 用户口径）；顶栏「切换模式：ChatGPT／Codex」是另一条产品线，不走；「临时聊天」为临时对话、不入历史，可作零残留测试通道。
  补全＝一次发送 **5 步**：① `POST /backend-api/f/conversation/prepare` → ② `POST /backend-api/f/conversation`（**主请求**，响应 `text/event-stream`）→ ③ `POST /backend-api/sentinel/chat-requirements/prepare` → ④ `POST /backend-api/conversation/init` → ⑤ `POST /backend-api/sentinel/chat-requirements/finalize`。
  **首轮客户端自铸**：`parent_message_id="client-created-root"`、message `id` 与 `create_time` 由客户端生成 ⇒ **不需要 `session_mint`**（与 DeepSeek／千问不同型）。
  **两态（2026-10-02 16:2x 实测，字段级）**：首轮**无 `conversation_id` 字段**、`parent_message_id="client-created-root"`；续轮补 `conversation_id` ＋ `parent_message_id`＝上一轮**助手**消息 id（实测 `607558c1-…`）⇒ 两态 diff 只此三处。注意 `prepare` 带回的父 id 与主请求**不一致**（`4f698990-…` vs `607558c1-…`），直发不得取 prepare 的 id。
  **响应分帧（2026-10-02 16:5x 实测）**：主请求 SSE 共 8 帧块（`event: delta_encoding`/`data: "v1"` → `input_message` → `event: delta`（v1 增量编码 `{p,o,v}`）→ **`stream_handoff`** → metadata ×2 → `data: [DONE]`）；**答案正文不在 SSE 里**——`stream_handoff` 后走 **WebSocket conduit**（按 `conversation-turn-<turn_id>` 主题 `subscribe`／`stream-item.encoded_item` 内嵌原样 SSE／`heartbeat`／`done`／`unsubscribe`）⇒ L0 直发要拿到答案须 WS 订阅，或找到关闭 `resume_with_websockets` 的开关（未找到，未验证）；定案前 `recipes/chatgpt.json` 不按「SSE 直出答案」写。案卷＝`docs/evidence/pilots/chatgpt-two-state-and-response-framing-2026-10-02.md`。
  **签名器**＝站点自带（sentinel prepare/finalize ＋ 页内 JS 挑战串）；凭据＝`Authorization: Bearer <access token>`（页内 `GET /api/auth/session` 的 `accessToken` 可取）⇒ 走 `request.signer`／页内直发，**令牌与挑战串不落 recipe**。
  列表端点＝`GET /backend-api/conversations?is_archived=false&is_starred=false&limit=30&order=updated&offset=0`（带 Bearer；不带 Bearer 回 `items:[]`）。
  **清账端点＝已坐实（2026-10-02 16:5x，订正「仍未知」）**：`DELETE https://chatgpt.com/backend-api/conversation/id/<uuid36>`（**无 body**，注意多一段 `id/`；同资源的变更路径不同形——置顶＝`PATCH /backend-api/conversation/<uuid36>` body `{"is_starred":true}`）。复核＝`GET /backend-api/conversations?...` 列表不含该 id ＋ `GET /backend-api/conversation/<uuid36>` → **404 `conversation_deleted`**。**抓取手法**：该请求页面级 `Network` 抓不到，须用 `Fetch.enable`（`requestStage:"Request"`）⇒ 本站嗅探/清账取证走 Fetch 域。
  **模板 `recipes/chatgpt.json` 已按实测重写（2026-10-02，仍 `DRAFT`）**：单请求 `POST /backend-api/f/conversation`；两态＝首轮**整键不落 `conversation_id`**（新增模板节点 `{"omit_if_empty":…}`）＋ `parent_message_id:"client-created-root"`，续轮带 `conversation_id` ＋ `parent_message_id`＝会话对象里**最后一条助手消息 id**（`fetch_field.select=last_assistant_message_id`；**不取 `prepare` 带回的 id**）。鉴权＝`Authorization: Bearer {{access_token}}`（`fetch_field` 读 `/api/auth/session` 的 `accessToken`）＋ `oai-device-id`（cookie）＋哨兵两步 `chat-requirements/prepare`（可空体）→ `finalize`（两个 `fetch_field` 槽位串接，凭据不落 recipe 文本）。**正文口径＝`response.kind:"sse_handoff_poll"`**：主流只当前奏（止于 `stream_handoff`），正文改由 `response.poll` 轮询会话对象取（`select=last_assistant_text`，且必须 ≠ 本轮 `parent_message_id`，防把上一轮答案当结果）；页内以单条 JSON 行 `{"__uws_poll_answer":…}` 回传，`chat_direct` 与 `recipe.Verifier` 各自识别。本轮新增 DSL：`now_epoch`（数字型 `create_time`）、`fetch_field.select`、`{"omit_if_empty":…}`、`response.poll`。**仍未做**：第 9 条真机自证回放（未发车）、`/f/conversation/prepare` 与 `/conversation/init` 两步是否必需、`browser_context.instance_id` 现用每次新铸 UUID（站点实际是页内稳定 UUID，未验服务端是否校验）。`DRAFT` 期间 fail-closed 走 DOM。
  **第 9 条真机自证已跑（2026-10-02 21:0x，结论＝直发被 Cloudflare JSD 哨兵挡）**：网关换新构建（由 `git archive HEAD` 干净导出构建，避开并线未提交 WIP）重启后，对页留 `chatgpt.com/` 的真实会话跑 `POST /v1/diag/recipe/verify` ⇒ **HTTP 403 `{"detail":"Unusual activity has been detected from your device…"}`**（`recent_drifts[0].reason=start_unhealthy`）。根因已定位：`prepare` 已不是「空体即可」形态，实测回 `{persona, prepare_token, turnstile:{required,dx}, proofofwork:{required,seed,difficulty}, so:{required,collector_dx}}`；站点 `finalize` body 实测带 **8KB 的 `proofofwork` ＋ `turnstile`**（`turnstile`/`collector_dx` 是 Cloudflare JSD 的混淆挑战串），二者由页内 `window.cloudflare.jsd.executeOnce({callback,nonce})` 注入的 iframe（`/cdn-cgi/challenge-platform/scripts/jsd/api.js` ＋ `jsd/main.js`）跑挑战后产出、并被 Cloudflare 侧密钥加密 ⇒ **离线不可铸**；只带 `prepare_token` 的 finalize 虽仍 200，但 token 无效。⇒ ChatGPT **不走页内直发**，改走 DOM 通路（由站点自己铸 token）。`recipes/chatgpt.json` 保留 `DRAFT` 作记录，除非将来做「页内 JSD 集成」，否则它不是 ChatGPT 的接线方向。
  **DOM 通路已接线（2026-10-02，提交 `29a849d2`）**：`pkg/engine/knowledge.go` 新增 `chatgpt_input`（`.ProseMirror[contenteditable='true']`，实测 aria-label「询问 ChatGPT」；旧 `#prompt-textarea` 只作兜底）／`chatgpt_send`（`button[aria-label='发送']`，**输入非空后才出现**——空输入时页面上没有该按钮，故必须先 input 再 click）／`chatgpt_answer`（`[data-message-author-role='assistant']` ＋ `.markdown` 兜底）／`chatgpt_new_chat`（`button[aria-label='新聊天']`）；`apps/gateway/provider.go` 把 ChatGPT 从通用 `chat_input/send_button/chat_answer` 拆出专用映射（通用那套选择器是 DeepSeek 的 `textarea`，匹配不到 ProseMirror）。单测 `TestChatGPTProviderSemantics` ＋ `go build ./...` ＋ `go test ./pkg/engine/... ./apps/gateway/... -count=1` 全绿；网关（launchd `com.guileen.uws-gateway`）与引擎均已换到干净构建 `v1.5.5+29a849d2`，`GET /v1/health` 的 `channels` 已含 `chatgpt-web`。
  **真机 DOM 闭环未通（环境性，非选择器）**：引擎新开的 ChatGPT 会话页渲染进程不响应 CDP（引擎日志 `uws action extract semantic="chatgpt_answer" … failed: eval_error:cdp: Runtime.evaluate: context deadline exceeded`），登录检查 fail-closed 拦在发送前（网关回 `authentication_check_failed`；`POST /v1/auth/reset {"channel_id":"chatgpt-web"}` 清锁后同样复现）；同机既有用户页 `680E6E…` 对同样 CDP 调用正常响应 ⇒ 归因「引擎共享窗口里那个后台 Tab 被冻结/挂起」。**本轮未向站点发出任何消息，额度未动**（403 不落会话、DOM 两次都停在发送前）。下一步第一动作＝让该 Tab 处于可渲染/前台（或按引擎既有做法避免后台冻结）后重跑一次 DOM 发送，通过再按 §14 清账。
  实测页面读数（只读，2026-10-02）：模型选择器 aria-label＝「选择 ChatGPT 模型」，菜单项 `最新`(选中)／`GPT-5.6 Sol`／`GPT-5.5`（站点标注 2026-10-14 下线），组头显示 `6 Pro`；**另有「思考强度」滑杆**（`role=slider`，min 0／max 4 ＝ 5 档，当前 index 4 标签 `Pro`、`data-maximum=true`，伴 `UltraUsageWarningText`「更快消耗使用额度」）——**模型名与思考强度是两个独立参数**（默认「最新」＝GPT-6；搜索**默认开、不可关**），网关当前只建模了前者，强度参数尚未落地（缺口）；**默认取中/高档**，不同模型的档位数可能不同，档位名未逐一实测。侧栏额度读数＝剩余 2%、每周重置、下次 10 月 4 日 01:16。`GPT-5.6 Sol`／`GPT-5.5` 的**站点请求体 model 字符串未实测**，不得臆造。案卷见 `Tasks.md` `T-25` 接站队列条。

## 14. 站点会话清账（删除 + 待删除列表；ACTIVE，2026-09-26 用户定案）

- **动机**：讨论会在站点服务器留下大量会话——用户口径「影响很不好」，要求把「会话删除」接进来；两种兑现方式都接受：**讨论结束立即删掉刚产生的会话**，或**入「待删除列表」由后台异步删**。
- **边界（硬）**：只删**调用方显式给出**（或由该会话标签页实测读出）的会话 ID；**不提供**「删除全部历史 / 清空用户真实历史」的能力；不碰用户自己的窗口与标签页。
- **站点链路（Gemini，实测）**：删除＝`batchexecute` RPC **`GzXR5e`**，`f.req=[[["GzXR5e","[\"c_<16hex>\"]",null,"generic"]]]`，body 另带 `at=WIZ_global_data.SNlM0e`，URL 槽位 `bl=WIZ_global_data.cfb2h`、`f.sid=WIZ_global_data.FdrFJe`、`_reqid=页内递增计数器`，头 `X-Same-Domain: 1`——属 §13 recipe 家族的一个**动作**（同一套页面态槽位）。删除后站点会自行重列侧栏（`MaZiqc`），故「侧栏不再列出」是可靠信号。
- **站点链路（千问，2026-10-02 接入，实删已验）**：删除＝页内 `fetch` **`POST https://chat2-api.qianwen.com/api/v1/session/delete/batch`**（body `{"session_ids":["<32hex>"]}`；签名走 `window.__QIANWEN_CHAT_SDK__.qwenSign`，同 `session/get` 一族）；**成功判据**＝HTTP 2xx ＋ `data.delete_success:true` ＋ 复核「`session/get.status != 1` 且 `v2/session/page/list` 不再列出」（§15 案卷实测：2xx／`success_ids` 回执／列表消失三步齐）。通道＝`qianwen-web`（§13 SOP 第 1 条登记）；未指定 `channel_id` 时按会话 ID 形状消歧（`c_<16hex>`⇒gemini、`<32hex>`⇒qianwen，两种形状不重叠），**形状也消歧不了即拒绝猜通道**（返回「无法确定通道」）。
- **会话 ID 归一**：接受 `/app/<16hex>`、`<16hex>`、`c_<16hex>`（大小写不敏感），内部一律 `c_<16hex>`；**无服务端 ID 的新对话拒绝猜 ID**（读 `location.pathname`，拿不到就报错）。
- **成功判据（fail-closed）**：只有 **HTTP 2xx 且删除后页面侧栏不再列出该会话** 才判 `deleted`；「2xx 但仍在列表」「非 2xx」「脚本报错/槽位缺失」一律算**未成功**——不谎报删除成功。
- **待删除列表语义**：落盘 `~/.uws/gateway-purge-queue.json`（`UWS_PURGE_QUEUE` 覆盖路径，`-` 关闭该能力），原子写（临时文件 + rename）；状态 `pending`（未试）与 `failed`（试过未成功）**都算待删除**，只有 `deleted` 销账（失败会留痕 `attempts`/`last_error` 并继续重试，避免静默漏删）；后台 worker 默认 10 分钟一轮（`UWS_PURGE_INTERVAL` 覆盖，`<=0` 关闭），仅在有待删除项时动作。
- **执行器来源**：删除是**页内 fetch**（复用页面 Cookie / `at`），因此需要活标签页：关会话时**先删后关**（趁标签页还在）；后台清账优先复用同通道任一存活会话，没有就**新建临时标签页**并在收尾关闭，且不劫持「当前会话」语义。
- **证据**：[`evidence/pilots/session-purge-2026-09-26.md`](evidence/pilots/session-purge-2026-09-26.md)（真实 RPC 原文、一次真实删除的前后核验、失败判据的由来）。
- **未验证选项**：Gemini 页面存在 `button[aria-label="Temporary chat"]`（同页文本 `Turn on temporary chat`）——能否作为自动化默认（是否随标签页保持 / 是否影响多轮上下文 / 与直发路径是否兼容）**未验证**，登记 `T-27`，不擅自改默认行为。
- **默认收口（2026-09-30 起；2026-10-03 派单 d27954eeb 收紧默认）**：`DELETE /v1/sessions/{id}`
  默认**只解绑**，清账须显式 `?purge=1`；且会话空闲超过 `UWS_SESSION_IDLE_TTL` 会被自动收口
  —— 生命周期（什么时候关、关前删什么、目标怎么定、站点不支持删时怎么办）见 §9「会话生命周期与收口」。

- **调试保留：`UWS_KEEP_SESSIONS`／`?keep=1`（ACTIVE，2026-10-03 派单 d8657acc9；用户 2026-10-03 02:40 指令）**：
  置位后**任何关会话清账都不删站点对话**，历史留在站点侧供事后 review（痛点＝自动清账跑完对话就没了，没法 review）。
  - **载体（一处定义、两端可用）**：进程级环境变量 **`UWS_KEEP_SESSIONS=1`**（只有 `1`/`true` 算开）；
    请求级 **`?keep=1|0`** 或头 **`x-uws-keep-sessions: 1|0`** **显式优先**且可覆盖进程级（进程没开也能单请求保留，开着也能单次显式删）。
  - **覆盖路径**：① `session_closed` 自动清账（空闲回收 `reapIdleSessions`、一次性会话 `finishOneShot`）；② 显式 `DELETE /v1/sessions/{id}?purge=1` ——
    两条都只回 `{"status":"skipped","reason":"debug_keep","conversation_id":"…"}`，并逐字记 **`调试保留 · 未清账 sid=… conv=…`**。
  - **不覆盖**：`POST /v1/purge {"conversation_id":…}`（显式指定 ID 的删除）**不受该标志约束** ——
    它是「明确知道要删哪条」的算子通道，也是残留清理的唯一出路；标志只挡「关会话顺手删」。
  - **标志开时仍然会变的**（review 时该看什么）：网关会话照样解绑（`/v1/sessions` 少一条）、页签照样关（`tab_closed`）、引擎落盘绑定照样清。
    **唯一不变的是站点侧对话仍在**；review 入口＝站点自己的侧栏，或另开一个该站点会话后 `GET /v1/sessions/{id}/conversations`（`S3`）。
  - **默认零变化**：未设标志 ⇒ 行为与 `0794aa0c` 逐字一致（绑定核对 fail-closed、`purge` 默认 false、`session_closed` 口径均不动）。
  - **真机成对读数（2026-10-03 03:3x–04:0x）**：标志关 ⇒ `purge.status=deleted verified=True domListed=False` ＋ 侧栏不再列出该对话；
    标志开 ⇒ `purge.status=skipped reason=debug_keep` ＋ 日志 `调试保留 · 未清账 sid=gf-keepon-02 conv=c_c9f3e3f0c1ea7a07（UWS_KEEP_SESSIONS 开启，跳过站点对话删除，历史留站点侧供 review）`
    ＋ 侧栏仍列出该对话（标题「简洁确认」，**保留供 review，未清**）。**调用侧无需改动**：`labs/world-agent/agent-hub/` 全库无清账动作
    （`grep -rn "purge|/v1/sessions|close_after"` 命中 0，只有 chat 调用），故该标志由网关单点覆盖整条链路。

- **接力后的旧会话清账（ACTIVE，2026-10-02 用户定案）**：一段站点对话被「总结 → 接力到新的站点对话」、
  且**新对话已成功开轮**之后，旧对话不再续聊 ⇒ **旧对话必须清掉**（用户口径：聊天不停、历史该清的清掉）。
  两条硬约束：① **先确认新对话可用再删旧** —— 新对话没有成功轮次就删旧 = 接力中断、上下文无处可查；
  ② **删不掉不许静默丢弃** —— 立即删失败一律入待删列表（`pending`／`failed` 都算待删除、后台每轮重试），
  保证「这次没清成功，后面还能回头再清」（用户 2026-10-02 原话）。
- **窗口资源同步回收（ACTIVE，2026-10-02 用户定案）**：接力／收口的同时回收旧页签（§9 会话生命周期），
  **只关自己开的**；用户自有窗口与标签页一律不动（口径：「该清理的也得清理，该保留的得保留」）。

## 15. 浏览器宿主自愈（关窗 ≠ 断链路；ACTIVE，2026-10-01 用户定案）

- **动机（用户 2026-10-01）**：只关掉调试 Chrome 窗口，网关 8000 仍 200、但 9222 没了 ⇒ 网页通道失去宿主 → 退回游客模式、模型被锁 ⇒ 推理 5xx。**进程活着 ≠ 通道可用**。
- **纳管**：`--attach-cdp` 非空即纳管该 CDP 端点（默认探测 `http://127.0.0.1:9222`；`--attach-cdp=none` = 引擎自建隔离 Chrome，不纳管）。网关持有宿主守护循环，探活周期 `UWS_BROWSER_HOST_INTERVAL`（秒，默认 15）。
- **自愈**：探活失败 ⇒ 用**固定 user-data-dir**（`--chrome-profile`，默认 `~/.uws-chrome-debug`）拉起持久调试 Chrome 并等待就绪——登录态在 profile 里，一次登录长期有效，不要求用户重登。连续拉起有冷却（5s）；**未授权（`UWS_ALLOW_CHROME_LAUNCH` 不是 1/yes/true/y）时 fail-closed，绝不启动 Chrome**，只记录失联。
- **可用性定义**：通道可用 = **进程 + 宿主 + 一次真实推理**。`GET /v1/health` 追加 `browser`（`managed`/`attached`/`relaunches`/`down_since`/`last_error`/`launch_error`）、`host_ok`、`engine_ok`、`ready`、`last_inference`、`last_inference_ms`；`GET /v1/health?deep=1`（可选 `&model=`）**真跑一次最小推理**并把结果写入就绪读数（有效期 `UWS_READY_TTL`，秒，默认 600）。
- **代码落点**：`apps/gateway/browser_host.go`（守护循环）、`apps/gateway/readiness.go`（端到端就绪）、`apps/gateway/handler.go`（`/v1/health` 暴露）、`cmd/uws-gateway/main.go`（接线）。
- **边界**：本条款只保证「宿主在、链路通」；页面退回游客模式（profile 未登录）属登录态问题，不在自愈范围。另：`8045` 被非预期进程（Antigravity Tools）占用，**只登记不改**。

## 16. 待定

- Anthropic `/v1/messages` 适配（共用路由核）；
- 外部通道密钥本地加密存储；
- 遥测 / 日志（PII 脱敏复用引擎 logstore 规则）。

## 17. 文本直通接口（POST /v1/text；已迁移到 Agent Hub）

- **动机（用户 2026-10-01）**：「把 :8000 的 Gemini 通道封成文本进 → 文本出的本地接口」——让脚本 / 定时任务把**免费网页版 Gemini** 当模型用（心跳那笔付费唤醒可改走它），调用方**不必知道通道名与模型名**。
- **迁移目标入口**：`POST http://127.0.0.1:18080/v1/text`，由 Agent Hub 既有 HTTP 服务提供；WebTouch `:8000/v1/text` 已退役并返回 `410 gateway_moved`。Agent Hub 保留本条请求/响应形状。入参两种，等价：
  - `Content-Type: text/plain` ⇒ 请求体即是那段文本；
  - JSON `{"text":"...","model":"...","channel":"...","session_id":"...","new_session":true,"close_after":true,"timeout_ms":120000}`（`text` 必填，其余可选）。
- **默认路由（固定，不必猜）**：`gemini-web` + `gemini-flash`；显式给 `model`（如 `gemini-3.1-pro`）或 `channel` 才改道。
- **出参**：`{"text":"<回答>","model":"...","channel":"...","session_id":"...","elapsed_ms":1234,"id":"..."}`——`text` 就是那段文本。
- **错误码（固定，字段＝`error.semantic`）**：
  | code | HTTP | 含义 |
  | --- | --- | --- |
  | `browser_host_down` | 503 | 调试 Chrome 宿主不可达（§15）；**快速失败**，不等整轮超时 |
  | `channel_timeout` | 504 | 超时（默认 `UWS_TEXT_TIMEOUT`＝120s，可被 `timeout_ms` 覆盖） |
  | `queue_full` | 429 | 在飞并发已满且排队超时（默认 `UWS_TEXT_MAX_INFLIGHT`＝2、`UWS_TEXT_QUEUE_WAIT`＝90s）；附 `Retry-After: 10` |
  | `login_required` / `human_verification_required` | 401 | 页面退回游客模式或被风控挑战，需用户在该 Profile 手动登录 |
  | `authentication_check_failed` | 503 | 登录态未知（保护账号，拒绝提交） |
  | `empty_answer` | 502 | 通道返回空回答（页面异常/风控） |
  | 引擎语义（如 `provider_5xx`） | 502 | 引擎侧失败，原样透出 `error.semantic` |
- **回退到付费通道的开关**：网关侧只**报错不代跑**（不做跨通道自动降级，避免悄悄花钱/换身份）；调用方按上表判 `error.semantic`／`GET /v1/health` 的 `ready` 决定回退。本机现有开关位置：`~/.cc-connect/config.toml` 的 `[[projects]]` → `p.name="agenthub"` → `[projects.agent.options] model = "gemini-flash"`（＝走 8000 免费网页通道），改成付费 provider 的模型即回退。
- **代码落点**：Agent Hub `internal/gateway/model_api.go`（入口）及同包文本适配器；WebTouch 不再提供客户端文本模型路由，只保留 Hub 专用 `/internal/provider/chat` 执行契约。
- **宿主检查归属**：浏览器宿主不可达的快速失败由 WebTouch 私有 chat/responses 执行入口统一完成；快照不可达时先做一次短超时 CDP 复核，失败则返回 `503 browser_host_down`、`retryable=false`，不调用引擎。文本入口的解析和排队仅归 Hub。
- **边界**：本条款只做「文本进 → 文本出」；不做流式、不做多轮上下文管理（多轮请用 `/v1/chat/completions` 或复用 `session_id`）。`text` 是通道原文**逐字**透出，网关不做清洗——实测 2026-10-01 回答文本前会带一行 `[State: X]` 标记（引擎侧产物，已登记为缺陷，另行修）。
- **实测（2026-10-01 18:47–19:00，免费网页通道）**：连续 4 次真实调用单轮耗时 18.6s / 28s / 35s / 38.0s（中位 ~32s）⇒ 单会话顺序吞吐约 **2 次/分钟**；结构上限＝拟人节流 8 轮/分钟/会话（`pacer.go`，超出排队）、文本闸门在飞 2、排队 90s、单次超时 120s。
