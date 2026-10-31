# WebTouch 测试驱动开发 (TDD) 核心检查单

> 版本：V1.1（2026-08-19 完善）
> 覆盖范围：以 V1.4 全套规格为唯一事实源——`spec/algorithm-kernel.md`、`spec/coordinator.md`、`spec/physical-patches.md`、`spec/knowledge-versioning.md`、`spec/security-dx.md`、`proto/uws_service.proto`，并吸收 `docs/` 各业务域活规范。
> 用法：本检查单是「先写测试、后写实现」的验收标准集合。每个新功能/修复必须先找到对应条目，编写失败的单元/集成测试（RED），实现至绿灯（GREEN），再重构（REFACTOR）。所有模块提交代码前，相关测试必须绿灯；CI 门禁与命令见 §0.3。

---

## 0. TDD 工作流与测试分层

### 0.1 红-绿-重构循环

- 编写任何一行功能代码前，先从本文档选取对应验收条件，编写失败的单元/集成测试；
- 实现最小代码使测试通过；
- 重构并保持绿灯；性能/资源类条目（§9）必须同时挂接基准，防止「功能过了、性能退了」。

### 0.2 测试分层与类型定义

| 测试类型 | 说明 | 典型位置 |
| --- | --- | --- |
| 单元测试 | 单模块/单函数，mock 掉 CDP、浏览器、LLM、VLM 等外部依赖 | `tests/unit/...` |
| 集成测试 | gRPC 契约、SQLite 规则库、引擎 + 探针合流 | `tests/integration/...` |
| 端到端测试 | 真实 Chromium（headless）/ Mock 站点 | `tests/e2e/...` |
| 性能/基准测试 | 延迟、内存、并发红线（§9） | `benchmarks/...` |
| 安全/模糊测试 | 注入、边界、伪造回调（§5/§7） | `tests/security/...` |

### 0.3 CI 门禁与本地命令

- `make test`：单元 + 集成测试全绿；核心模块（engine/pipeline）覆盖率门禁 ≥ 80%；
- `pytest tests/integration/test_grpc_contract.py`：gRPC 契约回归，含 proto 兼容性（TDD-API-07）；
- `make pre-commit`：lint（ruff / clang-format / mypy）+ 单测 + 契约测试全绿；
- 在线引擎变更必须附带延迟/内存回归基准（§9），未带基准或劣化超限不允许合并；
- 规则与蒸馏变更必须附带困难负样本与拓扑孤岛评估用例（对应 `contributing.md` §5）。

### 0.4 里程碑映射

| 本文档章节 | 模块 | 主要里程碑 | 测试类型 |
| --- | --- | --- | --- |
| §1 探针 | `probe_wasm` | M2 | 单元 + 性能 |
| §2 匹配/分类 | `matcher` / `classifier` | M3 / M4 | 单元 + 集成 |
| §3 引擎/状态机/热更 | `browser_agent` / `async_verifier` | M3 / M5 / M7 | 集成 + E2E |
| §4 协调器/规划 | `coordinator` | M6A / M6C / M7 | 集成 + E2E |
| §5 安全合规 | 全系统 | M4 / M7 | 安全 + 模糊 |
| §6 离线蒸馏/学习 | `pipeline` / `learning` | M2 / M3 / M5 | 单元 + 集成 |
| §7 gRPC 契约 | `proto` / 网关 / 适配层 | M1 / M6A | 契约 + 集成 |
| §8 CLI / DX | `cmd/uws` | M6A | E2E + 单元 |
| §9 性能/资源 | 全系统 | M2 / M4 / M5 / M7 | 基准 + 门禁 |

### 0.5 测试确定性与共享夹具

- 零 flaky：所有涉时/涉网用例必须受控——固定时钟、固定网络延迟、Mock CDP 会话；
- 共享夹具 `tests/fixtures/`：Mock Chromium/CDP、Mock VLM 服务、Mock 破解服务、Mock 博客站点（TDD-COORD-05）、黄金样本集（AXTree JSON + 期望候选节点）；
- 性能基线固定：延迟/内存基准在同一硬件与容器镜像下对比，基线值入库供门禁使用；
- 数据库隔离：SQLite 用例使用 `:memory:` 或独立临时库，严禁共享线上规则库。

---

## 1. 探针与物理提取层 (WASM Probe) - C++

*聚焦性能、内存边界与特征提取正确性*

> 工具链状态（2026-08-20 续13）：emcc 3.1.74 已就绪（emsdk 独立安装，
> `~/emsdk/upstream/emscripten/emcc`；沙箱内构建需 `EM_CACHE` 指向可写目录）。
> `pkg/probe_wasm` 已按 `spec/algorithm-kernel.md` §5 实现：16MB 线性内存硬边界 +
> 分级截断/溢出队列 + AST 降噪哈希（零堆分配），native 单测 + WASM standalone 构建 +
> Node 实测基准全绿；剩余 TDD-WASM-07 的「JS 慢速遍历 200ms 补偿」待 WASM 注入 V8
> 集成（引擎侧 CDP `Runtime.evaluate` 通道）。

### 1.1 性能与内存安全

- [x] **TDD-WASM-01**（P0/M2/性能）：给定一个包含 100 交互节点的标准 HTML，提取全量 AXTree 及 `NodeFeature`，执行耗时必须 `< 15ms`。（2026-08-20 点亮：WASM standalone 经 Node 实测 100 次采样 P99 = 0.63ms，见 `benchmarks/bench_probe_wasm.py` S1）
- [x] **TDD-WASM-02**（P0/M2/内存）：构造一个超大型长列表页面（序列化后 DOM 超过 16MB），探针必须安全抛出溢出事件并不发生 OOM（内存泄漏），同时 Prometheus 计数器 `uws_wasm_truncation_total` +1。（2026-08-20 点亮：native 20000 节点（≈25MB 序列化）→ 安全截断，kept>0、溢出队列=差集、无越界写；**2026-08-20 续15 接线完成**：引擎侧原子计数器 `Server.WasmTruncationTotal` + decision_trace `wasm_truncated` 标记，真实 Chrome 2 万节点页触发截断并累计；Prometheus HTTP 导出待指标依赖定案）
- [x] **TDD-WASM-03**（P0/M2/单元）：触发截断时，必须按照优先级（视口内 > 按钮/输入框 > 其他）保留前 `N` 个特征。（2026-08-20 点亮：分级准入 + 驱逐——高优先级节点可驱逐更早到达的低优先级节点，溢出队列保留 BFS 序）

### 1.2 AST 降噪哈希 (Semantic Hash)

- [x] **TDD-WASM-04**（P0/M2/单元）：给定两个功能相同但包含不同随机十六进制内存地址或动态字符串的 JS 闭包函数，`ast_semantic_hash` 的输出必须 **完全一致**。（2026-08-20 点亮：AST 降噪=剥离注释/字符串/数字/十六进制，仅保留操作符+调用点+控制关键字）
- [x] **TDD-WASM-05**（P1/M2/单元）：剥离动态属性后，能正确识别相同的 API 调用链（如都调用了 `fetch` 和 `preventDefault`）。（2026-08-20 点亮：相同调用链一致、缺失 preventDefault 则不同）
- [x] **TDD-WASM-06**（P0/M2/回归）：跨会话碰撞率：取 1000 对「同一业务逻辑、不同随机字面量」的闭包样本，降噪哈希一致率必须 > 95%（对应 M2 验收线）。（2026-08-20 点亮：1000 对一致率 100%）

### 1.3 截断降级与内存硬边界

- [x] **TDD-WASM-07**（P1/M2/集成）：Buffer 溢出后，JS 降级慢速遍历必须在 200ms 超时内补充「溢出队列中 BFS 排名前 200」的节点并合并进候选池；超时则仅使用 WASM 截断结果，且不阻塞主流程。（2026-08-20 核心已落地：溢出队列保留 BFS 序 + `uws_probe_overflow_peek` 取前 200；**2026-08-20 续15 点亮集成层**：CDP `Runtime.evaluate` 注入 standalone WASM + 页内 DOM→bulk 采集器 + 截断 200ms JS 慢速遍历补偿合并——真实 Chrome 2 万节点页 `TestCDPSessionWasmTruncation` 全绿（采集 316ms、kept=12288、overflow=7744、补偿合并 ≤200）；超时仅用截断结果不阻塞主流程）
- [x] **TDD-WASM-08**（P0/M2/内存）：当 Buffer 使用率 > 95% 时触发降级（预留 5% 余量）；16MB 线性内存为硬边界——任何写入不得越界，越界时探针受控报错，宿主进程不崩溃、不 OOM。（2026-08-20 点亮：usage>95% 置 needs_fallback；容量满后受控进溢出队列，usage 永不越界；WASM 侧 `-sINITIAL_MEMORY=16777216` + 禁止增长）
- [x] **TDD-WASM-09**（P1/M2/单元，2026-08-27 VLM 极简 AXTree 导出）：`ExportToVLMFormat`
  输出 `[node_id] tag(role) "semantic_text" [offscreen]` 逐行极简 AXTree——保留 BFS
  node_id 供 VLM 指挥官精确指认、剔除 tier>3 且无语义文本的装饰节点、视口外节点打
  `[offscreen]` 标记。（2026-08-27 点亮：`TestWasm09ExportVlmFormat` 黄金用例
  （5 节点 4 行输出 + 重复导出幂等 + cap=0/1/8/256 缓冲区边界，不越界写）；
  WASM 侧 `_uws_probe_export_vlm` 已加入 `-sEXPORTED_FUNCTIONS` 并经
  `WebAssembly.Module.exports` 实测导出）
- [x] **TDD-WASM-10**（Tier 1.5 接线，2026-08-28 结构特征位图载荷）：`PackedStructuralPayload`
  （探针 16MB 限制）——载荷恒 64B、字段偏移与 `pkg/matcher/structcodec.go` 布局一致、
  tag/role 词表条目数与 Go 对齐（30/29），跨语言对拍静态契约；`pkg/probe_wasm/test/probe_wasm_test.cc`。
- [x] **TDD-WASM-11**（Tier 1.5 接线，2026-08-28 WASM 内置结构签名 O(1) 提取）：`probe.h`
  `SerializedNode` 增 `structural_hash`（追加尾部，既有字段偏移不变、尺寸 1288B）；`probe.cc`
  `FillFromScratch` 用 tag/role/最近语义祖先 role 构造短语串（栈缓冲零堆分配）并调用
  `ast_hash.cc` 的 `GoStructuralSimHash64`（RFC1321 MD5 + md5 前 8 字节小端投票，与 Go
  `StructuralSimHash64` 逐位对齐）；JS 侧 `nearestSemAncRole` 按真实 DOM 提供祖先 role（跳过
  generic/none），`probe.go` 递增 `probeSerializedNodeSize=1288` 并解码，`buildProbeCandidates`
  优先用探针内置哈希（O(1)、截断稳健），为 0 时回退 Go 重建。黄金对拍 `TestWasm11StructuralHashGoAligned`。

---

## 2. 匹配与推理引擎 (Matcher & Classifier) - Go

*聚焦决策准确率与困难负样本识别*

### 2.1 规则命中逻辑

- [x] **TDD-MATCH-01**（P0/M4/单元）：当页面包含节点文本 "登录" 且规则库包含 `text_pattern="登录"` 时，L1 AC 自动机必须直接命中该节点入围候选池。（2026-08-19 逻辑层点亮：`pkg/matcher` AC 自动机 + SQLite 规则库）
- [x] **TDD-MATCH-02**（P0/M4/集成）：当页面包含文本 "Sign in"（不在 L1 规则内），但 `site_synonyms` 包含 TENTATIVE 同义词 "Sign in" 时，L2 BM25 应以 `0.2` 的微弱权重扰动该节点得分，将其推入 L3。（2026-08-19 点亮：`pkg/matcher` 新增 `site_synonyms` 表 + `MatchQuery` L2 召回（0.5 语义召回先验 + 0.2 扰动 = 0.7）；引擎默认种子 TENTATIVE 同义词 "Sign in"，集成层经真实引擎 gRPC 断言置信度 0.7；ACTIVE 升 1.0、POISONED 隔离见逻辑层单测）
- [x] **TDD-MATCH-03**（P0/M4/安全）：SQL 注入防护测试：在传入恶意的 `target_semantic="'; DROP TABLE uws_rules;--"` 时，必须被防注入层拦截，或使用参数化查询安全处理，数据库不抛错。（2026-08-19 逻辑层点亮：`pkg/matcher` 全查询参数化，恶意输入不破坏 `uws_rules` 表；2026-08-20 安全层点亮：`tests/security/test_input_sanitization.py` 经真实引擎 gRPC 验证注入 payload 在 API 输入边界被 `INVALID_ARGUMENT` 拦截、引擎健康不受影响）
- [x] **TDD-MATCH-04**（P1/M4/集成）：否定关键词黑名单：页面节点含 `negative_keywords`（如广告/弹窗容器）时，即使文本命中，也不得进入候选池。（2026-08-19 点亮：`pkg/matcher` 新增 `uws_negative_keywords` 表与容器级过滤（`context_hint.zonal_keyword` 或语义本身命中即拦截，返回 `ErrBlockedByNegativeKeyword`）；引擎映射为 gRPC NOT_FOUND，集成层经真实引擎验证，逻辑层单测覆盖）
- [x] **TDD-MATCH-05**（P1/M4/集成）：L3 SimHash 物理校验：仅对 Top-K 候选执行结构比对，SimHash 汉明距离 > D_h=5 的候选必须被剔除或降权。（2026-08-20 逻辑层点亮：`pkg/matcher` 新增 64-bit SimHash（`SimHash64`/`HammingDistance`/`SimHashOverlap`，与 `pipeline/cc_fetcher/topology.py` 的 `simhash64` 逐位对齐）+ `RefineByStructure`（Top-K 短名单 + D_h=5 剔除，黄金夹具单测：同词袋重排距离 0 保留、异语义文本剔除、Top-K 外不参与、topK≤0 全量）；**2026-08-20（续6）集成层点亮**：SimCDPSession 页面候选注入（`PageCandidate`）→ 引擎 runAction 经 L3 校验——结构一致候选执行、「登录广告」vs「登录」结构不一致全池剔除 → NOT_FOUND、L2 同义词候选不被文本代理误伤，`TestMatch05L3StructureRejectIntegrationGRPC` 全绿）
- [x] **TDD-MATCH-06**（P1/M4/集成）：A* 多步搜索：中间置信度区间（θ_low ≤ C < θ_high）触发 ε=0.02 探索或 A* 搜索，D_max=5 为硬边界；命中 HTTP 4XX/5XX 物理错误的节点进入 AC 黑名单（Penalty=+∞），逻辑偏移惩罚按 `C_base × [1 + (1/SimHash_Overlap)]²` 计算。（2026-08-20 逻辑层点亮：`pkg/matcher/astar.go`——`Decide` 决策带（θ_high 0.85 direct / θ_low 0.50 llm_fallback / 中间区间 ε=0.02 explore 或 astar，rng=nil 稳定 astar）；`AStarSearch` 深度钳制（D_base=3、流程标志 +2、D_max=5 硬边界）、HTTP 4XX/5XX 物理湮灭 +∞ 并入 AC 黑名单、逻辑偏移 `C_base × [1 + (1/SimHash_Overlap)]²`（overlap≤0 → +∞）、祖父回溯连坐（2 层向上有限附加惩罚）；黄金 Go 单测 8 例全绿；**2026-08-20（续6）集成层点亮**：引擎 runAction 中间置信度区间消费 `AStarSearch`——页面候选图（`PageCandidate.Children`）接根节点子图、路径末步节点文本执行（`TestMatch06AStarSearchIntegrationGRPC`：login_form→login_form_submit 执行末步）、HTTP 403 根节点物理湮灭入服务级黑名单 → NOT_FOUND，全绿；**2026-08-20（续23）真实侧点亮**：WASM 探针 kept 节点按 BFS 父子链路重建 `PageCandidate` 树（node_id + 父 BFS 索引），引擎预序遍历全树直接匹配嵌套候选 + A* 消费 Children；真实 Chrome `TestCDPSessionWasmProbeChildren` 与引擎 `TestCandidateTreeNestedDirectMatchGRPC` 全绿）

### 2.2 ONNX 困难负样本分辨

- [x] **TDD-ONNX-01**（P0/M4/集成）：给定正样本（真正需要点击的按钮）和困难负样本（它旁边的另一个外观极其相似的按钮），ONNX 模型的打分必须保证：正样本 `> 0.85`，负样本 `< 0.50`。（2026-08-19 点亮：逻辑层 `pkg/classifier` 决策核 + `Scorer` 接口（θ 边界三分）；集成层经真实引擎 gRPC 验证正样本执行且 C≥0.85、困难负样本 `NOT_FOUND`。**2026-08-20 真实侧点亮（A 案 ort-go CGO）**：`pkg/classifier/onnx.go` OnnxScorer + 5 维特征与 `train_onnx.py` 逐位对齐（golden.json 对拍 1e-4）；引擎对页面候选 Confidence=0 节点真实打分——正例 0.999 执行、非黑名单负例 NOT_FOUND；ORT 库缺失自动降级 mock/skip）
- [x] **TDD-ONNX-02**（P1/M4/性能）：每次导出新模型后回归：模型文件 < 1MB（num_leaves=31），CPU 单次推理时间处于基准基线内（初始阈值 5ms，随硬件固化）。（2026-08-20 点亮：`benchmarks/bench_classifier.py` 实跑——真实 LightGBM→ONNX 模型 11KB < 1MB，CPU 200 次推理 P50/P99 均 ≤5ms；`pipeline/exporter/train_onnx.py` 为唯一训练入口，导出内建体积上限）
- [x] **TDD-ONNX-03**（P1/M3/回归）：在黄金困难负样本回归集（兄弟节点 + SimHash 最近邻 3 干扰节点）上 AUC ≥ 0.90，且新版本不得低于上一版本（对应 M3 验收线）。（2026-08-19 点亮：逻辑层 `pkg/classifier` AUC 秩和法回归工具；集成层经真实引擎驱动 9 正 + 9 困难负黄金集，AUC=1.0 ≥ 0.90。**2026-08-20 真实侧点亮**：`train_onnx.py` 真实模型在黄金训练集上 AUC≥0.90，`tests/unit/test_train_onnx.py` 固化为回归）

---

## 3. 在线执行引擎与状态机 (WebTouch Engine) - Go

*聚焦浏览器生命周期、时序状态和拦截响应*

### 3.1 沙盒隔离与初始化

