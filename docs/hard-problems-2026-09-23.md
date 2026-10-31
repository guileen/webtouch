# WebTouch 难点说明书（2026-09-23）

> **用途**：交给外部智能体接手分析用的**现状与难点材料**，不是任务单，也不是规格。
> **取证口径**：标 `[已核]` 的条目由本机实测/实读于 2026-09-23；标 `[未核]` 的一律不得当事实用。
> 事实源优先级：`GLOBAL.md`（唯一必读全文）> `Goals.md` / `Tasks.md` / `handoff.md` 三件套 > 本文。

## 0. 这是什么

WebTouch（仓库内 `labs/world-agent/webtouch`）是一套**在真实浏览器里做确定性动作、并只认页面侧证据**的个人自动化底座。
它有三个共享同一运行时的产品方向：个人 AI 总线（OpenAI 兼容网关）、免封杀零打扰的桌面执行器官、Vibe Coding 交付验收层（QA Studio）。

代码实际分成三层，跨语言：

| 层 | Go 侧 | Python 侧 |
|---|---|---|
| 算法层（须能脱离浏览器跑/测/优化） | `pkg/matcher` `fingerprint` `classifier` `knowledge` `vlm` `challenge` `verifier` `probe_wasm` | `pipeline`(2782 行) `learning`(222 行) |
| 引擎层（浏览器环境 + 对应用层封装） | `pkg/browser`(10609 行非测试) `pkg/engine` `pkg/viewerhub` `pkg/vault` | `coordinator`(6277 行) |
| 应用层（真正的应用） | `apps/gateway` `pkg/qa` `pkg/provider` `pkg/webcontrol` `cmd/*` | —（历史 `coordinator/app/` 已归档） |

`[已核]` 应用层与引擎层之间是**进程/协议边界**：`uws` CLI 的 Python 子命令转发到 `python3 -m coordinator.cli`，网关则经 gRPC 调引擎，不链接 `pkg/engine`。
`[已核]` `go list -deps ./cmd/uws-gateway` 已不含 `uws/pkg/browser` 与 `uws/pkg/engine`（2026-09-23 分层收口结果）。

## 1. 不可越过的红线

- **R1 零打扰**：自动化不得抢占系统键盘/前台焦点；唯一例外是用户显式点小窗那一下。接管窗不因"整齐"挪动用户已挪顺手的窗。`[已核]` 该约束在 Swift 侧有实现与单测：`cmd/viewer/Sources/ViewerCore/ViewerFocusGuard.swift`、`ViewerPanelController.swift`、`Tests/ViewerCoreTests/ViewerFocusGuardTests.swift`。
- **总线不做对外 SaaS**：凭据与总线只服务本机，不为外部客户代理/转售。
- **凭据只走 vault 解引用**：明文唯一出口 = 引擎在本机对 `@vault:<名>` 解引用后直填；不进 argv/`ps`、不进协调器、不过 gRPC、不进日志；取不到即 fail-closed。
- **语义白名单是安全边界**：`pkg/engine/sanitize.go::semanticRe` 不含 ASCII `/`，不改。
- **只认页面侧证据**：`Status: SUCCEEDED` 不是动作生效的证据，不得以报告自述结案。

## 2. 难点清单

### D1 没有唯一当前阶段目标（组织性难点，最大）
- 现象：`Goals.md` 的一句话目标把三个产品并列写在一起，下挂 4 个终局目标 + 5 条当前阶段目标；状态行是 `ACTIVE · ENGINE_HARDENING`，同时另一条线标 `HOLD`。`[已核]` `Goals.md:3,9,15-20,29-35`。
- 为什么难：四个终局目标**共享同一套运行时**（一个内核 + 一个网关 + 一个协调器 + 一份 gRPC 契约），所以拆仓/拆线解决不了；取舍只能发生在目标层，而每条线单独看都"很重要"。
- 现状：未定案。这是本轮唯一没被任何技术改动缓解的难点。

