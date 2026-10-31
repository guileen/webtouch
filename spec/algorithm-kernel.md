# WebTouch(webtouch) 浏览器智能义肢系统内核设计文档（V1.4 — 合并冻结版）

*状态：自洽冻结，可编码。生效日期：2026-08-19。*

---

## 1. 项目概述

本方案构建一套**低成本、自进化、高确定性**的浏览器自动化智能体。核心思想：将LLM的“智能”蒸馏为传统规则引擎的“配置数据”（词典、模板、模型），并通过离线学习不断固化，使在线执行环以极低计算成本（CPU单核、内存<200MB）完成绝大多数网页操作任务，仅在罕见情况下调用LLM兜底。

系统由两个环组成：

- **在线执行环（实时）**：加载知识库，对浏览器AXTree进行匹配与决策，执行原子动作，并返回局部验证结果，由外部协调器驱动任务流程。
- **离线学习环（批量）**：从Common Crawl和在线日志中蒸馏知识，经影子评估后注入知识库。

知识库分两层：

- **L1 通用语义知识库（WebTouch）**：从Common Crawl离线蒸馏，辅以组件库先验基因注入，提供跨站点的通用网页语义理解。
- **L2 站点特化知识库**：从在线低置信度案例中学习，针对特定站点持续进化。

**物理执行层嵌入**：在线探针采用预编译WASM模块（16MB线性内存）注入V8上下文，以C++级速度遍历Fiber树并提取降噪特征，单次感知延迟从O(n)降至近似O(1)（≤15ms）。

**对外接口**：本系统对外暴露原子动作执行接口（gRPC），由外部协调器负责任务解析、状态机管理和宏观成功判定。WebTouch仅负责定位节点、执行动作、局部验证与日志记录。

---

## 2. 系统架构图

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        离线蒸馏流水线（周期性）                              │
│                                                                             │
│  Common Crawl WARC/WET → 页面过滤 → 结构特征提取 → LLM批量蒸馏              │
│        ↓  (拓扑去重/域名熔断)                                               │
│  规则聚合/冲突消解（哈希分片流式归约）→ WebTouch导出（SQLite + ONNX）             │
│                                                                             │
│  组件库基因注入（Storybook/Playground）→ 标准组件AXTree解析 → 高置信基线规则  │
│        ↓   （常驻AC L1白名单，置信度0.95，ONNX复核豁免）                    │
│  在线日志 → 低置信度案例分析 → LLM站点蒸馏 → 影子评估（动态贝叶斯）→ L2注入  │
│        ↓   （神经符号蒸馏：new_synonyms TENTATIVE态入库）                   │
└─────────────────────────────────────────────────────────────────────────────┘
                          ↓ 发布（版本化镜像 + 对象存储）
┌─────────────────────────────────────────────────────────────────────────────┐
│                    在线执行引擎（浏览器Agent - gRPC服务）                    │
│                                                                             │
│  WASM探针注入（16MB Arrow Buffer）→ 全量特征提取（含AST降噪handler_hash）   │
│        ↓                                                                    │
│  L1 AC绝对匹配（核心词典）→ L2 BM25概率召回（含TENTATIVE同义词微扰动）      │
│        ↓                                                                    │
│  上下文约束细筛 → ONNX分类器打分（困难负样本训练）→ 置信度C                 │
│        ↓                                                                    │
│  C ≥ θ_high → 直接执行（含Fitts动力学曲线 + Z轴遮挡检测）                  │
│  C < θ_low  → LLM兜底 + 日志（受预算看门狗管控）                          │
│  中间区间   → 执行 + ε探索（或触发A*多步搜索，深度≤5）                    │
│        ↓                                                                    │
│  动作执行 → 异步局部验证（History API监听SPA + Network Idle交叉验证）      │
│        ↓  （状态机：STABLE / NAVIGATING / NAVIGATED / NEW_WINDOW_OPENED）  │
│  日志存储（熵控采样 + PII自动脱敏 + zstd压缩 + 内外存分离）                │
└─────────────────────────────────────────────────────────────────────────────┘
                          ↑ gRPC（含InitializeContext/ReloadKnowledge）
┌─────────────────────────────────────────────────────────────────────────────┐
│                    外部协调器（Coordinator - 独立进程）                      │
│  - 解析用户指令为原子动作序列（模板驱动 + LLM辅助）                         │
│  - 维护任务状态机、重试、分支、跨步骤记忆                                   │
│  - 判断宏观任务完成、处理Crash恢复                                          │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 3. 模块划分与职责

| 模块 | 职责 | 输入 | 输出 |
|------|------|------|------|
| `cc_fetcher` | 从Common Crawl获取原始HTML样本，含拓扑去重与域名熔断 | 域名/语言/数量 | WARC记录或HTML |
| `component_gene_injector` | 从组件库演示站提取标准AXTree，生成高置信基线规则，常驻L1白名单 | 组件库URL列表 | 先验规则（Confidence=0.95） |
| `wasm_feature_probe` | 预编译WASM探针（16MB Arrow Buffer），注入V8提取特征及AST降噪哈希 | CDP会话 | `NodeFeature`列表（二进制序列化） |
| `llm_distiller` | 批量调用LLM生成通用规则和神经符号（强制结构化输出） | 特征批次 | `DistilledRule`列表 + `SiteSynonym`（TENTATIVE） |
| `aggregator` | 规则去重、聚合、冲突消解（哈希分片，内存<2GB） | 规则列表 | 聚合规则 |
| `uws_exporter` | 导出SQLite词典和ONNX模型（困难负样本训练），版本化并上传对象存储 | 聚合规则/训练集 | WebTouch Artifact（SQLite + ONNX + 版本清单） |
| `browser_agent` | 在线执行：感知（WASM）、匹配（AC-BM25）、决策、动作执行、异步验证、日志PII脱敏 | 原子意图（gRPC） | 动作结果（含context_state） |
| `online_learner` | 处理低置信度日志，生成L2规则及同义词（TENTATIVE→ACTIVE状态机） | 日志 | L2规则条目 + 同义词状态更新 |
| `shadow_eval` | 影子评估L2候选规则（动态共轭先验：拓扑孤岛/高危区域差异化阈值） | 候选规则 + 历史日志快照 | 毕业/淘汰/继续观察决策 |
| `async_verifier` | 动作后监听DOM变化、导航、新窗口。含SPA软路由劫持（History API）和Network Idle交叉验证 | CDP会话 + 目标节点ID | 验证结果（含NAVIGATING中间态） |
| `snapshot_store` | PII自动脱敏、压缩并存储页面快照（zstd），内外存分离 | 快照JSON | 压缩BLOB或URI |

---

## 4. 数据模型（合并冻结版）

### 4.1 特征结构 `NodeFeature`

```python
@dataclass
class NodeFeature:
    # 基础标识
    tag: str                # HTML标签
    role: str               # 映射后的AX角色
    # 文本语义
    semantic_text: str      # 自身文本/aria-label/value
    parent_semantic_text: str
    zonal_keywords: list    # 最近3层关键词（已去停用词）
    # 结构特征
    parent_tag: str
    parent_role: str
    in_form: bool
    input_type: str
    dom_depth: int
    is_leaf: bool
    # 剥离后的稳定属性
    attributes: dict        # 仅保留aria-*、data-*等
    # ★【V1.4修订】物理行为签名（AST降噪哈希）
    ast_semantic_hash: str  # 剥离数字/字符串字面量后的操作符+API调用序列的LSH
    cdp_script_id: str      # 降级用
    # 复合键（用于AC粗筛）
    composite_key: str      # 形如 "[ROLE:button][TEXT:提交]"
    # 原始信息（仅蒸馏用）
    xpath: str
    class_names: list
    id: str
    page_title: str
    page_url: str
```

**AST降噪哈希算法**（取代原`handler_hash`）：
- 提取函数源码后，正则剔除所有数字字面量、字符串字面量及十六进制内存地址。
- 仅保留操作符类型（`&&`、`||`、`=>`）、DOM API调用特征（`fetch`、`preventDefault`）及关键字序列。
- 对降噪后的字符串计算局部敏感哈希（LSH），确保两次不同会话中同一业务逻辑的哈希碰撞率>95%。

### 4.2 核心规则表 `uws_rules`（SQLite）

```sql
CREATE TABLE uws_rules (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_type       TEXT NOT NULL,          -- 'text_pattern' | 'selector' | 'combined'
    text_pattern    TEXT,                    -- 纯字面量（短词），参与AC-BM25索引
    selector        TEXT,
    role            TEXT,
    action          TEXT NOT NULL,
    semantic_label  TEXT,
    confidence      REAL NOT NULL,
    parent_role_constraint TEXT,
    required_zonal_keyword TEXT,
    in_form_required INTEGER,
    source          TEXT,                    -- 'llm_distill' | 'online_learn' | 'component_gene'
    status          TEXT,                    -- 'active' | 'staging' | 'deprecated'
    ttl             INTEGER,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_uws_rules_text ON uws_rules(text_pattern);
```

### 4.3 ★【V1.4新增】站点同义词表（神经符号蒸馏防投毒）

```sql
CREATE TABLE site_synonyms (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    site_domain     TEXT NOT NULL,
    synonym         TEXT NOT NULL,            -- 如 "Sign in"
    base_weight     REAL DEFAULT 0.2,        -- 初始极低权重，限制破坏力
    status          TEXT DEFAULT 'TENTATIVE', -- 'TENTATIVE' | 'ACTIVE' | 'POISONED'
    hit_count       INTEGER DEFAULT 0,        -- 在线被BM25命中且任务最终成功的次数
    source_rule_id  INTEGER,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_synonym_domain ON site_synonyms(site_domain);
```
**状态机**：TENTATIVE权重仅做微小扰动（增益0.2）；`hit_count >= 5` 后跃迁为ACTIVE（权重升至1.0）；若导致频繁兜底则标记POISONED，物理隔离。

### 4.4 在线日志表 `online_logs`（含PII脱敏）

```sql
CREATE TABLE online_logs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id      TEXT NOT NULL,
    site_domain     TEXT,
    page_url        TEXT,
    ax_tree_hash    TEXT,
    node_features   TEXT,                    -- 候选节点特征JSON（PII已脱敏）
    snapshot_blob   BLOB,                    -- zstd压缩快照（PII已脱敏）
    snapshot_uri    TEXT,
    snapshot_size   INTEGER,
    confidence      REAL,
    decision        TEXT,                    -- 'direct' | 'llm_fallback' | 'explore'
    action_taken    TEXT,
    success         INTEGER,
    latency_ms      INTEGER,
    context_state   TEXT,                    -- STABLE | NAVIGATING | NAVIGATED | NEW_WINDOW_OPENED | CHALLENGE_INTERCEPTED
    decision_trace  TEXT,                    -- JSON: 含规则ID、BM25分、AST哈希匹配
    pii_masked      BOOLEAN DEFAULT 1,      -- ★强制脱敏标记
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);
```
**PII脱敏强制策略**：对 `input[type="password"]`、`textarea`、含 `pwd/pass/ssn/phone` 等字段名的节点，其 `value` 和 `semantic_text` 在存入前全部替换为 `[REDACTED]`；快照中对应文本节点执行相同操作。

### 4.5 版本与对象存储表

```sql
CREATE TABLE knowledge_versions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    version      TEXT NOT NULL,
    schema_hash  TEXT NOT NULL,
    model_hash   TEXT NOT NULL,
    artifact_uri TEXT NOT NULL,              -- s3://...
    description  TEXT,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    rollback_to  INTEGER
);
```

---

## 5. 算法选型与参数锁定（合并冻结版）

| 组件 | 选型 | 关键参数 |
|------|------|----------|
| **文本匹配（L1粗筛）** | **AC自动机（核心短词）** | 仅匹配经停用词过滤后的2~4字核心Token。命中即入候选池。 |
| **文本匹配（L2概率召回）** | **BM25倒排索引** | 对L1漏判节点，在视口内构建微型内存倒排索引，TF-IDF变体评分，彻底解决词缀变化与乱序。 |
| **结构相似度** | **SimHash 64-bit** | 汉明距离阈值D_h ≤ 5，用于L3物理校验。 |
| **AST语义哈希** | **局部敏感哈希（LSH）** | 剥离动态字面量后计算，容忍闭包环境差异。 |
| **分类器** | **LightGBM → ONNX** | 训练集强制采用**困难负样本挖掘**（正样本兄弟节点 + SimHash最近邻3个干扰节点）。`num_leaves=31`，模型<1MB。 |
| **多步搜索（A*）** | **深度钳制 + 非线性惩罚** | • 基础深度D_base=3；含流程标志（step/checkout）补偿+2；绝对上限D_max=5。<br>• 物理湮灭（HTTP 4XX/5XX）：Penalty=+∞，节点入AC黑名单。<br>• 逻辑偏移：Penalty = C_base × [1 + (1/SimHash_Overlap)]²，祖父回溯连坐。 |
| **页面快照压缩** | **zstd 级别3 + PII脱敏** | <256KB存SQLite BLOB，≥256KB存外部存储。 |
| **异步验证** | **MutationObserver + History API劫持 + Network Idle** | 防抖150ms，总超时1500ms。<br>**SPA检测**：监听`History.navigated`（CDP `History.enable`）及`window.pushState`自定义绑定。一旦捕获，状态先切`NAVIGATING`，等待Network Idle（活动连接<2且持续500ms）及DOM突变衰减后，再切`NAVIGATED`。 |
| **WASM探针** | **C++ → WASM（单例注入）** | 线性内存硬上限16MB Arrow Buffer。溢出时按BFS截断低权重节点，触发`uws_wasm_truncation_total`指标，并降级至JS慢速遍历（超时200ms）。 |
| **构建工具** | **uv + Makefile + Docker** | 同V1.3。 |

### 5.0 WSTN 路由契约（M2 离线冻结，尚未替换生产 A*）

WSTN 的跨语言消费边界以 [`wstn-routing-v1.schema.json`](wstn-routing-v1.schema.json) 为唯一机器事实源：
上游必须解析出精确的 `current_state_id` 与 `target_state_id`，路由器返回 `FOUND|FALLBACK`、有序动作与
审计证据。`wstn.route.v1` 顶层字段冻结，实验性数据只能进入 `extensions`；任何顶层字段或语义变化必须
发布新 Schema 版本。Python `pipeline/wstn.py` 是 Tier 0 参考内核，不是消费接口；Go 执行器可立即按
Schema 并行开发，但在 Tier 1/Tier 2 与运行时接线门禁完成前，现有 `pkg/matcher/astar.go` 仍是生产路径。

页面状态离散化由 `pipeline/wstn_state.py` 提供：对 DOM 语义地标、可访问性角色、表单类型、链接、
可见文本 shingles 与归一化路由编码 64-bit SimHash；忽略 CSS class、元素 id、generic wrapper、查询串、
属性顺序和纯数字动态文本。2026-09-14 受控页面 pair 基准由校准集独立选得 `D_th=6`，holdout 跨页
误合并率 0%、同页拆裂率 0%；该阈值尚未被真实站点分布验证。`CausalWSTNRecorder` 只在动作前后跨越
该状态边界时生成边，并仅保存哈希/计数证据。真实 CDP 因果录制与生产 A* 替换仍未完成。

### 5.0.1 分类器特征契约（2026-08-20 追加，与 pipeline/exporter/train_onnx.py 对齐）

ONNX 分类器输入为 **5 维 float32 特征**（候选节点文本 × 目标语义 配对），输出二分类
概率（class 1 = 语义命中）。特征定义（追加式，不修改既有冻结参数）：

| 序号 | 特征 | 计算 |
|---|---|---|
| f0 | simhash_sim | 1 − 汉明距(SimHash64(候选), SimHash64(目标)) / 64 |
| f1 | jaccard | 候选与目标 token 集合 Jaccard 相似度（token 规则同 `topology.py`） |
| f2 | exact_match | 归一化小写完全相等（0/1） |
| f3 | substring | 目标语义是候选子串或候选是目标子串（0/1） |
| f4 | log_len | log1p(候选长度)/8，封顶 1.0 |

训练约束保持冻结：`num_leaves=31`、模型 <1MB、CPU 单次推理 ≤5ms（TDD-ONNX-02）；
训练集强制困难负样本（兄弟节点 + SimHash 最近邻 3 干扰，TDD-ONNX-03 黄金集 AUC
≥0.90）。ONNX 导出由 `dump_model()` 手工构建 TreeEnsembleClassifier
（ai.onnx.ml opset 3，`post_transform=LOGISTIC`），导出时逐样本 parity 校验
（LightGBM vs onnxruntime，容差 1e-3）。

### 5.0.2 ★【2026-09-24 追加】语义标签归一化契约（定位层 `0/12` 的根因修复）

**背景（机制事实）**：`target_semantic` 是**内部控制面标识**（协调器 DSL 宏展开出的
`search_box` / `search_button`，以及 `username_input` / `submit_btn` 一类词表标签），
页面 DOM 上**不存在**这些字符串。物理层定位（`findElementMode` 的字面打分）曾把该串
直接拿去与页面文案比对 ⇒ 等价于「用内部编号搜页面文本」，真实站点上与目标互不匹配。
12 站实采 `input:search_box` 命中正确搜索框 **0/12** 是这条的必然结果
（证据：`docs/evidence/pilots/site-coverage-2026-09-24.md` §4/§6）。

**条款（追加式；不修改 §4.2/§4.3 既有表结构与冻结参数）**：

1. **归一化是派发前的强制步骤**：任何把 `target_semantic` 送进页面字面比对的路径，
   必须先把语义展开为**候选文案集合** `C(s) = [c1..cn]`（有序，`c1` 为最通用的页面文案）。
   内部标签**不得**再作为 `want` 直接参与字面比对。
2. **多候选取最高分**：`score(el) = max over c ∈ C(s) of tier(el, c)`；三档保持不变——
   `100` 元素文本 == 候选、`80` 元素文本包含候选、`60` 候选包含元素文本。
   比较**大小写不敏感**（页面文案大小写不可控，不由标注方决定）。
3. **碎片档设下限**：`60` 档要求元素文本长度 ≥ 2 个字符（禁止单字符碎片命中）。
   理由：候选集合变多会等比放大 `60` 档的假阳性面；单字符元素（`×`、`»`）不可能是
   目标语义本身。
4. **不依赖站点知识**：`C(s)` 的唯一来源是**通用语义别名表**（跨站点内置资产）。
   站点规则 / 同义词（§4.2 / §4.3）只做叠加与提精度，**不得**作为本条款的前置条件——
   否则必然退化为逐站手写规则，与二八定律相反。
5. **未覆盖即原样**：别名表未覆盖的语义标签保持**单候选（= 原串）**行为，零变化
   （fail-closed，不做启发式猜测）。
6. **可观测**：多候选命中时必须能区分「实际命中的候选」与元素文本（物理层日志 /
   命中字段回显），不得只回一个笼统的成功。

**归属**：别名表与展开函数归**物理层**（字面比对的发生地）；引擎侧 `RulesBySemantic`
仍按**原始语义**取规则，不受归一化影响（防规则通道被改写）。

**验收（先红后绿）**：

- 纯函数：`expand("search_box")` 返回有序候选集且含中文与英文通用文案；未覆盖标签原样单候选。
- 定位（真实浏览器 + 本地页）：页面提供 `placeholder="Search Amazon"` 的输入框时，
  `search_box` 必须命中该 `input`（改动前必红）。
- 真机（判定口径）：`docs/system-map-2026-09-23.md` §9.6 的 12 站复跑，
  `input:search_box=webtouch` 命中**正确输入框**的比例（**基线 0/12**）。
- 回归：条款 3 的下限不得破坏既有宽泛点击语义用例（`第 4 集` / `描述` 两条既有测试仍绿）。

**边界（诚实）**：「别名展开后 12 站能命中几个」在实测前是**未验证预期**；本条只承诺
「机制不再必然为 0」，不承诺命中率。本条也只修「找得到」——「找错元素仍报 `SUCCEEDED`」
（案卷 §4.2 缺陷①）由后验门禁另立条款，本条不覆盖。

### 5.0.3 ★【2026-09-25 追加】`input` 后验门禁契约（假成功 `SUCCEEDED` 的根因修复）

**背景（机制事实）**：`input` 的落地自检曾只判「目标元素自身文本非空」（`inputLanded`：
`el.innerText || el.value` 非空即通过）。当定位命中**非可编辑元素**（`span` / `div` /
`button` / `i`）时，该元素的原生文案本身就非空 ⇒ 自检必然通过、物理层回报
`SUCCEEDED`，而输入值只是被 `Input.insertText` 打进了当时恰好持有焦点的别处或直接丢弃。
12 站实采里 `github.com` 的 `input:search_box` 命中 `span` «Sign in»（营销头部的
`button` «Search» 及其内层 `span`，页面当时**根本没有**可编辑搜索框）仍报成功；
引擎日志同轮 `uws locate` 另见 `tag=span/i/div/button`（`Search`、`搜尋`、`语音搜索`）
等不可编辑命中（证据：`docs/evidence/pilots/site-coverage-2026-09-24.md` §4.2/§8）。

**条款（追加式；不修改 §5.0.2 与 §4.2/§4.3 既有表结构，也不改任何冻结参数）**：

1. **可编辑性门禁（fail-closed；只裁文本汇支路）**：`input` / `fill`，以及 `select` 的
  原生 `<select>` 分支，其目标**必须**是可编辑元素——文本类 `input`（排除 `hidden`/
  `button`/`submit`/`reset`/`image`/`checkbox`/`radio`/`file`/`range`/`color`，且非
  `disabled`/`readOnly`）、`textarea`、`select`、`[contenteditable]`、`role=textbox`。
  命中非可编辑元素时**不得**以任何「值进了页面别处」的形式记成功；该次尝试算未命中，
  重试耗尽后明确失败。**支路边界**：`select` 作用于**自定义下拉触发器**时目标是菜单
  触发器、成功判据是「打开菜单并点到选项」（2026-08-31 Airwallex 条款），该支路保持
  原行为，不由本条裁定（避免无证据的行为回归）。
