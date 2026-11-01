# WebTouch 活规范与文档体系地图（Living Specs Map）

> **唯一事实源纪律**：文档是唯一事实源，代码只是文档规范的编译输出。
> 门禁规则见 [`../AGENTS.md`](../AGENTS.md)；顶层意志见 [`../Goals.md`](../Goals.md)；当期任务见 [`../Tasks.md`](../Tasks.md)；运行态见 [`../handoff.md`](../handoff.md)。
> 本文是 WebTouch 各业务域活规范（Living Spec）与代码实现的唯一聚合导航。

---

## 一、 五大核心领域矩阵（高内聚闭环）

WebTouch 体系按业务边界划分为五大高内聚领域。每个领域包含清晰的职责、代码落点、当前生效的活规范以及生命周期状态：

| 领域 | 核心职责 | 代码落点 | 当前生效活规范（Living Specs） | 状态 |
| :--- | :--- | :--- | :--- | :--- |
| **01-Bus**<br>浏览器模型 provider | Agent Hub 调用网页模型的站点适配、账号/Profile/指纹绑定、会话连续、登录探针、页内直发与 DOM 兜底、流式响应、站点风控保护；客户端统一模型 API 由 Agent Hub 承担 | `apps/gateway/`<br>`cmd/uws-gateway/`（源码目录；二进制 `webtouch-gateway`，`uws-gateway` 为弃用软链） | - [`gateway-contract-2026-08-26.md`](gateway-contract-2026-08-26.md)<br>- [`cli-session-contract-2026-10-31.md`](cli-session-contract-2026-10-31.md)（对外 CLI + 本机 session lease）<br>- [`cli-fetch-contract-2026-10-31.md`](cli-fetch-contract-2026-10-31.md)（匿名 HTTP 读取）<br>- [`agent-interaction-contract-2026-09-23.md`](agent-interaction-contract-2026-09-23.md)<br>- [`api-reference.md`](api-reference.md)<br>- [Agent Hub 边界规范](2026-10-03-agenthub-model-gateway-webtouch-boundary.md) | `ACTIVE` |
| **02-Organ**<br>免打扰桌面执行器官 | 原生 Chrome CDP 接管、WebRTC 阻断、原型链 Native 伪装、macOS 伴生悬浮壳（`NonActivatingPanel` 绝不抢焦点）、人机挑战感知与防乱点熔断 | `pkg/browser/`<br>`cmd/viewer/`<br>`pkg/challenge/`<br>`pkg/fingerprint/` | - [`fingerprint-browser-engineering-blueprint-and-checklist.md`](fingerprint-browser-engineering-blueprint-and-checklist.md)<br>- [`fingerprint-and-viewer-final-spec-2026-09-12.md`](fingerprint-and-viewer-final-spec-2026-09-12.md)<br>- [`viewer-requirements-2026-09-11.md`](viewer-requirements-2026-09-11.md)<br>- [`../spec/physical-patches.md`](../spec/physical-patches.md) | `ACTIVE` |
| **03-Engine-Alg**<br>确定性引擎与决策内核 | 候选建池（`pageCandidatePool` 口径）、确定性 node_id 定位、AX 导出、分层调度；管理冷态算法护城河 | `pkg/matcher/`<br>`pkg/engine/`<br>`pkg/classifier/`<br>`pkg/verifier/`<br>`pkg/probe_wasm/`<br>`cmd/tier1/`<br>（`benchmarks/tier1/` 不在本树） | - [`tier1-candidate-quality-2026-09-23.md`](tier1-candidate-quality-2026-09-23.md)<br>- [`algorithm-assets-inventory-2026-09-23.md`](algorithm-assets-inventory-2026-09-23.md)<br>- [`scheduling-trace-2026-09-23.md`](scheduling-trace-2026-09-23.md)<br>- [`wstn-algorithm-task-spec.md`](wstn-algorithm-task-spec.md)<br>- [`../spec/algorithm-kernel.md`](../spec/algorithm-kernel.md) | `ACTIVE`<br>*(ONNX/WASM 资产 PARKED)* |
| **04-Learning**<br>经验沉淀与正负闭环 | 记录可信 Execution Episode，区分正/负/未知样本，严把 G-PROOF 命中自证门禁，杜绝经验投毒 | `pkg/knowledge/`<br>（`learning/`、`pipeline/` 不在本树） | - [`knowledge-ops.md`](knowledge-ops.md)<br>- [`../spec/knowledge-versioning.md`](../spec/knowledge-versioning.md) | `PARKED`<br>*(待前置基线建立后激活)* |
| **05-QA-Studio**<br>Vibe Coding 交付验收层 | 充当独立 Judge，以 Acceptance Contract 为边界，汇集多源证据出具客观审查与 Agent-ready 修复指引 | `pkg/qa/`<br>`pkg/webcontrol/`<br>`pkg/provider/`<br>`pkg/vlm/`（Eye 视觉显微原语） | - [`qa-studio/README.md`](qa-studio/README.md)<br>- [`qa-studio/commercial-validation-execution-plan-2026-09-14.md`](qa-studio/commercial-validation-execution-plan-2026-09-14.md)<br>- [`qa-studio/delivery-readiness-requirements-2026-09-15.md`](qa-studio/delivery-readiness-requirements-2026-09-15.md) | `HOLD`<br>*(业务挂起)* |

