# WebTouch 贡献者指南

欢迎参与 WebTouch 建设。命令主名是 `webtouch`（`uws` 为弃用别名）。本文档指导如何搭建环境、开发模块、遵循规范并提交变更。

---

## 1. 开发环境搭建

### 1.1 前置依赖
- **Python**: 3.11+（推荐使用 `uv` 管理虚拟环境）
- **工具链**: `make`, `docker`, `buf` / `protoc`
- **WASM 编译（可选）**: Emscripten (`emcc` 3.1+)

### 1.2 快速启动
```bash
git clone <repo-url> && cd webtouch
uv venv && source .venv/bin/activate
uv pip install -e ".[dev]"
make proto-gen  # 编译 proto 生成代码
make test       # 运行基础单元测试
```

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

- **Python**：使用 `ruff` 进行格式化与 lint，启用 `mypy` 静态类型检查。
  ```bash
  make lint   # 运行 ruff check + ruff format --check + mypy
  make fix    # 自动格式化与修复
  ```
- **C++ / WASM**：遵循 Google C++ 规范，使用 `clang-format`。
- **安全检查**：外部输入字段（`target_semantic`、`value` 等）必须通过白名单校验与长度限制。

---

## 5. 测试要求

提交改动必须附带自动化测试，并通过以下验证：

```bash
# 运行单元测试（核心模块覆盖率门禁 ≥ 80%）
pytest tests/unit --cov=engine --cov=pipeline

# 运行 gRPC 契约与端到端测试
pytest tests/integration/test_grpc_contract.py
```

- **在线引擎**：修改必须包含延迟回归测试，确保无内存泄漏。
- **规则与蒸馏**：必须包含困难负样本与拓扑孤岛评估用例。

---

## 6. 分支与提交规范

### 6.1 分支策略
- `main`：稳定发布分支，受保护。
- `develop`：日常主开发分支。
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
