# WebTouch QA & Improvement Studio：Vibe Coding 时代的交付验收与体验打磨层

> **当前状态**：2026-09-16 商业决策升级：产品从单一 QA 扩展为 **Improvement（体验演进）**，确立以 **Local Skill + CLI + Remote API** 为主形态，引入 **Communicate 结构化沟通协议**。Level 1/2 可用于 URL-first 与本地真实案例交付；Level 3 仅随真实付费/强行为证据支持的关键链路受控推进；Level 4 保持 `HOLD`。商业与产品运营执行以[商业决策与信任架构](commercial-strategy-and-trust-architecture-2026-09-14.md) §15-16 及[商业与产品运营执行方案 v8](commercial-validation-execution-plan-2026-09-14.md)为事实源。
>
> **当前放行状态**：`DRY`、`PAID-CONCIERGE`、`PUBLIC` 均为 `HOLD`；首笔礼宾收款只由 `DRY + PAID-CONCIERGE` 阻断，不等待公开 URL intake。逐项阻断、动作和证据见[交付准备 Requirement Registry](delivery-readiness-requirements-2026-09-15.md)。

> **定位与愿景**：Vibe Coding Acceptance & Improvement Layer；连接 Builder/Coding Agent 与高质感、高转化、商业就绪的真实软件状态。
> **核心使命**：以独立 Judge + Improvement Copilot，把粗糙生成原型收敛为绑定物理事实与美学规范的 Acceptance Contract、就绪差距、质感提升提案与可直接交给 Agent 的修复指令。
> **对外宣传口径**：不仅告诉你离 Ship Ready 还差什么，更在本地终端/IDE 与你和 Coding Agent 交互沟通，一键将 AI 原型升级为商业级精致产品。

---

## 一、 行业剧变与商业核心痛点 (Market Drivers & JTBD)

```
┌────────────────────────────────────────────────────────────────────────┐
│                        AI 研发时代的生产力断层                          │
├──────────────────────────────────┬─────────────────────────────────────┤
│      代码生产端 (10x 爆发)       │        交付审核端 (1x 停滞)         │
├──────────────────────────────────┼─────────────────────────────────────┤
│ • Cursor, Claude Code, v0, Devin │ • 开发者依然手动打开 F12 控制台     │
│ • AI 辅助显著加快前端生成         │ • 手机模拟器反复切换断点找溢出      │
│ • PR 产出频率提高（待量化）       │ • 肉眼比对设计稿像素与间距          │
│                                  │ • 审核成为最大吞吐瓶颈与心智黑洞    │
└──────────────────────────────────┴─────────────────────────────────────┘
```

### 1. 行业断层：生成更快，验收仍重（待客户量化）
在 Vibe Coding 浪潮下，前端代码的产出速度被 AI 彻底重塑。然而，**交付验收依然极度依赖人类肉眼与手动操作**：
* 移动端视口是否有灾难性的横向滚动条？
* 控制台里是否躺着未捕获的运行时异常（Unhandled Promise Rejection）或裂开的静态资源？
* 按钮在深色模式下是否对比度失效或被浮动导航栏遮挡？
* 点击“确认支付”后页面是否真的跃迁到了成功状态？

**人类被迫沦为 AI 生成代码的“质检流水线人肉电池”**。

### 2. 用户雇佣 WebTouch 的终局任务 (Jobs-To-Be-Done)
用户雇佣 WebTouch QA Studio，不是为了看复杂的测试代码，而是为了：
1. **知道离可交付还差什么**：在 0→1 首次上线或 1→1.1 生产发布时，给出 Readiness Gap 与 Priority；
2. **把判断变成证据**：将多视口、视觉差分、运行时、交互链路、代码/配置和人工复核汇入同一 Evidence Protocol；
3. **让 Builder 可以继续收敛**：生成可直接交给 Coding Agent 的修复指令，修复后按同一 Contract 复验；
4. **留下可复核的交付凭证**：每条结论绑定 URL、版本、时间、环境和范围，条件变化后标记 `STALE`，不让旧结果冒充当前事实。

### 3. 当前首单体验契约：URL-first，但不偷换责任边界

