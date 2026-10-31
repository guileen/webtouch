# WebTouch CLI 工具与配置指南 (CLI & Configuration)

本指南面向最终用户、AI Agent（如 Codex / Claude Code）以及数据爬虫工程师，说明如何以**最少配置、傻瓜化**的方式调用 WebTouch 引擎，而无需关心底层的 gRPC 通信细节。

## 1. 软件形态与运行方式

**WebTouch 不是浏览器插件，也不是魔改版的自有浏览器。**
它是一个独立的跨平台可执行二进制 CLI 工具 (`uws`)。它在底层通过 CDP (Chrome DevTools Protocol) 协议接管你系统中已经安装的普通 Chrome / Chromium 浏览器。

- **对于 AI Agent**：通过执行简单的 `uws run` 命令行，Agent 可以轻松驱动浏览器。
- **对于 Linux 爬虫**：在无界面的 Linux 服务器上，CLI 会自动拉起 Headless Chromium。
- **无需写 gRPC 代码**：gRPC 仅供内部解耦和分布式微服务架构使用。日常的单机或 Agent 操作全部由 CLI 提供傻瓜化封装。

### 1.1 引擎守护进程（uws daemon，取代 launchd）

引擎（`cmd/uws-engine`，监听 gRPC 50051 的在线执行环）由 `uws` CLI 直接托管，不再依赖 launchd。
`uws run/crawl/api` 在执行前会先 `ensureEngine`——若引擎未监听则按需拉起，跑完自动闲置（不随 CLI
退出被杀，也不常驻开机自启）。

```bash
uws daemon start      # 拉起引擎守护进程（默认 127.0.0.1:50051，真实 Chrome 模式）
uws daemon status     # 查看运行状态（pid / 地址 / 启动时间 / 是否监听）
uws daemon restart    # 重启（先停后起）
uws daemon stop       # 停止
```

**托管地址与状态目录**：

- 地址：默认 `127.0.0.1:50051`，可用环境变量 `UWS_ENGINE_ADDR` 或 `daemon` 子命令的
  `--engine-addr`/`--grpc-addr` 覆盖。子命令（run/crawl/api）的 `--engine-addr` 若指向
  非托管地址且未监听，CLI 会提示先 `uws daemon start` 托管，**不会**为陌生地址静默拉起第二个引擎。
- 状态文件：pid/元数据/日志落在 `~/.uws/engine.pid`、`~/.uws/engine.json`、`~/.uws/engine.log`
  （`UWS_ENGINE_DIR` 可覆盖目录，便于测试隔离）。
- 引擎二进制：默认取 `uws` 同目录的 `bin/uws-engine`，其次 `PATH`；`UWS_ENGINE_BIN` 显式指定最稳。
- 启动参数：默认 `--browser cdp --log-path ~/.uws/engine-onlinelogs.db --vlm-learn-path ~/.uws/learned.json`
  （真实 Chrome + 在线日志落库 + **VLM 自进化默认开**：学到的「页面 + 语义 → 定位」下次免 VLM；
  落点自证与写入门禁见 spec §5.0.9 条款 6，失效条目会在复用判 miss 时自动清掉）；
  追加/覆盖用 `UWS_ENGINE_ARGS`；关闭自进化用 `UWS_LEARN=off`（`UWS_LEARN=<路径>` 可换存储位置）。

> 说明：引擎以后台（新会话）进程方式启动，`uws` CLI 退出后仍保活；`uws daemon stop/restart` 由 CLI
> 通过 pid 直接控制。VLM 兜底默认开启（`--vlm-fallback` 默认 `true`），Key 自动读
> `UWS_VLM_API_KEY || DEEPSEEK_API_KEY`。

### 1.2 对外 session（CLI lease）

对外能力走 CLI，不走常驻 HTTP。session 生命周期在本机 lease，命令只携带句柄：

```bash
webtouch session create --channel gemini-web
webtouch session use --session wts_...          # 或 export WEBTOUCH_SESSION=wts_...
webtouch session close --session wts_...
```

`uws` 与 `webtouch` 等价。stdout 是 JSON（`webtouch.cli.session.v1`）。Hub 只记账其中的 `hub.channel` 与 `hub.site_conversation_id`。同一账号必须已经 `identity bind`，本命令不新建指纹。契约见 [`cli-session-contract-2026-10-31.md`](cli-session-contract-2026-10-31.md)。

