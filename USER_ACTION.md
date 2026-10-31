# webtouch/USER_ACTION.md —— 待确认 / 待授权 / 备选方案记录

> 用途：/loop 自律执行中，权限受限无法推进、或方案取舍影响不大时，
> 记录「后续再处理」的事项；每轮循环先检查本文件是否有可推进项。

## 待确认/待授权（需用户决策）

### 0. 重启调试 Chrome 让反节流启动参数生效（2026-09-12，GLOBAL #92 S1）

- **要做什么**：重启监听 9222 的调试 Chrome（`~/.uws-chrome-debug` 配置目录），
  使其带上 `--disable-backgrounding-occluded-windows --disable-renderer-backgrounding
  --disable-background-timer-throttling`（已进 `coordinator/attach.py` 等启动路径）。
- **为什么需要用户**：该实例当前正被 ChatGPT 桌面端挂载（同一调试端口），Agent 不主动杀用户进程；
  重启会丢失该窗口的临时标签（登录态在 profile 目录，保留）。
- **不需要用户懂原理**：一句话 ——「挑个方便的时刻让我重启一下调试 Chrome」。
- **生效后验证（Agent 做）**：把前台切到别的应用（飞书/iTerm）后复跑
  `UWS_S1_RUNS=3 bash scripts/verify_s1_occlusion.sh`，对比 baseline 组是否复现遮挡挂起。

### 1. 真实 ONNX 运行时适配器接线（TDD-ONNX-01/03 集成层完成条件）

- **背景**：`pkg/classifier` 已点亮决策核（Scorer 接口 + θ 0.85/0.50 三分），
  但真实 ONNX 推理未接线。本机 macOS 13 x86_64 无 onnxruntime wheel/共享库
  （见 GLOBAL.md §5.5）。
- **候选方案**：
  - A：Go 侧引入 ort-go（CGO 绑定），模型加载 + 空特征推理 Sanity Check
    （对接 `pkg/knowledge` 的 `OnnxReady` 字段），需在可用 onnxruntime 的环境
    （Linux CI/Docker）或本机降级装旧版 dylib；
  - B：推理下沉 Python 服务（grpc 侧 service），Go 经 gRPC 调用，绕开本机 CGO；
  - C：保持 mock 契约，等 pipeline/exporter（M6B）产出真实模型后再接线。
- **建议**：C（最小依赖），导出器落地后连同 TDD-ONNX-02 基线一起做。
- **状态更新（2026-08-20）**：环境阻塞已解除——本机 macOS 13 x86_64 pin
  `onnxruntime==1.23.0`（官方 macosx_13_0_x86_64 cp311 wheel）即可安装；
  `train_onnx.py` 已产出真实 LightGBM→ONNX 模型（11KB、parity 校验、AUC≥0.90）。
  Go 侧接线仍待决策（A 案 ort-go CGO vs B 案 Python scorer），未动 Go 引擎
  （保持零依赖、无回归风险）。
- **状态更新（2026-08-20 续21，A 案已落地）**：用户拍板 A（ort-go CGO），
  理由见操作记录续20。实现：`pkg/classifier/onnx.go`（OnnxScorer +
  `Features` 与 train_onnx.py 逐位对齐）+ `knowledge.Artifact.OnnxModel` +
  引擎 `--onnx-runtime-lib` + KB 热更/回滚同步重建 + 页面候选 Confidence=0
  节点真实打分（L2 同义词保持背书）。版本配对 ort-go v1.23.0 ↔ ORT 1.23.0；
  `make ort-lib` 从 .venv 提取 dylib。Go 单元/集成 + 契约层
  `test_tdd_onnx_real_model_loaded_contract` 全绿；无 ORT 库时自动降级 mock、
  测试自动 skip。B 案（Python scorer 服务）弃用：双进程模型版本漂移会破坏
  KB-06 回滚一致性，且违背在线环单二进制/零常驻服务价值观。

### 2. TDD-ONNX-02 性能基线（模型 <1MB / CPU 推理 ≤5ms）

- 依赖真实 ONNX 模型产出（num_leaves=31），`benchmarks/bench_classifier.py`
  骨架已就位，模型可用后点亮。
- **状态更新（2026-08-20，已点亮）**：`benchmarks/bench_classifier.py` 实跑真实
  模型——11KB < 1MB，CPU 200 次推理 P50/P99 ≤5ms；`train_onnx.py` 内建体积上限
  （超限抛 TrainError）防规格退化。

### 3. ONNX 真实模型接入（TDD-ONNX-01/03 集成层 + TDD-ONNX-02 基线）

- **状态更新（2026-08-19 晚）**：gRPC 引擎接线已完成（TDD-API-01/02/03/04/06/07 契约层点亮），
  **匹配/分类集成层已点亮**：`pkg/matcher` 新增 `site_synonyms`（L2 TENTATIVE 同义词 0.2 扰动）
  与 `uws_negative_keywords`（黑名单过滤），引擎 runAction 走候选池管线；
  `tests/integration/test_matcher_classifier.py` 的 TDD-MATCH-02/04、TDD-ONNX-01/03
  经真实引擎 gRPC + mock scorer 全部点亮（`make pre-commit` 全绿）。
- 仍待真实 ONNX 模型产出（pipeline/exporter M6B → num_leaves=31）后再接
  ort-go/推理服务并点亮 TDD-ONNX-02 性能基线。
- **状态更新（2026-08-19 深夜 /loop）**：M6B 蒸馏流水线已实现并点亮
  TDD-PIPE-01..06 + TDD-WATCH-01/02；`pipeline/exporter/exporter.py` 已能产出
  SQLite（uws_rules + site_synonyms + uws_negative_keywords + knowledge_versions）+
  `classifier.onnx`（真实模型训练前为确定性占位）+ manifest + checksums；
  困难负样本挖掘器（`pipeline/exporter/training.py`）已就位。
  **下一步**：lightgbm → ONNX 真实训练（num_leaves=31，模型 <1MB）接替占位，
  再接线 ort-go/推理服务（#1 的 A 案）与 KB 契约层真实对象存储 fetcher（#7）。
- **状态更新（2026-08-20，真实模型已产出）**：`pipeline/exporter/train_onnx.py`
  落地——种子规则 + negative_keywords + HardNegativeMiner 构造训练集 → LightGBM
  （num_leaves=31）→ **手工 ONNX 导出**（skl2onnx ≥1.17 已移除 LGBM 转换器，
  弃旧版转换器改自建 TreeEnsembleClassifier + parity 校验）→ exporter 以
  `onnx_model_bytes` 落真实 `classifier.onnx`（`tests/integration/
  test_pipeline_learning.py` 验证替换占位 + onnx.checker + ORT 推理）。
  TDD-ONNX-02/03 真实侧已点亮。**仍待**：Go 侧 ort-go/Python scorer 接线决策
  （#1 的 A/B 案）与真实 CC→LLM 语料喂入训练（当前为种子数据）。
  **状态更新（2026-08-20 续29）**：Go 侧 A 案已落地（见 #1 续21）；真实 CC→LLM
  语料喂入仍待——`uws parse-warc` 离线链路已点亮（TDD-CLI-03/COORD-08，
  `coordinator/warc_extractor.py`），下一步可把 parse-warc 产出的 Markdown 语料
  接入 `pipeline/llm_distiller` 做真实蒸馏训练，替换当前种子数据。
  **状态更新（2026-08-20 续30，检索 API 层已点亮）**：CC 搜索引擎 demo 的
  检索侧闭环——`uws search`（TDD-CLI-08/09，`coordinator/search.py`）对
  parse-warc/crawl 语料做零依赖 Okapi BM25 全文检索（CLI 查询 + `--serve`
  本地 HTTP `/v1/search`，详见 GLOBAL §5 裁决 #28）。**仍待**：真实 CC→LLM
  语料喂入蒸馏（本条目后半）——把 parse-warc 产出的 Markdown 接入
  `pipeline/llm_distiller` 替换种子数据，是下一高价值步骤（依赖 LLM 通道，
  可用 `cloud-ai-gateway` 或注入式 llm_call 先跑小样验证）。
  **状态更新（2026-08-20 续31，真实语料获取入口已落地）**：`uws cc-fetch`
  （`coordinator/cc_fetch.py`：collinfo.json / wet.paths.gz / 限量并发断点续传
  下载 WET，TDD-CLI-10/11 点亮，见 GLOBAL §5 裁决 #29）；WARCReader 流式化
  （记录级内存）适配服务器批量解析。**仍待**：parse-warc 产出的 Markdown →
  LLMDistiller → Exporter 接入（需 LLM 通道：cloud-ai-gateway / 注入式
  llm_call 先跑小样），接入后导出 `uws_l1.db + classifier.onnx` 即完成
  离线蒸馏第一个闭环。
  **实测（2026-08-20 续31 真实闭环）**：下载 1 个真实 WET（CC-MAIN-2026-30
  段 00000，62MB 压缩）→ parse-warc 14s 产出 20,975 篇 Markdown（369MB）→
  `uws search "privacy policy"` 命中真实隐私政策页。单文件级闭环已跑通；
  服务器规模化后待补：①检索索引持久化/增量构建（当前每次 build 21k 篇 ≈
  22s 纯 Python BM25，百万级需 SQLite FTS5 / DuckDB / 向量 RRF，见 GLOBAL
  §5 裁决 #28 遗留）；②蒸馏 LLM 通道接入；③部署打包（systemd + 批量脚本）。
  **状态更新（2026-08-20 续32，索引持久化已落地）**：`PersistentIndex`
  （SQLite 倒排索引 + 指纹增量同步，TDD-CLI-12）已点亮，`uws search
  --index <path>` 二次运行不再全量重建（实测 300 篇：全量 333ms → 增量重开
  9.6ms，查询 P99 11ms）；打分与内存 BM25 逐位一致。**仍待**：①>10M 篇级
  升级项（预分词 FTS5 / DuckDB / 向量 RRF——实测默认 FTS5 unicode61 分词器
  把连续 CJK 当一个 token 且 bm25 k1 不可配置，故未采用，见 GLOBAL §5 裁决
  #30）；②蒸馏 LLM 通道接入（parse-warc Markdown → LLMDistiller → Exporter，
  候选 cloud-ai-gateway / 注入式 llm_call）；③部署打包（systemd + 批量脚本
  + 磁盘/带宽预算：1 个 62MB WET → 369MB Markdown，100 文件 ≈ 37GB 语料 +
  6GB WET，500~1000 WET 起步需 500GB~1TB 盘）。
  **状态更新（2026-08-20 续33，CC 工具链已移交）**：`cc-fetch` / `parse-warc` /
  `search`（含 `PersistentIndex` 与全部测试）已整体迁至
  `labs/world-replay/search-engine/`（`se` 包），uws 保持义肢纯净（CLI 仅
  run/crawl/api）。①>10M 篇级索引升级项改归 search-engine 设计
  （P0.5 FTS5 持久化校验，见其 `docs/cc-batch-pipeline-and-search-service.md`）；
  ②蒸馏 LLM 通道接入仍属 webtouch 离线蒸馏环，输入改为消费 search-engine
  的 parse-warc 产物；③部署打包同归 search-engine。

### 6. TDD-MATCH-05 L3 SimHash 物理校验（集成层）

- **背景**：L1/L2 候选池已点亮（TDD-MATCH-01/02/04），但 L3 需对 Top-K 候选做
  SimHash 结构比对（汉明距 > D_h=5 剔除/降权）。v1.4 冻结契约没有
  node_features/候选 DOM 输入 RPC，SimCDPSession 也不承载 DOM，集成层无注入通道。
- **状态更新（2026-08-20 /loop）**：**逻辑层已点亮**——`pkg/matcher` 新增
  `SimHash64`（与 `pipeline/cc_fetcher/topology.py` `simhash64` 逐位对齐，
  已交叉验证同一文本输出一致）、`HammingDistance`、`SimHashOverlap` 与
  `RefineByStructure`（Top-K 短名单 + D_h=5 剔除，黄金夹具 Go 单测全绿，
  `GLOBAL.md` §5 裁决 #13）。**仍待**：集成层经「页面→候选」内部通道点亮——
  候选特征本应来自 WASM 探针→引擎的内部 DOM/AXTree 通道（非 ExecuteAction RPC
  输入）；可用 SimCDPSession 注入候选文本走引擎→matcher 管线点亮，或等真实 CDP
  会话落地后自然携带（不属契约缺口，无需 v1.5 RPC）。
- **状态更新（2026-08-20 续6）**：**集成层已点亮**——`SimCDPSession` 新增
  `SetPageCandidates`（`PageCandidate`：文本/显式置信度/SimHash/Flow/HTTPStatus/
  Children 候选图），引擎 runAction 消费页面候选通道：L1 候选经 `RefineByStructure`
  结构校验（「登录广告」vs「登录」全池剔除 → NOT_FOUND）、L2 同义词候选跳过文本代理
  校验（不误伤跨语言同义）；`TestMatch05L3StructureRejectIntegrationGRPC` 全绿。
- **候选方案**：
  - A：逻辑层先行——`pkg/matcher` 实现 64-bit SimHash + Top-K 结构比对，
    用黄金候选夹具直接写 Go 单测（不依赖 gRPC），点亮验收的实现层；
  - B：v1.5 契约追加候选特征输入 RPC（需解冻契约，走追加-only 规则）；
  - C：等真实 CDP 会话接入（`pkg/browser` 生产 CDPSession）后，经
    ExecuteAction + AXTree 特征自然携带结构信息再验证。
- **建议**：A 先行（逻辑层可测、零依赖），C 随真实 CDP 会话补集成验证。

### 4. 真实 CDP 会话适配器（生产 CDPSession 实现）

- 当前引擎服务默认使用 `pkg/engine` 的 `SimCDPSession`（模拟会话，动作仅记录）。
- **2026-08-19 更新**：M5 SPA 时序接线已实现层点亮——`SimCDPSession` 增加可脚本化
  导航/网络/DOM 事件（`SetNavEvents` + `NavigationState`），引擎 ExecuteAction 已接入
  verifier 状态机（动作后捕获 pushState → 立即 NAVIGATING；下一动作入口做 ≤timeout_ms
  稳定性等待 → NAVIGATED / DEADLINE_EXCEEDED）。TDD-ENG-06/07 实现层全绿。
