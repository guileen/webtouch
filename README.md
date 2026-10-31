# WebTouch

> **终局认知事实源**：对外统一名称 **WebTouch**（CLI 名 `webtouch`，`uws` 为兼容别名）。
> 核心设计与认知纲领唯一事实源见 [`WebTouchKeyDesign.md`](WebTouchKeyDesign.md)；
> 商业级指纹蓝图与 42 项清单见 [`docs/fingerprint-browser-engineering-blueprint-and-checklist.md`](docs/fingerprint-browser-engineering-blueprint-and-checklist.md)；
> **Virtual VLM 平台角色（2026-09-17 定案）**：在 Virtual VLM 云端智能体 SaaS 中，WebTouch 定位为**现实世界操作与浏览器证据能力 provider**（由 Agent Hub 注册与调度）；WebTouch 探索并验证的 Gateway Profile 架构是 Virtual VLM Gateway 的协议设计资产。现有本地引擎、CLI 与独立 QA Studio 路线保持独立运行。

**一套面向 AI Agent 的浏览器执行与站点身份底座、个人桌面自动化能力，以及 Vibe Coding 独立交付验收层。**

WebTouch 的三大核心使命：
1. **提供浏览器模型执行能力，夺回桌面自动化主权**：为 Agent Hub 提供**网页模型 provider 与浏览器交互总线**，通过双轨传输引擎（In-Page Direct 加速 + 健壮 DOM 自愈兜底）实现可靠交互，配合网站账号、Vault 与稳定的账号/Profile/指纹绑定守护个人身份；客户端模型 API、provider 路由与 CLI/Web 连续体由 Agent Hub 负责；
2. **免封杀、零打扰的物理世界浏览器执行器官**：官方原生 Chrome 偏好注入彻底封死 WebRTC UDP 泄漏、原型链 Native Hook 伪装、确定性 PRNG 渲染噪声、macOS AppKit 原生无打扰伴生壳（`NonActivatingPanel` 绝不抢占打字焦点），以及两段式极简 AXTree 与自愈执行；
3. **Vibe Coding 时代的独立交付验收与体验打磨层（Acceptance & Improvement Layer）**：Builder/Coding Agent 负责生成与修复，WebTouch 作为独立 Judge 与体验副驾驶，不仅拦截物理客观缺陷，更提供专业级视觉审美、排版律动与交互工效提升提案，通过结构化 Communicate 协商协议在本地对齐取舍，以 Local Skill + CLI + Remote API 三位一体形态指导 Coding Agent 闭环修复与复验。

## 🌟 核心特性

- **极速在线执行与拓扑寻路**：吸收 `memory_systems`（PDDTMN）拓扑转移网思想，通过预编译 WASM 探针在 V8 内毫秒级提纯特征，结合 SimHash 拓扑路由以 `<15ms` 快速求解动作转移最优路径；
- **深度证据采样与双轮学习**：吸收 `search-engine` 深度证据采样思想，CC (Common Crawl) 离线通识矿 (L1) 与日常实战 Trace 蒸馏 (L2) 双轮驱动，动作前后状态突变闭环采样，无确凿证据不沉淀先验；
- **独立 Judge 与可信证据图**：将多视口对比、像素 Diff、语义检查、交互回放、运行时与其他证据源纳入同一 Evidence Protocol；明确未覆盖边界与重开条件，不让 Builder 自证完成；
- **原生深度反检测**：内置 Fitts 动力学曲线拟真轨迹、WebRTC Preferences 物理阻断、PRNG 确定性噪声注入与 State Bundle 冷迁移。

## ⚔️ 竞品差异化

传统方案（如 Browser Use, OpenClaw, Hermes）本质上是“LLM 驱动的 Browser Agent”，而 WebTouch 是“规则引擎 + LLM 蒸馏”：

| 维度 | 传统 LLM Agent (Browser Use 等) | WebTouch (本项目) |
|---|---|---|
| **在线决策** | 每次交互依赖 LLM 实时推理 | 规则匹配打分（< 15% LLM 兜底） |
| **单步延迟** | 500 ~ 2000 ms (取决于网络和推理) | **< 50 ms** (规则命中时) |
| **Token 成本** | 随操作步骤线性增长，成本高昂 | **极低** (离线固定成本 + 少量兜底) |
| **知识积累** | 无 (每次遇到相同页面重新推理) | **自进化** (L1 离线蒸馏 + L2 影子评估) |
| **适用场景** | 灵活、低频的长尾非确定性任务 | **高频、大规模、成本敏感的自动化操作** |

