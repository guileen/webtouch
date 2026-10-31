# probe_wasm/src

WASM 探针 C++ 源码目录（M2 已落地，2026-08-20）：
- `probe.h/cc`：`ProbeEngine`——固定宽度 NodeFeature 序列化（16MB 硬边界）、
  分级准入 + 驱逐（视口内 > 交互 > a > div[role] > 其他）、溢出队列（BFS 序）、
  usage>95% 降级标记、越界受控报错。
- `ast_hash.h/cc`：`AstSemanticHash`——剥离注释/字符串/数字/十六进制，保留操作符 +
  调用点 + 控制关键字；64-bit SimHash LSH（FNV-1a 零堆分配投票）。
- `wasm_main.cc`：standalone WASM 导出（逐字段 + 批量 `nodes_bulk` 协议，
  惰性初始化，16MB 固定内存）。

构建/验证：
```bash
make probe-test     # native（cmake + ctest）
make probe-wasm     # standalone WASM（emsdk emcc）
make probe-bench    # Node 实测（S1/S2 P99 < 15ms）
```
剩余：页内 DOM→bulk 采集器 + WASM 注入 V8 + JS 200ms 降级补偿（见 USER_ACTION #14）。