---

## 2. 全局配置 (Zero-config 理念)

WebTouch 追求“开箱即用”，默认不需要任何配置即可使用预装的离线规则库。但由于遇到复杂陌生网页时需要 LLM 进行兜底推理，**必须配置一次兜底模型 API**。

### 2.1 傻瓜化配置兜底大模型
支持任何兼容 OpenAI 接口的高性价比模型（推荐 DeepSeek-Chat）：

```bash
# 设置兜底模型 API (全局生效，自动保存在 ~/.uws/config.yaml)
uws config set fallback_model deepseek-chat
uws config set fallback_api_base https://api.deepseek.com/v1
uws config set fallback_api_key "sk-xxxxxx"
```

*如果你使用的是本地大模型（如 Ollama），可以直接指向本地：*
```bash
uws config set fallback_api_base http://localhost:11434/v1
```

### 2.2 配置打码 / VLM 视觉求解服务（WAF 对抗）

遇到 `CHALLENGE_INTERCEPTED`（Cloudflare / WAF / 滑块验证）时，协调器会挂起当前
执行流、抓取页面截图并交给 VLM / 打码服务计算轨迹坐标，随后原生拖拽解锁并无缝
继续原任务。默认**未配置**时保持安全降级（任务置 PAUSED，不盲试、不封号）。

```bash
# 指向任意兼容协议的 HTTP 打码/VLM 服务
uws config set vlm_provider http
uws config set vlm_endpoint "https://solver.example.com/v1/solve"
uws config set vlm_api_key "sk-xxxxxx"     # 敏感值，输出自动隐藏
uws config set vlm_timeout 10              # 单次求解超时（秒）
uws config set vlm_max_solves 3            # 单任务求解封顶（防无限重试）
```

请求协议：`POST {"image": "<base64 PNG>", "challenge_hint": "<目标语义>"}`，
配置了 api_key 时携带 `Authorization: Bearer <key>`；响应
`{"x1":..., "y1":..., "x2":..., "y2":..., "action":"drag"}`。
本地演示/测试可用 `uws config set vlm_provider mock`（固定坐标确定性求解）。

---

## 3. 常见操作场景 (CLI 实战)

### 3.1 场景一：Agent 接管本地可见浏览器
当 Agent（如 Claude Code 或本地运行的脚本）想在你的电脑上帮你操作网页时，可以通过 `--attach` 参数接管当前正在运行的 Chrome。
它**不会**破坏你当前的标签页，而是新建一个标签页，但共享你当前的登录态和指纹。因为是真实的物理浏览器，极大地降低了被反爬虫识别的概率。

`--attach` 支持三种取值（run / explore / api 通用）：

- `debug`（默认）：附着 `uws-chrome-debug` 持久调试 Chrome（profile `~/.uws-chrome-debug`，
  端口 `9222`）；端点未就绪时自动拉起。登录态与日常浏览器物理隔离，适合自动化/反爬实验。
- `default`：附着**用户日常 Chrome**（默认 profile，端口 `9223`）。若已在调试端口监听则直接
  复用；若 Chrome 正在运行但无调试端口，会先优雅退出再带 `--remote-debugging-port` 重启
  （窗口/标签自动恢复、登录态保留）。该重启动作在交互终端会二次确认，非交互环境需显式
  `UWS_ATTACH_CONFIRM=1` 才放行；也可先手动关闭 Chrome 再运行。
- 显式 CDP URL：如 `--attach http://127.0.0.1:9222`，原样使用。

```bash
# Agent 下发的终端指令：自动打开淘宝并搜索显卡，返回商品列表
# 推荐：一句自然语言目标（chat 目标模式，Agent/VLM 自决）；结果完整打印 stdout
uws "在淘宝搜索 RTX 4090，提取第一页商品名称和价格列表"

# 确定性已知路径用 DSL（结果同样直接打印 stdout；--output 仅在需要留档时另存）
uws run --attach default --target-url "https://www.taobao.com" \
  --plan "search:RTX 4090\nextract:第一页商品列表 | $列表\nreply:$列表"

# 只读观察之前建立的专属会话：只用已就绪 CDP 回传 URL / AXTree，不启动/重启 Chrome；缺失/过期 session 会拒绝
uws run --observe --session codex-taobao-check --screenshot
```