### D2 真实浏览器环境的对抗性
- 现象：动作要在真实 Chrome 里确定性执行，同时要躲风控与验证码（代码里已列名 Cloudflare / PayPal / Arkose / reCAPTCHA / hCaptcha），还要不抢焦点。
- 证据：`[已核]` `pkg/browser` 10609 行非测试 + 7890 行测试；`pkg/challenge` 549 行（`FastDOMProbeScript`，注释称 <2ms 穿透 iframe）；`pkg/fingerprint` 2317 行（profile / WebRTC / 时区等指纹面）。
- 为什么难：**确定性与隐蔽性互相拉扯**；窗口焦点是 macOS 系统级约束，应用层无法自决；风控规则外部变化。
- 现状：Tier 2 四维测量在真实 Chrome/CDP 上跑（`cmd/tier2` → `pkg/browser/tier2.go`）。

### D3 证据链：怎么证明"动作真的生效"
- 现象：点击必须自证落点（`hit_element` 回显）、要有 AX 影子证据、要区分"引擎说成功"与"页面真的变了"。
- 证据：`[已核]` 2026-09-23 提交 `2154d494`（命中回显 / 指针文本叶消歧 / AX 影子证据）、`42d0ba3a`（proto `InitContext.require_existing_session`）、`coordinator/engine_client.py` 的 `HitElement`。
- 为什么难：证据要能**离线复核**，还要能区分"没生效"与"生效了但证据没采到"；漏判会污染学习样本。
- 现状：命中回显已落地；"落点与目标相关性比对、判 miss 不返回成功且不写 learned"仍在阶段目标里未闭环。

### D4 算法层被种在浏览器里（结构性难点）
- 现象：`pkg/browser/tier2.go`、`pkg/browser/probe.go` 直接调用 matcher；算法实验入口 `cmd/tier2` 必须靠 `browser.RunTier` 拉起浏览器才能跑；`pkg/probe_wasm` **没有 Go 代码**（只剩 `CMakeLists.txt` + 一个 C++ 测试 + `build/` 残留），而 `WebTouchKeyDesign.md` 把 WASM 探针列为技术护城河。`[已核]`
- 为什么难：算法层的要求是"能脱离浏览器执行、验证、测试、优化"，但当前**算法验证路径本身长在浏览器里**，迭代速度与可复现性都被绑死。
- 现状：算法包本身是干净的——`matcher/fingerprint/classifier/knowledge/vlm/challenge/verifier` 零 `pkg/browser` 依赖，测试基本离线（只有 1 个 fingerprint 测试文件碰真浏览器）`[已核]`。病灶只在 tier 实验这条路径。

### D5 学习层远弱于规则层
- 现象：`learning/` 全部只有 222 行（`online_learner` 136 / `shadow_eval` 49 / `snapshot_store` 36），而硬编码启发式的 `pkg/matcher` 是 1826 行非测试；比例约 1:8。`[已核]`
- 为什么难：要"靠学到的跑"需要三重条件——可离线训练/评估的样本闭环、影子评估、在线学习的门禁（G-PROOF / T-3）；而样本来自真实交互，又被 D2/D3 制约。
- 现状：WSTN 路线到 Tier 0 离线 ROC + 5 步 Mock 因果录制；真实本地站 CDP 录制与生产路径替换未完成。

### D6 跨语言双栈的契约漂移
- 现象：Go 引擎 + Python 协调器 + gRPC + proto 双份生成物（Go/Python）。重试语义**以协调器侧为准**（Fibonacci 1s/2s 最多 2 次），而 proto 错误码表注释写的是"重试 1 次"——同一事实两处不一致。`[已核]` `coordinator/MEMORY.md` 明确说测试不得写死 3 个退避间隔。
- 为什么难：契约是唯一事实源，但"生成物 + 注释 + 实现"三处都可能漂移；历史上已发生过一次（Python API 服务器被整体切除、收敛到 Go 网关，`coordinator/__init__.py` 有记录）。
- 现状：M1（运行时收敛）已销账；注释与实现不一致这条仍在。

