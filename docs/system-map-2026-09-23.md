# WebTouch 系统功能与目录地图（2026-09-23）

> **用途**：第三方在提整理 / 解耦 / 模块化 / 新需求落点建议前，先建立系统全貌。
> **事实源**：目标红线 [`../Goals.md`](../Goals.md)、任务验收 [`../Tasks.md`](../Tasks.md)、运行状态 [`../handoff.md`](../handoff.md)、入口路由 [`../AGENTS.md`](../AGENTS.md) 与 [`../GLOBAL.md`](../GLOBAL.md)。本图只做导航与审查输入，不复制它们的状态。
> **取证边界**：静态核对至审查批次 `6e07d10a`；Labs `main` 与 `origin/main` 同步于 `ec0ac32e`。本图含明确标注的目标责任模型；其余现状截至该批次，源码与 Hub 未改。WebTouch 本轮文档更新（`Goals.md` / `Tasks.md` / `README.md` / `GLOBAL.md` / 本图 / 项目 handoff）已随本提交入库。运行态未变（Gateway `v1.5.5+c4314714`、Engine `v1.5.5+d7455e39`）。用户报告的闭环门禁全绿；本图未重跑 build/test/浏览器。成熟度标「已接线 / 局部 / 研究 / 归档 / 待审」。
> **§9 / §10 补记（2026-09-24，`T-14` 只读审计）**：新增两节的代码/文档快照取自文档体系重组批次 `8dff14b6` 之后的 HEAD；判定依据为只读 `rg` + 实跑 `go test` / `pytest`（复跑命令见 §9.6），本轮未改运行时代码。§1~§8 的旧批次结论未重跑，冲突时以 §9 为准。

## 1. 系统边界一览

WebTouch 是本机优先的真实浏览器执行系统：外围有 Python 协调器、Go 浏览器引擎、个人 AI Gateway、QA/站点检查、原生 Viewer，以及离线算法与学习流水线。共用仓库与部分运行时，但不全在同一条在线请求链上。

```text
用户 / Agent
  ├─ uws CLI（Go） ── 部分命令转发 ──> coordinator（Python 规划、护栏、证据、反思）
  │                                      │ EngineClient / gRPC
  ├─ Gateway（Go HTTP/OpenAI API） ──────┤
  ├─ QA / Studio（CLI + pkg/qa + provider + webcontrol）
  └─ Viewer（Swift 原生面板） <── 本机 socket / ViewerBridge ──┐
                                                              v
                         proto/uws_service.proto ──> Engine（Go gRPC）
                                                        ├─ Browser/CDP
                                                        ├─ matcher / classifier / verifier
                                                        ├─ challenge / fingerprint / vault
                                                        ├─ VLM / knowledge / session 状态
                                                        └─ viewerhub

离线/旁路：pipeline（Trace/WSTN/训练） --候选数据/显式版本交接待核--> learning（影子评估/先验）；
tier1 离线 matcher 评测；tier2 经 browser 启动真实浏览器；
probe_wasm 是 C++/WASM 实现，由 pkg/browser 的 Go/JS 宿主经 CDP 注入真实页面。
```

- **Engine 边界**：`proto/uws_service.proto`（绑定在 `proto/gen/`）是 Engine gRPC 的唯一契约源；`apps/gateway` / `cmd/uws-gateway` 是独立 Go 应用，不得反向成为核心库依赖（现只依赖 `pkg/contract`、`pkg/fingerprint`，引擎通信走 EngineClient 抽象）。
- **proto 不是全系统唯一协议**：Gateway 另有 OpenAI/Responses HTTP 契约，Viewer 有 Go/Swift socket，QA Provider 暴露 EvidenceBundle，DOM 探针有 Go/C++/页内 JS 序列化布局，离线学习另有 artifact schema。新需求应更新它实际跨越的那个边界。
- **跨层硬约束**：不抢系统焦点；动作成功以页面侧证据为准；凭据只经本地 vault 解引用；协议字段 / 动作语义由 proto 源定义并同步生成物；在线学习只消费过证据与晋升门禁的样本。审查建议应保留这些语义，或明确指出要改哪条红线。

### 当前消费方与接口需求

| 消费方 | 接入与实际用法 | 对 WebTouch 的需求 | 接线判定 |
| --- | --- | --- | --- |
| AIGC 本地创作 (`aigc/`) | `aigc/gateway.py` 经 `uws bus env` 取得 Agent Hub 模型 API；Hub 的 `webtouch` provider 调用 WebTouch 浏览器执行服务；AIGC 的登录暂停复位仍调用 WebTouch 管理 API | OpenAI 兼容模型目录与文本生成；稳定返回模型 ID/文本/延迟/token；认证拒绝与挑战有可识别语义且 fail-closed；Key 不落盘；支持模型覆盖 | **经 Agent Hub 的 WebTouch provider**（登录态与站点风控归 WebTouch） |
| Agent Hub WebTouch 节点 | `internal/worker/webtouch.go` 注册 `webtouch` 节点；`cmd/ahub/webtouch_graph.go` 的 `webtouch-inspect-v1` 执行 `uws inspect --isolated` 子进程并解析 JSON EvidenceBundle；超时向 CLI 发 SIGTERM | 有界 URL 体检/视觉差分、视口与超时、JSON 稳定、脱敏摘要、Verdict/HealthScore、digest；权限 fail-closed、SSRF 阻断；独立临时 Profile，不自动附着个人 Chrome | **直接代码接线**（当前是 QA/证据工作，非任意多步动作 provider；Graph 公开输入目前只有 URL） |
| World Dash | `world-dash/adapters/gateway.ts` 调 Hub Gateway 的 overview/runs/sessions/control，并有 `/responses` 客户端方法 | 主要需要 Hub 的运行/会话/控制契约；只有显式选择 WebTouch Graph 才经 Hub 间接用到 WebTouch | **Hub 控制面客户端**；未找到 Dash 选择 WebTouch Graph 的调用点，别把 Gateway 管理 API 当 WebTouch API |
| Sidecar / Judgment Runtime | 文档把 WebTouch 列为按需的现实/浏览器证据 provider；Hub Profile 已配 `webtouch`/`browser_inspect` 能力 | 仅在证据缺口时触发，按 scope/capability 授权，收脱敏带出处的结构化证据，不进高频主事件路径 | **产品/权限设计已出现，未发现独立实现**；不计当前直接代码接线 |

**计数口径**：静态源码可证实 **2 个直接消费入口**（AIGC Personal AI Bus、Agent Hub WebTouch QA 节点）。World Dash 是 Hub 控制面客户端，Sidecar 是 Hub 调度目标。WebTouch 自身的 CLI/Coordinator/Gateway/QA Studio/Viewer 是一方组件，不重复计数。清单表示接线状态，不证明服务都在运行。

## 2. 用户可见功能与实现落点

