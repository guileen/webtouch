# pkg/vlm 记忆

## 2026-08-27 VLM 感知兜底（用户指令直接实施）

- 动机：uws 语义层只「听」可交互元素（button/a/input/role），/store 书卡是纯 Vue div
  （0 个 a 标签）→ 书名 `no_match`；VLM 兜底 = 截图看一眼 → 候选 → 确定性验证执行。
- 模型：`deepseek-v4-flash-vision-exp`（2026-08-21 上线），OpenAI 兼容 chat/completions，
  base64 内联 + `detail=low`，单图 ≤384 token 按 V4-Flash 文本价。
- **输出是文本、无原生 bbox**：prompt 要求 `x_ratio/y_ratio`（元素中心 ÷ 截图宽高），
  引擎换算为视口 CSS 像素（截图即视口尺寸，1:1）。
- **VLM 只产候选，不产结论**：坐标/文本/选择器都要回到确定性层执行（ClickAt /
  ExecuteActionWithSelector），成功才采信。
- **自进化 = learned.json**：`(normalized URL + semantic) → {selector|semantic, node_text,
  hits}`，原子写；下次同页面同语义先查 learned，命中即不再调 VLM（"下次就会听了"）。
- 学习键用 NormalizeURL（去 query/fragment）；命中失败（页面变了）自动落回 VLM 再学。
- 隐私/成本：截图含页面内容，发第三方 API 需用户知情；默认 `--vlm-fallback` 关闭，
  API Key 走环境变量 `UWS_VLM_API_KEY`，不落日志/文档。
- **针对性截图/缩放（核心能力）**：图片进模型前自动缩放——小于 ~384×384 总像素等比
  放大、更大等比缩到 ~800×800 等效；**单图 token 上限 384**（2000×2000 与 5000×5000
  相同），多图各自独立计费；`detail=low` 先缩到 512×512（更快更省），
  `original/high` 保留原图。因此全页整图会丢小字细节——uws 用「先粗后精」：
  全页截图粗定位 → CDP clip 裁剪目标区域 → 放大二次精修坐标 → ClickAt。

## 2026-08-27 下午实测：成本与可靠性（小说站 /store 真实闭环 + 成本代理度量）

- **成本实测**（官方峰谷价：V4-Flash 视觉同价）：输入缓存未命中 ¥1.5(空闲)/¥3(高峰)
  百万 token、输出 ¥4.5(空闲)/¥9(高峰) 百万。
  - Godfall 书卡点击成功（粗 642 + 精 602 tok）≈ ¥0.003~0.006（首次 7.49s）；
    learned 命中 0.76s / 0 调用 / ¥0。
  - **失败场景成本陡增**：Wilder 13.6s / 1927tok（推理 1470/1476=100%）、
    Search books 18.3s / 2329tok（推理 98%）→ 单次 ¥0.007~0.019。
  - 结论：**钱几乎可忽略；真正要治理的是延迟与确定性**。
- **可靠性反哺**：
  - 单一 `temperature=0` **挡不住 long reasoning**——失败时 completion 几乎全是
    reasoning token，速度 4~5 倍变慢、价格 4~6 倍变贵。
  - 默认 `--vlm-timeout-ms` 15000 偏短，长推理会触发 `context deadline exceeded`
    （Wilder 实测）。建议 20~25s。
  - `parseResult` 只容忍裸 JSON/markdown 围栏；模型若夹带思考链或追加文字会导致
    `no json object`。建议 prompt 显式「只输出 JSON、不要思考过程」。
  - 失败（candidates exhausted / 超时 / 空文本）与成功在引擎侧坍缩成同一种信号
    （`no_match`），无法统计真兜底率；建议引擎对「已调用 VLM 但未验证」给不同状态码。

## 2026-08-27 傍晚：关闭 thinking 是「看→匹配→快猜」的真正开关

- **诊断（A/B，同一张 /store 图）**：旧 prompt（"最匹配/精确到中心"）触发模型 5484
  字符思考链（`reasoning_content`），reasoning 155~1574/输出 token，单次 3.9~14.9s。
  给 prompt 加「不要推理」反而**更多**思考（784/502/662）——负向指令让思考模型更卖力
  确认自己没在想，**不是提示词措辞问题，是模型默认开启 thinking 模式**。
