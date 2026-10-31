# WSTN 网页状态拓扑转移网与 s3b6 路由算法验证规范与索引指南

> **文件定位**：供协作智能体、独立子进程或评测沙箱开展 **WSTN (Web State Topology Network)** 算法验证的任务说明书与代码/文档索引。  
> **核心目标**：在零 Token、单核 CPU、纯离线环境下，独立验证从 `memory_systems` 吸收的 `s3b6` 概率波前路由与解耦信用回传算法在网页状态拓扑网上的有效性、抗噪性及延迟边界（P99 ≤ 15ms）。

---

## 一、 关键文件与源码物理路径索引

协作者或子智能体开展工作时，请按顺序查阅以下关键事实源与参考源码：

### 1. 算法渊源与原型（Labs 内部事实源）

| 文件路径 | 模块角色 | 重点查阅段落 / 关键符号 |
| :--- | :--- | :--- |
| [`memory_systems/pddtmn_260902/route.py`](../../memory_systems/pddtmn_260902/route.py) | **算法原型实施源** | • `RouterParams`: `start_k=3`, `branch_k=6`<br>• `RouteOutcome`: 命中标记与决策步索引<br>• `Router.route()`: 势能 $\Phi$ 计算与波前发散逻辑 |
| [`memory_systems/pddtmn_260902/learn.py`](../../memory_systems/pddtmn_260902/learn.py) | **信用回传公式** | • `propagate_hebbian_credit()`: 赫布边权强化<br>• `credit_weights(..., credit_mode="decouple")`: 解耦模式防中心走廊污染 |
| [`memory_systems/pddtmn_260902/graph.py`](../../memory_systems/pddtmn_260902/graph.py) | **轻量图底座** | • `SQLiteGraph`: 边的存储、权重更新与衰减接口 |
| [`memory_systems/总实验报告.md`](../../memory_systems/总实验报告.md) | **历史评测标准** | • §0 与 §2: 10-seed 统计显著性（McNemar $p < 10^{-6}$）与延迟门禁（s5b8 候选 9.169ms PASS；s3b6 对照 8.389ms） |

> **口径纠偏（2026-09-14）**：`9.169ms` 是 `memory_systems` 中候选
> `s5b8 + decouple` 的历史 P99；同批 `s3b6` 对照为 `8.389ms`。两者都不是
> WebTouch WSTN 的实测结果，只能作为门禁来源与算法渊源，不能抄作本项目成绩。

### 2. WebTouch 架构、既有算法与基准（工程宿主）

| 文件路径 | 模块角色 | 重点查阅段落 / 关键符号 |
| :--- | :--- | :--- |
| [`WebTouchKeyDesign.md`](../WebTouchKeyDesign.md) | **终局设计总纲** | • §3.3: WSTN 网页状态拓扑转移网思想<br>• §3.4: 深度交互因果突变采样机制 |
| [`spec/algorithm-kernel.md`](../spec/algorithm-kernel.md) | **冻结算法规范** | • §5: 算法选型与冻结参数（AC、BM25、SimHash 64位汉明距）<br>• §5.0.1: 5 维 ONNX 特征定义（SimHash 距、Jaccard、Exact、Substring、LogLen） |
| [`pipeline/topology.py`](../pipeline/topology.py) | **Python 拓扑特征** | • `simhash64(text)`: 64 位词频加权哈希<br>• `hamming_distance(a, b)`: 汉明距离计算<br>• `TopologyDeduper`: 同域相似页面聚类过滤 |
| [`pkg/matcher/simhash.go`](../pkg/matcher/simhash.go) | **Go 侧哈希实现** | • `SimHash64()`, `HammingDistance()`: 与 Python 逐位对齐的签名实现 |
| [`pkg/matcher/astar.go`](../pkg/matcher/astar.go) | **历史 A\* 寻路** | • `Decide()`: 三阶决策门限（0.85 / 0.50）<br>• `SearchNode`: 历史候选结构（需替换为 WSTN 候选流） |
| [`benchmarks/probe_astar_consistency.py`](../benchmarks/probe_astar_consistency.py) | **重要踩坑实验** | • 证明 DOM 在 0/50/200ms 存在时序漂移，导致单步分类器违背启发一致性，证明了为什么需要 WSTN 宏观拓扑图 |
| [`benchmarks/tier1/`](../benchmarks/tier1/) | **既有元素级离线评测集生成器** | • 当前不存在 `golden_truth.json` 静态文件<br>• `drift_set.py` / `run_tier1.py`: 生成并评测元素定位漂移，不可冒充页面状态标签 |
| [`benchmarks/tier2/`](../benchmarks/tier2/) | **真机 Mock 沙箱** | • `test_pages.py`: 静态与动态表单测试页，提供真实 CDP 交互环境 |