| 功能面 | 对外入口 | 主要实现 | 输入 / 输出 | 边界与审查点 |
| --- | --- | --- | --- | --- |
| CLI 与浏览器任务 | `uws`、`webtouch` | `cmd/uws/`；Python 命令在 `coordinator/` | 目标/DSL → 页面动作、报告、截图/AX | CLI 同时承载进程控制、任务、QA、身份、状态、凭据、Viewer；Agent 主链走 coordinator/Engine gRPC，QA CLI 可经 `pkg/qa` 直接建 session。按用例区分边界 |
| 自然语言与计划执行 | `uws <目标>`、`uws run` | `coordinator/{goal,planner,executor,reflector,guard,vlm}.py` | 目标/计划 + 页面观察 → 一步动作、任务状态、反思/报告 | Python 拥有高层任务语义；Go 引擎不重复规划/重试/恢复 |
| 浏览器会话与确定性动作 | Engine gRPC / CLI | `pkg/engine/`、`pkg/browser/` | Context、ActionIntent → 页面事件、命中/验证、页面状态 | Engine 是策略/编排汇合点，Browser 是 CDP/窗口/AX/输入/截图机制；两层责任需持续区分 |
| Agent/模型互动总线 | OpenAI 兼容 HTTP | `cmd/uws-gateway/`、`apps/gateway/` | Chat/Responses、Profile、Tool call → 流式响应/动作 | 进程边界清楚；站点登录/挑战状态、Profile 身份、动作授权各自归属需审查一致性 |
| 页面感知与目标命中 | 引擎内部 | `pkg/probe_wasm/` + `pkg/browser/probe.go`、`pkg/matcher/`、`pkg/classifier/`、`pkg/vlm/`、`pkg/engine/vlm_fallback.go` | DOM/AX/截图/语义 → WASM 候选、matcher 候选与 VLM 兜底 | WASM 注入已接入 Browser；matcher 可独立测试，但 Browser 的 probe/tier2 直接调 matcher；「命中」≠「页面效果证明」 |
| 页面效果验证 | Engine / 协调器报告 | `pkg/verifier/`、`pkg/engine/`、`coordinator/evidence.py` | 动作前后页面状态 → observed changes、hit、状态结论 | 成功回执与页面后验不是同一判据；需明确唯一所有者 |
| 人机挑战与熔断 | Engine / Gateway | `pkg/challenge/`、`apps/gateway/authstatus.go` | 页面信号/认证检查 → challenge、暂停、锁定、人工复位状态 | 引擎页面 challenge 与 Gateway 站点认证锁属于不同策略域；共用名词不等于共享状态所有者 |
| 身份、会话状态与并发租约 | CLI / Gateway / Engine | `pkg/fingerprint/`、`pkg/browser/lease_factory.go`、`cmd/uws/state.go`、`cmd/uws-lease/` | Profile、StateBundle、Lease → 身份与会话隔离 | 三者相关但语义不同；Cookie/凭证、身份伪装、并发占用各归其界 |
| 本地凭据 | `uws vault`、Engine 动作 | `pkg/vault/`、`cmd/uws/vault.go`、Engine 集成 | vault 名称引用 → 本机直填 | 明文不可经 argv/协调器/gRPC/日志（数据流安全边界） |
| 桌面观看与控制 | `uws viewer`、常驻 Viewer | `pkg/viewerhub/`、`pkg/browser/viewer_bridge.go`、`cmd/viewer/` Swift | Frame、Pointer、Intent、状态 → 非激活观看窗/受控交互 | Go/Swift 由 socket 协议耦合；协议版本、焦点语义、坐标变换需成对验收 |
| QA / 网页验收 | `uws qa`、`uws inspect`、Studio | `pkg/qa/`、`pkg/provider/`、`pkg/webcontrol/`、`cmd/uws/{qa,inspect}.go` | URL/Viewport/DOM 与截图 → Overflow、console、link/image、热区及报告 | Hub 走 `inspect --isolated`；普通 CLI 默认不变，仍可显式 `--cdp-url`。Provider 用自有 `ViewportSpec`，QA 产品流程与底层能力须分层 |
| 网页知识与规则热更 | Engine | `pkg/knowledge/`、`pkg/classifier/`、`pkg/matcher/` | 版本化规则/模型 → 匹配与分类运行态 | 核对 schema/规则/模型/版本回滚的所有权；Knowledge 不应变成任意业务数据仓 |
| Trace 蒸馏与离线学习 | 离线脚本/实验 | `pipeline/`、`learning/`、`benchmarks/` | Trace、页面状态/拓扑 → 候选规则、模型、评估报告 | 数据/研究路径；只有经影子评估、证据门禁与显式发布契约后才应影响在线执行 |

## 3. 目录职责地图

| 路径 | 职责 / 关键子区 | 依赖方向 | 属性 |
| --- | --- | --- | --- |
| `cmd/uws/` | 用户 CLI：chat/goal/run、daemon、health、QA/Studio、vault、state、request、viewer、inspect | 调 coordinator；部分直接用 `pkg/*` 或起 Engine | 多用例入口；Go 对 Python 边界需持续核实 |
| `cmd/uws-engine/` | Engine 进程组装、gRPC server、浏览器与配置启动 | `pkg/engine`、browser、fingerprint、knowledge、viewerhub、vlm | Composition root |
| `cmd/uws-gateway/` + `apps/gateway/` | 独立 HTTP / OpenAI 兼容 API、Profile、Provider、认证状态、Tool/Chat/Responses | gateway → EngineClient/gRPC、contract、fingerprint | 应用层；不被核心反向依赖 |
| `cmd/uws-lease/` | HTTP/TLS 租约服务入口 | `pkg/fingerprint` Lease backend | 可独立部署 |
| `cmd/viewer/` | SwiftUI/AppKit Viewer 与 ViewerCore：焦点守卫、面板、socket、指针、帧源 | 本机 Viewer socket 协议 | 跨语言客户端 |
| `cmd/tier1/`、`cmd/tier2/` | 算法基准入口 | Tier1 → matcher；Tier2 → `pkg/browser` → matcher | Tier1 离线；Tier2 依赖浏览器 |
| `coordinator/` | Python 任务语义、规划、执行、护栏、证据、VLM、反思、crawler/explorer、gRPC client | 内部模块 → Engine gRPC；生成 Python proto | 在线协调器；`coordinator/app/` 已归档 |
| `proto/`、`proto/gen/` | Engine API 源契约与 Go/Python 绑定 | 所有跨进程调用方 | 唯一跨语言契约源；生成文件不可手改 |
| `pkg/engine/` | gRPC service、session/context、动作编排、鉴权、知识接线、VLM fallback、日志、AX shadow、Viewer | 直接依赖 browser、challenge、classifier、fingerprint、knowledge、matcher、vault、verifier、viewerhub、vlm | 依赖汇合较多；需查是否承担过多策略/集成责任 |
| `pkg/browser/` | CDP/Target/窗口/AX、鼠标键盘拖拽、probe/screenshot/screencast、ViewerBridge、恢复与 profile | challenge、contract、fingerprint；部分路径直接 matcher | 最大运行时包；浏览器机制与部分算法/会话策略混合 |
| `pkg/matcher/`、`pkg/classifier/` | AX/语义匹配、规则、结构编码、A*/WSTN、分类/ONNX | matcher 尽量纯；classifier 用 matcher | 算法域；Tier1 离线可运行 |
| `pkg/verifier/`、`pkg/challenge/`、`pkg/contract/` | 页面状态验证、挑战检测/熔断信号、共享内部契约类型 | 被 Engine/Browser 消费 | 候选稳定领域边界；确认是否独立于具体 browser 实现 |
| `pkg/fingerprint/`、`pkg/vault/` | 指纹 Profile/状态包/HTTP identity/Lease；本地加密凭据库 | browser、gateway、Engine/CLI | 两个不同安全域，别并成泛化 profile/session 包 |
| `pkg/viewerhub/`、`pkg/webcontrol/` | Viewer session/intent hub；本地 Web 控制 UI 与服务 | viewerhub → browser；webcontrol → browser/challenge/qa/viewerhub | 本地控制面，聚合多能力 |
| `pkg/qa/`、`pkg/provider/` | QA 运行时/报告；对外证据 provider | QA Runner 经窄 interface 消费浏览器 | Provider → QA Runner 是证据用例消费领域服务，不因包名判为倒置 |
| `pkg/browser/profile_dir.go` | 临时 Profile 创建、锁检测、陈旧临时目录回收 `ReapStaleTempProfiles` | `pkg/qa` isolated 路径调用；仅处理 `$TMPDIR/uws-cdp-*` | 持久 Profile 不在回收范围。**缺口**：`processAlive` 把所有 signal-0 错误当「进程已死」，需改为仅确认进程不存在时才删 |
| `pkg/vlm/` | VLM 客户端、Eye/工具、learned store | Engine、CLI、QA 使用 | 模型访问与页面策略应经接口分层 |
| `pipeline/`、`learning/`、`benchmarks/` | Trace、拓扑、WSTN、蒸馏、训练、影子评估与性能实验 | Python 离线模块内依赖 | 研究/离线域；生产晋升点需明确 |
| `pkg/probe_wasm/` | C++ DOM 候选特征/截断探针与 standalone WASM 构建源 | Make/CMake 构建；`pkg/browser/probe.go` 读 WASM 经 CDP `Runtime.evaluate` 注入；`cdp_session.go` 预安装 | 在线运行链已接线；产物缺失时降级到 JS 文本采集。Go 适配器在 `pkg/browser/`，不在该 C++ 目录 |
| `tests/`、包内测试、`cases/` | unit/integration/e2e/security、fixture 与场景数据 | 分别验证算法、协议、运行态 | 真实浏览器 E2E 成本高；应分层使用 |
| `docs/`、`spec/`、`AGENTS.md`、`GLOBAL.md`、`Goals.md`、`Tasks.md`、`handoff.md` | 使用文档、技术规格、读取路由、目标、任务与状态 | 互相链接及代码/协议 | 多文件同名需写清相对根；活跃事实与历史记录显式区分 |
| `archive/`、`.mem/` | 归档实现、旧文档、历史 handoff | 无在线依赖（预期） | 默认不参与当前实现；路径文字可能出现在历史中 |
| `bin/`、`build/`、`.venv/`、`.pytest_cache/`、`uws-*-assets/` | 本地构建物、环境、临时验收产物 | Make/开发运行 | 非源码；忽略策略、恢复脚本、归档清理另审 |

