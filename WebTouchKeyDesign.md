# WebTouch Key Design (核心设计纲领与主执行蓝图)

> **定位**：WebTouch 全系统唯一事实源与认知对齐纲领。包含目标、约束、核心算法、架构路径与执行里程碑。后续所有设计、实现、重构与外部调用必须向本文档严格对齐。

---

## 一、 项目本质与四大终局目标 (JTBD)

**WebTouch 是浏览器执行与站点身份底座：管理网站目录、网站账号与 Vault，维持账号和 Profile/指纹的稳定绑定，并为 Agent Hub 执行网页交互。它同时提供桌面自动化与 Vibe Coding 交付验收能力。客户端模型 API 与 CLI/Web 连续体归 Agent Hub。WebTouch 自己的对外能力面是 CLI（`webtouch` / `uws`）：session 生命周期放在本机 lease，CLI 只携带 `session_id`。职责边界见[跨项目唯一规范](docs/2026-10-03-agenthub-model-gateway-webtouch-boundary.md)，session 契约见 [`docs/cli-session-contract-2026-10-31.md`](docs/cli-session-contract-2026-10-31.md)。**

用户雇佣 WebTouch 解决四大刚性终局诉求：

1. **提供稳定、可控的浏览器执行与站点身份（Browser Execution & Site Identity）**：
   - **痛点**：多 Agent 协同时代，人类被降级为在不同网页 AI（DeepSeek、Gemini、Claude、ChatGPT）与终端之间复制粘贴提示词和代码片段的人形搬运工。
   - **交付标准**：保存网站账号凭据与登录态，把同一账号稳定绑定到原有浏览器 Profile/指纹；接受 Agent Hub 的网页执行请求，复用账号身份、站点会话和风控策略。旧 OpenAI 兼容路径仅在迁移期作为兼容入口，后续由 Agent Hub 统一面向客户端。
2. **免封杀、零打扰的物理世界桌面执行器官（Non-Activating Silent Organ）**：
   - **痛点**：传统自动化工具（Playwright、Selenium、Browser-Use）运行时不断抢夺操作系统键盘焦点，导致人类正在输入的代码被打断，且易遭商业反爬风控秒杀。
   - **交付标准**：基于官方原生 Chrome 与独立沙箱，内置 WebRTC 物理偏好注入阻断、原型链 Native 伪装与确定性渲染噪声；配套 macOS AppKit 原生非激活伴生悬浮壳（`NonActivatingPanel`，360×250），后台流式显示，遇 2FA/WAF 时呼吸提醒并一键真窗口接管，解决后自动收回，**全程绝对不抢占输入焦点**。
3. **Vibe Coding 时代的独立交付验收层 (Vibe Coding Acceptance Layer / QA Studio)**：
   - **痛点**：AI 编码时代，Agent 生成组件与页面的速度提升了 10 倍，但人类肉眼走查与手动测试的速度依然是 1 倍。人类沦为 AI 生成代码的“质检流水线工人”，审核压力巨大。
   - **交付标准**：Builder/Coding Agent 负责生成与修复，WebTouch 作为独立 Judge；先把产品意图、验收标准、版本、环境与范围冻结为 Acceptance Contract，再汇集浏览器/视觉、源码配置、日志、部署与人工复核等 Evidence Provider，输出 Readiness Gap、Priority、可复核证据和 Agent-ready Fix Instructions。浏览器自动化只是证据能力，不是产品类别；关键条件变化后旧结论必须标记 `STALE`。
4. **单兵高精度主权外骨骼（Sovereign Power Tool，非灰产转售 SaaS）**：
   - **商业定性**：彻底放弃做大众消费级大模型转售代理（避免反规模效应、封号风险与法律高压线）。定位为个人开发者的高精度主权武器，在个人工作台上实现 100% 确定性的生产力放大。

---

## 二、 绝对约束与不可逾越红线 (Constraints & Invariants)

后续任何代码修改、重构或外部 Agent 执行任务时，必须严格遵守以下六大铁律：

1. **零打扰打字红线（Focus Invariant，最高禁令）**：
   - 任何单测、构建、自动化或后台守护进程，**严禁弹出任何可见窗口抢占系统键盘与打字焦点**；
   - AppKit 原生伴生面板无论在何种状态下，**绝对不可成为 Key Window 或 Main Window**；
   - 单元测试默认无头执行，真实 Chrome 端到端测试必须由环境变量 `UWS_RUN_CHROME_E2E=1` 显式隔离。
