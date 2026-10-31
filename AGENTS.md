# AGENTS.md — webtouch 子项目入口门禁（模块化协作）

> 本文件是 webtouch 项目的入口；在 webtouch 内工作的 Agent（Codex/Claude 等）自动加载。
> **核心认知与设计事实源**：分层、边界与核心定位以 [`WebTouchKeyDesign.md`](WebTouchKeyDesign.md)
> 为全系统唯一事实源；指纹实现以 `docs/fingerprint-browser-engineering-blueprint-and-checklist.md` 为准；
> 历史分层稿见 `docs/evidence/archive/web-touch-architecture-2026-09-02.md`。

## 硬性入口（每次任务必做）

1. **先完整读 `webtouch/GLOBAL.md`** —— 它是本项目唯一必读全文：核心定位 + 五大领域读取地图 + 关键红线。
2. 按 [`docs/README.md`](docs/README.md) 的五大领域矩阵只读对应文件；**禁止全量通读 docs/ + spec/ + archive/**（浪费 Token 且不聚焦）。`GLOBAL.md` 不复写这张地图。
3. `archive/` 默认不读；`docs/tdd-checklist.md` 是验收标准唯一索引；测试骨架映射见 `tests/README.md`。
4. 进入工作前读 [`handoff.md`](handoff.md) 顶部最新块（当前值 / 阻塞 / 下一步）与 [`Tasks.md`](Tasks.md) 对应门禁；目标或红线有疑问再看 [`Goals.md`](Goals.md)。

## 目标 / 任务 / 交接（2026-09-23 用户定案，后续标准）

本项目按三个文件分工，**它们是本项目状态与目标的事实源**，跨项目协调只在 `labs/handoff.md` 留指针：

| 文件 | 域 | 装什么 | 纪律 |
| --- | --- | --- | --- |
| [`Goals.md`](Goals.md) | 目标（低频、稳定） | 终局目标、终局红线、当前阶段目标、证据与决策规则 | 目标或红线变化才改；不写流水 |
| [`Tasks.md`](Tasks.md) | 任务（唯一任务书） | 门禁、任务、依赖、验收判据、停止条件、授权清单 | 同一 todo 不在两处勾选；完成必须回写证据 |
| [`handoff.md`](handoff.md) | 状态（AOF，≤150 行） | 当前值 + 硬约束 + 下一步 + 证据指针 | 每次任务结束在此加节或刷新当前值；超限按 `cog-compaction` 下沉归档，不继续加行 |

1. 任务结束、切换上下文或用户要求交接时，**更新 `handoff.md`**（外加 `Tasks.md` 的对应状态）；跨项目影响再更新 `labs/handoff.md` 的指针表。
2. 新增或变更活规范 → 更新唯一地图 `docs/README.md`；跨域稳定决策 → 更新 `GLOBAL.md` §4。不得在两处复制五大领域矩阵。
3. 本目录历史流水：`.mem/handoff.md`（2026-09-17 前的遗留快照，只作考古，其中 QA Studio 三门状态仍有效）、`.mem/handoff-归档.md`；`labs/handoff-归档.md` 保存根 handoff 的历史节。
4. 交接时间必须取自系统时钟（`date '+%Y-%m-%d %H:%M'`），不写估计值。

## 文档体系与事实源纪律（2026-09-24 用户定案）

1. **文档是唯一事实源**：代码实现只是文档规范的编译输出；改动代码必须规范先行，禁止无文档改动或先斩后奏。
2. **活规范与历史案卷物理隔离**：
   - **活规范（Living Spec）**：只保存在对应子域（总线 Bus / 执行器官 Organ / 决策引擎 Engine-Alg / 经验闭环 Learning / 验收层 QA Studio）及 `docs/` 索引中，永远反映当前最新有效契约，并显式标注状态（`ACTIVE` 运行中 / `PARKED` 冷态待命 / `HOLD` 业务挂起 / `DRAFT` 评审中草案），严禁时态漂移与设想当现实现象；
   - **历史案卷（Evidence）**：一次性实跑报告、单站探针导出、评测结果与截图一律归入 `docs/evidence/`（`research/`、`pilots/`、`archive/`），作为不可篡改的凭证，严禁平铺在规范目录制造噪音。
3. **高内聚自治，严禁到处翻找**：
   - 核心业务域的意图、契约、代码映射与测试闭环集中在一处；处理哪个域的问题，只看该域的规范，严禁跨域无序拼图检索。详见 `docs/README.md`。
4. **文档减熵原则（奥卡姆剃刀）**：
   - 不增设解释文档的元文档，文档复杂度过高也是屎山；
   - 新增结论必须改写或下沉原有条目，不新开平行记录；废弃或单次测试内容及时归卷，保持活文档绝对精炼。

## 运行/发布规范（稳定模式，2026-09-03 用户定案）

> 边界：**skill 只讲“怎么用”，本文件是“怎么维护/发布”的项目规范**，二者不互相混入。

1. **稳定版 = 本地最近成功构建**：`make install`（= `make build` + `make probe-wasm` +
   固定 `/usr/local/bin/{webtouch,uws,uws-engine}` → 仓库 `bin/` 内产物）。产物统一在
   `bin/`：`bin/webtouch` + `bin/uws-engine`（仓库根 `webtouch`/`uws` 为兼容软链）；
   wasm 在 `build/probe_wasm_wasm/`。`uws probe`（AXTree + 截图报给 Agent 层）开箱可用。
2. **版本核对/升级**：`uws daemon status` 看运行版本；二进制更新后执行
   `make engine-restart`（build + daemon restart），或 `make install` 后再
   `uws daemon restart`。禁止只替换二进制不重启（版本漂移根因）。
   **默认策略（2026-09-03 定案）**：release / 引擎二进制更新若影响功能一致性，
   默认立即 restart 对齐运行版本，**不再询问**；restart 不影响已登录的调试 Chrome
   （Chrome 由 uws 独立生命周期管理，9222 端口与登录态保留）。
3. **任务期间允许构建与重启（2026-09-23 用户定案放宽）**：WebTouch 是测试环境，Agent 可自行
   `make build/install/engine-restart`、`uws daemon restart`、`go build/go test`、
   在 `/tmp` 下写诊断脚本（含只读 CDP）、起停自建浏览器会话与 profile、清理自建 profile 锁。
   只剩两条硬约束：(a) 改完源码必须 restart 才能宣称新版本生效（防版本漂移）；
   (b) 不得用重启/清锁来掩盖真实账号页的风险信号（见 `Tasks.md` 停止线，那只锁账号页不锁引擎）。
   环境异常（入口缺失、probe 缺 wasm、锁残留）先自行修复，修不了再上报；
   不再默认"停下等维护者"。
4. **浏览器生命周期收口 uws**：`--attach debug` 自动拉起/清理 stale 锁
   （`coordinator/attach.py`，setsid 常驻），不使用 launchd（历史 plist 仅留档未加载）。
5. **反馈闭环**：稳定模式缺能力 → 记录 feedback（USER_ACTION/GLOBAL），按
   `docs/tdd-checklist.md` 走 TDD，收敛后 `make install` 重新发布。
6. **执行护栏（付费/扣费）**：动作命中付费/扣费词或页面出现付费 CTA + 确认类点击时，
   executor 在执行前 PAUSED「需要确认」，报告附原因/URL/AXTree/截图；无绕过参数。
   上层 Agent 必须停下询问用户，禁止代为确认或改计划绕过。

## 通用约定（与上层工作区一致）

- 语言：中文；TDD：红-绿-重构；**不可逆破坏才先确认**（删凭据库、关用户窗口、force push），
  其余直接执行；文档与代码同步更新。停止线纪律总则见 `labs/AGENTS.md`「安全与停止线纪律」。
- **单文件复杂度与行数上限（2026-09-25 Top 原则 · 含存量豁免）**：
  - **新增硬门禁**：**新建**源码文件与**新建** Markdown 文档的体量不得超过门禁——**阈值数值不在本文件定义**，见全局 `~/.codex/AGENTS.md`「文本体量门禁」；超限不得合入。
  - **存量豁免（Grandfather Clause）**：已存在的超限文件不因存量本身阻塞其它改动（清单与精确行数登记在 [`Tasks.md`](Tasks.md) `T-14` 第十一批 ④，仅 webtouch 子面即 13 个，如 `pkg/browser/cdp_session.go` 4098 行）；但在其中**修 bug、补特例时严禁继续堆砌**，新增能力必须抽到独立子模块、helper 或新文档。
  - **存量巨石拆分独立立项**：拆分单独受领任务、不得夹带在业务修复批次里。
  - **字节维度与审计入口（2026-09-26 用户裁定 · 默认生效）**：行数与字节**同时**卡、取严（**阈值数值见全局 `~/.codex/AGENTS.md`「文本体量门禁」，本处不复写**）。日常审计＝`../../bin/complexity-audit.sh`（未登记的超硬限文件退出码 1）；webtouch 面超硬限存量登记与全仓登记同表：`../../docs/complexity-exemptions.txt`。口径细则唯一事实源＝`../../docs/complexity-budget-spec-2026-09-26.md`，本处只留指针；行数维度的存量清单仍见上款（`Tasks.md` `T-14` 第十一批 ④），两处不重复登记。
  - **目录规模上限（2026-09-26 用户裁定 · 双列口径）**：同一目录**非测试文件**（不含 `_test.go`）软 **25** / 硬 **40**、**条目总数**（含测试）软 **50** / 硬 **80**（旧「≤ 20 个」作废）。**Go 包边界＝目录**，`pkg/browser`（86 条目/40 非测试，超硬限）、`pkg/engine`（55/26，关注区）等是**目录债务**：同包内拆文件（可机器证明零语义改动）照做，**拆子包**（改导出面）须独立立项（计划见规范附录 A，阶段 1 已批准开工）。审计脚本「目录超限（硬）」「目录关注区（软）」「Go 包 god-package 超硬限/关注区」各段默认只告警、`--strict` 仅硬限计入失败；另：**语料/实验产物目录**（`experiments/*`、`references/corpus/*`、`product-genome/objects/*`、`*/assets/versions/*/prompts/*`）按规范 §三 明示名单整类排除，不计入统计。
- 新增模块/改契约/更新规格时，更新该事实的唯一所属规范与 `docs/README.md` 地图；跨域不变量写入 `GLOBAL.md` §3，稳定决策写入 §4。不得维护平行矩阵。
- 模块专属经验写该模块 `MEMORY.md`，通用经验才进 `GLOBAL.md`。