## 4. 关键运行链与数据流

| 链路 | 顺序 | 边界应承载什么 | 不应泄漏 / 混淆什么 |
| --- | --- | --- | --- |
| 普通页面任务 | CLI → coordinator → Engine gRPC → engine service → browser/CDP → 页面，结果原路返回并采集证据 | coordinator 管任务分步与用户目标；Engine 管会话与受控动作；Browser 管协议与物理操作 | `SUCCEEDED` ≠ 页面效果；重试/幂等语义不能在 Go/Python/proto 各写一份 |
| 目标定位 | AX/截图 → matcher/规则或 VLM → 候选/动作 → browser 命中 → 后验 verifier/evidence | 定位候选、执行落点、动作效果应可分别观测 | 不把「有候选 / 发出事件 / 点中目标 / 状态改变」折成一个布尔 |
| Gateway 对话 | HTTP → handler/auth guard → Profile/channel/provider → Engine gRPC → OpenAI 风格响应 | Gateway 管协议兼容、Profile/渠道与授权状态；Engine 提供页面执行能力 | 认证锁 ≠ 浏览器 challenge 锁；请求不得越过个人 Profile/安全边界 |
| Viewer | Engine/browser screencast → ViewerBridge/hub → socket → Swift；用户交互走 intent 反向链 | Go 侧持会话与页面执行；Swift 侧负责非激活展示/输入呈现/本机交互 | 自动化不抢系统焦点；Go/Swift socket schema 保持同步 |
| QA 检查 | CLI/Studio → QA runner/provider → 浏览器 session → 多源证据 → report/webcontrol；Hub 路径强制 `--isolated`，CLI 处理 SIGINT/SIGTERM 以运行 `defer session.Close()`，Hub 超时发 SIGTERM 并等最多 5 秒 | Provider/BrowserSession 抽象隐藏具体 CDP；报告保存判据与出处；QA Runner 清理自管进程与临时 Profile | 正常取消路径能清理；SIGKILL 时 defer 不保证运行。回收器只删锁指向的死 PID Profile |
| Context 取消与副作用去重 | gRPC ctx → Engine → Browser 动作；派发前标记，错误区分 `canceled_before_dispatch` / `canceled_after_dispatch`；after 情形以 `indeterminateGrace=15s` 挡同 intent 盲目重放 | 调用方 deadline 与动作上限取较小（单次动作 8 秒；多阶段切换语义 25 秒，见 gateway-contract §6）；派发后状态是「结果未知」而非已完成 | 主 ActionSession/Node 路径已贯通；**缺口**：VLM 坐标回退 `ClickAt`/`InputAt` 缺派发状态分类，post-dispatch 取消会被当普通失败并清 in-flight；`indeterminate`/`inflight` 仅在相同 intent 再来时才清，未重试的唯一键可能长期占 map |
| 学习晋升 | 交互 Trace → pipeline 蒸馏/训练 →（自动交接状态待核）learning shadow eval → 评审/版本化发布 → 在线消费者 | 离线生成物须有 schema、基准、版本与可回滚的唯一接入点 | 未证明的 miss/错误落点不可作正样本；旁路产物不得暗中改变线上行为 |

### 三条独立演化线（目标责任模型；新需求分流与第三方审查主视图）

下图是建议的职责边界，不声称当前调用已完全按此解耦；现状证据仍见本图 §3–§5。

```text
Agent Hub（跨 Provider Graph/run/task）→ WebTouch Provider entry
                                         ├─ Coordinator（浏览器任务步骤 / 语义恢复）
                                         ├─ QA / inspect（体检与证据）
                                         └─ Gateway / 其他本地入口
                                                ↓ 页面任务
                                  Engine（session / intent / 安全状态）
                                     ├─ Browser 采集与物理派发（Probe/WASM、AX、截图、CDP）
                                     └─ Algorithm 快路径 → 不确定/长尾时 VLM → 有序候选或 abstain
                                              ↓ 决策 + 执行 + 页面后验
                                  Learning Episode（正/负/未知，脱敏并带来源）
                                     └─ 离线 replay / shadow eval → 版本化知识产物
                                                                  └─ 门禁后交给 Algorithm/Knowledge
```

| 演化线 | 唯一回答的问题 | 输入 → 输出 | 隔离方式与独立验收 |
| --- | --- | --- | --- |
| **整体调度** | 下一步做什么、何时重规划、任务预算是否耗尽？ | Hub run/task；WebTouch Coordinator 的页面步骤 → step/intent/attempt 与有界恢复 | Hub 只管跨 Provider；Coordinator 只管浏览器任务语义；Engine 只管 intent/session/护栏；Browser 只管物理可靠性。Fake Engine/Browser 模拟测试，不依赖算法、VLM、真页面 |
| **算法决策** | 当前页面上哪个候选最可能对应目标，证据是否足够？ | 版本化 Observation（WASM/DOM/AX/截图）+ 目标 → 有序候选/分数/来源/abstain | 决策接口离线可回放；运行服务可持明确版本的只读规则树/权重索引/模型，推理不写经验。更新经 T-12 版本化产物；各算法与 VLM 单独计时评估；Browser 留作采集适配器。固定 Snapshot fixture/replay，无需启动浏览器 |
| **学习沉淀** | 哪次决策/动作为什么成败，什么经验能安全复用？ | 决策、派发、命中、页面后验与纠正 → append-only Episode、评估集、版本化产物 | 原始 Episode 与标签分开；`positive / negative / unknown` 可追溯；敏感字段脱敏；离线 shadow/replay、人工/规则审核、晋升与回滚。未经门禁不写在线行为 |

**共享契约（先定语义，再决定是否改 proto 或目录）**：沿 `run/task → step → intent → attempt` 关联；Observation 有 schema/出处；Decision 记录候选来源、排序、置信与耗时；Execution 分开记录派发状态、物理结果、`hit_element` 与页面后验，任一项都允许 unknown；Episode 引用上述记录及标签来源。`SUCCEEDED` 不自动变正样本，取消或因果不清标未知。proto 只承载确有跨进程消费者需要的语义。

**基线指标（先测再定阈值）**：调度看每层预算/取消/重复副作用；算法看分站点与交互类型的阶段延迟、Top-k 与落点相关性、Probe/WASM 降级、VLM fallback 占比；学习看样本来源与标签质量、跨站点失败复发/恢复复用、holdout 评估和回滚。样例先取公开页面与本地 fixture，按站点隔离 holdout，不用真实账号样本。现有代码顺序“matcher 在前、VLM 在后”不等于快路径有效；当前命中率与 fallback 占比尚无可靠基线，视为待测假设。

## 5. 解耦现状：已有边界 vs 边界缺口