2. **零灰产与账号主权守则（Anti-Grey Market & Sovereignty）**：
   - WebTouch 是个人私有中枢，绝不作为公共 API 代理对外商业化转售；
   - 坚持个人主权总线原则，不搞免密黑客破解，严格依赖用户在沙箱目录中建立的合法登录凭证；
   - 单页面必须无条件遵守 `HumanPacer` 拟人频控约束（强制轮次冷静期、输入停顿与随机 Jitter），严防超人类速度操作导致账号被风控。
3. **双轨自愈原则，严禁单一技术执念（Dual-Track Pragmatism）**：
   - 严禁盲目执念于“纯页内直发（In-Page Direct）”：直发通道作为高通量流式加速器，在协议清晰、无 PoW 算力防刷的场景下追求极致性能；
   - 健壮 DOM 自愈交互（两段式极简 AXTree + `node_id` 确定性定位 + 遮挡自愈 + 智能重试）作为不可动摇的物理兜底底座；
   - 直发遇阻或报错时，系统必须毫秒级自动平滑降级至 DOM 路径，绝不阻塞任务。
4. **算法资产守卫原则，严禁盲目切除（Asset Preservation）**：
   - WASM 探针（`pkg/probe_wasm`）、拓扑哈希（`pipeline/topology.py` 与 `pkg/matcher`）、Meta Classifier 与 ONNX 内核（`pkg/classifier`）是系统的技术护城河；
   - 当前定性为 **“暂停待命并理顺依赖”**，必须保持接口冻结与测试绿灯，**严禁当作废弃代码删除**。
5. **数据按需探索原则，严禁全量无脑抓取（On-Demand Mining）**：
   - Common Crawl (CC) 是通识组件基因的特征矿，仅在明确定义的组件模式下进行按需抽样与离线提纯；
   - 严禁建立或维护全网海量 WARC 抓取流水线，避免陷入无意义的数据维护黑洞。
6. **单写者租约与冷迁移安全边界（Profile Lease & State Bounds）**：
   - 同一 Profile 目录在任何时刻仅允许单一写者持有租约（LeaseBackend）；
   - State Bundle（Cookies + LocalStorage + SessionStorage）导出必须走 AES-256-GCM 加密，严禁明文回显敏感凭证；跨端恢复只允许在同身份、未导航的纯净会话中导入。

---

## 三、 核心算法全景与跨项目深度融合 (Core Algorithms & Cross-Project Fusion)

WebTouch 的算法内核与 Labs 内的 `memory_systems`（记忆系统）及 `search-engine`（深度搜索）深度同构，形成“感知-记忆-寻路-采样”的完整闭环：

```
┌────────────────────────────────────────────────────────────────────────┐
│                        核心算法矩阵与跨项目融合                        │
├──────────────────────────┬─────────────────────────┬───────────────────┤
│    感知与特征提纯        │    状态图与寻径路由     │   证据采样与交付  │
├──────────────────────────┼─────────────────────────┼───────────────────┤
│ • Proper WASM 探针       │ • WSTN 网页状态转移网   │ • 深度交互因果采样│
│   (V8 内部极速 AST 抽取) │   (吸收 memory_systems) │   (吸收 search)   │
│ • SimHash / StructHash   │ • s3b6 概率拓扑寻路     │ • 可信交付证据图  │
│   (拓扑阻尼哈希/汉明距离)│   (<15ms 求解最优动作)  │   (多视口Diff/锚点│
│ • 义眼视觉量化审查       │ • Meta Classifier       │ • 离线/实战双轮矿 │
│   (--diff, --zoom 显微)  │   (ONNX 执行脑干)       │   (CC L1 + Trace) │
└──────────────────────────┴─────────────────────────┴───────────────────┘
```

### 1. V8 级毫秒特征提纯（Proper WASM Probe）
- **代码物理位置**：`pkg/probe_wasm/`（C++ / WASM 源码与预编译二进制）。
- **算法机制**：直接注入 V8 运行时，在 DOM 树与 RenderTree 遍历阶段抽取关键交互节点、ComputedStyle 计算样式与语义角色（ARIA Role），输出极简拓扑 AST。
- **价值**：彻底规避 CDP 协议传输序列化海量 JSON 的几十毫秒延迟，将单步感知耗时压缩至 `<5ms`。

