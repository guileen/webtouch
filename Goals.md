# WebTouch 目标与决策（Goals）

> **状态**：`ACTIVE · ENGINE_HARDENING`（QA Studio 验收层线 `HOLD`）
> **定位**：本文件是 WebTouch 的**目标域**——终局目标、红线、当前阶段目标与决策规则（低频、稳定）。
> 任务 / 依赖 / 验收 → [`Tasks.md`](Tasks.md)；当前状态与证据指针 → [`handoff.md`](handoff.md)；项目唯一必读入口与结论事实源 → [`GLOBAL.md`](GLOBAL.md)；Agent 互动入口契约 → [`docs/agent-interaction-contract-2026-09-23.md`](docs/agent-interaction-contract-2026-09-23.md)；核心设计纲领 → [`WebTouchKeyDesign.md`](WebTouchKeyDesign.md)。
> 生效：2026-09-23（用户定案：webtouch 侧 `Goals.md` / `Tasks.md` / `handoff.md` 为后续标准）。

## 一句话目标

面向 AI Agent 的「浏览器执行与站点身份底座」＋「个人桌面自动化能力」＋「Vibe Coding 独立交付验收层」；在**不打扰用户、不抢系统焦点**的前提下，把真实浏览器操作、网站账号与执行证据做成个人自己的确定性器官。WebTouch 为 Agent Hub 提供网页模型 provider 执行能力；客户端模型 API、provider 路由和 CLI/Web 连续体归 Agent Hub。（展开见 `WebTouchKeyDesign.md` 一、）

## 四大终局目标（指针，不复制正文）

1. 提供网页模型 provider 与浏览器交互执行总线；Agent Hub 负责客户端模型 API、provider 路由和跨客户端连续体。
2. 免封杀、零打扰的物理世界桌面执行器官（Native Hook 伪装 + 确定性渲染噪声 + macOS `NonActivatingPanel`）。
3. Vibe Coding 独立交付验收层（QA Studio：Acceptance Contract + Evidence Provider + Readiness Gap + Agent-ready Fix）。
4. 单兵高精度主权外骨骼（个人主权武器，**非**灰产转售 SaaS）。

## 终局红线（不得越过）

- **R1 零打扰**：自动化不得抢占系统键盘 / 前台焦点；唯一例外是用户显式点小窗那一下。接管窗不因「整齐」挪动用户已挪顺手的窗。
- **个人主权总线不做对外 SaaS**：总线与凭据只服务本机个人工作台，不为外部客户做代理 / 转售。
- **凭证只走应用层 vault**：明文的唯一出口 = 引擎在本机对 `@vault:<名>` 解引用后直填；不进 argv/`ps`、不进协调器、不过 gRPC、不进日志；取不到即 fail-closed 停下。
- **语义白名单是安全边界**：`pkg/engine/sanitize.go::semanticRe` 不含 ASCII `/`，不改；遇此类语义换写法（子串口径）。
- **只认页面侧证据**：`Status: SUCCEEDED` 不是动作生效的证据；不以报告自述结案。

## 当前阶段目标（2026-09-23）

1. **引擎稳定性收口**：点击「自证落点」——报告回显 `hit_element`、落点与目标相关性比对、判 miss 不返回成功且不写 learned。
2. **AutoDL 账号密码线**：改密入口可达 → `input:<合法语义>=@vault:autodl` → 由用户在场完成短信验证。**（2026-09-24 用户口径：改密疑已完成、暂不着急 ⇒ 本条 `HOLD`，不占推进面；G-CLICK 证据与停止线仍有效）**
3. **坐标换算取证**：先补「VLM 原始 `x_ratio/y_ratio` + 当轮 `curW/curH/origin`」再定性根因，之后才谈修哪一层。**（2026-09-30 分轴定性已做：10 组真机配对读数 ⇒ 换算链 10/10 恒零误差，误差全在模型估计且 x 轴内容驱动；证据 [`docs/evidence/vlm-coord-forensics-2026-09-30.md`](docs/evidence/vlm-coord-forensics-2026-09-30.md)，判据见 [`Tasks.md`](Tasks.md) `T-1` 后续复核）**
4. **发布纪律**：稳定版 = 本地最近成功构建；改二进制必须 `make install` + `zsh -ic './uws daemon restart'`，并复核 VLM 未被静默禁用。
6. **头部网站覆盖（2026-09-24 用户口径）**：按二八定律收敛——先把访问量最高的头部站点打通（登录 / 搜索 / 表单 / 内容抽取等常见交互），终局判据是「大部分常见网站上工作良好」，而不是单站过关。覆盖矩阵、逐站状态与失败归因见 [`Tasks.md`](Tasks.md) `T-14`。
5. **Agent 互动入口**：复杂/歧义任务由 Codex 主控跨步骤规划与后验判断；WebTouch 负责页面证据、隔离与确定性执行。快速目标模式保留；新路径须先完成隔离页面 dogfood，在线 learned 仍受 G-PROOF/T-3 门禁约束。