> 注意：Chrome 未以调试端口启动时，日常浏览器需要一次「退出并重启」才能被 CDP 接管，
> 这是 Chrome 的安全机制（不允许对运行中的实例动态开启调试端口），并非 WebTouch 的限制。
> 若日常 Chrome 的登录态在非默认 profile（如「Profile 1」），可用
> `UWS_CHROME_PROFILE=<profile 目录>` 指定后再 `--attach default`。

### 3.2 场景二：智能爬虫与离线 Common Crawl (Markdown + Frontmatter 格式)
在处理爬虫数据时，最受大模型欢迎的格式不是乱糟糟的 JSON，而是类似 `Crawl4AI` 的 **Markdown 正文 + YAML Frontmatter 头部结构**。

WebTouch 支持**在线智能爬取**：它不关心底层 HTML，而是根据语义信号（如
`articles`, `no ads`）提取干净的 Markdown。你可以通过 `--depth` 自动深入
下一级页面，无需外部代码干预。

**模式 A：在线智能爬虫 (指定深度自动下钻)**
```bash
# 从入口 URL 开始，提取文章正文、作者头像，自动翻页/点击详情深入 2 层
cat urls.txt | uws crawl \
  --concurrency 8 \
  --depth 2 \
  --signals "main_article, author_name, no_ads" \
  --format md \
  --output-dir ./corpus_cleaned/
```

**模式 B：CC 语料工具链（已移交 search-engine）**

`cc-fetch` / `parse-warc` / `search` 三条 CC 语料工具命令已随代码整体移交
`labs/world-replay/search-engine/`（`se` 包，2026-08-20），uws 不再包含
搜索引擎 demo 层。使用方式与完整闭环见
`labs/world-replay/search-engine/docs/cli-and-config.md`：

```bash
cd ../search-engine
python -m se cc-fetch --limit 8 --concurrency 4 --output-dir ./cc_wet/
python -m se parse-warc ./cc_wet/*.wet.gz --output-dir ./cc_md/
python -m se search --corpus ./cc_md/ --query "Common Crawl"
python -m se search --corpus ./cc_md/ --serve --port 8001
```

### 3.3 场景三：Web-to-API（与大模型网页对话）
启动 WebTouch 浏览器 provider 执行服务；客户端模型 API 由 Agent Hub 提供。命令已精简为 `uws api`。

```bash
# 启动本地浏览器执行服务（内部接口只供 Agent Hub 调用）
./webtouch api --listen 127.0.0.1:8000
```
*注：网站账号、Vault 和账号对应的浏览器 Profile/指纹由 WebTouch 管理；Hub 经内部 provider 契约调用网页模型，同一账号持续复用既有身份。*

客户端使用 Agent Hub 本机模型入口（默认 `127.0.0.1:18080`）：

```bash
# WebTouch 浏览器执行服务健康检查
curl http://127.0.0.1:8000/v1/health

# 通过 Agent Hub 调用 DeepSeek 网页模型
curl -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"provider":"webtouch","channel_id":"deepseek-web","model":"deepseek-chat","messages":[{"role":"user","content":"你好，介绍一下你自己"}]}'
```

每次请求的网页编排为：`input(chat_input)` → `click(send_button)` →
`extract(chat_answer)`；回答提取轮询直到「出现不同于发送前基线的新回答且连续
稳定 1.5s」才返回（多轮不会把上一轮的旧回答当新回答返回，2026-08-25 实测修复）。

### 3.4 场景四：义眼评审（uws chat，直接与视觉模型对话）

复刻/站点评审这类需求，不需要一整套测量/规格对比原语，只需一条：把图（或让 uws 开站点
截图）+ 一句问题交给视觉模型，让它在「有一定认知的体验评审者」视角下给出反馈——是否一致、
是否满足需求、按钮位置、手势是否成功、视觉/布局/文案差异。