### 2. 阻尼拓扑指纹与组件基因（StructHash & SimHash）
- **代码物理位置**：`pipeline/topology.py`（Python 基础算法）与 `pkg/matcher/simhash.go`（Go 运行时）。
- **算法机制**：将 DOM 树的父子层级关系、标签权重与文本分布投影为 64 位 SimHash；通过汉明距离（Hamming Distance）判定页面拓扑相似度。
- **价值**：天然免疫前端混淆工具生成的随机 CSS 类名与动态 DOM 抖动，准确识别“这是登录框”、“这是搜索结果列表”等核心组件基因。

### 3. 跨项目算法融合一 —— 网页状态拓扑转移网 (WSTN，吸收 Memory Systems PDDTMN 思想)
- **概念渊源**：`labs/world-agent/memory_systems` 的概率动态密度拓扑记忆网络（PDDTMN）。
- **算法机制**：
  - **打破单页死假设**：网页不是孤立的死截图，而是高阶有向状态转移图；
  - **状态节点（State Node）**：页面 DOM 拓扑指纹（SimHash）+ 当前交互焦点；
  - **转移边（Action Edge）**：用户或 Agent 施加的动作意图（如 `click:submit`, `input:query`）；
  - **拓扑寻径路由（Topological Hop Routing）**：借鉴 PDDTMN 的 `s3b6`（3步、6束）受限图搜索算法，在已知站点的状态网中以 `<15ms` 快速求解到达目标状态的最优动作路径；
  - **突破**：**将传统 Agent“每走一步都呼叫昂贵 VLM 进行视觉推理”的笨拙模式，降维为“在拓扑图上沿高概率边极速转移”**。

### 4. 跨项目算法融合二 —— 深度证据采样与可信交付图 (Deep Evidence Sampling，吸收 Search Engine 思想)
- **概念渊源**：`labs/world-replay/search-engine` 的 DeepSearch 证据图与原文锚点机制。
- **算法机制**：
  - **实战探索的因果闭环采样**：Agent 探索未知站点或沉淀自动化规则时，严禁盲目试错，必须遵循 `[动作前状态锚点] -> [动作触发] -> [动作后状态突变证据]` 的闭环采样。只有具备确凿前后因果证据支持的交互，才沉淀为 Site Recipe；
  - **交付验收的可信证据图（Acceptance Evidence Graph）**：WebTouch 交付给人类的不是一句空洞的“测试通过”，而是由独立 Judge 把多视口截图、像素 Diff、AXTree 突变、运行日志、源码/配置、部署状态与人工复核组织为带锚点的 Evidence Protocol；每条结论包含判定理由、证据与重开条件。

### 5. 执行脑干（Meta Classifier & ONNX 决策内核）
- **代码物理位置**：`pkg/classifier/` 与 `pipeline/exporter/`。
- **算法机制**：基于轻量级决策树/神经网络（ONNX），根据当前页面拓扑与任务意图，直接给出候选动作节点排序。
- **价值**：规则命中时单步决策延迟 `<50ms`，Token 消耗为 0。

### 6. 义眼视觉认知评审原语（Eye Vision Inspection）
- **代码物理位置**：`pkg/vlm/eyetools.go` 与 CLI 参数 `--diff`, `--zoom`, `--region-color`。
- **算法机制**：像素级绝对差值矩阵生成、局部关键区域动态高倍放大、色彩分布直方图量化，为视觉大模型提供确定性客观证据，而非让模型凭空猜测。

---

## 四、 统一架构拓扑与运行时收敛 (Paths & Topology)

### 1. 五层架构全景