- **状态更新（2026-08-20 续8，已实现）**：`pkg/browser` 落地最小纯 stdlib WebSocket
  （RFC6455）+ CDP 客户端 + `cdpSession`（`browser.CDPSession` + ActionSession +
  VerifierEvents + CandidateSource 全接口）：
  - 启动模式每上下文独立 headless Chrome（临时 user-data-dir，Cookie/缓存物理隔离）；
    附着模式（`attach_cdp_url`）新建标签页共享登录态、不破坏原会话（TDD-ENG-08 点亮）；
  - 物理动作：语义 DOM 定位 + `elementFromPoint` 遮挡检测（occluded）+
    Fitts 三次贝塞尔鼠标轨迹（ENG-03）+ `Input.insertText` 输入；403+Turnstile
    挑战页返回 `challenge_intercepted`（引擎映射 UNAUTHENTICATED，ENG-05 接线）；
  - SPA 时序：`Page.navigatedWithinDocument` → pushState、Network XHR/Fetch
    在途计数、MutationObserver 限频采样；页面候选 = DOM 交互节点采集（真实
    MATCH-05/06 输入通道）；
  - 接线：`engine.Options.SessionFactory` + `cmd/uws-engine --browser=sim|cdp
    --chrome-path --cdp-port --headed`；
  - 验证：Go 单测（WS/CDP 伪服务器 8 例）+ 真实 Chrome 集成（导航/探活/候选/点击/输入/
    SPA pushState/遮挡/附着 4 例，无 Chrome 自动 skip）+ Python E2E
    `tests/e2e/test_engine_attach.py`（`UWS_E2E_CDP=1` 门控）；`make pre-commit`
    全绿、`go test -race` 全绿。
  - **状态更新（2026-08-20 续9）**：**ENG-13 NEW_WINDOW_OPENED 已点亮**——
    `Target.setDiscoverTargets` + `Target.targetCreated` 事件源（`browser.WindowEvents`
    一次性读清）→ 引擎映射 `ContextState_NEW_WINDOW_OPENED`；物理输入前
    `Target.activateTarget` 前台化（修复后台标签 `Input.dispatchMouseEvent` 挂起超时）；
    验证：Go 引擎映射 `TestEng13NewWindowOpenedMapping` + 真实 Chrome 集成
    `TestCDPSessionNewWindowOpened` + Python E2E `test_tdd_eng_13_new_window_opened`
    （`UWS_E2E_CDP=1` 启动模式全链路）全绿。
  - **P0 加固已完成（2026-08-20 续12）**：视口外元素自动滚动定位；导航失败显式化
    （loadFailed + chrome-error frameNavigated 双信号）；在途 XHR/Fetch 30s TTL 剪枝；
    页面级 HTTPStatus 接入候选（≥400 整页物理湮灭）。集成用例
    `TestCDPSessionScrollIntoView` / `TestCDPSessionNavFailure`（Chrome 151 实测全绿）。
  - 遗留：ENG-12（sannysoft 外网）、ENG-03 CDP 级鼠标事件采样断言（可选增强）、
    ENG-05 真实 Turnstile 页 E2E、
    协调器真实 Attach 新 Target（需 v1.5 RPC/CLI 定案，TDD-COORD-09 真实侧）、
    `--browser=cdp` 启动模式 CLI E2E（可选，Go 集成已覆盖）。
    **extract 语义增强已落地（2026-08-20 续24）**：findElement 跨全部语义源评分 +
    零分不入选（见 GLOBAL §5 #25），data-testid 语义可精确定位、无语义关联不再误报。
  - P1/P2 待调研结论后分批：viewport/proxy 应用（ENG-01 真实侧补全）、重启保登录态、
    Chrome 崩溃自愈、shadow DOM、iframe、Cookie 失败可观测、SessionUrl 真实化、wsconn 加固。
    **调研任务书：`docs/cdp-research.md`（与内部工程解耦，可整体交外部 AI 执行）。**

### 5. TDD-API-05 mTLS 集成用例

- 服务端已实现 `--tls-cert/--tls-key/--tls-ca`（`pkg/engine/tls.go`，
  RequireAndVerifyClientCert 模式）。
- **状态更新（2026-08-20）**：**已完成**——`tests/integration/test_grpc_contract.py`
  在测试内本地生成 PKI（`_generate_mtls_pki`：根 CA + 服务端 SAN=127.0.0.1 证书 +
  合法客户端 + 伪造 CA 客户端），`mtls_engine` 夹具以 mTLS 启动真实引擎；
  三例全绿：明文连接/无客户端证书/伪造 CA 客户端在传输层被拒（UNAVAILABLE/
  DEADLINE_EXCEEDED），合法客户端握手通过且无 API Key 仍返回 UNAUTHENTICATED。
  `make pre-commit` 全绿（54 passed/13 skipped + 契约 9 passed）。

### 7. 知识库热更契约层剩余用例（TDD-KB-02/04/05/07 契约层 + KB-06）

- **2026-08-19 更新**：实现层（`pkg/engine/m5_integration_test.go`，bufconn + 注入
  多版本 fetcher）已点亮 TDD-KB-01..05/07；契约层点亮 KB-01（dry-run 不切换 +
  未知版本 fetch_failed）与 KB-03（内置 ONNX 未就绪 → sanity_check_failed 保旧版）。
- 契约层剩余用例待真实对象存储 fetcher（M6B exporter 产出 SQLite+ONNX artifact +
  S3/OSS 下载校验实现）接入后补齐。
- KB-06（P0 自动回滚：连续 50 次 NOT_FOUND → <1 分钟回滚）为 M7 E2E；注意 v1.4
  冻结契约 `HealthStatus` 无回滚标记字段，需 v1.5 追加-only 或 M7 另行定案。
- **状态更新（2026-08-19 深夜 /loop）**：M6B exporter 已产出可下载校验的
  SQLite+ONNX artifact 布局（manifest/checksums 齐备）；下一步实现对象存储
  fetcher（S3/OSS 下载 + SHA256 校验）即可补齐 TDD-KB-02/04/05/07 契约层。
- **状态更新（2026-08-19 深夜续 /loop）**：**TDD-KB-02/04/05/07 契约层已点亮**——
  新增 `pkg/knowledge/httpfetcher.go`（HTTP(S) 网关/CDN/预签名 URL 直连：
  manifest + SHA256 + SQLite 加载 + 规则状态归一 + 内存估算，零外部依赖）；
  `cmd/uws-engine` 新增 `--knowledge-base-url` / `--knowledge-version` 接线；
  exporter manifest 自动写入 `memory_estimate_mb`（KB-04 门禁输入）。
  `tests/integration/test_knowledge.py` 新增 `http_engine` 夹具（exporter 四版本产物 +
  本地 HTTP 服务）点亮 KB-02/04/05/07；`make pre-commit` 全绿。
  降级实现，真实部署若需直连对象存储再补签名实现。
- **状态更新（2026-08-20）**：**KB-06 已点亮**——proto v1.5.0 追加
  `HealthStatus.rolled_back`/`rollback_reason`；`pkg/knowledge.Store.AutoRollback`
  复用双缓冲 previous 句柄（防乒乓）；引擎连续 NOT_FOUND 计数（阈值 50、成功动作
  重置、触发即清零）自动回滚；实现层 + 契约层（真实 HTTP fetcher 产物）双层全绿。

### 8. Web-to-API 会话能力后续演进（2026-08-25 登记，本轮已落地核心）

- **背景**：多轮 / 新会话 / session 映射已解决（见 GLOBAL.md §5 裁决 #34：
  extract 取最后回答块 + 基线轮询 + `text=` 选择器 + `/v1/sessions`），
  但标准 OpenAI API 的「流式」与「真并发会话」还未覆盖。
- **待推进候选**：
  - A：SSE 流式（`stream: true`）：引擎层已有按需 extract 能力，可在轮询期间把
    「最后回答块增量文本」以 `data: {...}` 逐块推给客户端（OpenAI `choices[].delta`）。
    风险点：文本稳定判定与流式推送到帧的时序对齐，需要单独 E2E 验证；
  - B：真多标签并发会话：引擎 `contexts` 目前按 client key 单上下文，多个
    session_id 会串行在同一页面。扩展 = 引擎 `contexts` 支持 key→map[ctxID]，
    `ExecuteAction` 经 `context_hint["target_id"]` 路由（proto 追加-only 不破坏契约），
    API 每个 session_id 持有独立 context（受 Target 池上限约束）；
  - C：会话历史恢复：切换回旧 session_id 时按会话标题/URL 在侧边栏定位并点击恢复
    （DeepSeek 会话列表是纯 div，可复用 `text=` 定位能力）；
  - D：引擎 re-init 标签页清理：`InitializeContext` 释放旧 Target 后应关闭旧标签页，
    避免每次 API 重启在调试 Chrome 里积累孤立 deepseek 标签。
- **建议**：A（流式）价值最高且最贴近「标准 API」；D 顺手可做；B/C 等真实多用户
  场景出现再投入。
- **状态更新（2026-08-25 深夜，A 已闭环）**：SSE 流式（`stream: true`）经方案 1
  （CDP Network 层 SSE 捕获）落地——Go `pkg/browser` 新增 `ChatStreamSource`
  （`Network.streamResourceContent` + `Network.dataReceived.data` 被动捕获，
  解析干净 content/reasoning delta）；引擎 `extract` 经
  `context_hint["prefer_sse"]="1"` 优先返回 SSE 快照；API `/v1/chat` 支持
  OpenAI 兼容 `stream:true` 增量推送（`choices[].delta` + `finish_reason` +
  `data: [DONE]`）。验证见 GLOBAL.md §5 裁决 #35。剩余 B/C/D 维持候选：
  B=真多标签并发会话（引擎 `contexts` 按 client key 单上下文，需 context_hint
  路由扩展）；C=会话历史恢复；D=引擎 re-init 旧标签页清理。
- **状态更新（2026-08-25 深夜续，真实回归闭环）**：用户实测「what is javascript?
  answer in 200 words in markdown」超时——根因 = DeepSeek Web SSE 升级为补丁式
  协议（初始快照 + 显式 APPEND + **隐式 `{"v":"..."}` 增量** + FINISHED），旧
  解析器不识别导致快照恒空；修复见 GLOBAL.md §5 裁决 #36。另发现并修复第二个
  真实 bug：生成中途停顿 >1.5s 时文本稳定性判定把半截当完成——`ActionResultResponse`
  追加 `chat_stream_done`（v1.5.2 append-only，bindings 原子再生成），API 在
  SSE 接入时以 done 为准。真实 E2E（完整 markdown / 多轮 / stream:true）全绿。
- **状态更新（2026-08-25 晚，B+D 已闭环）**：真多标签并发会话落地（v1.5.3
  append-only：`InitContextRequest.context_name` 命名上下文 + `CloseContext`
  RPC + `context_hint["context"]` 路由）；`DELETE /v1/sessions/{id}` 关 Tab、
  `POST /v1/sessions` 开新 Tab、`new_session:true` 重建同名 Tab（旧 Tab 关闭，
  D=re-init 标签清理一并解决）；标签被手动关闭后 `target_destroyed` 自动重建
  重试；API 每会话一把锁（不同 session 可并行）。C（会话历史恢复）仍待真实
  多用户场景再投入；Gemini 逐 token 流式（batchexecute PCck7e 轮询）待评估。
  剩余 B/C/D 维持候选。
- **状态更新（2026-08-25 深夜续2，Gemini 支持落地）**：Gemini 站点接入完成
  （NDJSON 捕获 + getResponseBody 兜底 + 3 模型单选 + Extended thinking 复选框，
  详见 GLOBAL.md §5 裁决 #37）；共享/站点差异按 Provider 插件化拆分
  （Go `deepseek_sse.go`/`gemini_ndjson.go`/`gemini_model.go`，Python
  `coordinator/providers/`）。待网络稳定后补：Extended thinking 开关 + 
  `stream:true` 的登录态真实 E2E（Google Cookie 轮换/安全检测干扰）。

### 16. 冷启动调试 Chrome 的「后台启动」（2026-09-11 登记，未验证）

- **背景**：附着模式每会话的专属小窗已不抢焦点（GLOBAL #91）；但 `--attach debug`
  在调试 Chrome **未运行**时会直接 exec 拉起 Chrome——macOS 会把 Chrome 带到前台，
  仍可能打断用户输入（一次性）。冷启动窗口已改成小窗（`--window-size` 默认 460x880）。
- **候选方案**：
  - A：macOS 用 `open -g -n -a "<app bundle>" --args ...`（后台启动，不到前台）。
    实测受阻：Chrome 同 `.app` 多实例策略在已有实例时拒绝 `-n`；且本机沙箱下
    LaunchServices 不可用（`kLSServerCommunicationErr`），**本机无法验证**，收益仅限
    「完全没有任何 Chrome 在跑」的场景。
  - B：先 `open -g` 再探测端点，失败回落直接 exec（多 4~6s 冷启动延迟）。
  - C：维持现状（直接 exec + 小窗），冷启动一次性打扰接受。
- **建议**：C（现状）。真机上若确被冷启动打扰，再按 B 做一次真机验证（需用户在真机执行）。
- **状态**：未落地（代码曾试 A/B 后回退，避免未验证路径进入运行链路）。

## 已择方案（供追溯）

- 2026-08-19：classifier 点亮采用「决策核 + mock scorer」（方案 A），
  弃用 ort-go 直绑（本机构建风险）与 Python onnxruntime mock（本机装不上且引擎未接线）。
- 2026-08-19：引擎 gRPC 服务接线采用「新增 pkg/engine 服务层」方案（DAG M6A 前置，
  解锁 TDD-API 契约层 + 集成层），模拟 CDP 会话先行、真实适配器后置（见待办 #4）。
- 2026-08-19（晚 /loop）：集成层点亮采用「matcher 落数据模型（site_synonyms +
  uws_negative_keywords）+ MatchQuery 候选池管线 + 引擎接线」方案（待办 #3 的 A 案），
  弃用硬编码种子（B 案，未落地规格表结构）与仅点亮 ONNX-01/03（C 案，P0 MATCH-02 留白）；
  TDD-MATCH-05 记入待办 #6（逻辑层先行，见 USER_ACTION.md 候选方案 A）。