```bash
./uws chat "判断两个实现是否一致" --img ref.png --img clone.png      # 图对图（默认结构化 JSON）
./uws chat "这是哪种布局？" --img shot.png --plain                   # 单图评审，出自然语言
./uws chat "对比这两个站点第一屏" --url https://a.example --url https://b.example
./uws chat "帮我检查 a.com 和 b.com 的 differences"                   # 纯自然语言：自动提取站点+像素 diff
./uws -p "两个实现是否一致" --img a.png --img b.png                  # -p 是 chat 顶层别名
```

输出（默认，给 Agent 程序化消费）：

```json
{
  "verdict": "different",
  "summary": "按钮位置不同",
  "checks": [{"name": "按钮位置", "status": "fail", "note": "X 方向偏移"}],
  "confidence": 0.85
}
```

**与义肢的区别**：义肢（`Resolve`/`run`/`commander`）把 VLM 当执行手，产候选坐标/步骤并让
引擎做确定性动作；义眼（`chat`）不产可执行动作、不规划多步、不自主执行，只做好「看一看 +
给反馈」。图片源：`--img`（本地读 bytes / URL 透传）、`--url`（经引擎 InitializeContext+
CaptureScreenshot 采样，每站点独立 context 防 Cookie 串）、`--page`（截当前会话）。API Key
读 `UWS_VLM_API_KEY` || `DEEPSEEK_API_KEY`（不落日志/文档）。

**证据原语（让 VLM 调用 WebTouch 能力，而非裸眼看图猜）**：

```bash
# 像素级 diff：两张图逐像素对比，差异图(洋红=差异)+统计注入问题，VLM 给量化判据
./uws chat "差异点在哪？给量化判据" --img ref.png --img clone.png --diff

# 区域放大（放大镜）：细看第一张图的指定区域
./uws chat "按钮的圆角/阴影细节" --img shot.png --zoom 120,80,200,120,4

# 区域取色（比色卡）：第一张图指定区域的平均色
./uws chat "导航栏主色是什么" --img shot.png --region-color 0,0,390,48
```

`--diff` 生成的是**确定性证据**（差异像素占比/bbox/最大信道差），VLM 只负责解释该证据，不
背对像素猜；`--plain` 走 `Ask(JSON=false)` 并强约束「不要输出 JSON/围栏」；`--url` 若两站
截图像素几乎相同（sim 引擎不真实导航），自动注入警示让模型回 `unknown` 而不误判一致。
单步超时由 `--chat-timeout-sec` 控制（默认 60s）；`DEADLINE_EXCEEDED` 按协调器
Fibonacci 1s/2s 最多重试 2 次。页面会话首次请求初始化并常驻复用（不因每次对话
重建浏览器上下文）。生产接入真实 LLM 网页时，按需在
`chat_input`/`send_button`/`chat_answer` 语义上补充站点规则即可。

**自然语言自决（2026-08-28 新增）**：当调用方不显式传 `--url`/`--diff`（也不传 `--img`/
`--page`），但问题本身自带 ≥2 个站点 URL 且意图是「比较/查差异一致性」时，`chat` 会自动：
①`extractSiteURLs` 抽取问题里的 URL（支持 `https://…` 完整地址与裸域名，裸域名自动补
`https://`）；②按比较意图把它们当 `--url` 站点采样；③集齐 ≥2 张有字节数据图后自动开
`--diff` 拿像素证据。这样用户直接问「帮我检查 a.com 和 b.com 的 differences」即可得到
`{verdict,summary,checks,confidence}` 的量化判据，无需知道底层标志。门控条件（≥2 URL +
比较意图 + 未显式给视觉输入）刻意保守，避免把「打开某个站点看看」这类单站请求误当对比而
误开引擎。实测：`uws chat "帮我检查 example.com 和 wikipedia.org 的 differences"` →
`verdict=different`（像素差异 96.3%、bbox 覆盖全图、4 条 checks）。