- **根治**：请求体加 `"thinking":{"type":"disabled"}`（DeepSeek 视觉模型支持）→
  **reasoning 归零**，完成 43 token，**单次 1.1~1.5s**（旧 3.9~14.9s）。
  备选：`"reasoning_effort":"none"` 也归零但 10s（疑似 fallback）；`max_tokens<=150`
  能压 reasoning 但坐标更差；**`thinking:disabled` 是唯一又准又快的开关**。
- **提示词改写**：把「找到最匹配可点击元素/尽量精确到中心」换成「看图作答不要推理，
  眼睛一瞄报中心比例」——从语义推理题拉回视觉检索；仍是同一 JSON schema，引擎解析不变。
- **配套**：加 `"max_tokens":300` 防夹带思考尾巴撑爆 JSON（实测不截断正常输出）。
- **实测收益（新 build 真闭环）**：/store「Godfall」首次 **4.63s**（旧 7.49s/22.96s）、
  每调用 318~378 token（旧 602~2069）、0 推理 token；learned 命中仍 0.66s。
  单次成本从 ¥0.003~0.006 进一步降到 **¥0.001~0.002**（按峰谷价）。
- **注意**：`thinking:disabled` 是 DeepSeek 特定形态；其他 provider 需按各自 disable
  形态（`reasoning_effort:none`/`enable_thinking:false` 等），建议 `Client` 抽象出
  `thinking` 配置项，不要硬编码在 `Resolve` 里。

## 2026-08-27 深夜：『粗看细看返回』两段式闭环在真实 Chrome 上验证（用户指令实测）

- **能力定位（先给结论）**：「粗看细看返回」指 `--vlm-refine`（默认 true）的两段式
  **粗看（全页 VLM 候选）→ 裁剪区域（60%×45% 视口，钳制）→ 细看（二次 VLM 精修）→
  回读元素 CSS 路径并点击**。它是引擎**内部 refine→click 编排**，不是对外返回粗+细坐标。
  `ActionResultResponse` 只回 `success/confidence/error_reason/extracted_data`，没有坐标/
  区域字段；细看结果通过引擎日志暴露（`vlm: refined to x,y (crop ...)`）。
- **真实 Chrome 闭环（用本地模拟 DeepSeek 端点注入粗/细候选，不花真实 key/费用）**：
  `/store` 点「Godfall」（纯 Vue div 书卡，语义层 0 个 a/button → `no_match`）。
  引擎日志铁证：`vlm: refined to 238,166 (crop 103,51 247x411)` →
  `vlm: clicked at 238,166 ... selector=div#app > ... > p.intro` → 导航到
  `/detail/2084223467137273856`（正文确认为 Godfall）。learned.json 写入真实 CSS 路径，
  **第二次命中 `learned hit ... 0.64s`（0 次 VLM）**——「粗看细看 + 自进化」双闭环成立。