| 边界 | 当前证据 | 判断 / 请第三方重点审查 |
| --- | --- | --- |
| 应用与浏览器核心 | Gateway 是独立进程并经 Engine gRPC；`go list -deps ./cmd/uws-gateway` 未含 browser/engine | 已有可用进程边界。查其他入口是否把核心库直接嵌进应用，是否应统一到稳定协议 |
| Python 协调器与 Go Engine | `coordinator` 经 EngineClient/gRPC；proto 有双语言 bindings | 在线任务层与浏览器运行时的主要解耦缝。两边仍各持规划/重试/恢复/证据策略，需逐项指定唯一所有者 |
| 算法与浏览器 | matcher/classifier 核心包可离线调用；`pkg/browser/probe.go`、`tier2.go` 直接调 matcher，Tier2 CLI 起浏览器 | 算法包本体干净，但实验/采样适配器与算法职责混合。先抽离离线输入输出契约，不先做大目录搬迁 |
| WASM 探针与 Go | `pkg/probe_wasm` 是 C++/WASM；Go/JS 宿主在 `pkg/browser/probe.go` 经 CDP 注入，`make probe-wasm` 构建，`make install` 含该目标 | **已接入运行时，不是僵尸资产**。保留候选采集；审查 ABI/序列化契约、注入失败降级、构建与发布产物一致性 |
| 重试与恢复策略 | `coordinator/reflector.py` 管动作级 deadline/abort/reinit/replan；`pkg/engine/vlm_fallback.go` 有受限 VLM 多轮；`pkg/browser/{click_recovery,input_reliability}.go` 管物理重试；Engine 还管 intent 去重、幂等结果、SPA 稳定交接 | **多个重试层但不是同一策略**。分别拥有任务重试、动作内感知回退、物理派发重试、传输去重、会话状态；为同一操作关联 attempt/错误原因，以单调递减 deadline 控总耗时，保留各层局部上限与禁重放规则。现有证据不足以仅凭重叠断言「确定性震荡」或指数级追踪成本 |
| QA 与浏览器 | `pkg/qa.BrowserSession`/`SessionAdapter` 提供接口；Runner 可直建 CDP session；`pkg/provider` 已改用自有 `ViewportSpec`；`pkg/webcontrol` 仍路由 status/QA/Viewer/circuit reset | Provider → QA 是证据用例消费领域服务，非已证实的倒置；后续关注类型所有权与测试边界。WebControl 是多能力本地控制面，非静态站点，可用窄端口减弱耦合 |
| 学习与在线运行 | `learning/shadow_eval` 用 snapshot store/priors；`pipeline` 有 Trace/WSTN/训练；难点盘点称在线 learned 门禁未闭环 | 暂按旁路研究能力画图；核实是否有其他线上消费者、生成物协议与晋升门禁 |
| Engine 聚合 | `pkg/engine/service.go` 导入 browser、challenge、classifier、fingerprint、knowledge、matcher、vault、verifier、viewerhub、vlm | Engine 很可能同时是 composition root、领域策略与 RPC 实现。可拆内部领域服务，但保留单进程/单接口作渐进方案 |
| 运行态状态所有权 | Engine 持 context/session/Target 绑定、intent 幂等缓存与在途去重、页面 verifier 状态、breaker、互斥槽位、vault resolver；Browser session 持 CDP 连接、窗口与物理输入状态 | Engine **不是无状态动作驱动器**。可收走高层规划/语义重试，但必须保留会话、安全、互斥、去重与页面生命周期不变量，并写成明确契约 |
| Browser 包 | `pkg/browser` 同时含 CDP、Target/session、动作、AX/probe、指纹/profile、Viewer bridge、Tier2 | 包边界按机制、状态所有权还是运行阶段划分尚不清楚；以变化原因与可独立测试接口为拆分判据 |
| 文档事实源 | `GLOBAL.md` 兼作必读地图与长期裁决账本；`Goals/Tasks/handoff` 三件套刚建立；目录说明有过期项 | 第三方应评估地图、低频架构决策、历史证据、运行态状态的单点归属，而非只给文件搬家建议 |

### 重试预算与 Intent 生命周期：建议吸收后的修正

| 提案点 | 可吸收的核心 | 需修正处 |
| --- | --- | --- |
| 分层预算 | 为 Coordinator 语义恢复、Engine 单动作 VLM 轮次、Browser 输入/点击恢复分别设可观测预算；沿链传播统一 deadline/cancel，关联步骤与 attempt | `B_goal≤3 / B_vlm≤2 / B_input≤3` 非当前实现事实。三层上限不能相乘当总耗时；以 deadline 为全局硬边界，局部上限只说明该层可用次数 |
| intent_id | 保留 Engine 在途去重与短期结果回放；同一请求因传输超时重发时复用同 ID | 语义重规划/再做一次动作必须分配新 intent ID 并关联 task/step + 新 attempt；复用旧 ID 会被 60s 缓存当同一操作。幂等键是请求去重键，非任务生命周期 ID |
| 状态模型 | 明确请求接收、执行、页面效果评估、最终结论，以及可观测 attempt 记录 | 当前无跨重启持久 Intent 状态机；60s map 是运行期去重缓存。`Evaluated` 不是清楚终态；需表达结果未知/客户端取消/挑战阻断，并定义终态不可变与副作用动作重放规则。物理派发 success、`hit_element`/`local_verification`、Coordinator `SUCCEEDED` 语义不同，不能塞进一个成功终态 |
| Verifier 状态 | 明确页面后验状态的所有者、目标范围与销毁条件 | 现有 SPA verifier 以 Engine 的 Target/page 为范围；`NAVIGATING` 可跨 RPC 留给下次动作稳定化，不能缩成单 intent 结束即销毁 |
| 执行指标 | 若调用方确要诊断，可暴露按层统计与关联 ID | proto 追加字段 wire 兼容但有成本；成功响应字段覆盖不到 gRPC error。先定消费者、计数口径与错误响应承载，再定字段 |
| Provider / QA | Provider 对外类型不应把内部 QA 细节扩散给外部 | 真实泄漏点是 Provider 请求暴露 `qa.Viewport`/`qa.BrowserSession`；改为 Provider 自有 DTO，SessionOverride 留在内部适配层。`ActionSession` 是浏览器动作接口，不匹配当前 Inspect/Diff 用例 |
| WebControl | 用窄端口注入 QA、Breaker reset、Viewer input/Hub 可减少具体包耦合 | `pkg/webcontrol` 实际提供 status/QA/Viewer/reset API 与 WebSocket 流，非静态站点。切接口可做，删后端职责/重写静态服务不成立 |
| WASM 探针 | 保留 WASM 资产并增强边界回归 | Go/JS 宿主经 CDP 在线注入已确认；校验重点是 C++/WASM 序列化布局、Go 解码、页内桥接、注入失败降级，而非抽象 JS↔Go 单测 |

状态化 Matcher/Verifier/Challenge 的建议应落到各自状态的生命周期与所有者：Matcher 可原子替换规则快照；Verifier 是按页面/Target 的短期时序状态；Engine 防乱点熔断与 Gateway 站点认证/挑战锁也不是一个状态。无需为形式一致强行共享类型或统一状态机。

## 6. 新需求的落点路由（避免临时跨层改动）