**义眼主控循环（多轮能力，VLM 自决，2026-08-28）**：引擎具备多轮证据能力——`chat` 默认走一个
主控循环：VLM 每轮返回一个决策 JSON（`{"action":"conclude|zoom|region_color|diff",...}`），
`conclude` 就返回 `{verdict,summary,checks,confidence}` 收尾；请求 `zoom`/`region_color`/`diff`
则执行确定性原语、把结果作为新证据追加给 VLM 进入下一轮。系统提示强约束「**优先单轮收尾**：
现有证据够就直接 conclude，确不足才请求一个原语，每轮最多一个，不空转」。所以「要不要多轮」
由**任务性质 + VLM 自主决策**，而常见站点对比因已自动注入站点截图 + diff 证据，**单轮即收尾**
（实测 `verdict=different`、像素差异 96.3%，一轮完成）。轮数上限默认 4（`eyeMaxRounds`），超限安全降级。
**定位**：仍非标准 function-calling——是 OpenAI 兼容 chat + 自定义 JSON 契约；且「能否多轮」
取决于任务是否需要额外证据，不需要就绝不空转。

**多轮对话与新会话**：客户端模型请求统一发往 Agent Hub；Hub 管理连续体，WebTouch 保留站点对话和物理 Tab 状态。网页站点自身持有对话上下文时，同一轮次链路继续传递稳定 `session_id`：

```bash
# 同一 Hub 会话连续对话（保持稳定 session_id）
curl -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"provider":"webtouch","channel_id":"deepseek-web","model":"deepseek-chat","messages":[{"role":"user","content":"第二问"}],"session_id":"my-session"}'

# 新 Hub 会话显式发起新网页对话（仍复用 WebTouch 既有账号/Profile/指纹）
curl -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"provider":"webtouch","channel_id":"deepseek-web","model":"deepseek-chat","messages":[{"role":"user","content":"第一问"}],"session_id":"brand-new","new_session":true}'

# WebTouch 管理端查看/关闭物理浏览器会话（不承载模型 API）
curl http://127.0.0.1:8000/v1/sessions
curl -X DELETE http://127.0.0.1:8000/v1/sessions/my-session
```

说明：`session_id` 是稳定会话标签，**每个 WebTouch 会话固定绑定一个浏览器标签页**（引擎命名上下文）；`new_session:true` 会创建新的站点会话 Tab，但仍使用 WebTouch 中与网站账号绑定的既有 Profile/指纹，
上下文隔离靠标签页物理隔离，不再依赖页面「开启新对话」按钮。不同 `session_id`
的对话可并行（每会话一把锁）；标签被手动关闭后下次发消息会自动重建新 Tab 并
重试，不再报「找不到 Target」。

**流式输出**：Agent Hub `/v1/chat/completions` 支持 OpenAI
兼容 `stream: true`，以 `text/event-stream` 逐块推送 `choices[].delta.content`
增量（首块带 `role=assistant`），收尾发 `finish_reason:"stop"` 与 `data: [DONE]`：

```bash
curl -N -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"provider":"webtouch","channel_id":"deepseek-web","model":"deepseek-chat","messages":[{"role":"user","content":"你好"}],"stream":true}'
# data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}
# data: {"choices":[{"index":0,"delta":{"content":"好"}}]}
# data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
# data: [DONE]
```

文本来源 = 引擎 `extract(chat_answer)` 优先消费 **CDP Network 层捕获的 SSE 干净
Markdown**（`context_hint["prefer_sse"]="1"`），不再是 DOM innerText——联网搜索的
引用角标（`-1-4-7` 类碎片）在源头即被剔除；`reasoning_content` 思考链与正文在
网络层天然分流。

**模型/模式与开关（`model` / `thinking` / `search`）**：站点 Provider 把这三个
请求参数映射为页面上的物理切换动作，站点差异收口在 `coordinator/app/providers/`：

- **DeepSeek（chat.deepseek.com）**：`model` 是三模式单选（`deepseek-fast`=快速模式、
  `deepseek-expert`=专家模式、`deepseek-vision`=识图模式；也可直接传 `快速模式`/
  `专家模式`/`识图模式`）；`thinking` 是深度思考开关，`search` 是智能搜索开关。
  模式只在「新会话」落地页可选，因此**切换模式需新会话**（未知 `session_id` 或
  `new_session:true`，此时自动开新 Tab 并应用新模式）；**请求模式 ≠ 会话已锁定
  模式时 API 会自动开新会话（关旧 Tab + 重建 + 切换），无需客户端关心
  new_session**（2026-08-26 修复：此前会话已存在且未传 new_session 时会静默沿用
  旧模式）；同一会话请求相同模式则继续多轮、不重建。`GET /v1/sessions` 返回
  `models`（每个会话已锁定的模式族名）。
  未收录的 `model`（如旧 `deepseek-chat`）不切换、沿用页面默认快速模式。
  专家模式无智能搜索开关，请求 `search:true` 会显式报错。