- **执行材料最小化**：首 1–3 单只把经授权的公开 URL 与 Ship Moment 作为必填执行材料；页面路径、视口、参考图和版本标识可选，仓库与 Source Map 不必上传。
- **零安装交付**：客户无需安装 Skill、MCP 或 Plugin；先收到可读 HTML、最小机器可读结果和可复制给 Coding Agent 的修复指令。
- **不静默扩数据**：Runner 首轮不主动发现、额外抓取或留存 `.map`。只有真实付费任务证明精确源码定位会改变修复结果，且客户另行同意用途、范围、保留和删除时，才做一次有界增强实验。
- **人工复验而非假自动化**：首次交付可访问后 48 小时内可人工申请一次同范围复验；公开自动复验链接、Skill/MCP 自动闭环都等重复付费与真实交接摩擦再解锁。
- **状态口径不变**：运行事实是 `EXECUTED/NOT_RUN`；Judge 裁决是 `PASS/FAIL/BLOCKED/NEEDS_EVIDENCE`。`NEEDS_FIX` 只可作为面向人的摘要，不得取代机器四态。

---

## 二、 核心技术壁垒与架构哲学 (Architecture & Secret Sauce)

WebTouch QA Studio 以**“分层神经符号感知与差分证据分析”**为目标架构；计算成本、延迟和精度必须分别由真实项目与 goldset 验证。

对外部市场与客户，我们展现的是深厚的高精尖技术矩阵；而在系统底层，是一套极度优雅、确定性极高的执行底座：

```
┌────────────────────────────────────────────────────────────────────────┐
│                   WebTouch QA Studio 智能质检架构                      │
├────────────────────────────────────────────────────────────────────────┤
│ Layer 4: 独立 Judge 与 Acceptance Contract                             │
│ • 多源证据裁决 • Readiness Gap/Priority • 修复指令 • STALE 生命周期    │
├────────────────────────────────────────────────────────────────────────┤
│ Layer 3: 因果状态拓扑网 (Causal State Machine & WSTN)                   │
│ • 动作前状态 -> 交互触发 -> 动作后状态突变 • 0 Token 自动化链路验真    │
├────────────────────────────────────────────────────────────────────────┤
│ Layer 2: 差分张量与视觉显微原语 (Differential Tensor Inspection)       │
│ • 亚像素级色差矩阵 • 动态高倍显微放大 • 洋红高亮差异 Overlay 提取      │
├────────────────────────────────────────────────────────────────────────┤
│ Layer 1: 亚毫秒级物理不变量体检 (Sub-millisecond Invariants Engine)    │
│ • 官方原生 Chrome 物理沙箱 • 移动/平板/桌面三视口即时重放              │
│ • 横向溢出死锁检测 • 运行时 Console 异常穿透监听 • AXTree 焦点审查     │
└────────────────────────────────────────────────────────────────────────┘
```

### 1. 亚毫秒级物理不变量体检（Deterministic Invariants）
利用注入 V8 的亚毫秒特征探针与 CDP 事件监听，无死角扫描所有客观物理指标：
* **几何拓扑自适应检测**：自动注入 `390px`（iPhone）、`768px`（iPad）、`1440px`（Desktop）等标准视口，物理计算 `scrollWidth > clientWidth`，毫秒级判定响应式排版是否发生横向溢出崩溃；
* **运行时无死角拦截**：挂载原生事件总线，穿透监听 `window.onerror`、Promise 异常、CSS 资源缺失与 4xx/5xx 网络请求；
* **人机工效与可用性审查**：遍历可达性树（AXTree），对点击热区小于 $32 \times 32\text{px}$、文本对比度超标、Z-Index 遮挡的元素施加物理预警。

### 2. 高维差分张量分析与视觉显微原语（Eye Vision Inspection）
* **亚像素绝对差分矩阵**：基于纯 Go 原生实现的快速差分算子，无需大模型消耗即可完成两幅高分辨率画布的像素对比；
* **洋红高亮与 BBox 定位**：将变化区域精准框定（Bounding Box），生成直观的变动热力图；
* **局部高倍显微（Dynamic Zooming）**：针对微小间距与图标变形，动态提取 400% 局部放大图，为高层审查提供微观切片。

### 3. 因果状态转移闭环（WSTN Graph）
彻底打破传统测试脚本“瞎等 5 秒 sleep”的愚钝做法，根据页面 DOM 拓扑指纹构建状态转移网：
* 只有当点击动作切实引发了页面拓扑指纹的因果跃迁，系统才判定该交互成功；
* 自动记录“交互前状态 $\to$ 动作触发 $\to$ 交互后状态”的因果证据链。