| 新需求改变什么 | 首选所有者 | 必须经过的边界 / 验收 | 常见越界信号 |
| --- | --- | --- | --- |
| 目标解析、步骤规划、反思/任务恢复 | `coordinator/` | EngineClient + proto；Python 单测，必要时 gRPC 集成 | 在 Browser/Engine 重写任务计划或 VLM 反思 |
| 页面动作/API、动作状态/回执 | `proto/`（契约）+ `pkg/engine/`（策略）+ `pkg/browser/`（执行） | 先定语义所有者；proto-gen 原子更新 Go/Python；同一 ctx/deadline 到达每个物理派发；区分派发前取消 / 派发后未知 / 页面后验 | 只加 bool/字符串却未定义语义、幂等与错误情况；VLM 坐标回退绕开取消分类 |
| 多层重试、回退、恢复 | `coordinator/`（任务步/反思）、`pkg/engine/vlm_fallback.go`（单动作视觉解算）、`pkg/browser/`（物理调用与遮挡自愈） | 为一次 intent 画出触发条件、次数、预算、错误分类与终态；物理重试不替代语义重规划，语义重试不重复支付/发布等非幂等操作 | 把所有「重试」搬给一层会丢动作幂等/物理输入就绪/局部视觉感知 |
| DOM 候选探针/WASM 行为 | `pkg/probe_wasm/` + `pkg/browser/probe.go` | serialized node/structural hash 布局保持 Go/C++/页内 JS 一致；native/WASM/真实 CDP 三种测试分层 | 只搬走 C++ 目录会切断 browser 在线候选通道；不能因「目录内无 Go 文件」判未接线 |
| AX/DOM 候选或排序算法 | `pkg/matcher/` 或 `pkg/classifier/` | 纯输入/输出、离线 fixture、基准；Browser 仅适配页面快照 | 算法包开浏览器/读 CDP；tier benchmark 与生产路径共用隐藏状态 |
| 页面动作完成判据/恢复策略 | verifier/Engine/coordinator 之一（先定唯一事实源） | 明确预期状态、观测源、超时、不可重放动作；单测 + 必要公开页验收 | 多层各自推断成功；重试与反思隐式触发 |
| 站点挑战 / Gateway 登录状态 | challenge（页面信号）或 Gateway auth policy（产品认证状态） | 分别定义输入信号、状态持久化、复位授权与 fail-closed | 用一个全局 challenge 状态混合页面拦截与站点未登录 |
| 浏览器身份/Profile/Cookie/租约 | `pkg/fingerprint/` + browser 集成；凭证归 vault | profile/state/lease 独立契约；同档案、TLS、导入导出与并发测试 | Cookie/凭据流经 Coordinator/日志/共享泛化 Session |
| Viewer 新交互/状态 | Go ViewerBridge/Hub + Swift ViewerCore | socket schema 与焦点/坐标语义双端测试；公开场景真机验收 | 单边改客户端或把窗口管理塞进 Browser 动作策略 |
| QA 新扫描器/报告能力 | `pkg/qa/`；浏览器消费走窄 interface；产品步骤归 QA Studio | 离线 fixture + provider contract + 报告证据；真实页只验必要集成 | 扫描规则直写 `pkg/browser`，或把商业流程塞进通用引擎 |
| Trace 学习/规则自动更新 | `pipeline/` → `learning/`；在线接入另立版本接口 | 样本来源、标签可信度、影子基准、晋升阈值、回滚齐全 | 「跑过一次」或 `SUCCEEDED` 直接写入在线 learned |

## 7. 第三方审查问题（请输出依据、风险与最小渐进方案）

1. 按变化原因划分，当前产品/运行时/算法/数据四类边界各是什么？哪些目录位置与实际职责不符？请引用具体调用或导入证据。
2. `coordinator`、Engine、Browser 职责是否唯一？规划、重试、幂等、恢复、命中证明、页面后验判断分别该由哪层拥有？
3. `pkg/engine` 与 `pkg/browser` 中哪些依赖构成高扇入/高扇出或循环式概念耦合？建议的最小接口/数据契约切口在哪？
4. Gateway、Viewer、QA Studio 是同一平台的客户端/应用，还是独立应用组合？哪些确实应共享？
5. `pipeline/`、`learning/` 哪些是可运行能力、研究工具、未接线设计或历史残留？请逐条给证据，不据文档愿景推断。`probe_wasm/` 已有 `pkg/browser/probe.go` 在线宿主适配，请审查其测试与交付韧性，别当作未接线资产。
6. 哪些契约必须成为机器可检查的单一来源（proto、Viewer socket、Profile/StateBundle、QA Evidence、Trace/learned 资产）？如何防生成物/注释/实现漂移？
7. 若不拆仓、不重写，建议的 3 个渐进解耦批次是什么？每批边界、前置条件、消费者迁移、兼容策略、验收与回滚各是什么？
8. 哪些旧目标、重复实现、空跑门禁、失效文档或归档代码在增加新需求成本？请区分安全删除、需兼容保留、仅需改索引三类。
9. Hub、WebTouch Coordinator、Engine、Browser 的计划/恢复/重试/去重分别归谁？是否能用独立假引擎覆盖各自状态转移？
10. Probe/WASM、matcher、classifier/WSTN 当前的命中、耗时、降级与 VLM 调用比例是否可观测？离线快照能否不启动浏览器独立回放？
11. 哪些证据足以给 Episode 标正/负/未知？恢复经验如何复用、评估、晋升与回滚，且避免未证实失败污染在线规则？

## 8. 阅读路径

- 先看 §1–§3 建立结构，再看 §4 数据流、§5 解耦证据，最后按 §7 给结论。
- 目标优先级看 [`../Goals.md`](../Goals.md)；当前要做什么看 [`../Tasks.md`](../Tasks.md)；验证当日运行事实看 [`../handoff.md`](../handoff.md)。
- 改代码前按 [`../AGENTS.md`](../AGENTS.md) 与 [`../GLOBAL.md`](../GLOBAL.md) 选模块入口；本文不取代模块规格、测试矩阵或真实浏览器验收。
- 想知道「活规范承诺的东西是否真在代码里」看 §9；想按站点推进可用性看 §10。

---

## 9. 文档 ↔ 代码对齐矩阵（`T-14` · 2026-09-24 只读审计）

> **性质**：每一行是「活规范承诺 ↔ 代码锚点 ↔ 测试/现场证据 ↔ 判定」四元组，判定口径见 §9.0。
> **范围**：`docs/README.md` 五域矩阵点名的 17 份活规范（01-Bus 3 / 02-Organ 4 / 03-Engine-Alg 5 / 04-Learning 2 / 05-QA-Studio 3），外加六条红线与不变量的锚点复核（§9.2）与反向无主能力清点（§9.3）。
> **纪律**：本节的每一格都必须能被独立复现（文件:行号 / 测试名 / 可复跑命令）；不接受「报告自述」作为接线证据。

### 9.0 判定口径（四态）

| 判定 | 含义 | 处置 |
| :--- | :--- | :--- |
| `一致` | 文档承诺在代码里有可复现锚点，且有测试或现场证据 | 不动 |
| `文档滞后` | 代码已实现或已改口径，文档仍写旧状态 | 改文档 |
| `补规范` | 代码有真实能力，却无活规范认领（含地图漏列代码落点） | 补规范锚点 / 补地图列 |
| `未接线` | 文档承诺无代码落点，或有代码但无任何测试与证据 | 补代码 或 降级标注 |

### 9.1 五域逐规范矩阵

#### 01-Bus（个人 AI 互动总线）

| 活规范 | 核心承诺 | 代码锚点 | 测试 / 现场证据 | 判定 |
| :--- | :--- | :--- | :--- | :--- |
| [`gateway-contract-2026-08-26.md`](gateway-contract-2026-08-26.md) | 九类端点、强制通道前缀 `/v1/{channel}/…`、`x-uws-*` 扩展头、Markdown-First 隐蔽动作 polyfill | `apps/gateway/handler.go:42-61`（端点分发）、`:67`（`forcedChannelPath`）、`:253`（`handleChat`） | `apps/gateway/{handler,responses_api,authstatus,chat}_test.go`、`engineclient_test.go` | `一致` |
| [`agent-interaction-contract-2026-09-23.md`](agent-interaction-contract-2026-09-23.md) | 快速模式 / Agent 主控模式；`run --observe --session` 只读且 fail-closed；AX-first；页面侧后验 | `coordinator/cli.py:101`（`--observe`）、`:268-277`（不得混入动作）、`:334`（`require_existing_session`）、`:369-402`（空白 Target 不当续接成功） | 现场五步闭环 `docs/evidence/pilots/dogfood-live-2026-09-23.md` | `文档滞后`：§验收「未通过项」仍写「回执不回显命中节点」，实际 `extract` 已回显（`pkg/engine/evidence_test.go:118` 绿），抽取门禁已落 `pkg/browser/cdp_session.go:2546`（内容节点入池 + extract 取末位） |
| [`api-reference.md`](api-reference.md) | Engine gRPC 每个 RPC 的字段与错误语义成节 | `pkg/engine/service.go:324/582/641/696/1325/1354/1477/1883/1908/1936/1953`（11 个 RPC 全部实现） | `pkg/engine/*_test.go`（22 个测试文件） | `文档滞后`：缺 `ExportPageTree`(v1.5.5)、`ExportState`(v1.5.6) 两节，仅 §0 修订史提及；已补（见 §9.4） |

