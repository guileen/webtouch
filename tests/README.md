# UWS 测试骨架说明

> 目标：docs/tdd-checklist.md（V1.1）是验收标准的唯一索引；本骨架保证
> 「每个 TDD ID 有主归属文件 + 可收集 + 可运行（骨架阶段全 skip 绿灯）」，
> 后续开发按「红-绿-重构」逐条点亮。

## 分层与语言就近原则

| 层 | 语言 | 位置 | 说明 |
| --- | --- | --- | --- |
| 单元/逻辑测试 | Go | `pkg/*/*_test.go` | 包内单测（Go 惯例），checklist 标「单元」的用例主归属 |
| 单元测试 | Python | `tests/unit/` | planner / pipeline / watchdog / adapter / cli |
| 契约/集成测试 | Python | `tests/integration/` | 经 gRPC 驱动引擎（需 mock CDP/LLM 等夹具） |
| 端到端测试 | Python | `tests/e2e/` | 真实 Chromium / Mock 站点（骨架阶段全 skip） |
| 安全/模糊测试 | Python | `tests/security/` | 注入、边界、伪造回调 |
| 性能/基准 | Python | `benchmarks/` | 延迟/内存/并发红线 |
| 探针测试 | C++ | `pkg/probe_wasm/test/` | native 编译；WASM 目标 M2 接 emscripten |

## TDD ID → 主归属文件

| TDD 章节 | ID 范围 | 主归属文件 |
| --- | --- | --- |
| §1 探针 | TDD-WASM-01..08 | `pkg/probe_wasm/test/probe_wasm_test.cc`（性能→`benchmarks/bench_probe_wasm.py`） |
| §2 匹配 | TDD-MATCH-01..06 | 单元：`pkg/matcher/matcher_test.go`；集成：`tests/integration/test_matcher_classifier.py`；安全：`tests/security/test_input_sanitization.py` |
| §2 分类 | TDD-ONNX-01..03 | 集成：`tests/integration/test_matcher_classifier.py`；性能：`benchmarks/bench_classifier.py` |
| §3 引擎 | TDD-ENG-01..13 | 逻辑：`pkg/browser/browser_test.go` + `pkg/verifier/verifier_test.go`；集成：`tests/integration/test_engine.py`；E2E：`tests/e2e/test_engine_attach.py`（08/12/13）、`test_engine_captcha.py`（ENG-05 确定性挑战拦截）、`test_export_state.py`（v1.5.6 ExportState 凭证导出）——均 `UWS_E2E_CDP=1` |
| §3.4 知识库 | TDD-KB-01..07 | `pkg/knowledge/knowledge_test.go` + `tests/integration/test_knowledge.py` |
| §4 协调器 | TDD-COORD-01..09 | 集成：`tests/integration/test_coordinator.py`；E2E：`tests/e2e/test_coordinator_vlm.py`（04）、`test_coordinator_crawl.py`（05/08） |
| §4 爬虫驱动 | TDD-COORD-05（08 已移交 search-engine） | 单元：`tests/unit/test_crawler.py`（05）；E2E：`tests/e2e/test_coordinator_crawl.py`（05 真实侧，`UWS_E2E_CDP=1`） |
| §4 规划器 | TDD-PLAN-01..05 | 单元：`tests/unit/test_planner.py`（01/02/04）；集成：`tests/integration/test_coordinator.py`（03/05） |
| §5 安全 | TDD-SEC-01..06 | `tests/security/test_pii_redaction.py`（01/03）、`test_input_sanitization.py`（02/06）、`test_solver_callback.py`（04/05） |
| §6 蒸馏 | TDD-PIPE-01..06 | 单元：`tests/unit/test_pipeline.py`（01/02）；集成：`tests/integration/test_pipeline_learning.py`（03..06） |
| §6 学习 | TDD-LEARN-01..03 | `tests/integration/test_pipeline_learning.py` |
| §6 看门狗 | TDD-WATCH-01..02 | 集成：`tests/integration/test_pipeline_learning.py`（01）；单元：`tests/unit/test_watchdog.py`（02） |
| §7 gRPC | TDD-API-01..07 | `tests/integration/test_grpc_contract.py`（CI 门禁专用） |
| §7 适配层 | TDD-ADAPTER-01..02 | `tests/unit/test_adapter.py` |
| §8 CLI / 目标模式 | TDD-CLI-01..22 + TDD-GOAL-01..09（03/08-12 已移交 search-engine） | 单元：`tests/unit/test_cli.py`（05）+ `test_cli_run.py`（16/20/21/22）+ `test_cli_probe.py`（19）+ `test_goal.py`（GOAL-01..09）+ `test_treefmt.py`（GOAL-08 摘要）；E2E：`tests/e2e/test_cli_*.py`（01 run / 02 crawl / 04 api / 06 zero-config / 07 debug-ui） |
| §9 性能 | TDD-PERF-01..07 | `benchmarks/bench_*.py` |
| §9.2 WSTN | TDD-WSTN-01..07 | `tests/unit/test_wstn.py` + `test_wstn_state.py`；`benchmarks/bench_wstn_s3b6.py` + `bench_wstn_state_pipeline.py` |

## 运行命令

```bash
make test          # go test ./pkg/... ./cmd/... + pytest tests/{unit,integration,security}
make pre-commit    # lint + test + gRPC 契约回归
make probe-test    # C++ 探针 native 测试（cmake + ctest）
make bench         # 性能基准（骨架阶段全 skip）
make collect       # 收集审计（确认所有骨架文件可被收集）
pytest tests/e2e   # E2E（需真实 Chromium：UWS_E2E_CDP=1 时执行 attach 与启动模式用例——ENG-08 附着、ENG-13 新窗口，其余骨架 skip）
UWS_E2E_CDP=1 pytest tests/e2e/test_export_state.py tests/e2e/test_engine_captcha.py tests/e2e/test_coordinator_vlm.py -v  # ExportState/ENG-05/COORD-04 三例（真实 Chrome）
```

## 目录结构映射说明

- 代码目录与五大子域划分以 `docs/README.md` 为主纲（`01-Bus`, `02-Organ`, `03-Engine-Alg`, `04-Learning`, `05-QA-Studio`），
  并按 `docs/contributing.md` 与 tdd-checklist 映射各模块实现；
- 新增 `pkg/knowledge/` 承载知识库双缓冲/回滚（TDD-KB-*，spec/knowledge-versioning.md）——
  规格未单列，为可测试性而设的引擎内子包边界。