### D7 恢复/幂等/反思的语义复杂度
- 现象：崩溃恢复只重放幂等动作（`IDEMPOTENT_ACTIONS`，且未标 `non_idempotent`），支付/发帖显式标记**绝不自动重放**；`NAVIGATING` 是异步交接、协调器不做 `wait_stable`；每任务最多 1 次反思，回退点取最近 5 步内最后一个 STABLE。`[已核]` `coordinator/MEMORY.md` 前 20 行。
- 为什么难：这些规则彼此耦合，全都建立在引擎侧证据之上；改任意一条都要重跑 E2E，而 E2E 又依赖真实浏览器（见 D2）。

### D8 状态与记忆本身的膨胀
- 现象：`[已核]` 根级台账 `GLOBAL.md` 251 KB / 2118 行、`操作记录.md` 290 KB、`指令日志.md` 119 KB、`USER_ACTION.md` 101 KB（合计 761 KB）。引用面：`USER_ACTION.md` 35 处、`GLOBAL.md` 41 处、`handoff.md` 28 处，其中一部分在 `.proto` 与生成代码的注释里。
- 为什么难：项目纪律是"历史日志不改写、同一事实只有一处事实源"，于是**旧路径会永久留在历史记录里**；改一次根级文件名就会制造一大片陈旧引用，读者也分不清哪个是活指针。
- 现状：`handoff.md` 有 ≤150 行纪律（当前 109 行）；`GLOBAL.md` 仍在快速增长。

### D9 共享工作树的并发写
- 现象：`labs` monorepo 里多条工作线在同一棵树上写，快照分钟级过期；本次作业期间 `GLOBAL.md` / `README.md` / `handoff.md` / `sites/ipub.io/*` 的未提交改动都来自别线。`[已核]`
- 为什么难：任何"全量盘点"都会立刻过期；提交纪律（逐路径暂存、禁止 `git add -A`、按域拆分）不是洁癖而是安全前提。

### D10 环境前提与同名文件陷阱
- `[已核]` Go 的 `build`/`test`/`list` 需要写工作区之外的构建缓存（`~/Library/Caches/go-build`），在受限沙箱里会报 `operation not permitted`，必须提权执行。
- `[已核]` 本机没有 Mermaid CLI；要画图用 `dot`（graphviz）+ `rsvg-convert`。
- `[已核]` `cmd/viewer/.build` 是约 197 MB 的 Swift ModuleCache，**已被** `labs/.gitignore` 的 `**/.build/` 覆盖；真正未被忽略的是 `world-agent/webtouch/archive/`（约 88 MB）。
- `[已核]` 同名文件陷阱：`labs/` 与 `labs/world-agent/webtouch/` 各有一套 `GLOBAL.md` / `handoff.md` / `README.md` / `AGENTS.md`，改之前必须看清是哪一份。
- `[已核]` 入口集合是冻结的：`AGENTS.md`（自动加载的门禁）、`GLOBAL.md` / `Goals.md` / `Tasks.md` / `handoff.md` / `README.md` / `WebTouchKeyDesign.md`、根软链 `uws`·`webtouch`·`uws-gateway`（兼容用）、`cmd/`（Go 入口约定）。任何目录调整都必须绕开它们。

### D11 定位的「因果环」：排序需要答案，而答案正来自排序（算法性难点）