- [x] **TDD-ENG-01**（P0/M3/集成）：调用 `InitializeContext` 传入特定代理与自定义时区，启动的 Target 执行 `navigator.userAgent` 和时区探活时必须与传入值严格一致。（2026-08-20 点亮：逻辑层 `pkg/browser/browser_test.go::TestEng01InitContextProxyTimezone`（mock 会话断言 UA/时区/代理注入）；真实 CDP 层 `TestCDPSessionRealChrome` 断言 Emulation 覆盖后 `navigator.userAgent` 与时区探活一致）
- [x] **TDD-ENG-02**（P0/M3/集成）：创建 5 个并发 `BrowserContext` 并登录不同测试账号，验证互相之间 Cookie 不穿透、缓存物理隔离。（2026-08-20 点亮：逻辑层 `TestEng02ConcurrentContextIsolation`——5 并发 Context 各自注入 Cookie 互不穿透；真实 CDP 多 Context 隔离待部署环境补 E2E，机制已由启动模式独立 user-data-dir 保证）
- [x] **TDD-ENG-08**（P0/M6A/E2E）：attach 模式：传入 `attach_cdp_url` 接管本地运行的 Chrome，必须新建标签页（不破坏现有标签页），共享当前登录态；任务结束后引擎不崩溃、原会话不受影响。（2026-08-20 点亮：`pkg/browser` 真实 CDP 会话附着模式——`Target.createTarget` 新建标签页、`attachToTarget` 扁平会话；Go 集成 `TestCDPSessionAttachMode`（双会话独立 target、Close 不终止外部 Chrome）+ Python E2E `tests/e2e/test_engine_attach.py::test_tdd_eng_08_attach_cdp_url`（`UWS_E2E_CDP=1` 门控：原 target 保留、点击真实生效））
- [x] **TDD-ENG-09**（P0/M3/集成）：cookies 注入：`InitializeContext` 预置 Cookie 后，目标页面的请求/`document.cookie` 必须携带对应会话；与 TDD-ENG-02 组合验证「注入生效 + 并发隔离」。（2026-08-20 点亮：逻辑层 `TestEng09CookiesInjection`——双 Context 预置 Cookie 注入生效且互不泄漏；真实 CDP `Network.setCookie` 已接线，真实站点请求携带验证待 E2E）
- [x] **TDD-ENG-10**（P1/M3/集成）：Target 池配额：并发请求超过 `CPU_Cores × 1.5` 时新请求进入 FIFO 队列，30s 超时后返回 `RESOURCE_EXHAUSTED`；任务结束后 Target 随 BrowserContext 销毁，池大小回落。（2026-08-20 点亮：逻辑层 `TestEng10TargetPoolQuota`——容量满 → `RESOURCE_EXHAUSTED` + 超时等待；FIFO 队首先得槽位；释放后池大小回落。配额上限按部署 `CPU_Cores × 1.5` 配置，机制可测）
- [x] **TDD-ENG-11**（P1/M3/集成）：生命周期：Target 达到 500 次 Navigation 或 4 小时后必须优雅重启（排空队列超时 30s），已确认的动作结果不丢失。（2026-08-20 点亮：逻辑层 `TestEng11TargetLifecycleRestart`——4h 老化 / 500 次导航触发重启、导航计数重置、已确认结果不丢失、排空超时）

### 3.2 物理执行与防御

- [x] **TDD-ENG-03**（P0/M4/集成）：对任意目标执行 `click` 动作，CDP 下发的鼠标轨迹不得为直线，需生成服从 Fitts' Law 的三次贝塞尔曲线。（2026-08-20 点亮：`pkg/browser` `newBezierPath` 纯函数——Fitts 时长 + 随机垂直偏移三次贝塞尔，Go 单测 `TestBezierPathNonCollinear` 断言非直线/确定性；真实 CDP 集成 `TestCDPSessionRealChrome` 点击真实生效。可选增强：CDP 级鼠标事件采样断言，非验收必需）
- [x] **TDD-ENG-04**（P0/M4/集成）：目标按钮被一个透明弹窗（`z-index` 更大）完全遮挡。执行点击时，引擎必须判定被遮挡，触发平移或抛出 `ABORTED`，绝不允许穿透点击。（2026-08-20 点亮：`pkg/browser` `checkOcclusion`（`elementFromPoint` + `data-uws-target` 标记），Go 集成 `TestCDPSessionOcclusion` 全屏遮挡 → `occluded` → 引擎 ABORTED）
- [x] **TDD-ENG-05**（P0/M5/集成）：当访问触发 HTTP 403 且包含 Turnstile 特征的页面时，引擎不可死锁，必须立即返回状态码 `CHALLENGE_INTERCEPTED`。（2026-08-20 点亮：`cdpSession` 检测主文档 403+挑战头 → `challenge_intercepted`，引擎映射 `UNAUTHENTICATED`（协调器侧 CHALLENGE_INTERCEPTED 语义）。待补：真实 Turnstile 页 E2E（外部站点，后置））
  - **2026-09-23 修复（误报）**：`UNAUTHENTICATED`/`CHALLENGE_INTERCEPTED` **只能由真实挑战信号触发**（判据 `ChallengeSignal.IsHumanChallenge()`）；防乱点熔断自产信号（连续失败 `Anti-thrashing`、人工拉起 `CircuitBreaker`）**不得**计入人机验证，改走 `RESOURCE_EXHAUSTED`（协调器 PAUSE 且透传引擎原话）。复现与证据见 GLOBAL #120。
- [ ] **TDD-ENG-12**（P1/M7/E2E）：基础反检测：`--attach`/有头模式下通过 `https://bot.sannysoft.com/` 基础无头检测（`navigator.webdriver`、Chrome 对象、UA 一致性）。
- [x] **TDD-ENG-18**（P1/M7/集成，2026-08-27 指纹修复）：未显式指定 UA 时引擎
  **不再覆盖**为 "UWS/1.4 Engine"——附着模式保留真实浏览器指纹。反爬实证：百度/smzdm
  对 UA 异常的引擎 Tab 弹验证码，同一 Chrome 普通 Tab 不弹（dom_probe 显示引擎 Tab
  `navigator.userAgent="UWS/1.4 Engine"`、普通 Tab 为真实 Chrome UA）；去掉默认覆盖后
  smzdm 不再拦截。测试：`pkg/engine/evidence_test.go::TestInitializeContextNoUAOverrideByDefault`
  （捕获会话 Config，未指定 UA 时 UserAgent 必须为空）。
- [x] **TDD-ENG-17**（P1/M7/集成，v1.5.4 判定层证据链第一切片）：
  `ExecuteAction` 响应回显实际命中元素（`hit_element`：selector/rect/text，点击点
  `elementFromPoint` 真身为准）+ 动作前后 URL 证据（`page_url` / `url_changed`），
  让调用方识破「点击命中容器却报成功」假阳性（drama-h5 §3.2 / skincare 复测）。
  契约：`ActionResultResponse` 追加 `hit_element=10 / page_url=11 / url_changed=12`
  + 新 `HitElement` 消息（append-only v1.5.4）；浏览器层 `elementTarget.Sel` +
  `HitElementSource`（`LastHitElement`，语义定位/VLM ClickAt 均记录）+
  `PageURLSource`；引擎 `finishAction` 统一组装证据（成功与 VLM 兜底成功共用）。
  测试：`pkg/engine/evidence_test.go` 3 例（导航点击回显+url_changed /
  假阳性点击「success+event_triggered 但 URL 未变+命中容器」/ extract 回显命中元素）
  + 契约层 `test_tdd_api_11_evidence_hit_element_echo`（sim 合成证据）。

### 3.3 SPA 软路由与时序 (最核心机制)

- [x] **TDD-ENG-06**（P0/M5/集成）：发送一个点击动作触发 React Router 的 `pushState`，引擎必须立刻将状态变更为 `NAVIGATING`。（2026-08-19 实现层点亮：引擎 ExecuteAction 接入 verifier 状态机——动作后捕获 pushState 立即返回 `NAVIGATING`、绝不立即判完成；模拟 CDP 会话可脚本化事件，见 `pkg/engine/m5_integration_test.go`；契约层实时事件流待真实 CDP 适配）
- [x] **TDD-ENG-07**（P0/M5/集成）：在触发 `NAVIGATING` 后，若仍有未完结的 XHR/Fetch 请求，引擎不得转为稳定态。只有当活动连接 `<2` 持续 500ms 且无新增 DOM 突变时，才释放并返回 `NAVIGATED`。（2026-08-19 实现层点亮：进入下一动作时做 ≤`timeout_ms` 稳定性等待——Network Idle 500ms → `NAVIGATED`；忙碌连接/等待超时 → gRPC `DEADLINE_EXCEEDED`（协调器退避重试）；DOM 突变重置静息已接线）
- [x] **TDD-ENG-13**（P1/M5/集成）：点击 `target="_blank"` 链接后，引擎返回 `NEW_WINDOW_OPENED`(3) 并保留原 Target 可用；协调器可 Attach 新 Target 继续执行。（2026-08-20 点亮：`pkg/browser` 新增 `Target.setDiscoverTargets` + `Target.targetCreated` 事件源（`browser.WindowEvents` 一次性读清）；引擎 ExecuteAction 映射 `ContextState_NEW_WINDOW_OPENED`；物理输入前 `Target.activateTarget` 前台化（修复后台标签 `Input.dispatchMouseEvent` 挂起超时）；验证：引擎映射 `TestEng13NewWindowOpenedMapping`（sim）+ 真实 Chrome Go 集成 `TestCDPSessionNewWindowOpened`（新窗口捕获 + 原 target 点击生效）+ Python E2E `test_tdd_eng_13_new_window_opened`（`UWS_E2E_CDP=1`，启动模式全链路）。遗留：协调器真实 Attach 新 Target 需 v1.5 RPC/CLI 定案（TDD-COORD-09 真实侧））
- [x] **TDD-ENG-21**（P0/2026-09-22 用户现场反馈批次 #1/引擎+协调器）：**新窗口接管**——动作打开新窗口/新标签页后，引擎默认把会话作用面前移到新页（`browser.TargetSwitcher.SwitchTarget`：attach 新 target + 重开 CDP 域 + 重放身份/视口/探针 + 清页面时序 + 旧 session 事件摘除与新 session 重挂 + 观看流迁移 + detach 旧 session），**原 Target 不关、全程不抢前台（守 R1）**；契约新增 `new_target_id`/`active_target_id`（相等=已接管），协调器前移钉扎 Target 并继续任务；`context_hint.follow_new_target=0` 可关，关闭/接管失败时保持 v1.5.5 暂停语义。（2026-09-22 点亮：`pkg/engine` sim 3 例 `TestEng21*`；真实 Chrome `pkg/browser` `TestCDPSessionSwitchTarget`（`UWS_RUN_CHROME_E2E=1` 实测 PASS：新页可读可点 + 原页保留 + 可切回）；coordinator `tests/integration/test_coordinator.py::test_tdd_coord_09b/09c`；`go test ./pkg/...` 全绿）
- [x] **TDD-ENG-22**（P0/2026-09-22 用户现场反馈批次 #2/观看壳）：**屏幕上只留一块小窗**——默认全局单小窗，跟随最近**有动作**的会话自动切换；只有帧没有动作的会话抢不走面板（否则多 Agent 并发时画面来回翻）；级联槽位（`ViewerSessionRegistry`）与对应断言退役；面板位置就是用户拖到哪儿是哪儿。（2026-09-22 点亮：壳侧 `ViewerPanelRouter` 纯逻辑 9 例 `ViewerPanelRouterTests` 全绿；`swift test` 74 例 0 失败）
- [x] **TDD-ENG-23**（P0/2026-09-22 用户现场反馈批次 #2/壳+引擎）：**✕ 是真关**——壳侧真正丢弃面板与会话绑定（不再只是隐藏态），并向引擎发一条 `close` 意图（协议 v1 新增 `intent:"close"`），引擎把这个会话立刻静默回收（`viewerhub.Session.GoQuiet`：停推流 + 一条 `sessionClosed`）。**不**结束会话、**不**关真窗口、**不**搬动任何窗口；关掉之后画面照流但不再弹回来，直到**又有新动作**。（2026-09-22 点亮：Go `TestDecodeIntentAcceptsTheShellsCloseLine`/`TestApplyWindowIntentCloseNeedsNoMovableWindow`/`TestApplyWindowIntentCloseForUnknownSessionAnswersNotOk`/`viewerhub.TestCloseGoesQuietImmediatelyEvenWhileTheAgentIsBusy`/`engine.TestViewerCloseIntentQuietsTheSessionWithoutEndingIt`；壳侧 `ViewerPanelRouter` 的关闭断言 + `ViewerSocketIntent.Name.close`）
- [x] **TDD-ENG-24**（P0/2026-09-22 用户现场反馈批次 #2c/观看壳）：**展开超时只回落、不撤销**——壳 `expansionTimeout` 由 1.2s 提到 5s（与引擎 `viewerIntentTimeout` 同量级：一次展开是 5~6 次 CDP 往返）；超时后进入「已回落」态并**不**补发 `collapse`，迟到的 `ok=true` 把自绘放大面板**升级**成真页面；只有「用户在半路真的不要了」才替他把已经搬大的真窗口放回去。（2026-09-22 点亮：壳侧 `ViewerExpansionMachine` 纯逻辑 12 例 `ViewerExpansionMachineTests` 全绿；`expandedCanvas.onCollapseRequested` 改走上报宿主，避免「用户已收回、迟到回执又弹开」）
- [x] **TDD-ENG-25**（P0/2026-09-22 用户现场反馈批次 #2/引擎）：**附着会话的有头/无头双向判定**——引擎启动默认 `Headless: true`，附着模式只在检测到 `HeadlessChrome` 时把会话升级为有头、**从不回落** ⇒ 附到用户真 Chrome 的会话仍被当无头 ⇒ `windowPolicy()` 退化成 `tab`、展开直接 `ErrNoViewerWindow`（现场症状就是「点开只是一张点不动、滚不动的图」）。修法是 `browserHeadlessState` 三态 + 附着分支双向判定。**测试通过而现场不成立**的差别在配置同款：既有真机用例用零值 `CDPFactory{}`（`Headless=false`），只有守护进程带 `Headless=true`——`attach_headed_e2e_test.go` 专走引擎同款配置，不得改成零值工厂。（2026-09-22 点亮：`pkg/browser` 单测 + 真机用例；现场复验 `viewer.log` `intent result: intent=expand session=ctx-2 ok=true`，该会话真窗口上屏、点击期间 `frontmost` 保持 ChatGPT（R1 未被自动化抢）；提交 `934f60e5`）
- [x] **TDD-ENG-26**（P0/2026-09-22 用户现场反馈批次 #2/壳+引擎）：**展开 = 把真窗口抬到最前（不搬位置、不缩放）+ 小窗飞走让位 + 互斥不变量**——引擎 `RaiseViewerWindow` 只做「必要时救援进屏内 + 激活」，回执带真实矩形；壳拿它当「飞向它」的动画落点，演完 `orderOut`。**互斥不变量：小窗直播 ⟺ 真窗口不在前台**（0.4s 前台轮询 `ViewerPanelPresence`，AppKit 无关可单测；引擎拉起的窗口按 `browserPID` 认、附着模式按应用类别认），因此「收回胶囊」`ViewerCapsule.swift` 与其 placement 测试整体退役；壳侧状态机删掉 `.live`/`.collapsing` 与 collapse 回执。配套把「相交即可见」换成「可见面积 ≥ 50%」(`visibleEnoughForViewer`)，40px 夹取边不再算看得见。协议放宽：**expand 不再要求 `size`**（真机复验里正是这条校验把不带 size 的新壳意图当畸形输入静默丢弃 ⇒ 点开只有回落面板、连回执都没有）。（2026-09-22 点亮：Go `pkg/browser`/`pkg/viewerhub` 全绿 + 壳 `swift test` 68 例 0 失败；真机复验 19:40/19:41：`intent result ok=true`、`browser: 真窗口停在屏外…已搬进屏内 {24,49}`、`观看壳抬到最前`、小窗让位、切到访达后 `panel shown: 浏览器已让开`；动画中途被前台轮询 `orderOut` 的时序毛刺一并修掉）
- [x] **TDD-ENG-27**（P2/2026-09-22 用户现场反馈批次 #6/引擎）：**指针轨迹与落点拟人化**——现状是三点硬机器特征：语义点击的落点固定在元素包围盒**正中心**、物理移动一次到位（CDP 单跳、精确刹车）、静默期指针一动不动（HumanPacer 只管频控、不管轨迹）。落地：①落点 `clickLandingPoint` 在元素矩形内**随机采样**（边距 min(18%, 12px) + 高斯 σ=0.45×可用半径；偏移离正中 <0.5px 时沿随机方向推开 ⇒ 永不正中；宽或高 <2px、或 rng 为空时退回中心——随机化是「更像人」不是「更准」，拿不到几何宁可走老办法）；②`overshootPath` 在轨迹末尾沿末段方向追加「过冲峰（≤12%、上限 6px）→ 回拉 → 落点」，末段 <8px 不过冲；③`pointerStepDelay` 每步 4~13ms 抖动——**「多步非匀速」在真机上看得见的前提就是步间隔**，同一时刻到达的 `Input.dispatchMouseEvent` 会被 Chrome 合并成几跳，代价是单次点击 +0.04~1s（10~80 步）；④`startPointerDrift` 在会话 `Create` 尾部起 ticker，静默 ≥3s 后每 1.5s 按 0.35 概率插入 1~3 次 ≤1.5px 的 `mouseMoved`（不按下、不改页面语义），经既有 `notifyPointer` 喂观看小窗光标（批次 #6 第 4 条由此满足，无需另造轨迹）。**边界**：只作用于**语义点击** `clickAt(ctx, elementTarget)`；`ClickAt(x,y)`（VLM 坐标兜底）与 `dragAt` 的坐标由调用方求解，**不做二次随机/过冲**，否则等于篡改调用方落点。默认开（2026-09-22 用户拍板），`UWS_POINTER_DRIFT=0` 只关漂移。**顺带修掉的回归**：漂移会经 `ObservePointer → noteAgentActivity()` 一直顶住面板静默时钟 ⇒ #110 的「5 分钟无动作自动收面板」永远不会触发；现在 `ViewerPointer.Idle` 标记漂移，`viewerhub.Session.ObservePointer` 照样推光标、但**不计 Agent 活动**。（2026-09-22 点亮：`go test ./pkg/... ./cmd/... -count=1` **17 包 ok / 0 FAIL**（新增 `pointer_humanize_test.go` 6 例 + viewerhub 「idle 指针不重置静默时钟」1 例）；真机 e2e `TestCDPSessionHumanizedPointerEvidence`（真 Chrome，本机 9222）**连续两次 PASS**，EVIDENCE：`落点 [703 400]（中心 [720 406.5]，偏离 18.20px），mousemove 46 个，过冲 2.75px，漂移 5 点`；真机检测坑：坐标被取整到像素且过冲点前一点常就是落点本身 ⇒ 过冲检测要回看 4 个采样点当基线）
- [x] **TDD-ENG-28**（P1/2026-09-22 用户现场反馈 #7/引擎）：**会话续页跨引擎重启——同一个任务不再重复开新窗口**——现场症状：同一个任务每执行一次就新开一扇窗口，哪怕上一扇还开在同一个网站上、页面甚至已经干到一半；同类任务跑几次就堆几扇窗。根因**不是**「没做复用」——内存态续页（v1.5.6）是好的：同一个 `--session` 在同一引擎存活期内确实复用同一 Tab（真机验证过）；真正的原因是**会话→Target 绑定只活在内存里**：`s.contexts` 随引擎进程一起消失，而浏览器的标签页活得比引擎久 ⇒ 每次 `daemon restart`（每次发版都算）之后，同名会话只能再开一扇新窗，旧窗变成没人认领的孤儿（真机取证：跑任务 → `daemon restart` → 同 session 再跑，Target 由 `A1B4D6C3…` 变成 `925A369C…`，窗口数 +1）。落地：①引擎把绑定落盘到 `~/.uws/sessions.json`（0600；临时文件 + rename 原子替换；**只记「谁接谁的活」，不含任何 Cookie/凭据**；字段 `session/target_id/url/attach/profile_id/own_window/updated_at`）；②`InitializeContext` 在内存里没有活跃上下文时先查绑定，命中且 **attach 端点与 profile 都一致**（换了浏览器，targetId 就不再指向同一扇窗）才把 `cfg.ReuseTargetID/ReuseOwnWindow` 交给 `browser` 层；③`cdpSession.adoptTarget` 校验目标仍是 `page` 且非空壳后 `Target.attachToTarget` 接管，**不可用（被关/Chrome 重启过）一律回退为新开**、不报错、不阻塞任务；④接管来的窗口**不重新停靠**（用户可能已经把它挪到顺手的位置），只有新开窗才走停靠策略；⑤`CloseContext` 一并删除绑定，否则下次同名会话会去接管一扇已经关掉的窗。**边界**：只对命名会话（`--session`，CLI 默认 `default`）生效——匿名上下文按连接寻址，跨引擎进程没有意义；同站点但不同 session 的窗口**不会**互相接管，会话隔离语义不变；`--attach` 换成另一个浏览器时绑定自动失效。（2026-09-22 点亮：新增 `pkg/engine/session_store.go` + 单测 5 例（落盘往返/重开可读/0600 权限/坏文件备份为 `.corrupt` 且不阻塞启动/nil 存储安全）与 `pkg/engine/session_resume_test.go` 6 例（初始化即落盘、CloseContext 删除、重启后接管落盘 Target、换浏览器不复用、未配置存储保持旧行为、落盘字段名）+ `pkg/browser/session_resume_test.go` 4 例（接管已存在页、空壳/不存在一律拒绝、`initTarget` 接管并记回 ownWindow、目标失效回退新开）；`go test ./pkg/... ./cmd/... -count=1` **17 包 ok / 0 FAIL**；**真机**：`uws run --session resume-e2e` → `T1=1DB4E390FDE7095DA3D6EFC593B4D462`、1440x900 小窗 7→8 → `uws daemon restart` → 同 session 再跑 → `T2=T1` 且窗口仍为 8 ⇒ **接管同一扇窗，不再开新窗**）
  **续批（同日 21:24 · 绑定有效期）**：用户现场定调「孤儿窗自动回收没必要——万一它留在那里，用户可能还要看一看；可以设一个超时时间」⇒ **不关窗**，
  超时收缩的是**接管资格**：`SessionStore.SetTTL`（启动即 `pruneLocked` 清掉过期条目）、`Lookup` 对过期绑定视为不存在，默认 `DefaultSessionTTL = 24h`、
  引擎 `--session-ttl` 可配（0 = 永不过期）。`SessionStore` 全程没有关窗能力——过期只是「引擎不再去接管那扇窗」，窗口留给用户。
  新增单测 4 例（过期绑定不可续页 / 启动清理不留残留 / TTL=0 永不过期 / 引擎侧过期不接管）；`go test ./pkg/... ./cmd/... -count=1` **17 包 ok / 0 FAIL**；
  **真机**：`UWS_ENGINE_ARGS="--session-ttl=1s"` 重启 → 启动即清空 entries、同 session 再跑**换新窗**（`T1=34E7E410…`→`T2=A4DA60E5…`，窗口 2→3）；
  改回默认 24h 重启再跑 → `T3=T2`、窗口数不变（重新接管）。

