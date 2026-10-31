# webtouch/GLOBAL.md —— 项目核心规则与领域索引（≤150 行）

> 定位：WebTouch 核心事实源入口。在 webtouch 工作前必读。
> 架构大纲与唯一事实源：[`WebTouchKeyDesign.md`](WebTouchKeyDesign.md)；活规范地图：[`docs/README.md`](docs/README.md)；
> 目标：[`Goals.md`](Goals.md)；任务与门禁：[`Tasks.md`](Tasks.md)；状态与交接：[`handoff.md`](handoff.md)。
> 历史裁决（#1~#100）：已完整压缩归档至 [`archive/GLOBAL-归档.md`](archive/GLOBAL-归档.md)。

---

## 1. 核心定位与设计原则

- **工业级隐蔽执行底座**：原生 Chrome CDP 接管，偏好注入封死 WebRTC，原型链 Native Hook，确定性渲染底噪，macOS 伴生悬浮壳（`NonActivatingPanel` 绝不抢占前台焦点）。
- **浏览器模型 provider 与执行层**：为 Agent Hub 提供站点账号、Vault、账号/Profile/指纹绑定、网页会话与站点交互能力；Agent Hub 是客户端模型 API 和 CLI/Web 连续体唯一 owner。WebTouch 对外能力面是 CLI（`webtouch` / `uws`），session 生命周期在本机 lease。跨项目职责见 [Agent Hub 边界](docs/2026-10-03-agenthub-model-gateway-webtouch-boundary.md)，CLI session 契约见 [cli-session-contract-2026-10-31.md](docs/cli-session-contract-2026-10-31.md)。
- **确定性执行与实战蒸馏**：两段式极简 AXTree + node_id 确定性执行，自愈规则沉淀；ONNX/WASM 资产冷态待命（PARKED），Trace 现场蒸馏。

## 2. 活规范地图与读取

五大领域的职责、代码落点、规范文件与生命周期状态只在 [`docs/README.md`](docs/README.md) 维护；该地图是唯一聚合索引。处理模块前按地图进入对应规范，严禁全量通读 `docs/` 或 `archive/`。本文件只保留跨域定位、红线与稳定决策，不复制领域矩阵。

## 3. 高频红线与架构不变量

1. **R1 零打扰铁律**：执行动作不得激活浏览器抢占用户焦点；展开小窗只抬真窗口前置（互斥双向显示：小窗直播 ⟺ 真窗口不在前台）。
2. **防乱点熔断（Anti-thrashing）**：连续 3 次失败或挑战感知触发时立即硬熔断，**只锁触发它的真实账号页**，不锁引擎与无风险页面；只读动作（`extract`）豁免熔断。
3. **点击自证与后验原则**：`SUCCEEDED` 仅表示派发成功，不等于点中目标；必须比对 `hit_element` 与页面前后状态变化，未命中目标不得记入 learned 经验库。
4. **安全凭据库（Vault）**：`@vault:<key>` 由引擎侧本地解引用，`fail-closed`（找不到即停，绝不把字面量填进网页），明文严禁进 argv/日志/gRPC。
5. **互斥会话与续页**：会话绑定落盘 `~/.uws/sessions.json`，支持 24h 跨引擎重启续页；互斥任务（`--mutex-key`）同键共享窗口与动作串行排队。
6. **规范先行（Spec-First）**：改代码必须规范先行；单次测试报告与过程数据沉淀入 `docs/evidence/`，活规范严禁平铺垃圾文件。

## 4. 关键决策与演进索引（#101 ~ #132）

> 完整历史记录（#1 ~ #100）见 [`archive/GLOBAL-归档.md`](archive/GLOBAL-归档.md)。
> 旧引用交叉表：旧版 §3 五域矩阵 → [`docs/README.md`](docs/README.md)；旧版 §5 的 #1~#100 → `archive/GLOBAL-归档.md`，#101~#127 → 本节；旧版 §7 自动优化指令仅作历史记录，当前任务与执行门禁以 `AGENTS.md` / `Tasks.md` 为准。