2. **可编辑候选内重选**：定位层先按 §5.0.2 正常择优；若赢家不可编辑，则在**可编辑
   候选**（语义分 > 0）内重选最高分者作为目标（平分仍按面积小 / DOM 序的既有平局
   规则）。**不得**在可编辑候选中引入语义分 0 的元素——宁可失败，不赌「页面上唯一
   的输入框」（延续 §5.0.2 条款 5 的 fail-closed 口径）。
3. **失败原因分三态**：`no_match`（页面无任何语义闭包）/ `input_not_editable`（有语义
   闭包但无可编辑候选，须回显被拒元素的 `tag`/`text`）/ `input_not_landed`（元素合法但
   值没进去）。三者不得混成一句笼统失败。
4. **落地判据是「值真的在元素里」**：输入后从**同一元素**回读可编辑内容（`input`/
   `textarea`/`select` 读 `value`，`contenteditable` 读 `innerText`），判定回读值
   **包含**输入值（空白折叠 + 大小写不敏感；再退化到「忽略非字母数字字符」比较，兼容
   `13000000000 → 130 0000 0000` 一类格式化）。元素原生文案非空**不得**作为落地证据。
   自定义下拉支路（条款 1 支路边界）例外：仍按「回读非空」判定，判据改动只随文本汇支路。
5. **重试不得叠写**：同一动作内多次尝试必须幂等——重新写入前让选区覆盖已有内容
   （`input`/`textarea` 用 `select()`；`contenteditable` 在重试时先全选）；结果值中
   同一输入值不得重复拼接（ddg 实测 `webtouchwebtouch` 即此缺陷的产物）。
6. **只作用于输入通道**：门禁只改 `input`/`fill`/`select`；`click` 的宽泛语义
   （`第 4 集` / `描述`）与 `extract` 的严格模式（§5.0.2 之前的 2026-09-23 条款）行为
   **保持不变**。

**归属**：可编辑性判定与可编辑候选重选归**定位层**（`findElementMode` 的
`editableOnly` 分支）；派发前二次校验、落地回读与幂等写入归**派发层**
（`inputAt` / `inputWithRecovery` / `inputLanded`）。

**验收（先红后绿）**：

- 纯函数：`inputValueLanded` 对 `("webtouch","webtouch")` 真、`("","webtouch")` 假、
  `("130 0000 0000","13000000000")` 真（格式化退化档）。
- 页面侧（本地页 + 真实 headless Chrome）：页面**只有** `<button>Search</button>`
  （无可编辑搜索目标）时 `input:search_box=x` 必须**失败**且原因含 `input_not_editable`
  （改动前必红：旧行为回报成功）；页面有 `<input placeholder="Search">` 时必须成功，
  且该输入框 `value` 恰为写入值（不得叠写）；`contenteditable aria-label="Search"`
  的富文本编辑器必须仍被接受（召回保真）。
- 回归：`TestCDPSessionRealChrome` 的 `input` 用例（`hello-uws` → `#user.value`）仍绿；
  §5.0.2 的 search_box 三例与 `findElementOpt` 既有语义用例行为不变。
- 真机（判定口径）：12 站复跑，`github.com` 那格不得再报 `SUCCEEDED`；`google` /
  `wikipedia` / `duckduckgo` 三站真命中不得退化（wikipedia 的搜索框自带
  `placeholder`/`aria-label="Search Wikipedia"`，静态页取证已确认可编辑候选存在）。

**边界（诚实）**：本条只修「输入落到不可编辑元素 / 值没进去仍报成功」，**不**提高命中率，
也**不**解决「词对得很准但元素类型错」（百度 `div` «语音搜索» 被拒后仍 `unknown_error`）
——那属候选召回与角色感知，另立条款。另外：若某站的真实输入框**不带任何可打分的文本源**
（无 `aria-label`/`placeholder`/`title`/`data-testid`/可见文本，且关联 `<label>` 不在评分
源内），本条会把它判为 `no_match`/`input_not_editable` 而**诚实失败**；`input[type=search]`
与 `[role=search]` 内的输入等**跨站点标准结构信号**是否作为补充候选，留待 12 站读数后
另立条款决定（先测量、后加规则）。

### 5.0.4 ★【2026-09-25 追加】角色线性效用启发式打分与比较域约束契约（消灭「微型装饰节点逆淘汰真实输入框」）

**背景（机制事实）**：
12 站实采与引擎日志揭露了「面积反向竞争」物理缺陷（案卷 `site-coverage-2026-09-24.md` §8 百度场景：`uws locate semantic="search_box" matched_want="搜索" tag=div text="语音搜索"`）：
页面内装饰性微型图标（如语音搜索 `div`、放大镜 `span`）由于包含 `"搜索"` 或别名文案，文本语义打分可达 80 分；而真实的搜索框 `<input placeholder="搜索">` 同样获得 80 分。
在分数并列时，既有逻辑执行 `if (area > bestArea) continue;`（`cdp_session.go:2798`）。该逻辑原意是剔除包裹子元素的巨大容器，却使 24×24px 的微型非目标元素系统性战胜 450×40px 的真实表单输入框。
随后触发 §5.0.3 门禁，虽成功拦截非编辑元素，但由于赢家已被微型 `div` 抢占，最终退化为 `input_not_editable` 或 `unknown_error`，无法实现真实命中。

**条款（追加式；不修改 §5.0.2/§5.0.3 既有表结构与冻结参数）**：

1. **角色线性效用加成（Role Utility Bonus）**：
   定位层在处理文本汇支路（`editableOnly=true`）时，打分模型由单纯的词法匹配升级为「基于角色的线性效用启发式」：
   $$U(e) = S_{\text{match}}(e) + W_{\text{role}}(e)$$
   若候选元素 $e$ 满足可编辑性判据 `isEditableEl(e)`（`input`、`textarea`、`contenteditable`、`role=textbox/searchbox/combobox`），赋予 $W_{\text{role}} = +20$ 的角色确定性加成（综合得分封顶 100 分）。
   非可编辑节点（`div`、`span`、`button`、`i`）$W_{\text{role}} = 0$。
   由此，文本分同为 80 分时，真实输入框（80 + 20 = 100）绝对占优，在第一轮择优中直接胜出，无需退入重选。

2. **面积决胜严格约束于祖先后代包含域（Intra-Branch Only）**：
   严禁在无父子关系的跨分支（Disjoint / Cousin）孤立节点之间进行纯几何面积对比。
   仅当新候选元素与当前最优候选存在严格包含关系（`el.contains(bestEl) || bestEl.contains(el)`）时，面积缩减策略才生效，以提取内层精确叶子；
   在跨分支且综合效用得分完全一致的情况下，若其中一方具备可编辑属性，坚决优先选择可编辑节点；若属性同质，则按文档流序（DOM Order）首选稳定收敛，不再偏袒孤立微型节点。

3. **双轨可信度评价体系（Decision Fidelity & True Success Rate）**：
   外部评审与工程验证废除虚假单一命中率，采用双轨指标度量系统可靠性：
   - **动作判决保真度（Decision Fidelity, DF）**：
     $$DF = \frac{\text{True Succeeded}}{\text{Reported Succeeded}}$$
     门禁核心战果在于将 DF 从危险的假阳性区间（75%/50%）彻底推升至 **100%**（杜绝 GitHub 这类击中 span 却谎报成功的幽灵行为）。
   - **真实命中率（True Success Rate, TSR）**：
     $$TSR = \frac{\text{True Succeeded}}{\text{Total Sessions}}$$
     作为业务覆盖度指标，在 DF=100% 的前提下稳步提升。

**归属**：
角色效用启发式打分与包含域亲缘检测完全归属**定位层**（`cdp_session.go` `findElementMode`）。

**验收（先红后绿）**：
- 页面侧（本地页 + 真实 headless Chrome）：构建高仿百度结构——一个小图标 `<div style="width:20px;height:20px">语音搜索</div>` 并列一个主搜索框 `<input placeholder="搜索百度" style="width:400px;height:40px">`。执行 `input:search_box=webtouch` 时：
  - 改动前：面积决胜偏向微型 `div`，触发门禁报错 `input_not_editable`；
  - 改动后：输入框凭借角色加成（100 > 80）稳定胜出，输入值正确落盘，返回 `SUCCEEDED`。
- 回归：既有 §5.0.2（别名归一化）、§5.0.3（输入后验门禁）测试及通用点击、抽取用例全量保持绿灯。


### 5.0.5 ★【2026-09-25 追加】几何有效性硬剪枝公理与第二物理特征平分仲裁契约（彻底闭环 Google 回归）

**背景（机制事实）**：
Google 首页等现代 SPA 在 DOM 序较前位置常埋入无渲染或隐藏的 `textarea`（如 AXTree 中 `[28] textarea [offscreen]`）。在既有打分逻辑中，若隐藏节点在进入语义打分前未做物理排他性过滤，该节点同样被赋予 §5.0.4 的可编辑角色加成（满分 100 分）；在随后的包含域检测中，由于隐藏节点与真实搜索框 `[365] textarea(combobox)` 分属不同分支容器，`if (!preferLast) continue;` 的 DOM 序偏袒直接丢弃了真实搜索框，锁死并聚焦于离屏隐藏节点，导致搜索框虽在页面呈现聚焦态却无法真实落入字符，产生确定性回归。

**条款**：
1. **几何有效性前置硬剪枝（Physical Visibility Pre-filter）**：
   定位层遍历元素时，必须在进入文本提取与 `scoreEl` 打分函数之前执行零代价物理硬拦截：
   - 若元素计算样式满足 `display === "none"`、`visibility === "hidden"` 或 `opacity === "0"`，立即剔除；
   - 若元素属于 §5.0.6 条款 1 的黑名单标签，立即剔除（与本过滤器共用同一判定）；
   - 若元素显式声明 `aria-hidden === "true"`，立即剔除；
   - 若元素布局几何满足 `getClientRects().length === 0`，立即剔除；
   - 若几何尺寸 `width < 2 || height < 2`，立即剔除；
   不可见节点绝对禁止进入打分与角色赋权，从物理根源上消灭无渲染节点对候选池的先验污染。
2. **第二物理特征平分仲裁（Euclidean Center & Rendered Area Arbitrage）**：
   废弃跨分支同质且平分时的 DOM 顺序偏袒。当两个或多个候选元素得分并列且非父子包含关系时，计算视口中心欧氏距离与可见面积的第二特征值：
   $$\text{FeatureScore}(e) = \frac{\text{Area}(e)}{1.0 + \text{Distance}(e, \text{ViewportCenter})}$$
   主搜索框在视口中通常具备显著的居中性与有效渲染面积，凭借该特征量单调优选，消除 DOM 序随机偏袒。

### 5.0.6 ★【2026-09-25 追加】布局感知文本提取与 AST 节点黑名单契约（根治 Baidu 文本源污染）

**背景（机制事实）**：
Baidu 等站点的 DOM 结构中包含内嵌的 `<style data-for="result">` 或复杂模板块。探针或定位层若通过 `textContent` 粗暴抓取节点文本，会强行将内嵌 CSS 代码与脚本原语抽取为元素文本，并在 AXTree / ref 池中生成形如 `textarea "<style data-for=...>"` 的严重文本源污染，挤占正常文本框的召回与映射。

**条款**：
1. **AST 遍历黑名单物理硬拦截**：
   在 DOM 递归遍历与探针收集入口，凡标签名属于 `STYLE`、`SCRIPT`、`NOSCRIPT`、`TEMPLATE` 的节点，其整棵子树立即停止递归下降并彻底跳过。
   - **实现缺口（2026-09-25 回写标注）**：本条款原始表述中的「非文本 `SVG`」**当前未实现**（`pkg/browser/probe.go` 与 `pkg/browser/cdp_session.go` 的黑名单均只有上述四类）；缺口已在 `Tasks.md`（`T-14` 第十一批 ③）立项，补齐前不得当作已生效约束。
2. **优先采用布局感知渲染文本**：
   在非 input/textarea 元素上提取文本时，优先读取经过 Blink 排版引擎布局计算后的 `HTMLElement.innerText`，自动剔除不可见样式与脚本内容；若非 HTMLElement 回退，严禁读取任何黑名单标签及其子节点。
3. **内联数据载体特判（2026-09-25 回写补录；2026-09-25 第二次扩写至结构化数据块）**：
   内联模板污染不止经 `STYLE` 标签传播，也不止是 CSS 文本——站点会把整段模板或**结构化数据块**塞进 `textarea.value`（本轮实测 Baidu `[227] textarea "{ "hotsearch": … }"`：`<style` 污染已在此前批次消除，污染**换了载体**，数据块 JSON 取代 CSS 文本）。因此：
   - 探针交互集判定中，`textarea` 的 `value` 经空白归一后若以 `<style` 起始（大小写不敏感），或**可解析为 JSON 对象/数组**（去空白后首字符为 `{` / `[` 且 `JSON.parse` 成功），判为**数据载体容器**而非输入控件，不进入交互候选；
   - 文本提取入口（WASM 探针 `ownText`、溢出补偿遍历、`collectJSCandidates` 降级路径）对交互元素的取值，凡含 `<style` 子串或判为 JSON 数据载体者**整体丢弃**，不得进入语义文本源；
   - 判定必须**先解析后丢弃**（`JSON.parse` 成功才丢弃），避免把用户正在输入、恰好以 `{` 起始的普通查询误判为数据载体；
   - `<script type="application/json">`、`application/ld+json`、`<template>` 一类内联元数据**已由条款 1 的标签黑名单整棵子树剔除**（`STYLE` / `SCRIPT` / `NOSCRIPT` / `TEMPLATE`），本项仅明确其覆盖范围，不新增标签豁免。

### 5.0.7 ★【2026-09-25 追加】装饰叶子节点向承载容器反向穿透契约（Container Escalation · 消除 Wikipedia VLM 延迟）

**背景（机制事实）**：
Wikipedia 搜索场景中，页面将放大镜图标 `i "Search"` 与搜索框复合排列。定位层首轮将语义词 `"Search"` 匹配至 `i` 标签，触发 §5.0.3 门禁后经过 3 次重试失败，最终依靠 VLM 视觉兜底救场。该过程虽最终成功，却耗费了 3 次重试延迟（~3s）与一次多模态推理（~3s），造成严重的延迟耗散。

**条款**：
1. **装饰叶子节点的宿主输入框自动纠偏（Container Escalation）**：
   在文本汇（`editableOnly === true`）分支中，当候选赢家是非可编辑的叶子节点（如 `i.search-icon`、`span.search-label` 等）：
   - 不得直接作为不可编辑节点被动等待门禁拒绝；
   - 向上探测 3 层以内祖先容器（`<label>`、`<form>`、`<div>`）或紧邻兄弟节点；
   - 若容器内包裹或关联唯一的可见且可编辑输入框（`<input>` / `<textarea>`），定位器自动将目标命中点升级映射至该宿主输入框；
   - 确保首轮即命中真实输入控件并直接落盘，消除无效重试与视觉兜底开销。

### 5.0.8 ★【2026-09-25 追加】结构化遥测故障代号与 Reflector 决策保真契约

**背景（机制事实）**：
Go 引擎底层已细化多态失败原因，但由于协调器 `reflector.py` 在重试耗尽及第 110 行兜底处将错误硬编码覆盖为 `unknown_error` 或 `aborted_exhausted`，导致物理诊断信息完全丢失，无法形成自动闭环。

**条款**：
1. **标准结构化故障代号（Standard Fault Codes）**：
   物理层与协调器建立统一的枚举代号契约：
   - `INPUT_OCCLUDED`：目标中心点被遮挡；
   - `TARGET_NOT_EDITABLE`：目标元素非可编辑元素，被后验门禁拒绝；
   - `WRITE_NOT_PERSISTED`：写入后验回读校验失败，输入值未落盘；
   - `LOCATE_NO_CANDIDATE`：未召回任何有效候选元素；
   - `TARGET_ROLE_MISMATCH`：受体的角色画像与意图语义不符（敏感凭据字段被非凭据意图命中），派发前熔断（§5.0.9 条款 1）；
   - `vlm_landing_unverified`：VLM 兜底路径的输入缺少落地回读证据，硬判失败（§5.0.9 条款 2）；成功出口缺请求动作对应页面侧证据时同归此码（§5.0.9 条款 5）。
   - `VLM_ACTION_MISMATCH`：VLM 自报动作与请求动作不相容，**派发前**熔断（§5.0.9 条款 4）。
   - `TARGET_TOPOLOGY_RUPTURE`：Leased Token 同余断言失败（Role/结构身份指纹破坏，或几何位移超限）且不允许局域微重锚，**派发前**硬熔断（§5.0.10 条款 3）。
   - `CLICK_LANDING_MISMATCH`：点击成功出口的落点证据显示命中元素与目标语义无关（判 miss），不得返回 `SUCCEEDED`（§5.0.9 条款 6：VLM 坐标路径与 learned 复用路径；条款 7：确定性路径）。
2. **Reflector 契约保真与决策**：
   - Go 引擎在返回 `codes.Aborted` 时，在 status details / message 中携带明确的结构化代号；
   - `reflector.py` 基于结构化代号执行分支决策，在 `FailureDecision.reason` 中完整透传结构化代号与证据，杜绝在兜底处无条件改写为 `unknown_error`。
3. **探针降级显式标记（Perception Degradation Marker，2026-09-25 追加）**：
   页内 WASM 探针在严格 CSP 站点（`script-src` 未声明 `unsafe-eval` / `wasm-unsafe-eval`）无法实例化时，性质是**感知降级**而非普通失败（github 实测：`WebAssembly.instantiate(): … violates the following Content Security policy directive … 'unsafe-eval' is not an allowed source of script`，`ExportPageTree` 整体失败、报告显示「AXTree 不可用」，同轮动作**仍报 `SUCCEEDED`**）。因此：
   - 降级必须以结构化代号 `csp_wasm_blocked` **显式标注**：探针状态快照（`ProbeStats.Degradation`）、`ExportPageTree` 返回的错误串、引擎侧日志三处之一不得缺失；**禁止静默降级**（禁止只在人类可读文本里出现「AXTree 不可用」而无可判别代号）；
   - 降级态下 `ref` / AX 取证通道不可用，任何缺少 `hit_element` 与落地回读证据的结论都**不得视为已验证**（与 §5.0.9 条款 2 同口径）；
   - 本条款只定义「标记与保真」；上层（协调器/调度器）是否据此收紧裁决（如切换到防御性收敛、快速失败）属**上层策略**，须另行立项，不在本条款内默认开启。

### 5.0.9 ★【2026-09-25 追加】VLM 兜底意图-受体守卫与落地后验契约（越权写入硬拦截）

**背景（机制事实）**：
`github` 站实测：后验门禁在正门 3 次拒绝装饰性 `span "Sign in"`（代号正确），但控制流降级到 VLM 后，VLM 在无角色契约约束下把输入派发到**视口中心最显著的注册邮箱框 `input#hero_user_email`**（引擎自报 selector，日志 `vlm: main-control input at 485,511`），而语义目标是 `search_box`；报告层**无 `hit_element`**、AXTree 因 CSP 不可用，仍返回 `SUCCEEDED`。即「假成功」被挤到 VLM 这一新出口，且性质是**越权写入**（触碰安全停止线第 ① 类边缘）。

**条款**：
1. **意图-受体角色守卫（Intent-Receptor Role Guard）**：
   物理层必须在**派发前**读取受体的角色画像（`type` / `name` / `id` / `placeholder` / `aria-label` / `autocomplete`）并与意图语义比对：
   - 受体命中敏感角色（`password` / `email` / `tel` / `user(name)` / `login` / `sign-in` / `sign-up` / `register` / `auth`，或文案含 `密码` / `邮箱` / `手机` / `账号` / `用户名` / `验证码` / `OTP`）而意图语义**不含**对应凭据词时，立即熔断并返回结构化代号 `TARGET_ROLE_MISMATCH`；
   - 熔断发生在派发前：**不得**产生任何键盘/鼠标事件，**不得**写 `learned`（一次越权不得自进化成稳定越权），**不得**被重试循环吞成普通失败；
   - 守卫覆盖**全部** input 派发出口——确定性定位路径与 VLM 兜底路径（selector / 坐标就近输入 / 文本语义重试）**共用同一判定、单点实现**。
2. **落地后验无豁免（Landing Verification）**：
   VLM 兜底路径的输入同样必须过 §5.0.3 条款 4 的回读判据（同一元素回读值包含输入值）：
   - 回读不过或无法回读 ⇒ 硬判失败，返回 `vlm_landing_unverified`，**禁止**以 `SUCCEEDED` 出口出现；
   - 该判据**不得**被「无 `hit_element`」「AX 树不可用」「探针降级」等弱证据豁免。
3. **代号透传**：守卫与后验失败的结构化代号必须进入引擎日志与协调器 `FailureDecision.reason`（§5.0.8 条款 2），不得在兜底处被改写为 `unknown_error`。

4. **意图-动作相容性守卫（Intent-Action Congruence Guard，2026-09-25 订正追加）**：
   VLM 自报动作（`action`）与请求的期望动作（`expected_action`）是两件独立的事。若物理层无条件采信自报动作，就会绕过请求动作的证据判据：请求 `input` 却按 `click` 派发一次鼠标点击、请求 `click` 却按 `input` 写一次值，两者都被计为成功（假成功的新出口）。
   - **相容表**：`click` ↔ `click`；`input` / `fill` / `select` ↔ `input`；`extract` ↔ `extract`；`done` / `crop_zoom` / `back` / `impossible` 属控制流动作，不决定执行语义（`done` 的执行仍按**请求动作**及其证据判据），不受本守卫约束；
   - 不相容时禁止按自报动作派发：**不得**产生键盘/鼠标事件、**不得**写 `learned`（一次错配不得自进化成稳定错配）、**不得**换出口重试；返回结构化代号 `VLM_ACTION_MISMATCH`；
   - 本判定与条款 1 同属「派发前单点守卫」，覆盖 VLM 全部动作出口（含 learned 命中支路）。