- [x] **TDD-ENG-29**（P1/2026-09-23 用户现场反馈「vault 安全的话就用 age」/CLI+存储）：**本地凭据库 `uws vault`——age 加密、只落本机、明文只有一个出口**——落地：`pkg/vault/`（`Init/Open/Set/Get/Delete/List/Fingerprint`）+
  `cmd/uws/vault.go`（`init|set|get|list|rm`）。①密文 `~/.uws/vault.age`（0600、age X25519 + ASCII armor、整库一次性加密 ⇒ **连条目名都不泄露**）；
  ②identity `~/.uws/vault.key`（0600、`AGE-SECRET-KEY-1…`）；③写盘临时文件 + rename 原子替换（失败不留半截库）；
  ④**fail-closed**：缺文件/坏密文/identity 不匹配一律报错，绝不退化成空库或明文；⑤拒绝空名/空值。CLI 口径：`set` 走 `stty -echo` 无回显、值不进 argv/history，
  管道/`--stdin` 供脚本，**只回 `sha256[:12]` 指纹**；`get` 是唯一明文出口（`--fingerprint` 只回指纹）；`list` 只给名字/指纹/时间；路径 `UWS_VAULT_KEY`/`UWS_VAULT_FILE` 可覆盖；
  **不联网、不做对外服务**。**边界（诚实）**：identity 与密文同目录 ⇒ 防误传/误提交/备份泄露/明文 grep，**不防本机已被读取**；硬化下一步 = identity 进系统钥匙串。
  （2026-09-23 点亮：`pkg/vault/vault_test.go` 8 例（往返重开 / 密文不含明文与条目名 / 错 identity fail-closed / 未初始化与拒绝覆盖与 `--force` 重建 /
  list 隐藏明文 + 删除落盘 / 拒空值空名 / 密文损坏 fail-closed / 注入时钟）；`go test ./pkg/... ./cmd/... -count=1` **18 包 ok / 0 FAIL**；
  **真机**（临时库）：`init` → `set`（管道）→ `get --fingerprint` ≡ 写入侧指纹 → `get` 明文 → `list` 无明文 → `rm` → 空库；两文件均 0600、`grep` 密文无明文。
  **未做**：钥匙串存放 identity；给 `bin/autodl_run.py` 等的非交互取值通道（`--exec`/环境注入）；轮换/审计；用户真实库尚未写入条目（首条 = AutoDL 密码，须用户本人输入）。）

- [x] **TDD-ENG-30**（P1/2026-09-23 用户授权自决（「2 你决定就好」）/proto+引擎+协调器）：**互斥任务 = 共享同窗 + 不重导航 + 动作串行**——同互斥键的任务复用**同一扇窗**，后来者不得用 `initial_url` 把前一个任务的页面导航走；「排队等待」的真实形态是**动作级排队**（同 Target 的动作经 `BrowserContext.mu` 天然串行），**不引入租约/TTL**（任务结束没有信号，租约只能靠 TTL 猜：猜短了夺走前一个正在用的窗、猜长了后来者僵等，两败）；**未声明互斥键 = 保持按会话隔离**。
  落地：`proto` `InitContextRequest` 加 `optional string mutex_key = 12`（生成物同步）；引擎 `s.mutexSlots`（`mutexSlot{bc, owner}`，与 `s.contexts` 同锁）+ `mutexOwner`/`claimMutex`（先到先得）；`InitializeContext` 分支序 = **自有上下文优先**（同 session 续页照旧，含显式 `initial_url` 原地导航）→ **无自有且键已有主** ⇒ 复用拥有者上下文并**忽略 `initial_url`** → 否则新建/接管并登记为拥有者；`CloseContext` 拥有者关闭 ⇒ 归还键 + 解绑借用者，**借用者关闭不关共享窗**。协调器 `grpc_client`/`executor`(init + crash recover) 透传；`uws run` / `uws goal` 加 `--mutex-key`。
  **判据/语义**见 `USER_ACTION.md` 第 8 条与 `GLOBAL.md` #121。**验证**：`pkg/engine/mutex_test.go` 4 例（同键共享同 Target 且不重导航 / 不同键隔离 / 借用者关闭不关窗 / 拥有者关闭归还槽位）+ `tests/unit/test_cli_run.py` 透传用例；`go test ./pkg/... ./cmd/... -count=1` **18 包 ok / 0 FAIL**、`pytest tests/unit -q` **156 passed**；**真机**：`--session mxc --mutex-key login:stable` 开 example.com → `--session mxd` 同键带 `nav:https://news.ycombinator.com` ⇒ **同一 Target 且页面仍停在 example.com**（CDP `/json/list` 直读核对）。
  **边界（诚实）**：①同键**并发首次创建**存在登记竞态（后登记者覆盖槽位，顺序调用不受影响）；②goal 模式内层 `goto` 重初始化未带互斥键 ⇒ 借用者显式 `goto` 仍会导航共享窗。）

- [x] **TDD-ENG-31**（P1/2026-09-23 用户授权自决（AutoDL 密码线前置「你自己操作，我不需要记这个密码」）/引擎+协调器+proto）：**凭据直填 `@vault:<名>`——引擎侧解引用、fail-closed 暂停**——值写成 `@vault:<名>` 时由**引擎**在本机解引用：明文不进 `--plan` argv、不进协调器、不过 gRPC、不进日志（快照一律 `[REDACTED]`）；`evaluate` 是 JS 源码通道，不参与解引用；取不到凭据即报错停下、**绝不把引用串原样填进页面**。
  落地：`pkg/vault` 新增 `ReferencePrefix`/`ReferenceName`/`DefaultDir`/`ResolvePaths`（路径口径统一，`cmd/uws/vault.go::vaultPaths` 改为委托它）；引擎 `Options.VaultResolve` + `Server.vaultResolve` + `resolveVaultRef`（插在 `execute()` 最前）。
  **失败语义（真机发现并修正）**：凭据缺失复用 `FAILED_PRECONDITION` 会被协调器当「Target 崩溃」做 REINIT（白做一次上下文重建 + 报告只显示 `reinit_exhausted`）⇒ 引擎消息加稳定前缀 `vault_unresolved: `，反射器按前缀 **PAUSE 透传引擎原话**（与 RESOURCE_EXHAUSTED 同口径）；契约入 `proto/uws_service.proto` 错误码表 FAILED_PRECONDITION 行。
  **判据/语义**见 `GLOBAL.md` #122 与 `USER_ACTION.md` 第 5 条。**验证**：`pkg/engine/vault_fill_test.go` 3 例（解引用后填真值且引用串不落页面 / 缺凭据 fail-closed 且不填任何东西 / 非引用与 `evaluate` 不触发解析）+ `pkg/vault/vault_test.go` 2 例 + `tests/unit/test_reflector.py` 1 例；`go test ./pkg/... ./cmd/... -count=1` **18 包 ok / 0 FAIL**、`pytest tests/unit -q` **157 passed**；**真机**：`input:搜索实例名称=@vault:filltest` ⇒ `SUCCEEDED` 且页面 AX `[value=hello-from-vault]`；`@vault:nothere` ⇒ `PAUSED` + 原因 `vault_unresolved: 凭据直填失败 @vault:nothere: …`，页面值未被改写。
  **坑（诚实）**：目标语义含 **ASCII `/`**（如 `搜索实例名称/ID`）会被 `sanitize.go::semanticRe` 白名单拒 ⇒ `INVALID_ARGUMENT`，与凭据通道无关；**不改白名单**（安全边界，`node:` 引用可绕开），改用不含 `/` 的合法语义即可（匹配是子串口径）。

- [x] **TDD-ENG-14**（P1/M7/集成，方案 1 SSE 网络层捕获）：页面 fetch
  `text/event-stream` 聊天补全端点（如 `/api/v0/chat/completion`），CDP Network 层
  必须捕获 delta 增量并解析出干净 content（无 DOM 引用角标碎片），
  `loadingFinished` 后快照 `Done`；多轮新请求 `Epoch` +1 重置；引擎 `extract` 经
  `context_hint["prefer_sse"]="1"` 优先返回 SSE 快照（Epoch>0 且有内容 → 干净
  文本 + `chat_stream_done`；捕获轮失败/空响应收尾 → 回退 DOM observed；捕获
  进行中但内容为空（深度思考）→ 返回空串等 SSE）。
  （2026-08-25 点亮：SSE 解析器/chatStream 状态机单测 + 伪 CDP 事件注入
  `TestCDPSessionSSECaptureWiring` + 真实 Chrome 本地 SSE
  `TestCDPSessionSSECaptureRealChrome`（沙箱自动 skip）+ 引擎
  `TestExtractPrefersChatSSESnapshot` 4 例全绿；API `stream:true` 见 TDD-CLI-04）
  **2026-08-25 深夜续（真实回归修复）**：DeepSeek Web SSE 升级为补丁式协议——
  初始快照 `{"v":{"response":{"fragments":[{"id":2,"type":"RESPONSE","content":"```"}]}}}`
  + 首个显式补丁 `{"p":"response/fragments/-1/content","o":"APPEND","v":"mark"}`
  确立路径 + **后续隐式增量 `{"v":"down"}`**（省略 p/o，沿上一路径继续 APPEND）+
  收尾 `{"p":"response/status","o":"SET","v":"FINISHED"}` / BATCH `quasi_status`；
  `sseSink` 新增 seedFragments（按 type 分流 content/reasoning）/显式 APPEND/
  隐式增量/收尾解析，旧 text_delta 形态保持兼容；`ActionResultResponse` 追加
  `chat_stream_done`（v1.5.2 append-only），extract(prefer_sse) 带出 Done 供 API
  判定完成（生成中途停顿 >1.5s 不再把半截当完成）。测试新增：补丁快照/隐式增量/
  多片段 thinking+response/逐字节截断 + `chat_stream_done` 断言。
  **2026-08-26（实测回归修复）**：失败/空响应捕获轮不再霸占 extract——chatStream
  新增 `Failed` 标记（onFail 置位/onRequestStart 清零），引擎 extract 决策规则
  改为：快照有内容 → SSE 干净文本 + done；Failed 或 Done 且空内容 → 回退 DOM
  observed；进行中且空内容 → 空串等 SSE。当前 Gateway 预算耗尽且无完成证据即失败，
  不再将未完成 DOM / SSE 文本兜底为成功（见 gateway-contract 提交边界及回归）。
  测试：`TestExtractPrefersChatSSESnapshot` 决策规则 3 例 +
  `TestChatStreamFailMarksFailedAndResets` + API 2 例全绿。
- [x] **TDD-ENG-15**（P1/M7/集成，2026-08-25 用户实测反馈）：Gemini 页面加载慢时
  点击不得「点歪」后无法继续——物理层必须具备「定位稳定 → 点击 → 语义自检 →
  点歪副作用恢复 → 重试」闭环；标签页被手动关闭后 `ExecuteAction` 必须显式返回
  `target_destroyed`（供 API 自动重建新 Tab）。（2026-08-25 点亮：
  `pkg/browser/click_recovery.go`——`locateStableElement`（可见+可交互+几何两次采样
  <3px 稳定）/`detectStrayUI`/`recoverStrayUI`（录音→再点开关停录，菜单/弹层→Escape）/
  `verifyGeminiClick`（发送=输入清空、新对话=回答区重置、输入=落盘非空）；`Target
  .targetDestroyed` 事件 → `target_destroyed`；纯逻辑单测 `TestNeedSendEnabled`/
  `TestIsGeminiClick` + 真实 Chrome `TestCDPSessionTargetDestroyed`（沙箱自动 skip））
- [x] **TDD-ENG-16**（P1/M7/集成，2026-08-25 用户实测 DOM）：DeepSeek 网页
  三模式（快速/专家/识图）+ 深度思考/智能搜索开关物理切换——`div[role="radio"]`
  单选组按文本定位点击并校验 `aria-checked`；`div.ds-toggle-button` 开关读
  `ds-toggle-button--selected` 态、与目标不一致才点击并校验；专家模式无智能搜索
  开关时显式报错。（2026-08-25 点亮：`pkg/browser/deepseek_mode.go`——
  `switchDeepSeekMode`/`toggleDeepSeekSwitch` + `cdp_session.go` 接
  `deepseek_mode_switch`/`deepseek_thinking_switch`/`deepseek_search_switch`；
  纯函数单测 `TestPickDeepSeekMode`/`TestDeepSeekModeNameEqual` + 本地测试页
  真实 Chrome `TestDeepSeekModeToggleRealChrome`（沙箱自动 skip））
- [x] **TDD-ENG-17**（P1/M7/集成，**2026-09-03 附着视口跟随窗口**）：附着模式（
  AttachCDPURL）未显式指定 viewport 时不得再 setDeviceMetricsOverride 到
  1920x1080——先 `Emulation.clearDeviceMetricsOverride` 清除残留覆盖，再读实际
  `window.innerWidth/innerHeight`（兜底 outer）写入 cfg.Viewport，页面宽度=窗口宽度
  （修复“页面比窗口宽且无横向滚动条”，USER_ACTION #27）；显式视口（移动模拟等
  任务级需求）在附着模式仍按配置覆盖；headless 启动模式维持默认 1920x1080。
  实现：`browser.Config.ViewportExplicit` + `pkg/engine/service.go` 标记 +
  `pkg/browser/cdp_session.go` `applyViewportMetrics`/`adoptWindowViewport`。
  验收：`TestCDPSessionAttachMode` 真实 Chrome 断言默认 1280x900 窗口跟随 +
  显式 390x844 覆盖生效。
- [x] **TDD-ENG-18**（P1/M6C/单元，**2026-09-03 同名会话复用（续页）**）：引擎
  InitializeContext 对同一客户端 + 同一 context_name（Session ID）且旧上下文存活时
  **复用同一标签页**（显式 initial_url 原地导航、空则原样续页），不再“释放旧页开
  新页”——uws run/goal 跨命令单步续作由此可用；旧“同名重建开新 Tab / 默认上下文
  替换”契约测试改写为复用断言。上下文槽位按 API Key 全局寻址（默认空名仍按
  连接隔离），Target ID 查找放开为全局。验收：`TestSameContextNameReusesLiveTarget`
  + `TestNamedSessionReusesAcrossConnections`（真实两 CLI 进程场景）+
  `TestEngineReinitSameNameReusesTab` + `TestEngineDefaultContextReusesTarget`。
- [x] **TDD-ENG-19**（P1/M6C/单元，**2026-09-03 AX/ref 链接带 href**）：页面快照
  两段都可区分同文链接——AX 内容树 link 行序列化 CDP `url` 属性；ExportPageTree
  经 `probeHrefMapExpr`（与采集同 BFS 序读 kept 锚点）给 `[node_id]` ref 行追加
  `href="..."`（首引号文本不变，协调器 parse_tree_nodes 兼容）。react.dev 同文
  「文章页链接 vs 页内 # 锚点」不再盲点。验收：`TestSerializeAXContentKeepsSemanticRows`
  （url 属性）+ `refhref_test.go`（refRowID/注解/引号转义）。
- [x] **TDD-ENG-20**（P0/M6C/单元，**2026-09-03 ExecuteAction Target 钉扎 +
  Init current_url**）：共享浏览器各会话各管各的 Tab——协调器每次 ExecuteAction
  带 `context_hint["target_id"]`，引擎校验其属于 context_name 对应会话，不一致
  NotFound 且零物理执行；InitContextResponse 追加 `current_url=6`（续页旧会话时
  协调器首轮不再把真实页面误报成空白页）。验收：
  `TestExecuteActionTargetPinRejectsForeignTab` + `TestInitResponseCarriesCurrentURLOnReuse`。
- **真实 CDP 层 P0 加固（2026-08-20 续12，Chrome 151 实测全绿）**：视口外元素自动滚动
  定位（`TestCDPSessionScrollIntoView`）；导航失败显式化（`TestCDPSessionNavFailure`——
  连接拒绝 → chrome-error 判定 → `cdp: navigation failed: unreachable: ...`）；
  在途 XHR/Fetch 30s TTL 剪枝（`TestFreshInflightTTL` 单测）；页面级 HTTPStatus
  接入候选（≥400 整页物理湮灭）。外部调研任务书：`docs/evidence/research/cdp-research.md`。