- **#101 终局架构与债务清算**：废除 Tauri 壳收敛于原生 Viewer；CC 爬取归档转向实战 Trace（[`WebTouchKeyDesign.md`](WebTouchKeyDesign.md)）。
- **#102 浏览器 provider 拓扑**：Chrome/CDP + Engine + WebTouch 浏览器执行服务承载站点账号/Profile/指纹绑定、Vault、登录态和网页交互；客户端模型 API 与 CLI/Web 连续体归 Agent Hub。WebTouch 通用 OpenAI 模型门面已退役。2026-10-31 起对外入口改为 CLI，见 #132。
- **#103 算法资产保全与定位**：Proper WASM/SimHash/ONNX 暂停装配绝不放弃；融合 WSTN 拓扑转移网（[`docs/algorithm-assets-inventory-2026-09-23.md`](docs/algorithm-assets-inventory-2026-09-23.md)）。
- **#104 M1 销账与 Go 网关收敛**：彻底切除 Python api_server，运行时全面由 Go 二进制托管（`cmd/uws-gateway`）。
- **#105 WSTN Tier 0 路由与因果录制**：冻结 `wstn.route.v1` 契约，页面状态 ROC 校准（[`docs/wstn-algorithm-task-spec.md`](docs/wstn-algorithm-task-spec.md)）。
- **#106 QA Studio 交付验收层**：独立 Judge、Acceptance Contract 与礼宾商业验证定案（[`docs/qa-studio/README.md`](docs/qa-studio/README.md)）。
- **#107-#108 人机校验感知与防乱点熔断**：`pkg/challenge` 7 类挑战态雷达与 `CircuitBreaker` 护栏，阻断盲点封号。
- **#109 Web 交互控制面与流式视窗**：`pkg/webcontrol` 原生 Canvas 双向流式控制台（`uws studio`）。
- **#110/#114-#116 伴生观看小窗（Viewer）演进**：单一面板、✕ 真关（静默回收但不杀任务）、展开超时只回落、互斥双向显示。
- **#111 新窗口自动接管**：多标签页任务会话作用面自动前移，保留原 Target 不关（`TDD-ENG-21`）。
- **#113 默认窗口尺寸**：调整为标准排版尺寸 `1440x900`，屏外停靠防打扰（**屏外默认已被 #128 取代**）。
- **#117 拟人化指针轨迹**：高斯落点采样、过冲回拉与静默漂移（`pkg/browser/pointer_humanize.go`）。
- **#118 会话跨重启续页**：`~/.uws/sessions.json` 持久化绑定，24h TTL，同任务同窗口。
- **#119/#122 本地凭据库与 `@vault:` 直填**：纯 Go `age` 加密，引擎侧解引用，fail-closed 保护（`pkg/vault/`）。
- **#120 熔断与人机挑战解耦 + AX 影子观测**：错误码分流（`RESOURCE_EXHAUSTED` vs `UNAUTHENTICATED`），AX 影子量化。
- **#121 互斥任务（`--mutex-key`）**：同键共享窗口、不重导航、动作串行排队。
- **#123-#124 页面定位与行上下文**：结构化候选池 + 行上下文（`click:登录密码`）消歧，落点回显自证。
- **#125 Codex 主控入口**：Agent 单步主控，`webtouch run --observe` 只读观察，AX-first 与看图回合闭环。
- **#126 抽取门禁与只读豁免**：`extract` 严格模式（内容节点入池 + 精确匹配），命中回显，只读豁免熔断。
- **#127 技术演化三线隔离**：调度、算法、学习三线物理隔离，区分执行与后验（[`docs/system-map-2026-09-23.md`](docs/system-map-2026-09-23.md)）。
- **#128 专属窗口默认几何与窗口/Tab 治理**：默认改为「工作区内右上角固定位 + 尺寸夹进工作区 + **读回实际矩形**」，屏外停靠退为显式 `UWS_WINDOW_POS=offscreen`（取代 #113 的屏外默认）；实测「把 Tab 放进指定窗口」唯一路径 = 窗口内自建锚页 + 中键，代价 = 每次开会话前置一次窗口（建窗/搬窗/输入均不抢焦点）；取证 [`docs/evidence/pilots/window-tab-governance-2026-09-24.md`](docs/evidence/pilots/window-tab-governance-2026-09-24.md)，活规范 [`docs/fingerprint-and-viewer-final-spec-2026-09-12.md`](docs/fingerprint-and-viewer-final-spec-2026-09-12.md) §4.3。
- **#129 同指纹共享窗口默认化（`shared`）**：`UWS_WINDOW_MODE` 缺省 = `shared` —— 同指纹（同一 attach 端点 + 同一 profile）复用**同一扇**受管窗口，新会话 Tab 由该窗口内**自建锚页**以中键后台开出，`Browser.getWindowForTarget` 读回归属、落错即只关这一个自建 Tab 并 fail-closed（故障退化为 `window`，不阻塞会话）。**必要条件**：锚页是第二个 session，必须自己装「输入必达」护栏（焦点仿真 + 生命周期 active），缺它则窗口不在前台时中键挂起到超时（真机反证 → 修复 → 复验「一窗两 Tab」，9222 只有 2 扇窗）；派发只对 `mouseMoved` 重试 + 失败后 1s 宽限。代价 = 每起一个会话**可能**把该窗口前置一次（已定取舍，`window` 模式换回零前置）；取证 [`docs/evidence/pilots/shared-window-2026-09-24.md`](docs/evidence/pilots/shared-window-2026-09-24.md)，活规范 §4.3。
- **#130 展开动画与「真窗口到前台」两步（`T-17`，2026-09-24）**：点小窗展开 = ① 引擎把该会话真窗口抬到最前（CDP `Target.activateTarget`，**只是 Chrome 内部激活**，macOS 上不保证把 App 抬上来）＋ ② 壳拿到 `ok=true` 且确实要交棒时，按回执 `browserPID` 用 `NSRunningApplication.activate` 把**这个进程**抬到最前。**只按 pid、绝不按 bundle id**（bundle id 会命中用户自己的 Chrome，2026-09-13 评审已拒绝）—— 这是 R1「零打扰」的**唯一新增例外**，且只由「用户点小窗」这一个手势触发；附着模式（回 0）与无头会话都不动手。动画：落点 = 回执矩形**本身**（不是中心、不缩放），`0.18s` + `.easeOut`，动画内只插值 `frame`/`alpha`（不测量、不查 CDP）。取证 [`docs/evidence/pilots/viewer-expand-animation-2026-09-24.md`](docs/evidence/pilots/viewer-expand-animation-2026-09-24.md)，活规范 §4.4。
- **#131 小窗判据＝「真窗口看不看得见」+ 抬升挪到动画末段（`T-18`，2026-09-24 第二轮现场口径）**：① 小窗只在**真窗口看不见**时出现——按 `CGWindowList` 算可见面积占比，**≥60% 就让位**；真窗口**失焦但仍在屏上**时不再叠小窗（旧口径盯「前台」，那只是可见性的子集，用户看到的正是重复遮挡）。**遮挡者只算同 layer（0）且排在目标窗之前的窗口**、面积 ≥200×150（跨层收集会把 ChatGPT 的 `layer 3` 大窗误算成遮挡者）；取不到窗口列表才退回旧的前台判据。② **附着模式（`--attach`，`browserPID=0`）按展开回执的 `windowBounds` 在屏上认窗**（各边容差 4px，坐标空间与 `CGWindowList` 同一套）补出 pid 并记住——用户所有真实会话都在这条路上，不补 pid 等于判据不生效。③ `expand` 带 `deferRaise`：引擎**只摆位**（`PrepareViewerWindow`）、壳**末段**才发 `raise`（`Target.activateTarget`）＋ 按 pid 激活；这一下**必须排在非主线程**（`ViewerPanelController.raiseQueue`——主线程在解码大帧，`main.asyncAfter` 真机实测被推迟到 0.19–0.68s，设计值 0.15s）。④ **订正 #130**：那一条里「`Target.activateTarget` 在 macOS 上不保证把 App 抬上来」已被隔离取证**证伪**（自建会话 3/3 试次里引擎那一步本身就把前台切过去了，0.017–0.026s）——引擎那步**够用**，壳按 pid 激活是**真实场景兜底**（前台是同 bundle 的另一个 Chrome 实例时）。取证 [`docs/evidence/pilots/viewer-visibility-judgement-2026-09-24.md`](docs/evidence/pilots/viewer-visibility-judgement-2026-09-24.md)（三态读数与独立探针逐位一致），活规范 §4.4 第 3/4/5/6 条。
- **#132 对外 CLI、内部 session lease（2026-10-31）**：对外能力面改为 `webtouch` / `uws` CLI。session 状态机（active / idle / closed）落在本机 `~/.uws/cli-sessions.json`；CLI 只携带 `session_id`。Hub 只记账 `hub.channel` 与 `hub.site_conversation_id`。`:8000 /internal/provider/*` 降为迁移期入口（废弃头，或 `UWS_PROVIDER_HTTP=410`）。引擎 gRPC 仍是内部 IPC。契约 [`docs/cli-session-contract-2026-10-31.md`](docs/cli-session-contract-2026-10-31.md)。