## 🏗️ 架构概览

```text
┌────────────────────────────────────────────────────────┐
│               离线学习环 (Offline Distillation)          │
│ Common Crawl / 在线日志 ──► LLM 批量蒸馏 ──► 知识库打包  │
└──────────────────────────┬─────────────────────────────┘
                           │ (SQLite 规则 + ONNX 模型)
┌──────────────────────────▼─────────────────────────────┐
│                 WebTouch 引擎 (在线执行环)                    │
│ WASM探针感知 ──► AC-BM25召回 ──► ONNX决策 ──► 异步验证   │
└──────────────────────────▲─────────────────────────────┘
                           │ gRPC (Initialize / Execute)
┌──────────────────────────┴─────────────────────────────┐
│                 外部协调器 (Coordinator)                 │
│         任务解析 ──► 状态机管理 ──► 宏观成功判定           │
└────────────────────────────────────────────────────────┘
```

## 📂 目录结构

```text
webtouch/
├── AGENTS.md          # 项目入口门禁（自动加载）：按入口路由加载规范
├── GLOBAL.md          # 跨域不变量与稳定决策索引（≤150 行）
├── cmd/               # 应用子项目：uws（CLI）/ uws-engine（引擎守护进程）/ uws-gateway（统一网关）/ viewer（原生伴生面板）/ archive/desktop（已归档）
├── pkg/               # Go 引擎内核（能力层）：browser / fingerprint / probe_wasm / matcher / classifier / verifier / knowledge / engine / qa（QA Studio 质检内核）/ challenge（挑战态感知与防乱点熔断）/ webcontrol（Web 控制台与流式视窗）
├── apps/              # 应用层（消费内核、提需求、找问题）：gateway（WebTouch browser provider 执行服务；客户端模型 API 归 Agent Hub）
├── pipeline/          # 现场实战蒸馏与学习：trace_distiller / component_gene / topology / aggregator / exporter
├── learning/          # 自进化闭关：online_learner / shadow_eval / snapshot_store
├── coordinator/       # Python 协调器（离线实验室）：planner / executor / reflector（历史 app/api_server 已归档至 archive/coordinator/）
├── dev/               # 开发测试页（deepseek_test.html / gemini_test.html）
├── scripts/           # 一键启动等开发辅助脚本（网关已自动拉起调试 Chrome）
├── tests/             # 测试骨架：unit / integration / e2e / security / fixtures（TDD 映射见 tests/README.md）
├── benchmarks/        # 性能基准（TDD-PERF-*）
├── proto/             # gRPC 接口单一事实源 (uws_service.proto)
├── spec/              # 内部核心设计规格 (给实现者/Agent阅读)
│   ├── algorithm-kernel.md       # 算法内核与平台契约 (V1.4)
│   ├── long-page-viewport-routing.md # D11 / T-30 长页面视口路由契约
│   ├── coordinator.md            # 协调器设计
│   ├── physical-patches.md       # 物理执行补丁
│   ├── knowledge-versioning.md   # 知识库版本化与回滚
│   └── security-dx.md            # 安全合规与开发者体验
├── docs/              # 活规范、研发指南与案卷；活规范唯一地图见 docs/README.md
│   └── evidence/      # 单次试跑、研究和历史记录；逐篇目录见 docs/evidence/README.md
└── .agents/skills/webtouch-skill/
    └── SKILL.md       # Agent 入口：窄读 CLI 能力、逐步决策、AX + 截图单回合回传
```

| 研究资产 | 入口 | 状态 / 日期 | 判据 |
| --- | --- | --- | --- |
| WorkBuddy 网络观察 hook | [脚本与使用说明](scripts/research/workbuddy-cdp-observer/README.md)；[AgentHub 研究案卷](../agent-hub/docs/evidence/research/2026-10-03-agenthub-workbuddy-connector-research.md) | 研究中 / 2026-10-03 | 运行证据与待验项以 AgentHub 案卷为准。 |

## 🚀 快速开始