---

## 二、 算法模块契约定义 (Data Contract)

协助开发的智能体或外部进程应围绕以下三层抽象进行隔离实现与验证：

```
                    ┌───────────────────────────────┐
                    │     DOM Snapshot / AXTree     │
                    └───────────────┬───────────────┘
                                    │ EncodeState()
                                    ▼
                    ┌───────────────────────────────┐
                    │   State Node (64-bit Hash)    │
                    └───────────────┬───────────────┘
                                    │
                    ┌───────────────┴───────────────┐
                    │     WSTN Directed Graph       │
                    │  Nodes: State | Edges: Action │
                    └───────────────┬───────────────┘
                                    │ S3B6Route()
                                    ▼
                    ┌───────────────────────────────┐
                    │  Action Path: [Edge1, Edge2]  │
                    │  Latency < 15ms | 0 Tokens    │
                    └───────────────────────────────┘
```

### 1. 状态节点 (State Node)
```python
@dataclass(frozen=True)
class StateNode:
    state_id: str           # 唯一标识，如 "ds_login_view"
    simhash: int            # 64-bit DOM 拓扑哈希
    url_pattern: str        # 归一化路由（如 "https://domain.com/login"）
    interactable_roles: int # 可交互元素特征签名
```

### 2. 动作转移边 (Action Edge)
```python
@dataclass
class ActionEdge:
    source_state: str       # 起始状态 ID
    target_state: str       # 到达状态 ID
    action_type: str        # "click" | "input" | "select" | "scroll"
    target_semantic: str    # "登录按钮" 或 "node:14"
    weight: float = 1.0     # 赫布动量权重 [0.0, 10.0]
    success_count: int = 0  # 历史成功转移次数
    failure_count: int = 0  # 历史失败/阻断次数
    avg_latency_ms: float = 0.0
```

### 3. 路由求解器接口 (S3B6Router)
```python
class S3B6Router:
    def find_path(
        self, 
        current_state: StateNode, 
        target_intent: str, 
        start_k: int = 3, 
        branch_k: int = 6, 
        max_hops: int = 5
    ) -> RouteResult:
        """
        在 WSTN 图中快速求解从当前状态到达目标状态/意图的最优动作边路径。
        若路径不存在或置信度低于阈值，返回 status="FALLBACK"。
        """
```

### 4. 算法消费契约（`wstn.route.v1`，冻结）

机器契约：[`spec/wstn-routing-v1.schema.json`](../spec/wstn-routing-v1.schema.json)。

- **边界已冻结**：消费方提交 `current_state_id`、上游已经解析好的 `target_state_id`、仅作审计的
  `target_intent` 与三项上限；算法返回 `FOUND | FALLBACK`、状态路径、动作序列和诊断字段。
- **职责拆分**：意图到目标状态的召回/消歧属于上游 Intent Resolver；WSTN 路由器不得靠模糊字符串猜目标。
- **兼容纪律**：v1 顶层字段集合冻结。实验性附加数据只能写入 `extensions`；新增/删除顶层字段、改字段类型、
  改状态枚举或改变动作语义都必须发布新 Schema 版本，消费方不得猜测兼容。
- **消费纪律**：执行器只把 `status` 和 `actions` 当控制字段；`confidence` 是路由诊断值，在页面级校准完成前
  不得自行发明安全阈值，也不得绕过既有付费、登录、挑战页与副作用护栏。