- **反哺点（影响后续维护）**：
  1. **VLM 兜底是硬门禁**：必须先 `--vlm-fallback` + `UWS_VLM_API_KEY`（环境变量）才启用；
     key 缺失时引擎只在日志打「VLM 兜底禁用」，`vlmEnabled()` 处静默关断。诊断时先确认
     key 存在（用 `launchctl getenv` / `printenv` 只查存在性、不打印值），否则全白跑。
  2. **后台进程随 exec 父 shell 退出被 SIGHUP 杀**：`cmd &`/`nohup` 在一次性 exec 里不保活，
     需用长驻 PTY 会话（`tty=true` 的 exec）或 `setsid`。本地 mock 端点同理会话结束即断。
  3. **真实语义层对「非交互/任意文本」仍弱**：Bing 搜「A醇 夜用精华 成分」，语义层能
     extract 到 `搜索` 这类交互元素（0.03s），但 `正文`/`页面正文`/进入具体结果均
     `no_match`/`occluded`/`verification timeout`——**读任意区域文本`正是 VLM extract 待补**，
    也是此前「提取路径假闭环」要治的（extract-only 只写 node_text、无确定性定位器）。

## 2026-08-27 VLM 主控会话（GLOBAL §5 #52 落地，用户「思考方案，开始执行」）

- **能力定性**：把 `pkg/engine/vlm_fallback.go` 的 `vlmTry` 从「固定先粗后精」重构成
  **VLM 主控会话**：`Resolve(图, 请求上下文) → Result{action, ...}`，action ∈
  `click / crop_zoom / extract / back / done / impossible`。VLM 持有 uws 能力原语清单
  （`vlm.Capabilities(action)` 生成），自决何时裁剪、后退、判定无解。
- **决策流向**：主控循环每轮给当前图（首轮整页低分，`crop_zoom` 后给裁剪图高清），
  VLM 返回动作；`crop_zoom` → 引擎裁剪放大回喂（可多轮）；`back` → 浏览器后退重截；
  `done/extract` → 取文本；`impossible` → 无解；轮数上限 `--vlm-max-rounds`（默认 5）防死循环。
- **append-only 兼容**：`Result` 追加 `Action/WRatio/HRatio` 字段；模型若仍输出**无 action 的
  历史候选**（只 found/text/x_ratio/y_ratio/selector），引擎走旧 `vlmLegacy`「粗看→细看→点击/提取」
  路径——真实 Chrome 双闭环与既有 6 例引擎测试不破坏。`--vlm-refine` 退化为「历史坐标候选的
  自动裁剪兜底」，主控 path 的裁剪由 VLM 用 `crop_zoom` 自决，不再受其约束。
- **失败区分信号（§50 反哺 c）**：`vlmTry` 返回 `vlmOutcome`（Skipped/Success/Transient/Exhausted）。
  `Skipped/Transient` → 调用方保持 NOT_FOUND（失真率可统计）；`Exhausted`（impossible/轮数耗尽）→
  调用方返回 `Aborted("vlm main-control exhausted")`，与纯兜底失败（NotFound）区分开。
- **`Resolve` 契约变更**：签名从 `Resolve(ctx,png,w,h,target)` 扩为 `Resolve(ctx,png,w,h,ResolveRequest)`
  （Target/Action/Capabilities/Round/Crop）；`MockClient` 记录 `Reqs` 断言上下文透传，
  `DeepSeekClient` 的 prompt 把动作集合与「要不要 crop/back」作为上下文喂给模型。
- **测试**：新增主控 5 例（impossible→Aborted、crop→click 且 clip/坐标正确、back→重截点击、
  轮数上限降级、done(extract) 取文本）；全量 `go test ./...` 绿、Python 131 passed/12 skipped、
  gRPC 契约 11 passed、gofmt/vet 干净。
- **遗留（评审点，见 GLOBAL §5 #52）**：
  1. 「uws 直接理解复杂请求并自动分步」= **协调器/引擎自动规划（auto-planner）**，未在本轮实现（这是
     下一步最大待办，不是一个引擎原子改动）；当前门控仍是「卡住（no_match/C<θ_low）才唤 VLM」。
  2. `back` 已加 `browser.HistoryNavigator`（CDP 经 `history.back()`，sim 可测）；「后退/多轮」状态机
     归属（引擎会话 vs 协调器）待定。
  3. 真实 DeepSeek 是否稳定输出 `action` 字段、`crop_zoom` 比例是否可靠，需真机冒烟验证（无 key 时
     先走历史候选路径兜底，行为不回归）。
- **描述性修复（2026-08-27 复测反哺）**：extract 主控提示词从「找到文字X」改为
  「照抄与目标相关的可见内容」（价格带数字单位、成分列全、标题照抄）——旧提示词会让
  模型只回字面标签（如「价格」而非「226元」）；JSON 解析容忍字符串内裸换行
  （`sanitizeJSONNewlines` 转义后重试），否则成分表等多行文本报
  `invalid character '\n' in string literal`。实测：smzdm 列表页
  `extract:商品价格` 返回「226元（需用券）318元」、文章页返回「226元（需用券）329元」。
  遗留：主控无 scroll 动作，折叠区内容（商品介绍成分段）需滚动才能读；
  结果开新标签页需 Attach（协调器缺口）。
- **配置默认化（2026-08-27）**：`--vlm-fallback` 默认 true（`--vlm-fallback=false`
  关闭）；API Key 自动读 `UWS_VLM_API_KEY` || `DEEPSEEK_API_KEY`
  （`cmd/uws-engine/vlmAPIKey()`，单测锁定）。主引擎 50051 已按此重启（launchd
  会话经 `launchctl setenv UWS_VLM_API_KEY` 注入，Key 不回显）。**2026-08-27 当晚已改用
  `uws daemon` 托管（取消 launchd）**：由 CLI 以 Setsid 拉起引擎，继承 CLI 所在 shell 环境，
  故 `DEEPSEEK_API_KEY` 直接生效，无需再 `launchctl setenv`；引擎 Key 仍不回显、不进日志/交接。

## 2026-08-27 混合指挥 node_id + learned 回写（v1.5.5）

- **`Result.NodeID`（append-only）**：`pkg/vlm` `Result` 追加 `node_id` 字段（0=未用，
  传统坐标/选择器路径不变）；`parseResult` 同步解析。这是「坐标 vs node_id」两种内部
  形态走向统一契约的第一步，`vlmTry` 主控循环尚未消费（仍走截图/坐标候选）。
- **extract 假闭环修复**：新增 `browser.VLMProbeSession.ExtractAt(x,y)`（cdpSession
  经 `elementPathAt` + 标记元素读文本；SimCDPSession 确定性模拟）。引擎
  `vlmExtractEntry` 在坐标命中时回读真实 selector 写 learned（修「extract 只写
  NodeText、无定位器 → learned 判 stale 照烧 VLM」，GLOBAL §5 #50）。**遗留**：主控
  `done/extract` 只回文本无坐标时仍 NodeText-only（ExtractAt 仅坐标命中生效）。
- **node_id 成功回写**：`executeByNode` 成功后 `learnNodeHit` 用命中元素证据
  （selector+可见文本）写 learned「页面+元素文本→定位器」（键=元素可见文本）。

## 2026-08-28「义眼」EyeClient（用户「只需一条原语」设计）

- **定位转变**：用户明确不需要那套「测试/规格对比」原语（measure/spawn/diff/几何断言），
  只需一条 `uws chat "问题"`（或顶层 `uws -p "问题"`）。把 UWS 从「义肢」（VLM 帮引擎
  找坐标执行）变成「义眼」（VLM 直接看图/图对图/站点认知给反馈）。
- **与 agent 的区别**：义眼**不产可执行动作、不规划多步、不自主执行**。它就是「喂图 + 问一句
  → 视觉模型作为有认知的体验评审者返回判断」这一件事。给 Agent 用（默认结构化 JSON），
  也可 `--plain` 出自然语言。
- **实现**：`pkg/vlm/eye.go` 的 `EyeClient.Ask`（多图 OpenAI 兼容，content 数组文本+若干
  image_url；`thinking:disabled` 降本；`max_tokens:1000` 防 JSON 撑爆）+ `ParseEyeContent`
  （容忍围栏/杂音/裸换行，复用 `sanitizeJSONNewlines`）。`cmd/uws/chat.go` 接 `chat`/`-p`，
  `--img`（本地读 bytes/URL 透传）、`--url`（经引擎 InitializeContext+CaptureScreenshot 采样，
  每站点独立 context_name 防 Cookie 串）、`--page`（截当前 default 会话）、`--json`/`--plain`。
- **坑**：①`InitContextRequest` 必须带 `task_id`（否则 `InvalidArgument: task_id required`）；
  ②`--plain` 不是「显示完再回退」，而是整个走 `Ask(JSON=false)` 让模型直接出自然语言
  （否则模型仍倾向给 JSON）；③`parseChatArgs` 不读盘、`--img` 延迟到 `runChat` 再读，
  便于测试与尽早暴露路径错误；④站点采样在 `sim` 引擎下不真实导航（两张不同 URL 可能是
  相同模拟字节，模型会判「一致」），真实对比需 `--browser cdp`。

## 2026-08-28 义眼「图像证据原语」（用户「像素级对比可做」指正）

- **用户指正**：像素级对比是可行的——不是让 VLM 裸眼看一张图猜，而是**让 VLM 调用 UWS 的
  内置能力**（色相偏移叠加、局部放大、区域颜色统计）去拿证据。`--url`、`--plain` 都不是
  能力问题，而是**提示词问题**（没写清约束/没给证据）。
- **落地（pkg/vlm/eyetools.go，纯标准库，不依赖模型/会话）**：
  `DiffImages`（逐像素 diff → 差异 overlay 图，洋红=差异、灰底=第二张；返回
  `DiffStats{ratio,bbox,max_channel}`，尺寸不一致按最小宽高比较）、`RegionAvgColor`
  （区域平均色 `R,G,B`，供 VLM 当比色卡）、`ZoomImage`（区域最近邻放大）。
- **提示词修复**：`--plain` 现在 `Ask(JSON=false)` + 动态系统提示「请用简明中文直接回答、
  不要输出 JSON、不要用 markdown 围栏」（模型仍偶尔自我 JSON → 但这已是强约束）。
  `--url` 站点截图「几乎相同」（diff ratio<0.1%）时注入
  `identicalShotsWarning`：让模型回 `unknown` 而非误判「两站一致」（sim 假阳性的提示词层抑制）。
- **实测**（`--diff` + 两张「左上红块 vs 右上蓝块」）：VLM 基于差异图给出量化判据
  「差异像素 8.3%、最大信道差 200、bbox 20,20,269,69 覆盖两方块」——这正是「关键差异出来
  了」的落地。**要点**：diff 是确定性证据，VLM 只做解释，不背对像素猜。

## 2026-08-28 义眼「自然语言自决路由」（用户「不传 --diff/--url 也要能很好完成」）

- **用户指正**：`--diff`/`--url` 这两个设计有用（更快、少一轮 LLM 判断），但**必须确信不传
  这两个参数、只问一句「帮我检查 a.com 和 b.com 的 differences」也能很好完成**。即自然语言
  入口要自能走到「提取站点 → 采样 → 像素 diff → 量化判据」，而不是靠调用方显式拼 `--url/--diff`。
- **落地（cmd/uws/chat.go，纯字符串路由）**：`extractSiteURLs`（scheme URL + 裸域名，裸域名补
  `https://`）、`detectCompareIntent`（对比/比较/差异/一致/diff/vs… 关键词）、
  `applyNaturalLangRoute`（条件：未显式给视觉输入 + ≥2 URL + 比较意图 → 自动填 opts.urls）、
  runChat 内自动开 `--diff`（≥2 张且前两张有字节数据 + 比较意图）。门控刻意保守，避免把
  「打开某站看看」这类单站请求误判为对比而误开引擎。