- **语义定位作用域修复 + 失败诊断通道（2026-08-27，uws-novel-test 前置/批次 1）**：
  `findElementOpt` 语义评分分支漏声明几何变量 `r`，语义命中（如「More ›」）
  必然 `ReferenceError`；旧 `executeAction` 把它与无匹配统一 mask 成
  `target_not_found`，黑盒实验误判为会话路由 Bug。修复 = 评分分支自声明
  `const r = el.getBoundingClientRect()`；诊断 = 失败原因分类
  `eval_error:<exceptionDetails>` / `eval_empty` / `no_match` + `|url=<href>`；
  `InitContextResponse` 的 target_id/session_url 改返回真实 CDP targetId
  （`browser.TargetDebugInfo`，`findContextLocked` 兼容回传路由）。
  验证：真实 Chrome `TestCDPSessionFindElementSemanticMatchNoScopeBug`（沙箱 skip）+
  纯逻辑 `TestClassifyFindFailure`/`TestWsBaseFromEndpoint` +
  `TestInitContextExposesRealTargetID`；现场实证 extract「More」成功、click
  导航至 /more/h5Banner?sectionId=…，T00 截图 md5 与基线一致。

### 3.4 知识库热更新与回滚

*对应 `spec/knowledge-versioning.md` 双缓冲、原子切换与 P0 自动回滚*

- [x] **TDD-KB-01**（P0/M5/集成）：`dry_run=true` 时仅下载并校验 SHA256，不切换：`active_version` 与 `schema_hash`/`model_hash` 均不变。（2026-08-19 点亮：实现层 `pkg/engine` bufconn + 注入多版本 fetcher；契约层经真实二进制验证 dry-run 不切换 + 未知版本 fetch_failed）
- [x] **TDD-KB-02**（P0/M5/集成）：正式切换成功后返回正确的 `active_version`/`previous_version`/`switch_time_ms`；切换写锁窗口 < 1ms，期间并发 `ExecuteAction` 无失败、无半更新状态；旧句柄在 TTL 60s 内仍可服务进行中的异步验证。（2026-08-19 点亮：实现层 gRPC 切换 + `GetKnowledgeVersion` 版本/哈希一致性；写锁 <1ms 与并发无半更新/旧句柄 TTL 见逻辑层 `pkg/knowledge/knowledge_test.go`；2026-08-19 深夜续：契约层经真实 HTTP fetcher + exporter 产物点亮）
- [x] **TDD-KB-03**（P0/M5/集成）：Sanity Check 失败（关键规则索引缺失、ONNX 空特征推理异常）时返回 `ok=false` + `reason="sanity_check_failed"`，旧版本保持有效。（2026-08-19 点亮：实现层注入异常产物验证；契约层经真实二进制验证内置知识库 ONNX 未就绪时切换被拒、旧版保持）
- [x] **TDD-KB-04**（P1/M5/集成）：加载新版本时可用内存 < 300MB，拒绝切换并返回 `RESOURCE_EXHAUSTED`。（2026-08-19 点亮：实现层注入高内存产物，双缓冲驻留超限拒绝切换；2026-08-19 深夜续：契约层经 manifest `memory_estimate_mb=500` 产物验证拒绝并保旧版）
- [x] **TDD-KB-05**（P1/M5/集成）：版本不兼容（`min_engine_version` 高于当前引擎，或 `max_engine_version` 低于当前引擎）且 `force=false` 时拒绝；`force=true` 跳过兼容性校验完成切换（紧急回滚路径）。（2026-08-19 点亮：实现层 `version_incompatible` + `force=true` 跳过；2026-08-19 深夜续：契约层经真实产物验证拒绝 + force 强制切换；2026-08-20 续7：`max_engine_version` 上限校验落地——`x` 通配语义 `v1.4.x`=v1.4 任意 patch、`v1.x`=v1 任意 minor/patch，httpfetcher 透传 manifest，exporter 默认对齐 v1.5.x；Go 逻辑层 9 例 + 契约层 `test_tdd_kb_05b_max_engine_version_reject` 全绿）
- [x] **TDD-KB-06**（P0/M5/集成）：P0 自动回滚：连续 50 次 `NOT_FOUND` 触发自动回滚至上一稳定版本（< 1 分钟），`HealthCheck` 附带回滚标记，`rollback_logs` 写入审计记录。（2026-08-20 点亮：`pkg/knowledge` 新增 `AutoRollback`（复用双缓冲 previous 句柄、回滚一致性修复、防乒乓）；`pkg/engine` 连续 NOT_FOUND 计数（成功动作重置、阈值 50、触发即清零）接通自动回滚；proto v1.5.0 `HealthStatus` 新增 `rolled_back`/`rollback_reason`；实现层 `m5_integration_test.go` 与契约层 `test_knowledge.py`（真实 HTTP fetcher 产物）双层验证全绿）
- [x] **TDD-KB-07**（P1/M5/集成）：回滚一致性：新版本中 ACTIVE 但旧版本无对应记录的 L2 规则被自动重置为 STAGING 并进入重新评估（防止污染旧知识）。（2026-08-19 点亮：`ReloadKnowledge` 回滚旧版本路径同样触发一致性修复——`demoteUnknownActive` 在 Reload/Rollback 共用；实现层 gRPC + 逻辑层双验证；2026-08-19 深夜续：契约层经真实 HTTP fetcher 验证回滚 RPC 全链路）
- [x] **R1（2026-08-20 点亮，架构接线）**：知识库热更→匹配管线贯通——matcher 由 KB active artifact 全量重建（KB 单一事实源），热更/人工回滚/P0 自动回滚成功后 `syncMatcherWithKnowledge` 同步；`knowledge.Artifact` 携带完整规则/同义词/黑名单快照（`RulesFull`/`Synonyms`/`NegativeKeywords`，httpfetcher 从 uws_l1.db 全量加载）；matcher `ReplaceAll` 单事务替换 + RWMutex 并发安全（读者要么全旧要么全新，无半更新）。验证：实现层 `TestR1HotReloadChangesMatchingGRPC`（register_now 0.90→0.80、banner 黑名单放行→NOT_FOUND、回滚还原）+ `TestR1AutoRollbackRestoresMatchingGRPC`（坏版本黑名单封杀登录 50 次 NOT_FOUND → 自动回滚 → 登录恢复）；契约层 `test_r1_hot_reload_changes_matching`（真实 HTTP artifact 双层验证）

### 3.5 VLM 感知兜底与自进化（2026-08-27 新增）

*对应 `pkg/vlm`（DeepSeek 视觉候选 + learned 存储）+ `pkg/browser`（ClickAt / 区域截图）
+ `pkg/engine/vlm_fallback.go`（兜底编排）。理念：VLM 只产候选、不产结论；成功样本
回写 learned（URL+语义 → CSS 路径/内部语义），下次同页面免 VLM。*

- [x] **TDD-VLM-01**（P1/单元）：VLM 候选解析——`found/text/x_ratio/y_ratio/selector`
  容忍 markdown 围栏与夹带文字；无 JSON 输出报错。（`pkg/vlm/vlm_test.go` 点亮）
- [x] **TDD-VLM-02**（P1/单元）：DeepSeek 客户端请求形状——OpenAI 兼容
  `/chat/completions`、base64 内联、Bearer 鉴权、temperature=0；非 2xx 映射错误。
- [x] **TDD-VLM-03**（P1/单元）：learned 存储——JSON 原子持久化、URL 归一化
  （去 query/fragment）、命中累加 hits。
- [x] **TDD-VLM-04**（P1/集成）：引擎兜底编排——learned 命中免调 VLM（hits+1）；
  坐标点击成功回写 selector；extract 直接返回 VLM 文本；VLM 失败保持原 NOT_FOUND；
  物理层 no_match 后置兜底成功。（`pkg/engine/vlm_test.go` 点亮）
- [x] **TDD-VLM-05**（P1/集成）：针对性截图/缩放（核心能力）——先全页粗定位 →
  `CaptureScreenshotRegion` 裁剪（60%×45% 视口、边缘钳制）→ 二次 VLM 精修坐标 →
  ClickAt；精修坐标换算正确。（`TestVLMRefineRegionZoom` / `TestRefineRegionClamp` 点亮）
- [x] **TDD-VLM-06**（P1/单元，2026-08-27 傍晚）：看图即答配置——请求体必须带
  `thinking:{type:"disabled"}` + `max_tokens:300`，提示词含「不要推理」；这是关闭
  DeepSeek 视觉模型默认思考链、把「语义推理/精确到中心」拉回「看→匹配→快猜」的关键
  （实测 reasoning 归零、单次 1.1~1.5s、318~378 tok；旧 602~2069 tok）。
  （`pkg/vlm/vlm_test.go` `TestDeepSeekResolveFastGuess` 点亮）
- [x] **TDD-VLM-07**（P1/单元，2026-08-27 描述性修复）：extract 主控提示词从
  「找到文字X」（会回标签词「价格」而非内容）改为「照抄与目标相关的可见内容」
  （价格带数字单位、成分/配料列全、标题照抄）；JSON 解析容忍字符串内裸换行
  （`sanitizeJSONNewlines`：转义后重试，防 `invalid character '\n' in string literal`）。
  实测：smzdm 列表页 `extract:商品价格` 返回「226元（需用券）318元」真实价格。
  （`pkg/vlm/vlm_test.go` `TestDeepSeekExtractPromptAsksDescriptiveContent` /
  `TestParseResultToleratesNewlinesInText` / `TestSanitizeJSONNewlines` 点亮）
- [x] **TDD-VLM-12**（P1/单元 + 集成，**2026-08-31 input 兜底缺口修复**）：VLM 主控
  动作集合补 `input`——规则库未覆盖站点（如 Airwallex）的 `input:` 语义此前无人兜底
  （`Capabilities` 只有 click/crop_zoom/extract/back/done/impossible，VLM 对输入框
  只能回 click 或不 impossible，值永远填不进去）。修复：`pkg/vlm/vlm.go` 新增
  `ActionInput` + `Capabilities` 含 input 动作 + `promptFor` input 分支（引导定位
  输入框/文本域/下拉框并报告中心坐标，明确「按钮不算」「聚焦后引擎负责填值」）；
  `pkg/engine/vlm_fallback.go` 主控循环新增 `case vlm.ActionInput` →
  `vlmActOnInput`（selector 直填 → 坐标点击聚焦后按返回 selector 填值 → 文本语义
  重试；成功后回写 learned）。验收：`pkg/vlm/vlm_test.go`
  `TestDeepSeekInputPromptLocatesInputBox` / `TestCapabilitiesIncludeInput` +
  `pkg/engine/vlm_test.go` `TestVLMMainControlInputFillsValue` /
  `TestVLMMainControlInputDirectSelector` 全绿；`go test ./pkg/vlm ./pkg/engine
  ./pkg/browser` 无回归。
- [x] **TDD-VLM-13**（P1/集成，**2026-08-31 Airwallex 表单实战补充**）：input 兜底
  坐标命中的是「容器/label」而非输入框自身时，必须就近下钻——① `VLMProbeSession`
  新增 `InputAt(x,y,value)` 坐标原语（`cdp_session.go`：elementFromPoint → 自身/
  后代/祖先/兄弟兜底找 input/textarea/select → 聚焦填入 → 返回输入框自身 selector）；
  ② `findElementOpt` 把 `label` 加入候选集合 + `leafOf` 支持 label→control
  （`label.control`/`htmlFor`）下钻——Airwallex 描述 textarea 无 placeholder/aria-label，
  语义评分抓不到，此前只能靠 VLM 坐标（多输入框表单 VLM 会报错框：15:15 实测把「描述」
  写进 productName）。验收：`TestVLMMainControlInputFillsValue` 改断言 InputAt 路径 +
  `TestVLMMainControlInputDirectSelector` + `TestCDPSessionFindElementLabelToControl`；
  Airwallex 实机验证 `input:产品名称=auto-renewable week` + `input:描述=订阅有效期内，
  可阅读本书城所有短篇故事` 两字段 DOM 均正确填入（未提交）。
- [x] **TDD-VLM-08**（P1/单元，2026-08-27 配置默认化）：`--vlm-fallback` 默认开启
  （`--vlm-fallback=false` 可关），API Key 自动读 `UWS_VLM_API_KEY` || 
  `DEEPSEEK_API_KEY`（`cmd/uws-engine/vlmAPIKey()`；WebTouch 优先、DeepSeek 回退）。
  （`cmd/uws-engine/vlm_key_test.go::TestVLMAPIKeyAutoReadsDeepSeekOrUWS` 点亮；
  主引擎 50051 已按默认开启部署并功能验证）

- [x] **TDD-VLM-09**（P1/单元 + 集成，**2026-08-28「义眼」**）：`uws chat` 直接与视觉模型对话做
  「体验评审」——区别于义肢 `Resolve`（产候选坐标/selector 供执行），义眼喂 1~N 张图（`--img`
  本地/URL、`--url` 站点截图、`--page` 当前会话）+ 一句自然语言问题，模型按结构化 schema
  `{verdict,summary,checks,confidence}` 返回给 Agent 消费的判断（是否一致、是否满足需求、
  按钮位置、手势是否成功）。实现：`pkg/vlm/eye.go`（`EyeClient.Ask` 多图 OpenAI 兼容 +
  `ParseEyeContent` 结构化解析；请求体带 `thinking:disabled` 与 `max_tokens:1000`）+
  `cmd/uws/chat.go`（`chat`/`-p` 子命令，`--img/--url/--page/--json/--plain`；经引擎
  InitializeContext+CaptureScreenshot 采样站点）+ `cmd/uws/main.go` 放行。验收：
  `pkg/vlm/eye_test.go` 6 例 + `cmd/uws/chat_parse_test.go` 3 例；端到端实测 `--img` 图对图、
  `--url` 站点对比、`-p` 别名、`--plain` 自由文本。**注意**：义眼不产可执行动作、不做 agent
  （不规划/不自主执行），只做好「看一看 + 给反馈」。

- [x] **TDD-VLM-10**（P1/单元，**2026-08-28 义眼图像证据原语**）：`pkg/vlm/eyetools.go`
  确定性图像原语（VLM 可调用的 WebTouch 能力，纯标准库、不依赖模型/会话）——`DiffImages`
  （逐像素 diff，生成差异 overlay 图 + `DiffStats{ratio,bbox,max_channel}`，尺寸不一致按最小
  比较）、`RegionAvgColor`（区域平均色 "R,G,B"）、`ZoomImage`（区域最近邻放大）。
  验收：`pkg/vlm/eyetools_test.go` 7 例（差异检出/相同无差异/尺寸取最小/区域取色/缩放尺寸/
  坏图报错/stats 可读）。
- [x] **TDD-VLM-11**（P1/集成，**2026-08-28 证据编排**）：`uws chat --diff`（≥2 张图 → 像素
  diff 差异图 + 统计注入问题）、`--zoom x,y,w,h[,scale]`、`--region-color x,y,w,h`；`--plain`
  强约束自然语言（`ask(JSON=false)` + 系统提示「不要输出 JSON/围栏」）；`--url` 站点截图
  「几乎相同」时注入警示（`identicalShotsWarning`，防止 sim 引擎把两站误判一致，输出
  verdict=unknown）。验收：`cmd/uws/chat_parse_test.go` 增 2 例（evidence flags/parseRect）；
  端到端实测 `--diff` 对「左上红块 vs 右上蓝块」给出量化判据（差异 8.3%、bbox 覆盖两方块）、
  `--plain` 出自然语言、`--url` sim 假阳性触发 unknown。**要点**：diff 是确定性证据，VLM
  基于它给判据，不是裸眼看图猜。

- [x] **TDD-VLM-12**（P1/单元 + 集成，**2026-08-28 自然语言自决路由**）：用户明确「不传
  `--diff`/`--url`，只是问一句也能很好完成」。`chat` 在未显式给视觉输入时，若问题带 ≥2 个
  站点 URL 且为比较/查差异意图，自动抽取 URL（`extractSiteURLs`，支持 scheme 与裸域名并补
  `https://`）→ 转 `--url` 站点采样 → 集齐 ≥2 张字节图后自动开 `--diff` 拿像素证据；门控
  保守（仅比较意图 + ≥2 URL + 无显式输入），避免误开引擎。验收：
  `cmd/uws/chat_parse_test.go` 增 `TestExtractSiteURLs`（6 例）/`TestDetectCompareIntent`（8 例）
  /`TestApplyNaturalLangRoute`（3 例）；端到端实测：`uws chat "帮我检查 example.com 和
  wikipedia.org 的 differences"` → `verdict=different`（像素差异 96.3%、bbox 覆盖全图、4 条
  checks）；两站内容几乎相同（example.com/example.org）→ `verdict=unknown`（不误判一致）。
  **要点**：让用户/Agent 无需知道 `--url`/`--diff` 也能完成站点对比，且确定性证据优先。

- [x] **TDD-VLM-13**（P1/单元 + 集成，**2026-08-28 义眼主控循环（多轮能力，VLM 自决）**）：用户
  明确「要不要多轮是任务性质 + VLM 自主决策，引擎要有能力支持多轮，但 VLM 决策时要尽量单轮完成、
  避免过慢」。`chat` 默认走主控循环：VLM 每轮返回 `EyeTurn{action:conclude|zoom|region_color|diff,...}`
  ——`conclude` 直接给 `{verdict,summary,checks,confidence}`；请求证据则执行确定性原语
  （`DiffImages`/`ZoomImage`/`RegionAvgColor`）把结果作为新证据追加，进入下一轮；系统提示强约束
  「优先单轮收尾、确不足才请求一个原语、每轮最多一个、不空转」；轮数上限 `eyeMaxRounds=4`，超限
  安全降级为 unknown。实现：`pkg/vlm/eye.go` 增 `EyeTurn`/`ParseEyeTurn`/`EyeControlSystemPrompt`
  + `EyeRequest.System`；`cmd/uws/eye_loop.go`（`eyeMainControl`/`runEyeEvidence`/`describeImages`）；
  `runChat` 非 `--plain` 走 `eyeMainControl`。验收：`eye_turn_test.go`（conclude/zoom/杂音 3 例）、
  `eye_loop_test.go`（diff/zoom/region_color/未知 action 4 例 + mock VLM 多轮主控 1 例：round1 请求
  zoom→round2 conclude，calls=2、证据图追加）；端到端真实 Chromium：`uws chat "帮我检查 example.com 和
  wikipedia.org 的 differences"` → 自动注入站点截图+diff 证据后**单轮 conclude**（`different`、96.3%）。
  **要点**：多轮是「能力」不是「默认路径」——VLM 自决，常见任务一轮而已，避免过慢。

---

## 4. 协调器与反思兜底 (Coordinator) - Python

*聚焦任务编排、错误恢复与宏观自愈*

### 4.1 异常映射与自动重试

- [x] **TDD-COORD-01**（P0/M6C/集成）：模拟引擎返回 `DEADLINE_EXCEEDED`，协调器必须触发 Fibonacci 退避重试（间隔 1s、2s，最多 2 次），重试成功后继续后续步骤。（2026-08-19 点亮：`coordinator/executor/executor.py` + `reflector.py`，FakeEngine 脚本化 + 可注入时钟断言 1s+2s 退避、重试后继续后续步骤；`tests/integration/test_coordinator.py`）

  > 对齐说明：`spec/coordinator.md` §6.1 与 SKILL.md 以「最多 2 次」为协调器侧策略；proto/algorithm-kernel 的「重试 1 次」是引擎侧契约建议。实现时统一采用协调器侧「最多 2 次、间隔 1s/2s」，测试不得写死 3 个退避间隔。