因此算法消费方可在 Tier 0/Tier 1 尚未完成时基于 v1 Schema 与夹具并行开发；评分公式、权重与搜索实现
可以继续实验演进，但不得破坏这个边界。

Go 侧已提供严格消费类型与解析门禁：`pkg/matcher/wstn.go`。它拒绝未知顶层字段、非连续动作链与非法索引；
这表示消费方已经可以开工，但不表示 WSTN 已替换生产 `pkg/matcher/astar.go`。

---

## 三、 独立验证的三阶实验方法

协助智能体可直接在无浏览器环境（纯 Python/Go）下开展前两阶实验：

### 阶梯 1: 纯图论与算法基准仿真（Tier 0 Benchmark · 零成本 · 毫秒级）
1. **构造测试用例图**：
   - 构造一个包含 200 个节点、500 条边的合成有向图（模拟多页面交互拓扑）；
   - 预设 5 条多跳黄金路径（3~5 跳，如：`首页 -> 搜索 -> 列表 -> 详情 -> 支付`）；
   - 注入 30% 噪声分支、5 处循环依赖（环路）与 2 处断路（模拟 404/失效边）。
2. **测试维度**：
   - **正确性**：给定起始状态与目标，`s3b6` 路径到达召回率；
   - **延迟压测**：循环执行 10,000 次路由求解，记录 P50 / P95 / P99 纯计算时延；
   - **信用回传**：针对成功路径执行 `decouple` 权重更新，连续迭代 50 轮，检查黄金路径权重是否显著增强，Hub 节点是否被抑制；
   - **自愈绕行**：动态将主路径某条边标记为失效（weight $\to 0$），检验算法在第 2 次查询是否能即刻无死循环地命中备用边。

### 阶梯 2: DOM 变异与状态离散化稳定性验证（Tier 1 Benchmark · 零 API · 秒级）
1. **测试数据集**：
   - 现有 [`benchmarks/tier1/drift_set.py`](../benchmarks/tier1/drift_set.py) 只生成元素定位漂移样本；本阶必须另建带“同一逻辑页面 / 不同逻辑页面”pair 标签的页面状态集，不得假定不存在的 `golden_truth.json` 已提供该数据。
2. **测试维度**：
   - **同页面鲁棒性**：计算同一功能页面在变异前后的 DOM SimHash 汉明距 $\Delta h$。验证 $\Delta h$ 的分布区间；
   - **异页面区分度**：计算登录页、购物车、详情页等不同页面之间的汉明距 $D_{diff}$；
   - **最优判定窗标定**：通过受试者工作特征曲线（ROC），确定最佳汉明距离阈值 $D_{th}$（推荐区间 $3 \le D_{th} \le 5$），实现状态聚类的零混淆。

### 阶梯 3: 本地 Mock 站因果闭环录制（Tier 2 · 零 API · 1~2 分钟）
1. **运行环境**：
   - 启动 [`benchmarks/tier2/test_pages.py`](../benchmarks/tier2/test_pages.py) 提供的本地动态测试站点。
2. **测试维度**：
   - 发起 5 步真实交互，通过 CDP 监听并在每步前后提取 DOM 快照；
   - 验证 `[动作前状态] -> [动作触发] -> [动作后突变]` 的闭环建图流水线是否能自动生成标准 WSTN JSON。

---

## 四、 验收标准与硬核门禁 (DOD)

外部进程或智能体提交验证成果时，必须满足以下量化指标方可判定合格：

