# snapshot_harness — 页内快照 / GT 判据真机夹具

支撑 §5.0.13 条款 7（候选骨架**对称挂载**，`CON-214` 伪特征硬线）与 §5.0.14 条款 1③ / 条款 2
（受体派发点为唯一锚点、三级判据）的**「已锁死」级证据**。按 `CON-219` 持有仓内 VCS 路径。

## 前置

- Node ≥ 22（依赖全局 `fetch` / `WebSocket`）；**无 npm 依赖**。
- 本机装有 Google Chrome（默认 `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`）。

## 运行

```bash
node tests/fixtures/snapshot_harness/harness.mjs
```

- `UWS_FIXTURE_CHROME`：覆盖 Chrome 可执行文件路径。
- `UWS_FIXTURE_PORT`：覆盖调试端口（默认 `9333`）；**`9222` / `9224` 被显式拒绝**（`CON-217`
  宿主交互域零侵入硬线）。
- 每次运行自建**独立临时 profile**（`$TMPDIR/wtx-snapshot-harness-XXXX`）与独立端口，运行结束
  （含异常退出）自动回收 profile 目录并 `SIGKILL` 自建 Chrome；**不接管、不写入、不关闭任何既有会话**。

## 期望输出

末行 `fixtures=13 pass=13 fail=0`，退出码 `0`。

## 设计要点

- **单一事实源**：页内脚本不在此复制，运行时自 `pkg/browser/cdp_session.go` 的
  `const pageSnapshotJS = \`…\`` 提取 ⇒ 夹具测的是引擎**实际下发**的脚本，脚本改了夹具立刻跟着变。
- 13 个用例覆盖：KB selector、模糊 tag/text、歧义无枢轴、selector 未命中、零候选、几何兜底、
  1 阶跳数上限、祖先链优于几何、祖先 3 阶边界、受体落在 `<svg>`/`<path>` 上走祖先链、受体落在空白
  区**如实判 -1（不编造 GT）**、最近可交互祖先、**对称骨架（三候选 3/3/3 邻居、`rels=parent,sibling,sibling`）**。

## 审计边界（不得外推）

- 本夹具验证的是**页内脚本行为**（页内候选枚举 / GT 判据 / 1 阶骨架及其截断），
  **不覆盖** Go 侧落盘映射（`pkg/engine/episode.go` 的 `capturePreDispatch` / `skeletonFromNeighbors` /
  顶层 `Skeleton` 降为 GT 别名）——后者由 Go 单测
  （`pkg/engine/episode_test.go::TestEpisodeCandidateSkeletonSymmetric`）覆盖。
- 夹具证明的是「给定页面上页内脚本的输出正确」，**不能**用于推断线上站点命中率或蒸馏收益。
