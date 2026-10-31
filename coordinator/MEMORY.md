# coordinator/ 模块经验（2026-08-19 M6C 点亮）

- **NAVIGATING 是异步交接，不做 wait_stable**：v1.4 同步契约下 ExecuteAction 返回
  `NAVIGATING` 即视为本步完成，稳定性等待由引擎在「下一动作入口」执行
  （GLOBAL.md §5 裁决 #9）；协调器若自行等待会双倍耗时且无对应 RPC。
- **重试语义以协调器侧为准**：DEADLINE_EXCEEDED → Fibonacci 1s/2s 最多 2 次
  （`spec/coordinator.md` §6.1）；proto 错误码表注释的「重试 1 次」是引擎侧契约建议，
  测试不得写死 3 个退避间隔。
- **NOT_FOUND 禁止盲目重试**：Reflector 直接判 REPLAN，重规划失败后按
  `step.on_failure` 处理（默认 abort）；测试用 `on_failure="skip"` 验证反思回退路径。
- **崩溃恢复只重放幂等步骤**：`IDEMPOTENT_ACTIONS = {input, select, focus, hover,
  scroll, wait, extract}` 且未标 `non_idempotent`；支付/发帖显式标记绝不自动重放。
  无 NAVIGATED 快照 = 全量重跑（幂等已完成步骤仍重放，非幂等保持完成不重放）。
- **反思上限**：每任务最多 1 次反思（`MAX_REFLECTIONS = 1`），回退点取最近 5 步内
  最后一个 STABLE；`decision_trace` 只收引擎返回的非空 `decision_trace_id`。
- **L2 Embedding 降级**：`TokenOverlapEmbedder`（字符 bigram Jaccard）仅为零依赖
  占位，生产换 MiniLM/BGE 向量检索（`spec/coordinator.md` §4.1）。
- **NEW_WINDOW_OPENED 处理（2026-09-22 更新，P0-1）**：Attach 缺口已由**引擎侧接管**绕过——
  引擎默认把会话作用面切到新 target 并在响应回报 `new_target_id`/`active_target_id`；
  executor 见 `active == new` 即前移 `ctx.current_target_id` 并**继续任务**（不再 PAUSED）。
  两者不等（接管失败 / `context_hint.follow_new_target=0`）时保持 v1.5.5 暂停语义。
  `GrpcEngineClient.attach_target` 仍无 RPC（不再需要，仅存量接口）。
- **VLM 打码引擎原语（v1.5.1，2026-08-20 续16）**：`CaptureScreenshot` RPC + ExecuteAction
  drag 原生拖拽已落地（USER_ACTION #9 完成）；`GrpcEngineClient.capture_screenshot` 返回
  真实 PNG。executor `_solve_challenge` 的 EngineClient 契约无需改动；生产 VLM 求解器
  （DeepSeek-VL / 打码 API）替换 `MockVlmSolver` 即闭环。E2E 挑战页用「403+CF 挑战头 +
  reload 200」模拟真实 Turnstile 流（挑战状态随 Document 响应重算，见 browser/MEMORY）。
- **VLM 配置项（2026-08-20）**：CLI `vlm` 段 + `coordinator/config.py`（纯 stdlib
  YAML 子集读取，与 cmd/uws/config.go readConfig 同构）+ `HttpVlmSolver`
  （POST base64 截图 + challenge_hint，Bearer 鉴权，非 2xx/非法响应抛 VlmError）+
  `build_vlm_solver` 工厂；`Executor(max_vlm_solves=...)` 可配置覆盖。
  未配置 → None → PAUSED 降级（行为兼容）。HttpVlmSolver 测试用注入 urlopen 假
  响应，不发真实网络请求。
- **UUIDv7 占位**：task_id/intent_id 当前用 uuid4 生成；规格写 UUIDv7，仅影响
  排序性不阻塞功能，生产接入时替换。