7. **Stage 3 四阶闭环（2026-09-25 用户 `[State: E]` 定案）**：意图族高熵扩批（M1）→ 轻量对偶蒸馏器 `pair_skeleton_distiller`（M2）→ 金库隔离与人机接力 S0~S5（M3）→ G-PROOF 全域回放与 `T-3` 放行（M4）。`T-3` 放行由白皮书 §5.2 量化为四行硬指标（`Precision@1 ≥ 90%` / `Recall@3 ≥ 98%` / 陌生二极域连续 20 站零失误 / 凭据泄露命中 `= 0`），未达标前决策 Scorer 恒 `nil`。计划事实源 → [`docs/stage3-adaptive-interaction-engine-plan-2026-09-25.md`](docs/stage3-adaptive-interaction-engine-plan-2026-09-25.md)；任务与验收 → [`Tasks.md`](Tasks.md) `T-23`。
8. **AI 交互总线直发与 Recipe 编译（2026-10-01 用户定案）**：面向在线 Agent AI 对话与 AI 搜索服务（Gemini、DeepSeek、ChatGPT、Claude 及 AI 搜索类服务），建立「首跑因果采样 → In-Page Direct Recipe 自动蒸馏 → 毫秒级直发 → 漂移自动降级」的统一闭环能力；不追求全网任意表单通用，但在此类 AI 在线问答服务上彻底跑通通用范式（任务书见 [`Tasks.md`](Tasks.md) `T-25`，契约见 [`docs/gateway-contract-2026-08-26.md`](docs/gateway-contract-2026-08-26.md) §13）。


### 目标与取舍标准（2026-09-30 用户口径回写）

- **目标（用户原话）**：一个**可以学习的、自动化的**浏览器使用引擎。
- **取舍标准（由目标推出，三条）**：① **可学习** —— 每次执行留下可复用证据，判错要能被纠正；② **自动化** —— 常见网站上的事能自己走完，不靠人守着；③ **不打扰**（R1 零打扰）—— 不抢系统键盘与前台焦点。
- **用法**：这三条是取舍题的默认解算器 —— 标准能推出答案的**不问用户**，直接自决并在交付里声明（依据＝[`labs/agent-tools/AGENT-PATCH.md`](../../agent-tools/AGENT-PATCH.md) 第 8 条）。只有两条目标互斥、标准自身冲突时才问，且问冲突本身。
- **已有推论**：判 miss 是比「盲目乱点」更有用的明确诊断，应保留；但「连败即冻结整页」与「自动化」冲突 ⇒ 熔断应对「判 miss」与「盲目乱点」区别对待（属技术内部口径，Agent 自决；实现未开工）。

## 技术演化目标：三条隔离线

这三条是**共用技术底座的能力目标**，不替四个产品终局目标排序，也不要求拆仓。每条线有自己的输入、输出、验收与上下文包；跨线只通过共享证据语义协作。