- 2026-08-19（深夜 /loop）：M6B 蒸馏流水线采用「纯 stdlib 零依赖 + 分模块落地」方案
  （SimHash/WARC 自实现，不引 warcio/lightgbm/onnx 本机装不上的包）；
  ONNX 占位导出先行、真实模型训练后置（对应待办 #1/#3），保证 M6B 全链路可测可交付。
- 2026-08-19（深夜续2 /loop）：M6C 协调器核心采用「planner（L1/L2/L3 + 校验）+
  executor（重试/重规划/崩溃恢复/反思）+ reflector + 可插拔 EngineClient」方案；
  L2 Embedding 先用零依赖 `TokenOverlapEmbedder` 降级、生产换 MiniLM/BGE；
  NEW_WINDOW_OPENED Attach 在 v1.4 无 RPC，真实客户端抛 UNIMPLEMENTED 待 v1.5/CDP 定案。
- 2026-08-20（/loop）：M6A 单元层采用「Go 纯 stdlib 手写 YAML 子集 config 子命令 +
  coordinator/playwright_adapter.py 包装 EngineClient 协议」方案（TDD-CLI-05 +
  ADAPTER-01/02 全点亮）；弃用 B 案（配置仅内存 mock、适配层并入 executor 无独立
  可测边界）与 C 案（本轮改 MATCH-05 SimHash，M6A 分支继续空转）。
- 2026-08-20（/loop 续）：同时点亮两个剩余 P1 零依赖用例——TDD-MATCH-05 逻辑层
  采用「`pkg/matcher` 64-bit SimHash + `RefineByStructure`（NodeText 文本代理结构签名，
  与 pipeline `simhash64` 逐位对齐，等真实 DOM 特征通道落地后仅替换输入）」方案；
  TDD-PLAN-05 采用「executor 条件表达式扩展（`in`/布尔/列表字面量）+ loop 专属
  集成测试」方案（4 例全绿）；两者互不依赖、不同技术栈并行推进。
- 2026-08-20（/loop 续2）：TDD-MATCH-06 逻辑层采用「A* 深度钳制 + 物理黑名单 +
  祖父回溯连坐」方案（`pkg/matcher/astar.go`——`Decide` 决策带 + `AStarSearch` 统一入口，
  8 例 Go 单测全绿）；顺带点亮安全层 TDD-SEC-02/06 + MATCH-03（`tests/security/` 复用
  session 级引擎夹具，`validateIntent` 先于生命周期检查，无需 InitializeContext）。
- 2026-08-20（用户授权：能确定做且自信的改动全做；未上线无兼容包袱）：v1.5 契约
  采用「追加-only 最小集」方案——`HealthStatus` 追加 `rolled_back`/`rollback_reason`
  （不加多余字段、不改既有语义）；KB-06 采用「Store.AutoRollback 复用双缓冲
  previous + 引擎连续 NOT_FOUND 计数」方案；SEC-01/03 采用「online_logs SQLite 落库
  （--log-path）+ 敏感语义整值脱敏 + node_features/decision_trace 仅元信息」方案；
  snapshot zstd 压缩维持明文 JSON（#13 候选 B），不引入新依赖。
- 2026-08-20：ONNX 采用「本机 pin onnxruntime==1.23.0 + lightgbm 手工 ONNX 导出
  （dump_model → TreeEnsembleClassifier，ai.onnx.ml opset 3）+ parity 校验」方案
  （弃 skl2onnx：≥1.17 移除 LGBM 转换器；不引旧版转换器保确定性）；VLM 配置采用
  「CLI vlm 段（provider/endpoint/api_key/timeout_sec/max_solves）+ 协调器
  config 加载 + HttpVlmSolver（纯 stdlib）+ build_vlm_solver 工厂」方案，
  Mock 仍为默认降级，生产打码服务替换即闭环。

## 待推进（2026-08-19 深夜续2 登记）

### 8. TDD-PLAN-05 条件分支与循环（P1/M6C）

- 简易条件解析器（`{{ var }} == 'x'` / `!=`）与 loop（`max_iterations`/`exit_condition`）
  已在 executor 内置但无专属测试；下一轮补 `condition` 表达式扩展（含 `in`/布尔）与
  loop 超限安全退出用例。
- **状态更新（2026-08-20 /loop）**：**已完成**——条件表达式扩展 `in`/`not in`、
  布尔/数字/列表字面量（类型感知比较），缺失变量与非法表达式安全默认；
  loop `max_iterations` 封顶、`exit_condition` 提前退出、`{{ var }}` 插值、
  循环中途失败不吞错；`tests/integration/test_coordinator.py` 新增 4 例全绿
  （TDD-PLAN-05 验收单勾选）。生产 Embedder（#10）与 VLM（#9）继续后置。

### 9. TDD-COORD-04 VLM 视觉打码联动（P0/M6C E2E）✅ 已完成（2026-08-20 续16）

- **状态更新（2026-08-20 /loop）**：**集成层已点亮**——`coordinator/vlm.py`
  （`VlmSolution`/`VlmSolver` 协议/`MockVlmSolver` 确定性求解器）+
  `EngineClient.capture_screenshot` 契约 + executor `_solve_challenge` 编排：
  `CHALLENGE_INTERCEPTED` → 截图 → VLM 坐标 → ExecuteAction drag
  （`challenge_slider`）→ 解锁后重试原步骤无缝继续；单任务求解 3 次封顶
  （`MAX_VLM_SOLVES`，防无限重试）；无求解器时保持既有 PAUSED 降级语义。
  验证：`tests/unit/test_vlm.py` 3 例 + `tests/integration/test_coordinator.py`
  4 例全绿（解锁继续/求解耗尽/无求解器/拖拽失败）；`make test-py`
  68 passed / 13 skipped。
- **状态更新（2026-08-20 续16，已落地）**：v1.5.1 追加-only 两个引擎原语全部完成：
  - `CaptureScreenshot` RPC（proto v1.5.1 + Go 引擎 `CaptureScreenshot` + Python
    `GrpcEngineClient.capture_screenshot` 接线；PNG + 视口尺寸；生命周期/串上下文/
    monitor 越权守卫全测）；
  - ExecuteAction `expected_action="drag"` 原生拖拽（`pkg/browser.ParseDragCoords` +
    `dragAt`：mousePressed → 贝塞尔 mouseMoved(buttons=1) → mouseReleased，坐标直发
    bypass 语义匹配与挑战门禁；模拟会话同语义）；
  - **顺带修复真实 bug**：`Navigate` 收尾无条件清 `challenge`/`lastStatus`，会把初始
    导航判定的拦截冲掉 → 改为挑战状态随 Document 响应重算（403+挑战头=拦截，
    200 自动清除）；
  - 验证：Go 引擎层（`vlm_test.go` 生命周期/串上下文/越权/挑战流）+ 真实 Chrome
    （`TestCDPSessionDrag` 滑块 50→100、`TestCDPSessionCaptureScreenshot` PNG 魔数）
    + Python E2E `tests/e2e/test_coordinator_vlm.py`（403+CF 挑战头 → UNAUTHENTICATED
    → 真实 PNG → Mock VLM → drag → /solve → reload 200 → 原步骤重试成功）
    `UWS_E2E_CDP=1` 全绿；`make pre-commit` 全绿（Go 全包 + Python 69 passed/13 skipped
    + 契约 11 passed）。
- 仍后置：生产 VLM 求解器（DeepSeek-VL / 打码 API）替换 `MockVlmSolver`（无阻塞）。

### 10. 生产 L2 Embedding（MiniLM/BGE）

- `TokenOverlapEmbedder` 仅为零依赖降级；部署环境接入 MiniLM/BGE 向量检索并复跑
  TDD-PLAN-02 阈值语义。

### 11. M6A CLI/适配层剩余（2026-08-20 登记）

- 单元层已点亮：TDD-CLI-05（config 持久化）、TDD-ADAPTER-01/02（Playwright 映射 +
  边界隔离）。
- **状态更新（2026-08-20 续24）**：协调器爬取真实侧点亮（TDD-COORD-05）——
  `coordinator/crawler.py` CrawlDriver + Mock 博客站真实 CDP E2E 全绿（signals
  提取、广告过滤、下一页深入、Markdown+Frontmatter 输出）；CLI 子命令编排仍待
  M6A CLI 定案（Go CLI 与 Python 协调器的接线方式）。
- **状态更新（2026-08-20 续27）**：TDD-CLI-04（`uws api` Web-to-API）已点亮——
  `coordinator/api_server.py`（纯 stdlib ThreadingHTTPServer，OpenAI 兼容
  `/v1/chat` + `/v1/health`）+ cli.py `api` 子命令；chat 编排 input→click→extract，
  DEADLINE 1s/2s 重试；E2E（真实 curl + FakeChatEngine）与单元（注入时钟）全绿。
  `uws` Go 二进制对 run/crawl/parse-warc/api 引导至 `python3 -m coordinator.cli`。
- 剩余 TDD-CLI-03 与 TDD-COORD-08（`uws parse-warc` 离线）为同一能力，
  依赖 WARCReader + signals 提取接线（无浏览器/无网络）；适配层已具备可复用
  `PlaywrightAdapter`（页 API → ExecuteAction），接入 `uws` CLI 时直接消费。

### 12. TDD-MATCH-06 集成层（引擎 runAction 中间区间接线，P1/M4）

- 逻辑层已点亮（2026-08-20）：`pkg/matcher/astar.go`（决策带/深度钳制/物理湮灭入黑名单/
  逻辑偏移公式/祖父连坐，8 例 Go 单测全绿）。
- **状态更新（2026-08-20 续6）**：**集成层已点亮**——引擎 runAction 中间置信度
  区间（θ_low ≤ C < θ_high）消费 `AStarSearch`：页面候选图（`PageCandidate.Children`）
  接根节点子图、路径末步节点文本执行、HTTP 4XX/5XX 物理湮灭入服务级黑名单
  （`Server.astarBlacklist` 跨请求驻留）；`TestMatch06AStarSearchIntegrationGRPC`
  （login_form→login_form_submit 执行末步 + 403 根湮灭 NOT_FOUND）全绿。
- **状态更新（2026-08-20 续23，真实侧点亮）**：WASM 探针 kept 节点按 BFS 父子链路
  重建 `PageCandidate` 树（JS 采集器回传 `node_id` + 父 BFS 索引 `pIdx`，Go 侧
  `buildProbeCandidates` 建树：父驱逐→上提为根、空文本父容器剪除后子节点上提）；
  引擎 `pageCandidatePool` 预序遍历全树直接匹配（嵌套候选不依赖 A* 才可达），
  `astarSelect` 消费 `Children` 子图。验证：Go 单测（乱序/孤儿/空父上提）+
  真实 Chrome `TestCDPSessionWasmProbeChildren`（form > 登录/注册）+ 引擎
  `TestCandidateTreeNestedDirectMatchGRPC` 全绿；「纯规则 + A*」兜底模式
  （TDD-WATCH-01）联动已随集成层点亮。

### 13. online_logs 落库与快照压缩（2026-08-20 登记）

- **已做**：`pkg/engine/logstore.go` online_logs SQLite 落库（`cmd/uws-engine
  --log-path`）+ PII 脱敏（SEC-01/03 点亮）；`node_features`/`decision_trace`
  仅长度/类型元信息。
- **待确认**：`snapshot_blob` 规格写 zstd 压缩，当前以明文 JSON 落库（体积小、
  便于审计）。候选：
  - A：引入 `klauspost/compress`（纯 Go）按规格实现 zstd 快照；
  - B：维持明文 JSON（快照仅存元信息，不落原始值，泄露面极低）；
  - C：改用 gzip（stdlib 零依赖，但偏离规格压缩格式）。
  - 建议：B 维持现状（SEC-03 验收以「无明文」为准已通过；zstd 属格式优化，
    待真实 CDP 快照链路落地后再按 A 补）。

### 14. WASM 探针 M2（2026-08-20 续13 登记，emcc 3.1.74 就绪）

- **状态更新（2026-08-20 续13，已实现核心）**：`pkg/probe_wasm` 从骨架落地为完整实现：
  - `src/probe.h/cc`：`ProbeEngine`——16MB 线性内存硬边界（native 默认 16MB 容量，
    WASM 侧 15MB Arrow 区 + 1MB 预留程序/栈/scratch/溢出队列，对应规格「预留 5% 余量」）、
    分级准入 + 驱逐（视口内 > 交互 > a > div[role] > 其他，溢出队列保留 BFS 序）、
    usage>95% 置 needs_fallback、越界受控报错不崩溃；固定宽度 `SerializedNode`（≈1280B）
    二进制序列化（Arrow 语义降级，消费端就绪后再定列式布局）；
  - `src/ast_hash.h/cc`：`AstSemanticHash`——剥离注释/字符串/数字/十六进制字面量，
    保留操作符（≥2 字符）+ API 调用点 + 控制关键字；64-bit SimHash LSH（FNV-1a 投票，
    零堆分配 + token 投票缓存；声明名不参与签名）；与 Go/Python `simhash64` 同构但
    哈希函数不同（内部一致性要求，不承诺跨语言逐位对齐）；
  - `src/wasm_main.cc`：standalone WASM 导出（`--no-entry` + `-sINITIAL_MEMORY=16777216`
    + `-sALLOW_MEMORY_GROWTH=0`），惰性 placement-new 初始化（standalone 实例化不自动
    跑全局构造器，宿主无需调用 `_initialize`）；字段协议（scratch + `node_field`）与
    批量协议（`\x1e` 记录 / `\x1f` 字段分隔 + `nodes_bulk`，分块注入兼容 64KB scratch）；
  - `test/probe_wasm_test.cc`：TDD-WASM-01..06/08 + bulk/分块全部 native 点亮；
  - `benchmarks/bench_probe_wasm.py`：S1（100 交互节点 P99=0.63ms）+ S2（5000 DOM
    节点页面 P99=11.61ms），红线 15ms；
  - `Makefile probe-wasm / probe-bench` 目标（EMCC/EMCMAKE/EM_CACHE 可配置，
    沙箱内需 EM_CACHE 指向可写目录）。