#### 02-Organ（免打扰桌面执行器官）

| 活规范 | 核心承诺 | 代码锚点 | 测试 / 现场证据 | 判定 |
| :--- | :--- | :--- | :--- | :--- |
| [`fingerprint-browser-engineering-blueprint-and-checklist.md`](fingerprint-browser-engineering-blueprint-and-checklist.md) | 六层纵深防御；Preferences 层封 WebRTC；原生原型链 Hook；硬件/Client Hints 自洽；确定性渲染噪声；State Bundle + 租约；零干扰 Viewer | `pkg/fingerprint/{profile,hook,registry,state_bundle,lease}.go`；`pkg/browser/cdp_session.go:254/276/308`（`--disable-translate` + `translate.enabled=false` + `ip_handling_policy`）；`pkg/fingerprint/profile.go:364/378/396`（WebRTC 策略、泄漏警告、`socks5h` 归一化） | `pkg/fingerprint/*_test.go`、`pkg/browser/{webrtc_pref,fingerprint_audit,fingerprint_phase3}_test.go`、`scripts/fingerprint_audit.sh`、`make probe-test` | `一致`（「严禁 `--headless=new`」属部署约定：代码只有 `pkg/browser/cdp.go:425` 的 headless 状态探测，无「拒绝 headless 启动」断言） |
| [`fingerprint-and-viewer-final-spec-2026-09-12.md`](fingerprint-and-viewer-final-spec-2026-09-12.md) | 指纹档案契约、Hook 规范、身份注册表、State Bundle 契约 + 单写者租约、Viewer S5 双轨收回 | `pkg/fingerprint/state_bundle.go`、`pkg/engine/service.go:582/641`（`ExportState`/`ImportState`）、`pkg/fingerprint/lease.go`、`pkg/browser/lease_factory.go`、`cmd/uws-lease/`、`pkg/viewerhub/` | `state_bundle_test.go`、`lease_test.go`、`registry_test.go`、`pkg/engine/viewer_test.go` | `一致` |
| [`viewer-requirements-2026-09-11.md`](viewer-requirements-2026-09-11.md) | 硬性要求「绝不抢系统焦点」；交互规格；分期验收 | `cmd/viewer/Sources/ViewerCore/ViewerPanelController.swift:68/74`（`.nonactivatingPanel`）、`:103`（`becomesKeyOnlyIfNeeded=false`）、`:487/496`（`canBecomeKey=false`） | `cmd/viewer/` 下 13 个 Swift 测试文件；`docs/evidence/viewer/*` | `一致`（S4/S5 真机取证仍缺，见 §9.3 缺口） |
| [`../spec/physical-patches.md`](../spec/physical-patches.md) | `InitializeContext` RPC、`CHALLENGE_INTERCEPTED` 状态码、外部破解回调契约、WASM 截断补偿 | `proto/uws_service.proto:46`、`:245`（`CHALLENGE_INTERCEPTED = 4`）；`coordinator/solver_callback.py`；`pkg/browser/probe.go:712`（`compensateProbeCandidates`） | `tests/security/test_solver_callback.py`、`pkg/browser/probe_test.go`、`pkg/engine` 的 `wasmTruncTotal` 计数 | `一致` |

#### 03-Engine-Alg（确定性引擎与决策内核）

| 活规范 | 核心承诺 | 代码锚点 | 测试 / 现场证据 | 判定 |
| :--- | :--- | :--- | :--- | :--- |
| [`tier1-candidate-quality-2026-09-23.md`](tier1-candidate-quality-2026-09-23.md) | 可复跑 tier1 管线（生成器→runner→judge）+ recall@k/MRR/分段耗时 | `benchmarks/tier1/{drift_set,run_tier1,malhotra_judge,bench_candidate_quality}.py`、`cmd/tier1/main.go` | 文档 §2/§6 实测数字；§8 红→绿变异三处；产物 `build/tier1/*.json` 不在仓（需先生成） | `一致`（合成集基线；真实站点语料按其 §3「未开始」自述未采集） |
| [`algorithm-assets-inventory-2026-09-23.md`](algorithm-assets-inventory-2026-09-23.md) | 每条资产标注真实/合成、标签来源、站点覆盖与 holdout、可离线复现 | 表内逐行：`pkg/classifier/testdata/golden.json`、`benchmarks/bench_*.py`、`pkg/probe_wasm/`、`benchmarks/tier2/` | 逐行复核成立：`bench_matcher/bench_gate/bench_llm_fallback` 确是 `pytest.skip` 骨架；`bench_wstn*` 自带 disjoint holdout | `一致`（n=8 golden 与「1000 步黄金语料」缺失已在文档自述） |
| [`scheduling-trace-2026-09-23.md`](scheduling-trace-2026-09-23.md) | 四层调度所有权、关联键、引擎状态转移表、预算矩阵 | `pkg/engine/service.go:430`（`--mutex-key`）、`pkg/engine/session_store.go`、`pkg/engine/service.go:1523`（`noteNotFound`）、`coordinator/executor/` | `pkg/engine/scheduling_test.go`（4 例）、`interrupt_test.go`、`tests/integration/test_coordinator.py` | `一致` |
| [`wstn-algorithm-task-spec.md`](wstn-algorithm-task-spec.md) | 冻结 `wstn.route.v1` 契约；Tier0/1/2 三阶实验 | `spec/wstn-routing-v1.schema.json:24`、`pipeline/wstn.py:27`、`pipeline/wstn_state.py`、`benchmarks/bench_wstn*.py`、`benchmarks/tier2/` | `tests/unit/test_wstn.py:72`、`test_wstn_state.py`、`benchmarks/reports/EVAL-wstn-*.md` | `一致`（M5 真实 CDP 录制与生产 A* 替换按文档自述未完成） |
| [`../spec/algorithm-kernel.md`](../spec/algorithm-kernel.md) | 数据模型（`uws_rules`/`site_synonyms`/`online_logs`）、决策阈值、影子评估毕业、PII/预算/指标 | `pipeline/exporter/exporter.py:25`、`pkg/engine/logstore.go:28`、`pkg/knowledge/knowledge.go`、`pipeline/watchdog.py:17`（`OFFLINE_FUSE_RATIO=0.80`）、`pkg/engine/service.go:51`（`AutoRollbackThreshold=50`） | `tests/security/test_pii_redaction.py`、`tests/unit/test_watchdog.py`、`pkg/engine/m5_integration_test.go:399`（TDD-KB-06 自动回滚）、`kb_r1_test.go:137` | `未接线`（局部）：§8.2 Prometheus 五指标无导出通道（`promhttp` 零命中），仅有 `wasmTruncTotal` 原子计数器——用户 2026-08-20 已定案「不引入 client 依赖」，属有意后置，改文档标注 |

#### 04-Learning（经验沉淀与正负闭环）

| 活规范 | 核心承诺 | 代码锚点 | 测试 / 现场证据 | 判定 |
| :--- | :--- | :--- | :--- | :--- |
| [`knowledge-ops.md`](knowledge-ops.md) | 版本查询/健康检查、dry-run 预检 + 原子热更、三阶段灰度、P0 自动回滚、面板指标 | `pkg/engine/service.go:1908/1936/1953`、`pkg/knowledge/knowledge.go:179`（`Reload`）/`:237`（`Rollback`）/`:261`（`AutoRollback`）、`pkg/knowledge/httpfetcher.go:121` | `pkg/knowledge/{knowledge,httpfetcher}_test.go`、`pkg/engine/m5_integration_test.go:399`、`kb_r1_test.go:137` | `未接线`（局部）：§6 Grafana/PromQL 面板同 §8.2，无指标导出通道 → 改文档标注 |
| [`../spec/knowledge-versioning.md`](../spec/knowledge-versioning.md) | artifact 布局 + `manifest.json` 契约、双缓冲原子切换、兼容/内存闸、灰度与回滚 SOP | `pkg/knowledge/httpfetcher.go:46-153`（manifest + SHA256 校验）、`knowledge.go:389`（`swapLocked`）、`:400`（`memoryOK`）、`:410`（`sanityOK`）、`:418`（`compatible`） | `knowledge_test.go`（Reload/Rollback/AutoRollback 各路径）、`httpfetcher_test.go` | `一致`（灰度金丝雀是运维 SOP，本就不要求代码；文档已如此定位） |