5. **成功出口证据门槛（Acceptance Evidence Gate，2026-09-25 订正追加）**：
   `vlmOutcomeSuccess` 是**生产者自报**的结局标记，不得单独决定出口（终局红线「只认页面侧证据」；`Status: SUCCEEDED` 不是动作生效的证据）。
   - 成功出口判据 = 结局为成功 **∧** 观测串携带**请求动作对应**的页面侧证据：`input` / `fill` / `select` ⇒ `input` 族观测（落地回读已过，§5.0.3 条款 4）；`click` ⇒ `click` 族观测 **∧** 落点自证通过（条款 6：落点证据须与目标语义相关，缺证据的三态处置见条款 6，本门槛只判「动作族 + 落点相关性」两件事）；`extract` ⇒ 非空提取文本；
   - 任一不满足 ⇒ 不得返回 `SUCCEEDED`，统一以 `vlm_landing_unverified` 归类并透传代号（§5.0.8 条款 2），不得静默降级成普通失败；
   - 分工：条款 4 是**拒绝侧**单点（判动作相容性），本条款是**接受侧**单点（判证据存在性），二者不重复判定同一条件。

6. **点击落点自证契约（Click Landing Attestation，T-3；2026-09-30 追加）**：
   `SUCCEEDED` 只表示派发成功，不等于点中目标（终局红线「只认页面侧证据」，`GLOBAL.md` §3.3）。落点自证＝把**物理层在派发前解析出的受体**（§5.0.14 决议 0）与请求目标语义比对，证据载体＝`hit_element`（`selector` / `tag` / `text`）。
   - **证据要求**：落点证据必须携带命中元素的**可见文本**（自身 `innerText`；为空时上溯至多 3 层祖先，且**仅当该祖先的文本可归因于命中元素**——即除命中链上的子节点外，其兄弟子树均无文本贡献——时才接受，归因不成立一律留空＝证据不足；截断 ≤120 rune）——只回 `selector` 不算落点自证。文本取证与 `elementFromPoint` 解析受体在**同一步**完成（点解析原语，实现 `pkg/browser/find/element.go::ElementAtPoint`）。
   - **上溯可信边界（2026-09-30 真机订正，`T-3`）**：命中元素自身无文本（canvas / 图标 / 纯图形部件）时，祖先容器的文本可能来自**与命中元素无关的旁支**。真机实证：canvas 上画的 `Sign in` 按钮位于一个还含说明段落的 `<div>` 内，旧口径照收该段落文案当落点证据 ⇒ 一个**点击确实生效**（页面 `state` 已由此改变）的合法落点被判 miss 硬失败，连败 3 轮后触发防乱点熔断、整页被冻结。故「上溯」只认**可归因于命中元素的文本**；归因不成立 ⇒ **证据不足**（出口不变、不写 learned）。宁可少认证据，不认错证据。
   - **三态判据**（`target_semantic` 与 hit 文本规范化后互含判定：大小写不敏感 ＋ 空白字符全部剔除，使 `Sign In` 与 `signin` 等价）：
     ① **相关**：`hit 文本 ⊇ 目标语义` 或 `目标语义 ⊇ hit 文本`——覆盖「点中自身」「点中其后代」「点中其祖先容器」三种命中形态；
     ② **miss**：hit 文本非空、与目标语义**双向均不互含**，且二者属**同一文字系统**（汉字 / 非汉字同侧）；**跨文字系统时互含判定没有判别力**（中文语义不可能与纯拉丁文本互含，「双向不互含」是文字系统的必然而非点错的反证）⇒ 归 ③ 证据不足；
     ③ **证据不足**：无 `hit_element` 或 hit 文本为空（含只回 `selector` 的坐标点击）。
   - **跨文字系统边界（2026-09-30 真机订正，`T-3`）**：真机实例——语义「登录」命中英文按钮 `Log in`（VLM 语义匹配后坐标点击，**点击本身正确**），旧口径照判 miss 硬失败、连败 3 轮触发防乱点熔断。判据只回答「这次落点是不是明显点错」，不回答「两种语言的词是不是同一个意思」；跨文字系统时它无从判断 ⇒ **证据不足**（出口不变、不写 learned）。
   - **出口**：② miss ⇒ **不得**返回 `SUCCEEDED`，返回结构化代号 `CLICK_LANDING_MISMATCH`（不重试、不换出口：不把假成功换成假重试），原样进 `FailureDecision.reason`（§5.0.8 条款 2）；① 相关 ⇒ 出口不变；③ 证据不足 ⇒ **出口不变**（缺证据不构成反证，避免误杀无文本图标按钮 / 空白区点击等合法落点）。
   - **learned 写入门禁（本条款核心防线）**：只有 ① **相关** 才允许写 `learned`；② miss 与 ③ 证据不足一律**不得**写（`vlmActOnClick` 过去的无条件 `vlmLearn` 属缺陷，已修）。覆盖全部点击写入口：VLM 坐标点击、VLM 候选 selector 点击、`node_id` 确定性点击回写（`learnNodeHit`）。
   - **learned 复用路径同样自证（2026-09-30 追加）**：复用命中（`vlmTry` 的 learned 分支）确定性执行后必须再判一次落点——学到的条目在页面改版后可能落到无关元素上。判 miss ⇒ 不返回 `SUCCEEDED`、不换出口重问 VLM，并**清掉该失效条目**（`vlm.LearnedStore.Forget`，下次重新走 VLM 学习）；① 相关 / ③ 证据不足维持原出口（免 VLM 是收益，不是免检）。
   - **写入默认启用（2026-09-30，用户口径「自动学习是对的」）**：`uws daemon` 托管默认带 `--vlm-learn-path <stateDir>/learned.json`（`UWS_LEARN=off` 关闭；引擎 flag 自身默认仍空，含 `off/none/false/no/0/-` 哨兵 ⇒ 库与测试不受影响）。放行依据＝本条款落点自证 ＋ learned 写入门禁已落地且有真机证据（`T-3`）⇒ 原「`T-3` / G-PROOF 门未放行则不得打开在线 learned 写入」的前提消失。
   - **适用边界**：`extract` 与非点击输入族不受本条约束（各自有 §5.0.3 条款 4 回读判据；`input` 族写入门禁见条款 2）；**node_id 寻址点击**（`target_semantic` 形如 `node:<n>`）不存在独立目标语义可比对，落点自证不适用（其正确性由探针 node_id 负责）。
   - **实现状态（2026-09-30）**：三态判据与 learned 写入门禁已落地（`pkg/engine/click_landing.go` ＋ `vlmActOnClick` / `vlmActOnCandidate` / `learnNodeHit` 三处写入口单点收口）；落点文本取证亦已落地（`pkg/browser/find/element.go::ElementAtPoint`：解析受体与取文本在**同一步**求值内完成，`clickAtInner` 落账到 `lastHit`，`LastHitElement()` 现携带 `text`）；**上溯可信边界**亦已落地（`elementAtPointJS` 只接受「旁支无文本贡献」的祖先，旁支文案一律归证据不足；`scripts/t3-landing-ancestor-probe.mjs` 真机断言）；**跨文字系统边界**亦已落地（`pkg/engine/click_landing.go::hasHan`：汉字/非汉字不同侧归证据不足；单测 `pkg/engine/click_landing_test.go` ＋ 真机 `click:登录` 复核）。**learned 复用路径自证**亦已落地（`pkg/engine/click_landing.go::learnedReuseFault` ＋ `pkg/vlm/learned.go::Forget`：复用判 miss ⇒ 清条目 ＋ 硬失败；单测 `pkg/engine/vlm_test.go::TestVLMLearnedReuseLandingMismatchForgets`），自进化写入由 `uws daemon` 默认开启（`cmd/uws/daemon.go::learnedPath`）。
   - **真机复证（2026-09-30，新构建 `v1.5.5+b603ece0` ＋ 自建 9224 独立 profile；细节与观测串见 `Tasks.md` `T-3`）**：① 页面侧——逐字取自构建源码的 `elementAtPointJS` 在真实页面（`iana.org/help/example-domains`）返回正确的 `{sel,tag,text}`，空白区返回零值，图标型元素触发「上溯 ≤3 层祖先」；② 报告层——`click:Domains` 真机回显 `hit=`a` "Domains" ← <CSS 路径>` 且页面实际导航；③ **`CLICK_LANDING_MISMATCH` 真机已复现（同日第二轮；标签不存在仍一律 `impossible`，因为主控 click 提示词是字面匹配——复现路线是「命中元素自身无文本、祖先文本来自旁支」）**：VLM 坐标点击 canvas 上画的 `Sign in` 按钮，引擎日志出现 `vlm: click landing mismatch … hit="This paragraph is unrelated filler copy inside the panel. state: CANVAS-CLICKED"`，连败 3 轮触发防乱点熔断；对照用例（命中元素及其祖先链均无任何文本）稳定走「证据不足 ⇒ 出口不变（`SUCCEEDED`）」。该复现同时证实观察 (i) 的**假失败**（点击已生效仍硬失败、还冻结整页）⇒ 已按「加校验」订正（见上「上溯可信边界」与实现状态）；(ii) miss 出口当时只在 VLM 坐标路径生效（`clickLandingOf` 仅被 `pkg/engine/vlm_fallback.go` 调用），确定性 / learned / `node_id` 寻址路径只记录落点证据、不判出口——**该判断已被同日第三批之二推翻**（learned 复用路径：`learnedReuseFault`；确定性路径：条款 7）。原「判出口收益为零」的理由作废：真机给出「单候选被绑定给无关语义仍报 `SUCCEEDED`」的现成假成功（`click:Order history` ⇒ `(生产选中="Sign in", 候选数=1)`，页面无该元素），收益不为零；改动面约束（A* 合法换步 / 跨文字系统）改由条款 7 的基准集规则处置。

7. **确定性路径点击出口判据（T-3 第三批之二；2026-09-30 追加）**：
   **背景（机制事实）**：条款 6 首轮落地时 miss 出口只覆盖 VLM 坐标路径与 learned 复用路径，确定性路径（语义 / 选择器直查）**只记录落点证据、不判出口**（理由＝「命中文本天然含目标语义，判出口收益为零」）。真机反证（2026-09-30，同批夹具）：页面**没有** `Order history` 元素，单候选 `Sign in` 被强行绑定给无关语义（引擎日志 `[WSTN-SHADOW] … (生产选中="Sign in", 候选数=1)`），报告仍 `SUCCEEDED` —— 与 VLM 路径同形的假成功 ⇒ 该理由作废，本条立。
   - **基准集（判据锚定）**：比对基准 = `target_semantic` 的**语义别名展开集**（`pkg/browser/semantics.Wants`，§5.0.2：内部标签 `submit_btn` → 页面文案「提交」/`submit`/…），**∪** **与本目标语义同源**的 L2 同义词文本（KB 背书，见「豁免」）。A* **多步换步**（路径长度 $\ge 2$）时基准整体换成 `exec_semantic`——末步是到目标的中间步骤，与目标文案无必然包含关系；长度 $=1$ 的「路径」不构成换步（根即答案），仍按 `target_semantic` 判。
   - **三态沿用条款 6**：相关 / miss / 证据不足（空文本、无 `hit_element`、跨文字系统）的判据与归一口径完全一致。基准集内任一元素与落点互含即算**相关**；**至少一条同文字系统**的比对可判且全部不互含才判 **miss**（跨文字系统不参与判 miss，见条款 6「跨文字系统边界」）。
   - **豁免（KB 背书，且必须与目标语义同源；2026-09-30 真机订正）**：落点与**同源** L2 同义词文本互含时算相关——「文本互含」判据对同义词没有判别力（`登录` / `登入` 这类同义对不互含，误判会造出与 VLM 路径同形的假失败 ＋ 防乱点熔断），故由离线知识库背书豁免。**同源判据**：`site_synonyms.source_rule_id` 指向的规则，其语义标签命中 `target_semantic`（`pkg/matcher` 按语义标签检索规则并回取同源同义词）。**「池内 L2 一律背书」的旧口径作废**：候选池的同义词召回与目标语义**无关**（`MatchQuery` 按**页面节点文本**召回忆，池中出现 `Sign in` 只说明页面上有该同义词文本，不说明它与目标语义有关），一律背书会把本条款要修的那档假成功原样放过——真机实证（`/tmp/t3det/det.html`，构建 `v1.5.5+3016a156`）：页面仅一个候选（`Sign in`，L2，0.7），目标语义 `Order history`，报告仍 `SUCCEEDED` ＋ `hit=a "Sign in"`，引擎日志**无** `deterministic click landing mismatch`（观测串 `[WSTN-SHADOW] … (生产选中="Sign in", 候选数=1)`）。订正后同一夹具判 miss（见「真机复证」）。
   - **出口**：判 miss ⇒ **不得**返回 `SUCCEEDED`、**不得**写 `learned`（`clickLearnGate` 不变）、**不得**换出口（**不得**回落 VLM 兜底重问——与 learned 复用路径同口径）；返回结构化代号 `CLICK_LANDING_MISMATCH` 并透传（§5.0.8 条款 2）。判 miss 属**事后取证**：命中的那次派发无法收回，本条款提供的是「不报成功 ＋ 不再复用」，不是「阻止这次点击」。
   - **不适用**：`node_id` 寻址路径（`target_semantic` = `node:<n>`）没有独立目标语义可比，落点证据照常回显、真伪判据留给调用方（协调器报告已给 `hit_element`）；`extract` 与非点击族不受本条约束。
   - **实现状态（2026-09-30）**：判据与出口单点＝`pkg/engine/click_landing.go`（`clickLandingReference` 基准锚定 / `clickLandingVerdictRefs` 三态 / `deterministicLandingFault` 出口 / `endorsedSynonyms` 同源背书），A* 路径长度由 `astarSelect` 回传（`pkg/engine/service_engine.go`）；同源背书查询＝`pkg/matcher/rule.go::Store.SynonymsForSemantic`（规则语义标签 × `site_synonyms.source_rule_id` 联结）＋ `pkg/matcher/matcher.go` 同名包装。单测：`pkg/engine/click_landing_test.go`（基准锚定 / 别名展开 / 同源背书 / 确定性 miss 硬失败且不调 VLM / 相关落点回归）。
   - **真机复证（2026-09-30，自建 9224 独立 profile ＋ 本地 8791 夹具服务，用户 9222 未触碰）**：夹具 `/tmp/t3det/det.html` ＝「唯一候选＝KB 同义词 `Sign in`（`div#go` 内 `a#signin`）＋ 无关目标语义 `Order history`」① **订正前**（构建 `v1.5.5+3016a156`）：`./webtouch run … --plan 'click:Order history'` ⇒ `Status: SUCCEEDED`、`hit=a "Sign in" ← a#signin`、日志仅 `[WSTN-SHADOW] … (生产选中="Sign in", 候选数=1)`、**无** mismatch 行 ⇒ 假成功复现（豁免过宽）；② **订正后**（构建 `v1.5.5+a6c13b20`／pid 20376，同夹具同命令）⇒ 日志 `engine: deterministic click landing mismatch semantic="Order history" ref="Order history" hit="Sign in"`（协调器重试各次均同判，3 次后触发防乱点熔断 ⇒ 报告 `PAUSED` 并原样透传代号，与条款 6 的代价同形）；③ **正向对照** `--plan 'click:Sign in'`（目标语义＝页面确有文本）⇒ `Status: SUCCEEDED`、`hit=a "Sign in" ← a#signin`，出口与 `hit_element` 回显不变。
   - 与条款 5 的分工：条款 5 是 VLM 路径的**接受侧**单点，条款 6 后半 / 本条是 deterministic 与 learned 复用路径的**接受侧**单点，三者共用同一套三态判据与 `hit_element` 证据，不重复判定同一条件。

8. **兜底视觉细节预算契约（Visual Detail Budget；2026-09-30 追加，`T-1` 后续复核）**：
   把截图交给视觉模型时，`image_url.detail` **不得恒为 `low`**（旧实现硬编码 `low`，是把「点歪」制造出来的缺陷本身）：`low` 档在服务端先把图降采样到约 512 px 长边的缩略图，小目标**在模型看到之前就已丢细节**，坐标只能靠猜。
   - **判据（按图像长边单点选档）**：图像长边 $>768$ px ⇒ `high`；$\le 768$ px ⇒ `low`（768 = 低档经验预算 ~512 px ＋ 1.5× 余量）。整页截图（1440×813 CSS / 2880×1626 @DPR2）恒为 `high`；小裁剪（长边 $\le 768$ px，如 320×200 CSS 裁剪 @DPR2 = 640×400）仍走 `low`，不额外付费。判据只看**图像像素**（模型看到的那张图），与 CSS 视口、DPR 无关。
   - **依据（单变量对照，[`docs/evidence/vlm-detail-resolution-ab-2026-09-30.md`](../docs/evidence/vlm-detail-resolution-ab-2026-09-30.md) §1）**：同一张图、同一模型、同一提示词、`temperature=0`，只把 `detail` 从 `low` 换成 `high` ⇒ 出界 miss **−104 px → −2 px**（2.89 半宽 → 0.05 半宽）；换算链（条款 6 前身取证）与答题约定（比例／像素／百分比／报真实图尺寸，四种同量级 −84…−127 px）均已证伪为变量。
   - **代价（同案卷 §2.4）**：prompt token `655 → 1431`、延迟 `0.78s → 0.91s`（同机各 1 次）。兜底是稀有路径（语义/选择器听不见才走），该量级成本换取命中率，取舍已定。
   - **不做**：不靠给图叠参考线／参考点补偿（低分辨率下边缘刻度被完全忽略、编号参考点更差 −173 px，见案卷 §1）；也不指望模型自觉请求 `crop_zoom`（10 组真机样本 0 次主动请求）——裁剪复看若要做须由**引擎强制**，不属本条。
   - **实现落点**：`pkg/vlm/vlm.go::detailForImage`（自动选档单点，`Resolve` 内消费）；`ResolveRequest.Detail` 非空时按其**显式覆盖**（供 Eye 视觉显微原语与离线探针按需强制档位）。改档位不得改动本条判据之外的任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。

8. **内部控件语义的落点自证豁免（2026-10-01 追加；同日扩围）**：
   **背景（真机事实）**：① `gemini_model_switch` 的物理层落点是站点自己的「模型选择器按钮」，其可见文本是**当前档位**而不是目标语义（真机 2026-10-01 通道实测：`确定性路径落点与目标语义无关 ref=gemini_model_switch hit=Flash`，同一轮菜单自检判 `判定=正常 选择器="Open mode picker, currently Flash" 当前档位="Flash"`）；② 扩围后的同类实例：`gemini_send` 的落点是站点固定发送按钮，可见文本 `Send message`（真机 2026-10-01 13:28:07 引擎日志：`engine: deterministic click landing mismatch semantic="gemini_send" ref="gemini_send" hit="Send message"`）。条款 6/7 的文本互含判据对这类语义**没有判别力**：它们是**引擎/网关侧内部控件名**（页面上不存在该字符串），而站点专有文案按 §5.0.2 纪律**不得**进通用别名表；其正确性由各自的**专用后验**负责（切换＝选择器回读 ＋ 菜单自检；发送 / 新对话＝`pkg/browser/input/recovery.go::verifyGeminiClick` 的输入区 / 回答区清空回读）。拿「控件文案 vs 内部标识」判 miss 只会造出假失败（`CLICK_LANDING_MISMATCH` 是「不得换出口」硬失败），并把整页点进防乱点熔断。
   - **判据**：目标语义属**站点控件语义**（集合由 [`pkg/browser/semantics`](../pkg/browser/semantics/action_budget.go) `IsSiteControlSemantic` 单一维护）时，落点自证**不适用** ⇒ 一律归**证据不足**：不判 miss、不返回 `CLICK_LANDING_MISMATCH`、不换出口重问，也不写 learned。**准入两条（须同时满足）**：① 语义标识是引擎/网关侧内部控件名（页面上不存在该字符串）；② 该语义在物理层有专用后验。当前登记＝状态型切换四例（`gemini_model_switch` / `gemini_thinking_switch` / `deepseek_mode_switch` / `deepseek_thinking_switch`）＋ 站点固定控件两例（`gemini_send` / `gemini_new_chat`）；其余站点控件语义**不预先扩围**（`send_button` / `doubao_send` 等今日靠文本巧合通过，无实测缺口）。
   - **边界**：豁免只按**目标语义**生效，普通点击与 `node_id` 寻址（沿用既有「不适用」口径）行为不变；豁免**不放松**任何语义自身的失败门——切换未生效仍按 `model_switch_failed` / `thinking_switch_failed` / `mode_switch_failed`、发送未生效仍按 `verifyGeminiClick` 回读失败透传（§5.0.8 条款 2）。
   - **实现落点**：`pkg/engine/click_landing.go::landingSelfCheckApplies`（`deterministicLandingFault` / `clickLearnGate` / VLM 坐标出口三处共用同一判定）；单测 `pkg/engine/click_landing_test.go::TestStateSwitchSemanticSkipsLandingSelfCheck`；真机通道验证见 `handoff.md` 2026-10-01 节。

### 5.0.10 ★【2026-09-25 追加】穿透寻底与 Leased Token 结构身份局域微重锚契约（WebTouch v2.0 Phase 1）

**背景（机制事实）**：
- 现代页面普遍在真实受体之上铺「透明手势代理层」（`opacity:0` / `aria-hidden="true"` / `pointer-events:none`）。页内 `document.elementFromPoint(x,y)` 只回最外层宿主或顶层透明层，抹杀底层真实可编辑受体（点击落到遮罩、输入无处可去）。
- 封闭 Shadow Root（`mode:"closed"`）在**任何** Execution World（含 Isolated World）下 `shadowRoot` 恒为 `null` ⇒ 页内纯 JS 无法穿透；且 `composedPath()` 是 **Event** 实例的方法、`Element` 上不存在，取组合祖先只能用 `getRootNode()`（非组合根）逐级 `host` 上溯。
- 水合（hydration）会在派发窗口内重建节点，并常同时改动 `placeholder` / `aria-label`（Name 变）或把 Role 掉成 `generic` ⇒ 「Name 恒等」不能作为重锚判据（会让重锚几乎永不生效，退化成「一失配即终局失败」，白掉站点覆盖）。