- 现象：把点击/提取目标定到具体 DOM，本质是「在一堆候选里挑一个」。真实页面上这个集合很大（`[已核]` Wikipedia「Web browser」实测 **672** 个可作候选的节点），必须排序取前 K，而今天**唯一的序是锚点序**——锚点＝受体派发点＝**用户/引擎已经选中的那个元素**，即我们要找的答案本身。事实源＝`pkg/browser/cdp_session_snapshot.go` 的 `K = min(N, 64)` 与「有锚点按距锚点欧氏距离升序 / 无锚点退化文档序」、案卷 §8.5。
- 为什么难（三条缺一不可）：① **循环依赖**——排序要的是「与目标的相关性」，而目标的定位恰恰是排序要产出的结果，不能靠锚点自举；② **退化的兜底也没用**——无锚点时退化**文档序**，等价于「只看页面顶部」，`[已核]` 那 64 个候选全落在 y 17–725（页面 1440×6800 的**顶部 10.7%**），其余 89.3% 一个候选都没有，示例目标排在第 **279 / 672** 位、页面 59% 处 ⇒ 根本不在池里；③ **另一条路也堵死**——整页截图 ＋ VLM：13600 物理 px 高按 `detail=high` 卡到长边 2048 ⇒ 降采样 **0.15×**，目标那一行只剩 **70×7 px**（每字符约 2.7 px），长页**整页坐标回归**实测偏 **+78 px 出界**，而同页**裁块离散选择**在字高只剩 6.7–12 px 时仍 **3/3 全对**。⇒ 两条路同时失效。事实源＝案卷 §8.7、§7.1、§7.2。
- 与其它难点的关系：D2（真实浏览器的对抗性）使它更难——没有可离线标定的页面版式先验；D5（学习层远弱于规则层）意味着「学一个排序器」暂时没有可信的样本闭环。
- 现状：**定案待实施（2026-10-01）**。四阶重构方案（Phase 0 CDP 检索粗锚点自举、Phase 1 AX 地标/标题树分治、Phase 2 10 维 ONNX Ranker 与 Episode 闭环、Phase 3 候选块复选闭环）与四项深度修正（无副作用检索、别名展开联动、G-PROOF 样本严选、长页视口切片限宽）已定案，完整方案与路线图见 [`docs/evidence/d11-positioning-redesign-2026-10-01.md`](evidence/d11-positioning-redesign-2026-10-01.md)；`Tasks.md` 的 `T-30` 第二刀已对齐排期，下个会话集中实施。

## 3. 已知的过期文档（别照着做）

- `docs/contributing.md:36-37` 仍把 `coordinator/app/api_server.py` 与 `coordinator/app/providers/` 列为"应用层"。`[已核]` 这两处已于 2026-09-14 物理移除并归档到 `archive/coordinator/`。
- `Makefile` 的 `audit-layers` 里 Python 那半条门禁查的是 `coordinator.app` —— 一个**已不存在的路径**，属于空跑门禁（永远通过）。`[已核]`
- 任何日期型文档里的旧路径（`pkg/gateway` 等）都是历史记录，**不是**当前布局。

## 4. 未核实清单（外部智能体不得当事实）

- `[未核]` 所有文档里自述的"测试全绿/194 passed"之类数字。本次实测只覆盖 5 个 Go 包（`apps/gateway` / `pkg/engine` / `pkg/browser` / `pkg/challenge` / `pkg/qa`）+ `go build ./...`。
- `[未核]` Python 侧近期 5 个提交（`coordinator/*.py`）**未执行过测试**，只做了 Go 侧编译级检查。
- `[未核]` `gofmt -l` 列出的 5 个未格式化文件（`pkg/engine/knowledge.go`、`pkg/fingerprint/profile.go`、`pkg/provider/sanitizer.go`、`apps/gateway/provider_test.go`、`apps/gateway/reasoning_test.go`）是**既有状态**，与 2026-09-23 的改动无关；即 `make lint` 目前是红的。
- `[未核]` 本文引用的体积与行数会随时间变化，尤其是 `GLOBAL.md`。

## 5. 建议的介入顺序

1. 读 `AGENTS.md` → `GLOBAL.md`（唯一必读全文）→ `Goals.md` / `Tasks.md` / `handoff.md`。
2. 用 `make audit-layers` 与 `go build ./...`（需提权）建立边界与健康现状。
3. 若目标是"改结构"：先读 `docs/contributing.md` 的分层定义，但**按第 3 节修正过期部分**。
4. 若目标是"改质量"：优先 D3（证据链闭环）与 D5（学习层），它们是其余难点的放大器。
5. 介入 D1（唯一阶段目标）前先要业主裁定，不要在无人授权下替业主排序产品方向。
