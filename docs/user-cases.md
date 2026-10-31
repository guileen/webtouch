# WebTouch 用户场景与最佳实践 (User Cases & Best Practices)

WebTouch 的设计不仅满足通用的网页测试与数据抓取，更是为了适应前沿的 AI Agent 交互和大规模高复杂度的数据清洗任务。历史展开名 Universal Web Surrogate 与命令名 `uws` 已弃用，现行命令是 `webtouch`。

本文档详细映射了用户的核心期望与 WebTouch 引擎在设计架构上的支持链路，以证明系统的严密性与用户友好性。

---

## 场景一：低 Token 成本的本地浏览器接管（Personal Browser Assistant）

**用户期望**：
> “我可以让我的 agent 操作我的浏览器，并且消耗的 token 很少，比如帮我完成搜索文档（有一定深度，可能到了目标网站，然后要点击详情），获取商品列表和参数，然后返回我想要的信息。并且我可以进一步决定对某个已打开的页面做进一步的操作。”

**系统设计映射与支持**：

1. **接管本地浏览器 (`attach_cdp_url`)**：
   - **设计支持**：在调用 `InitializeContext` 时，允许传入 `attach_cdp_url` (例如 `ws://127.0.0.1:9222/...`)。WebTouch 引擎不再强制新建无头浏览器，而是直接 Attach 到用户桌面正在运行的 Chrome 实例。用户可以亲眼看着 Agent 在自己的屏幕上操作。
2. **极低 Token 消耗**：
   - **设计支持**：基于 WebTouch 核心的 `L1 通用语义知识库` 和 `AC-BM25 + ONNX` 级联分类器，Agent 寻找“搜索框”、“详情链接”或“下一页”时完全在 CPU 内存中离线匹配（单步延迟 <15ms），**绝对不会将庞大的网页 HTML 源码发给大模型**。仅在遇到从未见过的复杂交互形态时，才动用 LLM 进行兜底判断（LLM 调用率设计目标 < 15%）。
3. **精准的数据提取 (`extract` action + `extracted_data`)**：
   - **设计支持**：协调器可以下发 `expected_action: "extract"`，引擎感知目标节点块后，将商品列表、参数解析为结构化数据，直接填充至返回的 `ActionResultResponse.extracted_data` 中，避免开发者额外编写正则或解析器。
4. **有状态的持续多步交互（Session Persistence）**：
   - **设计支持**：使用 `ExecuteActionStream` 双向流。一旦目标网页打开并完成搜索，沙盒（BrowserContext）与流式会话保证了页面状态不被销毁。用户可以基于当前页面继续下发新动作（如“点击第三个商品”、“加入购物车”）。

---

## 场景二：基于 Common Crawl 的服务器级智能爬虫（Intelligent Crawler）

**用户期望**：
> “在 Linux 服务器上，我可以针对 Common Crawl 的海量网页，从某个关键字、领域、入口网站出发，完成智能的浏览和内容的提取。我不需要进行复杂的 HTML 解析，就能将浏览过的网页结构化存储，为之后的搜索和排序准备语料。”

**系统设计映射与支持**：

1. **摆脱复杂 HTML 解析（AXTree 语义感知）**：
   - **设计支持**：WebTouch 并不是一个基于正则表达式或 BeautifulSoup 的系统。WASM 探针在底层提取的是 **可访问性语义树 (AXTree)**（结合了 `role`, `semantic_text`, `zonal_keywords` 等人类视觉线索）。这意味着爬虫不关心页面是用 React、Vue 还是嵌套了多少层 `div`，它直接按语义寻找“正文内容”、“作者信息”、“下一页”等关键信息块。
2. **大规模吞吐与成本控制（单核高频计算）**：
   - **设计支持**：在 Linux 服务器跑规模化爬虫的最大痛点是资源爆炸。WebTouch 的 WASM 探针 + C++ 物理层强制了 16MB 内存截断，配合动态 Target 池管理（最大并发受 `CPU_Cores * 1.5` 约束），可以极高密度地爬取结构化语料，而且不需要为海量无关页面承担 LLM API 推理的高昂费用。
3. **高质量结构化语料自动降噪**：
   - **设计支持**：系统在 `NodeFeature` 提取中使用了 **AST 降噪哈希** 和 **Zonal Keywords**，天然剥离了网页中的动态混淆脚本、十六进制内存地址和无关广告弹窗。最终输出的数据自带干净的语义骨架结构，直接作为高信噪比语料入库，对后续检索（Search）与排序模型（Rank）极其友好。
4. **CC 语料工具链（2026-08-20 续33 已移交 search-engine）**：
   - `cc-fetch` / `parse-warc` / `search` 三条 CC 语料工具命令已随代码整体迁至
     `labs/world-replay/search-engine/`（`se` 包，`python -m se cc-fetch|parse-warc|search`），
     webtouch 仅保留在线爬取（run/crawl/api）保持义肢纯净；
   - 完整闭环 = `se cc-fetch` → `se parse-warc` → `se search`
     （真实数据 → 离线语义提取 → BM25 检索），仅下载阶段需要网络；
   - 离线蒸馏环（WebTouch pipeline）仍以 Common Crawl 语料为原料，与 search-engine
     的语料管道互为上下游（详见 search-engine 的 `docs/cc-batch-pipeline-and-search-service.md`）。

---

## 场景三：将大模型 Web UI 封装为 API 访问（Web-to-API Surrogate）

**用户期望**：
> “我可以让 Agent 直接使用 Gemini 网页 / Deepseek 网页，开启新会话，进行问答，并且让这种问答过程像访问 API 一样方便，就好像真的在看屏幕一样。”