| 验收项 | 指标要求 | 检验方法 / 判据 |
| :--- | :--- | :--- |
| **纯算时延** | **P99 $\le 15\text{ms}$** | 单核 CPU 执行，合成 200 节点图 1,000 次寻径耗时统计 |
| **路径召回率** | **$\ge 90\%$** | 在 30% 噪声分支干扰下，黄金路径全跳召回占比 |
| **状态聚类混淆率** | **$< 5\%$** | Tier 1 数据集下，不同逻辑页面被归并为同一状态的比例 |
| **断路自愈性** | **1 步切换** | 主边失效后，第 2 次路由必须成功切换至备份分支，死循环率为 0 |
| **Hub 走廊防污染** | **通过** | 50 轮随机流量通过后，高频公共节点的边权方差 $\le$ 阈值，未出现垄断 |
| **无回归门禁** | **100% 绿灯** | Go 侧单测与现有 Python 测试套件无任何报错（`pytest` 194 passed） |

---

## 五、 成果交付物规范

协助验证完成后，请在 `agent-tools/webtouch/` 目录下产出以下三项物料：
1. **评测报告**：`benchmarks/reports/EVAL-wstn-s3b6-verification.md`（包含 P99 时延曲线、汉明距分布图表与 McNemar 显著性检验结果）；
2. **基准脚本**：`benchmarks/bench_wstn_s3b6.py`（具备一键复现性，支持 CLI 参数 `--nodes`, `--drift`）；
3. **数据产物**：`benchmarks/data/wstn_benchmark_result.json`（原始测量数据，供主线核查）。

---

## 六、执行里程碑（2026-09-14 校准版）

> 排序原则：先冻结会影响所有下游的契约与口径，再实现纯内核，再生成证据，最后才接浏览器与 Go 运行时。

### M0：事实与实验边界冻结（本轮）

- 区分 `memory_systems` 历史证据、WebTouch 既有 Tier 1/1.5/2 证据与本次 WSTN 新证据；
- 明示 WebTouch 适配语义：`start_k` 是首层波前宽度，`branch_k` 是后续每个状态的候选扩展上限；
- 合成图只证明路由器在受控拓扑中的正确性、性能与自愈，不证明真实网页状态离散化或 VLM 调用率收益；
- 冻结本轮不做项：不启动 Chrome、不调用外部 API、不接 Go 运行时、不改旧 A* 行为。

完成判据：本节、数据契约和验收口径互不冲突，后续会话可由 README 直接进入本规范。

### M1：可复用 WSTN 纯内核（本轮核心）

- 在 `pipeline/wstn.py` 实现 `StateNode`、`ActionEdge`、`WSTNGraph`、`RouteResult`、`S3B6Router`；
- 路由以状态 SimHash 目标相似度、边可靠度、学习权重与延迟共同构造势能，并按受限波前排序；
- `decouple` 信用只强化存在真实分支选择的决策边与最终到达边，单通道公共走廊保持稳定；
- 失效边显式湮灭，路径内禁止重复状态，避免环路与回溯。

完成判据：纯 stdlib、可序列化、固定输入逐位确定；单元测试覆盖黄金路径、环、FALLBACK、信用与断路。

### M2：Tier 0 基准与统计证据（本轮核心）

- 生成 200 节点 / 500 边、5 条 3~5 跳主路径及备用路径、30% 误导噪声、5 个环、2 条预失效边；
- 多 seed 配对比较 `s3b6` 与单分支贪心基线，报告终点命中率、黄金全跳召回率与精确 McNemar；
- 跑 10,000 次纯路由，记录 nearest-rank P50/P95/P99；50 轮信用学习验证走廊不污染；
- 主路径首次失败后湮灭该边，第 2 次查询必须走备用路径且无环。

完成判据：结构化 JSON 与 Markdown 报告可由一条命令重建；任何未达门禁项保持 FAIL，不改判据迁就结果。

### M3：回归与索引闭环（本轮）

- 跑新增单元测试、基准自检、既有 Python `tests/unit/ tests/integration/`；
- 在不触碰浏览器的前提下跑 Go matcher 定向回归；
- 更新 `docs/tdd-checklist.md`、README、项目 handoff 与 Labs 根 handoff。

完成判据：从 Labs README 可沿项目入口找到规范、脚本、数据与报告；工作树风险如实记录，不夹带他线改动。

### M4：页面级状态离散化 ROC（已完成受控离线标定 · 2026-09-14）