- **实测（真实 Chrome cdp，AutoRoute 全程自动）**：
  - `uws chat "帮我检查 example.com 和 wikipedia.org 的 differences"` → `verdict=different`，
    `checks` 4 条（布局/内容/像素量化 96.3%/视觉风格），confidence 1——自然语言入口完成得很好。
  - `uws chat "帮我检查 example.com 和 example.org 的 differences"` → 两站页内容几乎相同，
    `identicalShotsWarning` 触发 → `verdict=unknown`（**不误判一致**，安全降级）。
- **边界（诚实）**：两站渲染内容「几乎相同」时走 unknown（√ 防 sim 假阳性），但这意味着
  「真一致的克隆」也会被保守判 unknown 而非 confident consistent——这是刻意取舍；若要让
  「内容一致」也能给 positive verdict，需在 `identicalShotsWarning` 里区分「真实导航且内容
  一致」与「未导航/加载失败」，待用户拍板再调。

## 2026-08-28 义眼「主控循环」（用户：要不要多轮由 VLM 自决，引擎要有能力，但要单轮优先）

- **用户定调**：要不要多轮是任务性质决定的、由 VLM 自主决策；引擎必须有能力支持多轮；但 VLM
  决策时要尽量单轮完成，避免过慢。
- **落地（多轮 = 能力而非默认路径）**：`chat` 非 `--plain` 默认走主控循环 `eyeMainControl`：
  VLM 每轮返回 `EyeTurn{action:conclude|zoom|region_color|diff,...}`；`conclude` 直接给
  `EyeResult` 收尾；请求证据则执行确定性原语（`DiffImages`/`ZoomImage`/`RegionAvgColor`）把结果
  作为新证据追加、进入下一轮。`pkg/vlm` 增 `EyeTurn`/`ParseEyeTurn`/`EyeControlSystemPrompt`
  （`capabilities` 动态注入）+ `EyeRequest.System`（覆盖系统提示）；`cmd/uws/eye_loop.go` 实现循环、
  `runEyeEvidence`、`describeImages` 与 `clip`。