**条款**：
1. **双层分工的穿透寻底（Hit-Piercing）**：
   - **页内 JS 只做几何层叠判定**：以 `document.elementsFromPoint(x,y)` 栈自顶向下剔除「透明手势代理层」（`opacity === "0"` ∨ `aria-hidden="true"` ∨ `pointer-events:none`，且自身非可编辑、子树内无可编辑载体），取第一个非代理层作为物理受体干；组合祖先回溯用 `getRootNode()` + `host`（**禁止**依赖 `composedPath()`）；
   - **封闭 Shadow DOM 的穿透与物理击发走宿主层（CDP）**：页内 JS 判定「命中封闭宿主」时，由宿主以 `DOM.getNodeForLocation(x, y, ignorePointerEventsNone=true)` 取真实命中节点，`DOM.resolveNode` + `Runtime.callFunctionOn` 在**该节点对象**上聚焦/取值（CDP 不受 `closed` 封装约束），并以 `Accessibility.getPartialAXTree(fetchRelatives=false)` 取角色/名称作为受体身份主干；页内 JS 不得承担该职责；
   - **封闭宿主交接优先于「就近输入框」启发式**（2026-09-25 真机 E2E 订正）：命中元素自身是封闭宿主（自定义元素且 `shadowRoot === null`）且其 light DOM 子树内无可编辑载体时，页内**必须立即上报 `closed_shadow_host` 交宿主层穿透**，**禁止**再向兄弟 / 祖先子树搜索「就近输入框」——真机实测（E2E 场景 A/E）该启发式会把值写进 DOM 里另一个无关 `input`（假成功 + 越权写入，守卫也因受体被换而失效）；
   - **AX 受体选择：可编辑角色优先，`editable` 属性仅作兜底**（2026-09-25 真机 E2E 订正）：`role ∈ {textbox, searchbox}` 命中时，取几何包含命中点者中内容盒面积最小者；**无角色命中时**才退到 `editable` 属性判据（真机取值形态为 `plaintext` / `richtext`，**不是**布尔 `"true"`）。理由：`input[type=search]` 一类控件的 **UA shadow 内部容器**带 `role=generic` + `editable="plaintext"` 且内容盒更小，只按「面积最小＝最内层」会把物理击发落到浏览器内建渲染节点上（回读为空）；
   - **受体身份主干**：角色与身份判定一律以 AX 切片（或宿主层等价画像）为准，天然剥离隐藏数据载体与不可交互载体。
2. **Leased Token 结构身份同余（Structural Identity Congruence）**：
   派发前的目标断言以**租约**形式持有，时效 $\le 1500\text{ms}$（标称，待 Phase 1 前置真机采样标定）；命中失配时，仅当**同时**满足以下三条才允许局域微重锚：
   - **Role 恒等**（AX 角色，或 AX 不可用时的等价标签角色）；
   - **结构身份指纹相等**：`id` / `name` / `type` / DOM 路径；**Name 文案（`placeholder` / `aria-label` / 文本）只作弱加权，不得作为恒等判据**，指纹破坏即熔断；
   - **几何位移** $\|\Delta d\|_2 \le 32\text{px}$（标称）：超限即熔断。
3. **单调阻尼与强制落地回读（Budget = 1）**：
   - 微重锚全局硬上限 **1 次**（单步）；**严禁**把控制权交回 VLM 自由发挥（杜绝水床位移）；
   - 重锚击发后**必须无条件执行既有落地回读**（§5.0.3 条款 4：同一元素回读值包含输入值）；回读不过或无法回读 ⇒ 硬判失败（VLM 路径返回 `vlm_landing_unverified`），绝无假成功出口；
   - 租约过期、结构身份指纹破坏、几何位移超限三者之一成立即熔断，返回结构化代号 `TARGET_TOPOLOGY_RUPTURE`。
4. **代号透传**：`TARGET_TOPOLOGY_RUPTURE` 必须进入引擎日志与协调器 `FailureDecision.reason`（§5.0.8 条款 2），不得在兜底处被改写为 `unknown_error`。
5. **Phase 1 准入门禁**：动态微位移场景下单步自愈率 $\ge 95\%$ 且无次生漂移越权；高危意图（注册 / 邮箱 / 密码）派发至非对应受体时拦截率维持 **100%**（§5.0.9 条款 1 不变）。

### 5.0.11 ★【2026-09-25 追加】派发标记时点与中断语义契约（Dispatch Mark Timing）

**背景（机制事实）**：
物理层以 `dispatchTracker` 区分两类中断：定位/等待阶段中断（**没有**任何副作用，可安全重放，代号 `canceled_before_dispatch`）与物理事件已发出后中断（结果未知，不得盲目重放，代号 `canceled_after_dispatch`）。该契约此前只存在于代码注释，未入规范；实现上 `input` 族与部分 `click` 族分支在**定位之前**就打了派发标记，于是定位阶段的超时/取消把真因（如 `input_failed:context deadline exceeded`）覆盖成 `physical_event_dispatched_outcome_unknown` —— 调用方据此放弃重试，而动作其实从未派发：既没生效也不能重放。

**条款**：
1. **标记时点**：`disp.mark()` 只能在「定位与全部派发前校验（守卫 / 可编辑 / 重锚 / 遮挡）通过、物理事件即将发出」的时点调用；**禁止**在定位之前打标记。参照实现：`inputAtPierced`（穿透出口）与 `clickAtInner`（坐标出口）。
2. **覆盖范围**：确定性定位族（`clickWithRecovery` / `inputWithRecovery` / 站点特化切换族）与坐标族（`ClickAt` / `InputAt`）共用同一时点语义；派发原语（`inputAt` / `clickAt`）在其派出第一个输入事件前标记，调用方**不得**在更早处重复标记。
3. **中断语义保真**：定位阶段被取消/超时必须报 `canceled_before_dispatch`（可安全重放）；已派发后中断报 `canceled_after_dispatch`（结果未知）。真因不得被覆盖：物理层结构化失败代号与中断分类必须同时可见。

---

### 5.0.12 ★【2026-09-25 追加】在线 Episode 采集与脱敏契约（Learning Data Boundary）

**背景（为什么需要）**：`T-12` 的 Execution Episode 是 ONNX 晋升与「调试即变强」的共同前置。但采集边界若只写「不含页面正文」，离线端将**无法重算** §5.0.1 的 5 维特征（`simhash_sim` / `jaccard` / `exact_match` / `substring` / `log_len` 全部依赖候选文本与目标语义），特征对拍与 §5.2 影子评估随之失效 —— 隐私红线的正确形态是**最小充分裁剪**，不是全量剔除。同时，若采集协议未包含**平行候选状态**（影子观察），事后消除反事实偏差的代价不可控。

**条款**：
1. **观测先行与配对落盘**：单条 Episode（append-only、本机落盘、不入交付仓）必须同时包含 ① 意图与目标语义；② **候选集合**（元素身份 + 裁剪后文本 + 原始 rune 长度）；③ 生产决策与其派发状态（`canceled_before_dispatch` / `canceled_after_dispatch` 及结构化失败代号）；④ 页面后验（`hit_element` / 回读值 / URL 变化 / `unknown`）；⑤ **影子观察字段**（同一候选集上的打分与 Top-1，允许为空，但**字段必须存在**）；⑥ 采集时刻与页面 URL。生产决策与影子观察**同条配对**落盘 —— 缺失的平行候选状态事后补齐代价呈指数上升。
2. **a11y 骨架最小充分裁剪**：只保留目标元素及其**直系 1 阶**父/子/兄弟节点的标签元数据（`tag` / `role` / `aria-label` / `name` / 哈希化 `class`）与**截断文本（≤32 字符）**；**丢弃**全局 DOM、非目标区域文本、页面正文、Cookie、凭据与私密输入值（`@vault:` 引用只留引用名）。因裁剪导致 §5.0.1 特征无法逐位重算的候选，其重算值必须标注**近似**并**不得用于毕业判定**；为此必须同时存**原始 rune 长度**与**截断标记**（`log_len` 依赖原始长度、`exact_match` / `substring` 依赖完整文本）。
3. **脱敏硬过滤**：落盘前对全部文本字段执行正则过滤（电话、邮箱、Bearer/Token、UUID），命中替换为占位符并记 `redacted=true`；过滤不得改变未命中字段的字节内容（保证特征可重算）。
4. **第一指标与拒识口径**：自进化的第一物理指标＝**快路径零负迁移精度**（激活态 `Precision@Fast ≥ 0.99`）与**安全拒识率**；端到端任务失败率降为**滞后指标**（级联系统 `P(Fail)` 受 VLM 随机性与网络抖动支配，信噪比低；快路径的 Type I 误判才会直接使任务暴毙）。拒识事件**只认物理层结构化代号**（`VLM_ACTION_MISMATCH` / `vlm_landing_unverified` / `TARGET_ROLE_MISMATCH` / `TARGET_TOPOLOGY_RUPTURE`），**不得**以 `SUCCEEDED` 计数。本条款**不依赖**「快路径占比极低」这一前提（2026-09-25 复核：VLM 主控决策与可见失败动作审计同量级，且每请求事件分母待 `T-11B` 补齐）。
5. **断点隐式捕获（用户修正即 gold）**：自动化在重试/报错处停住后，**用户本人**在受管 target 内手动完成该步时，记录器捕获该单步事件并标记 `T-20-Implicit-Gold`（含元素身份与页面后验），作为最高质量正样本。**实现缺口（已立项）**：当前引擎**没有**用户输入观测通道（`Input.*` 仅用于派发、页内 `addEventListener` 仅存在于测试页）⇒ 需新增记录器（可复用 `Page.addScriptToEvaluateOnNewDocument` 的探针注入通道），前置条件是「报错后 target **不得立即释放**、记录器保持武装」。该 gold 标记**只入离线样本**，不构成在线 learned 写入的豁免（仍过 §5.0.9 与 G-PROOF）。
6. **离线可回放性（页面侧候选投影，2026-09-25 用户裁定 CON-205 追加）**：Episode 的 `candidates[]` 必须是**页面侧候选集投影**——即「页面候选 × 目标语义」的候选身份 + 裁剪后文本，而不是引擎内部决策池的残留快照。候选集是 §5.0.1 五维 pairwise 特征算子（`simhash_sim` / `jaccard` / `exact_match` / `substring` / `log_len`）的**输入空间**：`candidates[]` 全空的样本无特征对可算，进蒸馏集只会让离线训练在数学上退化，故**严禁全空样本入蒸馏集**（消费端须 fail-closed 丢弃并计数，不得静默当负样本）。**实现事实（2026-09-25 真机抽检）**：9 条真实 Episode 的 `candidates` 全为 `null`——引擎候选池只含 KB 规则 ∪ 探针显式打分节点（`Confidence>0`），真实 CDP 采集节点 `Confidence=0` 时仅吸收 L2 同义词命中（[`pkg/engine/service.go`](../pkg/engine/service.go) `pageCandidatePool` 有意如此，防无关节点被 L3 误剔成假 `NOT_FOUND`），叠加 WASM 探针未启用 ⇒ 公开站无 KB 规则时池恒空。因此**「只读影子通道」与「页面侧候选集/1 阶骨架注入」是同一个原子交付单元**（影子打分本就作用于该候选集），不得拆成两批；影子观察字段（条款 1 ⑤）在两者同时就位前保持 `null`，不得以引擎决策池冒充页面候选集填充。

---

### 5.0.13 ★【2026-09-25 追加】`T-22` 第二批原子技术规范（只读影子通道 ＋ 页面侧候选投影）

**背景（为什么需要）**：§5.0.12 条款 6 确立了「只读影子通道 ＋ 页面侧候选集/1 阶骨架注入」为**单一原子交付单元**，但只给了存在性约束、未给字段与协议。用户 2026-09-25 给出第二批的**实现级技术契约**（Go schema / CDP 只读快照协议 / 脱敏与截断 / 消费端 fail-closed），并要求**物理实现冻结为单一提交单元**。本条款把该契约规范化为可编译、可验收的条文；与既有 `episode.v1` 的映射与订正见条款 6 表。

**条款**：
1. **交付单元唯一性（不得拆批）**：第二批 ＝ **一次提交**内同时就位四件事：① 页面侧候选集投影（含 1 阶骨架）；② `is_ground_truth` 唯一标定；③ **只读**影子 ONNX 通道与 `shadow_inference` 字段；④ 消费端 fail-closed 与 quarantine。四者缺一即视为整批未交付。**影子通道只读**：不得参与 `decision`、不得抢决策权；在线 learned 写入仍恒禁（`T-3` / G-PROOF 门）。

   **接线边界（硬门禁；2026-09-25 实现中发现，随本批代码生效）**：本批**只落代码侧的只读影子通道**，**不得**接通 `cmd/uws/daemon.go` 的 `--onnx-runtime-lib`。理由：接通会经 [`pkg/engine/knowledge.go`](../pkg/engine/knowledge.go) `syncScorerWithKnowledge` 构建 `onnxScorer`，而 [`pkg/engine/service.go`](../pkg/engine/service.go) 的 `pageCandidatePool` 会用它给真实 CDP 节点打 `Confidence=score` 并作为 L1 候选入池 ⇒ **决策带被直接改动**，越 `T-3` / G-PROOF 门。影子通道只在 `Server.shadowInference` 内**只读** `s.onnxScorer`、分数**只落盘**、**恒不参与 `decision`**；无 ONNX 会话（未接线态）时 `shadow_inference` 如实为 `null`，由条款 5③ 计入 `shadow_missing_total`。**接线时点＝`T-3` / G-PROOF 放行后的另一批**，本批不得为使影子「有数」而接线。
2. **页面侧候选集（schema 与投影口径）**：
   ① `candidates[]` **永不为 `null`**：必须以**页面侧候选集投影**填充——对页面快照采集到的交互节点逐一给出「元素身份 ＋ 裁剪后文本 ＋ 1 阶骨架」，**不得经过引擎决策池**（条款 6 的反模式）。
   ② 候选身份字段（`PageCandidate` 口径）：`index` / `tag` / `role` / `selector_hash`（选择器**只落哈希**，不落原文）/ `rect`（`x` / `y` / `w` / `h`，供条款 3③ 距离截断与离线核对）/ `interactive`。
   ③ 候选上下文字段（`CandidateContext` 口径）：`text`（裁剪后）/ `text_rune_len` / `text_truncated` / `is_ground_truth` / `skeleton`。
   ④ 页面确无交互节点时落**空数组**（非 `null`），并在 `production` 记 `candidates_source=page_projection` ＋ `page_candidate_count=0`；该样本按条款 5 判非法。
   ⑤ `production.candidates_source` 第二批起恒为 `page_projection`（旧样本缺该字段视为 `engine_pool` 遗留）；`candidates_truncated` 记录条款 3③ 是否发生截断。
3. **CDP 只读快照提取协议**：
   ① **只读**含义：单次页内求值返回结构化快照，**不发任何 `Input.*` 域命令**、不改 DOM、不触发导航；与派发路径分离，采集失败不得影响决策。
   ② **交互性选择器清单 ＝ 既有清单的加性扩列**（不删不减）：`button, a[href], input:not([type=hidden]), textarea, select, [role="button"], [role="link"], [role="menuitem"], [role="checkbox"], [onclick], [data-testid], [tabindex]:not([tabindex="-1"])`。
   ③ **候选上限 `K = min(len(nodes), 64)`**：全集超限时按「**距目标派发坐标（`target_x` / `target_y`）的欧氏距离最近**」升序保留前 64 条；无派发坐标（未派发 / 熔断 / 定位阶段失败）时退化为**文档序**前 64 条，并置 `candidates_truncated=true`。截断必须**确定性**（同页且同坐标 ⇒ 同结果）。
   ④ **1 阶骨架**：只取目标的**父 / 直接子 / 兄弟**（直系 1 阶，不递归）；每个节点只留 `tag` ＋ `role`，**丢弃全部 class**（订正 §5.0.12 条款 2 的「哈希化 class」）；节点文本按条款 4 脱敏并截断。**对称挂载口径见条款 7**（第二批起**每个候选**各挂自身 1 阶骨架，消除「仅 GT 挂载 ⇒ 骨架成伪特征」）。
   ⑤ **`is_ground_truth` 唯一**：由**页面回读**（`resp.GetHitElement()`）与候选集对齐后，在命中的那条候选上标 `true`，**且全样本恰一条**；未派发、无回读或对不上候选时**全为 `false`**（不得猜、不得回填陈旧命中）。
   ⑥ 去重口径按**候选身份**（`selector_hash`），**不再按文本去重**（同文案多节点是真实候选，按文本合并会丢失 §5.0.1 `exact_match` / `substring` 的负例空间）。
4. **脱敏与截断规范**：
   ① **正则字典取并集**：既有规则（Authorization / Bearer / 键值凭据 / JWT / EMAIL / UUID / 电话三组）**全部保留**，**新增**用户口径 `\+?[0-9]{10,18}`（边界条件见③）。
   ② **`input` / `textarea` 的 value 一律不进文本字段**：其值替换为字面量 `[VAL_MASKED]`，**绝不落盘明文**（既有 JS 降级路径把 `el.value` 当文本采集，属必须订正的泄漏面）。
   ③ 规则**顺序敏感**：凭据类 → JWT → EMAIL / UUID → 电话窄口径 → `\+?[0-9]{10,18}` 宽口径**最后**执行，且须满足「前后非数字、非小数点」边界（`(?<![\d.])\+?\d{10,18}(?![\d.])`），避免吞噬 UUID / 日期片段。**已知代价（显式声明，不得静默放宽）**：13 位毫秒时间戳串会被遮蔽；因 §5.0.1 五维特征不依赖纯数字串，该代价可接受。
   ④ 占位符家族：`[REDACTED:TOKEN|JWT|EMAIL|UUID|PHONE]`（既有形态**保持不变**，已落盘样本可继续解析）＋ value 专用 `[VAL_MASKED]`；命中必须计入 `redaction.hits` 与 `redaction.fields`。
   ⑤ 骨架文本截断：**UTF-8 ≤ 32 字符**，多字节安全切分（不得截出半个字符）。
   ⑥ 隐私口径（自决并声明）：`url_before` 只留 `scheme://host/path`（query 剥离，沿用既有口径）；`url_hash = sha256(去 query url)[:12]`；`target_selector` **只落哈希**，不落原文。
   ⑦ 红线不变：本地敏感页（银行 / 内网 / 含凭据页）一律不采集，**隐私红线高于数据量**。
5. **消费端 fail-closed 三项代数条件与 quarantine**：离线消费端 [`pipeline/episode_consumer.py`](../pipeline/episode_consumer.py) 逐条校验——
   （落点说明：校验器独立成可导入模块，不放进 `pipeline/trace_distiller.py`——后者在 HEAD 即因 `from pipeline.component_gene import extract_gene` 指向不存在的符号而**不可导入**，属既有缺口、与本批无关，另行立项；消费端调用方在缺口修复后从本模块取校验器。）
   ① `len(candidates) >= 1`（非空）；
   ② `sum(is_ground_truth) == 1`（恰一条）；因未派发 / 无回读而全 `false` 的样本**同判非法**（属歧义样本，不得当负样本）；
   ③ 影子接线后 `len(shadow_inference.scores) == len(candidates)`（基数一致）；`shadow_inference == null` 视为**早于第二批**的样本，**不**据此 quarantine，但必须计入 `shadow_missing_total`。
   违反任一 ⇒ 样本**不进入**蒸馏集，原样隔离到 `<state>/episodes/quarantine/episodes-YYYYMMDD.jsonl`（append-only，不改原文件），并累加 `quarantined_episodes_total`。**严禁静默丢弃或当负样本**（§5.0.12 条款 6）。
6. **与既有 `episode.v1` 的映射与差异订正表**（复用 / 新增 / 改名 / 订正四类，逐条显式）：

| 用户契约（第二批） | 实现落点（[`pkg/engine/episode.go`](../pkg/engine/episode.go)） | 处置 |
|---|---|---|
| `trace_id` | `episode_id` | **复用**（语义等价，不新增字段） |
| `step_index` | `intent.node_id` | **复用** |
| `timestamp` | `at`（RFC3339） | **复用** |
| `url_hash` | `page.url_hash = sha256(去 query url)[:12]` | **新增** |
| `target_selector` | `candidates[].selector_hash` | **新增**（只哈希，不落原文） |
| `dispatched_outcome` | `production.dispatch_state` | **改名等价**（取值仍按 §5.0.11：中断两态 ＋ 结构化失败代号；第一批附加值 `not_dispatched` / `dispatched_outcome_known` / `dispatched_outcome_unknown` 保留） |
| `PageCandidate` | `EpisodeCandidate`（＋ `x` / `y` / `w` / `h` / `interactive`） | **扩字段** |
| `CandidateContext` | 并入 `EpisodeCandidate`（`text` / `text_rune_len` / `text_truncated` / `skeleton`）＋ **新增** `is_ground_truth` | **扩字段**（沿用既有嵌套形态，不引入第二层结构） |
| `ShadowInference` | `episode.shadow` → **改名** `shadow_inference`，**新增** `model_version` | **改名 ＋ 扩字段**（`scores` / `top1` / `model_hash` 保留） |
| `pkg/episode/types.go` | 实际落点 [`pkg/engine/episode.go`](../pkg/engine/episode.go) | **路径映射**（既有 Episode 数据流结构体） |
| schema 版本字段 | 保留 **`episode.v1`** | **声明**：扩字段不升版，新增字段均可缺省（`0` / `false` / `null`），旧样本仍可解析 |
| 交互性选择器清单 | [`pkg/browser/cdp_session.go`](../pkg/browser/cdp_session.go) 页内 JS | **加性扩列**（`[role="checkbox"]` / `[tabindex]:not([tabindex="-1"])`） |
| 「哈希化 class」（§5.0.12 条款 2） | `EpisodeNeighbor.class_hash` | **订正为丢弃**（只留 `tag` ＋ `role`；class 哈希对同构节点无判别力且多一处泄漏面） |
| `el.value` 进候选文本 | `collectJSCandidates` 降级路径 | **订正**：value → `[VAL_MASKED]`，不得落明文 |
| 候选按文本去重 | 既有 `seen` 集合 | **订正**：改按候选身份（`selector_hash`）去重 |
| 每候选 1 阶骨架（条款 7） | `EpisodeCandidate.skeleton`（**全部候选**，非仅 GT） | **扩字段**（对称挂载；`episode.v1` 不升版、新字段可缺省） |