审计已确认仓内不存在 `benchmarks/tier1/golden_truth.json`，现有 Tier 1 由脚本生成元素定位漂移样本。
必须新建独立页面状态 pair 数据，不得拿元素匹配 P_new/FH 冒充状态聚类混淆率。
完成后才标定 $D_{th}$、绘制 ROC，并给出状态爆炸 / 状态塌缩的双侧错误率。

实测已落 `pipeline/wstn_state.py` 与 `benchmarks/bench_wstn_state_pipeline.py`：8 个逻辑页面状态 ×
12 个 DOM 变体，校准集与 holdout 各 1,128 对且快照不重叠。阈值只由校准集按 Youden J → balanced
accuracy → 低 FPR → 低阈值的顺序选取，结果 `D_th=6`；没有为迎合原建议区间 3–5 而改判。
独立 holdout 的不同状态误合并率 0%、同状态拆裂率 0%，通过 `<5%` 门禁。该结果仍只证明受控页面分布，
真实站点/框架/多语言分布外推保持未验证。

### M5：本地 Mock 因果闭环与运行时装配（录制内核已完成，真实 CDP 待许可）

先在 Tier 2 生成标准 WSTN JSON 和动作前后突变证据，再定义 Go 侧只读候选流接口；只有离线门禁与
页面级状态门禁都通过后，才将路由器接入 `pkg/matcher`，旧 A* 保留为可回退对照臂。

`CausalWSTNRecorder` 已用 5 步离线 Mock journey 验证：生成 6 状态/5 动作边的连续可路由图；每条证据
包含前后状态 ID、签名 SHA-256、汉明距、URL 是否变化、token 增删计数与延迟，不保存原始 HTML/文本；
同一聚类内的无变化动作会被拒绝，避免假因果边。真实 `benchmarks/tier2/test_pages.py` + Chrome CDP
采集因项目“未经明确许可不得运行真实 Chrome”红线保持 `NOT_RUN`，因此 M5 尚未销账，也未接 Go 运行时。

### 总控复审结论（编码前二次审核）

本计划不是由规划器直接定稿。总控复审后补上以下约束，避免基准“自己出题、自己宣布胜利”：

1. **隐藏真值**：合成器可保存黄金路径供评测，但路由器只能看到节点签名与边属性，禁止读取黄金路径、反向最短路或任何答案泄漏特征；噪声中必须包含“比正确下一跳更像目标”的误导死路。
2. **算法身份隔离**：WebTouch 默认采用 Boltzmann 势能排序后的确定性 Top-K 波前；PDDTMN 原型的无放回随机采样仅作为可选复现模式。两者都必须带模式名，禁止把适配实现称为原源码逐位复刻。
3. **基线身份隔离**：McNemar 对照臂是同一 WSTN 势能下的单分支贪心路由，不是历史 Go A*，报告不得混称；比较单位是同一 seed、同一查询的配对成败。
4. **指标定义先于运行**：噪声率 = 噪声边数 / 总边数；黄金全跳召回 = 返回路径与预注册主路径逐边一致；终点命中率只看是否到达目标，两者分报；自愈备用路径不计入主路径全跳召回。
5. **性能边界如实写**：计时只包围内存图上的 `find_path`，不含数据生成、JSON 和报告写入；纯 stdlib 单线程执行，但 macOS 上不能强制 CPU affinity，报告不得把“单线程”夸成“已物理绑核”。
6. **信用机制不冒充移植**：通用网页节点没有 `cN.sN` 走廊编号，因此本轮 `decouple` 是基于“有多个可用出边的真实决策点 + 末跳”的 WSTN 适配；必须与全路径信用对照，公共单通道走廊边的绝对权重变化应为 0。
7. **状态编码证据分层**：Tier 0 的 SimHash 仍只充当合成状态签名与路由启发；其后新增的页面 pair 基准才负责 ROC。当前 `D_th=6` 与 0% holdout 双侧错误只适用于受控 8 状态/96 快照，不能从旧元素级 Tier 1 外推，也不能冒充真实站点总体分布。