- [x] **TDD-COORD-02**（P0/M6C/集成）：模拟引擎返回 `NOT_FOUND`（元素确实消失了），协调器不得死循环，必须调用 `planner`（LLM）发起局部重规划。（2026-08-19 点亮：NOT_FOUND → Reflector 判 REPLAN → `Planner.replan` LLM 生成替代子计划合并进执行流；失败步骤不盲目重试）
- [x] **TDD-COORD-03**（P0/M6C/集成）：模拟 Target 崩溃返回 `FAILED_PRECONDITION`。协调器应当重新初始化环境，并正确**只重放幂等步骤**（如表单填写），不可重放已完成支付的点击步骤。（2026-08-19 点亮：REINIT → 重新 InitializeContext → 只重放 `IDEMPOTENT_ACTIONS` 且未标 `non_idempotent` 的已完成步骤；测试断言支付点击仅执行 1 次）
- [x] **TDD-COORD-06**（P1/M6C/集成）：崩溃恢复：HealthCheck 连续 3 次失败判定 Target 死亡——存在最近 `NAVIGATED` 快照时：重新 `InitializeContext` → 导航至快照 URL → 重放幂等步骤 → 跳过 `non_idempotent`（支付/发帖）→ 从中断点继续；无快照时全量重跑。（2026-08-19 点亮：`_probe_health` 连续 3 次失败触发 `_recover_crash`，快照/全量重跑两场景均断言）
- [x] **TDD-COORD-07**（P1/M6C/集成）：反思机制：同一任务连续失败 ≥ 2 次时，收集最近 5 步 `decision_trace`，回退至最近一个 `STABLE` 步骤重放，断言错误不累积（对比顺序直跑基线）。（2026-08-19 点亮：`_reflect` 丢弃回退点及之后的 completed/skipped 标记并重放，`decision_trace ≥ 5` 断言；反思每任务最多 1 次防死循环）

### 4.2 VLM 视觉打码联动

- [x] **TDD-COORD-04**（P0/M6C/E2E）：当协调器收到 `CHALLENGE_INTERCEPTED` 状态，必须挂起当前执行流，抓取截图并调用 Mock 的 VLM 服务。VLM 返回坐标后，协调器下发物理拖拽动作解锁页面，随后原任务流无缝继续。（2026-08-20 集成层点亮：新增 `coordinator/vlm.py`（`VlmSolution`/`VlmSolver` 协议/`MockVlmSolver` 确定性求解器）+ `EngineClient.capture_screenshot` 契约 + executor `_solve_challenge` 编排——截图 → VLM 坐标 → ExecuteAction drag（`challenge_slider`）→ 解锁后重试原步骤无缝继续，单任务求解 3 次封顶（`MAX_VLM_SOLVES`，防无限重试）；`tests/unit/test_vlm.py` 3 例 + `tests/integration/test_coordinator.py` 4 例全绿（解锁继续/求解耗尽 PAUSED/无求解器降级 PAUSED/拖拽失败 PAUSED）。**2026-08-20 续16 真实 CDP E2E 点亮**：v1.5.1 追加 `CaptureScreenshot` RPC + `ExecuteAction drag` 原生拖拽（坐标直发、bypass 语义匹配与挑战门禁）；`tests/e2e/test_coordinator_vlm.py`（403+CF 挑战头 → UNAUTHENTICATED → 真实 PNG 截图 → Mock VLM → drag 滑块 → /solve → reload 200 → 原步骤重试成功）`UWS_E2E_CDP=1` 全绿）

### 4.3 爬虫与数据提取管道

- [x] **TDD-COORD-05**（P0/M6A/E2E）：以 CLI 执行 `uws crawl --depth 2` 爬取 Mock 博客站点。验证：它能自动寻找下一页链接并深入提取，最终输出的 Markdown 数据中，包含所要求的 `signals`（如正文和作者），且**不含**侧边栏的广告内容。（2026-08-20 续24 真实侧点亮：`coordinator/crawler.py` CrawlDriver + Mock 博客站真实 CDP E2E——首页提取正文/作者 → 点击「下一页」深入第二层 → Markdown+YAML Frontmatter 输出、广告/推广文本零残留，`UWS_E2E_CDP=1` 实测 33s 全绿；顺带修复 findElement 语义源匹配真实 bug（GLOBAL §5 #25）。CLI 子命令编排仍待 M6A CLI 定案）
- [x] **TDD-COORD-08**（P1/M6A/E2E，**2026-08-20 续33 已移交 search-engine**）：`se parse-warc` 离线模式：输入 WARC 归档，按 `signals` 提取并输出 Markdown + YAML Frontmatter，广告/噪声剔除规则与在线模式一致。验收细则随代码迁移至 `labs/world-replay/search-engine/docs/tdd-checklist.md`。
- [x] **TDD-COORD-09**（P1/M6C/集成）：任务执行中触发 `NEW_WINDOW_OPENED`：协调器自动 Attach 新 `target_id` 并继续执行，`navigation_counter` 正确递增。（2026-08-19 点亮：`attach_target` 切换 `current_target_id` + `navigation_counter` 递增；真实 gRPC 客户端 Attach 待 v1.5/CDP 定案，见 `USER_ACTION.md`）

### 4.4 规划器与模板引擎

*对应 `spec/coordinator.md` §4 分层规划与 §8 模板引擎*

- [x] **TDD-PLAN-01**（P0/M6C/单元）：L1 精确模板：`template_id` 精确匹配时直接实例化（< 1ms），`{{ var }}` 变量插值正确，全程不调用 LLM。（2026-08-19 点亮：`coordinator/planner/planner.py` `TemplateRegistry` + `{{ var }}` 插值（缺参报 `PlanValidationError`，不静默留占位）；测试断言 LLM 零调用）
- [x] **TDD-PLAN-02**（P1/M6C/单元）：L2 模糊模板：语义 Embedding 相似度 > 0.85 时命中 Top1 模板（约 50ms）；低于阈值不得误命中，不得调用 LLM。（2026-08-19 点亮：可插拔 `Embedder` 协议 + 零依赖 `TokenOverlapEmbedder` 降级（生产换 MiniLM/BGE）；测试注入 FakeEmbedder 断言命中与阈值拒绝）
- [x] **TDD-PLAN-03**（P1/M6C/集成）：L3 LLM 补全：复用模板片段 + LLM 补全缺失步骤，输出严格符合 JSON Schema，仅含合法 `action_type` 且依赖存在。（2026-08-19 点亮：`_plan_from_llm` 严格校验 + 语义依赖重映射（`depends_on` 可引用 target_semantic）；非法 action/死链拒绝）
- [x] **TDD-PLAN-04**（P0/M6C/单元）：计划校验：循环依赖、死链（依赖不存在的 `step_id`）、空/纯占位 `target_semantic` 必须被拒绝或修复，禁止进入执行器。（2026-08-19 点亮：`validate_plan` DFS 三色环检测 + 死链/空占位/非法 action/重复 ID 全量拒绝）
- [x] **TDD-PLAN-05**（P1/M6C/集成）：条件分支与循环：`condition: "{{ has_captcha }} == true"` 分支语义正确；`loop.max_iterations` / `exit_condition` 生效，循环超限安全退出（不无限翻页）。（2026-08-20 点亮：条件表达式扩展 `== | != | in | not in` + 布尔/数字/`['a','b']` 列表字面量（类型感知比较：bool↔"true" 互认、int/float 互通）；缺失变量安全默认（`==` 不执行 / `!=` 执行）、非法表达式默认不执行分支；`loop.max_iterations` 封顶安全退出、`exit_condition` 满足提前退出、`{{ var }}` 插值、循环中途失败不吞错；`tests/integration/test_coordinator.py` 4 例全绿）
- [x] **TDD-PLAN-06**（P1/M6C/单元，**2026-08-27 append-only**）：请求级自动分解 DSL：`parse_plan_dsl` 把「少 token、无成对关键字」的 DSL（`verb:操作数`、`search:q` 宏、`a || b` 失败回退、`x | $var` 绑定、`?guard` 守卫、`nav:`/`reply:` 计划级指令）编译为真实可执行 `ActionNode[]`，编译产物必须过 `validate_plan`；`Planner._plan_from_llm` 接受 DSL 字符串或旧 JSON steps（append-only 兼容）。验收：`tests/unit/test_planner.py` 3 例（「搜索A类精华并返回成分表」分解 / `||login`→replan+`||reply`→abort / LLM 出 DSL 字符串）全绿。（设计边界见 `coordinator/MEMORY.md`：`nav/reply/$var` 是计划级元数据、非 ActionNode，因引擎无 goto/reply 动作）
- [x] **TDD-PLAN-07**（P1/M6C/单元，**2026-08-27 append-only**）：DSL 确定性引用 `node:<id>`：`click:node:12` / `extract:node:31` 编译为 `ActionNode.node_id`（探针 BFS 编号，跳过语义派发），target 保留 `node:<id>` 可读性；非正整数（`node:abc`/`node:0`/`node:`）解析期 `PlanValidationError`，编译产物过 `validate_plan`。验收：`tests/unit/test_planner.py` 2 例（编译到 node_id + 拒绝非正整数）。（执行接线——node_id 回查定位器 RPC——待引擎，见 GLOBAL §5 #57）
- [x] **TDD-PLAN-08**（P1/M6C/集成，**2026-08-27 v1.5.5**）：`ActionNode.node_id` 运行时透传：executor `_execute_step` 把 `node_id` 传给 `execute_action`，引擎 `node_id>0` 跳过语义/选择器派发按 BFS 序回查执行。验收：`tests/integration/test_coordinator.py` `test_tdd_plan_08_node_id_threading`（FakeEngine 断言 click/extract 的 node_id 透传）。
- [x] **TDD-PLAN-09**（P1/M6C/集成，**2026-08-27 v1.5.5**）：DSL `| $var` 运行时打通：executor 把 extract 成功结果写回 `TaskContext.variables`（按 `TaskDefinition.bindings`），`reply:$var` 由调用方插值。验收：`tests/integration/test_coordinator.py` `test_tdd_plan_09_extract_binding_writeback` + `tests/unit/test_cli_run.py` `test_run_task_plan_consumes_dsl`（initial_url/bindings/result 全链路）。
- [x] **TDD-PLAN-10**（P1/M6C/单元，**2026-09-02 append-only**）：DSL `drag:<x1,y1,x2,y2>` 原生拖拽动词：坐标操作数整体即 value（target 固定语义 `drag`），分隔容忍逗号/空格（镜像引擎 `ParseDragCoords`）；非 4 数字/非数字/空操作数解析期 `PlanValidationError`；编译产物过 `validate_plan`；executor 通用映射把 value 透传引擎 `ExecuteAction expected_action="drag"`。验收：`tests/unit/test_planner.py` 2 例 + `tests/unit/test_cli_run.py` 1 例全绿（DSL 直通引擎坐标原语，滑块演示 `uws run --plan "drag:..."` 端到端打通；VLM 坐标兜底仍缺，见 USER_ACTION #28）。

---

## 5. 安全合规与 PII 屏蔽

*安全红线*

- [x] **TDD-SEC-01**（P0/M4/安全）：向 `<input type="password" value="mySecret123">` 注入内容，拉取该动作产生的在线日志（`online_logs`）及 JSON 快照。必须断言其中所有的 `"mySecret123"` 均被转换为 `"[REDACTED]"`，且布尔位 `pii_masked=true`。字段级扩展：`pwd`/`pass`/`ssn`/`phone`/`id_card` 字段名节点同样生效。（2026-08-20 点亮：`pkg/engine` 新增 `online_logs` SQLite 落库（`--log-path`，schema 与 algorithm-kernel §4.4 对齐）+ PII 脱敏（敏感字段名命中 → 语义与值替换 `[REDACTED]`、`pii_masked=true`）；实现层 `logstore_test.go` + 契约层 `test_pii_redaction.py`（真实引擎落库断言）双层验证全绿）
- [x] **TDD-SEC-02**（P0/M4/安全）：外部协调器传入非法的 `target_semantic`（如超长越界字符串或包含 XSS 标签），接口必须阻断并返回 `INVALID_ARGUMENT` 错误码。（2026-08-20 安全层点亮：`tests/security/test_input_sanitization.py` 经真实引擎 gRPC 验证超长（121 字符）与 XSS 标签 → `INVALID_ARGUMENT`；实现层 `pkg/engine/sanitize.go` `validateIntent` 先于生命周期检查，无需 InitializeContext）
- [x] **TDD-SEC-03**（P0/M4/安全）：快照级脱敏：对 `snapshot_blob`（zstd 解压后）与 `node_features` JSON 做全树扫描，密码/手机号/证件号明文一律不得存在；`decision_trace` 仅保留长度与类型元信息。（2026-08-20 点亮：`redactJSON` 全树 PII 扫描（敏感键→值替换、字符串值 secret 原文替换）；`node_features`/`decision_trace` 仅写长度/类型元信息，不落原始文本/值；契约层 `test_pii_redaction.py` 断言全树无明文。**遗留**：`snapshot_blob` 规格写 zstd 压缩，当前以明文 JSON 落库（体积小便于审计），zstd 压缩待接入压缩依赖后补齐，见 `USER_ACTION.md`）
- [x] **TDD-SEC-04**（P0/M7/安全）：外部破解服务回调防伪造：无 `X-Solver-Signature` 或 HMAC-SHA256 校验失败的回调必须被拒绝，状态机不被污染。（2026-08-20 点亮：实现 `coordinator/solver_callback.py`，支持 `X-Solver-Signature` HMAC-SHA256 严格验签；对缺失 Header、错误签名、Payload 篡改全部抛出 `InvalidSignatureError` 拒绝并阻止状态机污染；`tests/security/test_solver_callback.py` 全绿）
- [x] **TDD-SEC-05**（P1/M7/安全）：破解超时降级：同步 ACK 超时（2s）、破解等待超时（60s）、回调超时重试 1 次后，最终返回 `UNAUTHENTICATED` 并升级人工接管，不得无限重试。（2026-08-20 点亮：实现 `AsyncSolverManager` 超时与重试控制，支持 ACK 超时检查(2s)、任务超时检查(60s)、最大重试 1 次限制，超限转 FAILED 并升级人工接管；`tests/security/test_solver_callback.py` 全绿）
- [x] **TDD-SEC-06**（P1/M4/安全）：参数化边界唯一见 `security-dx.md` §2.2；`input` ≤256 KiB、`evaluate` ≤1 MiB、其他 value ≤4096 字节；非 evaluate 禁 `javascript:`；语义与 hint 边界不变，超限派发前拒绝。验证：`pkg/engine/sanitize_input_test.go` 覆盖长中文、精确上限/超限、动作隔离与前缀，`tests/security/test_input_sanitization.py` 经真实引擎 gRPC 验证拒绝与合法输入过边界（未初始化 → FAILED_PRECONDITION）。

---

## 6. 离线蒸馏与自进化流水线 (Pipeline & Learning) - Python

*聚焦 CC 抓取、规则聚合、导出与影子评估*

### 6.1 CC 抓取与拓扑去重

- [x] **TDD-PIPE-01**（P0/M2/单元）：拓扑去重：同站点相似页面（SimHash 汉明距 ≤ 3）只保留 1 条；同域名相似页 > 50 触发域名熔断（该域名拉黑）。（2026-08-19 点亮：`pipeline/cc_fetcher/topology.py` 实现 64-bit SimHash（词频加权 md5）+ `TopologyDeduper`（簇内去重）+ `DomainFuse`（>50 拉黑）+ `CrawlGate` 组合闸门；`tests/unit/test_pipeline.py`）
- [x] **TDD-PIPE-02**（P1/M2/单元）：容错：损坏/截断的 WARC 样本不导致管道崩溃，计入失败率指标并跳过。（2026-08-19 点亮：`pipeline/cc_fetcher/warc.py` `WARCReader` 容忍 gzip/纯文本、头损坏与 payload 截断，成功/失败分别计数；`tests/unit/test_pipeline.py` 以 gzip WARC + 截断记录验证）

### 6.2 LLM 蒸馏与规则聚合

- [x] **TDD-PIPE-03**（P0/M2/集成）：蒸馏输出强制结构化：LLM 返回缺少 `distillation_data`/`new_synonyms`/`negative_keywords` 时重试或丢弃；`new_synonyms` 一律以 TENTATIVE 权重 0.2 入库，禁止直接 ACTIVE。（2026-08-19 点亮：`pipeline/llm_distiller/distiller.py` `LLMDistiller` 批量提交 + 结构化校验（有限重试→丢弃计数）+ 防投毒（LLM 声明 ACTIVE/1.0 一律压回 TENTATIVE/0.2）；`tests/integration/test_pipeline_learning.py`）
- [x] **TDD-PIPE-04**（P1/M2/集成）：聚合确定性：相同输入两次运行产物完全一致（哈希分片流式归约），内存 < 2GB；冲突规则（同 `text_pattern` 不同 action）被显式消解并记录。（2026-08-19 点亮：`pipeline/aggregator/aggregator.py` `Aggregator` 按 `hash(text_pattern)%256` 归约、冲突按「高置信→action 字典序」确定性消解且每个 text_pattern+action 对只记一条；`tests/integration/test_pipeline_learning.py` 双跑一致性 + 冲突审计）

### 6.3 导出与版本化

- [x] **TDD-PIPE-05**（P0/M2/集成）：导出完整性：SQLite + ONNX + manifest.json + checksums.sha256 齐全，各文件哈希与 manifest 一致，版本号语义化且唯一。（2026-08-19 点亮：`pipeline/exporter/exporter.py` `Exporter` 产出 `uws_l1.db`（schema 与 pkg/matcher 对齐 + knowledge_versions）、`classifier.onnx`（真实模型产出前为确定性占位）、`uws_l2_default.db`、manifest.json、checksums.sha256；`verify()` 全量校验；版本 vX.Y.Z 语义化 + 重复拒绝；`tests/integration/test_pipeline_learning.py`）
- [x] **TDD-PIPE-06**（P0/M3/集成）：困难负样本挖掘：训练集生成器必须为每个正样本生成「兄弟节点 + SimHash 最近邻 3 个干扰节点」负样本；抽样审计 100 条断言满足，防止模型退化为深度分类器。（2026-08-19 点亮：`pipeline/exporter/training.py` `HardNegativeMiner` 兄弟负样本 + 3 近邻（允许与兄弟重叠，取池内汉明距最小 3 个）；`audit()` 抽样审计；`tests/integration/test_pipeline_learning.py`）

### 6.4 在线学习与影子评估

- [x] **TDD-LEARN-01**（P0/M5/集成）：同义词状态机：`hit_count` < 5 保持 TENTATIVE（权重 0.2）；≥ 5 次命中且任务成功 → ACTIVE（权重 1.0）；连续导致兜底 → POISONED 物理隔离，不再参与召回。（2026-08-19 点亮：`learning/online_learner/synonyms.py` `SynonymStateMachine`，连续兜底阈值取 2（规格未给次数，见 `learning/MEMORY.md`）；`tests/integration/test_pipeline_learning.py`）
- [x] **TDD-LEARN-02**（P0/M5/集成）：动态先验毕业：拓扑孤岛（兄弟 ≤ 1 且非高危）α=0.1/β=0.1，1 次命中即可毕业；高危区域（支付/提交/密集列表）α=3/β=2，需连续 9 次命中；转 ACTIVE 还需激活次数 ≥ 20、score ≥ 0.85、5 天无衰退。（2026-08-19 点亮：`learning/online_learner/priors.py` 共轭先验 `can_graduate`/`can_promote` 两阶段；测试全覆盖边界）
- [x] **TDD-LEARN-03**（P1/M5/集成）：影子评估回放：候选 L2 规则回放历史快照时必须比对 `ast_semantic_hash`（物理同构）；快照缺失时不得毕业。（2026-08-19 点亮：`learning/shadow_eval/evaluator.py` + `snapshot_store/store.py`；哈希不一致/快照缺失 → 不毕业）