7. **候选骨架对称挂载（2026-09-25 用户 State C 追认 ＋ CON-214 伪特征硬线；`INT-218①` 前置必修、唯一阻断扩批项）**：
   ① **对称性硬线（不得放宽）**：pairwise 训练空间内**全部候选**（含非 GT 负样本）必须携带**同构维度**的 1 阶骨架特征；**严禁**「仅 GT 候选挂骨架、负样本 `neighbors` 全空」——后者使骨架退化为 GT 的**伪特征**，离线蒸馏等价于用标签泄漏特征训练（用户 `CON-214`）。
   ② **协议扩展**：页内只读快照的**每个入选候选**各携带**自身** 1 阶骨架（父 / 直接子 / 兄弟），字段口径沿条款 3④（只留 `tag` ＋ `role`，class 一律丢弃）与条款 4⑤（文本 UTF-8 ≤ 32 截断 ＋ 脱敏）；`skeleton.source` 仍为 `page_snapshot_1hop`。
   ③ **每候选邻居上限（自决并声明）**：**≤8 条**，顺序 `parent → children → siblings`；超出按**文档序**截断并置位**截断标记**（页内 `neighbors_truncated` → 落盘 `skeleton.truncated`）；按节点身份去重、结果**确定性**（同页同状态 ⇒ 同结果）。
   ④ **兼容与不升版**：顶层 `PageSnapshot.Skeleton` 保留为其 **GT 候选** `neighbors` 的**别名**（旧口径读取路径不变）；`episode.v1` **不升版**，新增字段（`neighbors` / `truncated`）均可缺省，旧样本仍可解析。
   ⑤ **不改动面**：`is_ground_truth` 语义（条款 3⑤）、采样时点（§5.0.14 条款 1）、**影子只读边界**（条款 1 / §5.0.14 条款 3）与 `struct_hash` / `confidence` / `matched_rules` **占位口径**（`INT-218②`：保持占位、暂缓补采、不阻断扩批）**均不变**。

**验收（第二批）**：`candidates` 非空且 `candidates_source=page_projection`；`is_ground_truth` 恰一条；`shadow_inference.scores` 基数与候选一致；消费端实跑一次能把非法样本隔离并计数；**条款 7 对称性：非 GT 候选的 `skeleton.neighbors` 亦非空、每候选 ≤8 条、`class_hash` 恒空**；`gofmt` / `go vet` / `go test`（18 包）/ `pytest` 全绿；先红后绿。**不修改任何冻结参数**（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02`）。

---

### 5.0.14 ★【2026-09-25 追加·订正】第二批订正：采样时钟前移 ＋ DOM 子树 GT ＋ 影子正交 ＋ 消费端三级录取

**背景（为什么推翻 §5.0.13 的四条）**：`4e19895a` 交付并按 §5.0.13 运行态验收后，用户给出实现级推翻性契约（State A 诊断 ＋ State B 施工图）。核心判定：上一版把数据飞轮推入**冷启动死锁**——① 采样时点在**动作收尾**，导航型 `click` 取到的是导航后的新页面（`page_projection_after_nav`），而决策依赖的是**动作前**状态 $s_t^-$，属因果倒置，且会把 WebTouch 最核心的导航型 `click` 整类废掉；② GT 判据只用**矩形包含**，在 `<button><svg/><span/></button>` 类现代组件下命中点极易落在内部叶子，良性样本被判 0 命中；③ 影子通道与决策链路耦合（经 `syncScorerWithKnowledge` → `s.onnxScorer` → `pageCandidatePool`），使「只读影子」被迫连带禁用；④ 消费端三项代数条件**一刀切**，把冷启动期最需要的弱监督样本（影子未接线）一并隔离——瓶颈在覆盖率/多样性而非样本纯度。**归因纪律**：①③④ 为架构判定，直接采纳；②的「冒泡穿透误杀」在**我方样本上尚未证实**（当日 4 条 `ground_truth_count` 中已逐条确认的 2 条是 `no_match`＝目标文本不存在、无命中点，属设计内判非法），故条款 2 按「判据升维」实现，其收益须以**旧样本回放**验证，**不得在写归因时当作已验证事实**。

**条款**：
1. **采样时钟前移（Pre-dispatch Sampling）**：
   ① **时序状态机（唯一合法顺序）**：意图解析 → 候选池与执行目标确定 → **只读采样（GT 锚定 ＋ 影子打分）** → 派发 → 后验回读 → 落盘。**严禁在派发后取样**（含「收尾取样」）；Episode 的 `candidates[]` / `is_ground_truth` / `shadow_inference` 三者必须反映**派发前**页面状态。
   ② **采样点（落点映射，自决）**：`pkg/engine/service.go::runAction` 决策块结束之后、`ep.markDispatched()` 之前（决策目标已定、物理事件未发）。派发分支（`evaluateDirect` / `executeByNode` / `executePhysical`）之后不得再触发采样。
   ③ **两阶段只读**：Phase A 解析 **pivot 身份**（决策目标的派发前身份：优先命中 KB 规则的 `Selector`，缺省时用只读定位 `findElementOpt` 取节点；两者皆不可得 ⇒ pivot 缺失）；Phase B 以 pivot 为枢轴取页面侧快照 ＋ 锚定 GT ＋ 影子打分。两阶段**均不得发出任何 `Input.*` 域命令、不得改 DOM、不得触发导航**；采样异常**不得影响决策**（fail-soft：采样失败 ⇒ `candidates` 落空数组并如实标记，动作照常派发）。
   ④ **`candidates_source` 取值收敛**：订正后恒为 `page_projection`；第三值 `page_projection_after_nav` **废弃**（不再产生，常量保留仅供旧样本解析）；无只读快照能力时退回 `engine_pool`（第一批口径，保持既有测试不回退）。
   ⑤ **K=64 距离截断的枢轴**改用**派发前的决定目标坐标**（pivot 矩形中心）；无 pivot 时退化为文档序并置 `candidates_truncated=true`。
2. **GT 判据升维（DOM 子树包含 ＋ 几何兜底）**：`is_ground_truth` 的唯一性判定改为**三级判据**，按序判定、命中即止：
   ① **恒等**：命中元素与候选为**同一页内元素**（候选身份 `selector` 相等）⇒ 该候选为 GT；
   ② **祖先链包含（容忍冒泡穿透）**：自命中元素**向上 ≤ K=3 阶**祖先链中命中候选 ⇒ 该候选为 GT；多候选同时命中时以「**最近的可交互祖先**」消歧（阶数最小者；同阶取候选文档序），保证唯一；
   ③ **几何兜底**：无 DOM 关系（pivot 缺失 / 命中元素不在候选集祖先链上）时，退化为「矩形包含 → 矩形中心最近」（§5.0.13 条款 3⑤ 原判据，保留）。
   任何一级都无法判定 ⇒ **全 `false`**（不得猜、不得回填陈旧命中）；全样本恒**恰一条**或**零条**。K=3 为本版取值，改动须订正本条款。
3. **影子通道正交化（只读旁路，零副作用）**：
   ① **独立字段与独立入口**：影子打分器持于**独立字段** `Server.shadowScorer`，由**独立配置入口**构造（拟 `--shadow-onnx-model` 提供模型字节、共享库复用 `--onnx-runtime-lib`）；构造路径**不得**经过 `syncScorerWithKnowledge`、**不得写** `s.onnxScorer`、**不得把分数写进** `pageCandidatePool` 或任何决策带。影子打分**无状态**：只读候选集 → 结果只落 `shadow_inference`，不参与 `decision`、不写 `learned`。**单测硬断言**：构造影子 scorer 后 `s.onnxScorer` 仍为 `nil`、决策路径输出不变。
   ② **门禁边界（取代 §5.0.13 条款 1「一律不接线」）**：`T-3` / G-PROOF 只约束「**影子打分进决策带/候选池**」与**在线 learned 写入**；**只读影子旁路不受此门限制**。本条把「影子恒不接线」订正为「影子可接线、但必须正交」。
   ③ **降级如实**：无共享库 / 无模型 / 无 active artifact ⇒ `shadow_inference` 如实 `null`，由 `shadow_missing_total` 计数，**其它链路零变化**（不得因此跳过采样、不得影响 `candidates_source`）。
4. **消费端三级弹性录取（取代 §5.0.13 条款 5 的一刀切）**：离线消费端 [`pipeline/episode_consumer.py`](../pipeline/episode_consumer.py) 逐条**分级录取**，判定按 Tier1 → Tier2 → Tier3 顺序：

| 级别 | 条件 | 处置 | 权重 |
|---|---|---|---|
| **Tier1 黄金** | `len(candidates) >= 2` ∧ `sum(is_ground_truth) == 1` ∧ `shadow_inference != null`（且 `len(scores) == len(candidates)`） | 直接进蒸馏集 | `w = 1.0` |
| **Tier2 弱监督** | `len(candidates) >= 2` ∧ `sum(is_ground_truth) == 1` ∧ `shadow_inference == null` | 入库并**排入离线补打分队列**（进蒸馏集） | `w = 0.5` |
| **Tier3 阻断隔离** | `len(candidates) < 2` ∨ `sum(is_ground_truth) != 1` ∨ 捕获异常（不可解析 / 非法结构 / 影子分基数不一致） | 原样隔离到 `<episodes_dir>/quarantine/`（append-only，不改源文件）并记 `quarantine_reason` | — |

   ① 门槛 `len(candidates) >= 2` **照用户原文逐字执行**（自决并声明）：单候选页（页面恰有一个交互节点）判 **Tier3**；如需放宽为 `>= 1` 须由用户口径订正后改条款与实现。
   ② 计数保留 `quarantined_episodes_total`（隔离区唯一计数源，append-only 语义不变）＋ 新增 `tier_counts`（三级各计数）与 `weights`（按级别声明的权重）。
   ③ **严禁静默丢弃**（§5.0.12 条款 6）：Tier2 是**降权**不是丢弃；Tier3 是**隔离**不是当负样本。

**与 §5.0.13 的取代关系表**（逐条显式；未列条款原样有效）：

| §5.0.13 条款 | 处置 | 取代者 / 说明 |
|---|---|---|
| 条款 1（接线边界：影子**一律不接线**） | **被取代（放宽）** | §5.0.14 条款 3：影子可接线，但必须**正交旁路**；`T-3`/G-PROOF 只管「进决策带」与在线 learned 写入 |
| 条款 2⑤（`candidates_source` 第二批起恒为 `page_projection`，含 `after_nav` 取值） | **被取代** | §5.0.14 条款 1④：`after_nav` 废弃（不再产生，常量保留供旧样本解析） |
| 条款 3⑤（GT＝命中点与候选**矩形**对齐） | **被取代** | §5.0.14 条款 2：DOM 子树包含（≤3 阶）优先 ＋ 几何兜底 ＋ 最近可交互祖先消歧 |
| 条款 5（消费端三项代数条件**一刀切**隔离） | **被取代** | §5.0.14 条款 4：三级弹性录取（Tier1/Tier2/Tier3 ＋ 权重 ＋ `quarantine_reason`） |
| 条款 2①②③④、条款 3①②③④⑥、条款 4（脱敏/截断）、条款 6（差异订正表） | **原样有效** | 采样时点/GT 判据/影子接线/消费端分级之外的字段口径不变 |
| 「快照在收尾取」的实现决议（`Tasks.md`「第二批实现设计决议①」） | **被推翻** | §5.0.14 条款 1①：改为派发前采样 |

**契约符号 → 实现落点映射表**（用户 State A/B 中在本仓库不存在的符号，逐条映射，标**自决**）：

| 用户契约符号 | 实现落点 | 处置 |
|---|---|---|
| `pkg/engine/runner.go` / `DispatchWithPreProjection` | [`pkg/engine/service.go`](../pkg/engine/service.go) `runAction`（决策块后、`ep.markDispatched()` 前） | **落点映射**（自决） |
| `ResolveTarget` / `window.__uws_resolve_node` | [`pkg/browser/cdp_session.go`](../pkg/browser/cdp_session.go) `findElementOpt`（只读定位）＋ KB 命中选择器（`bestSelector`） | **落点映射**（自决） |
| `CapturePreDispatchSnapshot` | [`pkg/engine/episode.go`](../pkg/engine/episode.go) `episodeRecorder` 的 pre-dispatch 快照槽位（新增） | **落点映射**（自决） |
| `emitEpisode` | `episodeRecorder.finish` → `episodes.Append`（既有） | **改名等价** |
| `pkg/episode/types.go` | `pkg/engine/episode.go`（沿用 §5.0.13 条款 6 映射） | **路径映射** |
| 影子独立打分器 | `Server.shadowScorer`（新增字段）＋ `Server.shadowInference` 改读该字段 | **新增** |

**实现决议（条款 1/2 落地，自决并留痕）**：

0. **真值锚点＝物理受体（本批核心决议，用户裁定 2026-09-25）**：`is_ground_truth` 的枢轴必须来自**物理层在派发前解析出的受体**，不得来自候选池、KB 语义文本或任何引擎侧推断。三条定理：① 不取物理受体 ⇒ 候选池一旦为空（本机实测 `cand_count=0`：池只吸收 L2 同义词或经 ONNX 打分的 L1，而 ONNX 受 `T-3` 门未接线）枢轴恒空 ⇒ GT 恒 `false` ⇒ 新样本全部落 Tier3 ⇒ 「无 GT ⇒ 无样本 ⇒ 无评估集 ⇒ 无接线许可」的**飞轮冷锁**；② **语义文本唯一性伪命题**——用「Tag ＋ Text 页内唯一」猜测真值会引入不可控假正例并污染离线特征，**排除**；③ **物理受体回传完备性定理**——派发链在发出物理事件前必经受体解析（`click` 的 `findElementOpt`、`input/fill/select` 的 `locateStableElement`、`extract` 的 `findElementMode`、VLM 坐标的 `elementPathAt`），故物理层此刻必持有受体引用，而 `disp.mark()`（＝「已派发」分界，§5.0.11）是回传的**硬上界**：任何在 `mark()` 之后的回读都属事后取证，违反「GT 反映派发前状态」契约。据此 **否决**「维持现状（无候选池路径即无枢轴）」、**排除**「语义文本唯一退化」、**确立**「物理受体回传」为唯一物理自洽解。

1. **接口形态**：`browser.PageSnapshotSource.PageSnapshot(pivot PageSnapshotPivot)` 取代原 `(targetX,targetY)`；`PageSnapshotPivot` ＝ **受体派发点**（`HasPoint/PointX/PointY`，即决议 0 的物理受体，经 `browser.PreDispatchReceptorSink` 在派发前回传）＋ **补充身份**（`Selector/Tag/Text`，仅在无受体点时可用的兜底）。`browser.PageSnapshot` 回带 `GroundTruthIndex`（哨兵 `PageSnapshotGroundTruthUnknown = -1`）。K=64 截断与几何兜底的锚点**优先取受体派发点**；无受体点时退回补充身份解析出的元素矩形中心。
2. **GT 三级判据落在页内**（`pkg/browser/cdp_session.go::pageSnapshotJS`）：判据需要 `elementFromPoint` ＋ 祖先链，只有页内可得；原 Go 版几何判定 `episodeGroundTruthIndex` 及其单测删除。命中代理＝枢轴矩形中心处最深元素（`elementFromPoint || pivot`）。**覆盖证据**：自建 headless Chrome 夹具 **12 例**全过（订正批⑤ 扩例；脚本为临时产物 `/private/tmp/wtx-gt/harness.mjs`，不入仓），关键甄别例＝「两候选矩形都含命中点，祖先链判出 `Anc`（index 1）而非几何兜底的文档序首候选」＋「祖先恰 3 阶（含）命中、4 阶外不命中」＋「**受体派发点落 `<path>`/`<svg>` ⇒ 祖先链命中按钮（index 1），若走几何兜底只会给文档序首候选**」＋「**受体派发点落空白 ⇒ `gt_index=-1`（不编造 GT）**」；另覆盖 SVG/Span 穿透、同文本多节点、选择器不匹配、零候选页、几何兜底、最近可交互祖先。
3. **pivot 来源：受体为唯一正解**（替代原「三段式」）：页内定位**先**用受体派发点（`elementFromPoint(PointX, PointY)` 得受体元素，命中代理＝该元素）；补充身份降级为**兜底**——`querySelector(Selector)` → 「Tag ＋ Text 且页内唯一」（扫描上限 2000 元素，被上限截断即判不唯一），仅在**无受体派发点**时启用。**绝不接受「多节点取第一个」**，**绝不以语义文本唯一性猜测真值**（决议 0②）。**受体模式下取消「矩形中心最近」几何猜测**（订正批⑤，自决并声明）：受体落空时唯一诚实答案是 `gt_index=-1`（GT 恒 0 条），用最近候选中心「猜」即**编造 GT**，与决议 0② 同源；该取消已在 `pageSnapshotJS` 落成守卫，并由夹具 `receptor-on-blank-all-false` 证明。
4. **无 pivot 时**：候选退化文档序并**恒置** `candidates_truncated=true`；GT 全 `false`（页内返回 -1）。
5. **采样失败 vs 无能力**：现有接口下两者同号（`ok=false`）⇒ 一律退回 `engine_pool` 并如实标记来源（条款 1④ 优先）；条款 1③ 的「落空数组」仅指「采样成功但页内 0 候选」。
6. **采样点唯一，且严格在派发前**：`runAction` 的 `wstnShadowAudit` 之后、`ep.markDispatched()` 之前；**(c) 修订**——具备 `browser.PreDispatchReceptorSink` 能力的会话，采样下移到物理层「受体解析后、物理事件发出前」（`disp.mark()` 之前），`runAction` **不再**在旧点采样，以免抢跑消耗唯一幂等槽使受体采样失效；无该能力或走直发通道（drag/evaluate/node_id）的会话维持旧点采样一次（无受体点 ⇒ 文档序 ＋ GT 全 false），成本与旧版收尾取样相同。
7. **VLM 兜底路径覆盖缺口（已随决议 0 (c) 收敛）**：决策带 NOT_FOUND 的四处 `vlmAtNotFound` 在采样点之前 return，而其物理目标由 VLM 在派发时依截图决定——引擎侧派发前**没有**可锚定的 pivot，若拿被拒候选冒充会**编造 GT**，故这些 Episode 落 `engine_pool`（无 GT ⇒ 消费端 Tier3 隔离）。**【已随决议 0 (c) 收敛】**：`vlmAtNotFound` 只在**决策带**（`runAction`）内 return，并不改变物理派发链——真正发出事件的仍是同一会话，其派发前必经受体解析（VLM 坐标点击走 `elementPathAt`），故 (c) 落地后这些 Episode 同样取得受体派发点、不再恒落 `engine_pool`。剩余未覆盖情形＝「在解析受体**之前**即失败」（坐标落空白、无元素可解），此时如实退回 `engine_pool`。
8. **影子同点观测**：采样成功时 `shadow_inference` 直接取采样时刻读数；未采样（退回 `engine_pool`）时按既有口径重算。`page_projection_after_nav` 常量仅保留供旧样本解析。

9. **候选空集轻量重试（`INT-218②`，自决并声明）**：采样时若页内快照**成功但候选为 0**（＝条款 1③ 的「落空数组」），允许在**同一采样槽内**做轻量等待重试，上界 **≤2 次 × 400ms 退避**；重试后仍为 0 ⇒ **如实落空集**（`candidates_source=page_projection`、`candidates=[]` ⇒ 消费端 fail-closed），**绝不编造候选、绝不改判 GT**。硬边界：① **不改变采样时点**（仍严格在派发前）；② **不改变唯一性**（同一 Episode 仍只采样一次，重试属该次采样内部行为，幂等槽不被抢占）；③ **不触碰决策路径与影子通道**（采样异常一律 fail-soft）。**快照失败不重试**（`ok=false` 与「无能力」同号，见决议 5；对无能力会话重试纯属浪费预算）。**归因留痕（本机 88 条 Episode 实测，2026-09-25）**：空集样本 **47/47 全部来自「未采样」类**（`engine_pool` 46 ＋ 旧口径 `page_projection_after_nav` 1），其中 **18 条为 `canceled_before_dispatch`（未派发）**；**0 条**为「投影跑过但 0 候选」⇒ 本条在本机语料上**命中 0 次**，性质是**保险性**改动（防晚水合 SPA 的未观测类），**不得**据此宣称采集录取率提升。

**验收（第二批订正批）**：① 真机上**导航型 `click` 亦能取到决策时候选**（不再出现 `page_projection_after_nav`）；② `is_ground_truth` 恰一条，且**含 `SVG` / `Span` 冒泡穿透**的点击可正确定位；③ 构造影子 scorer 后 `s.onnxScorer` 仍 `nil`、决策路径输出不变（单测断言）；④ 消费端三级录取有单测 ＋ 用**当日真实 17 条样本**回放对比存活数（**读数以实测为准，不得预设结论**）；⑤ `gofmt` / `go vet` / `go test`（18 包）/ `pytest` 全绿；先红后绿；⑥ `Tier2` 权重 `0.5` 与离线补打分队列在统计中可见。**不修改任何冻结参数**（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02`）。

