# 整体调度可追踪表（T-10 · 2026-09-23）

**范围**：WebTouch 内 `run/task → step → intent → attempt` 的调度归属、关联键、
预算上限与状态转移；覆盖 **协调器（Python）→ 引擎（Go）→ Browser 物理层** 三层，
外加 Agent Hub 的归属核对（只读外部仓 `world-agent/agent-hub`，证据见 §1、§6）。
**性质**：表内的每一行都挂代码锚点；「可重放 / 不可重放」的判断来自
`pkg/engine/service.go` 的幂等账本与 `pkg/engine/interrupt.go` 的中断语义。
**重新验证**：`GOCACHE=/private/tmp/webtouch-go-cache go test ./pkg/engine -run TestSchedule -count=1`、
`.venv/bin/python -m pytest tests/integration/test_coordinator.py -q -k tdd_sched_10`。

## 1. 调度所有权（谁拥有哪段策略）

「重试」不是一个所有者，而是四段各自有界的策略；把其中一段挪到另一层就会产生重复拥有。

| 层 | 拥有 | 不拥有（禁止下沉/上浮） | 锚点 |
| --- | --- | --- | --- |
| Agent Hub | 跨 Provider 的 Graph/Node/Run 编排与节点级超时（5–60s，默认 20）；它把 WebTouch 当**子进程**调用 | 不拥有浏览器步骤恢复、页面语义重做，也不持有 intent/去重状态（只按进程取消） | `world-agent/agent-hub/internal/graph/graph.go:52,110`、`internal/graph/run.go:87`；`internal/worker/webtouch.go:111,160-170`（节点执行体与超时）、`:56-105`（`uws inspect --isolated` 子进程 + SIGTERM/5s WaitDelay） |
| 协调器 Coordinator | 任务分步、`max_steps`、失败分类重试预算、崩溃恢复重放、step↔intent 关联、VLM 求解上限 | 不拥有物理重试，不直接判定「页面是否命中」 | `coordinator/executor/executor.py:170`、`coordinator/reflector/reflector.py:56` |
| 引擎 Engine | session、intent 去重与幂等账本、派发前/后中断分类、运行护栏（熔断/挑战） | 不知道 step/attempt 语义，不替协调器收敛任务级预算 | `pkg/engine/service.go:779`、`pkg/engine/interrupt.go:18` |
| Browser 物理层 | 物理派发、局部可靠性重试（点击/输入各 3 次）、如实回报「已派发/未派发」 | 不做语义级重规划，不吞掉中断 | `pkg/browser/click_recovery.go:153`、`pkg/browser/click_recovery.go:234`、`pkg/browser/cdp_session.go`（`dispatchTracker`） |

## 2. 关联键：run/task → step → intent → attempt

| 键 | 生产位置 | 形式 | 说明 |
| --- | --- | --- | --- |
| `task_id` | 上层（CLI/Hub/Coordinator） | 字符串 | 协调器任务身份，`TaskDefinition.task_id` |
| `step_id` | `coordinator/models.py::ActionNode.step_id` | 计划内唯一 | 失败报告与回退点用它定位「卡在哪一步」 |
| `intent_id` | `coordinator/executor/executor.py:448` | `intent-{task_id}-{step_id}-{session_epoch}-{iteration}` | **同一个 step 的重试复用同一 intent_id**（命中引擎幂等/在途去重）；VLM 求解另加 `-solve-{n}` 后缀（`executor.py:410`） |
| `attempt` | 协调器隐式持有 | `session_epoch`（崩溃恢复，`executor.py:482`）、loop `iteration`、`step.retry_count` | 新 attempt = **新 intent_id**（见 §3 第 5 行） |
| `dispatch_state` | 物理层回报 → 引擎分类 | `not_dispatched` / `dispatched` | `pkg/browser` 的 `ObservedCanceledBeforeDispatch` / `ObservedCanceledAfterDispatch`，由 `browser.IsCanceledObserved` 解析（`pkg/engine/service.go:1090`） |

## 3. 引擎侧状态转移表（intent × 派发状态 → gRPC 码 → 可否重放）

账本键 = `客户端上下文 key + intent_id`（`service.go:787`）；下表是 `Server.execute`
（`service.go:779`）的全部出口。