```
┌────────────────────────────────────────────────────────────────────────┐
│ Layer 5: 原生伴生、人机接管与验收工作台 (Interface, HITL & QA Studio)   │
│ - macOS AppKit NonActivatingPanel 悬浮面板 (360x250, 绝不抢占打字焦点)   │
│ - Guard 护栏与类型化暂停 (遇 2FA / WAF / 付费时微提醒，一键接管解决)      │
│ - 独立 Judge + Acceptance Contract + 多源证据 + 修复指令与复验闭环      │
├────────────────────────────────────────────────────────────────────────┤
│ Layer 4: 站点适配与浏览器模型执行 (Browser Provider & Site Adapter)      │
│ - 仅供 Agent Hub 调用的网页模型执行契约；客户端模型 API 归 Agent Hub    │
│ - 账号 / Vault / Profile 与指纹绑定；同账号复用稳定浏览器身份           │
│ - 双轨传输引擎：In-Page Direct 加速 + 健壮 DOM 自愈交互兜底              │
├────────────────────────────────────────────────────────────────────────┤
│ Layer 3: 学习与自进化内核 (Learning Core & Graph Hop Traversal)        │
│ - 吸收 Memory Systems (PDDTMN) 拓扑跳转寻路：网页状态拓扑转移网 (WSTN) │
│ - 吸收 Search Engine (DeepSearch) 深度证据采样：动作前后状态突变闭环采样 │
│ - 双轮语料驱动：CC (Common Crawl) 离线通识基因矿 (L1) + 实战 Trace 蒸馏 (L2)│
│ - 算法资产守卫：WASM 探针 (V8 极速特征) + SimHash + Meta Classifier (ONNX) │
├────────────────────────────────────────────────────────────────────────┤
│ Layer 2: 智能体感知与确定性动作内核 (Sensory & Action Primitives)        │
│ - 两段式 AXTree 极简提纯 (语义内容树 + node:ID 交互索引树，极低 Token)    │
│ - 确定性动作原语 (node_id 直发、原生坐标拖拽、遮挡自愈、输入必达重试)     │
│ - 义眼视觉模型认知审查 (--diff 像素对比、--zoom 显微放大、--region-color)│
├────────────────────────────────────────────────────────────────────────┤
│ Layer 1: 工业级隐蔽指纹底座 (Stealth Fingerprint & State Sandbox)        │
│ - 官方原生 Chrome + Preferences 预置 (彻底封死 WebRTC 非代理 UDP 直连)   │
│ - 原型链 Native Hook 伪装 (重写 toString 为 [native code], 无变量污染)   │
│ - PRNG 确定性渲染噪声 (Canvas 2D / AudioContext LSB 掩码微扰，同身份幂等) │
│ - AES-256-GCM State Bundle (Cookies + LocalStorage 跨端冷迁移) + 租约互斥 │
└────────────────────────────────────────────────────────────────────────┘
```

### 2. 统一运行时收敛：浏览器执行服务作为唯一实施源
- **废除 Python API Server**：历史实现已切除并归档至 [`archive/coordinator/api_server.py`](archive/coordinator/api_server.py)，不再保留重复的 HTTP 监听逻辑；
- **浏览器执行事实源**：Chrome CDP 附着、多 Tab 会话隔离、账号/Profile/指纹绑定、Vault 与站点交互收敛至 Go 二进制 [`cmd/uws-gateway`](cmd/uws-gateway/main.go) + [`apps/gateway`](apps/gateway)。客户端 OpenAI 兼容 API、官方模型 provider 路由与 CLI/Web 连续体由 Agent Hub 持有；WebTouch 旧 OpenAI 路径仅在迁移期兼容保留；
- **Python 职能纯化**：Python 代码完全退出常驻 API 服务，回归其最擅长的核心职能：**离线数据科学实验室**（负责 CC 语料处理、组件基因拓扑提取、模型训练与离线评测）。

### 3. 运行拓扑
对外只有 CLI。内部仍由这些实体协同，它们都不是对外 API：
1. **调试宿主 Chrome (CDP :9222)**：承载用户真实身份与登录凭证，运行于专用沙箱配置目录；
2. **物理执行引擎 (`uws-engine` gRPC :50051)**：内部 IPC。接管底层 CDP 链路、注入抗风控指纹、执行页内脚本与确定性动作重试；
3. **本机 session lease**（默认 `~/.uws/cli-sessions.json`）：持有 `session_id` 的生命周期。`webtouch session create|use|close` 读写它；
4. **浏览器执行服务 (`uws-gateway` HTTP :8000)**：迁移期内部入口，`/internal/provider/*` 已废弃，不再当对外契约。客户端模型请求发往 Agent Hub，Hub 以本机 exec CLI 调用 WebTouch。

---

## 五、 实施路径与四个阶段里程碑 (Roadmap & Milestones)

系统演进分为四个边界清晰、可独立验证的阶段：

### Milestone 1 (M1)：运行时全面收敛与 Python API 彻底切除（已完成 · 2026-09-14 销账）
- **边界更新**：此里程碑记录 WebTouch 网关实施源的历史收敛；OpenAI 兼容路径现处于 Agent Hub 接管期间的兼容状态。目标边界见[跨项目唯一规范](../agent-hub/docs/2026-10-03-agenthub-model-gateway-webtouch-boundary.md)。
- **目标**：消除双端维护与协议漂移，确立 Go 网关为系统唯一 API 实施源。
- **具体工作**：
  1. 彻底切除并归档 `coordinator/app/api_server.py` 及相关多余提供商包装；
  2. 确保 CLI 命令（`uws api`, `uws bus`）100% 路由至 Go `cmd/uws-gateway`；
  3. 回归验证全量 Go 测试与 Python 离线单元测试。