**域级结论（更正一处既往误判）**：`PARKED` 的定义是「接口冻结、**测试保持绿灯**、暂停在线装配」。本批实跑证明 `learning/`（4 个模块）与 `pipeline/`（13 个模块）**全部被 `tests/` 直接 import**，其中 `tests/integration/test_pipeline_learning.py` 实跑 **9 passed**（TDD-LEARN-01/02/03 + PIPE-03/04/05/06 + WATCH-01）。因此「`learning/`、`pipeline/` 零测试，与 `PARKED` 冲突」的说法**不成立**：目录内无测试文件是事实，但测试落在 `tests/{unit,integration}`，且是绿的。唯一未被任何测试 import 的是 `pipeline/topology.py` 与 `pipeline/trace_distiller.py`，二者均被 `wstn.py`/`wstn_state.py` 间接消费。

#### 05-QA-Studio（Vibe Coding 交付验收层）

| 活规范 | 核心承诺 | 代码锚点 | 测试 / 现场证据 | 判定 |
| :--- | :--- | :--- | :--- | :--- |
| [`qa-studio/README.md`](qa-studio/README.md) | Level 1 无头体检、Level 2 视觉差分与显微原语、Level 3 规格驱动走查（受控）、Level 4 `HOLD` | `cmd/uws/qa.go`、`pkg/qa/{runner,reporter,types,viewport}.go`、`pkg/qa/reporter.go:78`（`ComputeDiff`）、`pkg/vlm/eyetools.go:33`（`DiffImages`）、`pkg/webcontrol/server.go` | `pkg/qa/*_test.go`（4）、`pkg/vlm/eyetools_test.go`、`cmd/uws/qa_test.go` | `一致`（L1/L2 已接线；L3 受控、L4 `HOLD` 与文档一致） |
| [`qa-studio/delivery-readiness-requirements-2026-09-15.md`](qa-studio/delivery-readiness-requirements-2026-09-15.md) | Requirement Registry：逐项阻断 + 证据等级 + 文件所有权 | 逐项阻断多为运营/法务项（无代码）；技术侧落 `pkg/provider/`（EvidenceBundle provider） | `pkg/provider/provider_test.go` | `补规范`：`pkg/provider` 是 05 域实现，但 `docs/README.md` 05 行只列 `pkg/qa/`、`pkg/webcontrol/` → 补地图列 |
| [`qa-studio/commercial-validation-execution-plan-2026-09-14.md`](qa-studio/commercial-validation-execution-plan-2026-09-14.md) | 门禁日历、统一漏斗、定价实验、ADVANCE/HOLD/STOP | §9「最小技术合同」是唯一技术承诺：动作护栏 → `pkg/challenge/circuit_breaker.go` + `coordinator/guard.py`；循环预算 → `coordinator` `max_steps` + Fibonacci 退避 | `tests/integration/test_coordinator.py`、`pkg/engine/scheduling_test.go` | `一致`（计划本体是运营文件，整体 `HOLD`；其技术合同有锚点） |

### 9.2 红线与不变量的代码锚点（`GLOBAL.md` §3 六条）

| 红线 | 代码锚点 | 守卫测试 / 证据 |
| :--- | :--- | :--- |
| R1 零打扰（不抢焦点） | `cmd/viewer/Sources/ViewerCore/ViewerPanelController.swift:68/487/496` | `cmd/viewer/` Swift 测试；`docs/evidence/viewer/*` |
| R2 防乱点熔断 + 只读豁免 + 只锁触发的真实账号页 | `pkg/challenge/circuit_breaker.go:129`；`pkg/engine/service.go:870`（`readOnlyAction`）/`:907`（放行）/`:1123`（不计数） | `pkg/challenge/circuit_breaker_test.go`、`pkg/engine/challenge_test.go:181`（extract 的 no_match 不触发熔断） |
| R3 点击自证与后验（`SUCCEEDED` ≠ 命中） | `proto/uws_service.proto`（`hit_element`）；`pkg/engine/service.go` 动作前 URL 快照 | `pkg/engine/evidence_test.go`（含 `:118` extract 回显）、`node_direct_test.go:75` |
| R4 Vault `fail-closed` | `pkg/vault/vault.go:174` | `pkg/vault/vault_test.go` |
| R5 互斥会话与续页（24h） | `pkg/engine/session_store.go`；`pkg/engine/service.go:430` | `pkg/browser/session_resume_test.go`、`pkg/engine/mutex_test.go` |
| R6 规范先行 | 本节即执行面 | `scripts/{audit-layers,consumer-boundary-check,proto-check}.sh` 本批复跑 rc=0 |
| 不变量：`semanticRe` 白名单无 ASCII `/` | `pkg/engine/sanitize.go:28` | `tests/security/test_input_sanitization.py` |

### 9.3 反向清点：代码有、活规范未认领

| 代码单元 | 现状 | 处置 |
| :--- | :--- | :--- |
| `pkg/contract/` | Engine 通信抽象契约，只被本图点名，**0 测试** | 登记缺口：或补最小契约测试，或明确它只作接口容器 |
| `pkg/verifier/` | 自身 doc 声明对应 `spec/algorithm-kernel.md §5`，但该 spec 未点名包；仅本图与 `contributing.md` 提及 | 补规范锚点（algorithm-kernel §5 补 `pkg/verifier/`） |
| `pkg/vault/` | 由 `GLOBAL.md` §3 红线 4 与 `tdd-checklist` 认领，无活规范章节 | 补规范（建议归 02-Organ 或单列安全节） |
| `pkg/classifier/`、`pkg/probe_wasm/` | 03 域实现（ONNX 打分、WASM 探针），地图 03 行未列 | 补地图列（已补，见 §9.4） |
| `pkg/provider/` | 05 域实现（EvidenceBundle provider） | 补地图列（已补） |
| `cmd/tier1`、`cmd/tier2`、`cmd/uws-gateway`、`cmd/uws-viewer-demo`、`cmd/uws-engine` | 入口型二进制，无自有测试 | 可接受：逻辑在 `benchmarks/tier1`（Python judge）、`benchmarks/tier2`、`apps/gateway`、`cmd/viewer`、`pkg/engine` 侧被测；改入口须同步两侧 |
| `cmd/viewer`（Swift） | 13 个 Swift 测试文件，但不在 Go/Python 测试口径内 | 缺口：S4/S5 真机取证仍缺（`docs/evidence/viewer/` 未覆盖到当前 HEAD） |

### 9.4 本轮处置（只读审计 + 文档修正）

| # | 处置 | 落点 |
| :--- | :--- | :--- |
| 1 | 新增本节对齐矩阵与 §10 站点矩阵骨架 | 本文件 |
| 2 | 补 `ExportPageTree`(v1.5.5)、`ExportState`(v1.5.6) 两节 | [`api-reference.md`](api-reference.md) |
| 3 | 改写 §验收「未通过项」为当前状态（抽取门禁 + 命中回显已落码，附锚点） | [`agent-interaction-contract-2026-09-23.md`](agent-interaction-contract-2026-09-23.md) |
| 4 | 03 行补 `pkg/classifier/`、`pkg/verifier/`、`pkg/probe_wasm/`；05 行补 `pkg/provider/` | [`README.md`](README.md) |
| 5 | §8.2 Prometheus 指标标注「导出通道后置（用户 2026-08-20 定案）」 | [`../spec/algorithm-kernel.md`](../spec/algorithm-kernel.md) |
| 6 | §6 Grafana 面板标注同上 | [`knowledge-ops.md`](knowledge-ops.md) |
| 7 | 更正「`learning/`、`pipeline/` 零测试」误判为实测结论（17 模块全部被测，9 passed） | [`../Tasks.md`](../Tasks.md) `T-14` 缺口行 |

### 9.5 头部站点覆盖矩阵（`G2` · 12 站实采结果）