| # | 进入条件 | 是否发生物理派发 | 返回 | 同 intent 再来的结果 | 可否安全重放 |
| --- | --- | --- | --- | --- | --- |
| 1 | cache 命中且未过 `IdempotencyTTL`(60s) | 否 | `OK` + 缓存副本 | 同 #1（不再派发） | 不需要（已是结论） |
| 2 | `indeterminate` 命中且距今 < `indeterminateGrace`(15s) | 否 | `ABORTED`「physical outcome unknown」 | 同 #2，直到窗口过期 | **禁止**：副作用可能已落地 |
| 3 | `indeterminate` 已过窗口 | 否 | 删除 `indeterminate`+配对 `inflight` 后按新 intent 走 | 可派发 | 允许（调用方显式重试） |
| 4 | 同 key 正在 `inflight`（并发重复提交） | 否 | `DEADLINE_EXCEEDED`「intent_id already in flight」 | 同 #4 | 退避后可重试（未派发） |
| 5 | 新 intent（或 #3 释放后） | **是** | — | — | — |
| 5a | ↳ 成功 | 是 | `OK` + 写 cache | 命中 #1 | 不需要 |
| 5b | ↳ 普通失败（如 `NOT_FOUND`） | 是 | 原错误，`inflight` 释放 | 可派发 | 由协调器裁决（通常 replan） |
| 5c | ↳ 中断，`dispatched=false`（定位/等待阶段） | 否 | `CANCELED` / `DEADLINE_EXCEEDED` + 「not_dispatched」 | `inflight` 已释放，可派发 | **允许**（无副作用） |
| 5d | ↳ 中断，`dispatched=true`（鼠标/键盘/导航已发出） | 是 | `CANCELED` / `DEADLINE_EXCEEDED` + 「结果未知」 | 保留 `inflight` + 写 `indeterminate` → 转 #2 | **禁止**（窗口内 ABORTED） |
| 6 | 清扫（每 `sweepInterval`(30s) 节流，`execute()` 入口触发） | 否 | — | cache 过期即删；`indeterminate` 过窗口连同配对 `inflight` 一起回收 | 有界性：条目数 ≤ 一个清扫间隔内新产生的 intent 数 |

要点：

- 中断分类**先于** VLM 兜底（`service.go:1090`）；VLM 坐标路径的 `obs2` 复用同一分类点，
  所以 `ClickAt`/`InputAt` 派发后取消同样落到 #5d 而不是被当成「候选都试过了」。
- 死 ctx 不会被展开成 `NOT_FOUND`：四处 VLM 兜底收口到 `vlmAtNotFound`（`pkg/engine/vlm_fallback.go`）。
- 码位：ctx 取消 → `CANCELED`；ctx deadline → `DEADLINE_EXCEEDED`（`interrupt.go:44`）。

### 一次 intent 的时序（可追踪样例）

```
协调器 _execute_step           引擎 execute()                      物理层
intent-<task>-<step>-<e>-<i> ─▶ 攒 inflight 占位 ───────────────▶ dispatchTracker.mark()
                                 │                                   ├─ 未派发即中断 → ObservedCanceledBeforeDispatch
                                 │                                   └─ 已派发后中断 → ObservedCanceledAfterDispatch
                                 ◀─ IsCanceledObserved(observed) ────┘
                                 ├─ not_dispatched → 删 inflight，返回 not_dispatched（可重放）
                                 └─ dispatched     → 留 inflight + indeterminate[now]（窗口内 ABORTED）
```

## 4. 预算矩阵（各自有界，互不代管）

| 层 | 预算 | 值 | 锚点 |
| --- | --- | --- | --- |
| 协调器 | 任务级步骤budget | `max_steps`（默认 50），用尽 → `FAILED max_steps_exceeded` | `executor.py:170`；`models.py:54` |
| 协调器 | 单步 DEADLINE_EXCEEDED 重试 | Fibonacci 1s/2s，最多 2 次（受 `step.max_retries` 钳制） | `reflector.py:26`、`reflector.py:56-76` |
| 协调器 | ABORTED 物理遮挡自愈 | 0.2s，最多 3 次 | `reflector.py:27-28` |
| 协调器 | REINIT 崩溃恢复 | 最多 1 次（之后 ABORT） | `reflector.py:29`、`reflector.py:94` |
| 协调器 | VLM 挑战求解 | 每任务最多 3 次，超限 `PAUSED challenge_solve_exhausted` | `executor.py:MAX_VLM_SOLVES` |
| 引擎 | 单次动作时间预算 | 请求的 `timeout_ms`（默认 1500） | `proto/uws_service.proto` + `execute()` |
| 引擎 | intent 幂等缓存 TTL | 60s | `service.go:39` |
| 引擎 | 派发后未知观察窗口 | 15s | `interrupt.go:18` |
| 引擎 | 账本清扫间隔 | 30s | `service.go:43`、`service.go:761` |
| Browser | 点击/输入局部重试 | 各 3 次 | `click_recovery.go:153`、`:234` |