- **单轮优先**：系统提示强约束「现有证据够就直接 conclude；确不足才请求一个原语；每轮最多一个；
  不空转」。常见站点对比因已自动注入站点截图 + diff 证据，**单轮即收尾**（实测
  example.com vs wikipedia.org → `different/96.3%` 一轮）。轮数上限 `eyeMaxRounds=4`，超限安全降级。
- **验证**：单元（`eye_turn_test.go` 3 例；`eye_loop_test.go` 4 例 + mock VLM 多轮主控 1 例：
  round1 请求 zoom → round2 conclude，calls=2、证据图追加）；端到端真实 Chromium 主控循环单轮收尾。
- **定位澄清**：仍是 OpenAI 兼容 chat + **自定义 JSON 契约**（`action`/`EyeTurn`），**非标准
  function-calling**；「能否多轮」取决于任务是否需要额外证据——不需要就绝不空转（不像通用 agent
  loop 那样每步都建模）。

## 2026-08-31 VLM 主控缺 input 动作（Airwallex 实战，用户反馈「VLM 兜底也不行吗」）

- **现象**：附着已登录 Chrome 操作 Airwallex Billing（规则库未覆盖站），`click:新产品`
  VLM 主控能成功（`main-control click at 1814,79` → create 表单打开、产品名 DOM 已被填），
  但 `input:产品描述=...` 反复 `vlm: impossible`，值填不进去；extract 同样只回标签词。