| ID / 目标 | 归属与输入 → 输出 | 独立演化判据 | 边界 |
| --- | --- | --- | --- |
| **O-SCHED 整体调度** | Agent Hub 拥有跨 Provider 的 Graph/run/task；WebTouch Coordinator 拥有浏览器任务内的步骤与语义恢复；Engine 拥有 session、intent 去重与运行护栏；Browser 拥有物理派发与局部可靠性 → 有界、可追踪的步骤执行 | 能用 fake Engine/Browser 测任务分步、deadline、恢复预算、幂等与取消；不依赖 VLM 或真浏览器 | 不把 Hub 调度、Coordinator 任务策略、Engine intent 生命周期和 Browser 物理重试混成一个“重试”所有者 |
| **O-ALG 算法决策** | 页面 Observation（Probe/WASM、AX、DOM 特征、截图）+ 目标 → 有序候选、置信/来源或 abstain | 固定 Snapshot fixture 可离线重放；按站点与交互类型构建样例矩阵和跨站点 holdout；分别量候选质量、各阶段耗时、Probe/WASM 降级与 VLM 兜底占比 | 常见/高置信页面先用规则、matcher、classifier/WSTN 等快路径；低置信、无候选或明确视觉长尾才转 VLM。Browser 可采集/适配，不能吞并算法判定；规则/模型可驻留明确版本的只读快照，更新走显式晋升；不先搬目录 |
| **O-LEARN 学习沉淀** | 决策、派发、命中、页面后验及人工纠正组成的 Execution Episode → 可回放样本、影子评估与版本化知识产物 | 多站点上的失败、恢复和纠正能持久追溯，并在未见站点 holdout 中证明复用收益；产物可晋升、回滚 | 标签分 `positive / negative / unknown`；仅页面后验支持正样本，只有可归因失败才记负样本；取消后结果未知不硬标错。`SUCCEEDED` 单独不是标签；脱敏后记录，不保存 Cookie、vault 内容或凭据；原始日志不直接改在线行为 |

### 共用边界与量度

- 贯穿 `run/task → step → intent → attempt` 的关联键；每次 Observation 有版本/出处；候选记录决策来源、排序和耗时；执行记录分开保存 `dispatch_state`、物理结果、目标命中和页面后验，任何一项都可为 unknown；Episode 引用这些证据并保存标签来源。这里先冻结**语义**，再决定具体 Go/Python/proto/schema 字段。
- 基线先行：调度量预算耗尽、重复副作用和取消分布；算法量各层延迟、分站点/交互类型的 Top-k 命中与落点相关性、Probe/WASM 可用与降级、VLM 调用占比；学习量样本标签质量、跨站点失败复发/恢复复用、影子评估与回滚。先用公开页面和本地 fixture；按站点切分 holdout，未取基线前不填目标百分比或性能承诺。
- 先让调度与算法各自用模拟器/固定快照并行形成基线；学习可并行定义记录与回放，但在线晋升须等共享证据语义成立；点击的页面特定 learned 先过 G-PROOF / T-3，其他动作须过各自的页面效果证据门。完成一个能力不要求同时重构另两条线。

## 证据与决策规则

- 每条结论挂证据指针（文件 / 日志路径 / sha256 指纹 / 线上状态）；未拿到证据前标「未定论」，不得升格为根因。
- 页面动作是否生效看页面侧证据（AX `[value=…]`、弹窗出现、`elementFromPoint` 命中目标）；页面真实地址直读 CDP `/json/list`。
- 同一事实只有一处事实源；发现两处不一致，当场指出并把其中一处降级为指针。
- 只有命中 `labs/AGENTS.md`「安全与停止线纪律」五类真风险（真实账号凭据/验证码、银行支付扣费、对外可见发布、不可逆删除、用户明确说停）才停下问。
  「要不要现在修、修哪一层、要不要压缩文档、要不要重启服务」这类内部取舍由 Agent 自决并直接执行，
  在交付里说明选择与证据即可，不得为此停下来等确认。

## 不做什么（边界）

- 不做对外 SaaS 转售、不做明文凭据传递、不为让点击成功去放宽语义白名单或改写页面语义。
- 不碰真实账号的登录 / 人机验证 / 改密 / 短信验证码，不在同一个真实账号页上反复重试；
  除此之外的构建、重启、改代码、起停自建浏览器会话与 profile 均为已授权操作。
- **不破解人机验证**：验证码 / 2FA / 风控一律走 Handoff——Agent 侧只做「识别 → 挂起 → 通知 → 轮询鉴权成功」，绝不下发任何绕过性操作；自动化键鼠在接力期间物理卸载（Stage 3 白皮书 §十）。
- 不把内部证据改写成市场成立证据（QA Studio 门禁：G1 需求与买方 → G2 受控试点 → G3 非关联方付款 → G4 付费批次重复性）。