### 6.5 LLM 预算看门狗

- [x] **TDD-WATCH-01**（P0/M5/集成）：每日预算：达 80% 熔断离线蒸馏（LOW 优先级），在线兜底（HIGH）不受影响；达 100% 切断所有 LLM 出口，引擎退化为「纯规则 + A*」模式且不 panic。（2026-08-19 点亮：`pipeline/watchdog.py` `LLMBudgetWatchdog` 共用配额池、离线 80% 熔断/在线 100% 切断；`tests/integration/test_pipeline_learning.py`）
- [x] **TDD-WATCH-02**（P1/M5/单元）：令牌桶：在线突发容量 20 Token、每秒恢复 2 Token 行为正确（并发突发被限流，恢复速率符合预期）。（2026-08-19 点亮：`pipeline/watchdog.py` `TokenBucket`（容量 20/每秒恢复 2，可注入时钟）；`tests/unit/test_watchdog.py`）

### 6.6 WARC 结构蒸馏（候选立项 · 未排期）

> 对应 search-engine tdd-checklist §9（TDD-WARC-01/02/03 跨项目总控，2026-08-21 记录）。
> 原料 = WARC 原始 HTML（Range 单记录，State C 已实证可行）；目标 = WebTouch L1 通用结构
> 规则与组件基因——即用户确认的「学网页关键结构」（WET 纯文本无 DOM，不适用）。
> 上游 CC 抓取/蒸馏链（PIPE-01..06、WATCH-01/02）已就绪，本线补原料端与结构端。

- [ ] **TDD-PIPE-07**（P1/M2/集成）：WARC Range 单记录：按 CDX offset/length 拉取
  WARC response 并解压单 gzip member，输出原始 HTML；损坏/404 计入失败率不中断
  （复用 PIPE-02 容错语义）。
- [ ] **TDD-PIPE-08**（P1/M2/集成）：离线 HTML → 结构特征：解析原始 HTML 生成与在线
  探针同构的 NodeFeature（tag/role/attributes/dom_depth/组件签名），喂入现有
  LLMDistiller（PIPE-03 防投毒 + PIPE-04 聚合确定性复用）。
- [ ] **TDD-PIPE-09**（P1/M3/集成）：组件基因注入：从组件库演示站（Shadcn/AntD）提取
  标准 AXTree 生成 Confidence=0.95 基线规则，常驻 L1 白名单豁免 ONNX 复核；
  导出经 PIPE-05/06（版本化 + 困难负样本）闭环。

---

## 7. gRPC 契约与 API 层

*对应 `proto/uws_service.proto` 与 `security-dx.md` §1*

- [x] **TDD-API-01**（P0/M1/契约）：生命周期前置：未调用 `InitializeContext` 直接 `ExecuteAction` 必须返回 `FAILED_PRECONDITION` 并拒绝执行；初始化后正常执行。（2026-08-19 点亮：`pkg/engine` + 契约层 `test_grpc_contract.py`）
- [x] **TDD-API-02**（P0/M1/契约）：幂等性：相同 `intent_id` 在 60s TTL 内重复提交返回相同缓存结果，且动作只物理执行一次（副作用计数为 1）；TTL 过期后新意图正常执行。（2026-08-19 点亮：`pkg/engine` 幂等缓存 + decision_trace_id 一致性断言）
- [x] **TDD-API-03**（P1/M1/契约）：`ExecuteActionStream` 双向流：长连接内请求与响应一一对应、顺序一致；流中断后按 `FAILED_PRECONDITION` 语义可重建会话。（2026-08-19 点亮：`pkg/engine` 顺序处理 + 契约层顺序断言）
- [x] **TDD-API-04**（P0/M1/安全）：API Key 鉴权：无 Key / 非法 Key → `UNAUTHENTICATED`；角色越权（如 monitor 调 `ReloadKnowledge`）→ `PERMISSION_DENIED`。（2026-08-19 点亮：`pkg/engine` Authenticator 拦截器，角色默认权限见 `security-dx.md` §1.2）
- [x] **TDD-API-05**（P1/M1/安全）：mTLS：开启后无证书/无效证书的连接在传输层被拒绝。（2026-08-20 点亮：测试内本地生成 PKI——根 CA + 服务端 SAN 证书 + 合法客户端 + 伪造 CA 客户端，`tests/integration/test_grpc_contract.py` 三例全绿：明文连接/无客户端证书/伪造 CA 客户端均在传输层被拒；合法客户端通过握手且应用层鉴权仍生效）
- [x] **TDD-API-06**（P1/M1/契约）：`HealthCheck` / `GetKnowledgeVersion` 返回的版本、哈希与当前生效知识库一致（`schema_hash`/`model_hash` 对齐 manifest）。（2026-08-19 点亮：内置 artifact v1.4.0 元信息 + 契约层一致性断言）
- [x] **TDD-API-07**（P0/M1/契约）：Proto 兼容性回归：仅允许追加字段/枚举，禁止修改已有字段 tag；CI 中生成桩代码 diff 为空（与 `contributing.md` §3 对齐）。（2026-08-19 点亮：契约层用例重新生成桩并与仓库 `proto/gen` 逐字节比对）
- [x] **TDD-API-08**（P0/M6C/契约，v1.5.1）：`CaptureScreenshot` RPC（未初始化 FAILED_PRECONDITION / target_id 不匹配 NOT_FOUND / monitor 越权 PERMISSION_DENIED / 返回 PNG+视口尺寸）+ `ExecuteAction expected_action="drag"`（value="x1,y1,x2,y2" 坐标直发；非法坐标 ABORTED）。（2026-08-20 续16 点亮：Go 引擎层 `vlm_test.go`（生命周期/串上下文/越权/挑战流）+ 契约层 `test_tdd_api_08_capture_screenshot_and_drag`（真实二进制 sim 会话）全绿）
- [x] **TDD-API-09**（P1/M6C/契约，v1.5.3）：多命名上下文（多标签会话）：
  `InitContextRequest.context_name` 创建命名上下文；`ExecuteAction` 经
  `context_hint["context"]` 路由到对应标签页；同客户端可并行持有多个上下文；
  同名重建替换旧槽位（旧 Tab 关闭）；`CloseContext` 释放命名上下文（幂等）；
  关闭后执行动作返回 FAILED_PRECONDITION；幂等缓存按「客户端+上下文+intent」隔离。
  （2026-08-25 点亮：`pkg/engine/multicontext_test.go` 5 例——命名路由 /
  CloseContext 幂等与释放 / 同名重建 / 默认上下文旧语义 / 截图 NotFound 边界；
  契约层版本断言同步 v1.5.3）
- [x] **TDD-API-10**（P1/M6C/单元，2026-08-25）：DeepSeek 三模式与开关的 Web-to-API
  编排——`model` 参数映射到模式（快速/专家/识图）且仅在「新会话」切换（进入会话后
  模式单选组消失）；`thinking` 映射深度思考、`search` 映射智能搜索（布尔开关、同值
  不重复切换）；未收录 `model`（如旧 `deepseek-chat`）不切换沿用默认；`/v1/models`
  返回三模式 + search 元信息。**2026-08-26 增补**：请求模式 ≠ 会话已锁定模式时
  API 自动开新 Tab 兑现模式（不依赖客户端 new_session，修复「专家模式没选对」），
  同模式多轮不重建；`GET /v1/sessions` 返回 `models`。（2026-08-25 点亮：
  `coordinator/providers/deepseek.py` 三模式目录 + `model_on_new_chat_only`/
  `search_semantic`；`api_server.py` `_ensure_search` + 新会话条件切换 +
  2026-08-26 自动重建兑现；`tests/unit/test_providers.py` 2 例 +
  `tests/unit/test_api_server.py` 全绿）
- [x] **TDD-API-12**（P1/M6C/契约，**2026-08-27 v1.5.5 混合指挥**）：`ActionIntentRequest.node_id`
  （`optional int32`，append-only）确定性引用——`node_id>0` 跳过语义/选择器派发按 BFS
  序回查执行，负值 INVALID_ARGUMENT；新增 `ExportPageTree` RPC（未初始化
  FAILED_PRECONDITION / 导出极简 AXTree 文本）。验收：Go 引擎 `pkg/engine/node_direct_test.go`
  4 例（绕过语义匹配 / 负 node_id 校验 / ExportPageTree 生命周期与数据 / learned 回写）+ 契约层
  `test_tdd_api_12_node_direct_and_export_page_tree`（真实二进制 sim 会话）。
- [x] **TDD-API-13**（P1/M6C/契约，`T-31` · 2026-10-01 · **GREEN**（真机 C1–C5 全 PASS，22:46））：站点会话身份（契约 §9「字段级冻结」）——
  chat 响应（非流式＋SSE 完成帧）带顶层 `site_conversation_id` + `page_url`，读不到为 `null`；
  `POST /v1/sessions` 与 chat 请求接受 `site_conversation_id`（body / 头 `x-uws-site-conversation-id`）并直接落该对话页；
  `GET /v1/sessions/{id}/conversations` 枚举站点侧栏；未登记地址模板的站点 fail-closed。
  验收：`apps/gateway/*_test.go` 单测（回填 / null / L0+L1 两链路 / 恢复 fail-closed）＋ 真机探针
  `python3 scripts/verify_t31_site_conversation.py gemini-web` 全绿（C1–C5，原始读数落 `docs/evidence/pilots/t31-acceptance-*.log`）。
  **2026-10-01 基线（RED）**：当前运行版响应无该字段，探针 C1/C4/C5 FAIL。
  **2026-10-01 收口（GREEN）**：A 方案（取值来源＋`from_url` 槽位双源：页地址 ① / L0 直发响应体 ②，并**地址对齐闭环**）落地
  `df68be86`＋`c48aee6a`；真机探针 C1–C5 = PASS 5 / FAIL 0 / NOT_IMPL 0（`t31-acceptance-20261001-224543.log`），侧栏该对话 1 条。
  **2026-10-02 00:5x 独立第二实例复核（`:8001`，非作者实例）**：Gemini C1–C5 复现全 PASS（`t31-acceptance-20261002-005110.log`）；**DeepSeek 侧 `site_conversation_id` 恒 `null`**（L0 直发 + 页地址不离开首页）⇒ 本条目按 **Gemini 达标**点亮，DeepSeek 覆盖记为 `T-31` 遗留项，不在本条冒充。
  **2026-10-01 真机（旁路 `:18300`，构建自 `1bbcc36c`）**：C5 PASS；C1 FAIL（L0 直发不跳会话页 ⇒ 按 §9 冻结口径回 `null`，属合规）、C2/C4 连带 FAIL、C3 未跑到（S3 另以直接读数 28 条为证）——判据/契约层待 **A/B 裁定**后重跑收口。证据 `docs/evidence/pilots/site-conversation-continuity-2026-10-01.md`。

  **2026-10-02 02:01 DeepSeek 覆盖点亮（`4b1760c8`）**：取值来源③（recipe `producer: session_mint` 且 `echo: true` 的槽位回显）＋地址对齐 ⇒ 真机探针 `deepseek-web` **C1–C5 全 PASS**（PASS 5 / FAIL 0 / NOT_IMPL 0，`t31-acceptance-20261002-015919.log`）。**TDD-API-13 两站（Gemini＋DeepSeek）同时达标**；废弃下方「DeepSeek 覆盖记为遗留项」的表述。
- [x] **TDD-API-14**（P0/契约，2026-10-31）：对外 session 走 CLI lease。`create → use → close` 复用同一 `session_id` 与 Profile；未绑定账号 `profile_unbound` 且不落盘；`@vault:` 不进 lease 与错误 JSON；空闲唤醒、死亡 owner 可回收、超过 LeaseTTL 关闭。`/internal/provider/*` 带废弃头；`UWS_PROVIDER_HTTP=410` 返回 `provider_http_retired` 且不调用引擎；已关闭 CLI lease 的 provider chat 返回 409 且不进引擎。Hub 可落账字段只有 `hub.channel` 与 `hub.site_conversation_id`。（点亮：`pkg/clisession/store_test.go`、`cmd/uws/session_test.go`、`apps/gateway/cli_session_gate_test.go`）
### 7.1 Playwright 适配层

- [x] **TDD-ADAPTER-01**（P1/M6A/单元）：适配层映射：`page.fill` / `page.click` / `page.wait_for_navigation` 正确转换为 `ExecuteAction`（`target_semantic` / `expected_action` / `value`），用 mock gRPC stub 验证，无需真实浏览器。（2026-08-20 点亮：`coordinator/playwright_adapter.py` 包装 `EngineClient` 协议——fill→input、click→click、wait_for_navigation→wait/NAVIGATED，每次调用生成独立 `intent_id`；`tests/unit/test_adapter.py` FakeEngine 断言字段映射与幂等消费）
- [x] **TDD-ADAPTER-02**（P1/M6A/单元）：边界隔离：适配层所有调用仅通过公开 gRPC 接口，不触碰 WebTouch 内部状态（静态断言/审计测试）。（2026-08-20 点亮：源码静态审计——禁止 `uws_service_pb2`/`UwsEngineStub`/`grpc.`/`pkg` 引用，且引擎交互仅经 `EngineClient` 协议四方法）

---

## 8. CLI 与开发者体验

*对应 `docs/cli-and-config.md` 与 `docs/user-cases.md`*

- [x] **TDD-CLI-01**（P0/M6A/E2E）：`uws run --attach`：接管本地 Chrome 新建标签页完成目标任务并输出 `result.md`，不破坏原标签页与登录态。（2026-08-20 点亮：`coordinator/cli.py` 提供 `run` 子命令，加载声明式任务 `my_task.json` 驱动 `Executor` 逐步执行并输出结构化 `result.md`，`tests/e2e/test_cli_run.py` 端到端闭环验证退出码 0）
- [x] **TDD-CLI-02**（P1/M6A/E2E）：`uws crawl`：`--concurrency 8 --depth 2 --signals ... --format md --output-dir` 产出含 YAML Frontmatter 的 Markdown，`signals` 过滤生效，退出码 0。（2026-08-20 点亮：`coordinator/cli.py` 提供 `crawl` 完整子命令，驱动 `CrawlDriver` 深度遍历，支持 `--depth`、`--signals`、`--negative-keywords`、`--format` 与 `--output-dir` 文件分片导出，`tests/e2e/test_cli_crawl.py` 端到端全绿）
- [x] **TDD-CLI-03**（P1/M6A/E2E，**2026-08-20 续33 已移交 search-engine**）：`se parse-warc`：无网络/无浏览器环境下可离线运行，输出与在线模式同构。验收细则随代码迁移至 `labs/world-replay/search-engine/docs/tdd-checklist.md`。
- [x] **TDD-CLI-04**（P1/M6A/E2E）：`uws api --target llm_chat`：启动后普通 `curl` 可完成一轮对话并返回流式/完整文本（Mock 网页）。（2026-08-20 续27 点亮：`coordinator/api_server.py` 纯 stdlib ThreadingHTTPServer 实现 OpenAI 兼容 `POST /v1/chat` + `GET /v1/health` + `GET /v1/models`——chat 轮次 = input(chat_input) → click(send_button) → extract(chat_answer)，DEADLINE_EXCEEDED 按协调器 Fibonacci 1s/2s 最多重试 2 次；页面会话首次请求初始化并常驻复用；`coordinator/cli.py` 新增 `api` 子命令（--target/--url/--host/--port/--chat-timeout-sec/--engine-addr/--api-key），绑定默认 127.0.0.1；验证：单元 `tests/unit/test_api_server.py`（重试成功/超限放弃/缺 user 消息，注入时钟零等待）3 例 + E2E `tests/e2e/test_cli_api.py`（FakeChatEngine + 真实 curl 一轮对话/health/非法请求/参数接线）2 例全绿。**2026-08-25 深夜扩展（方案 1）**：`POST /v1/chat` 支持 `stream:true`——`text/event-stream` 增量推送 `choices[].delta.content`（首块带 role=assistant）+ `finish_reason:"stop"` + `data: [DONE]`（无 Content-Length，收尾显式关闭连接）；chat_answer extract 统一带 `context_hint["prefer_sse"]="1"` 优先消费网络层 SSE 干净文本；单元新增 3 例（干净文本 hint / 增量 delta / HTTP SSE）全绿。**2026-08-25 深夜续**：完成判定升级为 SSE done 语义（引擎 `ActionResultResponse.chat_stream_done`，v1.5.2 append-only）——SSE 通道接入时以流结束为准收尾（生成中途停顿 >1.5s 不误判），未接入回退文本稳定性；单元新增 2 例（SSE 停顿不算完成 / stream Done 才收尾）全绿）
- [x] **TDD-CLI-05**（P1/M6A/单元）：`uws config set`：持久化至 `~/.uws/config.yaml`；`fallback_api_key` 写入后文件权限 600，任何输出不打印明文。（2026-08-20 点亮：`cmd/uws` config 子命令——纯 stdlib 手写极简 YAML 读写（固定 schema + 别名 fallback_model/api_base/api_key）、目录 0700/文件 0600、敏感键输出隐藏、未知键拒绝且不落盘；`tests/unit/test_cli.py` 构建真实二进制验证持久化/权限/明文隔离/增量保留/未知键；Go 逻辑层 `cmd/uws/config_test.go` 并行覆盖）
- [x] **TDD-CLI-06**（P1/M6A/E2E）：零配置启动：无配置文件时引擎以纯规则模式启动并输出可读告警，不崩溃。（2026-08-20 点亮：`tests/e2e/test_cli_zero_config.py` 验证在全新临时 HOME 目录无任何 config.yaml 情况下，`cmd/uws-engine` 以默认规则启动打印警告且进程稳定不崩溃；`cmd/uws config get` 正常读取并对未配置键优雅输出 `<unset>`；`cmd/uws/config.go` 补齐 get 敏感键脱敏支持，单元测试 `test_cli.py` / `config_test.go` 并行绿灯）
- [x] **TDD-CLI-07**（P1/M6A/安全）：调试 UI：`:8080/debug/task/{request_id}` 只读返回脱敏 JSON，端口必须绑定 `127.0.0.1` 或内网，禁止公网暴露。（2026-08-20 点亮：新增 `pkg/engine/debug_server.go` 与 `cmd/uws-engine` 参数接线，只读暴露 `/debug/task/{request_id}`，自动剥离敏感明文并置 `pii_masked=true`；`pkg/engine/debug_server_test.go` 单测与 `tests/e2e/test_cli_debug_ui.py` E2E 端到端全绿）
- [x] **TDD-CLI-08/09/10/11/12**（P1/M6A，**2026-08-20 续33 已移交 search-engine**）：`se search` / `se cc-fetch` / 真实数据闭环 / 持久化索引（`--index` + `--rebuild`）验收细则随代码迁移至 `labs/world-replay/search-engine/docs/tdd-checklist.md`；WARCReader 流式化（PIPE-02）与 search BM25 长度归一修复仍属 webtouch pipeline 层（`tests/unit/test_pipeline.py`）。
- [x] **TDD-CLI-13**（P1/M6C/单元，**2026-08-27 append-only**）：CLI 自解释 + 容错：`uws help`（及无参）返回 Agent 友好手册（快速启动/子命令/网关/引擎/请求级 DSL/错误速查/反模式，共享文案 `cmd/uws/agent-guide.md`，Go `//go:embed` 嵌入）；`python -m coordinator.cli help`/`--help` 同样自解释（epilog，读取同一共享文件）；引擎错误自动映射 agent 友好对策（`coordinator/cli.py` `_engine_hint`，NOT_FOUND→重新规划、FAILED_PRECONDITION→重建上下文、UNAUTHENTICATED→VLM/人工接管等）。**由此 skill 极简并指向 `uws help`，保证 skill 与 CLI 始终一致**。验收：`tests/unit/test_cli.py` 4 例（`uws help` 内容/无参同手册 / Python help 子命令 / `_engine_hint` 状态码映射）
- [x] **TDD-CLI-14**（P1/M6A/E2E，**2026-08-27**）：引擎守护进程生命周期由 `uws` CLI 直管（取代 launchd）：`uws daemon start|stop|restart|status` 直接控制 `cmd/uws-engine`；`uws run/crawl/api` 在引擎未监听时按需自动拉起（lazy start，闲置不随 CLI 退出被杀）。实现：`cmd/uws/daemon.go`（纯 stdlib + Setsid 后台进程 + `~/.uws/engine.{pid,json,log}` 状态；`UWS_ENGINE_ADDR/UWS_ENGINE_BIN/UWS_ENGINE_DIR/UWS_ENGINE_ARGS` 可配）；`cmd/uws/main.go` 接线。验收：Go 单元 `cmd/uws/daemon_test.go` 6 例（engineArgs/地址解析/二进制发现/状态目录/端口探测/pid 存活）+ E2E `tests/e2e/test_cli_daemon.py` 1 例（start→status→restart→stop→auto-start 全闭环）。
- [x] **TDD-CLI-15**（P1/M6C/单元，**2026-08-27 混合指挥演示桩**）：`uws commander --tree-file <极简AXTree> [--image 截图] [--dry-run|--mock]`：极简 AXTree（+可选截图）→ OpenAI 兼容 VLM 端点 → 解析 node_id 动作 → 编译为 DSL 步骤（多行 `click:node:12` / `extract:node:31`，可直接喂 `parse_plan_dsl`）；node_id 不在 AXTree 中按 impossible 处理、非法动作/非 JSON 返回空（不直通动作）。实现：`coordinator/commander.py`（纯 stdlib，Key 读 `UWS_VLM_API_KEY || DEEPSEEK_API_KEY`，请求体含 `thinking:disabled`）+ `coordinator/cli.py` 子命令 + `cmd/uws/main.go` 放行（不依赖引擎）。验收：`tests/unit/test_commander.py` 8 例（parser/单动作/多动作/围栏/非法/DSL 输出含跳过/请求体/thinking 开关）。
- [x] **TDD-CLI-16**（P1/M6C/单元，**2026-08-27 v1.5.5**）：`uws run --plan "<DSL>"`
  （或 `--plan-file`）运行时消费 DSL：`run_task` 把 `parse_plan_dsl` 的
  `steps/initial_url/bindings/result` 编译进 `TaskDefinition`（initial_url 注入
  variables、extract 结果按 bindings 写回、`reply:$var` 插值出最终结果）；`Executor._initialize`
  从 variables 透传 attach。验收：`tests/unit/test_cli_run.py` `test_run_task_plan_consumes_dsl`
  （nav/node:12/extract|$var/reply 全链路）。
  **2026-08-28 复杂流程实测补**：`reply:` 复合字符串内嵌 `$var` 逐 token 插值
  （此前仅整串为单个 `$var` 生效，复合串原样返回）——`_resolve_dsl_result` 改为
  `re.sub(r"\$(\w+)")`，未绑定变量替换空串；新增
  `tests/unit/test_cli_run.py::test_resolve_dsl_result_composite_interpolates`。