> 当前版本：**v1.5.5**（node_id 确定性执行 + AXTree 导出 + DSL plan 文件）。
> 对外介绍/宣传版：[docs/evidence/archive/对外产品介绍-UWS-v1.5-2026-08-28.md](docs/evidence/archive/对外产品介绍-UWS-v1.5-2026-08-28.md)

### 0. 浏览器执行服务（Agent Hub 的 WebTouch provider）

```bash
./scripts/start_bus.sh
```
一键协同拉起：调试 Chrome（端口 9222，复用登录态）+ 执行引擎（gRPC 50051）+ WebTouch 浏览器执行服务（HTTP 8000）。网站账号、Vault 凭据与账号对应的 Profile/指纹由 WebTouch 管理；同账号请求复用原身份。客户端模型 API、provider 路由与 CLI/Web 连续体由 Agent Hub 提供；WebTouch 旧 `/v1/models`、`/v1/text`、`/v1/chat*`、`/v1/responses` 路径已退役。

**总线管理与环境注入：**
```bash
./uws bus status       # 探测 Chrome CDP(9222)、UWS Engine(50051)、浏览器执行服务(8000) 及活跃会话
```

Agent Hub 是客户端模型入口（本机默认 `http://127.0.0.1:18080/v1`）；网页模型请求选择 `provider=webtouch`，Hub 再调用 WebTouch 浏览器执行服务。WebTouch 仅开放 Hub 专用执行接口；本地执行服务自测可直接调用内部契约：
```bash
curl -N http://127.0.0.1:8000/internal/provider/chat \
  -H "Content-Type: application/json" \
  -d '{"channel_id":"deepseek-web","model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

**支持的高价值模型与多模态契约：**
- **Google Gemini**（重点推荐，真实网页版本对齐）：`gemini-3.8-flash`（主力 Flash）、`gemini-3.5-flash-lite`（极速）、`gemini-3.1-pro`（高级推理）、`imagen-3`（高清生图）、`veo-3.1`（Veo 3.1 视频生成）；
- **DeepSeek**（已全面合并升级为全新多模态大模型）：`deepseek-chat`（日常对话与图片理解）、`deepseek-reasoner`（R1 深度思考，支持 `<think>` 思考链与 `reasoning_content` 结构化分离），深度思考与智能搜索完全解耦且支持同时开启；
- **高价值创意平台**：`jimeng-web`（即梦 AI 网页端，集成字节跳动统一多模态 **Seedance 2.0** 视频生成与即梦生图）、`doubao-web`（字节跳动 **Doubao 1.5 Pro** 日常对话与绘图）。

**拟人频控与账号风控防护（Human Pacer）：**
网关内置 `HumanPacer`，自动对同一个浏览器页面施加拟人化节奏约束：强制轮次冷静期（默认 3s）+ 文本输入到提交的视觉停顿（1.5s）+ 0~30% 随机 Jitter，杜绝以超人类速度频繁请求导致的封号与验证码拦截。


### 1. 构建（版本+sha 一次注入）

```bash
make build          # 产出 bin/webtouch + bin/uws-engine + bin/uws-gateway + bin/uws-lease
make install        # 本地 release：build + probe-wasm + 固定 /usr/local/bin 入口
./webtouch version  # 打印 CLI / 引擎基准版本
```

### 2. 启动引擎守护进程（自动管理生命周期，防孤儿/版本漂移）

```bash
./uws daemon start    # 后台托管引擎（默认 127.0.0.1:50051，Setsid 独立进程）
./uws daemon status   # 显示构建版本 + 运行中引擎的实际 HealthCheck 版本；不一致会提示
./uws daemon restart  # 重启（make engine-restart = build + restart，杜绝漂移）
```

### 3. 目标模式（推荐/默认入口，2026-09-03）

不传子命令 / `uws chat "目标"` / `uws -p "目标"` 都是同一入口：一句自然语言目标即可。
文本 DSL 语法校验通过 → 自动走下方确定性 `run --plan`（不烧 VLM）；否则 VLM 每轮
“截图 + AXTree → 自决下一步”，引擎按 node_id 确定性执行。

```bash
./uws "发布微博：今天天气不错"      # 最短写法；登录页会停下等你（回车续跑/非交互报 WAITING_USER）
./uws chat "把 a.com 加入购物车"     # 显式写 chat 等价；付费/扣费护栏会暂停等你确认
```

结束自动输出：状态 + 当前 URL + 页面 AXTree 摘要 + 截图路径。

### 3.5 跑一个 DSL 任务（确定性自动化，熟悉目标站点场景）

```bash
# 稳定已知路径：整段 DSL 一次跑完（命令直传，不写临时计划文件；保留脚本才用 --plan-file）
./uws run --plan "nav:http://127.0.0.1:8899/shop.html
extract:全自动意式浓缩 | $desc
click:node:14 ; 白色
reply:产品：$desc"