---

### 5.1 决策阈值

- `θ_high = 0.85`：直接执行。
- `θ_low = 0.50`：LLM兜底（受预算看门狗控制）。
- `ε = 0.02`：探索概率。

### 5.2 影子评估毕业条件（★动态共轭先验）

废除全局常量。根据节点**拓扑独特性**和**业务风险**动态调整伪计数：

```python
# 拓扑孤岛（兄弟节点极少，低风险）：α=0.1, β=0.1 → 1次命中即可毕业
# 高危区域（支付/提交，或密集列表）：α=3, β=2 → 需9次连续命中
def dynamic_prior(node):
    siblings = count_visible_siblings(node)
    if is_high_risk_zone(node) or siblings > 5:
        return (3, 2)
    elif siblings <= 1 and not is_high_risk_zone(node):
        return (0.1, 0.1)
    else:
        return (1, 1)  # 默认拉普拉斯

score = (equivalent_count + α) / (matched_count + α + β)
```
激活次数≥20，score≥0.85且保持5天无衰退，方可转为ACTIVE。

---

### 5.0.15 ★【2026-09-30 追加】`T-30` 决策时点候选池与排序位序落盘契约（候选块复选的数据前置）

**背景（为什么需要）**：`T-30` 候选块复选的形状是「**引擎给候选集 → 模型做离散选择**」，其数据前置是「决策时刻引擎看了哪些候选、按什么序看」。已确证前半：候选池有算法（[`pkg/browser/cdp_session_eval.go`](../pkg/browser/cdp_session_eval.go) `PageCandidates()` → [`pkg/engine/service_engine.go`](../pkg/engine/service_engine.go) `candidatesFor` / `pageCandidatePool` 的 L1 置信度 / L2 语义规则 / L3 结构校验），真实页面池**中位 49–62 个**、**约四到五成顶到 K=64 截断** ⇒ 不能全裁、必须按排序取前 K。**后半缺失**：决策时刻的候选池与位序**无任何落盘** —— Episode 的 `candidates[]` 是**页面侧锚点投影**（[`pkg/browser/cdp_session_snapshot.go`](../pkg/browser/cdp_session_snapshot.go) 条款 3③：超 64 按「距受体派发点欧氏距离」升序保留），实测 176 条有 GT 样本中 **166 条 GT 的 `index`=0** ⇒ 拿它算召回会得到**虚假高命中**；`~/.uws/engine.log` 的 `cand=` 行来自 `[AX-SHADOW]` 影子通道（非主候选池，只有计数无位序）。缺的不是「有没有候选」，而是「**决策时按什么序看它们**」——这也正是 DL Lab 202609 C 域（候选序列 → 恰选一个）所需的事件形状（跨工作区**只对齐形状**，不复用其数据）。本条款只定义**落盘**，不改变任何决策行为。

**条款 1｜决策池投影（additive 字段）**：
1. 新增 Episode 顶层字段 `decision_pool`（`[]EpisodeDecisionCandidate`）：**永不为 `null`**，可以为**空数组**（与 `candidates[]` 同纪律）。
2. 每条元素字段（`EpisodeDecisionCandidate`）：`rank`（0 基**决策序**位序）／`stage`（`L1` \| `L2`，沿用 `matcher.Candidate.Stage`）／`source`（入池通道：`page_l1` \| `page_l2_kb` \| `semantic_direct`）／`confidence`（入池置信度，未打分路径为 0）／`selector_hash`（**只落哈希**）／`struct_hash`（已是哈希，原样）／`tag`／`role`／`text` ＋ `text_rune_len` ＋ `text_truncated`（**与 `candidates[]` 同一 `clip` 口径**：`episodeMaxTextRunes=32`、`clip()`、正则脱敏 —— 不新增任何泄漏面）／`is_selected`（本条是否**本次实际派发目标**；至多一条 `true`，未派发或目标不在池内时全 `false`，**不得**回填陈旧命中）。
3. **`is_ground_truth` 不复制进 `decision_pool`**（[自设]）：GT 的唯一合法判据仍在页面侧 `candidates[]`（§5.0.13 条款 3⑤ ＋ §5.0.14 条款 2 三级判据）；两处各存一份会使「唯一性」出现两个会漂移的事实源。决策池与 GT 的关联由离线端按 `selector_hash` 联结。
4. `source` 取值口径（在**入池落点显式标注**，由 `matcher.Candidate` 新增 `Source` 字段承载）：`page_probe` ＝ 页面候选自带显式置信度入池（探针 / 特征打分通道）／`page_onnx` ＝ 无显式置信度的页面候选经在线 ONNX 打分入池／`page_l2_kb` ＝ 页面候选经离线背书同义词入池／`semantic_direct` ＝ 无页面候选时 `candidatesFor` 的 `MatchQuery` 语义直查回退（**不得猜、不得留空**：确实无来源时落空串，离线端按未知处理）。
5. `production` 增补两个如实计数：`decision_pool_count`（落盘条数）与 `decision_pool_truncated`（是否被条款 2 的上限截断）。

**条款 2｜决策序定义（唯一合法排序）**：
1. **决策序** ＝ 引擎决策时对该次动作候选池采用的选举次序，复现口径为：先按 `confidence` **降序稳定排序**（`sort.SliceStable`，与 [`pkg/matcher/simhash.go`](../pkg/matcher/simhash.go) `RefineByStructureDual` / `refineTextChannel` 用于 top-K 选举的 `order` **同源同比较器**），同位序保持**池内输入序**（页面候选预序遍历序 ∪ 语义直查序）。`rank` ＝ 该序中的 0 基下标。
2. **与返回序的区分（实现硬性标注）**：`candidatesFor` 的 `append(kept, l2...)` 是**返回序**（L1 保留集在前、L2 其后），**不是**决策序；本条款要求落盘的是**决策序**，两者不一致时**以本条款为准**，实现必须就地注释标明。
3. **确定性**：同页 ＋ 同意图 ＋ 同候选集 ⇒ 同 `decision_pool`（含 `rank`）。
4. 落盘上限 `K_decision = 64`（与页面侧投影 K 一致，便于对拍；[自设]，非新定参数）：超限按**决策序前 64 条**落盘并置 `decision_pool_truncated=true`。截断必须确定性；**不得按文本去重**（同文案多节点是真实候选，§5.0.13 条款 3⑥）。

**条款 3｜与既有契约的边界（互不污染）**：
1. `decision_pool` **不参与**消费端 Tier1/Tier2/Tier3 录取判定 —— §5.0.14 条款 4 判据**原样**只看 `candidates[]` / `is_ground_truth` / `shadow_inference`。
2. **`CON-205` 原样**：全空 `candidates[]` 严禁入蒸馏集，**不因** `decision_pool` 非空而放宽。
3. **决策行为零变化**：本条款是**纯增量观测**。fail-soft：落盘失败只记日志，不得影响决策、不得影响 `candidates[]`、采样时点或影子通道。
4. `episode.v1` **不升版**（additive 字段先例＝§5.0.13 条款 7）。旧样本缺该字段 ⇒ 视为**未落盘**（`null` / 缺省），**不得**当作空池统计（同 `engine_pool` 遗留口径的教训）。
5. 池为空（canvas / 纯图页：DOM 无文字、无可点击叶子）时落**空数组** ＋ `decision_pool_count=0`：该情形**不得**据此判样本非法（GT 判据仍只看 `candidates[]`），但**不得**外推「候选块复选可用」——该类页面直接按条款 4④ 跳层。

**条款 4｜定位层级 fallback 阶梯（2026-09-30 用户口径，本条款固化）**：
1. 阶梯（**内层用尽才外扩**）：**确定性 → learned → VLM 坐标 → 候选块复选（`T-30`）→ 整页 VLM → 人**。
2. 跳「整页 VLM」的判据：**排序没把握 / 池为空 / 超时**（用户原话「如果这一步你自己都没有把握，那只能整图喂过去」）。
3. 「**排序没把握**」的量化口径**待定**，须以 `decision_pool` 的**位序分布读数**定义（候选：Top-K 累计置信度占比、前 K 与第 K+1 名分差）；在量化阈值经读数固化前，实现**不得**自行猜测该阈值（[自设]：避免把未验证阈值写死成产品行为），该期间按第 2 条的定性判据（池为空 / 超时 ＋ 引擎当前是否已有可信目标）执行。
4. **canvas / 纯图页**（`decision_pool` 恒空）**直接跳到整页 VLM**，不进入候选块复选。

**条款 5｜K（裁几块）取值纪律**：
1. 候选块复选的 **K 不在本条款规定**：须由 `decision_pool` 的**现场位序分布实测**确定（真目标落在决策序第几位），读数落 `docs/evidence/` 案卷后再回写本节。
2. **未取得读数前，不得以任何 K 实现候选块复选出口** —— 避免把「未验证的 K」写死成产品行为。

**条款 6｜离线基线事实（实测，2026-09-30 深夜；复跑见案卷 §8.5）**：
1. **两个「池」必须点名区分**：`production.page_candidate_count` ＝ **页面侧候选集**（只读快照的交互节点，**锚点序**截断到 64；真实页面中位 30–62、封顶 64）；`production.cand_count` ＝ **引擎决策池**（`candidatesFor` 出的匹配器候选）。二者**不是一回事**，历史文档里「池很大」指的是前者。
2. **真实页面上决策池几乎恒空**：`page_projection` 样本决策池 **0 率 82–97%、中位 0、最大 4**；决策池非空的仅 24 条（`ASTAR` 23 ＋ `DIRECT` 1）。机制（读码可得）：`pageCandidatePool` 对 `Confidence=0` 的真实 CDP 节点**只吸收 L2 离线背书同义词**（L1 要求探针显式置信度），公开页无 KB 规则即空池；在线 ONNX 打分**未接线**（条款 1 的接线边界，越 `T-3` / G-PROOF 门）。
3. **因此本条款的落盘对象（决策池 + 决策序）只是「引擎看了什么」的如实记录，不等于 `T-30` 的候选来源**：候选块复选的候选来源是**页面侧候选集**，而它今日唯一的序是**锚点序**（距受体派发点欧氏距离）——**锚点＝已选中的目标**，用它排「该问哪几块」是**循环依赖**。
4. ⇒ **`T-30` 的实质缺口 ＝ 页面侧候选集缺一个「与目标相关性」排序器**（不是位序没落盘一件事）。选型三选一（**均未验证**，且①/②不得越过条款 1 的接线边界）：① 复用现有 `matcher` 打分（5 维特征 ＋ 角色启发式 ＋ L3 结构）**对页面节点开口**；② 视觉模型产相关性分（＝ DL Lab 视觉-决策组合位置）；③ 不排序，按可见性/面积/文本长度粗筛 ＋ 小 K 硬问。
5. **不得**据第 2 条把「决策池为空」判成缺陷或回退理由：空池下决策核按既有路径走（classifier / VLM），本条款不改该行为；只要求**如实记录**（`decision_pool_source` 区分 `matcher` 与 `skipped`）。

**裁决与自设登记（[自设] 项）**：① `decision_pool` 不携带 GT 标记（单一事实源）；② `K_decision=64` 沿用页面侧 K 以便对拍；③ `source` 在入池落点显式标注（`matcher.Candidate.Source`），不由离线端反推；④「排序没把握」阈值待读数固化。

**本版显式缺口（不得当作已实现）**：**几何（`x`/`y`/`w`/`h`）不在本版 `decision_pool` 内** —— 匹配器候选 `matcher.Candidate` 不携带矩形，页面侧矩形只存在于只读快照 `PageSnapshotNode`（即 `candidates[]`）；补几何需给 [`pkg/browser/interfaces.go`](../pkg/browser/interfaces.go) `PageCandidate` 增 rect 并由 [`pkg/browser/cdp_session_eval.go`](../pkg/browser/cdp_session_eval.go) 的 `collectJSCandidates` JS 回传（该 JS 已算出 rect、仅做可见性过滤后丢弃）。**位序分布测量不需要几何**（序 ＋ 身份 ＋ 是否命中即可），故本版先不补；真正要按位序裁块时再补，补前不得假设 `decision_pool` 有几何。


**取证事实源**：[`docs/evidence/vlm-detail-resolution-ab-2026-09-30.md`](../docs/evidence/vlm-detail-resolution-ab-2026-09-30.md) §7（裁块清楚度与长页面）／§8（裁谁、裁几个、位序缺口）。

---

### 5.0.16 ★【2026-10-01 追加】CDP 原生检索粗锚点自举契约（D11 / T-30 第二刀 Phase 0）

**背景与病灶（难点册 D11）**：长页面（如 Wikipedia 672 个候选节点、1440×6800 视口）中，若无受体点与补充身份（`hasAnchor = false`），页面侧候选投影（`PageSnapshot`）退化为按文档预序遍历截断（$K=64$），导致 64 个候选全部挤在视口顶部 10.7%（$y=17\sim 725$），深水区候选（如 $y=4017$ 的第 279 位目标）在进入排序器前就被物理切掉。根本病灶在于将「排序参考原点（Guidance Origin）」与「最终物理受体（Physical Receptor）」混为一谈——欧氏距离截断只需要一个粗置信域即可自举局部性。

**条款 1｜零副作用 CDP 原生检索原语**：
1. **绝对禁止使用 `window.find()`**：`window.find()` 会改变全局文本选区（Selection）并强制触发浏览器原生平移滚动（auto-scroll），直接破坏视口与焦点几何，严重违反 R1 零打扰红线。
2. **CDP 原语下发**：一律采用 CDP `DOM.performSearch` ＋ `DOM.getSearchResults` ＋ `DOM.getBoxModel`，由 Blink 内部 C++ 在后台无损检索（支持穿透 DOM 与 Shadow DOM），提取匹配节点盒模型的视口中心坐标作为粗锚点；检索完成后立即调用 `DOM.discardSearchResults` 释放 Blink 内部缓存，防止内存泄漏。
3. **指标硬红线**：0 Token、耗时 $\le 2$ms，纯只读、零选区污染、零页面跳动。

**条款 2｜§5.0.2 语义别名展开集联动**：
1. **别名归一化展开**：检索目标不单是字面量，必须前置消费 §5.0.2 的别名展开集合 $C(s) = \text{semantics.Wants}(s)$（例如 `search_box` 展开为 `["搜索", "搜尋", "search", ...]`）。
2. **有序检索短路**：按别名集合有序依次检索，只要任一别名在页内产生可见盒模型（$w \ge 2 \land h \ge 2$）命中，即提取其中心点坐标 $(P_x, P_y)$ 作为粗锚点（Coarse Point）；命中多个有效盒模型时取首个。
3. **安全回退**：若所有别名均未检索到可见盒模型，则 fail-soft 维持原无锚点行为，绝不造假。

**条款 3｜解耦排序原点与物理受体（边界互不污染）**：
1. **排序参考原点（Guidance Origin）三级优先级**：物理受体派发点（Receptor）优先；无受体点时退化为补充身份（Selector → Tag ＋ Text 且页内唯一）；两者皆无时退化为 CDP 原生检索粗锚点（Coarse Point）。三者任一存在，即可恢复欧氏距离排序（局部性优先），将深水区目标召回至 Top-64 窗口。
2. **GT 判定隔离公理（严禁污染离线样本）**：CDP 原生检索粗锚点仅用于候选排序截断，**绝对不作为 Ground Truth（GT）判据**。页内三级判据（§5.0.14 条款 2）仅在具备物理受体派发点或精准补充身份时激活；仅有粗锚点时 `GroundTruthIndex` 恒为 `-1`（未知），严禁将粗锚点冒充为物理受体标记 GT，坚决执行 G-PROOF 严选训练样本门禁。

---

### 5.0.17 ★【2026-10-01 追加】AX 地标剪枝与标题树空间索引契约（D11 / T-30 第二刀 Phase 1）

**背景与病灶（难点册 D11 拓扑缺陷）**：
1. **DOM 预序先验偏置**：现代网页架构中，页眉（`<header>` / `[role="banner"]`）、全局导航栏（`<nav>` / `[role="navigation"]`）、全局搜索栏、侧边栏、语言选择器等外围交互元素在 DOM 预序遍历中天然占据前部排位。
2. **深水区 0% 失明**：在完全无目标锚点（既无受体派发点、又无精准补充身份、CDP 具象搜索亦未命中）时，朴素文档序截断（$K=64$）造成候选全集被视口顶部 10.7%（Wikipedia 实测 $y=17\sim 725$）的外围导航元素完全占满；真正承载业务核心功能的正文区（`<main>` / `[role="main"]` / `<article>`）占全页 89.3% 的有效交互节点被 100% 物理截断排除。
3. **结构化空间路标缺失**：长页面普遍具备清晰的章节标题层级（`H1~H6` / `[role="heading"]`）。当目标意图指向章节主题或无精确叶子文案时，缺乏自顶向下的纵向空间路标，导致局部性排序无法自举。

**条款 1｜AX Landmark 地标分类与配额保护机制（Landmark Quota Reservation）**：
1. **地标分类映射**：只读快照遍历交互元素时，通过最近语义祖先节点判定所属的地标区域属性 `landmark`（单向确定性）：
   - `main`：祖先包含 `main`、`[role="main"]` 或 `article`；
   - `nav`：祖先包含 `nav` 或 `[role="navigation"]`；
   - `header`：祖先包含 `header` 或 `[role="banner"]`；
   - `footer`：祖先包含 `footer` 或 `[role="contentinfo"]`；
   - `aside`：祖先包含 `aside` 或 `[role="complementary"]`；
   - `other`：未匹配以上语义容器的其他区域。
2. **配额保护（Quota Reservation）**：在无锚点（`!hasAnchor`）候选截断时，总窗口 $K=64$ 严格执行主内容保护分区：
   - 主内容区保底配额 $K_{\text{main}} = 48$（占 75% 预算）；
   - 外围辅助区配额 $K_{\text{other}} = 16$（占 25% 预算）；
   - **双向弹性回拨**：若 `main` 区内候选不足 48，剩余名额自动回拨给 `other`；若 `other` 区内候选不足 16，剩余名额自动回拨给 `main`；若全页无显式 `main` 容器，则全局平滑退化为单区，绝不产生名额浪费。

**条款 2｜H1~H6 标题树空间路标锚点自举（Heading Landmark Anchor Bootstrap）**：
1. **标题树提取**：快照同步提取页内可见标题元素（`h1, h2, h3, h4, h5, h6, [role="heading"]`），记录规范化文本、层级 `level`（1~6）及物理中心坐标 $(x_h, y_h)$。
2. **章节意图加权自举**：当无物理受体与精准粗锚点时，若目标语义 $S$（或其别名展开集 $\text{Wants}(S)$）与页内任一标题文本匹配（字面完全匹配或互含）：
   - 选取匹配度最高（完全匹配优先；同等条件下按文本长度差最小、层级最高、DOM 序最先消歧）的标题中心坐标 $(x_h, y_h)$ 作为**标题空间路标锚点（Heading Landmark Anchor）**；
   - 恢复欧氏距离排序：以 $(x_h, y_h)$ 为参考原点计算各候选欧氏距离，将该章节邻近的候选优先召回至 Top-64 窗口。
3. **样本隔离铁律**：标题路标锚点严格等同于 Guidance Origin，**绝对严禁参与 Ground Truth（GT）判定**。三级 GT 判据保持封闭，仅在具备物理受体或精准补充身份时激活，标题锚点激活时 `GroundTruthIndex` 恒为 `-1`。

**条款 3｜深水区分层均匀采样（Stratified Depth Sampling）**：
1. **触发边界**：当完全无锚点（既无物理锚点、CDP 粗锚点，又无标题匹配）且 `main` 候选超出配额（$>48$）时激活。
2. **等距步进分层**：不得直接取 `main` 的前 48 个 DOM 节点（否则仍会挤在 `main` 顶部前段）。在 `main` 区域内候选按物理纵坐标 $y$ 升序排列，采用系统等距抽样（Systematic Stratified Sampling，步长 $\text{step} = N_{\text{main}} / 48$），抽取分布于全深度区间的 48 个候选，强制覆盖 $y_{\min} \dots y_{\max}$ 全域，确保深水区 100% 存在感知候选。

**条款 4｜字段协议与向后兼容**：
1. `PageSnapshotNode` 新增 additive 字段 `Landmark string`（取值 `main` / `nav` / `header` / `footer` / `aside` / `other`），供离线 Ranker 作为 10 维特征之 `landmark_zone`；
2. `PageSnapshot` 新增 additive 字段 `Headings []PageHeading`（包含 `Text`, `Level`, `Tag`, `X`, `Y`）；
3. `episode.v1` 协议保持不变，老旧快照反序列化安全兼容。

**条款 5｜只读与零副作用硬红线**：
1. 地标判定与标题树索引完全在单次快照内通过只读 DOM 查询（`closest`, `querySelectorAll`）完成；
2. 严禁使用任何 `window.find()`、选区修改、触发平移滚动或修改 DOM，严格守住 R1 零打扰红线。

---

### 5.0.18 ★【2026-10-01 追加】ONNX Ranker 10 维特征与端到端页面候选排序契约（D11 / T-30 第二刀 Phase 2）

**背景与病灶（难点册 D11 排序与决策池断裂）**：
1. **决策池空率高达 82%~97%**：`pageCandidatePool` 对未打分（`Confidence=0`）的真实 CDP 节点此前仅吸收 L2 同义词命中，无 KB 规则的公开页导致引擎决策池绝大多数时刻为空（中位数为 0，最大仅为 4），造成 A* 搜索无根节点可供展开。
2. **纯字面 5 维特征表达能力匮乏**：原 5 维特征仅限于字面文本对（`simhash_sim`, `jaccard`, `exact_match`, `substring`, `log_len`），完全缺失 DOM 结构身份（Tag/Role）、视觉显著度（盒模型几何尺寸与面积）、上下文回响（骨架/父子文本与别名）、AX 地标区域（Landmark Zone）及空间相对距离衰减（Spatial Distance Decay）。
3. **L3 物理校验单一 SimHash 汉明距离一刀切误杀**：`RefineByStructure` 曾对全部 L1 候选强求 SimHash 汉明距离 $\le 5$。对于动作意图与按钮文案字面不直接重合的长尾场景，L3 校验将全部有效候选剔除为假 NOT_FOUND。

