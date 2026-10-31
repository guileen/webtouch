# WebTouch 贡献者指南

欢迎参与 WebTouch 建设。命令主名是 `webtouch`（`uws` 为弃用别名）。本文档指导如何搭建环境、开发模块、遵循规范并提交变更。

贡献受 [MIT 许可证](../LICENSE)约束；漏洞请按[安全政策](../SECURITY.md)私密报告。

---

## 1. 开发环境搭建

### 1.1 前置依赖与初始化

需要 Go 1.25.0、Python 3.11、uv 0.13.6、make、C/C++ 编译器、ripgrep、curl 和 unzip。Linux x86_64 与 macOS x86_64/arm64 可用同一工具链入口：

```bash
git clone https://github.com/guileen/webtouch.git
cd webtouch
git switch -c my-change origin/main
uv sync --frozen --group dev --python 3.11
bash scripts/ci-bootstrap.sh
export PATH="$PWD/.tools/bin:$PATH"
make ci
```

`make ci` 执行 gofmt 检查、分层与消费方审计、Proto 生成物一致性、全部 Go 包及 Python 基础测试、构建与版本自检。未检出兄弟仓时消费方审计会跳过，不代表跨仓已验。CI 不调用 `make install` 或生产 daemon；真实浏览器、WASM、ONNX 和桌面运行时需另行验证。当前主线有已知阻塞：分层审计发现 gateway 直接依赖 browser；部分 Python 测试引用本仓不存在的 pipeline / benchmarks，收集失败。修复前 CI 不应视为通过。

---

## 2. 目录结构与模块边界

> ⚠️ 早期目录按「在线执行环 `engine/` / 离线蒸馏环 `pipeline/`」划分，与当前仓库
> 实际结构不一致（`engine/` 已并入 `pkg/engine`；网关源码在 `cmd/uws-gateway`，
> 二进制名是 `webtouch-gateway`）。`pipeline/`、`learning/` 与历史架构稿不在本仓库树内。
> **当前目录结构以 [`README.md`](../README.md) 为准**，领域地图以 [`README.md`](README.md) 为准。

**模块边界与资源约束**：
- **内核层（`pkg/browser`、`pkg/verifier`、`pkg/probe_wasm` 等）**：严格控制依赖，
  CPU 单核、内存 <200MB，感知延迟 ≤15ms，单步命中延迟 <50ms。
- **应用层（`apps/gateway`、`cmd/uws-gateway`）**：在核心之外，只经对外 API 使用 WebTouch，
  **核心禁止反向依赖**（历史 Python `coordinator/app/` 已物理移除归档）。
- **数据安全与合规**：所有日志及快照必须通过 PII 脱敏；SQLite 查询严禁拼接，必须使用参数化查询。

---

## 3. Proto 变更流程

[`proto/uws_service.proto`](../proto/uws_service.proto) 是系统对外接口与内部通讯的单一事实源。

1. **向后兼容原则**：仅允许追加字段/枚举，禁止修改已有字段 tag 编号与语义。
2. **代码生成**：修改 proto 后执行 `make proto-gen` 重新生成各语言桩代码。
3. **原子提交**：Proto 文件改动必须与其生成的 bindings 处于同一提交中。

---

## 4. 代码风格与规范

`make lint` 当前只检查 Go 格式；Ruff、mypy 和 clang-format 尚未接入门禁。外部输入须校验与限制长度。

## 5. 测试要求

按变更范围编写测试，运行 `make ci`；`make pre-commit` 复用相同检查。未设覆盖率阈值门禁。在线引擎、规则与蒸馏变更还需对应性能和困难负样本验证，详见 [TDD 检查单](tdd-checklist.md)。

---

## 6. 分支与提交规范

### 6.1 分支策略
- 从 `origin/main` 创建工作分支并提交 PR；分支保护以仓库实际设置为准。
- 特性分支：`feat/<module>-<desc>`（如 `feat/probe-arrow-buffer`）
- 修复分支：`fix/<module>-<desc>`（如 `fix/async-spa-route`）

### 6.2 提交规范（Conventional Commits）
格式：`<type>(<scope>): <subject>`

- **type**：`feat`, `fix`, `refactor`, `perf`, `docs`, `test`, `proto`, `chore`
- **scope**：`agent`, `probe`, `distill`, `eval`, `grpc`, `coordinator` 等
- **示例**：`feat(probe): support 16MB arrow buffer in wasm probe`

---

## 7. PR 提交流程与模板

1. 基于 `develop` 创建分支，完成代码与测试。
2. 运行 `make pre-commit` 确保 linter 与单元测试全绿。
3. 提交 PR 并填写模板：

```markdown
## 变更描述
- 简述本次修改的核心动机与实现逻辑

## 涉及模块
- [ ] engine / [ ] pipeline / [ ] learning / [ ] proto / [ ] coordinator

## 验证与测试
- [ ] 单元测试通过（覆盖率: __%）
- [ ] 性能与延迟基准验证通过
- [ ] Proto 契约向后兼容

## 关联 Issue
Closes #
```