## 5. T-10 验收覆盖矩阵

| 验收项 | 测试 | 层 |
| --- | --- | --- |
| 预算耗尽 | `pkg/engine/scheduling_test.go::TestScheduleDeadlineNotDispatchedAllowsReplay`（deadline 未派发）、`::TestScheduleDeadlineAfterDispatchBlocksReplay`（deadline 已派发） | 引擎 |
| 预算耗尽 | `tests/integration/test_coordinator.py::test_tdd_sched_10_max_steps_budget_exhausted`、`::test_tdd_sched_10_deadline_retry_budget_exhausted` | 协调器 |
| transport 重发复用同一 intent | `test_coordinator.py::test_r4_retry_reuses_intent_id_and_epoch_bump_on_reinit`；Go：`TestInflightDedupSameIntent`、`TestApi02IdempotencyTtl`、`TestScheduleSuccessCacheHitDoesNotRedispatch` | 协调器 + 引擎 |
| 语义重做创建新 intent 并关联 step/attempt | `test_coordinator.py::test_r4_loop_iterations_use_distinct_intent_ids`；Go：`scheduling_test.go::TestScheduleNewIntentDispatchesIndependentlyOfUnknownWindow` | 协调器 + 引擎 |
| 未派发取消 | `pkg/engine/interrupt_test.go::TestInterruptBeforeDispatchAllowsRetry`、`::TestVLMPathCanceledBeforeDispatchAllowsRetry` | 引擎 |
| 派发后结果未知 | `interrupt_test.go::TestInterruptAfterDispatchReportsOutcomeUnknown`、`::TestVLMPathCanceledAfterDispatchReportsUnknown` | 引擎 |
| 非幂等动作禁重放 | `test_coordinator.py::test_tdd_coord_03_crash_replay_idempotent_only`（引擎侧无此概念，见 §6） | 协调器 |

## 6. 边界与已知缺口

- **引擎不认 step/attempt**：去重只按 `intent_id`。用**新** intent_id 打同一个物理目标，
  在引擎看来是一次新的决策（`TestScheduleNewIntentDispatchesIndependentlyOfUnknownWindow` 把这行钉死）；
  因此「同一步骤的重试必须复用同一 intent」（`executor.py:448`）是协调器的义务，不是引擎的保证。
- **`non_idempotent` 只在协调器**：`models.py::ActionNode.non_idempotent` + `executor.py:530::_is_idempotent`
  决定崩溃恢复只重放幂等步骤。proto 与引擎侧没有该字段，引擎不会替调用方拒绝对非幂等动作的重放。
- **清扫只在请求入口触发**：零请求的引擎不会主动回收账本；有界性依赖请求量（条目只在请求时产生），
  选择这个语义是为了不动状态机本身。
- **Hub 层边界（已核，2026-09-23 21:1x）**：Hub 拥有 Graph/Node/Run 的持久化状态与
  节点级超时，但**通过 `uws` CLI 子进程**触达 WebTouch（`internal/worker/webtouch.go:56-105`），
  因此它的取消粒度是**进程**（SIGTERM + 5s 后强杀），不是 intent；跨 Provider 的
  intent 去重/物理重试仍在协调器与引擎，Hub 不重复拥有。Hub 提供的门禁是
  SecurityContext 能力检查与 SSRF 沙箱（`internal/worker/webtouch.go:111-160`），
  与 WebTouch 内部调度正交。
- **调度事件目前没有落点**：表里的取消/未知/去重状态只出现在 gRPC 状态文案里，
  不写引擎日志——`~/.uws/engine.log`（3883 行，2026-08-27 20:51 → 09-23 16:55）
  中 `physical outcome unknown` / `intent_id already in flight` / `not_dispatched` /
  `indeterminate` / `inflight` 计数**均为 0**（不是没发生，而是不落盘）。
  因此「取消分布、重复副作用、预算耗尽分布」这类调度基线要测得先补事件落点，
  这是 O-SCHED 基线的前置条件，也是 Episode（T-12）证据面的自然内容。

## 7. 基线可测性（第一步，弱信号）

同一份日志里可数到的只有语义层事件：`no_match` 423 次、`VLM 兜底` 13 次
（同一窗口；**没有按请求/站点分层的分母**，也含 extract 的确定性 miss），
只能作为「快路径 miss 远多于 VLM 兜底」的弱信号，**不能**当作 T-11 的算法基线。
来源：`~/.uws/engine.log`（本机运行态 Engine `v1.5.5+d7455e39`）。