- **条件表达式语法（TDD-PLAN-05，2026-08-20）**：`{{ var }} ==|!=|in|not in <字面量>`，
  RHS 支持 `'字符串'`/`true|false`/数字/`['a','b']` 列表；`_eq` 类型感知
  （bool↔"true" 互认、int/float 互通），`_contains` 支持列表成员/子集与字符串子串；
  缺失变量安全默认（`==` 不执行 / `!=` 执行），非法表达式默认不执行分支。
  loop 的 `max_iterations` 封顶安全退出、`exit_condition` 满足提前退出、
  支持 `{{ var }}` 插值；循环中途失败按失败语义立即终止（不吞错、不继续剩余迭代）。
# 协调器记忆

## 2026-08-25 深夜：API 流式输出 + SSE 干净文本（方案 1）

- `_execute_with_retry` 增加 `context_hint`；`chat_answer` 的 baseline / 轮询 /
  流式 extract 统一带 `{"prefer_sse": "1"}`，引擎优先返回 CDP Network 层捕获的
  SSE 干净 Markdown（无 `-1-4-7` 引用角标碎片；空快照显式空串，多轮不串味）。
- **预算耗尽 DOM 兜底（2026-08-26 实测修复）**：SSE 捕获轮可能因连接失败
  （loadingFailed）/空响应/挂起而永远等不到 done，页面却已渲染完整回答——
  `_wait_for_new_answer` / `_stream_answer` 在预算耗尽时做最后一次**不带
  prefer_sse** 的 DOM extract：非流式直接返回 DOM 完整文本（优先于 SSE 半截
  文本）；流式把剩余增量补发后 stop。引擎侧空/失败捕获轮回退 DOM 见
  pkg/engine/service.go 与 GLOBAL §5 #40。
- `/v1/chat` 支持 `stream: true`：`_stream_answer` 短轮询（0.25s 粒度）把累计
  文本的增量按 OpenAI `choices[].delta.content` yield，首块带 `role=assistant`，
  稳定 `answer_stable_sec` 后发 `finish_reason:"stop"`；`stream_chat` 序列化
  `data: {...}\n\n` + `data: [DONE]`。
- HTTP SSE 踩坑：**无 Content-Length 的流式响应必须 `close_connection=True`
  收尾**，否则 urllib/OpenAI SDK 因 HTTP/1.1 keep-alive 永远读不到 EOF（10s 超时
  实测）；`_wants_stream` 在 `_route` 之前拦截 `POST /v1/chat` 的 `stream:true`。
- 测试：FakeEngine 脚本化增量文本，直接驱动 `chat_stream` 生成器与真实 HTTP
  SSE 两端验证；`answer_stable_sec=0.0` 时首块即收尾（单块 delta）。

## 2026-08-25 R-A：流式回答轮询 + sim 告警

- `ApiServer._extract_answer_with_polling`：send 后 DeepSeek 流式生成，回答节点延迟出现；
  在 `chat_timeout_sec` 预算内轮询 extract（轮次上限 = ceil(预算/1.5)+1，防注入 no-op sleep
  的环境死循环），空文本/未命中持续重试，预算耗尽抛最后错误。
- `_ensure_session` 检测 `target_id` 以 `sim-` 开头时向 stderr 打告警，避免把模拟会话的
  占位回答误当真实结果。

## 2026-08-25 深夜续：Provider 插件化 + DeepSeek 三模式/开关

- `coordinator/providers/`：`base.ChatProvider` 契约（site/semantics/models/
  model_switch_semantic/thinking_semantic/search_semantic/model_on_new_chat_only/
  input_settle_sec/new_chat_settle_sec + resolve_model/norm_key）；deepseek.py 与
  gemini.py 各自收口站点差异，`api_server.py` 只消费契约（不再 if site 分支）。
