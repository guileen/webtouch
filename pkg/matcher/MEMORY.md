# pkg/matcher MEMORY

## A* 多步搜索（TDD-MATCH-06，2026-08-20 点亮）

- **决策语义**：`Decide(conf, epsilon, rng)` —— C ≥ 0.85 direct；C < 0.50 llm_fallback；
  中间区间 rng=nil 或 ε ≤ 0 稳定 astar；ε 探索需显式注入 rng（测试确定性关键）。
- **深度**：D_base=3，流程标志（step/checkout）+2，D_max=5 硬边界；
  `MaxSearchDepth(flow)` 是唯一入口，引擎侧不得各写一套。
- **惩罚**：逻辑偏移 = C_base × [1 + (1/Overlap)]²；Overlap ≤ 0 视为结构完全无关 → +∞（死路）。
  物理湮灭（HTTP 4XX/5XX）不参与路径——「+∞ 语义」由「剔除 + 入黑名单」表达。
- **祖父连坐**：2 层向上的祖父记**有限**附加惩罚（`finiteJointPenalty` = C_base × 4，
  即 overlap=1 时最大逻辑惩罚）；绝不用 +∞（会毒化整支）。连坐影响祖父子分支扩展成本，
  A* 自然回溯到兄弟分支；`SearchResult.JointLiability` 留审计记录。
- **黑名单**：`PhysicalBlacklist` 内存态 + RWMutex（并发安全）；跨进程持久化归引擎/CDP 层，
  本包不落库。
- **坑**：
  1. heap 平局裁决必须含 `seq`（入队序），否则同惩罚分支顺序不稳定；
  2. 扩展路径必须 copy 后 append（`append(append([]SearchStep(nil), it.steps...), ...)`），
     复用切片会共享底层数组污染其他分支；
  3. `nodeHash` 对显式 SimHash 字段优先，0 才回退 `SimHash64(Text)`（空文本 hash 为 0 的场景）。
- **集成待办**：`SearchNode.Children` 目前由测试手工构造；引擎 runAction 中间区间接线需要
  候选图输入通道（v1.5 契约候选特征 RPC 或真实 CDP DOM/AXTree 候选流），见 USER_ACTION.md #12。