# 单步 = 原命令只传一条 DSL（不新增子命令）：每次结束输出「当前页面」URL + AXTree
# 摘要，读完即可继续下一条。默认直接打印 stdout，要文件才加 --output。
./uws run --plan "click:站点管理" --target-url https://esa.console.aliyun.com/
./uws run --plan "input:站点名称=api.myread.top"
./uws run --plan "extract:页面正文 | $正文"
```

`--plan-file plan.dsl` 示例（nav → extract 绑定变量 → click:node → reply 复合插值）：

```bash
nav:http://127.0.0.1:8899/shop.html
extract:全自动意式浓缩 | $desc
click:node:14 ; 白色
extract:已选颜色 | $sku
click:node:17 ; 加入购物车
extract:购物车数量 | $count
reply:产品：$desc；已选颜色：$sku；$count
```

> 想看极简 AXTree（含 node_id）：`./uws api --target ...` 或经 gRPC `ExportPageTree`；
> 混合指挥：`./uws commander --tree-file tree.txt`（AXTree → VLM 指挥官 → DSL node: 指令）。

### 4. Vibe Coding 交付验收层 (QA Studio, 2026-09-15)

面向 Vibe Coding 与 AI 编程时代的独立交付验收层：用户给出一个 URL，WebTouch 按明确版本、环境和范围建立 Acceptance Contract，以独立 Judge 汇集多源证据，交付 Readiness Gap、Priority、Evidence 和 Agent-ready Fix Instructions，并支持同范围复验。首 1–3 单执行材料坚持 URL-first：不要求上传 Source Map、仓库或安装 Skill/MCP/Plugin，先用独立 HTML、最小 JSON 与可复制修复指令完成零安装交接；增强证据和连接器只由真实付费后的定位/交接摩擦解锁。现有浏览器能力可切换移动/平板/桌面视口，扫描横向溢出、控制台异常、死链/裂图与小点击热区，并执行视觉差分；这些是 Evidence Provider，不是全部产品定义。具体耗时与节省幅度按真实任务记录，不预先承诺。

当前商业执行状态：`DRY / PAID-CONCIERGE / PUBLIC` 均为 `HOLD`。近期只推进真实 Runner/失败关闭、零安装交付包、首单最小安全与数据边界、人工订单/退款/48 小时复验 SOP、卖方与收款适配及买方/预算/逐单成本台账；通过 `DRY + PAID-CONCIERGE` 后才允许定向接 1–3 单，公开案例与陌生 URL intake 另过 `PUBLIC`。状态与下一步以文档导航中的 v8 和 Requirement Registry 为准。

```bash
# 1. 零配置多端体检（检查溢出、控制台异常与裂图）
./uws qa --url http://localhost:3000

# 2. 视觉基准比对体检（与 Figma 设计稿执行洋红高亮 Diff）
./uws qa --url http://localhost:3000 --ref-img figma.png

# 3. 指定输出目录与自定义多视口
./uws qa --url http://localhost:3000 --viewports 390,768,1440 --output dist/qa
```

### 4.5. Web 控制台与流式伴生视窗 (uws studio，云端交互控制面)

面向云端多端看板与真人实时接管：内嵌轻量毛玻璃 SPA 前端与 WebSocket 双向伴生流。**当前仅为本机原型，不是可公网部署的控制面**：尚未接真实 QA Runner，缺少公网认证、任务/租户隔离与隧道契约；不得直接由 Nginx 暴露。

```bash
# 默认在 http://127.0.0.1:8080 启动 WebTouch 控制台
./uws studio

