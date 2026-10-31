# pkg/verifier 模块经验（2026-08-19）

- 实现为自包含、可注入时钟的状态机：引擎侧通过 `FeedPushState` / `FeedNetwork` /
  `FeedDOMMutation` 喂事件，`Advance()` 周期性推进（后续集成见 tests/integration/test_engine.py）。
- 语义裁定（冻结规格未细化的部分）：
  - 静息起点（idleSince）在「观察到活动连接 <2 且 DOM 突变防抖已过」的那次 Advance 建立；
    FeedNetwork 回落到 <2 时也立即建立。
  - DOM 突变（防抖 150ms 窗口外的）作废已累计静息窗口；窗口内重复突变只合并一次。
  - 重复 pushState 不重置导航起点；总超时 1500ms 从首次 pushState 起算。
- TDD-ENG-06/07 Go 逻辑层已点亮（6 用例全绿）；gRPC/集成层（test_engine.py）留待引擎接线。