---

## 三、 四级演进阶梯：从零配置体检到 PRD 智能审计

WebTouch QA Studio 采用渐进式证据模型。Level 描述的是内部可调用的 Evidence Provider 深度，不是对外产品线，也不改变独立 Judge 的责任：

```
Level 1: 零配置无头健康探针 (Smoke Test)
  │ 只需 1 个 URL ────► 检查溢出、控制台报错与网络死链
  ▼
Level 2: 视觉基准像素差分 (Visual Regression)
  │ URL + Figma 设计稿 ─► 自动洋红高亮差异、尺寸偏离、颜色漂移
  ▼
Level 3: 规格说明与状态机验证 (Spec-Driven Flow)
  │ URL + 交互 DSL ───► 自动走查关键链路、状态跃迁闭环、表单自愈
  ▼
Level 4: 需求意图驱动验收（待商业验证，HOLD）
  │ 经确认的 Markdown/结构化准则 ─► 一个关键需求的可复核运行证据
```

### Level 1: 零配置无头健康探针 (Zero-Config Semantic Health Probe)
* **输入**：仅需一个网页地址（例如 `uws qa --url http://localhost:3000`）；
* **行为**：完成多视口渲染、控制台异常捕获、横向滚动条检测与资源健康度分析；耗时按真实运行记录；
* **交付**：基础健康度评分与多端缩略图看板。

### Level 2: 视觉基准像素差分 (Pixel-Perfect Visual Regression & Diff)
* **输入**：网页地址 + 设计稿基准图或上一版截屏（`--ref-img figma.png`）；
* **行为**：自动对齐视口，执行像素级差分运算，计算色差张量并框定漂移区域；
* **交付**：带洋红高亮的视觉差异对照表与量化偏差率报告。

### Level 3: 规格说明驱动走查 (Spec-Driven Flow Verification)
* **输入**：网页地址 + 结构化链路配置或轻量 DSL（如登录、下单）；
* **行为**：利用 WSTN 状态转移网以确定性动作原语执行多跳交互，验证状态突变因果证据；
* **交付**：交互全链路回放记录与节点跃迁证据图。

### Level 4（待商业验证，HOLD）：需求意图驱动验收

当前只保留产品假说，不进入通用研发。若真实连接、重复使用和付费门槛通过，首个解锁范围也仅为：输入经人工确认的 Markdown/结构化验收准则，验证一个真实 PR 的一个关键需求，并输出可回放、可人工复核的证据。PDF、Word、Figma、白板解析和自主测试拓荒不在首个范围内。

---

## 四、 行业横向对比矩阵 (Competitive Matrix)

> 下表中的性能与体验描述是内部目标，尚未完成外部同口径验证，不得直接作为公开比较广告或采购承诺。

| 核心维度 | 传统人工手动走查 | 传统自动化脚本 (Playwright/Cypress) | 市面纯 LLM Agent (如 Browser-Use) | **WebTouch QA Studio** |
| :--- | :--- | :--- | :--- | :--- |
| **启动门槛** | 极低（人肉点击） | 极高（需手写维护大量用例与 Selector） | 中等（需配 Prompt） | **零配置**（一个命令即可开跑） |
| **执行耗时** | 待实测 | 待同口径实测 | 待同口径实测 | **目标：缩短首值时间，真实记录** |
| **单次成本** | 人工时间 | 服务器与维护 | 模型、浏览器与维护 | **目标：完整核算并随复用下降** |
| **抗改版鲁棒性** | 依赖人工经验 | 依赖选择器与用例维护 | 依赖模型与动作护栏 | **目标：以 goldset 与真实 PR 衡量** |
| **系统侵扰性** | 需要人工占用 | 取决于运行环境 | 取决于运行环境 | **目标：最小前台干扰，可审计** |
| **交付可信度** | 人工判断 | 断言与日志 | 模型结论与运行记录 | **四态结论 + 可复核证据 + 未覆盖边界** |

---

## 五、 商业化路径、定价策略与 GTM 蓝图 (Commercial Strategy)

