# pkg/engine MEMORY（M5：SPA 时序接线 + 知识库热更 gRPC 全链路）

## 已点亮（2026-08-19）
- TDD-ENG-06/07 实现层：verifier 状态机接入 ExecuteAction——
  - 动作后捕获 pushState → 立即返回 NAVIGATING（异步交接，绝不立即判完成）；
  - 下一动作入口若 NAVIGATING → 做 ≤timeout_ms 稳定性等待：Network Idle 500ms →
    NAVIGATED；忙碌连接/超时 → gRPC DEADLINE_EXCEEDED；DOM 突变重置静息已接线。
- TDD-KB-01..05/07 实现层（bufconn + 注入多版本 fetcher）+ 契约层 KB-01/03 子集。

## 设计要点
- 每个 BrowserContext 一个 pageVerifier（verifier 状态机 + navCompleted 标记）；
  InitializeContext 时 dropPage（新任务不复用旧时序）。
- 事件源抽象：`verifierEvents`（NavigationState）——SimCDPSession 可脚本化
  （pushState/mutated 一次性事件、active 持续电平），真实 CDP 适配器实现同一接口。
- 响应 ContextState 语义：STABLE（无导航）/ NAVIGATING（pushState 刚捕获）/
  NAVIGATED（本次调用内完成稳定化，navCompleted 消费一次）；超时返回
  DEADLINE_EXCEEDED 错误码（spec §7.2：协调器退避重试）。
- Reload 与 Rollback 共用 `demoteUnknownActive`：目标版本未知的当前 ACTIVE 规则
  → STAGING（防止经 ReloadKnowledge 回滚污染旧知识，spec §5.4）。

## 踩坑
- 同步 ExecuteAction 无法边睡边喂假时钟：模拟阶段采用「动作后立即返回 NAVIGATING +
  下一动作入口有界等待」的降级语义；真实 CDP 事件流接入后改为实时等待（USER_ACTION #4）。
- waitForStable 的 deadline（request timeout_ms）与 verifier NavTimeout（1500ms）
  并存：timeout_ms 更小则先触发；两者都映射 DEADLINE_EXCEEDED。
- 契约层（真实二进制）内置 memoryFetcher 只服务当前版本且 OnnxReady=false：
  能诚实验证 dry-run 与 sanity 拒绝，但切换类用例必须注入 fetcher（实现层做）。

## KB-06 自动回滚 + online_logs（2026-08-20，USER_ACTION #7/#13）

- **KB-06**：引擎 `noteNotFound` 连续 NOT_FOUND 计数（阈值 50、成功动作重置、触发即
  清零）；触发后 `Store.AutoRollback` 复用双缓冲 previous 句柄重建回滚目标
  （不直接复用旧句柄——句柄持有创建时规则快照，一致性修复会改规则集），
  回滚后清空 previous 防乒乓；HealthCheck 经 proto v1.5 新字段附回滚标记，
  人工 Reload/Rollback 后标记清除。
- **online_logs（SEC-01/03）**：`logstore.go` modernc sqlite 落库（`--log-path`）；
  PII 脱敏 = 语义命中 `pwd/pass/ssn/phone/id_card` 即语义与值整替换 `[REDACTED]`
  + `pii_masked=true`；node_features/decision_trace 只写长度/类型元信息，
  不落原始文本/值。snapshot_blob 规格写 zstd，当前明文 JSON（体积小便于审计），
  待压缩依赖决策。

## 页面候选通道（2026-08-20 续9，N6 修复 + ENG-13 映射）

- **真实 CDP 未打分节点禁止逐节点 L1 入围**：`pageCandidatePool` 对 Confidence=0
  节点逐节点 `MatchQuery(n.Text)` 会让页面上任意命中规则的无关节点（如「登录」
  按钮）入围，随后 L3 以目标语义为参考全剔 → 假 NOT_FOUND（规则库未覆盖的语义
  必现，ENG-13 E2E 实测）。修复：Confidence=0 节点仅吸收 L2 同义词候选；L1 由
  语义直查路径覆盖（`MatchQuery(target_semantic)` 未命中 → classifier 兜底 →
  物理层 findElement 精确定位）。显式置信度（探针/ONNX 打分）行为不变。