> **状态定义**（活规范生命周期四态，任务/里程碑状态见 `Tasks.md`）：
> - `ACTIVE`：当前正在编译运行、接受请求与持续维护的生产契约；
> - `PARKED`：核心技术资产接口冻结、测试保持绿灯、暂停在线装配或待前置基线激活（如 ONNX 脑干、WASM 探针、自进化学习闭环）；
> - `HOLD`：商业化或高阶业务逻辑暂时挂起，不阻断当前核心底座收口；
> - `DRAFT`：新域或新方案草案，评审中尚未冻结。

---

## 二、 系统公共架构与开发者指南

跨越单一领域的全局架构指南与开发者必读材料：

- [贡献指南](contributing.md) · [MIT 许可证](../LICENSE) · [安全政策与私密报告](../SECURITY.md)

- **端点星（Cell-0: WebTouch）领地宪章与双连续体自治规约**：[`cell-0-territory-manifest-and-dual-continuum.md`](cell-0-territory-manifest-and-dual-continuum.md)（状态：`ACTIVE` · 2026-10-03；确立排他写锁、双连续体代际交叠护航、Codex 探马测绘规范与领地盲区字典）
- **v2.0 物理内核拓扑与感知执行规范 (RFC)**：[`webtouch-v2-architecture-rfc.md`](webtouch-v2-architecture-rfc.md)（状态：`APPROVED(目标态)` · 2026-09-25；消灭假成功水床位移、Isolated World 感知脱壳、AX 穿透寻底与 FD 3/4 管道生命周期强绑定）
- **Stage 3 演进白皮书（计划事实源 · 2026-09-25）**：[`stage3-adaptive-interaction-engine-plan-2026-09-25.md`](stage3-adaptive-interaction-engine-plan-2026-09-25.md)（用户 `[State: E]` 落盘：阶段一~四、6 大意图族、G-PROOF 矩阵与 `T-3` 阈值表、证伪协议、三类物理暗礁、三项 Bounty、M1~M4；任务与验收登记于 [`../Tasks.md`](../Tasks.md) `T-23`）
- **T-25 站点 recipe 切片排期（一次性产物 · 2026-10-01）**：[`t25-recipe-slicing-plan-2026-10-01.md`](t25-recipe-slicing-plan-2026-10-01.md)（按行数 × 复杂度折算的切片表、首片、瓶颈与能力边界；任务书与状态仍以 [`../Tasks.md`](../Tasks.md) `T-25` 为准）
- **系统全景地图**：[`system-map-2026-09-23.md`](system-map-2026-09-23.md)（功能、调用链、目录与代码边界的静态地图）
- **核心难点与约束**：[`hard-problems-2026-09-23.md`](hard-problems-2026-09-23.md)（D1~D11 难点、已知失效路径与核实口径）
- **D11 长页视口路由契约**：[`../spec/long-page-viewport-routing.md`](../spec/long-page-viewport-routing.md)（>3 视口触发、3–5 个只读文档切片、选中后滚动、回退与验收边界；算法内核 §5.0.19 仅保留指针）
- **开发与上手**：
  - [`../spec/security-dx.md`](../spec/security-dx.md) §2.2：输入字节边界（input 256 KiB / evaluate 1 MiB / 其他 4096）与派发前校验；`pkg/engine/sanitize.go` / `sanitize_input_test.go`（ACTIVE）。
  - [`quickstart.md`](quickstart.md)：快速开始
  - [`cli-and-config.md`](cli-and-config.md)：命令行与配置说明
  - [`contributing.md`](contributing.md)：代码贡献指南
  - [`coordinator-guide.md`](coordinator-guide.md)：外部协调器对接指南
  - [`tdd-checklist.md`](tdd-checklist.md)：TDD 验收标准总清单
  - [`user-cases.md`](user-cases.md)：用户场景与典型用例

---

## 三、 历史案卷库

单次实跑、探针与历史草案不属于活规范。本仓证据见 [开源合格化案卷](evidence/pilots/oss-qualification-2026-10-31.md)；`archive/`、`benchmarks/` 不在本仓库树内。