- **状态更新（2026-08-20 续15，真实链路已点亮）**：
  - WASM 注入 V8：`cdpSession` 经 CDP `Runtime.evaluate`（base64 → `WebAssembly.instantiate`）
    注入 standalone 探针，`window.__uwsProbe` 缓存；页内 DOM→bulk 采集器（BFS + 19 字段
    协议分块 ≤32KB 写 scratch，非交互节点不取 textContent、父语义文本按 BFS 缓存——修复
    parentElement.textContent 每节点重复摊平的 O(n²) 卡死根因，2 万节点页采集 316ms）；
  - Go 侧解码 kept 节点（semantic_text + ast_semantic_hash）生成 PageCandidate；
    截断时 200ms 内 `overflow_peek` BFS 前 200 节点 JS 慢速遍历补偿合并，超时仅用截断结果；
  - 引擎侧 `Server.WasmTruncationTotal` 原子计数器（`uws_wasm_truncation_total` 数据源）
    + decision_trace `wasm_truncated` 标记（online_logs 仅元信息，SEC-03 兼容）；
  - `cmd/uws-engine --probe-wasm <path>` flag（空 = 自动探测 build/ 产物，缺失降级 JS 文本采集）；
  - 验证：真实 Chrome `TestCDPSessionWasmProbe`（候选含 登录/注册/用户名）+ `TestCDPSessionWasmTruncation`
    （2 万节点截断 + 补偿合并）全绿；`make pre-commit` 全绿（68 passed/13 skipped + 契约 10）；
    `go test -race ./pkg/browser ./pkg/engine` 全绿。
- **状态更新（2026-08-20 续26，TDD-OPT-01 已点亮）**：
  - 探针常驻预实例化：`initTarget` 经 `Page.addScriptToEvaluateOnNewDocument` 注册
    文档级脚本，每个新文档解析前异步实例化探针（`window.__uwsProbe` 常驻，
    采集路径零实例化开销）；注册失败静默降级（采集保留按需实例化兜底）；
  - 多信号批量抽取：`cdpSession.PageCandidates` 同文档候选缓存（导航/已采样 DOM
    突变/2s TTL 三条件失效），爬虫每页 N 组 signals 从 N 次全量 BFS 降为
    「1 次采集 + N-1 次缓存命中」；缓存命中纯 Go 路径
    `BenchmarkCandidateCacheHit` 实测 6.4µs/次 ≪ 5ms；
  - 真实 bug 修复：`sampleMutationsOnce` 用 `evalResult`（string 解码）读数字
    计数器，类型错误被静默丢弃 → 真实 CDP 的 DOM 突变信号从未生效；改
    `evalValue` 解码 int 后，`TestCDPSessionProbeResidentAndCached` 验证
    突变→失效→重采全链路；`go test -race ./pkg/browser ./pkg/engine` 全绿。
- **剩余**：
  - `uws_wasm_truncation_total` Prometheus HTTP 导出：**已定案（2026-08-20 用户）——
    不引入 prometheus client 依赖（高性能 CLI 审美），维持 B 案（原子计数器 +
    gRPC 观测）；部署期 SRE 面板需导出时另选通道（独立 exporter / gRPC 观测接口）**；
  - ast_source（内联 JS 处理程序源）→ AST SimHash 通道：当前 DOM 文本候选由引擎侧
    `SimHash64(text)` 代理 L3（GLOBAL #13），真实 handler 源哈希待页内采集器补
    `el.getAttribute('onclick')` 等字段；
  - 真实页内 AXTree（角色/视口）与 `--browser=cdp` 全链路 E2E（`tests/e2e/`）按既有
    `UWS_E2E_CDP=1` 门控补充。

## 审查发现（2026-08-20 重点模块审查，待决策是否修复）

> 来源：用户指令「若不能推进则对已完成部分重点审查重要模块」。审查范围：
> pkg/engine、pkg/knowledge、pkg/matcher、pkg/classifier、pkg/verifier、
> coordinator/（executor/planner/reflector/grpc_client）、pipeline/exporter、
> online_logs。以下按影响排序。
>
> **修复状态（2026-08-20 续）**：用户「按你建议来，兼顾效率」→ 执行 A+B：
> R2/R3/R4/R5/R6 已修复并带回归测试；R7 部分修复（rune 计数、Store 默认版本
> v1.5.0、logstore 错误可观测）；R1（架构接线）与 R7 剩余小项（max_engine_version
> 上限校验、matcher reload 并发、LLM 计划字段透传）维持待办，不擅自动。

### R1（高/架构）：知识库热更与 ExecuteAction 匹配管线未贯通

- 证据：`ReloadKnowledge`/`AutoRollback` 只切换 `knowledge.Store` 句柄
  （pkg/engine/service.go）；`runAction` 的 L1/L2 匹配走 `s.matcher`
  自带 `:memory:` 种子库（`NewDefaultMatcher`），从不读取 `s.kb`；
  `Store.ExecuteAction(ruleID)` 方法存在但引擎从未调用。KB 集成用例只断言
  版本/哈希/回滚标记，未断言 ExecuteAction 行为随热更变化。
- 影响：M5/M7 核心承诺（热更改变匹配行为、P0 自动回滚恢复「大面积失明」）
  目前只在知识库元数据层成立；真实产物（uws_l1.db）与匹配内存库是两套数据。
- **已修复（2026-08-20 续6，方案 A 执行）**：
  - `knowledge.Artifact` 携带完整规则/同义词/黑名单快照（`RulesFull`/`Synonyms`/
    `NegativeKeywords`）；`httpfetcher` 从 uws_l1.db 全量加载（含 `Store.ActiveArtifact`
    深拷贝供重建）；`AutoRollback` 回滚目标同样携带快照；
  - matcher 新增 `ReplaceAll`（单事务原子替换 + RWMutex 并发安全，读者全旧/全新）；
    引擎 `NewServer` 由 KB artifact 构建 matcher（KB 单一事实源，删除双源种子），
    热更/人工回滚/P0 自动回滚成功后 `syncMatcherWithKnowledge` 同步；
  - 验证：实现层 `TestR1HotReloadChangesMatchingGRPC` + `TestR1AutoRollbackRestoresMatchingGRPC`；
    契约层 `test_r1_hot_reload_changes_matching`（真实 HTTP artifact）全绿；
    `make pre-commit` 60 passed + 契约 10 passed + `go test -race` 全绿。
- 遗留：matcher 重建非双缓冲（重建窗口内 ExecuteAction 由 RWMutex 串行等待；
  写锁 <1ms 承诺目前仅覆盖 KB 句柄切换，matcher 重建在切换后独立完成），
  真实部署如需严格零阻塞可改为「构建新 matcher → 原子换指针」。

### R2（高/安全）：PII 脱敏依赖语义标签关键词，非敏感命名输入会明文落库

- 证据：`entryFromAction` 仅当 `isSensitiveSemantic(semantic)`（pwd/pass/ssn/
  phone/id_card 命中）才把 value 替换 `[REDACTED]`；`redactJSON`/`redactText`
  全树扫描在线上路径从未被调用（仅测试引用）。向「username」等字段输入手机号/
  证件号，snapshot_blob 会以明文 JSON 落库，与 SEC-03「密码/手机号/证件号明文
  一律不得存在」不符。
- 建议：快照中永不落 value 原文（最保守），或对 value 做内容级敏感模式扫描
  （手机/证件号正则）+ 全树 redactJSON 接线。
- **已修复（2026-08-20）**：`entryFromAction` 快照结构性禁止 value 明文——
  无论语义标签是否敏感，非空 value 一律以 `[REDACTED]` 落库，`pii_masked`
  随 value 存在置位；`pkg/engine/logstore_test.go` 新增手机号填入 username
  用例 + 契约层 `test_pii_redaction.py::test_r2_value_never_persisted_plaintext`。

### R3（中高/资源）：InitializeContext 重复调用泄漏 Target 槽位，且无释放 RPC

- 证据：`s.contexts[key] = bc` 覆盖旧上下文时未 `pool.Release`（service.go），
  proto 无 CloseContext/Destroy 类 RPC；`Pool.available` 只减不增（除初始
  导航失败路径外）。同 key 反复初始化可耗尽池容量；真实 CDP 下等于泄漏浏览器进程。
- 建议：覆盖时先释放旧 bc；补一个显式释放 RPC（v1.5 追加新方法向后兼容）
  或由引擎侧 TTL/空闲回收兜底。
- **已修复（2026-08-20）**：`InitializeContext` 同 key 覆盖前先
  `pool.Release` 旧 Target；Go 回归 `TestReinitReleasesOldTarget`
  （重复 init 后 HealthCheck ActiveTargets=1）。显式释放 RPC 仍待 M7 定案。

### R4（中/幂等）：协调器重试不复用 intent_id，引擎幂等缓存失效

- 证据：`executor._execute_step` 每次调用生成新 `intent-{uuid}`；DEADLINE_EXCEEDED/
  ABORTED 重试不会命中引擎 60s 幂等缓存（缓存仅成功结果）。若首次动作实际已执行
  仅验证超时，重试会重复物理副作用。
- 建议：单步重试复用同一 intent_id（跨 planner 重放也稳定）；「超时但已执行」
  的完全去重需要引擎侧 in-flight 标记（更大改动，另议）。
- **已修复（2026-08-20）**：引擎侧新增 in-flight 去重（同 intent_id 并发提交
  返回 DEADLINE_EXCEEDED、完成后命中缓存，物理执行仅 1 次）；协调器侧
  intent_id = `intent-{task}-{step}-{epoch}-{iteration}`（同一步骤重试复用、
  崩溃恢复 epoch 递增、loop 迭代独立）。回归：Go `TestInflightDedupSameIntent` +
  `test_coordinator.py::test_r4_*`。残余：DEADLINE_EXCEEDED 且动作已执行的
  「歧义超时」场景仍需真实 CDP 状态核验才能完全去重（M7）。

### R5（中/正确性）：pageVerifier 并发访问无锁（数据竞争）

- 证据：`runAction`/`waitForStable` 直接读写 `page.v`/`navCompleted`，未持
  `pageMu`；gRPC unary 默认并发，同 key 并发 ExecuteAction 会竞争。当前测试
  顺序执行未触发（未跑 -race 并发用例）。
- 建议：per-Target 串行化执行（如 key 级锁）或给 pageVerifier 加锁。
- **已修复（2026-08-20）**：`pageVerifier.mu` 在 runAction 全程持有（同 Target
  动作串行化）；`TestConcurrentExecuteActionSameTarget`（20 并发）+ 全包
  `go test -race` 通过。

### R6（中/安全）：GrpcEngineClient 不支持 mTLS

- 证据：`coordinator/grpc_client.py` 固定 `grpc.insecure_channel`；服务端 mTLS
  已上线（TDD-API-05），生产协调器无法走 mTLS 传输。
- 建议：GrpcEngineClient 增加 tls/mTLS 选项（root CA + 客户端证书）。
- **已修复（2026-08-20）**：`GrpcEngineClient` 支持 `tls_ca_path` +
  `tls_client_cert/key_path`（secure channel），错误映射补 UNAVAILABLE；
  契约层 `test_r6_grpc_client_mtls_support` 对接 `mtls_engine` 夹具验证
  合法 mTLS 通、明文被拒。

### R7（低）：若干小项

- `validateIntent` 用字节数限制 target_semantic（≤120 字节而非字符），中文
  >40 字会被误拒（sanitize.go）。
- `max_engine_version` 解析但从不校验（仅查 MinEngineVersion）；导出默认
  max="v1.4.x" 与引擎 v1.5.0 不符时仍可加载。
- `knowledge.NewStore` 默认 engineVersion 仍 v1.4.0（EngineVersion 已 v1.5.0）。
- `ActionNode.max_retries` 从未被 Reflector 消费（用常量 2/3/1）；LLM 计划
  的 loop/condition/non_idempotent 字段被丢弃。
- `Matcher.Insert*` 与 MatchQuery 并发不安全（reload 重建指针），当前引擎
  运行时无写入故未触发；热更接线后需加锁。
- `logstore.Record` 错误被忽略（`_ =`），SQLite 并发写可能静默丢日志。

> 状态（2026-08-20 续）：rune 计数已修复（`utf8.RuneCountInString` + 安全层
> `test_r7_semantic_length_counts_runes`）；Store 默认版本升 v1.5.0；logstore
> 错误改 `log.Printf` 可观测。剩余：max_engine_version 上限校验需先定
> `v1.4.x` 通配语义 + 更新导出默认（避免既有 KB 集成用例被拒）；matcher
> reload 并发待 R1 接线时一并加锁；LLM 计划字段透传待 M6C 增强。
>
> **状态更新（2026-08-20 续23，max_retries 消费待定案）**：复盘发现 `step.max_retries`
> 的消费存在规格冲突，不能随手改：① GLOBAL §5 #2 裁决把 DEADLINE_EXCEEDED 重试硬锁为
> 「Fibonacci 1s/2s 最多 2 次」（`FIBONACCI_WAITS` 仅 2 档），step.max_retries 无法上调
> 该上限；② ABORTED（物理遮挡）当前常量 3、REINIT 当前常量 1，若改为消费 max_retries
> （默认 2）会改变默认行为（3→2 / 1→2），影响遮挡自愈与崩溃恢复语义；③ guide §2.1 的
> `max_retries`（默认 2）与三类错误各自上限的关系无书面定义。候选：A 仅对 DEADLINE 做
> `min(step.max_retries, 2)` 消费（只允许调低，语义最稳）；B 全部类目消费 max_retries
> （需同步更新 guide 与既有语义，可能动 COORD-03/ENG-04）；C 维持常量不动，把
> max_retries 语义重定义为「on_failure=retry 的策略开关」。待用户定案（影响协调器
> 重试行为，非高确定性顺手项）。
>
> **已拍板（2026-08-20 续24，方案 A，价值观依据：确定性优先 + 行为兼容 + 裁决不破）**：
> `reflector.classify` DEADLINE 路径消费 `step.max_retries`（钳制 ≤2，Fibonacci 上限），
> ABORTED/REINIT 固定上限不动；guide §2.1 已同步语义；`tests/unit/test_reflector.py`
> 4 例全绿（默认 2 / 调低 1/0 / 调高钳制 5→2 / ABORTED+REINIT 不随动）。