- **NEW_WINDOW_OPENED 映射**：runAction 在 verifier 事件后消费
  `browser.WindowEvents.NewWindowOpened()`（一次性读清），置
  `ContextState_NEW_WINDOW_OPENED`（优先于 NAVIGATING）；原 Target 由物理层
  activateTarget 前台化保证可继续执行。
# 引擎记忆

## 2026-08-25 R-A：内置 KB 扩 DeepSeek chat 规则 + 选择器贯通

- 内置 `DefaultArtifact` 规则 4 → 7：新增 `chat_input`（`textarea[placeholder*='发送消息']`）、
  `send_button`（`div[role='button'].ds-button--primary`）、`chat_answer`（`.ds-markdown`），
  TextPattern=语义标签、Selector 实测自 DeepSeek 页面 DOM；`KeyRuleIndexCount` 同步 7。
- `executePhysical` 增加选择器解析：`matcher.RulesBySemantic(semantic)` 取 active 且非空
  Selector 的首条规则，优先走 `browser.SelectorActionSession`；无规则/会话不支持时降级
  `ActionSession`（原行为零变化）。
- 引擎默认 sim 会话会「假成功」（占位文本）——协调器层已加 sim 告警；生产/演示必须
  `--browser cdp`。

## 2026-08-27 VLM 感知兜底（pkg/vlm + vlm_fallback.go）

- 挂钩：决策带 4 个 NOT_FOUND 分支（L3 全剔 / C<θ_low / A* 无路径 / classifier
  fallback）+ 物理层失败（no_match / eval_error / eval_empty，challenge 除外）都接
  `vlmFallbackFinish` / `vlmTry`；成功走 `finishAction` 复用 SPA 验证与 extract/日志。
- `vlmTry` 顺序：learned 命中（免 VLM）→ 截图 → VLM → 选择器 / 坐标（可精修）/
  内部语义文本三候选；成功即 `vlmLearn` 回写（命中再 +1 hits）。
- 2026-08-27 起 `vlmTry` 升级为 **VLM 主控会话**：VLM 返回 `Result{action}`（click /
  crop_zoom / extract / back / done / impossible），引擎按动作驱动多轮循环（crop 放大回喂、
  back 重截、done/impossible 收尾）；无 action 的历史候选走旧 `vlmLegacy`（粗看→细看→点击/
  提取），行为不回归。失败信号区分：`vlmOutcomeExhausted`（impossible/轮数耗尽）→
  `Aborted`，`Skipped/Transient` → 保持 NOT_FOUND（统计真兜底率）。详见 GLOBAL §5 #52 与
  pkg/vlm/MEMORY.md。
- 精修坐标 = 裁剪原点 + ratio × 裁剪尺寸；`refineRegion` 裁剪 60%×45% 视口并钳制边缘。
- 真实闭环实测：/store「Godfall」首轮 VLM 15.7s 点进详情、二轮 learned 0.66s。
- 教训：VLM 文本输出无 bbox，x_ratio/y_ratio 换算必须经 ClickAt 的 elementFromPoint
  路径回读真实元素 CSS 路径，绝不直接采信模型坐标执行 Fitts。