- **Gemini（gemini.google.com）**：`model` 是三档模型（`gemini-3.5-flash-lite` /
  `gemini-flash` / `gemini-3.1-pro`），`thinking` 是 Extended thinking 开关
  （可与任一模型组合）。

```bash
# DeepSeek 切到专家模式并开启深度思考（新会话）
curl -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"provider":"webtouch","channel_id":"deepseek-web","model":"deepseek-expert","messages":[{"role":"user","content":"你好"}],
       "thinking":true,"search":false,
       "session_id":"ds-expert-session","new_session":true}'

# 查看 Agent Hub 模型目录（含 WebTouch 网页模型）
curl http://127.0.0.1:18080/v1/models
```

---

### 3.5 场景五：站点探索报告（uws explore，启发式导航图/功能图）

「了解一个网站、生成导航图/功能图」是通用能力：既可用于测试摸底（页面类型、可交互元素、
testid 清单、锁定路径），也可用于竞品/目标站点的功能地图与验收前置。`uws explore` 用
**纯规则启发式**（不调 VLM，省 token）：从种子 URL 出发 BFS 同域链接，每页经 CDP 采集
标题/正文摘要/可交互元素（button/a/input/testid/role），按 URL+testid+正文关键词确定性分类
页面类型，输出 Markdown 报告（Mermaid 导航图 + 页面清单 + 按类型聚合的功能清单），
**每页自动截图**并内嵌进报告。

```bash
# 附着本地 Chrome（共享登录态）探索站点，默认 12 页 / 3 层
uws explore https://drama.tgaigc.com --settle-sec 5 --output site-report.md

# 收紧探索规模 / 改稳定等待
uws explore https://example.com --max-pages 6 --max-depth 2 --settle-sec 3

# 不截图（纯结构报告）
uws explore https://example.com --no-shots
```

输出结构：

- **导航图**：Mermaid `graph LR`，节点 = 已探索页面（路径 + 类型），边 = 同域链接发现；
- **页面清单**：每页截图（PNG）+ URL/深度/类型/判定依据/标题/关键元素（tag+文本+testid）/同域链接；
- **功能清单**：按页面类型聚合去重后的可交互能力（含 testid），可直接当测试用例素材；
- **限制**：仅覆盖同域 URL 可达页面；SPA 内部不可直接以 URL 打开的路由不会被发现；
  游客态缺登录后页面；iframe/Shadow DOM 内元素暂不采集。

完整报告始终直接打印 stdout（`--output` 仅另存副本）。截图存放：`--output report.md`
时存 `report.assets/`（相对引用，随报告移动需整体移动）；stdout 模式存当前目录
`uws-explore-assets/`。
CDP 不可用（attach 端点不可达）时自动降级为「URL 级快照」，不崩溃。

---

## 4. `~/.uws/config.yaml` 文件结构参考

如果你不喜欢敲命令行，也可以直接修改配置文件。这是配置的唯一来源，保持极简：

```yaml
# ~/.uws/config.yaml
core:
  max_targets: 10          # 最大并发标签页数量
  headless: auto           # Linux 下自动 headless，Mac/Win 下自动有头可见

llm:
  fallback_enabled: true
  provider: "openai_compatible"
  api_base: "https://api.deepseek.com/v1"
  api_key: "sk-xxxxxxxx"
  model: "deepseek-chat"

knowledge:
  # WebTouch 离线规则库的存放位置
  local_db_path: "/var/lib/uws/knowledge.db"

vlm:
  # WAF/滑块挑战的 VLM/打码求解（默认未配置=PAUSED 降级）
  provider: "http"          # mock | http
  endpoint: "https://solver.example.com/v1/solve"
  api_key: "sk-xxxxxx"      # 敏感，CLI 输出隐藏
  timeout_sec: 10
  max_solves: 3
```