- **验收完成判据（DOD）**：
  - `coordinator/app/api_server.py` 物理移除/归档；
  - `go test ./apps/gateway ./pkg/fingerprint ./pkg/viewerhub ./pkg/engine` 全部 PASS；
  - 历史验收曾直接调用 WebTouch `/v1/chat/completions`；当前职责边界已改为客户端请求走 Agent Hub，WebTouch 仅承接 `/internal/provider/*` 浏览器执行契约。

### Milestone 2 (M2)：WSTN 状态转移网与算法资产依赖理顺（当前进行中）
- **当前状态（2026-09-14）**：`wstn.route.v1` 消费契约、纯离线 Tier 0 与受控页面状态 ROC 已完成并通过（校准独立选得 `D_th=6`，holdout 跨页误合并/同页拆裂均 0%）；5 步离线 Mock 因果录制内核已生成连续 6 状态/5 边图。真实本地站 CDP 录制、生产 A* 替换及 Trace → Site Recipe/影子评估仍待完成，故 M2 不销账。
- **目标**：将 WASM 探针、SimHash 与 Meta Classifier 与 Memory Systems 的拓扑跳转寻路思想深度装配。
- **具体工作**：
  1. 规范化 `pipeline/topology.py` 与 `pkg/matcher` 的数据交互契约；
  2. 抽象定义网页状态节点与动作转移边数据结构（State Node + Action Edge）；
  3. 引入基于有向概率图的快速寻径算法（`s3b6` 拓扑路由单测），实现 `<15ms` 确定性路径规划。
- **验收完成判据（DOD）**：
  - 针对一个多步已知流程（如登录 -> 搜索 -> 导出），WSTN 状态转移网寻径耗时 `<15ms` 且 0 Token 消耗；
  - 离线学习能够从多轮实战 Trace 自动化导出 Site Recipe 并通过影子评估（`shadow_eval`）。

### Milestone 3 (M3)：Vibe Coding 前端交付验收工作台（核心产品线落地）
- **目标**：落地面向 AI 生成软件的独立交付验收层；商业路径以 URL-first 的 Ship Readiness Acceptance Job 先行，PR/CI 只是“1→1.1 生产发布”的后续场景分支。
- **具体工作**：
  1. 新增 CLI 命令 `uws qa --url <preview_url> --spec <spec.json>`；
  2. 自动启动多视口（移动端 390px、平板 768px、桌面 1440px）渲染捕获；
  3. 自动执行“像素级 Diff 差异比对 + WASM 拓扑结构体检 + 核心交互链路走查”；
  4. 生成带 Acceptance Contract、就绪差距、优先级、证据图、未覆盖边界、Agent-ready Fix Instructions 与结果生命周期的 Markdown/HTML 交付结果页。
- **验收完成判据（DOD）**：
  - 对一个明确版本与范围的真实 URL 执行 `uws qa`，产出可复核的交付结果页；耗时、人工分钟、修复采用与复验行为按真实任务记录，不预先承诺固定秒数。

### Milestone 4 (M4)：云端无头交付与 CI/CD 流水线接入
- **目标**：脱离本地桌面依赖，支持云端高并发无头运行，无缝嵌入 CI/CD。
- **具体工作**：
  1. 容器化打包无头执行引擎与网关镜像；
  2. 仅在“1→1.1 生产发布”出现重复付费证据后提供 GitHub Action 验收模板；
  3. 与 Labs 的 `world-dash` 及 `agent-hub` 完成调度接线。
- **验收完成判据（DOD）**：
  - 在云端环境中，代码提交 PR 后自动触发容器运行 `uws qa`，并自动在 PR 评论中贴出可视化交付验收报告。

---

## 六、 下个会话即时开工指引 (Directives for Immediate Execution)

为保证下个会话的 Agent 或开发者接手时**零歧义、零障碍、立即开工**，特制定即时动作检查清单：