- **实测反哺（2026-08-27 下午，小说站 /store 真实验证）**：
  - 点击路径自进化成立：/store「Godfall」首轮 VLM 7.49s（2 调用，全页 642 + 精修 602
    tok，`refined to 270,265 crop 76,0 247x411`）点进 /detail，二轮 learned 0.76s（0 调用），
    hits 1→2。
  - **提取路径自进化是「假闭环」**：`action=="extract"` 成功时 `vlmLearn` 只写
    `LearnedEntry{NodeText: res.Text}`（见 vlm_fallback.go extract 分支），条目**无
    selector 也无 semantic**；而 `vlmTry` 的 learned 命中只认 `Selector` / `Semantic`，
    两者皆空 → 判 `entry stale` → 回退重试 VLM。实测「Search books」/「/search Godfall」
    learned 后重跑仍 `no_match`。**反哺：extract 路径也要回读确定性定位器（复用
    ClickAt 的 elementFromPoint 思路做 ExtractAt），不能只存 node_text；否则存储被污染、
    下次照样烧 VLM。**
  - **失败不可观测 + 高推理放大成本**：`vlmTry` 对 `candidates exhausted` / 超时 / 空文本
    全部坍缩成 `no_match` 丢给调用方，测试者无法区分「语义真没有」与「VLM 没救回来」。
    Wilder 那次 13.6s、1927 tok（推理 1470/1476=100%）→ 2nd 次 15s 超时
    `context deadline exceeded`（默认 --vlm-timeout-ms 15000 偏短）；
    Search books 18.3s、2329 tok（推理 1833/1878=98%）。**结论：单一 `temperature=0`
    挡不住长推理，失败成本(¥0.007~0.019/10~18s)远高于成功(¥0.003~0.006/7s)。** 建议：
    ①prompt 显式「只输出坐标/文本、不要思考过程」或禁 reasoning；②超时提到 20~25s；
    ③对 exhausted 与非 exhausted 给不同 gRPC 信号。
  - **根治（2026-08-27 傍晚已改代码）**：高推理根因不是 prompt 措辞，是**模型默认开启
    thinking 模式**。`pkg/vlm` 请求体加 `thinking:{type:"disabled"}` + `promptFor` 改
    「看图作答不要推理」 + `max_tokens:300` → 实测 reasoning 归零、单次 1.1~1.5s、
    318~378 tok（旧 602~2069），首次 Godfall 点击 4.63s（旧 7.49s/22.96s）。详见
    `pkg/vlm/MEMORY.md`。**默认 --vlm-timeout-ms 15000 仍偏短**（长推理/网络抖动会
    `context deadline exceeded`），建议提到 20000~25000；生产初始化建议加
    `--vlm-timeout-ms 25000`。
- **判定层证据链第一切片（v1.5.4，2026-08-27）**：`finishAction`（成功与 VLM 兜底
  成功共用）统一回显 `hit_element`（来自 `browser.HitElementSource.LastHitElement`）
  + `page_url`/`url_changed`（动作前后 `PageURLSource.PageURL` 差分）。语义：
  `success+event_triggered` 只代表「事件触发」，不代表「目标达成」——调用方用
  `hit_element`（命中真身 selector/rect）+ `url_changed`（导航证据）在 assertion
  模式判假阳性（点中容器 / 相关搜索建议），task 模式作为重试依据。SimCDPSession
  默认合成 `sim:<action>:<semantic>` 证据（测试可 SetLastHit 脚本化容器命中、
  SetNavOnAction 脚本化导航）。下一步（设计文档 §6.3）：verdict 接口 +
  task/assertion 双模式 + 会话级证据导出（console/pageerror/network/state-diff）。

## 新窗口接管（TDD-ENG-21，2026-09-22 P0-1）

- **引擎默认接管**：`finishAction` 检测到 `NEW_WINDOW_OPENED` 后，若
  `context_hint.follow_new_target` 不是 0/false/off/no（默认开），就断言
  `browser.TargetSwitcher` 并 `SwitchTarget(ctx, newTargetID)`；接管失败只记日志+推一条
  viewer 事件，**不改** `NEW_WINDOW_OPENED` 语义（调用方据此继续暂停）。
- **契约**：`ActionResultResponse.new_target_id`（本次打开的新页）+ `active_target_id`
  （动作后实际作用页，取 `activeContextTargetID`：优先 `TargetSwitcher.ActiveTargetID`，
  回退 `TargetDebugInfo.TargetID`，再回退 Pool 内部 ID）。`new == active` 即已接管。
- **钉扎扩展**：`contextMatchesTarget` / `findContextLocked` 现在接受
  `bc.ID` / `PinnedTarget()`（`initResponse` 写入的原钉扎值）/ `activeContextTargetID`。
  不扩就是「接管后第一次带 `target_id` 的动作用 NotFound 打回来」。
- **`finishAction` 多了 ctx 形参**（SwitchTarget 需要），两个调用点（`runAction`、
  `vlmFallbackFinish`）同步更新。
- **验证**：`pkg/engine/eng21_test.go` 3 例（默认接管 / `follow_new_target=0` 不接管 /
  接管前后两种钉扎都能续页）；`go test ./pkg/...` 全绿。