**条款 1｜10 维多模态特征向量契约（Feature Vector Schema）**：
ONNX Ranker 输入为固定 **10 维 float32 特征向量**（候选节点上下文 $\times$ 动作意图），输入 Tensor 形状为 `[batch_size, 10]`：

| 序号 | 特征名称 | 取值范围 | 计算口径与数学定义 |
|---|---|---|---|
| `f0` | `simhash_sim` | $[0.0, 1.0]$ | 候选文本与目标语义的 64-bit SimHash 相似度：$1 - \text{Hamming}(H(c), H(t)) / 64.0$。 |
| `f1` | `jaccard` | $[0.0, 1.0]$ | 候选与目标经 $\text{\_TOKEN\_RE}$ 分词后的 Token 集合 Jaccard 相似度：$\|T_c \cap T_t\| / \|T_c \cup T_t\|$（并集为空时为 0）。 |
| `f2` | `exact_match` | $\{0.0, 1.0\}$ | 小写且去除首尾空白后文本完全匹配：$c == t \Rightarrow 1.0$；否则 $0.0$。 |
| `f3` | `substring` | $\{0.0, 1.0\}$ | 目标语义是候选文本子串或候选文本是目标语义子串：$t \in c \lor c \in t \Rightarrow 1.0$；否则 $0.0$。 |
| `f4` | `log_len` | $[0.0, 1.0]$ | 候选文本 Rune 码点数对数归一化：$\min(\ln(1 + \text{RuneCount}(c)) / 8.0, 1.0)$。 |
| `f5` | `role_compat` | $[0.0, 1.0]$ | 元素标签与角色（Tag/Role）与意图动作类型的契合度：<br>• 点击意图（Click/Select）：`button`, `a`, `[role=button]`, `[role=link]`, `[role=tab]`, `input[type=button\|submit]` $\Rightarrow 1.0$；`input` $\Rightarrow 0.4$；普通文本叶子 $\Rightarrow 0.2$；其余 $\Rightarrow 0.0$；<br>• 输入意图（Input/Fill）：`input`, `textarea`, `[role=textbox]` $\Rightarrow 1.0$；其余 $\Rightarrow 0.0$；<br>• 通用意图：交互性元素 $\Rightarrow 0.8$，普通叶子 $\Rightarrow 0.3$。 |
| `f6` | `visual_prominence` | $[0.0, 1.0]$ | 视觉几何显著度：元素物理宽 $W$ 与高 $H$（CSS 像素）。<br>• 不可见或折叠（$W \le 0 \lor H \le 0$）$\Rightarrow 0.0$；<br>• 有效面积 $\text{area} = W \times H$：$\min(\log_{10}(1 + \text{area}) / 6.0, 1.0)$（保证 100×30 按钮得 $\approx 0.58$）。 |
| `f7` | `context_echo` | $[0.0, 1.0]$ | 上下文与别名回响度：<br>• 命中 §5.0.2 别名展开集 $C(S)$ 或父祖/骨架邻居文本包含目标意图 $\Rightarrow 1.0$；<br>• 上下文与意图存在 Token 交集 $\Rightarrow 0.5$；<br>• 其余 $\Rightarrow 0.0$。 |
| `f8` | `landmark_zone` | $[0.0, 1.0]$ | 所属 AX 地标区域先验偏置（呼应 §5.0.17）：<br>• `main` / `article` $\Rightarrow 1.0$；<br>• `nav` $\Rightarrow 0.6$；<br>• `header` / `aside` $\Rightarrow 0.4$；<br>• `footer` / `other` $\Rightarrow 0.2$。 |
| `f9` | `spatial_distance_decay` | $[0.0, 1.0]$ | 空间相对距离衰减：与自举粗锚点或标题路标坐标 $(x_0, y_0)$ 的相对欧氏距离 $d$：<br>• 具备参考原点时：$1.0 / (1.0 + d / 1000.0)$；<br>• 无参考原点时：保持居中平滑先验默认值 $0.5$。 |

**条款 2｜pageCandidatePool 全量 CDP 节点放开打分与兜底 Scorer**：
1. **放开入池门禁**：`pageCandidatePool` 对真实 CDP 会话采集的未打分（`Confidence=0`）节点，全面放开打分准入，不得再仅吸收 L2 同义词命中；
2. **多级 Scorer 路由**：
   - 首选：若已配置加载 `Server.onnxScorer`，调用 ONNX 模型执行 10 维推理，置信度标记为 `score`，来源标记为 `page_onnx`；
   - 确定性降级（Heuristic Fallback Scorer）：若未配置 ONNX 模型（`onnxScorer == nil`），由内置确定性多模态启发式评分函数（10 维特征线性加权融合：$\sum w_i f_i$）进行确定性打分，置信度标记为融合分，来源标记为 `page_heuristic`；
3. **消除空池缺陷**：保障公开页真实 DOM 节点稳定进入决策池，决策池非空率由 3% 跃升至 $>80\%$。

**条款 3｜L3 结构校验弹性准入（Elastic L3 Refinement）**：
1. **多通道融合准入**：L3 校验升级为「双通道结构校验 ∪ 高分 Ranker 保护通道」；
2. **高分免剔除**：当候选经 ONNX / Heuristic Scorer 打分且置信度 $\ge \theta_{low} (0.50)$，或处于全页 Ranker 排序 Top-4 时，不得仅因字面 SimHash 汉明距离 $>5$ 而被 L3 判定剔除；
3. **假 NOT_FOUND 归零**：杜绝候选池因 L3 严苛过滤被清空进而导致动作流产。

**条款 4｜严选样本训练与模型持续进化**：
1. **训练样本来源**：模型重训样本严格来自 G-PROOF 落点自证（§5.0.9 条款 6/7）校验通过（`SUCCEEDED`）的历史真实 Episode，严防同义反复样本与被污染的粗锚点进入训练集；
2. **模型体积与延迟**：LightGBM 模型参数冻结为 `num_leaves=31`、模型字节 $<1\text{MB}$，ORT 推理延迟 $\le 5\text{ms}$；
3. **向后兼容性**：保留老版本纯文本 5 维调用签名，缺省属性自动补充中性默认值，确保现有单测与回放无损运行。

---

### 5.0.19 ★【2026-10-01 追加】Top-4 候选块裁块四宫格与 VLM 离散选择（A/B/C/D）契约（D11 / T-30 第二刀 Phase 3 收口）

**背景与病灶（难点册 D11 / 案卷 `vlm-detail-resolution-ab-2026-09-30.md`）**：
在长页面整页截图场景中，长边被压至 2048px 时文字有效像素缩至仅 2.7px/字，整页 VLM 坐标回归产生 $+78$px 漂移导致 22px 按钮出界 miss；而在相同页面上，截取局部 1:1 原生分辨率候选块并打上 A/B/C/D 标签让模型做离散选择，实测准确率达到 3/3（100%），且 Token 代价较整页节省 65%（495 vs 1431 tokens）。
在 Phase 2 完成 10 维多模态 Ranker 升级与放开真实 CDP 节点入池后，真实页面上真值目标已能以高置信度排入候选池前列。本契约作为 T-30 第二刀的最终闭环，确立「Top-4 候选块裁块拼接 ＋ 离散选择 A/B/C/D」原语，彻底取代易出界的坐标回归。

**条款 1｜输入空间与准入门禁（Admission Gate）**：
1. **触发时点**：位于定位阶梯第 3 层（介于 Ranker 排序与整页 VLM 之间）。当确定性规则与 learned 未命中，且候选池非空时触发；
2. **置信度判定与分流**：
   - 若候选 Top-1 置信度绝对碾压（$\text{Confidence} \ge \theta_{high}=0.85$ 且与 Top-2 分差 $> 0.15$），直接执行确定性派发，不消耗视觉预算；
   - 若候选池为空（纯 Canvas / 纯图页），或 Top-1 置信度极低（$\text{Confidence} < \theta_{low}=0.50$），直接跳过本层，退入第 4 层整页 VLM 兜底；
   - 若 Top-1 置信度处于模糊区间 $[\theta_{low}, \theta_{high}) = [0.50, 0.85)$，或 Top-1 与 Top-2 存在歧义（分差 $\le 0.15$），强制激活候选块复选闭环；实现入口仍需满足条款 1.1 的 deterministic/learned 未命中条件；
3. **前 K 截取**：按决策序（置信度降序稳定排序）至多截取前 $K = \min(\text{len}(\text{cands}), 4)$ 个候选（$1 \le K \le 4$）。

**条款 2｜原生分辨率裁块与四宫格拼图规范（Native Crop & Montage）**：
1. **带 Padding 局部视野**：对每个候选盒模型 $(x_i, y_i, w_i, h_i)$（CSS 像素），以其物理中心为基准外扩局部视野：
   $$\text{pad}_x = \max(24.0, w_i \times 0.2), \quad \text{pad}_y = \max(16.0, h_i \times 0.2)$$
   确保单个候选块视野尺寸不低于 $120 \times 48\text{px}$，且裁剪矩形严格钳制于当前视口 $[0, 0, W_{\text{viewport}}, H_{\text{viewport}}]$；
2. **1:1 原生像素保真**：各候选块直接裁自原生视口渲染（CDP `Page.captureScreenshot` 带 `clip` 矩形或整页 1:1 无损截图内存裁剪），严禁在裁块前进行二次降采样压缩，确保字符边缘锐利清晰；
3. **选项标头（Option Badges）**：依次为各候选块分配大写字母标签 $\text{Labels} = [A, B, C, D][:K]$。在每个子块左上角绘制高对比度标头（如白色圆角衬底 + 深色粗体大写字母）；
4. **自适应拼版排列（Montage Layout）**：
   - $K=1$：单候选块直接呈现；
   - $K=2$：双候选块上下纵向或左右横向并列；
   - $K \ge 3$：按 2×2 四宫格田字排版，拼接输出单张组合图像。

**条款 3｜离散选择提示词与确定性解析契约（Prompt & Discrete Parser）**：
1. **提示词规范**：严禁要求模型输出物理坐标、比例或推理过程，提示词冻结为离散选择模板：
   ```text
   看图作答，不要推理。图上把候选区域编号为 {A/B/C/D}（每块左上角有对应大写字母）。
   哪一块里有文字「{target_semantic}」？只回那一块的字母。
   只输出一个 JSON：{"choice":"X"}（X 取 {A/B/C/D} 之一，若均无或无法确定则输出 {"choice":"NONE"}，不要解释、不要坐标）。
   ```
2. **胜出目标映射与物理派发**：
   - 若模型返回 `choice` 为合法字母标签之一，映射回原候选列表中的对应候选 $C^* \in \{C_1, \dots, C_K\}$；
   - 派发通道：优先以 $C^*$ 的精确 `Selector` 派发物理动作；若选择器为空，则以其盒模型几何中心 $(x^* + w^*/2, y^* + h^*/2)$ 精确派发；
3. **拒识与安全降级**：
   - 若模型输出 `{"choice":"NONE"}`、解析无合法选项，或 VLM 调用超时，fail-soft 降级至第 4 层整页 VLM 兜底，杜绝假成功。

**条款 4｜零副作用与 G-PROOF 自进化闭环**：
1. **零状态打扰**：裁块与离散选择全程在内存与 CDP 截图通道中完成，不修改 DOM 树、不改变焦点、不触发自动滚动，捍卫 R1 零打扰红线；
2. **学习回写闭环**：离散选择胜出并经落点自证校验（§5.0.9 条款 6/7）返回 `SUCCEEDED` 后，将该映射回写至确定性学习条目（`learned.json`），实现「一次复选，后续 0ms 直连」，彻底闭合 Episode 自进化正向飞轮。

---

## 6. 核心流程设计（合并冻结版）

### 6.1 离线蒸馏流水线

1. **组件库基因注入**：遍历Shadcn/AntD/MUI等组件状态，生成Confidence=0.95基线规则，直接写入`uws_rules`并常驻AC L1白名单，**豁免ONNX复核**。
2. **CC页面获取**：拓扑去重（SimHash海明距≤3），域名级熔断（同域名相似页>50即拉黑）。
3. **LLM批量蒸馏**：累积≥50页或2MB载荷合并提交。强制输出`distillation_data`，内含`new_synonyms`（TENTATIVE权重0.2）和`negative_keywords`（入黑名单）。
4. **聚合**：按`hash(text_pattern)%256`分片流式归约，内存<2GB。
5. **ONNX训练**：**严格采用困难负样本**——正样本兄弟节点及SimHash最近邻3个干扰节点作为负样本，防止模型仅学到宏观深度差异。
6. **导出与版本化**：SQLite + ONNX上传对象存储，记录哈希。

### 6.2 在线执行引擎（gRPC服务）

1. **接收意图与上下文初始化**：若为新Target，必须先调用`InitializeContext`注入四元组（Proxy, Viewport, Cookies, Timezone）完成沙盒隔离。
2. **WASM探针全量感知**：单次注入16MB Arrow Buffer，15ms内返回全量特征（含AST降噪哈希）。若截断，记录指标并降级。
3. **三级级联召回（L1 AC → L2 BM25 → L3 SimHash）** ：
   - L1命中核心Token即入候选池。
   - L2对漏判节点构建视口内BM25倒排索引，TNEFT同义词以TENTATIVE权重（0.2）扰动。
   - L3仅对Top-K做SimHash和AST哈希物理校验。
4. **决策与执行**：
   - C≥0.85：执行。物理层强制调用`DOM.getBoxModel`做Z轴遮挡检测；若遮挡先`Input.dispatchScrollEvent`平移，再生成服从高斯分布（μ=120ms, σ=30ms）的三次贝塞尔鼠标轨迹滑向目标。
   - C<0.50：检查LLM配额。若配额充足，调用LLM兜底；若配额耗尽，返回`RESOURCE_EXHAUSTED`降级阻塞。
5. **异步局部验证（含SPA时序）** ：
   - 监听`History.navigated`或`pushState`捕获 → 立即置`NAVIGATING`。
   - **绝对禁止在`pushState`触发时立即判定完成**。必须等待Network Idle（活动连接<2持续500ms）且DOM突变衰减（MutationObserver防抖150ms）后，方可置`NAVIGATED`。
   - 若超时未稳，返回`DEADLINE_EXCEEDED`。
6. **日志与PII脱敏**：所有快照和文本存储前强制脱敏（密码、身份证号等）。高置信度动作仅存`decision_trace`；低置信度动作存全量脱敏快照。
7. **返回ActionResult**：含最终`context_state`（STABLE/NAVIGATING/NAVIGATED/NEW_WINDOW_OPENED/CHALLENGE_INTERCEPTED）。

### 6.3 在线学习（L2）与影子评估

- 筛选低置信度但最终成功的日志。
- 生成候选L2规则及`site_synonyms`（状态TENTATIVE）。
- 影子评估回放历史快照，比对`ast_semantic_hash`（物理同构）。
- 按**动态共轭先验**计算毕业分数，满足条件转ACTIVE。

---

## 7. 平台API与运维契约（合并自Platform Boundary Manual）

### 7.1 gRPC核心接口

```protobuf
syntax = "proto3";
package uws.v1;

service UwsEngine {
    // 必须：初始化浏览器沙盒（Proxy/Viewport/Cookies/Timezone）
    rpc InitializeContext(InitContextRequest) returns (InitContextResponse);
    // 单步动作（无状态）
    rpc ExecuteAction(ActionIntentRequest) returns (ActionResultResponse);
    // 双向流（高阶规划器使用，保持Session复用）
    rpc ExecuteActionStream(stream ActionIntentRequest) returns (stream ActionResultResponse);
    // 零停机知识库热更
    rpc ReloadKnowledge(ReloadRequest) returns (ReloadResponse);
    // 健康检查
    rpc HealthCheck(Empty) returns (HealthStatus);
}
```

### 7.2 确定性错误码映射（协调器必须遵循）

| gRPC Status | 语义 | 协调器对策 |
|-------------|------|------------|
| `OK` | 执行通过 | 前进 |
| `NOT_FOUND` | 无可信节点（C<θ_low） | **不可重试**，触发人工接管或任务回退 |
| `DEADLINE_EXCEEDED` | 探针/验证超时 | **Fibonacci退避重试**1次 |
| `ABORTED` | 节点脱离DOM或遮罩遮挡 | **立即重试**（等待动画帧） |
| `FAILED_PRECONDITION` | Target崩溃或意图ID冲突 | 重新初始化Target或丢弃流 |
| `RESOURCE_EXHAUSTED` | LLM配额耗尽或Target池满 | **降级阻塞**，拒绝低置信度动作 |

### 7.3 浏览器物理层运维（Runtime Plane）

- **进程管理**：强制注入 `--js-flags="--max-old-space-size=4096"`，生命周期限制：500次Navigation或4小时，达到后优雅重启（排空队列超时30s）。
- **Target池**：惰性创建，最大并发 = `CPU_Cores × 1.5`。每个任务独占`BrowserContext`，任务结束直接销毁。
- **网络静息判断**：活动连接数连续500ms降至2以下且无新XHR/Fetch，方可判稳。

---

## 8. 安全、合规与观测性（合并冻结）

### 8.1 PII脱敏硬性执行细则

- **字段级**：`password`、`pwd`、`pass`、`ssn`、`phone`、`id_card` 等字段的value自动替换为`[REDACTED]`。
- **快照级**：存储前对上述字段的文本节点执行DOM树级遍历屏蔽。
- **日志级**：`decision_trace`中若包含用户输入值，仅保留长度和类型元信息。

### 8.2 Prometheus核心指标（SRE红线）

| 指标 | 用途 |
|------|------|
| `uws_action_latency_ms`（P50/90/99） | 若P99>800ms，A*剪枝失效 |
| `uws_llm_fallback_rate` | 启动应>15%，随后收敛下降 |
| `uws_shadow_eval_graduation_total` | 停滞说明环境巨变 |
| `uws_browser_target_pool_size` | 长期贴近上限说明Context泄漏 |
| `uws_wasm_truncation_total` | 正比于页面复杂度，触发降级报警 |

> **接线现状（2026-09-24 `T-14` 复核）**：上表五指标**尚无 Prometheus 导出通道**——代码里只有
> `pkg/engine` 的 `wasmTruncTotal` 原子计数器（`uws_wasm_truncation_total` 的数据源，无 domain label），
> 全仓无 `promhttp` / `client_golang` 依赖、无 `/metrics` 端点。这是**有意后置**：用户 2026-08-20 定案
> 「不引入 prometheus client 依赖（高性能 CLI 审美）」，SRE 面板部署期另选导出通道。本节保留为目标红线，
> 部署前不得当作已上线能力引用；下表的「告警红线」同理属部署期告警，非引擎内建。

**告警红线**：
- 连续100次`NOT_FOUND` → 大面积失明，PagerDuty强制介入。
- 预算4小时内达80% → 熔断离线蒸馏。
- 10分钟内3次Chromium Crash → 内核恐慌。

### 8.3 LLM预算看门狗

- 在线兜底（HIGH优先级）与离线蒸馏（LOW优先级）共用配额池。
- 在线突发容量20，每秒恢复2 Token。
- **每日预算**：80%熔断离线；100%切断所有LLM出口，在线完全退化为“纯规则 + A*搜索”模式。

---

## 9. 开发里程碑与验收标准（修订）

| 里程碑 | 交付物 | 验收标准 |
|--------|--------|----------|
| M1（第1周） | CC管道 + WASM探针 + AST降噪哈希 | WASM感知延迟≤15ms；降噪哈希跨会话碰撞率>95% |
| M2（第2周） | LLM蒸馏 + 聚合器 + 组件库基因 | 同义词TENTATIVE入库；基线规则≥50条 |
| M3（第3周） | WebTouch导出 + ONNX（困难负样本） + 版本化 | 分类器在兄弟节点区分上AUC>0.90 |
| M4（第4周） | Agent集成 + SPA验证器 + PII脱敏 | SPA软路由正确捕获NAVIGATING→NAVIGATED；PII脱敏覆盖率100% |
| M5（第5周） | 在线学习 + 影子评估（动态先验） + 预算看门狗 | 隐蔽节点（拓扑孤岛）1次命中即可毕业；预算熔断有效 |
| M6（第6周） | 外部协调器集成 + 基准环境锁定 | 通过gRPC驱动完成5步登录流程，成功率>80% |

---

## 10. 风险与对策（V1.4补充）

| 风险 | 对策 |
|------|------|
| SPA路由时序坍塌 | `NAVIGATING`中间态 + Network Idle交叉验证，绝不单点触发 |
| LLM同义词投毒 | TENTATIVE权重0.2 + hit_count≥5才激活，POISONED隔离 |
| 闭包高熵摧毁哈希 | AST降噪（剔除数字/字符串字面量）+ LSH容忍 |
| 困难负样本缺失导致模型失效 | 强制兄弟节点 + SimHash最近邻挖掘 |
| A*深度爆炸 | D_max=5硬边界；物理错误+∞注入黑名单 |
| WASM 16MB截断丢节点 | 上报截断指标 + 降级JS全量慢速遍历（200ms超时） |
| 知识库热更期间不一致 | 双缓冲加载，切换瞬间原子化，旧版保留1个TTL周期 |

---

## 11. 版本历史

