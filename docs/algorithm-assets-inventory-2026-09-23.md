# 算法资产盘查（T-11A 第一步 · 2026-09-23）

**目的**：T-11 拆成 **T-11A 离线算法质量** 与 **T-11B 线上能力采用率** 后，
先回答「现有材料能不能直接支撑离线基线」——不新建遥测、不重搭 harness。
**口径**：每条资产标出 **真实/合成**、**标签来源**、**站点覆盖与 holdout**、**能否离线复现**。
**本轮只做盘查**：未跑基准、未改源码、未动运行态。

## 1. 资产总表

| 资产 | 真实/合成 | 标签来源 | 站点覆盖 / holdout | 离线复现 | 现状 |
| --- | --- | --- | --- | --- | --- |
| `benchmarks/tier1/drift_set.py`（生成器）+ `cmd/tier1/main.go`（runner）+ `benchmarks/tier1/run_tier1.py`（judge） | 合成 DOM（`SEED=42`，500 case = 5 类 × 20 语义 × 5 mutation：wrap/rename/css/tag_role/attr） | 构造真身身份（`original_node_id`/`truth_node_id`），并有 strict/relaxed 双 judge 自证 κ≥0.9 | 无站点概念；**单集、无 holdout** | 可（Go + Python），但需先跑生成器产出 `build/tier1/{drift_set,golden_truth}.json`——该产物不在仓内 | 管线与判分口径齐备；评的是单真身命中，不是候选排序质量 |
| `benchmarks/tier1/malhotra_judge.py`、`onnx_augment.py` | 合成 | 判分器（strict / Malhotra 兼容） | — | 可 | tier1 判分依赖，非独立数据集 |
| `pkg/classifier/testdata/golden.json` | 合成 | 未标注（8 条 `candidate/target/features/score`） | 无站点、无 holdout、**n=8** | 可 | 只能做模型回归冒烟，不能当质量基线 |
| `benchmarks/bench_classifier.py`、`bench_probe_wasm.py` | 合成节点构造 | 无标签（测延迟/体积） | — | 需要 ONNX 模型/构建产物 | **性能**基准，非质量 |
| `benchmarks/bench_matcher.py`、`bench_gate.py`、`bench_llm_fallback.py` | — | — | — | — | **骨架**（`pytest.skip`）：含 T-11B 想要的「1000 步黄金语料兜底率 <15%」指标，但语料本身不存在 |
| `benchmarks/tier2/`（`cdp_client`/`test_pages`/`sandbox`/`temporal_ensemble`） | 真实浏览器 + 本地生成页面（本机 HTTP 服务，跨域靠端口构造） | 页面自检断言 | 无站点留出 | 需真实 Chrome（本机可，沙箱内需提权） | 是真机链路，但页面非真实站点 |
| `benchmarks/bench_wstn_state_pipeline.py`（+`data/wstn_state_pipeline_result.json`）、`bench_wstn_s3b6.py`（+`data/wstn_benchmark_result.json`）、`reports/EVAL-wstn-state-roc-and-causality.md` | 合成 DOM mutation 集 | 构造 golden path + 因果记录；近靶死路按设计注入 | **自带 disjoint holdout**（阈值在一组 mutation 上标定、另一组上测量） | 可（浏览器无关） | 现有资产里**唯一**明确做了 holdout 的；口径可直接借用 |
| `pkg/matcher/*_test.go`（dual_channel / structhash / wstn / probe_lsh_topology） | 合成内联夹具 | 语义正确性断言 | 无站点、无 holdout | 可（Go 单测） | 保证语义正确，不产出质量数字 |
| `pkg/probe_wasm/{src,test}` + `make probe-test` | 合成 | gtest 断言 | — | 需构建 | 只测探针本身 |
| 真实站点证据：`docs/evidence/pilots/dogfood-live-2026-09-23.md`、`docs/evidence/viewer/*`、`dev/*.html` | **真实站点（n≈1）+ 本地 py 页面** | 页面后验（URL 变化 / AX 文本 / 截图） | 站点覆盖 1（example.com/iana.org）；无 holdout | 需真机 | 是唯一"真实站点 + 页面侧标签"的证据，量级不足以当基线 |
| 线上快路径观测：`~/.uws/engine.log`（`no_match` 423 / `VLM 兜底` 13，无分母） | 真实运行 | gRPC 状态与日志 | 多站点但不可分层 | 只读可数 | **属 T-11B**；取消/未知/去重事件不落盘（见调度表 §6） |

## 2. 结论：能不能直接做离线基线

- **能做的**：`cmd/tier1` 那套「固定输入 → 可重复回放 → 构造真值判分」的形态已经存在，
  WSTN 系列还额外给了 holdout 的做法；候选/结构哈希/LSH 的度量原语（`hamming_distance`、
  `simhash64`、`feature_vector`）也都在仓内。**不需要从零搭 harness。**
- **不能直接当基线的**：没有带标签的**真实站点**快照语料。现有语料全部是合成 DOM
  （tier1 drift、classifier golden、WSTN、matcher 单测）或**真实浏览器 + 本地页面**（tier2）。
  因此「真实站点上的候选质量」目前**无法测量**，只能先给出合成集上的管线基线与指标口径。
- **缺失的指标入口**：现有 judge 判「真身是否被选中」（Top-1 命中），没有
  **候选召回（recall@k）、排序质量（MRR/NDCG）、落点相关性**的分层测量；
  各阶段耗时也只有 probe/classifier 的微基准，缺 probe → 规则 → matcher → classifier → VLM
  的分段计时。T-11A 第二批要补的正是这些**度量**，而不是新数据集。
- **必须分开测的四件事**（避免用一个"成功率"掩盖不同故障）：候选没召回 / 候选选错 /
  动作未派发 / 动作已派发但无页面后验证据。前两项归 T-11A，后两项归 T-10 的派发状态与 T-12 的 Episode。

## 3. 下一步（T-11A 的第二批）

1. ~~复现 tier1 离线管线（生成器 → Go runner → judge），确认 `SEED=42` 可重放~~ —— **已完成**，
   见 [`docs/tier1-candidate-quality-2026-09-23.md`](tier1-candidate-quality-2026-09-23.md) §2/§8
   （顺带修掉生成器一处未播种随机数：同 seed 曾字节不一致）。
2. ~~按 WSTN 的 holdout 形态新增候选质量指标（recall@k / MRR / Top-k 落点相关性）与分段耗时~~
   —— **已完成**（`benchmarks/tier1/bench_candidate_quality.py` + runner 诊断输出）。
   合成集结论：零检索损失、**零排序难度**（真身秩只有 0/1），故该语料不足以标定阈值或比较重排策略。
3. 真实站点语料按需采集（公开页、无账号），并在采集时即按站点留出 holdout（标签：动作后页面后验
   + 候选身份双口径）；未采集前，T-11A 的所有数字都标注为「合成集基线」。**未开始。**