## 审查发现（2026-08-20 续6，R1/MATCH-05/06 实现过程中的顺带审查）

> 范围：本轮读过的 pkg/engine（service.go/session.go/knowledge.go）、pkg/knowledge
> （knowledge.go/httpfetcher.go）、pkg/matcher（matcher.go/rule.go/ac.go/simhash.go/
> astar.go）、pkg/classifier、pkg/browser、pipeline/exporter。按影响排序。

### N1（中/正确性，已修复）：候选池未应用决策带

- 证据：`runAction` 在 `len(cands)>0` 分支只取最高置信度就直接执行，任何 L1 命中
  （即使 confidence < θ_low）都放行；决策带只在候选池为空（classifier 路径）生效，
  与 algorithm-kernel §5「C<θ_low → LLM 兜底」矛盾。
- **已修复**：候选池统一走 `matcher.Decide` 决策带——direct 直接执行 / astar 多步搜索 /
  fallback NOT_FOUND；回归：既有 L1 高置信（0.95/0.90）与 L2 0.7 用例全绿，无低置信规则受影响。

### N2（中/一致性，已修复）：NOT_FOUND 计数口径不统一

- 证据：classifier fallback 路径调用 `noteNotFound`（P0 失明信号），但 negative_keywords
  拦截路径不计数——坏版本用黑名单封杀语义时不会触发自动回滚（R1 自动回滚测试暴露）。
- **已修复**：blocked / L3 全剔除 / A* 无路径三条 NOT_FOUND 路径统一 `noteNotFound`，
  与 spec §5.1「连续 50 个 NOT_FOUND = 大面积失明」口径一致。

### N3（低/模拟局限，记录不修）：页面候选显式置信度路径绕过黑名单

- `pageCandidatePool` 对 `Confidence>0` 的页面候选直接入围（不经过 MatchQuery 黑名单）；
  该通道目前仅测试注入可达（真实 WASM/CDP 落地后走逐节点 MatchQuery），记录为模拟局限。
- 2026-08-20（续12）：真实 CDP `PageCandidates` 已携带页面级 HTTPStatus（主文档 ≥400
  → 整页候选物理湮灭入 A* 黑名单），模拟显式置信度通道仍按原样记录不修。

### N4（低/已修复，2026-08-20 续7）：max_engine_version 上限校验

- `parseVersion`/`compatible` 只处理 `min_engine_version`；`max_engine_version` 解析后丢弃。
- **已修复**：`knowledge.Artifact` 新增 `MaxEngineVersion`；`compatible(min,max,engine)` 双边界
  校验（`x` 通配：`v1.4.x`=v1.4 任意 patch、`v1.x`=v1 任意 minor/patch，空=无上限）；
  httpfetcher 从 manifest 透传；引擎内置 artifact max=v1.5.x；exporter 默认
  `max_engine_version="v1.5.x"`（与当前引擎 v1.5.0 对齐，避免产物被自己拒载）。
  验证：Go 逻辑层 9 例（`TestKb05MaxEngineVersionEnforced` + `TestCompatibleMaxWildcardUnit`）、
  httpfetcher 透传断言、契约层 `test_tdd_kb_05b_max_engine_version_reject`（真实 exporter
  产物 + force 跳过）全绿；`make pre-commit` 61 passed/13 skipped + 契约 10 passed。

### N5（低/已确认，维持）：mock scorer 未知语义 0.90 执行

- `featuresFor` 对任何非困难负样本语义返回 [0] → mock 0.90 → 执行；这是模拟决策核的
  测试缝（R1 契约用例依赖它区分「无规则 → 0.90」与「规则命中 → 0.80」），
  真实 ONNX 导出后由模型打分替代，不改。

### N6（中/正确性，已修复，2026-08-20 续9 E2E 实测发现）：真实 CDP 候选通道假 NOT_FOUND

- 证据：`pageCandidatePool` 对 Confidence=0 的真实 DOM 采集节点逐节点调用
  `MatchQuery(n.Text)`，页面上任意命中规则的无关节点（如「登录」按钮）都会入围，
  随后 L3 `RefineByStructure` 以目标语义（如「新标签」）为参考把全池剔空 →
  返回「L3 structure mismatch」假 NOT_FOUND（ENG-13 E2E 实测，目标语义不在规则库时必现）。
- 影响：真实 CDP 页面通道下，规则库未覆盖的语义会系统性误判为「页面无此节点」，
  与物理层 findElement 的语义直查能力矛盾。
- **已修复**：`pageCandidatePool` 对 Confidence=0 节点仅吸收 L2 同义词候选
  （离线背书、跳过 L3 防误伤）；L1 由语义直查路径覆盖（MatchQuery(target_semantic)
  未命中 → classifier 兜底 → 物理层 findElement 精确定位）。显式置信度节点
  （探针/ONNX 打分）行为不变，MATCH-05/06 全部用例保持全绿。
### 15. 桌面版实测遗留（2026-08-26 登记，uws 实测后）

- **Tauri 原生窗口 + sidecar 启停未真实验证**：本轮 uws 驱动的是 Vite 前端
  （浏览器预览态，sidecar 按钮降级提示）。原生 `npm run tauri dev` 需验证
  sidecar 拉起（引擎/网关已占用端口时的冲突表现）、日志转发与状态栏联动；
  **状态更新（2026-08-26）**：调试 Chrome 已改为网关自动拉起（`EnsureChrome`，
  9222 不可达即启动持久 profile Chrome），不再依赖手动 `scripts/start_*_attached.sh`；
  原生窗口验证仍待跑。
- **引擎 Target 池随 session 累积耗尽**：每个新 session_id 开一个新 Tab，
  未 DELETE 前不释放（实测 17/24）。网关需会话上限/LRU 淘汰 + 桌面端
  「关闭会话」入口；引擎侧考虑空闲超时回收；
- **Gemini 桌面 UI 轮未跑**：DeepSeek 全链路已通过；Gemini 走同一网关代码
  （provider 有单测），但真实 UI 轮（含 Extended thinking 开关）待网络稳定
  后补测；
- **`desktop/`、`dev/`、`scripts/` 纳入 Git（已销账，2026-08-26 目录整理）**：
  M1 桌面工作、测试页、启动脚本已提交纳入版本控制；desktop 移入 `cmd/desktop/`
  （与 uws/uws-engine/uws-gateway 同目录）；desktop/.gitignore 已排除
  node_modules/dist/target/binaries 等构建产物。

### 25. 外部破解回调防伪造与超时降级（TDD-SEC-04 / TDD-SEC-05，已点亮）
- **状态更新（2026-08-20）**：`coordinator/solver_callback.py` 实现 HMAC-SHA256 验签（`X-Solver-Signature` 请求头）与防状态机污染机制；实现 `AsyncSolverManager` 管理 2s ACK 超时、60s 破解等待超时与重试 1 次超限升级 `UNAUTHENTICATED` 并接管；`tests/security/test_solver_callback.py` 全绿。

### 26. 零配置环境启动与 CLI 配置查询（TDD-CLI-06，已点亮）

- **状态更新（2026-08-20）**：`cmd/uws/config.go` 补齐 `config get` 子命令与敏感字段 `<redacted>` 保护；`tests/e2e/test_cli_zero_config.py` 验证全新临时 HOME 目录无配置文件时 `cmd/uws-engine` 与 `cmd/uws` 进程稳健运行、不崩溃。

### 27. 附着模式视口覆盖 bug（2026-08-31 用户报，待修）

- **现象**：附着调试 Chrome 操作 Airwallex 时，页面宽度与浏览器窗口宽度不一致
  （实测：屏 1536x960，窗口 outer 1536x935，页面 inner/visualViewport=1920x1080）。
- **根因（2026-08-31 用户诊断实锤「屏幕宽度被其他进程影响」）**：双因素——
  ① 引擎 `InitializeContext` 无 viewport 时默认 1920x1080（service.go:236），附着真实
  Chrome 时对该标签页应用 `Emulation.setDeviceMetricsOverride`（cdp_session.go:353）；
  ② 共享调试 Chrome（9222）被其他进程附着（drama 390x844 移动端自动化）后，其
  setDeviceMetricsOverride 会落在当前标签页上，Airwallex 页被套 `screen=390x844`。
- **候选方案**：A 附着模式未显式指定 viewport 时跳过 override、采纳实际标签页视口
  （引擎截图/坐标用实际尺寸）；B 先 CDP 读回实际视口写入 cfg.Viewport 再按原逻辑；
  C 用 `Browser.setWindowBounds` 把窗口拉到默认视口大小（侵入用户窗口，否决）；
  D 不同视口任务隔离：独立 profile/端口，或引擎每次附着先 clear 再按需设置。
- **建议**：A + D（附着跟随用户窗口；任务级 viewport 显式设置并在结束清理；
  headless 启动模式维持默认 1920x1080 不变）。
- **状态更新（2026-09-03，A + D 已落地，TDD-ENG-17）**：`browser.Config` 新增
  `ViewportExplicit`（engine 层只在请求显式带 viewport 时置真）；`cdp_session.go`
  附着模式未显式指定视口时先 `Emulation.clearDeviceMetricsOverride` 清除残留覆盖，
  再 `adoptWindowViewport` 读实际 `window.innerWidth/innerHeight`（兜底 outer）写入
  `cfg.Viewport`——页面宽度=窗口宽度，不再“比窗口宽又无横向滚动条”；显式视口
  （移动模拟等任务级需求）在附着模式仍按配置覆盖；headless 启动模式维持 1920x1080。
  验收：`TestCDPSessionAttachMode` 真实 Chrome 断言窗口 1280x900 跟随 +
  显式 390x844 覆盖生效。

### 28. DSL/chat 层缺 drag 动作（2026-09-02 滑块演示实测；目标层级 DSL > chat > python）

- **现象**：`uws run --plan "drag:124,200,300,200"` 报「非法 DSL 动词 `drag`」
  （可用 `['click','extract','focus','hover','input','scroll','search','select','wait']`）；
  `pkg/vlm` Capabilities 亦无 drag（click/crop_zoom/extract/input/back/done/impossible）。
- **背景**：引擎已有 drag 原语（`ExecuteAction expected_action="drag"` 坐标直发，真实
  Chrome 拖 range 滑块 50→100 及本演示页 12→100 均验证通过）；用户定案接口层级
  DSL > chat > python，drag 需上提。
- **候选方案**：A planner.py 增加 `drag:<x1,y1,x2,y2>` 动词（校验 4 坐标，直通引擎）；
  B VLM Capabilities 增加 drag（VLM 报起点/终点后引擎执行）；C 两者都做（DSL 为主路径，
  VLM 只在坐标不可确定性获得时兜底）。
- **建议**：C。
- **状态更新（2026-09-02，方案 A 已落地）**：planner.py 新增 `drag:` 动词（`DSL_COORD_VERBS`，
  坐标校验镜像引擎 `ParseDragCoords` 容忍度，target 固定语义 `drag`），executor 通用映射
  透传 value；TDD-PLAN-10 验收 3 例全绿，`uws run --plan "drag:..."` 端到端打通。
  方案 B（VLM Capabilities 增加 drag，坐标不可确定性获得时兜底）仍待办。
- **另发现（attach 模式多标签语义串扰）**：extract「验证通过」命中同浏览器另一标签页
  （D&B 滑块验证页「拖动滑块使滑块完全填满」）而非当前上下文页面，疑似候选池未按
  target 隔离，需排查（与 GLOBAL #72 视口污染同属共享调试 Chrome 隔离问题）。

### 29. WebTouch 分层定稿后的执行待办（2026-09-02 登记）

定稿：`docs/web-touch-architecture-2026-09-02.md`（六层结构 + 需求落层 R1-R8）。

- **R1 spike（页内直发闭环）**：在 9222 调试 Chrome 上对 DeepSeek/Gemini 各发一回合，
  不点 UI、不读 DOM：页内 `fetch`（Runtime.evaluate）→ 现有 SSE/NDJSON 解析器 →
  API 流式返回；顺带抓请求模板（含多轮 continuation 字段）与反爬行为对照。
- **状态更新（2026-09-02 续6：R1 首轮实证完成一半）**：DeepSeek 请求模板与 SSE
  原文已捕获（`tmp/r1-deepseek-template/round3/`）：create_pow_challenge →
  chat/completion（body `chat_session_id + parent_message_id` 即多轮 continuation），
  SSE 仍是补丁式协议（GLOBAL #36）。页内 fetch 直发网络层可行（同源、Cookie/Token
  自然携带、现有 SSE 捕获机制同样可武装），但 **DeepSeek 现强制每请求 PoW
  （DeepSeekHashV1，difficulty=144000）**：无头 40300 MISSING_HEADER、旧 pow 头
  40301 INVALID_POW_RESPONSE。官方 solver 是跨域 Worker chunk（37627 wasm /
  76608 JS 回退 + 8138 依赖 + sha3_wasm），页内直建跨域 Worker 被 Chrome 拒，
  blob 注入 JS 回退 60s 无解，离线公式未破 → **闭环卡在 solver 复现**。
  下一步：直接调用 `sha3_wasm_bg.7b9ca65ddd.wasm` 的 `wasm_solve`
  （或逆向 DeepSeekHashV1 prefix 公式）后接页内 fetch → 现有 sseSink → 流式 API。
  Gemini 网络不可用，暂缓。
- **R4 site recipe schema + POC**：定义 YAML 站点规则 schema（transport/auth-profile/
  discovery/endpoints/pages/signals 多候选/checks 漂移校验），拿 Reddit（http .json）
  与一个登录墙站（browser/in-page-direct）验证「探测→recipe→run→漂移检测」闭环。
- **R6 HITL 最小闭环**：内核 block 事件类型化 + 应用层状态机
  （BLOCKED→NOTIFIED→ARMED(人 ready 单次许可)→ACT→RE-EVALUATE）+ 站点级冷却；
  先接一个通知通道（终端/飞书）。
- **R5 账号体系选型（待用户拍板）**：vault（age+Keychain / 1Password）与 OTP 邮箱
  通道（IMAP app 密码 / OAuth）；选型后做账号矩阵 schema + 2FA 决策图。