- **根因**：`vlm.Capabilities()` 动作集合只有 click/crop_zoom/extract/back/done/impossible，
  **没有 input 动作**——VLM 对输入框只能回 click（引擎只聚焦不填值）或 impossible；且
  `promptFor` 非 extract 分支是「找到文字X」，对空输入框/占位符会误判 impossible。
- **修复**（TDD-VLM-12）：
  - `ActionInput = "input"` 枚举 + `Capabilities()` 注入 input 动作样例；
  - `promptFor` 增 input/fill 分支：目标=定位「输入框/文本域/下拉框」，报中心坐标，
    「按钮不算」「聚焦后引擎负责填值」「空输入框/占位符也算」；
  - 引擎 `vlm_fallback.go` 主控循环 `case vlm.ActionInput` → `vlmActOnInput`：
    selector 直填 → 坐标点击聚焦后按返回 selector 填值 → 文本语义重试；成功后回写
    learned（下次同页面免 VLM）。
  - 测试：`TestDeepSeekInputPromptLocatesInputBox` / `TestCapabilitiesIncludeInput`
    （pkg/vlm）+ `TestVLMMainControlInputFillsValue` / `TestVLMMainControlInputDirectSelector`
    （pkg/engine）。
- **经验**：① 语义探测新站点先读 `document.body.innerText` 找真实按钮名（Airwallex 中文
  「新建产品」实际叫「新产品」），别按英文直译猜；② VLM 主控动作集合要与引擎可执行原语
  对齐，缺一个动作 = 一整类语义在该类站点上永远兜不住；③ 输入兜底必须把「定位」和「填值」
  解耦——VLM 只报坐标/selector，值由请求携带（`req.GetValue()`），引擎负责聚焦与填入，
  避免视觉模型背值导致的幻觉。

## 2026-08-31 补：input 兜底实机两连坑（Airwallex 表单，TDD-VLM-13）

- **坑 1（坐标落容器）**：VLM 对「描述」报的坐标 (960,162) 落在 label 上而非 textarea
  （多输入框表单视觉分不清），旧路径 ClickAt 后按容器 selector 填值落空。修复：
  `VLMProbeSession` 新增 `InputAt(x,y,value)`——elementFromPoint → 自身/后代/祖先/
  兄弟兜底找 input/textarea/select → 聚焦填入 → 返回**输入框自身** selector（learned
  回写后下次免 VLM）。引擎 `vlmActOnInput` 候选 2 优先走 InputAt。
- **坑 2（语义匹配不到 label）**：Airwallex 描述 textarea 无 placeholder/aria-label，
  `findElementOpt` 候选集合（button/input/textarea/select/role…）不含 label，语义
  「描述」一个都匹配不到 → no_match 后 VLM 坐标把「描述」写进 productName（实测 DOM
  productName=「订阅有效期…」）。修复：候选集合加 `label` + `leafOf` 支持
  label→control（`label.control`/`htmlFor`）下钻；`isContainerLike` 加 label。
- **实机验证**：`input:产品名称=auto-renewable week` + `input:描述=订阅有效期内，
  可阅读本书城所有短篇故事` 两字段 DOM 均正确（未提交）。
- **经验沉淀**：表单类页面语义定位 = 「label 文本 → 关联控件」，不是「输入框自身
  文本」；VLM 坐标只作最后一层兜底，且必须与就近输入框原语（InputAt）配合，否则
  多输入框表单必错框。规则库候选集合要覆盖 label（表单骨架），别只盯可点击控件。