- **模式仅新会话可切**（DeepSeek）：`model_on_new_chat_only=True` 时 `chat/chat_stream`
  只在 `is_new` 调 `_ensure_model`——DeepSeek 的快速/专家/识图单选组只在
  `chat.deepseek.com/` 落地页出现，进入 `/a/chat/s/...` 后消失，多轮重切会找不到
  radio。Gemini 的模型选择器常驻输入栏，`model_on_new_chat_only=False` 保持任意轮可切。
  **2026-08-26 增补（用户实测「专家模式没选对」修复）**：请求模式 ≠ 会话已锁定
  模式（未锁定按模型目录首项=快速模式比较）时，即使客户端没传 `new_session`，
  API 也会自动重建 Tab 兑现模式（关旧 Tab + 开新 Tab + 切换），不再静默沿用旧
  模式；重建后的新 Tab 保持 `model=None`，`_ensure_model` 无条件做一次物理校验/
  切换。`GET /v1/sessions` 返回 `models`（会话 → 已锁定模式族名），测试页以此
  同步单选与 lastMode。同模式多轮不重建。
- **resolve_model 返回 None 语义**：DeepSeek 未收录字符串（如旧 `deepseek-chat`）返回
  None=不切换（沿用页面默认），与 Gemini「未收录透传给物理层匹配」不同——物理层
  匹配失败会给可用列表（DeepSeek 无模式列表错误语义，故走不切换）。
- **专家模式无智能搜索**：`search:true` 在专家模式下物理层 `toggleDeepSeekSwitch`
  找不到 `智能搜索` 开关 → 显式报错（不静默忽略），错误经 `_execute_with_retry`
  的 ApiChatError 语义透传回 HTTP 502。

## 2026-08-27 请求级自动分解 DSL（planner，用户「可以执行」落 V-model 0.1）

- **动机**：uws「直接理解复杂请求并自动分步」= auto-planner 的上半段。当前门控仍是
  「卡住才唤 VLM」，要往「请求级自动拆动作序列」推进，先定一个**少 token、无成对关键字**
  的 DSL（用户骨架 `find(xx) and do(xx) or login else reply('fail')` 显式被否，因其
  有 `()`/`and/or/else` 成对/关键字开销）。`coordinator/planner/planner.py` 新增
  `parse_plan_dsl(text, variables) → ParsedPlan`。
- **文法（一行一意图，append-only）**：
  - `<verb>:<operand>`：click / focus / hover / scroll / wait / extract（引擎真动作）；
  - `<verb>:<目标>=<值>`：input / select（双操作数）；
  - `search:q`：宏 → `input:search_box=q` + `click:search_button`（默认语义，LLM 可从
    AXTree 改写；常数 `DSL_SEARCH_*_SEMANTIC`）；
  - `a || b`：失败回退，编译为 `on_failure`（`reply:`→abort、`login`→replan、
    skip/retry/replan→原样、其余→replan）；
  - `x | $var`：本步输出绑定到变量（Var 名允许 Unicode，如 `$成分`）；
  - `?guard:hint`：前置守卫（记录到 preconditions，v0.1 不产步）；
  - `nav:url` / `reply:expr`：**计划级指令**（不产 ActionNode，存 `ParsedPlan.initial_url` /
    `result`，`Plan` 已加同名字段）。
- **关键约束（决定 dir/lifetime 边界）**：`ActionNode.action_type` 只在
  `click|input|select|focus|hover|scroll|wait|extract`；引擎**没有 goto/reply 动作**——导航
  走 `initialize_context(initial_url=...)`（变量 `initial_url`），任务结果=最终 extract 数据。
  所以 `nav`/`reply`/`$var` 不能硬塞进 ActionNode，只能是计划级元数据。
- **target 白名单约束（2026-08-27 复测暴露）**：引擎 `target_semantic` 只允许
  字母/数字/下划线/连字符/空格/中文（`sanitize.go semanticRe`），`|` 是 DSL 保留
  分隔符（绑定/回退）——操作数 target 含裸 `|`（旧示例 `搜索结果|第1个`）会在解析期
  报 PlanValidationError（曾到运行期才 INVALID_ARGUMENT）。示例统一改
  `click:第1个搜索结果`；value 字段不受此限（引擎 value 无字符白名单）。