---
- [x] **TDD-CLI-17**（P1/M6C/单元，**2026-08-28 站点探索**）：`uws explore <url>`：启发式探索
  站点——BFS 同域链接 + CDP 结构采集（标题/正文/可交互元素/testid/链接）+ 页面类型分类
  （landing/detail/player/auth/me/checkout/search/other）+ Mermaid 导航图/功能清单 Markdown
  报告（每页关键截图内嵌，存 `<报告名>.assets/`，`--no-shots` 可关）；纯规则不调 VLM，
  CDP 不可用降级为 URL 级快照。实现：`coordinator/explorer.py` +
  `coordinator/cli.py` 子命令 + `cmd/uws/main.go` 放行。验收：`tests/unit/test_explorer.py`
- [x] **TDD-CLI-18**（P1/M6C/单元，**2026-08-31 attach 目标三选**）：`--attach`
  `debug`（uws-chrome-debug，9222，未就绪自动拉起）/ `default`（用户日常 Chrome，9223，
  运行中无调试端口时优雅退出并带 `--remote-debugging-port` 重启，登录态保留）/
  显式 CDP URL；run/explore/api 通用。实现：`coordinator/attach.py`（纯 stdlib、
  Chrome 二进制/profile 发现、probe/wait、macOS AppleScript 优雅退出、
  非交互环境 `UWS_ATTACH_CONFIRM=1` 门禁）+ `coordinator/cli.py` 接线。
  验收：`tests/unit/test_attach.py` 8 例全绿（URL 直通/非法值/复用运行端点/自动拉起/
  default 未运行拉起/default 需确认/default 确认后重启/env 覆盖）。
  **2026-09-03 扩展**：Chrome 生命周期收口到 uws——拉起时 `start_new_session`（setsid，
  防命令结束后被 exec 会话回收）+ `clear_stale_debug_locks`（9222 未监听且 SingletonLock
  owner PID 已死才清理残留三件套，绝不误伤活实例）；新增 2 例，计 9 例全绿。
  13 例（分类/链接归一化/报告渲染/截图落盘与内嵌/BFS 封顶与外域隔离/CDP 降级/CLI 接线）+ 真实站点
  drama.tgaigc.com 探索报告（证据 `../.mem/drama/uws-test/evidence/explore/`）。
- [x] **TDD-CLI-19**（P1/M6C/单元，**2026-09-03 AXTree 上报 Agent 层**）：`uws probe`
  [--url <URL>] [--attach debug] [--output report.md] [--keep-open]：附着持久调试 Chrome，
  InitializeContext → ExportPageTree + CaptureScreenshot → Markdown（AXTree 含 node_id 全文）
  + PNG 落盘；默认快照后 CloseContext，`--keep-open` 保留标签页（人等登录/继续操作）。
  该原语让 Agent 观察页面不再需要写 gRPC 脚本（操作层级 DSL > Chat > Python 硬门禁配套）。
  实现：`coordinator/cli.py` run_probe + parser + `cmd/uws/main.go` 放行。
  验收：`tests/unit/test_cli_probe.py` 3 例全绿（树+截图落盘/keep-open 保留/init 失败上报）。
- [x] **TDD-CLI-20**（P1/M6C/单元，**2026-09-03 失败可消费化**）：`uws run --plan` 终结失败
  （FAILED/PAUSED）时输出 **Failure 报告**：失败步骤 + 动作/语义 + 引擎原因（原码保留，
  不再被 replan 覆盖成笼统文案）+ 当前 URL + AXTree 片段 + 截图 PNG（`<报告名>.assets/`）；
  配置 `UWS_VLM_API_KEY || DEEPSEEK_API_KEY` 时附 VLM 自然语言诊断（OpenAI 兼容
  chat/completions，仅基于证据解释，非致命），未配置/无余额时在报告里显式说明、
  绝不假装看过图。实现：`coordinator/evidence.py`（collect/explain/render）+ executor
  记录 `failed_step_id` + `cli.py` run_task 追加报告（无 --output 时整份打印到 stdout）。
  验收：`tests/unit/test_cli_run.py` `test_run_task_failure_attaches_evidence_report`
  （原因/树/截图落盘/VLM 未启用说明全断言）。
- [x] **TDD-CLI-21**（P0/M6C/单元，**2026-09-03 执行护栏：付费/扣费暂停**）：动作语义/值/
  URL 命中付费扣费词，或页面 AXTree 出现付费 CTA（立即购买/去支付/确认订单/自动续费/扣费等）
  后执行「确认类」点击（下一步/确认/提交/继续/开通等）→ ExecuteAction 前 PAUSED，
  报告标注「需要确认（任务已暂停）」+ 原因/URL/AXTree/截图；不调用 VLM、无绕过参数，
  上层 Agent 必须停下询问用户。实现：`coordinator/guard.py`（确定性规则）+ executor
  `guard_before_execute`/`_refresh_page_risk` + evidence render 分支。
  验收：`tests/unit/test_cli_run.py` 护栏两例（语义命中零动作外发 / 页面 CTA 点击暂停）。
- [x] **TDD-CLI-22**（P1/M6C/单元，**2026-09-03 原命令单步 / 结束点页面摘要**）：
  不新增子命令（用户明确不要 `uws step`）：`uws run --plan "<单条 DSL>"` 就是单步，
  DSL 参数直传、不写临时计划文件；默认直接打印 stdout（要文件才 `--output`）。
  任务成功结束时输出「当前页面（任务结束点）」：当前 URL + 停留页面 AXTree 摘要
  （含 node_id，可继续 `click:node` 确定性引用）；失败/暂停停住并输出 Failure /
  「需要确认」证据报告（TDD-CLI-20/21）。实现：`coordinator/cli.py`
  `_current_page_summary_lines` + run_task 成功分支；grpc_client 映射
  `page_url → current_url`。验收：`tests/unit/test_cli_run.py`
  `test_run_success_prints_current_page_summary`（成功输出 URL + AXTree 摘要）。
- [x] **TDD-CLI-23**（P1/M6C/单元，**2026-09-03 stdout 优先输出契约**）：run/goal/
  explore/probe 完整结果一律直接打印 stdout（Agent 消费 stdout，不写文件再读文件）；
  `--output` 仅在需要持久化留档时另存一份副本（即使传了 --output，stdout 仍先打印
  完整报告）。实现：`coordinator/cli.py` run_task/run_explore/run_probe +
  `coordinator/goal.py` run_goal_cli 统一改为 print 全文 + 可选落盘。
  验收：`tests/unit/test_cli_run.py`
  `test_run_task_with_output_still_prints_full_report_to_stdout`。
- [x] **TDD-CLI-24**（P1/M6C/单元，**2026-09-03 probe 默认 stdout + Markdown/AXTree
  合一 + Tab ID**）：`uws probe` 无 `--output` 默认不写报告文件——AXTree 随 Markdown
  一体打印 stdout（含 Task/Context/Target ID、URL、截图路径；Target ID = 真实 CDP
  target，--keep-open 时保留该 Tab 可继续定位）；截图默认落 `~/.uws/artifacts/
  probe-<pid>.png`（传 --output 时报告/截图同目录名规则不变）。
  验收：`tests/unit/test_cli_probe.py`
  `test_probe_default_stdout_merges_markdown_and_axtree`。
- [x] **TDD-CLI-25**（P1/纯 HTTP 身份伴随请求，**2026-09-13**）：`uws request <url> --profile-file <profile> [--state <bundle>]` 不启动 Chrome，复用档案的 UA/Accept-Language/UA-CH 与同 `profile_id` Cookie；上下文头可显式提供，但 Cookie/UA/语言/UA-CH/Host 不可覆写。默认不跟随重定向；显式 `--follow` 最多 10 次、只允许同 origin（scheme、host 与规范化端口；省略的 HTTP/HTTPS 默认端口等价），每跳都重施身份头并由 jar 在域/路径范围内带 Bundle 与新设 Cookie；跨 origin 必须拒绝（Go client 不具备浏览器 SameSite/发起页上下文）。分区 Cookie 与 SOCKS4 明确拒绝；不得把 Go 的 TLS/HTTP2 行为宣称为 Chrome。验收：`pkg/fingerprint/http_client_test.go`（身份头、Cookie、Bundle/分区 Cookie/SOCKS4、默认、同 origin 带 Bundle 重定向与跨 origin 拒绝、默认端口归一）与 `cmd/uws/request_test.go`（自然参数顺序、身份头保护）全绿。
- [x] **TDD-CLI-26**（P1/身份注册表跨进程互斥，**2026-09-13**）：同一身份目录的 `Save` / `Bind` / `Unbind` 必须以 OS 自动释放的文件锁串行化；第二个进程在持锁期间不得绕过 1:1 绑定检查。验收：`pkg/fingerprint/registry_test.go::TestRegistryWriteLockSerializesSeparateProcess` 用独立测试进程持锁，断言 `Bind` 只能在锁释放后完成。
- [x] **TDD-CLI-27**（P1/远程 Lease HTTPS，**2026-09-13**）：两个独立 `HTTPLeaseBackend` 通过真实本地 TLS listener 竞争同一 `profile_id`；第二持有者必须收到 `ErrLeaseHeld`，释放后才可接管。验收：`pkg/fingerprint/lease_test.go::TestHTTPLeaseBackendUsesTLSRemoteEndpoint`。
- [x] **TDD-CLI-28**（P1/Lease 服务 TLS 部署门禁，**2026-09-13**）：`uws-lease` 可用成对的 `--tls-cert` / `--tls-key` 直接服务 HTTPS；任何非回环明文监听、单边证书配置或无效监听地址都必须拒绝；错误或不匹配的证书对必须在绑定端口前失败。验收：`cmd/uws-lease/main_test.go::TestValidateLeaseServerTransport` 与 `TestLoadLeaseTLSConfigRejectsBadMaterialBeforeListening`。
- [ ] **TDD-EXT-01**（P0/真实验收，2026-09-13）：在操作者在场并授权开窗的条件下，分别完成有头外站、Viewer 双屏收回、真实代理出口、两节点共同 TLS State Bundle/Lease 四项验收；每项必须有独立脱敏证据目录和明确通过/失败结论，不得将无头、本机 fixture 或 smoke 计作完成。操作与判据：`docs/evidence/archive/fingerprint-viewer-acceptance-runbook-2026-09-13.md`。
- [x] **TDD-GOAL-01**（P0/M6C/单元，**2026-09-03 chat 目标模式内核**）：自然语言目标
  （不传视觉参数）→ 每轮“当前 URL + AXTree + 截图”交给 VLM 自决（click/input/done/
  impossible/login，node_id 必须真实存在于 AXTree）→ 引擎按 node_id 确定性执行；
  结束输出状态 + URL + AXTree + 截图。实现：`coordinator/goal.py`（guard.py 每动作前置、
  无进展检测、空树重试、VLM Key 缺失明确 FAILED、截图落 ~/.uws/artifacts 或 --out-dir）。
  验收：`tests/unit/test_goal.py` 成功链路（click→input→click→done 且 initial_url 推断
  weibo.com/compose）等 8 例全绿。
- [x] **TDD-GOAL-02**（P0/M6C/单元，**2026-09-03 目标模式护栏/登录语义**）：动作命中
  付费扣费词 → PAUSED「需要确认」且零动作外发；VLM 决策 login / impossible 含登录强标记 /
  引擎 UNAUTHENTICATED → WAITING_USER（非交互输出报告并退出，登录态由持久 Chrome
  保留可重跑；交互终端回车续跑）。验收：`test_goal.py` 护栏/登录/impossible-login 3 例。
- [x] **TDD-GOAL-03**（P1/M6C/单元，**2026-09-03 DSL 自动路由与入口分流**）：chat/裸命令
  文本先做 DSL 语法校验（parse_plan_dsl 编译出步骤/初始 URL/结果）→ 有效则自动走确定性
  `run --plan`（不烧 VLM）；否则目标模式。`uws "目标"`（不传子命令）= chat 目标模式；
  显式视觉参数（--img/--url/--page/--diff 等）才走义眼。实现：`coordinator/goal.py`
  `is_dsl_prompt` + `cmd/uws/main.go` default→chat + `chat.go` eye/goal 分流委派
  `coordinator.cli goal`。验收：`test_goal.py` DSL 判定 2 例 + cmd/uws Go 测试绿 +
  实机 `uws "发布微博：hello"` 停在登录页输出 WAITING_USER（URL/AXTree/截图）。
- [x] **TDD-GOAL-04**（P2/M6C/文档，**2026-09-03 命令/参数设计同步**）：帮助即手册
  （cmd/uws/agent-guide.md）、webtouch-skill SKILL.md、GLOBAL.md §82、本表与
  tests/README 映射同步“chat 默认推荐入口 / DSL 自动路由 / DSL 适用熟悉站点自动化”
  的用法与参数（无新增用户子命令：goal 是内部委派实现）。
- [x] **TDD-GOAL-05**（P1/M6C/单元，**2026-09-03 空页自救 + 起步 URL 推断**）：三条
  `uws 'google today news'` 全 FAILED 复盘——无显式 URL 且不在站点提示表的短语
  推断不出起步 URL → 引擎只开 about:blank → 空树快照 4 次后静默失败（VLM 全程
  未参与）。修复：① infer_initial_url 增搜索引擎短语（google/bing/百度…）与裸域打开
  推断；② 空页等 3 次后转交 VLM 决策（自然语言反馈卡点）；③ VLM 动作集新增 goto
  （text=完整 URL，复用引擎 InitializeContext 导航语义，零 Go 改动）。验收：
  test_goal.py 11 例全绿（新增搜索短语/裸域、空页 goto→done、空页 impossible
  自然语言反馈 3 例）；实机 `uws 'google today news'` SUCCEEDED（直达 Google 结果页）。
- [x] **TDD-GOAL-06**（P1/M6C/单元，**2026-09-03 会话续页 + 两段式页面快照**）：
  run/goal 增 `--session <ID>`（默认 default）并贯穿 initialize/execute 的
  context_name/context_hint；报告输出 Session ID/Target ID；goal 可续跑同一页。
  ExportPageTree 两段式：CDP `Accessibility.getFullAXTree` 层级内容树（标题/正文/
  链接/状态，供 VLM 阅读）+ 探针 [node_id] 交互树（引用不变）；目标模式
  done 自述“内容为空/未加载”时自动等帧重拍最多 2 次，防空结果提前结束。
  验收：Go `TestSerializeAXContent*` + Python test_goal.py 14 例（会话贯穿、
  goto 保会话、空 done 重拍等新增）。
- [x] **TDD-GOAL-07**（P0/M6C/单元，**2026-09-03 空转先换决策再熔断**）：同一 URL +
  页面签名 + node_id + 动作连续重复时，第一次带同文候选 href 提示让 VLM 换一次
  决策（不执行第二次）；仍重复 → 立即 FAILED，reason 含可消费诊断（node/文本/URL/
  候选 href 或 goto/单步建议），不再烧轮数上限。换不同 node 点同名链接也算停滞
  （页面级：URL+页面签名连续 ≥2 轮不变且 click/input → 提示 goto 打开文章页 href/
  至多一次纠错点击，仍不变熔断）；max_rounds 后执行了动作给一次 done/确认宽限。
  验收：test_goal.py
  `test_goal_repeat_same_node_hint_switches`（换 node 继续成功）+
  `test_goal_repeat_same_node_stops_with_diagnosis`（2 次决策即停、只执行 1 次）+
  `test_goal_stagnant_different_nodes_stops_with_goto_diagnosis`（换节点熔断）。
- [x] **TDD-GOAL-08**（P1/M6C/单元，**2026-09-03 完成优先 + 真实 URL 接地 +
  stdout 两段树摘要**）：SYSTEM_PROMPT 完成优先（页面已答目标即 done 总结）；
  init 回传 current_url 后首轮 URL 真实；goal 报告补 Target ID；报告树经
  `coordinator/treefmt.py` 截断（ref 交互块优先完整、语义块头部，budget 8000），
  evidence/cli 同源复用；SITE_HINTS 增 react.dev/learn。验收：test_goal.py
  parse href 兼容/report Target ID + tests/unit/test_treefmt.py 4 例。