- **命名收口批次**：UWS → WebTouch 的文档/技能名/CLI 文案机械改名，单独成批执行，
  不混入功能改动（uws-browser-agent skill 更名含 `.agents/skills` 软链）。

### 30. 全系统降噪（下会话，2026-09-02 登记）

执行清单：`docs/noise-cleanup-inventory-2026-09-02.md`（A-F 六组）。

- 本轮已完成：冲突文档打标记（architecture-and-planning / gateway-contract / README /
  AGENTS / GLOBAL §1）、登记候选清单、追加本条目——**未动代码、未删文件**。
- 下会话按 F 组顺序：只读复核 → A 批量改名单独 commit → B 文档降级修正单独 commit →
  D 逐项确认（删除/归档需用户确认）→ C 只加 import 边界与注释 → 收尾更新清单/GLOBAL。
- 决策点：CLI/二进制名 `uws*` 是否保留（对外破坏面），下会话需用户拍板。
- **进度（2026-09-02 续）**：F1 只读盘点完成；B 文档降级、C 边界注释、A 产品名术语收口
  （仅 prose，CLI/代码字面量/文件名保持）已落地（未 commit）；待拍板：CLI/二进制名、
  skill 目录名；D 组逐项确认待用户过目。
- **待拍板项已在 2026-09-02 续4 定案**：正式 CLI = `webtouch`（`uws` 兼容别名）、
  skill = `webtouch-skill`；D 组由 Agent 自决（保留认知资产：archive/cdp-research 双文档
  等不删，仅补 ignore/策略）。
- **进度（2026-09-02 续5：C 组结构落地）**：api_server + providers 迁入
  `coordinator/app/`（Python 应用层与核心同包但目录边界清晰）；Makefile 新增
  `audit-layers`（核心禁止反向依赖应用层）并挂入 pre-commit；遗留的两处
  「Universal Web Surrogate」帮助断言随命名收口修复；可再生构建产物/缓存清理
  （根 uws/tier1/tier2、bin/uws-engine、build/、.gocache/、desktop target/
  node_modules、各类缓存 ≈1.5GB）。全量门禁绿（Go 全包 + Python 189/12skip + 契约 13）。
- **降噪清单状态**：A/B/C/D/F1 全部落地并 commit；仅剩本清单「收尾」行与
  GLOBAL/USER_ACTION 状态同步（随本批完成）。

### 31. 目标模式 VLM 行为缺口（2026-09-03 京东电煮锅 --debug 实测登记）

已加 `uws "目标" --debug`（TDD-GOAL-09，中间过程可观测），实测暴露三处行为缺口，
均不在本批改策略（先保观测与文档）：

- **输入不提交**：京东首页 VLM 连续 7 轮 `input node_id=1554/1562`
  text="电煮锅 1-2L 600W"，引擎 input 全 success=True 但 URL 不变——目标模式
  缺「输入完成后回车/点搜索按钮」的动作链引导。候选：A SYSTEM_PROMPT 增加
  「input 后下一步必须提交（回车或点搜索按钮）并确认 URL 变化」；B input 动作
  执行后引擎侧做 value 落地自检并回显（可区分「没填上」与「填了没提交」）；C
  DSL `search:` 宏映射到目标模式（VLM 决策 input 时提示可用 goto 搜索 URL）。
- **动态页绕过重复守卫**：京东首页 AXTree 每轮变化（1430→1735→1560→1757 行），
  全树 md5 签名让「同节点+同动作」判不重复，直到同节点连续多轮才熔断。候选：
  重复判据改为「ref 节点子集签名/目标语义相关片段」或引入滚动位置+首屏可见 ref
  签名；换决策征询后仍回同决策时第二次直接熔断并给 goto/搜索 URL 建议。
- **每轮 prompt 膨胀**：93KB AXTree 全量进每轮 prompt（45k~60k token，usage
  实测见操作记录）。候选：ref 树预算化（只喂目标相关片段/前 N 条 + 折叠）；
  done/决策后附 usage 统计（--debug 已有），默认模式对超大树截断时打警告。

- **进度（2026-09-03 执行批次，TDD-GOAL-10/11）**：
  - 输入不提交：落地「重复 input → 自动点唯一搜索/提交按钮；无候选给一次换决策
    提示，仍重复即熔断」+ SYSTEM_PROMPT 提交规则（候选 A 大部分落地）。未做：
    候选 B 引擎 value 落地自检回显（需 Go/契约改动）；候选 C DSL search 宏（另行）。
  - 动态页绕守卫：落地 ref 规范子集签名 + 重复 input 判据改用（URL, 输入框文本,
    值），node_id/全树噪声不再参与；仍可后续做角色 nth 稳定键与首屏 ref 签名。
  - prompt 膨胀：落地 VLM 快照降面（20k 字符预算：ref 完整 + 正文标题/链接摘要），
    debug usage 统计此前已落地；「默认截断警告」由压缩标注替代（每轮恒定有界）。

## 2026-09-22 用户现场反馈批次（AutoDL 微信登录实战暴露，按优先级排列）

> 场景：用 WebTouch 驱动真实 Chrome 完成 AutoDL 微信快捷登录 + 取 token。以下每条都带
> 现场证据，**都需要改代码**（不是配置能解的），需用户点头后按 TDD 落地。

1. **新窗口/新 target 不接管 → 登录仍需人工（最高优先级，P0）｜✅ 已实现（2026-09-22，TDD-ENG-21）**：
   - **落地**：`browser.TargetSwitcher.SwitchTarget`（attach 新 target + 重开 CDP 域 + 重放身份/视口/探针 +
     清页面时序 + 旧 session 事件摘除与新 session 重挂 + 观看流迁移 + detach 旧 session，原 Target 不关、
     全程不抢前台）；引擎默认接管，`context_hint.follow_new_target=0` 可关；契约新增 `new_target_id` /
     `active_target_id`；协调器在 `active==new` 时前移钉扎 Target 并继续任务（不再 PAUSED）。
   - **验证**：`pkg/engine` sim 3 例 + 真实 Chrome `TestCDPSessionSwitchTarget`（`UWS_RUN_CHROME_E2E=1`
     实测 PASS）+ coordinator 集成 2 例；`go test ./pkg/...` 与 `tests/integration/test_coordinator.py` 全绿。
   - **遗留**：`--follow-new-target` 未做成 CLI 显式开关（现由引擎默认接管 + hint 关闭覆盖，等价能力已具备）；
     真实 AutoDL 微信登录全自动点「允许」尚待在用户 Chrome 上现场复验（需重启引擎加载新二进制）。
   - 原始现象（存档）：微信授权页点「快捷登录」后弹出授权弹窗，`允许` 必须用户手点；引擎报告
     `new_window_opened` 后停下，且后续语义定位在原 Tab 上失败。
   - 现状证据：引擎侧**已能检测**新 target（`pkg/browser/cdp_session.go`
     `newWindowTarget` 字段 + `NewWindowOpened()` 一次性读取，TDD-ENG-13），但协调器读到后
     只报 `new_window_opened`「当前版本不自动切换活动 Tab」；`coordinator/cli.py` 的报告里
     新窗口 AXTree 已能读到，动作却打不到。
   - 原始要求（存档）：`run`/`goal` 检测到新 target 后自动接管（同会话内可多 target），或提供
     `--follow-new-target` 显式开关；这是「微信登录全自动」和「操作非前台/其它 Tab」
     两个需求的同一根因。
2. **观看小窗：只留一个 + 能关掉 + 展开可用（P0）｜✅ 已实现并真机复验（2026-09-22，TDD-ENG-22/23/24/25/26）**：
   - **落地**：
     a) **单一面板（TDD-ENG-22）**：`ViewerHostController` 只留一块面板，新增纯逻辑 `ViewerPanelRouter`
        决定它在放谁（跟随最近**有动作**的会话；只有帧没有动作的会话抢不走面板）；`ViewerSessionRegistry`
        级联槽位与 `ViewerPanelAdmission` 退役，面板位置=用户拖到哪儿是哪儿。
     b) **✕ 是真关（TDD-ENG-23）**：壳真丢面板与会话绑定 + 新增 `intent:"close"`；引擎 `viewerhub.Session.GoQuiet()`
        立刻静默回收（停推流 + 一条 `sessionClosed`，并清 `quietHold`）。不结束会话、不关真窗口、不搬动任何窗口。
     c) **展开超时只回落（TDD-ENG-24）**：`expansionTimeout` 1.2s → 5s（对齐引擎 `viewerIntentTimeout`）；
        新增纯逻辑 `ViewerExpansionMachine` 把「超时回落」与「用户取消」分状态，迟到的 `ok=true` 把自绘放大面板
        **升级**成真窗口（旧代码在这里反而补发 `collapse` 把真窗口搬回去——这就是「点开只是一张点不动、滚不动的图」）；
        `expandedCanvas.onCollapseRequested` 改走上报宿主。
     d) **展开 = 把真窗口抬到最前 + 互斥不变量（TDD-ENG-26，2026-09-22 用户新口径）**：引擎不再搬位置、不缩放，
        `RaiseViewerWindow` 只做「必要时救援进屏内 + 激活」并回执真实矩形（`pkg/browser/viewer_window.go`），
        壳拿它当「飞向它」动画的落点（0.24s）后 `orderOut`；`ViewerExpansionMachine` 删掉 `.live`/`.collapsing`
        与 collapse 回执链路（`window_intent.go` 的 `collapse` 只剩「还原救援前位置」）。
        **互斥不变量：小窗直播 ⟺ 真窗口不在前台**——壳 0.4s 轮询前台应用（`ViewerPanelPresence`，AppKit 无关、
        可单测）：浏览器在前台就 `orderOut`，被挡到后面就亮回来继续直播。引擎拉起的窗口按 `browserPID` 认，
        附着模式没有 pid 只能按应用类别（Chrome/Chromium/Brave/Edge）认（刻意偏向多藏一次小窗）。
        配套把可见性判据从「相交即可见」换成「可见面积 ≥ 50%」（`visibleEnoughForViewer`），40px 夹取边不再算看得见。
     e) **收回胶囊退役 + 协议放宽**：新口径下收回 = 「切回去用别的应用」（前台轮询自动亮回来）或 ✕ 真关，
        `ViewerCapsule.swift` 与它的 placement 测试整体删除；wire 协议上 `expand` **不再要求 `size`**
        （`pkg/viewerhub/intent.go` 放宽为不带 size 也接受，老壳带 size 仍接受但不参与判定）。
   - **验证**：`cmd/viewer` `swift test` **68 例 0 失败**（`ViewerPanelRouterTests` 9 + `ViewerExpansionMachineTests`
     12 + `ViewerPanelPresence` 3；删掉胶囊 placement 测试后总数由 74 → 68）；`go test ./pkg/... ./cmd/...` 全绿；
     release 壳与守护进程均重建。
   - **现场复验（2026-09-22 19:40/19:41，真机一次真实任务）｜✅ 已完成**：`viewer.log` `expand requested:
     session=ctx-1` → 273ms 后 `intent result: intent=expand session=ctx-1 ok=true reason=-`；`engine.log`
     `真窗口停在屏外（1496,60 1440x900，工作区 0,25 1536x935），已搬进屏内 {24,49}` +
     `观看壳抬到最前 window=1621951157 原位={1496,60} 生效={24,49}`；小窗两个窗口均 `hidden`、`front`=Google
     Chrome（点开期间自动化没抢前台，守 R1）。互斥不变量**双向**验证：切到访达 → `panel shown: 浏览器已让开`
     （小窗回到 onscreen）；再点开 → `ok=true`、Chrome 重新前台、小窗再次 `hidden`。
   - **本轮真机发现并修掉的两个缺陷**：① `pkg/viewerhub/intent.go` 原对 `expand` 强制 `Size != nil && IsUsable()`，
     新壳按新口径不发 size ⇒ 整条意图被判畸形**静默丢弃（连 `intentAck` 都不回）**，现场症状与旧 bug 一模一样
     （「点开只有回落的自绘面板」）——已放宽（`TestExpandIntentNoLongerRequiresASize`）；② 前台轮询在飞走动画
     中途 `orderOut` 把动画截断（`intent result` 后 ~0.24s 出现 `panel hidden: 浏览器在前台`）——
     `suppressPresenceHide` 改到 `flyAwayAndHide` 的 completion 才清。
   - **仍未复验（不是代码）**：① 多会话同时活跃时「只有一块面板 + 跟随最近活跃」的**换绑**路径没在真机走到；
     ② ✕ 真关与本条 P1 排版现场效果未复验；③ 静默会话被复用时不唤回面板（现象已观察、原因未定）。
   - **更深缺陷（已识别、未修）**：意图解码失败时 hub 不回任何 `intentAck`（静默丢弃），壳只能等满 5s 超时回落。
   - **本轮侦察结论（代码级，可直接开工）**：
     a) **单一面板是回到原始需求、不是新设计**：`docs/viewer-requirements-2026-09-11.md` §3.6 早已写死
        「默认全局单小窗，跟随最近活跃会话自动切换视图；支持列表切换或 pin 固定；多会话并列多窗列为**待定项**」。
        实现（`ViewerHostController` + `ViewerSessionRegistry` 级联槽位）走的是「每会话一块面板」，属偏离。
        ⇒ 改动面：`cmd/viewer/Sources/ViewerCore/ViewerHostController.swift`（只保留一块面板，按最近有动作的
        会话重绑 `controller.update(session:)`；位置保持用户拖到的位置、不再级联），并新增纯决策类型
        （建议 `ViewerPanelRouter`）承载「该显示哪个会话、关掉后何时回来」，便于 `swift test` 覆盖；
        `ViewerSessionRegistry` 的级联槽位与 `ViewerSessionRoutingTests` 的级联断言随之退役。
     b) **「展开后只是图片」的根因已定位（高置信，待真机复现确认）**：壳的展开回执超时只有 1.2s
        （`ViewerHostController.expansionTimeout`），引擎那次 intent 要做 5~6 次 CDP 往返
        （`windowIDForTarget`/`getWindowBounds`/`setWindowState`/`setWindowBounds`/`evalValue(screen.*)`）
        且 `viewerIntentTimeout` 本来是 5s ⇒ 忙页面上极易超时：壳先回落到自绘放大面板（就是「点不动、滚不动」
        的那张图），**随后**引擎的成功回执到达时又走 `case .idle` 分支主动补发 `collapse`
        把真窗口搬回去 ⇒ 用户永远看不到真窗口。修法：超时只回落「不撤销」（区分「超时」与「用户取消」），
        迟到的 ok=true 把自绘面板**升级**成真窗口；`expansionTimeout` 与引擎 5s 对齐。
     c) **关闭语义的落点（自决并声明）**：✕ = 壳侧**真丢**面板与会话绑定（不再只是隐藏态），并向引擎发一条
        `close` 意图让引擎立刻进入该会话的静默回收（`viewerhub.Session` 已有 `quiet` 机制：停推流、
        保留会话与真窗口）。**不**销毁在跑的任务/真窗口 —— 与 GLOBAL #110 的安全语义一致；
        「含空会话回收」按「面板与会话记录真回收 + 引擎静默回收」落地，不引申为「点一下关掉别人的任务」。
   - **状态**：上述 a/b/c 三条已按 TDD 全部落地（`docs/tdd-checklist.md` 的 TDD-ENG-22/23/24）；
     留下的是真机现场复验，不是代码。
   - 现状：`pkg/viewerhub/hub.go` 明确「one panel per session」——今天一次 AutoDL 实战就建了
     ctx-1…ctx-7 七个会话，屏幕上七个面板，用户口径「看不过来」。
   - 关闭：GLOBAL #110 的壳侧关闭只收起该面板，会话仍在；新动作还会把它弹回来。
   - 展开：`pkg/browser/viewer_window.go` 的做法是把**真窗口搬到屏内**；壳自绘的放大面板
     按源码注释「只是图片：点不动、滚不动」。用户要的是「展开后网页就在小窗里处于网页状态」
     或「把浏览器整体恢复出来」。
   - 要求：单一面板 + 跟随当前活动会话；关闭 = 真关（含空会话回收）；展开二选一落地
     （面板内可交互需输入透传 + 高清流，或整体恢复浏览器窗口）。