> **唯一商业与产品运营执行事实源**：[`商业与产品运营执行方案 v8`](commercial-validation-execution-plan-2026-09-14.md)，以 v7 的 Concierge 关键路径为底稿完成 Map / Skill 产品门槛复审：首 1–3 单先走 `PAID-CONCIERGE`，执行材料坚持 URL-first、零安装，公开案例和陌生 URL intake 另过 `PUBLIC`；同时保留 Ship Moment/ICP、长期门禁、推广上线、验收、定价、指标、真实工期与研发解锁。
> **信任边界支撑文档**：[`商业决策与信任架构`](commercial-strategy-and-trust-architecture-2026-09-14.md)，只负责 Open Edge / Closed Intelligence、A/B/C 连接、数据政策与核心 IP 边界。
>
> **交付准备检查面**：[`交付准备 Requirement Registry`](delivery-readiness-requirements-2026-09-15.md)，137 项保留为全生命周期检查面，`CJR-01`～`CJR-13` 是首 1–3 单的唯一阻断清单；状态变化回写上方唯一执行方案。
>
> **API 架构与会诊契约规范**：[`Gateway Profile 与 VLM-Consult 架构规范 (2026-09-16，2026-09-17 修订)`](vlm-consult-and-scoped-endpoint-spec-2026-09-16.md)，定义 `/v1/gateways/{gateway_id}` 唯一推理入口、五类语义分工、协议兼容与执行兼容边界，以及 Local 持有执行权的 Tool-Calling 会诊闭环。
>
> **工程落地执行切片**：[`Fail-Closed 与 Cloud Direct 纵向切片执行计划`](fail-closed-and-cloud-direct-execution-plan-2026-09-16.md)，分仓置于 `HOLD`，优先完成 Fail-Closed、真实无头 Runner、复合稳定判据与首单演练。

当前首个可售实验是 **Ship Readiness Acceptance Job**：用户提交一个可访问 URL 与 Ship Moment，以 AI + 人工复核在目标 2 小时内获得受控结果，并可在首次交付可访问后 48 小时内人工申请一次同范围复验；实验价 US$99 只用于测试支付意愿。首单不要求 Source Map 或安装 Skill/MCP/Plugin。用同一 offer 比较“0→1 首次上线”与“1→1.1 生产发布”；Verified PR 保留为后者的一种证据丰富路径。Free、Pro、Team、Enterprise 只是长期包装假说，不是已定产品线。

对外优先使用“提交 URL，拿到 Ship Ready 的差距、证据和 Agent-ready 修复指令”。未经真实测量不得承诺“10 秒”“绝对确定”或“权威认证”。

---

## 六、 研发推进路线图 (Engineering Roadmap)

* **Phase 1 (当前 Milestone 3 起点)**：
  - [x] 实现 CLI 子命令 `uws qa` 及参数解析（`--url`, `--ref-img`, `--viewports`, `--output`）；
  - [x] 串接 CDP 视口快速切换与无头截图捕获管道；
  - [x] 集成 `pkg/vlm/eyetools.go` 像素 Diff 与洋红高亮输出；
  - [x] 生成标准 Markdown/HTML 交付报告（包含视口对比卡片与控制台健康度）。
* **Phase 2（当前首单真实性与交付闭环）**：
  - [ ] 消除无 Runner 假 PASS，Studio/CLI 接入同一真实执行链并失败关闭；
  - [ ] 冻结 Contract/Result、`EXECUTED/NOT_RUN`、Judge 四态与 `STALE` 生命周期；
  - [ ] 产品化零安装交付包：独立 HTML + 最小 JSON + 可复制修复指令；没有源码证据时不伪造文件名或行号；
  - [ ] 接入首单所需 URL/DNS/逐跳重定向硬阻断、单任务隔离与 48 小时人工复验 SOP。
* **Phase 3（仅在商业门槛通过后逐项解锁）**：
  - [ ] 真实付费任务证明 Source Map 会改变修复采用后，只做一次有界可选 Map 证据实验；
  - [ ] ≥3 个付费 Job、≥1 次复购/持续使用，并出现真实自动交接需求后，只原型化一种最小 Skill/MCP/Plugin 连接器；
  - [ ] 由真实任务受控解锁 `--spec` 单链交互走查（Level 3），不先通用化；
  - [ ] 仅当付费用户证明需求映射是购买或留存驱动时，实现经人工确认的 Markdown/结构化准则到 Acceptance Contract 的最小闭环；通用多模态 PRD 解析与自主探索继续 `HOLD`。