| 版本 | 变更 |
|------|------|
| V1.3 | 原版（四轮审计基础） |
| **V1.4（本版）** | **合并重写**：回写SPA时序、AC-BM25漏斗、AST降噪哈希、动态贝叶斯先验、困难负样本、A*钳制、WASM探针、PII脱敏、平台运维契约。尾部追加块全部删除，形成单版自洽规格。 |
| **V1.4 + 追加（2026-09-24）** | **§5.0.2 语义标签归一化契约**（定位层 `0/12` 根因修复：内部语义标签派发前展开为页面候选文案集合、多候选取最高分、碎片档加长度下限、不依赖站点知识）。不修改任何冻结参数，仅追加契约。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.3 `input` 后验门禁契约**（假成功根因修复：`input`/`fill`/`select` 只接受可编辑目标、赢家不可编辑时在可编辑候选内重选、落地判据改为「回读值包含输入值」、失败原因分 `no_match`/`input_not_editable`/`input_not_landed` 三态、重试不得叠写）。不修改任何冻结参数，仅追加契约。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.4 角色线性效用启发式与包含域面积约束契约**（面积逆淘汰根因修复：文本汇支路赋予可编辑元素 $W_{\text{role}}=+20$ 角色确定性加成、面积决胜严格约束于祖先后代包含域、确立 DF=100% 质量第一铁律与双轨评价模型）。不修改任何冻结参数，仅追加契约。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.5~§5.0.8 四契约**：§5.0.5 几何有效性硬剪枝公理与第二物理特征平分仲裁（Google 回归闭环）、§5.0.6 布局感知文本提取与 AST 节点黑名单（Baidu 文本源污染治理）、§5.0.7 装饰叶子向承载容器反向穿透（Container Escalation）、§5.0.8 结构化遥测故障代号与 Reflector 决策保真。前一批（`9bdfb171`）漏记本表，本次一并补登。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 订正（2026-09-25）** | **规范↔代码漂移回写**（独立复核发现）：① §5.0.5 条款 1 补 `opacity === "0"` 与黑名单标签前置剔除；② §5.0.6 条款 1 的「非文本 `SVG`」标注为**未实现缺口**（已立项）；③ §5.0.6 新增条款 3「模板容器型 `textarea` 特判」（`^<style` 与含 `<style` 取值整体丢弃）。另记：§5.0.5 条款 2 的 `area/(1+dist)` 与代码逐字一致，无需改动。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.9 VLM 兜底意图-受体守卫与落地后验契约**（越权写入硬拦截：派发前按受体角色画像熔断 `TARGET_ROLE_MISMATCH`、VLM 输入无回读证据硬判 `vlm_landing_unverified`、守卫单点覆盖全部 input 出口且命中不写 `learned`）。§5.0.8 条款 1 代号表同步扩列。不修改任何冻结参数，仅追加契约。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.6 条款 3 扩写为「内联数据载体特判」**：数据载体判定从 `^<style` 扩到**可解析的 JSON 对象/数组**（`textarea.value` 数据块，Baidu hotsearch 换载体），并明确 `application/json` / `ld+json` / `<template>` 由条款 1 标签黑名单已覆盖；文本提取入口（WASM `ownText`／溢出补偿／`collectJSCandidates` 降级路径）同口径丢弃，且**先解析后丢弃**。不修改任何冻结参数。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.8 新增条款 3「探针降级显式标记」**：严格 CSP 站点页内 WASM 探针不可实例化时，以结构化代号 `csp_wasm_blocked` 显式标注（`ProbeStats.Degradation` + `ExportPageTree` 错误串 + 引擎日志），禁止静默降级；降级态证据不足不得视为已验证。上层是否收紧裁决另立项，本条款不默认开启。不修改任何冻结参数。 |

| **V1.4 + 追加（2026-09-25）** | **§5.0.10 穿透寻底与 Leased Token 结构身份局域微重锚契约**（WebTouch v2.0 Phase 1 落地：透明手势代理层剔除 + 组合祖先回溯、封闭 Shadow DOM 穿透走 CDP 宿主层、租约 $\le 1500$ms + 结构身份同余 + $\le 32$px 局域微重锚 Budget=1 + 强制落地回读，熔断代号 `TARGET_TOPOLOGY_RUPTURE`）。§5.0.8 条款 1 代号表同步扩列。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 订正（2026-09-25）** | **§5.0.10 条款 1 按真机门禁订正两处实现顺序/判据**（Phase 1 真机 E2E 发现与规范原意相悖）：① 封闭宿主交接必须**先于**兄弟子树「就近输入框」启发式（实测会把值写进无关 `input`：假成功 + 越权写入）；② AX 受体选择改为「可编辑角色优先、`editable` 属性兜底」，并把该属性真机取值形态（`plaintext` / `richtext`，非布尔 `"true"`）与 UA shadow 内部容器干扰写入条款。不修改任何冻结参数。 |
| **V1.4 + 订正（2026-09-25）** | **§5.0.9 追加条款 4/5、新增 §5.0.11、代号表扩列**：① 意图-动作相容性守卫（请求动作与 VLM 自报动作不相容即**派发前**熔断 `VLM_ACTION_MISMATCH`，不派发/不写 learned/不换出口）；② 成功出口证据门槛（`vlmOutcomeSuccess` 不得单独决定出口，缺请求动作对应页面侧证据一律归 `vlm_landing_unverified`）；③ §5.0.11 把「派发标记时点与中断语义」从代码注释升为规范条款（定位前打标记 ⇒ 真因被 `physical_event_dispatched_outcome_unknown` 覆盖）。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.12 新增条款 6「离线可回放性（页面侧候选投影）」**（用户裁定 CON-205）：`candidates[]` 必须是页面侧候选集投影（5 维特征算子的输入空间），**全空样本严禁入蒸馏集**（消费端 fail-closed 丢弃 + 计数）；由真机抽检 9 条样本 `candidates` 全 `null` 的成因（候选池只含 KB 规则 ∪ 探针打分节点、CDP 节点仅吸收 L2、探针未启用）确立「只读影子通道 + 页面侧候选集/1 阶骨架注入」为**单一原子交付单元**，影子字段在两者就位前保持 `null`。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.12 在线 Episode 采集与脱敏契约**（Episode 字段与「观测先行」配对落盘、a11y 骨架最小充分裁剪 + 原始 rune 长度/截断标记以保证 §5.0.1 特征可重算、脱敏硬过滤、第一指标改为「快路径零负迁移精度 + 安全拒识率」、`T-20` 断点隐式捕获 gold 及其实现缺口与门禁）。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-09-25）** | **新增 §5.0.13「`T-22` 第二批原子技术规范（只读影子通道 ＋ 页面侧候选投影）」**（用户 State B 实现级契约落盘）：① 交付单元唯一性——页面侧候选投影 ＋ `is_ground_truth` 唯一 ＋ 只读影子 ONNX 通道 ＋ 消费端 fail-closed **必须在一次提交内同时就位**（影子只读、不抢决策权，在线 learned 写入仍恒禁）；② CDP 只读快照协议——交互性选择器**加性扩列**（`[role="checkbox"]` / `[tabindex]:not([tabindex="-1"])`）、`K = min(len(nodes), 64)` 且超限按「距派发坐标欧氏距离最近」截断（无坐标退化为文档序并置 `candidates_truncated`）、1 阶骨架只留 `tag` ＋ `role` 并**丢弃全部 class**、候选改按身份去重；③ 脱敏取**并集**（新增 `\+?[0-9]{10,18}` 数字边界宽口径、`input`/`textarea` value → `[VAL_MASKED]` 不落明文、骨架文本 UTF-8 ≤ 32 截断、`url_hash` / `selector_hash` 只落哈希不落原文）；④ 消费端 `pipeline/episode_consumer.py` 三项代数条件（候选非空 / `is_ground_truth` 恰一条 / 影子分基数一致）＋ `<state>/episodes/quarantine/` 隔离 ＋ `quarantined_episodes_total`。附「与既有 `episode.v1` 的映射与差异订正表」（复用 / 新增 / 改名 / 订正四类逐条）。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.13 追加条款 1「接线边界」并订正条款 5 落点**（本批实现中发现，随代码同批生效）：① `--onnx-runtime-lib` 接通会经 `syncScorerWithKnowledge` → `pageCandidatePool` 给真实 CDP 节点打分入池 ⇒ **同时激活决策路径**、越 `T-3` / G-PROOF 门，故本批**只落只读影子通道、不接线**（无 ONNX 会话时 `shadow_inference` 如实 `null`，由 `shadow_missing_total` 计数，接线=放行后的另一批）；② 消费端 fail-closed 校验器落点订正为独立可导入模块 `pipeline/episode_consumer.py`（`pipeline/trace_distiller.py` 在 HEAD 即因 `component_gene.extract_gene` 缺失不可导入，属既有缺口、与本批无关）。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 订正（2026-09-25）** | **新增 §5.0.14「第二批订正」并取代 §5.0.13 四条**（用户实现级推翻性契约）：① **采样时钟前移到派发前**（`service.go::runAction` 决策块后、`markDispatched` 前；禁止派发后取样，`page_projection_after_nav` 废弃）；② **GT 判据升维**为「恒等 → 祖先链 ≤3 阶包含 → 几何兜底 ＋ 最近可交互祖先消歧」，仍恰一条；③ **影子通道正交化**（独立 `Server.shadowScorer` ＋ 独立入口，绝不写 `s.onnxScorer`、绝不进 `pageCandidatePool`；`T-3`/G-PROOF 只约束决策带与在线 learned —— 取代条款 1「一律不接线」）；④ **消费端三级弹性录取**（Tier1 `w=1.0` / Tier2 `w=0.5` 并补离线打分 / Tier3 隔离 ＋ `quarantine_reason` —— 取代条款 5 一刀切）。附「取代关系表」与「契约符号 → 实现落点映射表」。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 实现（2026-09-25）** | **§5.0.14 条款 1/2 落地（第四笔）＋ 8 条实现决议**：采样时钟前移为 `runAction` 单点（决策块后、`markDispatched` 前；收尾永不再取样）；页内 pivot 三段式（KB 选择器 → Tag ＋ Text 页内唯一 → 无 pivot）＋ DOM 子树三级判据（`browser.PageSnapshot.GroundTruthIndex`，原 Go 版几何判定 `episodeGroundTruthIndex` 删除）；无 pivot ⇒ 文档序 ＋ `candidates_truncated=true` ＋ GT 全 false；采样失败与无能力同号 ⇒ 退回 `engine_pool`；VLM 兜底路径因无派发前 pivot 而落 `engine_pool`（已知覆盖缺口，已在条款内显式登记）。覆盖证据：自建 headless Chrome 夹具 10 例全过（含「祖先链胜过几何兜底」「祖先恰 3 阶」甄别例）。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 订正（2026-09-25）** | **§5.0.14 新增决议 0「真值锚点＝物理受体」并改写决议 1/3/6/7**（用户裁定：**否决**维持现状、**排除**语义文本唯一退化、**确立**物理受体回传为唯一物理自洽解；三条定理＝飞轮冷锁 / 语义唯一性伪命题 / 物理受体回传完备性）：`PageSnapshotPivot` 增受体派发点（`HasPoint/PointX/PointY`）并保留 `Selector/Tag/Text` 作补充身份；`browser.PreDispatchReceptorSink` 由 `cdpSession` 实现，click（含 gemini 恢复链）/input/fill/select/extract（含 VLM 坐标 `ClickAt`）在 `disp.mark()` **之前**回传一次受体（fail-soft）；采样点下移至物理层且仍唯一；页内定位改以受体派发点为锚（补充身份降级为兜底），骨架取自 GT 命中元素。登记运行态实测：GT 覆盖 3/17 → 0/17（旧版 `4e19895a` 同场景 `GT=1`、新版 4/4 全 `false`，`production.cand_count=0`），消费端回放 `accepted=3 / quarantined=18`、`reasons={candidates_empty:10, candidates_below_min:4, ground_truth_count:4}`。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 **(c) 落地后同批重验（`e58a8bc1`，运行态 `v1.5.5+e58a8bc1` pid 48397）**：GT 覆盖 0/17 → **4/4**（4 条真机样本 `gt_idx=[0]`、骨架 1 阶取自 GT 元素、`candidates_truncated` 不再恒真＝3 条 `false` ＋ DDG 1 条真实 `>64`）；消费端只读回放（25 条副本 `/private/tmp/wtx-eps5/`）`accepted=7 / quarantined=18`，4 条新样本转 **Tier2 弱监督**（`reason=None`、`w=0.5`），`ground_truth_count` 4 次命中仅剩旧样本 18–21 行（历史不回改）；真机夹具扩至 **12/12**；决议 3 增补「受体模式下取消矩形中心最近几何猜测」（自决并声明）。 |
| **V1.4 + 追加（2026-09-25）** | **§5.0.13 追加条款 7「候选骨架对称挂载」＋ State C 编号映射登记**（用户 `[State: C]` 追认；`CON-214` 伪特征硬线、`INT-218①` 唯一阻断扩批项）：页内只读快照为**每个入选候选**各携带**自身** 1 阶骨架（`tag` ＋ `role`、文本 ≤32 截断、**≤8 条**、`parent → children → siblings`、超出按文档序截断并置 `truncated`），消除「仅 GT 挂骨架、负样本全空」的伪特征；顶层 `PageSnapshot.Skeleton` 降为 **GT 候选 `neighbors` 的别名**（兼容）；`episode.v1` **不升版**、`is_ground_truth`／采样时点／影子只读边界／`struct_hash`·`confidence`·`matched_rules` 占位口径**均不变**。同批登记 `OBS-220/221/222/223`、`CON-214/215/216`、`INT-216/217/218/219`、`DEP-213/214/215` 与映射（`CON-212 ≙ §5.0.12 条款 2 ＋ §5.0.13 条款 4`；`CON-201` 待补发）。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 实现（2026-09-25）** | **§5.0.14 追加决议 9「候选空集轻量重试」**（`INT-218②`）：页内快照**成功但候选为 0** 时在同一采样槽内等待重试 **≤2 次 × 400ms**，仍为空则如实落空集（绝不编造候选/GT）；**不改采样时点、不改唯一性、不碰决策与影子**；快照失败（与「无能力」同号）**不重试**。**归因留痕**：本机 88 条 Episode 中空集 **47/47 全属「未采样」类**（`engine_pool` 46 ＋ 旧 `page_projection_after_nav` 1、其中 18 条 `canceled_before_dispatch`）、**0 条**为「投影跑过但 0 候选」⇒ 本条为**保险性**改动，不得宣称录取率提升。 |
| **V1.4 + 追加（2026-09-30）** | **§5.0.9 追加条款 7「确定性路径点击出口判据」**（假成功根因修复：确定性语义 / 选择器直查点击此前只记录落点证据、不判出口，真机给出「单候选被绑定给无关语义仍报 `SUCCEEDED`」的现成假成功）：基准集＝`target_semantic` 语义别名展开集 ∪ **与目标语义同源**的 KB 背书同义词文本，A* 多步换步（路径长度 $\ge 2$）改按 `exec_semantic` 判；判 miss ⇒ 不返回 `SUCCEEDED`、不写 `learned`、不换出口，代号 `CLICK_LANDING_MISMATCH`。**同批真机订正「豁免」口径**：背书必须与目标语义同源（`site_synonyms.source_rule_id` 关联）；「池内 L2 候选一律背书」的旧口径把本条款要修的假成功原样放过（真机实证：唯一候选 `Sign in` 绑定给 `Order history` 仍 `SUCCEEDED`），已作废。同批订正条款 6 真机复证里「确定性路径判出口收益为零」的旧判断（作废）。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-09-30）** | **§5.0.9 追加条款 8「兜底视觉细节预算契约」**（「VLM 点歪」根因修复：`image_url.detail` 硬编码 `low` 把整页截图降采样到 ~512 px 长边，小目标在模型看到之前就丢细节）。判据＝按**图像长边**单点选档（$>768$ ⇒ `high`，$\le 768$ ⇒ `low`），整页恒 `high`、小裁剪仍 `low`；依据＝单变量对照（同图同提示词只换 `detail`：−104 px → −2 px），代价 prompt token 655 → 1431 / 延迟 +0.13s。同时登记：参考线/参考点补偿**不做**（无收益、参考点更差），模型不会自觉 `crop_zoom`（引擎强制裁剪另议）。实现落点 `pkg/vlm/vlm.go::detailForImage`。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-09-30）** | **§5.0.15 `T-30` 决策时点候选池与排序位序落盘契约**（候选块复选的数据前置；已确证「裁谁有算法、裁几个有读数、**位序没落盘**」——Episode `candidates[]` 是页面侧锚点投影、176 条有 GT 样本中 166 条 GT `index`=0，拿它算召回＝虚假高命中）。新增 additive 字段 `decision_pool`（`rank` / `stage` / `source` / `confidence` / `selector_hash` / 几何 / `is_selected`）＋ `production.decision_pool_count`·`decision_pool_truncated`；决策序＝按 `confidence` 降序稳定排序（与 L3 top-K 选举 `order` 同源），上限 `K_decision=64`，空池落空数组，`is_ground_truth` **不复制**（单一事实源）；固化定位层级 fallback 阶梯（确定性→learned→VLM 坐标→候选块复选→整页 VLM→人，跳层判据＝排序没把握／池为空／超时）；候选块复选的 K 须由实测位序分布回写，**未取得读数前不得实现该出口**。Tier 录取与 `CON-205` 原样（`decision_pool` 不参与 Tier 判定）；`episode.v1` 不升版。**条款 6 登记离线基线**：真实页面**决策池 0 率 82–97%**（「池中位 49–62」实为**页面侧候选集**，两者必须点名区分）⇒ `T-30` 实质缺口＝**页面侧候选集缺相关性排序器**（今日仅锚点序＝循环依赖）。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-10-01）** | **§5.0.16 CDP 原生检索粗锚点自举契约**（D11 / T-30 第二刀 Phase 0：长页面具象文案定位粗锚点自举）。禁用 `window.find()`，以 CDP 原生 `DOM.performSearch` ＋ `DOM.getSearchResults` ＋ `DOM.getBoxModel` 联动 §5.0.2 别名展开集；提取首个有效盒模型中心点作为 `CoarsePoint` 恢复欧氏距离排序（长页目标直进 Top-64）；解耦 Guidance Origin 与 Physical Receptor，粗锚点不参与 GT 判定（`GroundTruthIndex` 保持 `-1`，防止脏样本污染）。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-10-01）** | **§5.0.17 AX 地标剪枝与标题树空间索引契约**（D11 / T-30 第二刀 Phase 1：长页面无锚点深水区候选召回）。解决朴素文档预序导致候选被顶部 10.7% 外围导航占满之陷阱。确立 AX Landmark 分类与配额保护机制：主内容区 `<main>` / `[role="main"]` / `<article>` 保底配额 $K_{\text{main}}=48$，外围区域配额 $K_{\text{other}}=16$，双向弹性回拨；确立 `H1~H6` 标题树空间索引：目标语义命中标题时自举标题路标锚点（Heading Anchor）恢复欧氏距离排序，粗引导原点严格隔离不污染 GT（`GroundTruthIndex=-1`）；无引导时采用纵向空间分层均匀采样（Stratified Depth Sampling）覆盖全页纵深。`PageSnapshotNode` 增 `Landmark`，`PageSnapshot` 增 `Headings`。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-10-01）** | **§5.0.18 ONNX Ranker 10 维特征与端到端页面候选排序契约**（D11 / T-30 第二刀 Phase 2：页面候选全量排序与决策池闭环）。解决真实页面决策池 0 率 82%~97% 之死结。确立固定 10 维多模态特征向量契约（`simhash_sim`, `jaccard`, `exact_match`, `substring`, `log_len`, `role_compat`, `visual_prominence`, `context_echo`, `landmark_zone`, `spatial_distance_decay`）；放开 `pageCandidatePool` 入池门禁，真实 CDP 节点全量打分入池；确立 ONNX 与确定性启发式 Scorer（Heuristic Fallback）两级路由；升级 L3 结构校验为弹性准入（高置信度与 Top-4 免剔除），杜绝假 NOT_FOUND；训练数据严守 G-PROOF 严选门禁。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-10-01）** | **§5.0.19 Top-4 候选块裁块四宫格与 VLM 离散选择（A/B/C/D）契约**（D11 / T-30 第二刀 Phase 3：候选块复选闭环收口）。解决长页整页截图压缩降采样（0.15×）导致坐标回归 $+78$px 漂移出界 miss 之顽疾。确立定位阶梯第 3 层候选块复选准入门禁：触发于模糊置信区间 $[\theta_{low}, \theta_{high}) = [0.50, 0.85)$ 或 Top-1/2 歧义时，截取决策序前 $K = \min(len, 4)$ 候选；确立 1:1 原生分辨率带 Padding（$\ge 24\times 16$px、最小 $120\times 48$px）局部裁块与四宫格田字拼版规范，子块标记 A/B/C/D 标头；冻结离散选择提示词模板（只输出 `{"choice":"X"}`，严禁算术坐标计算）；胜出候选直接映射精确选择器或中心物理派发；拒识 fail-soft 退回整页 VLM。超长页宏观视口路由契约见 [`long-page-viewport-routing.md`](long-page-viewport-routing.md)。成功经落点自证后回写 `learned.json`，彻底闭合自进化飞轮。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |
| **V1.4 + 追加（2026-10-01）** | **§5.0.9 条款 8 落点自证豁免扩围**（通道修复：8000 gemini-flash chat/completions 500）。豁免从「状态型切换语义」扩为「**内部控件语义**」，准入两条＝①语义标识是引擎/网关侧内部控件名（页面不存在该字符串，站点专有文案按 §5.0.2 纪律不得进别名表）＋②物理层有专用后验；登记六例＝切换四例（`gemini_model_switch` / `gemini_thinking_switch` / `deepseek_mode_switch` / `deepseek_thinking_switch`）＋ 固定控件两例（`gemini_send` / `gemini_new_chat`，后验＝`verifyGeminiClick`）。真机旧读数：`ref=gemini_model_switch hit=Flash`、`ref=gemini_send hit=Send message` 均被判 `CLICK_LANDING_MISMATCH` 硬失败 ⇒ 通道 500。不修改任何冻结参数（`θ_high=0.85` / `θ_low=0.50` / `ε=0.02` 原样）。 |

**文档状态：冻结，可编码。开发团队请以本V1.4版本为唯一法度。**