3. **默认窗口分辨率太小（P1）｜✅ 已实现（2026-09-22）**：
   - **落地**：`pkg/browser/window.go` 默认 `500x540` → **`1440x900`**；`coordinator/attach.py`
     `DEFAULT_WINDOW_SIZE` 与 `pkg/gateway/chrome.go` 冷启动参数同改 `1440x900`；
     `UWS_WINDOW_SIZE=WxH` 覆盖语义不变。窗口默认仍停在可见屏幕之外，所以放大默认值不影响
     「屏幕上只有直播小窗」；展开时窗口几何由壳决定。
   - **验证**：`go test ./pkg/browser ./pkg/gateway`、`pytest tests/unit/test_attach.py`（14 passed）全绿；
     真实 Chrome（附着 9222）`TestCDPSessionOwnWindowKeepsFocus` 实测窗口按 1440x900 创建（±80 容差内）。
   - **顺带发现（未修，超出本批范围）**：同一次实测日志显示`停靠被显示器布局夹回可见区
     （读回 left=1496 top=60 …工作区 0,25 1536x935）`——**与本次尺寸变更无关**：
     用变更前的 `500x540` 复跑得到同样的 `left=1496`，即 Chrome 153 的窗口位置夹取规则比
     `#110`/`GLOBAL` 记录的 (1730,-41) 更保守，真窗口今天会在右边缘露出约 40px 竖条。
     属既有缺陷，需按显示器布局重新设计停靠判据（见本轮报告）。
   - 原始要求（存档）：`pkg/browser/window.go` 默认 `360x540`、`coordinator/attach.py` 冷启动默认
     `460x880`，页面展示不全；建议 `1440x900` 并保留 `UWS_WINDOW_SIZE` 覆盖；默认值变更需重新 build。
4. **账号密码 / 出口代理统一管理（P1）｜✅ 选型已定 + 凭据库 v1 已实现（2026-09-23，TDD-ENG-29，GLOBAL #119）**：
   - 现状：**没有实现**。`docs/web-touch-architecture-2026-09-02.md` R5「Agent 专用账号体系：
     密码本、2FA、邮箱验证码」归应用层且「选型待用户拍板（vault/邮箱通道）；先 HITL 兜底」；
     同文件要求「凭证不进 Skill/YAML，只引用账号档案（应用层 vault）」，`USER_ACTION.md`
     亦记「vault（age+Keychain / 1Password）与 OTP 邮箱通道待拍板」。
   - 已有的是**代理/身份绑定**：`uws identity list|show|bind|unbind|resolve|use|check`
     + 契约里的 `ProxyConfig{host,port,username,password}`，但 GLOBAL #98 明令代理 URL 不得内嵌
     账号密码 ⇒「账号密码 + 出口代理统一管理」目前只有设计，没有实体。
   - ~~需要用户决策：vault 方案（age+Keychain / 1Password / 自建）~~（**已定：用 age**，见下）；OTP 邮箱通道仍未定。
   - **2026-09-22 用户拍板**：AutoDL 账号密码**作为第一条档案入库**；且「后续的密码管理都并到这套 vault 一起」
     （即第 5 条不再单独立项，统一走这里）。
   - **2026-09-23 用户定案 + 落地**：「3 安全的话就用 age」⇒ 用官方纯 Go `filippo.io/age`（无外挂二进制），实现 `uws vault`
     （`pkg/vault/` + `cmd/uws/vault.go`）：密文 `~/.uws/vault.age`（0600、X25519 + ASCII armor、整库加密、**条目名也不泄露**）、
     identity `~/.uws/vault.key`（0600）；`set` 终端无回显、值不进 argv/history、只回指纹；`get` 是唯一明文出口；`list` 只给名字+指纹。
     fail-closed（缺文件/坏密文/identity 不匹配一律报错，不退化成空库）；单测 8 例 + 真机 CLI 全链路已验证（18 包 ok / 0 FAIL）。
     边界诚实声明：identity 与密文同目录 ⇒ 防误传/误提交/备份泄露，**不防本机已被读取**；硬化方向 = identity 进系统钥匙串。
   - **仍未做**：①**用户真实库尚未写入任何条目**（首条 = AutoDL 密码，须用户本人 `uws vault set autodl` 输入，Agent 不接触明文）；
     ②钥匙串存放 identity；③给消费方（`bin/autodl_run.py` 等）的非交互取值通道（`--exec`/环境注入）；④OTP 邮箱通道。
5. **AutoDL 账号密码设置（2026-09-23 用户授权「你自己操作，我不需要记这个密码」，进行中）**：控制台入口为「账号 → 账号安全」
   （首页 AXTree 实测存在 `账号安全/访问记录/子账号/设置` 菜单项），设密码大概率需要短信验证 → 需用户在场配合；未完成，不得记为已完成。
   - **2026-09-23 现场侦察结果**：①登录态正常（炼丹师9550，会话 `autodl` / Target `F8CDE016CD4A8171EAF22B5B0AF0DA0C`）；
     ②**【02:10 终版更正】账号安全真实 URL = `https://www.autodl.com/console/center/account/safe`**（在登录态首页 `click:node:562` 点到「账号安全」后，
     用 CDP `/json/list` **直读**该 Target 的真实地址确认）。前两次结论**都是错的**：`/console/account/security`、`/console/center/security`
     **都是 404 页**，`/console/account/index` 也不是账号安全；**判据**：AutoDL 的 404 外壳固定渲染「去首页 Ns」按钮 + 页脚
     `docs`/`home`/`register`/`login` 四条链接，而这四条链接在**所有页面（含已登录页）的页脚都存在** ⇒ **不能拿它们当未登录的证据**
     （这正是 02:00 那次「登录态失效」误判的来源）；
     ③侧栏「账号」是**二级菜单**：不 hover 不渲染子项、子项**无 href**（走前端路由），「账号安全」的确定性 node 引用是 `[562]`；
     ④**文本点击在二级菜单里不可靠**：`click:账号安全` 报 ok 但 URL 不变，`hover:账号` + `click:账号安全` 落到了**邻项**「访问记录」；
     ⑤`click:node:562` 被引擎判 **`challenge_intercepted` 并 PAUSED**，失败截图显示页面完全正常、无验证码 ⇒ **已定位为误报并修复（根因见下）**；
     ⑥SPA 控制台页在 `probe` 的快速快照下是**全白**
     （不等 SPA 渲染），改用 `run --plan "nav:<url>"` 才有稳定 AXTree。
   - **【02:00 已闭环】根因（已实证 + 已修 + 已真机复验）**：`challenge_intercepted` 不是 captcha 容器误命中，
     而是**防乱点熔断被上报成了人机验证**——`no_match`（元素确实不存在）连续 3 次 → `CircuitBreaker.RecordFailure` 置 OPEN
     → 熔断门禁沿用 `codes.Unauthenticated`，而 UNAUTHENTICATED 在 proto 契约里等价于 `CHALLENGE_INTERCEPTED`
     → 协调器走「VLM 破解 → 人工接管」，最终 `PAUSED reason=challenge_intercepted`。复现证据：空白页（about:blank）
     连点 3 次不存在的元素，第 4 次即报验证码，而页面无任何挑战。修复：按**信号来源**分流
     （`ChallengeSignal.IsHumanChallenge()`）——熔断自产信号（Anti-thrashing / CircuitBreaker）改报 `RESOURCE_EXHAUSTED`
     （消息含 `anti-thrashing guard OPEN (not a human challenge)`），真挑战仍走 `UNAUTHENTICATED`；
     协调器对 RESOURCE_EXHAUSTED 改为透传引擎原话（不再泛化成 `resource_exhausted`）。单测 + 全量测试 + 真机复验均过。
   - **下一步第一个动作**：直接 `nav:https://www.autodl.com/console/center/account/safe`（已确认为正路，
     不必再走侧栏二级菜单/文本点击）；到页后再看是否需短信验证。
   - **【02:10 撤回】「登录态已失效」是误判 —— 登录态正常，无需用户重新登录**：同一浏览器/同一 Target 上
     `/console/homepage/personal` 渲染出**完整控制台**（账号 `炼丹师9550`；个人认证/会员等级与权益/我的订单/我的账单/我的代金券/开发票/
     账号安全/访问记录/子账号/设置 等菜单齐备），`/console/center/account/safe` 亦正常渲染。02:00 看到的「未登录外壳」实为
     **`/console/account/security` 这个 404 页的外壳**；换页复核即证伪。代价：白白把密码线与 AX 决定性样本卡了一轮，用户 02:1x 现场指正后更正。
     **教训**：判断 SPA 登录态**必须看内容区**（侧栏/主体），不看页脚链接或单个 URL 的渲染结果。
   - **不泄漏的写入方案（已定）**：密码由 Agent 本机强随机生成 → `uws vault set autodl`（管道、无回显、不进 argv/history、只回指纹）；
     填表用 `input:<目标>=@vault:autodl`（DSL 里**只有条目名**，明文由引擎在本机解引用）。
   - **【02:2x 已落地，原「缺通道」结论作废】凭据直填 `@vault:<名>` 已实现并真机验证**：值由**引擎侧**解引用
     （`resolveVaultRef` 插在 `execute()` 最前）⇒ 明文不进 `--plan` argv、不进协调器、不过 gRPC、不进日志（引擎快照一律 `[REDACTED]`）；
     取不到凭据**fail-closed**（报错停下，绝不把引用串填进页面）。**真机**：`input:搜索实例名称=@vault:filltest` ⇒ `SUCCEEDED` 且页面 AX 出现
     `[value=hello-from-vault]`；`@vault:nothere` ⇒ `Status: PAUSED`、原因 `vault_unresolved: 凭据直填失败 @vault:nothere: …`，页面值未被改写。
     细节与坑见 `GLOBAL.md` #122、`docs/tdd-checklist.md` TDD-ENG-31。
   - **仍未决（不得越过）**：①**明文填表**不做（`input:<目标>=<明文>` 会把密码写进 `--plan` argv 与本机 `ps`）——已有 `@vault:` 通道替代，保持不做；
     ②**短信验证码只在真正提交设密码时才会发出**，此前不向用户索要。
   - **可观测性（2026-09-23 新增，服务于「VLM vs AX 树」架构决定）**：`[AX-SHADOW]` 影子观测已在每次 VLM 视觉调用前采样
     （`pkg/engine/ax_shadow.go`，聚合 `scripts/ax-shadow-report.sh`），零行为改动。首批 6 条数据显示页面语义树很富
     （平均 95 行 / 33 条可交互 ref），但**「富」≠「能点」**——ref 编号来自探针 BFS，与 `Accessibility.getFullAXTree`
     的内容树是**两套编号**。是否把 AX 树升格为候选源 ⇒ **登录态已确认有效，可随时取「目标确实在树里、但 matcher 未命中」的
     决定性样本（期望 `hit=true` / `ref_hit=true`）再定；数据不成立就不动架构。**
   - **02:1x 决定性样本已取到，决策 = 暂不升格（数据不支持）**：累计 12 条采样（9 控制 + 3 真）。两条真样本（AutoDL 容器实例列表页，均为
     `no_match` 后花掉一次 VLM 调用）：
     ① `semantic="健康状态"` → `hit=true hit_lines=1 ref_hit=false`（命中的是 `columnheader`，**只可读不可点**）；
     ② `semantic="账单明细"` → `hit=true hit_lines=2 ref_hit=true`（命中行含探针 ref `[605] li(menuitem) "账单明细"`，但该节点是**离屏子菜单**
     ——「费用」二级菜单未展开时才有此节点）。
     **三条否决理由**：a)「AX 能读到」成立（2/2），但**「能点」未证实** —— 命中的 ref 全是离屏节点；b) 同一目标「ref 树里有、`cand=0`」⇒
     缺口在**候选构造**那一段（离屏过滤 / L3 结构校验），**不是 AX 数据量不够**，升格 AX 树解不了这个；c) **node ref 是短命引用**：
     紧接着 `click:node:605` 跨命令即 `node_not_found:605`（每次探针重新编号）⇒ 「ref 命中」≠「可直接点击」，跨命令不可复用 ref。
     ⇒ **暂不把 AX 树升格为候选源**；下一步缺口 =「探针/ref 命中的离屏叶子节点进不了候选池」，先补这一段再重估。
   - **顺带（生产侧佐证）**：上述 `node_not_found` 触发的熔断，引擎打出的是诚实的
     `anti-thrashing guard OPEN (not a human challenge): …连续 3 次操作失败无因果进展 (node_not_found:605)…` ⇒ 02:00 的 challenge 修复在生产路径继续成立。
   - **【02:3x 现场卡点】改密入口「点不到」——语义与 `node:` 两条确定性路都不可达**：①`click:修改`（登录密码行最右侧的「修改」）两次都 `Status: SUCCEEDED`，
     **页面无任何弹窗**（判据：`SUCCEEDED` 不算点到，要看页面变化）；②`[AX-SHADOW] … semantic="修改" cand=0`、`failed: no_match` ⇒ 该 `span.operation` **既不在语义候选池、也不在 ref 树**
     （`ref_hit=true` 是假命中，命中的是隐藏 dialog 文本「修改手机号」）；③/VLM 兜底两次落点 `(1379,171)`/`(1375,171)` 命中的是**行容器 `DIV.item`**（不是链接，也不是行中心），
     **y 正确（171 落在目标行 170.6~184.6）、x 偏约 340 CSS px**（目标 1717~1760）——用户现场两次反馈「鼠标总是点不到元素」「点歪了」与定量取证一致；
     ④**独立改密路由不存在**：`nav:…/console/center/account/password` 后实际 URL 仍停在 `/center/account/safe`；⑤goal 模式在本页**空转 9 轮**（把 `[481] 查看` 误当改密入口，
     点开「登录设备」再点 `close` 关掉，循环至上限 exit=1）⇒ ref 树缺节点时 node 路线会误指存在节点。**根因未定论**（缺 plan 路径的 VLM 原始比例证据），细节与证据指针见 `GLOBAL.md` #123。
   - **【02:3x 待用户拍板】是否现在就修「点不到元素」**：涉及**候选构造（离屏/非交互叶子节点进不了候选池，同 #120）或 VLM 坐标换算基准**，属新工程线。
     **默认动作 = 先记录缺口 + 先补证据**（在 plan 路径打印当轮 VLM 原始 `x_ratio/y_ratio` 与 `curW/curH/origin`，用一次 `click:修改` 换出这组数）**，等用户 GO 再动引擎**；
     **不**改 `semanticRe` 白名单，**不**为让点击成功去改页面语义。
   - **【前置未知，到那一步再问】改密弹窗若要求填「原密码」**：则需用户提供现有密码或走短信/邮箱验证 —— **属新阻塞，先停下问用户**，不得自行尝试绕过。
   - **AutoDL 密码线已完成的前置（02:3x）**：本机生成 24 位随机密码（未回显/未落盘/未进 argv）→ `./uws vault set autodl --stdin` ⇒ `已写入 autodl（sha256:448af89c5fd4）`，
     `uws vault list` 共 1 条；`nav:…/center/account/safe` ⇒ `SUCCEEDED`（会话 `autodlsafe`），页面经只读 CDP 读出六行（登录密码/手机绑定/实名认证/微信绑定/邮箱绑定/登录设备），
     **此刻全页 0 个 `input`**。**卡点 = 打开改密弹窗**（上一条），弹窗未开 ⇒ 未填表、未提交。