- **真实复测反哺（2026-08-27，dsl_drive.py 直驱 50062 引擎 + 9222 Chrome）**：
  - DSL 解析→引擎执行链路本身通（步骤顺序执行、extract/click 语义直查按预期失败）；
  - **语义匹配依赖页面真实文本**：调试 Chrome 渲染 Bing 为 zh 页（placeholder
    「搜索网页」、aria「在此处输入你的搜索 — …」），中文语义「搜索框/搜索按钮」
    全部 no_match；curl 拿到的 HTML 可能是日/英 variant，不能替代真实 DOM。
  - **抽象序数语义（`click:第1个搜索结果`）DOM 无字面元素**，语义层必 no_match；
    这类「用户语义」要么是真实文本/aria/placeholder、KB 同义词，要么只能靠 VLM
    解析——DSL 示例的「第1个」属于后者，需 VLM 才能兑现。
  - **input 15s 超时（input_failed: context deadline exceeded）**：匹配/输入/落盘
    自检未在预算内完成，无日志区分「没匹配上 vs 输入失败」，健壮性待查。
  - 结论：无 VLM Key 时语义层天花板=交互元素文本匹配；「搜索+读成分表」全链路
    必须 VLM 主控（或 KB 规则/选择器透传，批次 3 待办）。
- **VLM 主控复测（2026-08-27 晚，Key=~/.zshrc DEEPSEEK_API_KEY，50062）**：
  - **链路机械上通**：`nav → click:第1个搜索结果 → extract:成分表 | $成分 → reply:$成分`
    3 轮 run 全部 ok，变量绑定/final_result 正常；VLM 确实调用（模型
    deepseek-v4-flash-vision-exp，base api.deepseek.com）。
  - **点击假成功（复现 drama-h5 同类）**：`click:第1个搜索结果` 报 success +
    event_triggered=true，但 target_url 前后不变（仍在 Bing SERP）——VLM 点到
    容器/推荐位，引擎无导航结果自检（CDP target id 精确追踪实证）。
  - **提取假数据**：`extract:成分表` 返回页面右下角**相关搜索建议词**
    「红宝石精华成分表」（截图经 VLM 复核：该词只出现在搜索建议框），非真实成分表；
    `extract:核心成分` 只取到 Copilot 摘要小标题本身，不是整段成分内容——VLM 主控
    无「目标达成判定」，子串命中即当成功。
  - **可观测性缺口**：VLM 主控成功路径（click/extract）无 engine 日志，learned 文件
    也未写（主控 ActionExtract 只 vlmLearn NodeText，落地时机不清），成本/成功率
    无法事后统计。
  - 反哺：VLM 主控下一迭代必须补 ①click 后导航/上下文变化核验（URL 或元素是否变化）、
    ②extract 目标达成判定（结果是否含所需信息）、③主控每轮动作/结论日志。
- **接入**：`Planner._plan_from_llm` 现接受 LLM 返回 **str（DSL）** 或 dict（旧 JSON steps），
  同一 `validate_plan` 门禁，append-only；`tests/unit/test_planner.py` 新增 3 例（分解/
  失败回退/LLM 出 DSL 字符串）。
- **待办（v0.1 缺口，写进 GLOBAL §5 #52）**：`ParsedPlan.initial_url/result/bindings` 目前
  只被捕获，**`result`（`reply`）与 `$var` 的运行时打通未做**——executor 不把 extract 结果
  写回 `ctx.variables`，`Plan.result` 只是元数据；`?guard` 也未展开成真实登录步骤。真正把
  「自然语言→DSL→执行→回结果」闭环，还需：① executor 把 extract 数据写入变量；② `?guard`
  展开为可执行的登录模板步；③ DSL 作为 L4 冷启动规划的输出格式（替换旧 JSON prompt）。

## 2026-08-27 CLI 自解释 + skill 极简化（用户「同步到顶层 + 容错/自解释」）

