# tests/fixtures

共享测试夹具（docs/tdd-checklist.md §0.5）：

| 目录 | 用途 |
| --- | --- |
| `mock_cdp/` | Mock CDP 会话（pkg/browser、tests/integration/test_engine.py） |
| `mock_vlm/` | Mock VLM 服务（TDD-COORD-04） |
| `mock_solver/` | Mock 外部破解服务 + 回调签名（TDD-SEC-04/05） |
| `mock_sites/` | Mock 博客站点（TDD-COORD-05/08）与 SPA 示例页（TDD-ENG-06/07） |
| `golden_samples/` | 黄金样本集（AXTree JSON + 期望候选节点，TDD-ONNX-03） |
| `snapshot_harness/` | 页内快照 / GT 判据真机夹具（13 用例；§5.0.13 条款 7 对称骨架 ＋ §5.0.14 条款 1③/2） |

纪律：涉时/涉网用例固定时钟与网络延迟；SQLite 一律 `:memory:` 或独立临时库，严禁共享线上规则库。