6. **指针轨迹 / 落点拟人化（P2）｜✅ 已实现并真机验证（2026-09-22，TDD-ENG-27，默认开）**：
   - 现状：真输入的落点是元素包围盒**正中心**，移动是一次到位（CDP `Input.dispatchMouseEvent` 单跳）——
     落点与轨迹在页面事件里是很硬的机器特征；HumanPacer 只管频控，不管轨迹。
   - 要求：a) 落点改为元素内**随机采样**（高斯偏移 + 边距，永不落在正中）；b) 真输入改**多步移动**
     （10~20 步、非匀速、微带抖动与轻微过冲回拉）后再按下/抬起；c) 静默期按概率插入 1~3 次小幅漂移；
     d) 小窗光标复用引擎 pointer 事件自然跟随（不额外造轨迹）。
   - 边界：全部是**页面内 CDP 输入**，不动系统指针、不抢前台，不破 R1（零打扰红线）。
   - **落地**（`pkg/browser/pointer_humanize.go` 新增 + `cdp_session.go` 接线）：
     a) 落点 `clickLandingPoint` 元素内随机采样（边距 min(18%,12px) + 高斯 σ=0.45×可用半径；离正中 <0.5px 沿随机方向推开；
        退化几何/无 rng 退回中心）；b) `overshootPath` 末段过冲（≤12%、上限 6px）再回拉（末段 <8px 跳过）；
     c) `pointerStepDelay` 步间隔 4~13ms 抖动——没有它同一时刻的移动会被 Chrome 合并，真机上等于没走轨迹；
     d) `startPointerDrift` 静默 ≥3s 后每 1.5s 按 0.35 概率漂 1~3 次（≤1.5px、只发 mouseMoved），经既有 `notifyPointer`
        喂小窗光标。会话新增 `lastPointer/pointerInit/lastInputAt/clickRand`；`clickAt` 走 `humanizedClickTarget`。
     **默认开**（2026-09-22 用户拍板）；`UWS_POINTER_DRIFT=0` 只关漂移。
   - **顺带修掉的回归**：漂移经 `ObservePointer → noteAgentActivity()` 会顶住面板静默时钟 ⇒「5 分钟没动作自动收面板」永不触发。
     现在 `ViewerPointer.Idle` 标记漂移，`ObservePointer` 照样推光标但不计 Agent 活动（`pkg/viewerhub/session.go`）。
   - **验证**：`pointer_humanize_test.go` 6 例 + viewerhub 1 例；`go test ./pkg/... ./cmd/... -count=1` 17 包 ok / 0 FAIL；
     真机 e2e `TestCDPSessionHumanizedPointerEvidence` 连续两次 PASS，EVIDENCE：`落点 [703 400]（中心 [720 406.5]，
     偏离 18.20px），mousemove 46 个，过冲 2.75px，漂移 5 点`。
   - **仍未做/边界**：`dragAt`（拖拽）未动；`ClickAt(x,y)` 坐标点击不随机（坐标由调用方求解，二次随机等于篡改落点）；
    只跑过真机用例、未走引擎/协调器真实任务链路；多会话/多显示器未复验。

7. **执行完任务重复开新窗口（P1）｜✅ 已实现并真机验证（2026-09-22，TDD-ENG-28）**：
   - 现场：同一个任务每执行一次就新开一扇窗口，哪怕上一扇还开在同一个网站上、页面已经干到一半（原话：「每次执行同样的任务，它都一定是打开一个新窗口……
     哪怕是测试，我觉得都最好不要重复的打开新窗口，这占资源」）；并要求先把「什么叫同一个任务」想清楚。
   - 判据（本次定案）：**会话名**（`--session`，CLI 默认 `default`，也就是报告里的 Session ID）——它已经是既有对外契约，续页与隔离都按它走；
     同站点但不同 session 的窗口**不互相接管**，会话隔离不变。
   - 根因：内存态续页本身是好的（同 session 在同一引擎存活期内确实复用同一 Tab，已真机验证）；坏在**绑定只在内存里**，而浏览器标签页活得比引擎久
     ⇒ 每次 `daemon restart`（每次发版都算）之后同名会话只能再开一扇新窗，旧窗变孤儿。取证：跑 → `daemon restart` → 再跑，Target 从 `A1B4D6C3…`
     变成 `925A369C…`，窗口 +1。
   - 落地：绑定落盘 `~/.uws/sessions.json`（0600、原子替换、**不含 Cookie/凭据**）→ 引擎失忆时按「会话名 + 同一浏览器 + 同一 profile」接管原 Target
     （`adoptTarget`，目标不可用则回退新开）→ 接管来的窗口不重新停靠；`CloseContext` 一并删绑定；引擎新增 `--session-store`（默认开启）。
   - 验证：新增单测 15 例；`go test ./pkg/... ./cmd/... -count=1` 17 包 ok / 0 FAIL；真机 `resume-e2e`：`T1=1DB4E390…` → `daemon restart` → 再跑
     `T2=T1`、窗口数不变（8→8）⇒ 接管同一扇窗。
   - 顺带（用户批准）：关闭当时 7 扇孤儿小窗（停靠位 1496,60、各 1 标签）+ 本轮测试窗，只留用户自己的窗；已确认 AutoDL 现场 5 个标签未受影响。
   - **孤儿窗回收已定调（2026-09-22 用户）**：**不做自动回收**——「万一它留在那里，用户可能要看一看」；改成给绑定设**超时**：过期只是
     「引擎不再去接管那扇窗」（窗口照旧留在浏览器里，从此算用户的普通窗），默认 **24h**、`--session-ttl` 可配（0 = 永不过期），启动时顺手清掉过期条目。
     落地 `pkg/engine/session_store.go::SetTTL/pruneLocked/expiredLocked` + `cmd/uws-engine --session-ttl`；新增单测 4 例；真机：`--session-ttl=1s` 重启 → 启动即清空 entries、
     同 session 再跑**换新窗**（`T1=34E7E410…` → `T2=A4DA60E5…`、窗口 2→3），改回默认 24h 重启再跑 → `T3=T2`、窗口数不变（重新接管）。
   - 仍未做：`uws sessions prune`（手工清理绑定的命令）；同站点跨 session 的复用（见第 8 条）；多显示器未验。

8. **互斥任务 vs 非抢占任务（同站点跨 session 复用；P1｜✅ 已实现并真机验证，2026-09-23）**：
   - 用户口径（2026-09-22）：「会不会破坏会话隔离，这本质上没搞清楚」；**正确的判据是任务自己的属性**——「同一个任务本身就描述了这个任务
     是不是没有并发」：
     - **互斥任务（同一目标只能有一个活跃）**：例「登录某网站」——A 任务和 B 任务都是登录同一个站点，它们**互斥**，只能有一个同时活跃，
       并且**只需要一个窗口活跃**。
     - **非抢占任务**：例「打开某网站的一篇博客」与「打开同一网站的另一篇博客」——虽是同一个站点，但目标不同，**不互斥、不抢占**，各开各的窗。
   - 现状：能复用同一扇窗的只有**同一会话名**（`--session`）；跨 session 共享窗口会破坏隔离语义，故未做。
   - **2026-09-23 用户定调（「2 可以按你建议来」）**：①互斥键**由任务显式声明**（如 `--mutex-key=login:example.com`），**未声明 = 保持按会话隔离**；
     ②第二个互斥任务**排队等待**前一个结束（不是直接接管/拒绝）。
   - **实现（未开工）**：需要 ①`proto` 给 InitContext（+ CloseContext）加 `mutex_key`；②引擎 `ckeyFor` 派生出「互斥槽」并把落盘绑定按该 key 复用
     （同一互斥槽 = 同扇窗 + `BrowserContext.mu` 天然串行化动作）；③**互斥真正的难点是「前一个结束」没有信号**——窗口是持久设计、任务结束不调 CloseContext，
     所以要么引入按互斥键的**租约 + TTL**（参考现有 `cmd/uws-lease` / `fingerprint.LeaseBackend`），要么退化为「共享同窗 + 不重导航」；
     ④同槽已活跃时后来者**不得**用 `initial_url` 把前一个任务的页面导航走（这是「打架」的实际形态）。开工前先写 TDD 用例。
   - **2026-09-23 用户授权 Agent 自决（「2 你决定就好」）⇒ 定案 = 共享同窗 + 不重导航 + 动作串行**（不引租约）：
     同互斥键的任务**复用同一扇窗**（按互斥键复用已落盘绑定），后来者**不得用 `initial_url` 重导航**（前一个任务的页面原样留着），
     同 Target 的动作由 `BrowserContext.mu` 天然串行 ⇒ 这就是「排队等待」的真实形态（**动作级排队**，不会把命令挂死等一个不存在的"任务结束"信号）。
     **否掉「租约 + TTL」的理由**：任务结束没有信号，租约只能靠 TTL 猜——猜短了会把前一个任务正在用的窗夺走，猜长了后来者会僵等到超时，
     两种失败都比「共享同窗 + 动作排队」更糟；且租约要额外引一套状态与时钟语义。**未声明互斥键 = 保持现状（按会话隔离）**。
    **开工前先写 TDD 用例**（同键不同会话 → 同 Target；后来者不重导航；不同键互不影响）。
   - **2026-09-23 01:5x 落地（已 push，`8d5dabb1`）**：`proto` `InitContextRequest` 加 `optional string mutex_key = 12`（生成物同步）；
     引擎 `s.mutexSlots`（`mutexSlot{bc, owner}`，与 `s.contexts` 同锁）+ `mutexOwner`/`claimMutex`；`InitializeContext`
     **无自有上下文**且互斥键已有拥有者 → 复用拥有者上下文并**忽略 `initial_url`**；新建/接管时登记拥有者；`CloseContext`
     拥有者关闭 ⇒ 归还键 + 解绑借用者，**借用者关闭不关共享窗**。协调器 `grpc_client`/`executor`(init+recover) 透传，
     `uws run` / `uws goal` 加 `--mutex-key`。
   - **验证**：`pkg/engine/mutex_test.go` 4 例（同键共享同 Target 且不重导航 / 不同键隔离 / 借用者关闭不关窗 / 拥有者关闭归还槽位）；
     `tests/unit/test_cli_run.py` 新增透传用例；`go test ./pkg/... ./cmd/... -count=1` 18 包 ok / 0 FAIL；`pytest tests/unit -q` 156 passed。
     **真机**：`--session mxc --mutex-key login:stable` 开 example.com；`--session mxd --mutex-key login:stable` 带
     `nav:https://news.ycombinator.com` ⇒ **同一 Target 且页面仍停在 example.com**（不重导航，已用 CDP `/json/list` 直读核对）。
   - **已知边界（未做）**：①同键**并发首次创建**存在登记竞态（后登记者覆盖槽位；顺序调用不受影响）；
     ②goal 模式内层 `goto` 重新初始化未带互斥键 ⇒ **借用者显式 goto 仍会导航共享窗**（后续如需，可把 mutex_key 一并透传并明确「忽略导航」语义）。
