# probe_wasm 模块记忆（M2，2026-08-20 落地）

> 模块专属经验与踩坑；跨模块裁决见 `GLOBAL.md` §5 #21，验收见 `tdd-checklist.md` §1。

## 已点亮

- TDD-WASM-01（S1 P99=0.63ms）、02（20000 节点 16MB 截断）、03（分级准入+驱逐）、
  04/05/06（AST 降噪哈希，1000 对碰撞率 100%）、08（95% 降级 + 硬边界）；
  TDD-PERF-01（S2 5000 DOM 节点页 P99=11.61ms）。
- TDD-WASM-07 核心：溢出队列 BFS 序 + `uws_probe_overflow_peek`；JS 200ms 补偿待
  WASM 注入 V8（引擎侧 CDP `Runtime.evaluate` 通道，USER_ACTION #14）。

## 关键设计（踩坑沉淀）

1. **16MB 是 WASM 线性内存硬边界，不是 Arrow 区本身**：`-sINITIAL_MEMORY=16777216`
   + `-sALLOW_MEMORY_GROWTH=0`；Arrow 区取 15MB，留 1MB 给程序/栈/scratch/溢出队列
   （对应规格「预留 5% 余量」）。native 测试容量参数化（默认 16MB），小容量用例快且稳。
2. **standalone WASM 不自动跑全局构造器**：`WebAssembly.instantiate` 不调用
   `__wasm_call_ctors/_initialize`，全局 `ProbeEngine` 的 capacity 会是 0 → 首节点即
   「溢出」（kept=0/truncated=1 的假象）。解法：静态存储 + 惰性 placement-new，
   宿主零额外调用。
3. **emcc 首次构建/缓存**：沙箱内 `EM_CACHE` 必须指向可写目录（`make probe-wasm`
   默认 `build/emcc-cache`）；首次运行要建系统库（几分钟，之后增量秒级）。
4. **热路径零堆分配**：NodeScratch 用固定 char 槽 + 长度（不是 std::string）；
   AST 哈希单遍扫描 + FNV 投票（无 vector/string）；ctype 函数（isspace/isdigit）在
   热循环里是 locale 调用，换手写字符类判断；composite_key 手工拼接替代 snprintf。
   -O2 必须放 target_compile_options（放 link options 只影响链接，代码仍 -O0——
   实测 5000 节点 128ms → 26ms → 12ms 的三级跳）。
5. **AST 降噪细节**：引号必须排除出操作符字符类，否则字符串会被操作符游程吞掉
   （`fetch('/api-v1')` 的 `-v1'` 会渗入签名破坏字面量免疫）；`/` 单独停止操作符
   游程（注释提前处理）；function/class 后的声明名不参与签名（否则
   `submitA(` 被误判调用点）；重复 token 用 8 槽直接映射缓存（FNV 同哈希 → 同投票
   向量，碰撞安全）。
6. **批量输入协议**：`\x1e` 记录 / `\x1f` 字段分隔（19 字段固定序），分块写入
   （按记录边界切块）兼容 64KB scratch 与超大页面；逐字段 API 保留给 JS 降级补偿。
7. **驱逐语义**：缓冲满时高优先级节点可驱逐「最差已存节点」（tier 支配：任何溢出
   节点 tier ≥ 任何保留节点 tier），被驱逐节点按 BFS 序进溢出队列；memmove 前必须先
   取 victim 元数据（memmove 后指针指向被移位过来的下一条记录——实测把 id1 当成了
   id0 的经典 aliasing bug）。

## 构建/验证命令

```bash
make probe-test     # native（cmake + ctest）
make probe-wasm     # standalone WASM（emsdk emcc，16MB 固定内存）
make probe-bench    # Node 实测双场景 P99（产物缺失时自动 skip）
make pre-commit     # 全量回归（Go + Python + 契约）
```

## 剩余（真实链路前置，见 USER_ACTION #14）

- **已落地（2026-08-20 续15）**：WASM 注入 V8（CDP `Runtime.evaluate` + base64 +
  `WebAssembly.instantiate`）、页内 DOM→bulk 采集器（BFS + 19 字段分块）、
  JS 200ms 慢速遍历补偿合并、引擎 `WasmTruncationTotal` 计数器 + decision_trace
  `wasm_truncated` 标记。真实 Chrome 2 万节点页采集 316ms（kept=12288/overflow=7744）。
- 仍待：Prometheus HTTP 导出（依赖定案）、ast_source（内联 handler）→ AST SimHash
  通道、页内 AXTree 全链路 E2E。