- [x] **TDD-GOAL-09**（P1/M6C/单元，**2026-09-03 目标模式 --debug 中间过程可观测**）：
  `uws "目标" --debug`（顶层 chat / goal 委派均透传）逐轮把中间过程打到 stderr——
  ① 快照：URL + AXTree 行数/ref 数 + 每轮截图路径（debug 时才落盘，
  `goal-r<N>-*.png`）；② VLM：模型/端点、原始 content/usage（default_vlm_ask
  debug=True 时打印并随决策附 `_raw/_usage`）、解析后决策 JSON；③ 动作：
  node/文本/值、引擎成功/失败原因/结果 URL、goto/护栏/换决策（重复与停滞提示）
  全过程；stdout 报告契约不变（--output 仍只存报告副本）。实现：
  `coordinator/goal.py`（run_goal debug 参数 + dbg 实时打印 + _save_screenshot
  tag）+ `coordinator/cli.py` goal --debug + `cmd/uws/chat.go` parse/委派透传。
  验收：test_goal.py `test_goal_debug_flag_logs_intermediate_rounds` +
  `test_default_vlm_ask_debug_exposes_raw_and_usage`（debug 输出/截图落盘/
  raw+usage 断言）+ Go `TestParseChatArgsDebugFlag`。
- [x] **TDD-GOAL-10**（P0/M6C/单元，**2026-09-03 input 重复→确定性提交 + 无候选熔断**）：
  同 URL + 输入框文本 + 同值（不依赖 node_id/全树签名，动态页 ref 漂移不受影响）
  的连续 input 视为重复——VLM 只填框不提交的根因修正：有唯一「搜索/百度一下/
  查询/Search/Go」按钮 → 自动 click（guard 照常前置，零重复 input）；无唯一候选 →
  一次换决策提示（提交按钮清单），仍重复 → 立即 FAILED 给 goto/DSL 诊断；
  SYSTEM_PROMPT 增「input 只填一次、填完必须提交」规则。实现：`coordinator/goal.py`
  `_find_auto_submit`/`_input_submit_hint`/`_input_stuck_reason` + input 重复拦截。
  验收：`test_goal_repeated_input_on_dynamic_home_auto_clicks_submit`
  （京东两棵漂移树：input→自动 click，不再空转）+ 
  `test_goal_repeated_input_without_submit_stops_with_diagnosis`
  （无候选：一次提示后熔断，input 只执行一次）。
- [x] **TDD-GOAL-11**（P1/M6C/单元，**2026-09-03 VLM 快照降面 + ref 子集签名 +
  动作后 settle**）：93KB/45k~60k token 全树 prompt 不复存在——每轮 ask 改用
  `treefmt.trim_snapshot_for_vlm`（预算 20k 字符：ref 交互行完整优先 + 正文
  标题/链接/摘要，末尾标注「已压缩」）；`_page_signature` 改可交互 ref 规范子集
  （role+文本+href 去重排序，正文/轮播噪声不再算“页面变化”）；CLI 动作后默认
  等树稳定 ≤0.8s（`UWS_GOAL_SETTLE_S` 可调，库默认 0 不影响单测）。实现：
  `coordinator/treefmt.py`（RefRow/parse_ref_rows/trim_snapshot_for_vlm）+
  `coordinator/goal.py`。验收：tests/unit 166 passed（test_treefmt.py 新增
  ref 行解析/VLM 压缩 3 例 + test_goal.py 压缩快照入 prompt 1 例）。

## 9. 性能与资源红线（基准回归）

*对应 `spec/algorithm-kernel.md` §8.2 与 `spec/security-dx.md`*

- [x] **TDD-PERF-01**（P0/M2/基准）：感知延迟：5000+ DOM 节点页面，WASM 感知 + 截断提取稳定 < 15ms（连续 100 次采样 P99）。（2026-08-20 点亮：5000 DOM 节点页面（300 交互带处理器 + 4700 普通）P99 = 11.61ms；100 交互节点 P99 = 0.63ms，见 `benchmarks/bench_probe_wasm.py`）
- [ ] **TDD-PERF-02**（P0/M4/基准）：决策延迟：1000 个提取特征，AC-BM25 匹配 + ONNX CPU 推理 < 10ms；规则命中单步端到端 < 50ms。
- [ ] **TDD-PERF-03**（P0/M7/基准）：内存：引擎空载 RSS < 200MB；WASM 线性内存 ≤ 16MB 硬限制；知识库双缓冲驻留 < 300MB。
- [ ] **TDD-PERF-04**（P0/M7/基准）：并发稳定性：并发 Target 达 `CPU_Cores × 1.5` 压测 1 小时无 OOM、无 Context 泄漏（池大小长期贴近上限触发告警）。
- [ ] **TDD-PERF-05**（P0/M5/基准）：时序正确性：压测期间 `NAVIGATED` 误判率为 0（不存在「活动连接 ≥ 2 或 DOM 仍在突变」时提前判稳）。
- [ ] **TDD-PERF-06**（P0/全程/门禁）：延迟回归门禁：在线引擎每次变更必须跑基准，P99 不得劣化超 10%，否则阻塞合并。
- [ ] **TDD-PERF-07**（P1/M7/基准）：LLM 兜底率回归：固定 1000 步黄金语料上兜底调用率 < 15%（设计目标，每次知识库版本变更复跑）。

---

## 9.1 理论盲区实证探针（research probes，非验收门禁，2026-08-28）

> 依据 `docs/evidence/research/research-竞品调研与零点线-2026-08-28.md` §State:D，在 `node_features` RPC
> 桥接与基准建设前完成的三组确定性实证。结果与判定见
> `docs/evidence/research/research-零点线-三盲区实证-2026-08-28.md`。

- [x] **TDD-PROBE-01**（LSH 拓扑连续性）：`pkg/matcher/probe_lsh_topology_test.go`——
  同构异形注入（祖先链插 1~3 层 `<div>` 包装）：无阻尼流 Hamming=6/12/12 均 > D_h=5
  （k=1 即决策雪崩，J 仍在 0.87）；拓扑阻尼流（相对深度+跳过 generic 包装+aria/data 白名单）
  J≡1/Hamming≡0 存活。属性噪声（css-* 追加/乱序）：Jaccard 平滑单调衰减（maxStep=0.20、
  R²=0.8783）但 L3 二元决策在 noise_k=1 断崖（Hamming 0→11）；排除 class 后完全免疫。
- [x] **TDD-PROBE-02**（A* h(x) 时序一致性 + Top-K）：`benchmarks/probe_astar_consistency.py`
  （真实 onnxruntime + classifier.onnx）——三帧骨架→真实渲染 var=0.2215 > 0.1（FAIL，
  需时间戳衰减）；tie-aware 期望口径 exact 同文 recall@5=100%、漂移真身 recall@5=15% ≪ 95%
  （FAIL，非精确文本全卡地板 0.0008，名义 rank 为 tie 假象）。
- [x] **TDD-PROBE-03**（判分器正交性）：`benchmarks/probe_judge_divergence.py`（seed=42，
  N=71 Erratum 集）——Malhotra vs WebTouch Evidence 判决背离率 Div=33.8% ≫ 5%（FAIL）；
  Malhotra=Miss ∩ WebTouch=Hit=16 例（22.5%）：必须物理隔离双基准，禁止直接对照吹票。
- [x] **TDD-PROBE-04**（Phase 0 Tier 1 P_base）：`benchmarks/tier1/` + `cmd/tier1/main.go`——
  500 靶点漂移回归集（5 类 × 20 语义 × 5 mutation），Golden Truth κ(PathG vs relaxed)=1.0
  合格；P_base=70%（纯启发式 70%/FH 12.5%、node_id 降级 50%/FH 50%、综合 FH 30% > 5% FAIL）；
  rename 恢复率 0%（L3 把漂移文本判结构正交）；LSH 离线召回上限 83.2%（rename 子集 16%）
  → 不否决 LSH 但需结构通道；ΔP_min 公式按实测重定标为 P_new ≥ 85% + FH < 5%。
- [x] **TDD-PROBE-05**（Tier 1.5 表征修复）：`pkg/matcher/structhash.go`（阻尼 DOM 拓扑
  哈希 + L3 双通道）+ `train_onnx.py` drift_templates 增广 + `cmd/tier1` 三轨 runner——
  rename 100%（≥75% PASS）、var(C) 0.0984→0.0000（≤0.05 PASS）、综合 FH 30%→0%（<5% PASS）、
  完整引擎（L1→双通道 L3→ONNX 重排→身份门禁）**P_new=90% ≥ 85% PASS** → 解锁 Tier 2 前置。
- [x] **TDD-PROBE-06**（Tier 1.5 接线，2026-08-28 结构特征位图编解码）：`pkg/matcher/structcodec.go`
  ——`PackStructFeature`/`UnpackStructFeature` 产出恒 ≤64B、纯二进制（无 JSON/明文字符串）、
  确定性命中同构；词表外身份降为无约束（HasIdentity=false），满足 [State:D] RPC 通道硬约束。
- [x] **TDD-PROBE-07**（Tier 1.5 接线，2026-08-28 双通道 L3 + A* 拓扑剪枝门）：
  `pkg/matcher/simhash.go`——`RefineByStructureDual`（文本 OR 结构[且 role 兼容]）在规则携带
  StructHash 时自动升级；`TopologicalPruneGate`（D_h ≤ 软阈值 10 才进入 ONNX 重排区，无拓扑
  签名候选被排除）；`cmd/tier1` 重排前置拓扑门，P_new_onnx 由 90.0% → 91.1%，FH=0%。
- [x] **TDD-PROBE-08**（Tier 1.5 接线，2026-08-28 规则级 StructHash 全链路 + 引擎双通道接线）：
  `pkg/matcher/rule.go`（schema/查询/ReplaceAll 读写 struct_hash，含旧库幂等 ALTER 迁移）+
  `knowledge.RuleSnapshot.StructHash`/`httpfetcher`（artifact → 快照装载）+ `engine/knowledge.go`
  matcherSnapshot 映射 + `matcher.StructuralReference`（引擎在候选无 Matches 时从 KB 规则库显式取
  参考）+ `engine/service.go pageCandidatePool`（有参考走 `RefineByStructureDual`，否则文本单通道）；
  蒸馏侧 `pipeline/component_gene.py`（语义 → 结构签名校准器，`structural_simhash64` 与 Go 逐位对拍）
  + `exporter`/`distiller` 持久化 struct_hash。自证 `TestMatch05RuleStructHashDualChannelIntegrationGRPC`
  （rename 文本被结构通道救回，结构远隔候选 NOT_FOUND）。
- [x] **TDD-PROBE-09**（Tier 2 动态沙盒，2026-08-28 [State:D] 四维对抗约束矩阵）：`benchmarks/tier2/`
  ——维度0 时间系综采样（`temporal_ensemble.py`：DOMContentLoaded/Load/NetworkIdle 三断点页内自动
  探针，H(t0)≡H(t+Δ1)≡H(t+Δ2) 不变量检测；drifting 页检出 `88c27245→89886041` 漂移）+ `ComponentGene.
  load_reference` 固化规则级 StructHash；维度1 Hydration 抖动（`run_tier2.py`+`sandbox.py`：
  `Emulation.setCPUThrottlingRate` 1×/4×/6× + 网络延迟 50/150/300ms，18 用例生还 100%，rename
  `text_d=28/31`（文本通道失败）`struct_d=0`（结构通道救回）`dual_channel_fired=True`，定位 P99=0.41ms，
  候选峰值 2 无爆炸）；维度2 跨域内存屏障（`window.location.replace` 另一 origin，`url_changed=True` +
  hit_element 同步落盘，3 trials 丢失率 0%）；维度3 κ 校准（`malhotra_judge.kappa`，WebTouch 时间态 vs
  Malhotra 静态真值 κ=1.0000 ≥0.85）。纯逻辑单测 `tests/unit/test_tier2_temporal.py` 5 例。
  Go runner：`pkg/browser/tier2.go` + `cmd/tier2`（真实 CDP 同链路，修正 P99=nearest-rank、
  κ 全同标签标记 undefined），单测 `pkg/browser/tier2_test.go`。

## 9.2 WSTN S3B6 离线内核门禁（2026-09-14）

- [x] **TDD-WSTN-01**（P0/M2/单元+契约）：`StateNode`/`ActionEdge` 图可确定性 JSON 往返，
  `wstn.route.v1` 请求/结果顶层字段冻结，未知目标与不可达目标返回 `FALLBACK`，非有限势能序列化为
  JSON `null`；主归属 `pipeline/wstn.py`、`spec/wstn-routing-v1.schema.json`、`tests/unit/test_wstn.py`。
- [x] **TDD-WSTN-02**（P0/M2/基准）：200 节点/500 边、30% 噪声、五条隐藏黄金链路、10 seeds
  共 50 对用例，S3B6 全跳召回 100%（阈值 ≥90%）；明确单分支贪心对照 0%，McNemar 双侧精确
  `p=1.78e-15`。对照不是历史 A*。
- [x] **TDD-WSTN-03**（P0/M2/性能+自愈）：10,000 次单线程寻径 P99=1.871ms（阈值 ≤15ms）；
  主路径边湮灭后下一次查询切换至备份路径，重复状态数 0。
- [x] **TDD-WSTN-04**（P0/M2/学习）：50 轮解耦信用后黄金决策边权重 2.000→2.750，公共单向走廊
  权重增量为 0；全信用污染对照走廊 1.000→1.750。证据与复现入口见
  `benchmarks/reports/EVAL-wstn-s3b6-verification.md`、`benchmarks/bench_wstn_s3b6.py`。
- [x] **TDD-WSTN-05**（P0/M2/页面状态 ROC）：`pipeline/wstn_state.py` +
  `benchmarks/bench_wstn_state_pipeline.py`——8 状态 × 12 DOM 变体，校准/holdout 各 1,128 对；
  校准独立选得 `D_th=6`，holdout 跨页误合并率 0%、同页拆裂率 0%（两侧均 <5%）。
- [x] **TDD-WSTN-06**（P0/M2/因果录制内核）：`CausalWSTNRecorder` 对 5 步离线 Mock journey
  生成 6 状态/5 边连续 WSTN；无状态跃迁动作拒绝写边，证据只保存哈希与计数、不保存 HTML/原文。
- [ ] **TDD-WSTN-07**（P0/M2/真实 CDP 因果录制）：在明确浏览器许可后，用本地 Mock 站经 CDP
  录制 5 步动作前后 DOM，并与离线 recorder JSON 对拍；当前 `NOT_RUN`，不得以 fixture 冒充。

## 10. 测试矩阵总览

| 章节 | 编号范围 | 模块 | 里程碑 | 测试类型 |
| --- | --- | --- | --- | --- |
| §1 | TDD-WASM-01..11 | `probe_wasm` | M2 | 单元/性能/内存 |
| §2 | TDD-MATCH-01..06、TDD-ONNX-01..03 | `matcher`/`classifier` | M3/M4 | 单元/集成/安全/性能 |
| §3 | TDD-ENG-01..13、TDD-KB-01..07 | `browser_agent`/`async_verifier` | M3/M5/M6A/M7 | 集成/E2E |
| §4 | TDD-COORD-01..07、09（08 已移交 search-engine）、TDD-PLAN-01..07 | `coordinator` | M6A/M6C/M7 | 单元/集成/E2E |
| §5 | TDD-SEC-01..06 | 全系统 | M4/M7 | 安全/模糊 |
| §6 | TDD-PIPE-01..06、TDD-LEARN-01..03、TDD-WATCH-01..02 | `pipeline`/`learning` | M2/M3/M5 | 单元/集成 |
| §7 | TDD-API-01..07、TDD-ADAPTER-01..02 | `proto`/网关/适配层 | M1/M6A | 契约/集成/安全 |
| §8 | TDD-CLI-01..07（03/08-12 已移交 search-engine）、13..25 | `cmd/uws` + `coordinator/attach.py` | M6A/M6C | E2E/单元/安全 |
| §9 | TDD-PERF-01..07 | 全系统 | M2/M4/M5/M7 | 基准/门禁 |
| §9.1 | TDD-PROBE-01..09 | matcher/classifier/基准/tier1+tier2 | 2026-08-28 | research probes + Phase 0 基准 + Tier 1.5 + Tier 2 动态（非门禁） |
| §9.2 | TDD-WSTN-01..07 | `pipeline/wstn*.py` + WSTN 基准 | M2 | 单元/契约/性能/ROC/因果录制门禁 |

## 附录 A：关键阈值速查

| 阈值 | 数值 | 出处 |
| --- | --- | --- |
| WASM 感知延迟 | < 15ms | algorithm-kernel §5 / TDD-WASM-01 |
| 单步命中延迟 | < 50ms | README / TDD-PERF-02 |
| 匹配+打分（1000 特征） | < 10ms | M4 验收 / TDD-PERF-02 |
| WSTN 200 节点/500 边单线程寻径 P99 | ≤ 15ms | WSTN 任务规范 / TDD-WSTN-03 |
| WASM 线性内存 | 16MB 硬上限 | algorithm-kernel §5 |
| 引擎空载内存 | < 200MB | 发布检查项 / TDD-PERF-03 |
| 知识库双缓冲驻留 | < 300MB | knowledge-versioning §3.2 |
| θ_high / θ_low | 0.85 / 0.50 | algorithm-kernel §5.1 |
| ε 探索概率 | 0.02 | algorithm-kernel §5.1 |
| 同义词 TENTATIVE 权重 / 激活 | 0.2 / hit_count ≥ 5 | algorithm-kernel §4.3 |
| SimHash 汉明距离 | D_h ≤ 5 | algorithm-kernel §5 |
| A* 深度 | D_base=3，D_max=5 | algorithm-kernel §5 |
| Network Idle | 活动连接 < 2 持续 500ms | algorithm-kernel §5 |
| 幂等 TTL | 60s | proto / api-reference |
| 破解 ACK / 等待超时 | 2s / 60s，重试 1 次 | physical-patches §3.3 |
| 预算看门狗 | 80% 熔断离线 / 100% 全切 | algorithm-kernel §8.3 |
| LLM 兜底率 | < 15% | README / TDD-PERF-07 |
| 覆盖率门禁 | ≥ 80%（engine/pipeline） | contributing.md §5 |
- [x] **TDD-OPT-01**（P1/M6A/优化）：探针常驻注入（Pre-instantiation）：通过 `Page.addScriptToEvaluateOnNewDocument` 注入 WASM 模块，单次多信号抽取稳定 `< 5ms`。（2026-08-20 点亮：文档级预脚本在每个新文档解析前异步实例化探针（`window.__uwsProbe` 常驻），`cdpSession.PageCandidates` 增加同文档候选缓存（导航/已采样 DOM 突变/2s TTL 失效）——爬虫/协调器同一页面连续抽取多信号共享一次探针采集；真实 Chrome `TestCDPSessionProbeResidentAndCached` 全绿（常驻探针就绪/缓存命中毫秒级/突变失效重采），`BenchmarkCandidateCacheHit` 实测 6.4µs/次 ≪ 5ms；顺带修复真实 bug：`sampleMutationsOnce` 用字符串解码数字导致 DOM 突变信号在真实 CDP 静默失效）
- [ ] **TDD-CASE-01**（P0/M6A/真实用例）：Medium 真实博客实测：正文/作者/头像/评论结构化提取 + 广告与登录拦截零残留。