**系统设计映射与支持**：

1. **SPA 软路由与异步流式响应应对**：
   - **设计支持**：Gemini/Deepseek 这类现代 AI 对话界面是重度单页应用（SPA），传统自动化工具很难判定大模型是否“回答完毕”。WebTouch 引擎内置了底层的异步状态机：
     - 大模型流式输出（打字机效果）时，引擎的网络和 DOM Mutation 处于高度活跃期。
     - WebTouch 提供的 `wait` 语义操作结合底层 `Network Idle`（网络静息）交叉判定，能像人类的眼睛一样，准确等待“生成停止”或“回复完成按钮”出现后，再通过 `extract` 提取完整回答，实现了类似 API 同步调用的顺畅体验。
2. **规避严格的反爬虫策略（Cloudflare/DataDome 拦截）**：
   - **设计支持**：直接自动化访问大模型 Web 极易触发高阶机器人检测。WebTouch 在底层防御上提供：
     - **物理指纹隔离**：代理隔离、时区绑定与 Fitts 动力学曲线模拟人类真实的非线性鼠标滑动轨迹。
     - **挑战逃逸 (`CHALLENGE_INTERCEPTED`)**：遇到 WAF 验证码拦截时，系统不会死循环盲试导致封号，而是抛出专有状态码（`ContextState = 4`），触发外部 Webhook 交由专业打码/人工接管平台解决，随后无损恢复该网页会话。
3. **完全复用账号身份**：
   - **设计支持**：在 `InitContextRequest` 中，支持通过 `cookies` 字段全量注入从宿主机提取的用户身份票据。Agent 接管后直接呈现为已登录的 Plus/Pro 状态。结合上述能力，用户可以用几十行代码，把任何复杂的 Web 对话界面封装为一个稳定、原生的 RESTful API 供自己调用。

4. **连续对话 / 多轮 / 开新会话映射到标准 API（2026-08-25 实测落地）**：
   - **多轮回答提取**：`.ds-markdown` 回答块随轮次追加，extract 必须取**最后一个**
     回答块，且发送后要等到「出现不同于发送前基线的新文本并稳定 1.5s」才算完成——
     否则第二轮会把第一轮的旧回答原样返回（真实页面复现过：第二轮返回了第一轮的
     「信号满格，收到。」）。
   - **新会话 = 新 Tab（2026-08-25 晚，v1.5.3）**：每个 `session_id` 固定绑定一个
     浏览器标签页（引擎命名上下文）；`POST /v1/sessions` 开新 Tab、
     `DELETE /v1/sessions/{id}` 关 Tab、chat 请求的 `session_id`（未知 id 自动
     开新 Tab）与 `new_session: true`（旧 Tab 关闭+同名重建）都走「新 Tab」语义，
     上下文隔离靠标签页物理隔离，不再依赖页面「开启新对话」按钮；标签被手动关闭
     后自动重建重试。**DeepSeek 请求模式 ≠ 会话已锁定模式时，API 自动开新 Tab
     兑现模式**（关旧 Tab + 重建 + 切换，2026-08-26 修复「专家模式没选对」），
     同一模式多轮不重建。此前实现的 `text=` 选择器与 `new_chat_button` 规则保留
     作同页重置等场景的底层能力。
   - **标准 API 映射约定**：Web-to-API 是**有状态**代理，页面自持上下文——
     连续对话只传增量最后一条 user 消息；要隔离上下文就换 `session_id`。
   - **流式 SSE 与干净文本（2026-08-25 深夜，方案 1）**：`stream: true` 已落地——
     `POST /v1/chat` 以 `text/event-stream` 推送 `choices[].delta` 增量；
     文本改走 CDP Network 层捕获的 SSE JSON delta（`context_hint["prefer_sse"]`），
     联网搜索的引用角标碎片（`-1-4-7`）在源头剔除，不再依赖 DOM innerText 二次
     渲染（详见 GLOBAL.md §5 裁决 #35）。
## 场景四：真实复杂博客/专栏正文与细粒度元数据提取（Medium / Substack 实测）

**用户期望**：
> “在一篇包含广告、推广、付费弹窗、Clap/点赞悬浮、复杂代码块与抽屉式评论区的真实 Medium 博客中，提取干净的正文 Markdown（带 YAML Frontmatter）、标题、作者、作者头像、发布时间与评论列表，且不被反爬阻断，单页提取耗时降至毫秒级。”

**系统设计映射与实测优化解法**：

1. **双通道元数据与正文解析**：
   - **通道一（JSON-LD / OpenGraph 优先）**：直接从 `<script type="application/ld+json">` 零成本解析标准结构化对象（`headline`, `author.name`, `author.image`, `datePublished`），准确率 100%；
   - **通道二（WASM 树密度分析）**：在无结构化标签页面中，WASM 探针按「文本密度 + 标签深度 + 负向关键词过滤」自动剥离侧边栏、推荐位与弹窗，输出纯净 Markdown。
2. **常驻注入（Pre-instantiation）与批量信号抽取**：
   - 通过 `Page.addScriptToEvaluateOnNewDocument` 在页面生命周期初始化时编译挂载 `window.__uws_probe`；
   - 一次 WASM IPC 调用打包抽取 `title, content, author, avatar, comments`，使复杂 DOM 抽取耗时从秒级降低至 `< 5ms`。
3. **混合多路召回（BM25 + Semantic Vector）应对意图扩展**：
   - 面对近义词与词性扩展，通过字面词典（AC）与轻量向量分类器双通道召回，结合 RRF 融合打分，Top-3 命中率达 92%~96%。