- **思想**：让 CLI 成为「常新事实源」（self-explain skill），skill 极简并指向它，保证
  skill 永远和 CLI 一致，不再漂移。两个入口都自解释：
  - Go `cmd/uws/main.go`：`uws help`/`-h`/`--help`/无参 → `agentGuide()`（快速启动/子命令/
    网关/引擎/请求级 DSL/错误速查/反模式），exit 0；未知命令/转发错误补 agent 提示。
  - Python `coordinator/cli.py`：新增 `help` 子命令（`agent_help()` = `AGENT_GUIDE`）+
    argparse `epilog`；`main()` 顶 layer 兜底 `EngineError`/`OSError`/`ValueError`/`Exception`
    并打印 `_engine_hint()`（状态码→对策）；`run_crawl`/`run_api` 内部 except 补提示行。
- **顶层同步**：`.agents/skills/uws-browser-agent` 曾是断链（指向已迁走的
  `gl/agent-tools/webtouch`），已修到 `labs/agent-tools/webtouch/.agents/skills/uws-browser-agent`；
  并在 `.codex/skills/`、`.claude/skills/` 补发现链（指向 `.agents/skills/uws-browser-agent`）。
  ⚠️ `.agents/`、`.codex/` 在代码沙箱里**只读**，需升级权限才能改软链。
- **skill 极简**：`uws-browser-agent/SKILL.md` 由 183 行压到 ~40 行——保留触发词 +「先
  `uws help`」+ help 未强调的非平凡坑（VLM 需 `--vlm-fallback`+`UWS_VLM_API_KEY`、cdp vs sim、
  需登录一次持久 Chrome 等）+ 深读指针；详细机制以 `uws help` + `docs/` 为准。
- **避免双份漂移**：完整手册正文以 Go `agentGuide` 为准；Python `AGENT_GUIDE` 为等值副本
  （供 `python -m coordinator.cli help`），改中文案需两处同步。
  **2026-08-27 已根治**：共享文案落 `cmd/uws/agent-guide.md`——Go 用 `//go:embed`
  嵌入（`agentGuide()` 返回嵌入文本），Python 按模块相对路径读取同一文件（缺失时
  回退最小提示），两侧彻底同源，改中文案只改一处。
- **死代码清理（2026-08-27）**：移除已由结构性脱敏取代的 `redactText`/`redactJSON`/
  `walkRedact`（online_logs 改为 value 一律 `[REDACTED]` 的结构性保证，无需 JSON
  树扫描）；同步删除对应单测（`TestRedactText`/`TestRedactJSONTree`）。
- **测试**：`tests/unit/test_cli.py` 新增 3 例（`uws help` 内容/无参同手册、Python help
  子命令、`_engine_hint` 状态码映射），TDD-CLI-13。

## 2026-08-27 混合指挥 `uws commander` + DSL `node:<id>`（CLI/DSL 形态，TDD-CLI-15/PLAN-07）

- **形态纠正**：混合指挥演示不放 examples 独立脚本——UWS 是 CLI skill + DSL，不是
  embedding。外发请求桩收口为 `uws commander` 子命令（`coordinator/commander.py`）：
  `--tree-file` 极简 AXTree（`-` 读 stdin）+ `--image` 截图 → OpenAI 兼容 VLM 端点
  （`--dry-run` 打印请求 / `--mock` 本地模拟 / 真实读 `UWS_VLM_API_KEY ||
  DEEPSEEK_API_KEY`）→ 解析 `{"actions":[{"node_id","action","reason"}]}` → 校验
  node_id ∈ AXTree → 编译为**多行 DSL**（`click:node:12`，换行分隔，可直接喂
  `parse_plan_dsl`；`/` 只是文档视觉简写，不是 DSL 分隔符——2026-08-27 实测踩坑）。
- **DSL 确定性引用**：`parse_plan_dsl` 支持 `verb:node:<id>`（click/extract 等
  single verbs）→ `ActionNode.node_id`（models append-only 字段）；非正整数解析期
  `PlanValidationError`；target 保留 `node:<id>` 可读性，执行层消费 node_id 回查
  定位器（RPC 接线待办，勿假装已通）。