# 指定自定义端口
./uws studio --port 9090
```
- **Figma 级 Visual Diff 滑块**：双层拖拽滑动条，实时穿透查看基准设计稿与物理差分渲染；
- **缺陷瀑布流**：横向滚动条溢出（`scrollWidth > innerWidth`）、点击热区（`< 32×32px`）与控制台异常明细；
- **实时流式伴生画布与逆向接管**：低延迟推流渲染页面画面；在画布上点击或输入文本直接逆向派发至真实浏览器；
- **防乱点熔断告警与人工解除**：遇到 PayPal、Cloudflare Turnstile 等挑战态时瞬间红字熔断，支持人工点选通过后一键复位恢复执行。

### 5. 义眼模式（chat + 显式视觉参数，看图/对比/评审）

chat 默认是上面第 3 节「目标模式」；只要显式给了视觉输入（`--img/--url/--page/--diff` 等），
自动切到义眼：喂图（或让 uws 开站点截图）+ 一句问题，视觉模型作为有认知的体验评审者
返回结构化判断（是否一致/是否满足需求/按钮位置/手势是否成功），供 Agent 程序化消费。

```bash
./uws chat "判断两个实现是否一致" --img ref.png --img clone.png        # 图对图对比（默认结构化 JSON）
./uws chat "差异点在哪？给量化判据" --img ref.png --img clone.png --diff # 像素级 diff，差异图+统计
./uws chat "按钮细节" --img shot.png --zoom 120,80,200,120,4            # 区域放大，细看局部
./uws chat "这是哪种布局？" --img shot.png --plain                     # 单图评审，出自然语言
./uws chat "对比这两个站点第一屏" --url https://a.example --url https://b.example
./uws chat "帮我检查 a.com 和 b.com 的 differences"                     # 纯自然语言：自动提取 URL+开站点+像素 diff
./uws -p "两个实现是否一致" --img a.png --img b.png                    # -p 是 chat 顶层别名
```

> 义眼不产可执行动作、不规划多步、不自主执行——只做好「看一看 + 给反馈」；要执行目标请用
> 第 3 节目标模式（不带视觉参数）。详细参数见
> `docs/cli-and-config.md` 与 `uws help`。`--diff`/`--zoom`/`--region-color` 是 WebTouch 图像证据
> 原语：VLM 拿到确定性差异/放大/取色证据后再判断，而非裸眼看图猜。
> **自然语言自决（2026-08-28）**：只需问一句「帮我检查 a.com 和 b.com 的 differences」——
> 当问题自带 ≥2 个站点 URL 且意图是「比较/查差异」时，`uws chat` 自动提取 URL、打开站点截图、
> 启用像素 `--diff` 拿确定性证据，再让 VLM 给量化判据；无需显式 `--url`/`--diff`。
> **主控循环（多轮能力，VLM 自决）**：引擎具备多轮证据能力——VLM 可自主请求 `zoom`/
> `region_color`/`diff` 再判断，但被强约束为「现有证据够就直接 conclude（单轮优先、避免过慢）」，
> 只有确需细节才多跑一轮。所以「要不要多轮」由任务性质 + VLM 决定，不是模型天然的多轮工具调用
> （仍是自定义 JSON 契约，非标准 function-calling）。

## 📚 文档导航

> **活规范地图**：按 5 大高内聚业务域（Bus / Organ / Engine-Alg / Learning / QA Studio）组织的活规范总图详见 [`docs/README.md`](docs/README.md)。案卷逐篇目录见 [`docs/evidence/README.md`](docs/evidence/README.md)；旧代码与历史稿件索引见 [`archive/README.md`](archive/README.md)。

**项目状态与目标（2026-09-23 起为后续标准）：**
- [双模型总线真实联调（2026-09-26）](docs/evidence/pilots/debate-gateway-live-2026-09-26.md) — 实测案卷：认证探针 / 长文本修复与回归通过，World Dash → Hub 真实辩论的运行证据及边界。
- [WebTouch 目标与决策 (Goals.md)](Goals.md) — 目标域唯一入口：四大终局目标、终局红线（R1 零打扰 / 个人主权不做对外 SaaS / 凭证只走 vault / 语义白名单）、当前阶段目标与证据规则。
- [WebTouch 任务书 (Tasks.md)](Tasks.md) — 唯一任务与验收事实源：门禁（G-CLICK / G-PROOF / G-QA）、任务（含 T-23 Stage 3 与 2026-09-26 审查订正）、待用户拍板、停止条件与已授权动作。
- [WebTouch 项目交接 (handoff.md)](handoff.md) — 状态域（AOF）：当前值、本线硬约束、关键文件与证据指针、下一步；接力写在这里，不写进 `labs/handoff.md`。
- [WebTouch 系统功能与目录地图（2026-09-23）](docs/system-map-2026-09-23.md) — 面向第三方审查的功能、目录、消费方与调用链，并给出整体调度、算法决策、学习沉淀三条独立演化线及共享证据边界；静态地图，不替代目标/任务/运行状态事实源。
- [Wikipedia 真实站点候选采集试跑（2026-09-23）](docs/evidence/pilots/real-site-candidate-pilot-2026-09-23.md) — 单站真实 Chrome 采集与页面后验；含 562 个 Probe ref 的文本召回回放、候选身份标签，以及生产 matcher/离线 runner 路径差异和结构评分边界。
- [WebTouch Agent 互动入口契约（2026-09-23）](docs/agent-interaction-contract-2026-09-23.md) — 定义 Codex 主控与 WebTouch/VLM 分工、快速/逐步/只读观察模式、Target 连续性，以及 Python/Go 提示词中的 AX 与截图证据边界；真实浏览器闭环 2026-09-23 已走通（`DOGFOOD_PASS`）。
- [WebTouch 真实页面闭环验证（live dogfood · 2026-09-23）](docs/evidence/pilots/dogfood-live-2026-09-23.md) — 闭环五步的可复现记录（接管原 Target → AX → `click:node:11` → 后置证据）、日志行号、四层故障定位（缺陷在语义抽取层）与改进建议。

**开发者 / 外部协调器使用者：**
- [对外产品介绍（宣传版，归档）](docs/evidence/archive/对外产品介绍-UWS-v1.5-2026-08-28.md)
- [用户场景与最佳实践](docs/user-cases.md)
- [CLI 工具与傻瓜化配置指南](docs/cli-and-config.md)
- [快速开始教程](docs/quickstart.md)
- [gRPC API 完整参考](docs/api-reference.md)
- [如何编写协调器 (Coordinator)](docs/coordinator-guide.md)

**内核架构 / 研发规约 (Core Developers)：**
- [WebTouch 核心设计纲领 (WebTouchKeyDesign.md)](WebTouchKeyDesign.md) — WebTouch 浏览器身份与执行边界、隐蔽执行底座、Vibe Coding 交付验收、独立 Judge / Acceptance Contract / 多源证据、WSTN 拓扑寻路与 Go 执行服务。
- [WebTouch QA Studio 交付验收层白皮书 (2026-09-15)](docs/qa-studio/README.md) — Vibe Coding Acceptance Layer / Last-mile Verification 的产品与证据架构；Level 1/2 用于 URL-first 真实交付，Level 3 受证据控制，Level 4 保持 `HOLD`。
- [WebTouch 商业决策、体验演进与信任架构 (2026-09-16 升级)](docs/qa-studio/commercial-strategy-and-trust-architecture-2026-09-14.md) — **边界与商业决策事实源**；确立从单一 QA 扩展至 Improvement（体验演进），以 Local Skill + CLI + Remote API 为主交付形态，定义结构化 Communicate 沟通状态机与防咨询黑洞机制；负责 Open Edge / Closed Intelligence 与核心 IP 边界。
- [WebTouch Gateway Profile 与 VLM-Consult 架构规范 (2026-09-16，2026-09-17 修订)](docs/qa-studio/vlm-consult-and-scoped-endpoint-spec-2026-09-16.md) — **API 与会诊协议事实源**；定义 `/v1/gateways/{gateway_id}` 唯一推理入口、身份/配置/模块/能力/会话五类语义分工、协议兼容与执行兼容边界，以及 Local 持有执行权的 Tool-Calling 会诊闭环。
- [WebTouch QA Studio 商业与产品运营执行方案 v8 (2026-09-15)](docs/qa-studio/commercial-validation-execution-plan-2026-09-14.md) — **唯一商业与产品运营执行事实源**；在 v7 Concierge 关键路径上完成 Map / Skill 产品门槛复审，首 1–3 单坚持 URL-first、零安装，先走 `DRY → PAID-CONCIERGE`，公开案例和陌生 URL intake 另过 `PUBLIC`；保留双 Ship Moment、长期门禁、五泳道与 WIP 约束。
- [WebTouch QA Studio 交付准备 Requirement Registry (2026-09-15)](docs/qa-studio/delivery-readiness-requirements-2026-09-15.md) — 全生命周期检查面，也是 `multi-lane-execution-planning` 1.1.0 的 Requirement Backchaining 预演；`CJR-01`～`CJR-13` 是首 1–3 单的唯一阻断清单，其余条目在 `PUBLIC/PAID/BETA` 对应阶段生效，不另建路线图。
- [WSTN 网页状态拓扑网与 s3b6 路由算法验证规范 (2026-09-14)](docs/wstn-algorithm-task-spec.md) — 供协作智能体或独立进程开展纯离线、零 Token 算法验证的完整任务说明书、代码引用索引与验收门禁（P99 ≤ 15ms）。
- [WSTN S3B6 Tier 0 实验报告 (2026-09-14)](benchmarks/reports/EVAL-wstn-s3b6-verification.md) — 200 节点/500 边、30% 噪声、10-seed 的可复现实测；同时索引冻结的 `wstn.route.v1` Schema 与机器证据。
- [WSTN 页面状态 ROC 与因果录制报告 (2026-09-14)](benchmarks/reports/EVAL-wstn-state-roc-and-causality.md) — 8 状态/96 快照的校准+独立 holdout ROC（`D_th=6`，holdout 双侧错误 0%）与 5 步离线因果录制；真实 CDP 门禁保持 `NOT_RUN`。
- [商业级指纹浏览器工程蓝图与再造 Checklist (2026-09-14)](docs/fingerprint-browser-engineering-blueprint-and-checklist.md) — 官方原生 Chrome 六层纵深防御体系、WebRTC Preferences 封死、原型链 Native 保护、确定性渲染噪声、State Bundle 与单写者租约，含 42 项从零再造 Checklist。
- [浏览器指纹全套方案与展开态控制规范 (2026-09-12)](docs/fingerprint-and-viewer-final-spec-2026-09-12.md) — Phase 1~4 与 State Bundle 已接入 v1.5.7 引擎，并提供 `uws state export|inspect|import|restore`、可由 `uws-lease --tls-cert/--tls-key` 直接服务 HTTPS 的认证远程租约，以及 `uws request`（不启动浏览器、复用同档案 HTTP 可见身份与 Cookie）。纯 HTTP 请求不等同 Chrome TLS/HTTP2 指纹；真机双屏与真实双节点取证仍待验收。
- [Fingerprint + Viewer 真实验收运行手册 (2026-09-13，归档)](docs/evidence/archive/fingerprint-viewer-acceptance-runbook-2026-09-13.md) — 有头外站、Viewer 双屏、真实代理出口、双节点 State Bundle/Lease 的独立判据、脱敏证据与失败记录模板；本地回归不能替代这些结果。
- [Fingerprint + Viewer 完成审计 (2026-09-13，归档)](docs/evidence/archive/fingerprint-viewer-completion-audit-2026-09-13.md) — 原始承诺逐项映射到代码/测试/真实证据，明确已证明范围、被反证的绝对主张及四项剩余外部验收门槛。
- [指纹外站验收证据（headless，2026-09-13）](docs/evidence/fingerprint-audit/2026-09-13-headless/audit.md) — 判据：bot.sannysoft `HEADCHR_UA` FAIL→ok、UA-CH/时区/语言/屏幕/字体全对（硬判据 0 失败）；Phase 3 原型链 Hook（`[native code]`）与 Canvas 2D/Audio 确定性噪声注入落地（哈希跨档分歧）；真缺口 = WebRTC 出口不一致 / 无头无 WebGL（结论见方案 §7.5 与 §8）
- [项目交接快照（Viewer + Fingerprint，遗留 2026-09-17）](.mem/handoff.md) — **遗留**：现行状态域见 [handoff.md](handoff.md)；QA Studio 线的 `DRY / PAID-CONCIERGE / PUBLIC` 三门状态仍以该快照为准。
- [浏览器义肢系统内核设计文档](spec/algorithm-kernel.md)
- [安全、合规与开发者体验规约](spec/security-dx.md)

## 📄 License

WebTouch 核心项目为私有专有软件，当前不授予开源许可证。未来若公开开发者连接器、SDK、Schema
或示例，将在独立公开仓库中逐组件声明许可证；第三方依赖继续遵守其各自许可证。
