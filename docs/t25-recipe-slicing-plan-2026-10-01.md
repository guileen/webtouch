# T-25「站点 recipe：嗅探抓存 → 模板回放」切片排期（2026-10-01）

> **口径（用户 2026-10-01）**：不按人天；按**代码行数**估、再按**复杂度**折算；显式标出**测试执行**成本；
> 显式标出**超出 Agent 能力范围**的部分。
> **性质**：一次性排期产物（派单 `d06d1209a`，goal-office → pilot）。任务书与状态仍以 [`../Tasks.md`](../Tasks.md) `T-25` 为准；
> 规范先行已完成＝契约 §13 `DRAFT`（[`gateway-contract-2026-08-26.md`](gateway-contract-2026-08-26.md) §13）。

## 1. 现状读数（行数估计的物证，均为本机只读）

| 读数 | 值 | 位置 / 命令 |
| --- | --- | --- |
| 页内直发（站点形状硬编码 4 站） | 289 行 | `apps/gateway/in_page_direct.go`（`wc -l`） |
| 网络层响应捕获（只监听、不落盘） | 402 行 | `pkg/browser/modelbridge/stream.go`（`wc -l`） |
| CDP 事件接线 | 276 行 | `pkg/browser/cdp_session_events.go`（`wc -l`） |
| `requestWillBeSent` 现有解析面 | 只取 `requestId` / `type` / `request.url`；**method / headers / postData 未解析** | `pkg/browser/cdp_session_events.go:112-131` |
| 页内直发现存活缺陷 | `json.Marshal(req.Headers)`；生产路径无人赋值 ⇒ `null` ⇒ 生成脚本抛 TypeError ⇒ `not_dispatched` | `apps/gateway/in_page_direct.go:121`、`:138` |
| 漂移自检范式（可直接照抄） | 307 行 | `apps/gateway/modelmenu.go`（`T-24`） |
| recipe schema（规范先行） | §13 `DRAFT`（含槽位生产者类型与实测对照表） | `docs/gateway-contract-2026-08-26.md:232` |
| 唯一实测站证据 | **Gemini 一站**（`at`/`f.sid`/`bl` 三者逐字命中页面态） | `docs/evidence/pilots/sniff-recipe-first-capture-2026-09-26.md` |

## 2. 切片表（按行数 × 复杂度折算）

| 片 | 内容 | 生产行数 | 复杂度 | 测试执行成本（真正瓶颈） | 谁做 |
| --- | --- | --- | --- | --- | --- |
| **S1** | 抓存：`requestWillBeSent` 扩到 method / headers / postData，原始请求快照原子落盘 `~/.uws/recipes/` | ~110（事件接线改 ~10 ＋ 新 `pkg/browser/recipe_capture.go` ~100） | 低（纯被动、无页面上下文、无状态机） | 单测 ~90 行，**零真实页** | Agent |
| **S2** | 嗅探编译：把 URL query / 头 / 体切成「稳定骨架 ＋ 槽位」，按值回溯页面态判定 producer，产出 §13 schema JSON | ~220（新 `apps/gateway/recipe_sniff.go`） | 高（字符串切分 ＋ 值回溯 ＋ 生产者判定；最易过拟合单站） | 单测 ~150 行（Gemini 真实快照作 golden fixture）＋ **多站泛化回归**（现只有 Gemini 一站证据） | Agent |
| **S3** | 槽位自解：`page_global` / `from_url` / `counter` / `fresh_uuid` 四类取值器（页面上下文 eval） | ~160（新 `apps/gateway/recipe_slots.go`） | 中（`counter` / `fresh_uuid` 需页面侧状态） | 单测 ~110 行 ＋ 少量真实页 eval | Agent |
| **S4** | 回放：recipe → 页内 fetch；4 站硬编码降级为内置 recipe；顺带修「`Headers=nil ⇒ not_dispatched`」 | ~220（改 `in_page_direct.go` ~90 ＋ 新 `apps/gateway/recipe_replay.go` ~130） | 中（复用 `ExecuteInPageDirectStream`，契约清晰） | 单测 ~120 行 ＋ **真实站 E2E**（活登录标签页，沙箱外） | Agent |
| **S5** | 两态：首轮（无会话 ID）／后续轮（回填）＝ recipe 内一条分支，不写两套代码 | ~40（并入 S2 / S4） | 中（结构约束，非独立文件） | **真实两轮 E2E**（首轮建会话 → 后续轮回填） | Agent |
| **S6** | 漂移自检钩子：口径同 `T-24`（探测只报不改、失败即查、可定时） | ~130（新 `apps/gateway/recipe_probe.go` ＋ 路由） | 低-中（照抄 `modelmenu.go` 范式） | 单测 ~80 行 | Agent |
| **S7** | 真实页验收（**不是代码**）：嗅探 → 落盘 → 关页 → 新页回放 → 比对响应 | 0 | — | 3–5 轮真实会话，每轮需活登录站点 | Agent 执行；**账号 / 风控 / 付费归人** |

- **合计**：生产 ≈ **880 行**；按复杂度加权（低 ×1 / 中 ×2 / 高 ×3）≈ **1,800 等效行（≈ 2.0×）**；
  测试 ≈ **550 行单测**（可自动跑）＋ **5–8 次真实 E2E 会话**（无法进 CI）。

## 3. 首片

**S1（抓存原始快照）**——零外部依赖、纯被动监听、单测即可验，且是 S2 的唯一输入 ⇒ **可立即开始**，无前置、无需用户拍板。
先落原始快照（而非直接落 recipe），是为了让 S2「骨架/槽位」的编译过程可复盘：抓错了是抓存的错，切错了是编译的错，两段证据分开。

## 4. 瓶颈与能力边界

**瓶颈**
1. **泛化证据只有一站**：recipe schema 的证据只来自 Gemini 一条链路；S2 的槽位识别极易只对该站调参 ⇒ 「任意站点可用」暂不可证（契约 §13 边界已自认）。
2. **测试执行的机器成本在 S4 / S5 / S7**：每轮都要真实登录站点 ＋ 活标签页；E2E 无法进 CI、只能人工门控，且可能触发真实模型调用（费用）。

**能力边界（超出 Agent 范围）**
1. **反垃圾 / 身份信号服务端权重不可观测**（BotGuard 类字段）⇒ 只能证「该站该接口在该页面态下可复现」，不能证「任意站点可用」。
2. **账号、登录态与风控处置归人**；站点改版后 recipe **只报漂移、不自愈**（同 `T-24` 口径）。
3. **响应语义正确性**（答得对不对）不由本线判定——本线只保证请求可复现、响应可读。

## 5. 与任务书的关系

- 本文件是排期产物，**不改** `Tasks.md`：既因该文件已超复杂度硬限且未在 `docs/complexity-exemptions.txt` 登记，也因工作树里已带一条本线未提交的 `T-30` Phase 3 状态订正，不宜替它做提交。
- 本文是开工前的切片快照，不能作为当前实现状态；任务进度与验收状态只认 [`../Tasks.md`](../Tasks.md) `T-25`。