- **两层协议边界**（用户提示）：外部 parent agent ↔ uws 用 DSL；内部 uws ↔ VLM
  用结构化 JSON 机器契约（引擎 `pkg/vlm Result` vs commander node_id 形态）。
  commander 是「内部→外部」转译+校验点：VLM 概率输出绝不直通动作、不直接写外部
  DSL。内部两处形态后续统一契约（USER_ACTION 候选）。
- **验证**：`tests/unit/test_commander.py` 8 例 + `test_planner.py` node: 2 例全绿；
  Go `cmd/uws` 编译通过、`uws commander --mock` 端到端输出 DSL；round-trip
  commander DSL → `parse_plan_dsl` → ActionNode(node_id) 成立。

## 2026-08-27 混合指挥引擎侧接线落地（v1.5.5）

- **契约**：proto `ActionIntentRequest.node_id`（optional int32，append-only）+ 新增
  `ExportPageTree` RPC；`EngineVersion=v1.5.5`。`GrpcEngineClient.execute_action(..., node_id=)`
  + `export_page_tree(target_id)`；`EngineClient` 协议同步。
- **`uws run --plan`**：`cli.py` run 增 `--plan`/`--plan-file`；`run_task` 把
  `parse_plan_dsl` 的 `steps/initial_url/bindings/result` 编译进 `TaskDefinition`
  （initial_url 注入 `context.variables["initial_url"]`、attach 也经 variables 透传；
  结果 `reply:$var` 用 `_resolve_dsl_result` 插值）。executor `_execute_step` 透传
  `node_id`；`_execute_once` 把 extract 成功结果按 `task.bindings` 写回
  `ctx.variables`（修 `ParsedPlan` 只捕获不消费的 v0.1 缺口）。
- **引擎侧语义修正（重要）**：`node:12` 的 `target_semantic` 含 `:`，会撞引擎
  `semanticRe` 白名单——`validateIntent` 已对 `node_id>0` 跳过字符白名单（仅保留长度
  边界），否则 `uws run --plan "click:node:12"` 会 INVALID_ARGUMENT。
- **测试**：TDD-PLAN-08（node_id 透传）/ TDD-PLAN-09（$var 写回）/
  TDD-CLI-16（--plan 全链路）；`tests/unit/test_cli_run.py` 新增。

## 2026-09-03 目标模式 --debug（goal.py）与京东实测反哺

- **可观测性缺口闭合**：目标模式 VLM 调用全在 Python 侧（Go 只委派），此前每轮只有
  trace 摘要。`run_goal(debug=True)` 逐轮打 stderr：起步/初始化（session/target/
  resumed_url）、快照（URL + 树行数 + ref 数 + 每轮截图路径）、VLM 原始 content/
  usage（`default_vlm_ask(debug=True)` 附加 `_raw/_usage` 返回键）与解析后决策、
  引擎 ActionResult（success/error_reason/current_url）、goto/护栏、重复与停滞的
  换决策第二次征询。每轮截图 `_save_screenshot(..., tag=f"r{round_no}")`，只有
  debug 才落盘；stdout 报告契约不变（Agent 消费 stdout 仍稳定）。
- **VLM「笨」的实测分层（京东电煮锅）**：第一层「不动」——input success=True 但
  URL 不变，VLM 只填框不提交（缺「输入后回车/点搜索」原语意识）；第二层「同案复购」
  ——重复守卫因京东动态首页树签名（_page_signature 全树 md5）每轮变化而失效，等
  同节点连续才触发；触发换决策征询后 VLM 仍回同决策；第三层「看不见成本」——
  每轮 prompt 45k~60k token（93KB AXTree 全量），usage 里 prompt_cache 命中与
  未命中要分开看（京东命中率波动 131~53k miss）。后续治本候选：input 后语义自检+
  提交引导、重复判据对动态树降噪（ref 子集签名而非全树）、prompt 预算化。