> **口径自决**：用户口径是「按二八定律先打通**访问量最高**的头部站点，使大部分常见网站工作良好」。据此确定为主轴=公开访问量榜单，副轴=交互类型（登录 / 搜索 / 表单 / 内容抽取 / 多步流程）；采集时**锁定榜单快照版本**并记录来源，不凭印象选站。
> **一票否决**：本矩阵每格的证据必须是**页面侧**事实（AX `[value=…]`、弹窗出现、URL/角标变化、`elementFromPoint` 命中目标、截图与 AX 一致）。`Status: SUCCEEDED` 与报告自述**不作为**通过证据。
> **失败层归因**（必须分层，禁止合成单一「成功率」）：`定位`（候选未召回 / 选错）｜`抽取`（extract 假阴性 / 假阳性）｜`派发`（SUCCEEDED 但页面无变化）｜`后验`（无页面侧判据）。

**样本与实采结果（2026-09-24 实采，12 站）**

- 主轴榜单快照：Tranco 逐日名次，快照日期 **2026-09-23**（查询 2026-09-24 16:39）；样本 = 全球 8 + 国内 4。
- 探针口径：标准语义词表 `input:search_box=webtouch`（只填不提交）；每站独立 `--session t14-<site>`，`nav:` 与探针**同一次调用**（防活动标签页漂移污染快照）。
- 逐站原始记录、复现命令与证据指针：[`evidence/pilots/site-coverage-2026-09-24.md`](evidence/pilots/site-coverage-2026-09-24.md)。

| 站点（Tranco 2026-09-23） | 可达 `nav` | 目标定位 `input:search_box` | 抽取 | 点击后验 | 失败层 | 证据 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `google.com`（1） | `PASS` | `FAIL`（`unknown_error`，URL 未变） | `NOT_RUN` | `NOT_RUN` | `定位` | 案卷 §3；ref 池内确有 `[271] textarea(combobox) "Search"` |
| `youtube.com`（9） | `PASS` | `FAIL`（`unknown_error`） | `NOT_RUN` | `PASS`（命中 `yt-formatted-string "Shorts"`；CDP 侧 `youtube.com/shorts/…`） | `定位` | 案卷 §3 |
| `amazon.com`（24） | `PASS` | `FAIL`（假成功：命中 `span "Sign in"`） | `NOT_RUN` | `NOT_RUN` | `后验` | 案卷 §3 |
| `github.com`（29） | `PASS` | `FAIL`（假成功：`span "Sign in"`；**AX 树不可用**：CSP 拦 in-page WASM） | `NOT_RUN` | `PAUSED`（熔断 OPEN） | `定位` + `取证通道` | 案卷 §2 E5/E7 |
| `wikipedia.org`（30） | `PASS` | `FAIL`（`unknown_error`） | `NOT_RUN` | `PASS`（命中 `strong "English"`；CDP 侧 `en.wikipedia.org/wiki/Main_Page`） | `定位` | 案卷 §3 |
| `bing.com`（31） | `PASS` | `FAIL`（`unknown_error`） | `NOT_RUN` | `PARTIAL`（CDP 侧 `bing.com/images`；CLI 超时无回执） | `定位` + `工具链` | 案卷 §3 |
| `baidu.com`（87） | `PASS` | `FAIL`（`unknown_error`） | `NOT_RUN` | `PARTIAL`（CDP 侧 `news.baidu.com`；CLI 超时无回执） | `定位` + `工具链` | 案卷 §3 |
| `bilibili.com`（345） | `PASS` | `FAIL`（`unknown_error`） | `NOT_RUN` | `FAIL`（命中 `span "番剧"` 但未跳转） | `定位` + `后验` | 案卷 §3 |
| `taobao.com`（442） | `PASS` | `FAIL`（`unknown_error`） | `NOT_RUN` | `NOT_RUN`（首页即登录入口） | `定位` | 案卷 §3 |
| `duckduckgo.com`（471） | `PASS` | `FAIL`（`unknown_error`；`search:` 宏同源失败） | `NOT_RUN` | `NOT_RUN` | `定位` | 案卷 §3（含 FLAKY 与 AX 全名假成功两例） |
| `stackoverflow.com`（513） | `PASS` | `PAUSED`（`challenge_intercepted`；疑误判待复核） | `NOT_RUN` | `NOT_RUN` | `挑战感知` | 案卷 §3 |
| `zhihu.com`（1043） | `PASS`（首页即跳 `/signin`） | `FAIL`（`unknown_error`） | `NOT_RUN` | `NOT_RUN`（登录墙） | `定位` + 站点需登录 | 案卷 §3 |

其他来源（**非本轮实采**，仅作对照，回写时不得与本表混算）：`iana.org` 点击闭环 `PASS`（[`evidence/pilots/dogfood-live-2026-09-23.md`](evidence/pilots/dogfood-live-2026-09-23.md)）；`wikipedia.org` 搜索框定位 `PARTIAL`（真身秩 `{0:7,1:493}`，[`evidence/pilots/real-site-candidate-pilot-2026-09-23.md`](evidence/pilots/real-site-candidate-pilot-2026-09-23.md)）；`deepseek` / `gemini` 网页对话（网关侧，认证状态未确认）；`autodl`（熔断锁定，改密本体 `HOLD`，见 `Tasks.md` `T-4`）。

**本轮按失败层聚合的三条结论**

1. 主缺口在**定位层**：标准词表 `search_box` **0/12** 命中正确搜索框（9 站 `unknown_error`、2 站错元素假成功、1 站挑战暂停）；而 `nav` 12/12 可达、语义 `click` 实测 5 站中 4 站有页面侧变化 ⇒ 下一轮二八排序应先修**定位**，而不是泛化「成功率」。
2. **后验层缺门禁**：`input` 落到不可输入元素（`a` / `span` / 非目标 `button`）仍报 `SUCCEEDED`，报告缺「文本是否真进入可编辑元素」的页面侧判据（`GLOBAL.md` §3 红线 3 的判定精神尚未落到 `input`）。
3. **取证通道自身也是缺口**：大页面 ref 预算截断（8000 字符）使确定性兜底不可见、`node:` 编号不可跨快照、报告 `当前页面`/`Target ID` 可能与实际页错位、会话目标被外部销毁后不可自愈、CSP 站点 AX 树整体不可用 ⇒ 每格必须带「证据来源与可信度」标注。

**逐站采集模板（复跑命令）**

```bash
# 1) 同一调用内 nav + 探针（避免活动标签页漂移污染快照）
uws run --session t14-<site> --plan 'nav:<URL>⏎input:search_box=webtouch' --screenshot --output /tmp/t14-<site>-p1.txt
# 2) 定位是否命中：回执 hit_element 的 selector/text 必须与目标语义一致
# 3) 抽取是否真拿到内容：对比 AX 可见文本，检查假阳性（命中导航栏/搜索建议框）
# 4) 派发后是否有页面侧后验：URL 变化 / 角标 / 弹窗 / elementFromPoint 命中
#    注意：共享 9222 上报告「当前页面」段可能指向他会话标签页，须用 CDP /json/list 交叉核对
```

### 9.6 复跑命令（判定的可复现性）

```bash
scripts/proto-check.sh && scripts/audit-layers.sh && scripts/consumer-boundary-check.sh
gofmt -l pkg/ cmd/ apps/
go build ./...
go test ./pkg/... ./cmd/... ./apps/... -count=1          # 期望 18 包 ok / 0 FAIL
.venv/bin/python -m pytest tests/unit -q                  # 期望 158 passed
.venv/bin/python -m pytest tests/integration tests/security -q
```

## 10. 与 `T-11` ~ `T-14` 的关系

- `T-11A`（离线候选质量）与本站点矩阵的前两列（定位 / 抽取）共用同一套度量口径；站点矩阵补齐的是它们缺的**真实站点层**（带标签的真实站点快照语料）。
- `T-12`（Episode 正负闭环）与站点矩阵的「点击后验」列共用证据判据：无页面侧证据的样本一律记 `unknown`，不得进在线 learned。
- `T-13` / `T-14` 的结论已并入 §9；`T-14` 的下一步是 §9.5 的站点逐个采集（每站须有页面侧证据）。