### 1. 启动门禁与前期完成确认
- 按照 `labs/AGENTS.md` 规范，核对工作树状态（`git status --short`），保留其他项目的无关改动；
- **M3 前半段已圆满闭环**：`uws qa` 命令行表面、多视口（390/768/1440px）物理体检管线、横向溢出 `scrollWidth` 拦截、控制台未捕获暗病监听、洋红高亮像素差分、自包含 HTML/Markdown 交付报告生成，以及引擎内部 WSTN Dry-run Shadow 旁路审计均已 100% 落地并单测全绿；
- 确认本文档 [`WebTouchKeyDesign.md`](WebTouchKeyDesign.md) 为唯一事实源。

### 2. 下一阶段核心攻坚战役 (Next Stage Battleplan)

结合真实业务场景（如 Paypal 网站高阶人机风控卡点）与终局用户体验形态要求（告别纯命令行，走向云端可视化体验），攻坚战役落地记录如下：

#### 战役一：人机挑战态感知雷达与物理熔断中枢（P0 · 实战生命线，已完成）
- **核心痛点**：在 Paypal 等强风控站点上自动化频繁操作，但因未识别到人机验证页面，导致系统误判为普通延迟而盲目重复敲击失效区域，瞬间被风控升级为永久拦截；
- **技术实现与交付**：
  1. **落地 `ChallengeDetector` 挑战感知雷达 (`pkg/challenge/detector.go`)**：建立针对 Paypal Security Challenge、Cloudflare Turnstile/5s、Arkose Labs、reCAPTCHA、hCaptcha 等 7 大主流风控容器的 0-Token 极速 DOM/iframe 特征探针；
  2. **物理防乱点熔断护栏 (`CircuitBreaker`, `pkg/challenge/circuit_breaker.go`)**：一旦探测到页面处于挑战态或遭遇连续失败（3次），**立即强制熔断所有自动化重试与物理动作**，开路拒止盲目机械乱点；
  3. **Human-in-the-Loop 交互桥梁**：状态机联动 `UNAUTHENTICATED`，支持通过 `HumanInputForwarder` 逆向点击并解除挑战态，动作成功后熔断器自动复位。

#### 战役二：云端 Web 交互控制面与流式远程伴生视窗（P1 · 终局产品形态，已完成）
- **定位**：打破二进制 CLI 仅能服务极客与 CI/CD 的局限，构建面向最终用户、产品团队与客户的云端交互控制台；
- **核心体验与交付**：
  1. **Web SaaS 交付看板 (`pkg/webcontrol`)**：内嵌纯原生轻量化 SPA 前端（`assets/index.html`），输入 URL 即可触发多视口并发走查，出具可在线拖拽的 Figma 级双层 Diff 滑块与横向溢出瀑布流；
  2. **云端流式远程视窗 (Cloud Streaming Remote Window)**：基于 WebSocket 实现极低延迟流式画布推流与挑战态高亮告警；支持直接在网页 Canvas 上逆向点击、文本键入与一键熔断复位（`uws studio --port 8080`），彻底突破纯无头云端执行的物理死穴。

#### 战役三：WSTN 真实 CDP 因果实测闭环与 Level 3 规格驱动走查（P1 · 算法收口与产品合流）
- **技术实现与交付**：
  1. **算法线实测封顶**：`benchmarks/bench_wstn_cdp_causality.py` 驱动本地真实 Headless Chrome，完成 5 步真实 CDP 因果跃迁录制与寻径闭环（全量管线输出 `causal=PASS cdp=PASSED`），算法线宣告全面大功告成并封顶；
  2. **规格驱动走查演进**：持续将 WSTN 状态 SimHash（`D=6`）沉淀为因果跃迁裁判，深度融入 QA Studio 自动化验收流。

### 3. 后续战役登记（2026-09-24 用户定序：只登记需求、不开工）

#### 战役四：用户示范学习（teach-by-demonstration，用户这一遍 ＝ 教一遍）
- **需求**：自动化在某站失败、问 VLM 也失败时不再死磕 —— 打开该页并显示一个**不挡鼠标**的浮层给提示，把操作交给用户本人；Agent 记录用户的完整轨迹（点了哪些元素、走错与后退、输入与提交），并在密码输入 / 提交时提示「是否记入 vault」「是否随机生成密码」。产出两条：① 凭据进 vault；② 该站点的一份**可回放操作范本**。
- **定序（用户明示）**：**排在算法 / 学习与自动化线之后** —— 先做它会挤压既有任务的推进（「有了我这个，自动化就不好学了」）。
- **任务与验收事实源** → [`Tasks.md`](Tasks.md) `T-20`（状态 `NOT_STARTED`）。本处只登记需求，不复制验收判据。
