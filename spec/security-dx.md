
## 竞品补充分析：OpenClaw 与 Hermes 的浏览器操控机制

### OpenClaw 的浏览器自动化架构

OpenClaw 的浏览器自动化核心是基于 **agent-browser**（一个 Rust 编写的无头浏览器自动化 CLI）实现的。其架构特点包括：

1. **专用浏览器 Profile 隔离**：OpenClaw 运行一个由 Agent 控制的专用 Chrome/Brave/Edge/Chromium 配置文件，通过 Gateway 内的本地控制服务管理（仅回环地址），与用户个人浏览器完全隔离。
2. **确定性 Tab 控制**：提供 `list/open/focus/close` 等确定性 Tab 操作，Agent 动作包括 click/type/drag/select、snapshots、screenshots、PDFs。
3. **Playwright 后端 + CDP**：底层通过 Playwright 驱动浏览器，同时支持通过 CDP 连接现有 Chrome 实例。
4. **Snapshot-driven 操作循环**：内置 `browser-automation` skill，教导 Agent 执行 snapshot、stable-tab、stale-ref、manual-blocker recovery loop。

### Hermes 的浏览器自动化架构

Hermes Agent 的浏览器自动化采用**可插拔后端（pluggable backend）** 架构，支持多种 Provider：

1. **多种后端选项**：Browserbase 云模式（反机器人工具链）、Browser Use 云模式、Firecrawl 云模式、Camofox 本地模式（Firefox 指纹伪装）、本地 Chrome 通过 CDP 连接。
2. **AXTree 文本快照**：页面表示为**可访问性树（Accessibility Tree）文本快照**，交互元素获得 `@e1`、`@e2` 等引用 ID。这与 WebTouch 的 AXTree 方案在理念上高度一致。
3. **混合路由**：配置云 Provider 时，对私有/LAN 地址自动回退到本地 Chromium。
4. **反检测浏览器后端**：Camofox 是基于 Firefox 的指纹伪装浏览器。
5. **像素级无 CDP 方案**（hermes-computer-use）：通过 **screenshot + xdotool（OS级鼠标/键盘输入）** 驱动真实 Chrome，不暴露 `navigator.webdriver`，在 Cloudflare/reCAPTCHA/DataDome 等场景下绕过检测。

### 对 WebTouch 的启示与差异化定位

| 维度 | OpenClaw | Hermes | WebTouch（本设计） |
|------|----------|--------|---------------|
| **感知方式** | Snapshot（AXTree 文本快照） | AXTree 文本快照 + Vision | **WASM 探针 16MB Arrow Buffer** |
| **定位精度** | 引用 ID（`@e1`） | 引用 ID（`@e1`） | **AC-BM25 + ONNX + AST降噪哈希** |
| **执行延迟** | 数百 ms（CDP 往返） | 数百 ms | **< 15ms（WASM 单次注入）** |
| **LLM 依赖** | 每次操作依赖 LLM 决策 | 每次操作依赖 LLM 决策 | **规则优先，LLM 调用率 < 15%** |
| **成本模型** | 按 Token/API 调用计费 | 按 Token/API 调用计费 | **离线蒸馏固定成本 + 在线极低成本** |
| **反检测** | Playwright 内置 stealth | Camofox / 像素级无 CDP | **Fitts 动力学曲线 + Z轴遮挡检测 + JA3 指纹同构** |
| **知识进化** | 无（每次重新推理） | 无（每次重新推理） | **L1 离线蒸馏 + L2 影子评估自进化** |

**核心结论**：OpenClaw 和 Hermes 是“**LLM 驱动的浏览器 Agent**”——每次操作都由 LLM 决策，Token 消耗线性增长。WebTouch 是“**规则驱动的浏览器自动化底座**”——LLM 仅在离线蒸馏和罕见兜底时使用，在线执行是确定性规则引擎。前者灵活但昂贵，后者高效但需要知识库建设。WebTouch 的差异化在于**成本结构**和**执行速度**，而非灵活性。

---

## 文档四：安全、合规与开发者体验规约（Security, Compliance & DX Addendum）

**版本**：V1.0  
**状态**：冻结，可编码  
**依赖**：WebTouch内核V1.4，Platform Boundary Manual V1.0，Coordinator Design V1.0  
**生效日期**：2026-08-19

---

## 1. gRPC 认证与授权（Authentication & Authorization）

### 1.1 设计原则
- **零信任**：所有 gRPC 请求必须经过认证，无论来源（包括本地协调器）。
- **最小权限**：不同调用方（协调器、运维工具、监控探针）持有不同级别的凭证。
- **密钥轮换**：支持周期性密钥轮换，不停机。

### 1.2 认证方案：mTLS + API Key 双因素

| 层级 | 机制 | 说明 |
|------|------|------|
| **传输层（mTLS）** | 双向 TLS 认证 | 服务端和客户端各持证书，验证对方身份。防止中间人攻击和伪造客户端。 |
| **应用层（API Key）** | 请求头 `X-API-Key` | 每个调用方分配独立 API Key，用于细粒度权限控制和审计。 |

**证书管理**：
- 服务端证书：由内部 CA 签发，有效期 1 年，部署在 WebTouch 引擎节点。
- 客户端证书：每个协调器实例/运维工具独立签发，有效期 90 天。
- 证书吊销列表（CRL）：定期同步，吊销后立即失效。

**API Key 管理**：
```yaml
# 配置示例（WebTouch 引擎 sidecar）
api_keys:
  - key: "uws_coordinator_prod_xxx"
    role: "coordinator"
    permissions: ["ExecuteAction", "ExecuteActionStream", "HealthCheck"]
    expires_at: "2027-08-19T00:00:00Z"
  - key: "uws_ops_tool_yyy"
    role: "operator"
    permissions: ["ReloadKnowledge", "HealthCheck", "GetKnowledgeVersion"]
    expires_at: "2027-08-19T00:00:00Z"
  - key: "uws_monitor_zzz"
    role: "monitor"
    permissions: ["HealthCheck", "GetKnowledgeVersion"]
    expires_at: "2027-08-19T00:00:00Z"
```

### 1.3 密钥轮换流程

1. **新密钥生成**：运维人员通过内部工具生成新 API Key，写入配置并重启 Gateway（或热加载）。
2. **灰度切换**：协调器先使用新密钥重试，失败则回退旧密钥（双密钥窗口期 24 小时）。
3. **旧密钥撤销**：24 小时后，旧密钥从配置中移除，所有请求强制使用新密钥。
4. **审计日志**：每次认证失败记录 `trace_id`、来源 IP、时间戳，触发告警（连续失败 > 5 次/分钟）。

---

## 2. XSS / 指令注入防护（Input Sanitization）

### 2.1 威胁模型

WebTouch 引擎接收来自外部协调器的以下输入字段，可能包含恶意载荷：

| 字段 | 来源 | 风险 |
|------|------|------|
| `target_semantic` | 协调器（可能来自用户自然语言） | XSS、SQL 注入（若拼接到查询） |
| `value` | 协调器（用户输入，如表单填写值） | XSS、命令注入 |
| `context_hint` | 协调器（任意键值对） | 键名注入、JSON 篡改 |

### 2.2 防护策略（纵深防御）

**L1：输入验证（门禁）**
- `target_semantic`：长度 ≤ 120 字符，仅允许 `[a-zA-Z0-9_\-\u4e00-\u9fa5\s]`（中英文、数字、下划线、连字符、空格）。超限或含特殊字符（`<>&'"/`）直接拒绝，返回 `INVALID_ARGUMENT`。
- `value`：按 UTF-8 字节计限；`input` 允许 ≤256 KiB，支持多轮讨论历史与文本附件；`evaluate` 允许 ≤1 MiB 脚本，其他动作保持 ≤4096 字节。超限在派发前返回 `INVALID_ARGUMENT`，不截断、不分段发送。非 `evaluate` 仍禁止 `javascript:` 前缀；输入正文作为 CDP 文本参数传递，不作为 HTML 或代码执行；展示时按输出上下文转义。
- `context_hint`：键名长度 ≤ 64，仅允许 `[a-zA-Z0-9_]`；值长度 ≤ 256，执行与 `value` 相同的转义。

**L2：输出编码（逃逸）**
- 所有写入日志、快照、SQLite 的值，在存储前执行 JSON 序列化转义（Go/Python 标准库 `json.Encoder`）。
- 快照中若包含用户输入值（如 `input.value`），在 PII 脱敏基础上额外执行 HTML 实体编码。

**L3：参数化查询（SQLite）**
- 所有 `uws_rules` 和 `online_logs` 的查询必须使用**参数化查询**（Prepared Statement），严禁字符串拼接。
```python
# 正确示例
cursor.execute("SELECT * FROM uws_rules WHERE text_pattern = ?", (pattern,))
# 错误示例（禁止）
cursor.execute(f"SELECT * FROM uws_rules WHERE text_pattern = '{pattern}'")
```

---

## 3. Playwright 适配层轻量设计（Playwright Adapter）

### 3.1 设计目标
- **降低迁移成本**：现有 Playwright/Puppeteer RPA 开发者可以用熟悉的 API 风格调用 WebTouch。
- **薄封装**：适配层仅做 API 风格转换，不引入额外逻辑，保持 WebTouch 引擎纯净。

### 3.2 API 映射

| Playwright API | WebTouch gRPC 映射 | 说明 |
|----------------|---------------|------|
| `page.locator('text=登录').click()` | `ExecuteAction(target_semantic="登录", expected_action="click")` | 语义定位 |
| `page.locator('#username').fill('alice')` | `ExecuteAction(target_semantic="username_input", expected_action="input", value="alice")` | 语义标签由协调器映射 |
| `page.goto('https://example.com')` | `InitializeContext(initial_url=...)` | 导航在初始化阶段完成 |
| `page.screenshot()` | 通过日志快照或独立 `Snapshot` RPC（V1.5） | 暂不支持直接截图 API |
| `page.waitForSelector()` | 由 WebTouch 内部 Network Idle 机制保证 | 无需显式等待 |

### 3.3 Python SDK 示例（MVP）

```python
# 适配层接口（开发者视角）
from uws_adapter import UWSClient

client = UWSClient(endpoint="localhost:50051", api_key="xxx")

# 初始化浏览器
page = client.new_page(url="https://example.com/login")

# 语义化操作（与 Playwright 风格对齐）
page.fill("username_input", "alice")       # target_semantic 由适配层映射
page.fill("password_input", "secret")
page.click("login_button")

# 等待导航完成
page.wait_for_navigation()

# 提取数据
text = page.extract_text("welcome_message")
```

### 3.4 适配层部署方式
- **独立进程**：作为轻量级 Sidecar 与 WebTouch 引擎同 Pod 部署，或作为 Python 包由协调器直接引入。
- **不修改 WebTouch 引擎**：适配层仅调用标准 gRPC 接口，不触碰 WebTouch 内部状态。

---

## 4. 可视化调试工具 MVP 规格（Debug UI）

### 4.1 设计目标
- 降低开发者排障门槛：当规则误判或任务失败时，开发者能快速定位根因。
- **基于日志回放**：不实时连接浏览器，而是读取 `online_logs` 表中的历史数据重建现场。

### 4.2 功能规格（MVP，V1.0）

| 功能模块 | 说明 | 数据来源 |
|----------|------|----------|
| **任务时间线** | 按 `request_id` 聚合，展示动作序列、耗时、成功/失败 | `online_logs` |
| **决策轨迹详情** | 对单次动作展示 `decision_trace` 完整 JSON：规则匹配、ONNX 打分、BM25 得分、候选节点数 | `decision_trace` 字段 |
| **AXTree 快照可视化** | 渲染动作发生时的 AXTree（文本树），高亮候选节点和被选中的目标节点 | `node_features` + `snapshot_blob` |
| **规则命中分析** | 展示哪些规则被匹配、置信度、为何未命中更高分规则 | `decision_trace.rule_id` + `uws_rules` 表 |
| **失败分类统计** | 按错误码（`NOT_FOUND`/`DEADLINE_EXCEEDED`/`ABORTED`）聚合，展示高频失败模式 | `online_logs.success` + `error_reason` |

### 4.3 技术实现（轻量级 Web UI）

- **后端**：WebTouch 引擎内置一个只读 HTTP 端点（`/debug/task/{request_id}`），返回 JSON 格式的日志详情。
- **前端**：单页 HTML + Vanilla JS，通过 `fetch` 调用后端端点渲染视图。无需构建工具，单个 HTML 文件即可部署。
- **部署**：挂载在 WebTouch 引擎的 `:8080` 端口（与 gRPC 端口 `:50051` 分离），仅限内网访问。

### 4.4 安全约束
- 调试端口**必须**绑定 `127.0.0.1` 或内网 IP，禁止暴露公网。
- 所有快照和日志中的 PII 已脱敏，调试 UI 展示的是脱敏后的数据（符合合规要求）。

---

## 5. 竞品对比总结与定位声明（供市场/产品使用）

| 对比维度 | OpenClaw | Hermes | Browser Use | **WebTouch** |
|----------|----------|--------|-------------|---------|
| **架构模式** | LLM Agent + Playwright | LLM Agent + 可插拔后端 | LLM Agent + Playwright | **规则引擎 + LLM蒸馏** |
| **在线决策** | 每次 LLM 推理 | 每次 LLM 推理 | 每次 LLM 推理 | **规则匹配（< 15% LLM）** |
| **单步延迟** | 500-2000ms | 500-2000ms | 500-2000ms | **< 50ms（规则命中）** |
| **Token 消耗/任务** | 高（线性增长） | 高（线性增长） | 高（线性增长） | **低（固定 + 少量兜底）** |
| **知识进化** | 无 | 无 | 无 | **L1 离线蒸馏 + L2 影子评估** |
| **反检测** | Playwright stealth | Camofox / 像素级无 CDP | Playwright stealth | **Fitts 动力学 + JA3 同构 + Z轴检测** |
| **适用场景** | 灵活、低频任务 | 灵活、多后端 | 灵活、社区活跃 | **高频、大规模、成本敏感** |

**WebTouch 的独特价值主张**：
> 当你的浏览器自动化任务从“每周几次”变成“每秒几百次”时，LLM Agent 的 Token 账单会让你重新思考架构。WebTouch 把 LLM 的“智能”蒸馏成规则引擎的“配置”，让在线执行的成本趋近于零。

---

**文档四结束。至此，所有 P0/P1 级缺失文档已全部补齐。**

| 序号 | 文档 | 优先级 | 状态 |
|------|------|--------|------|
| 第0步 | `algorithm-kernel.md` V1.4 合并重写 | P0 | ✅ 已完成 |
| 文档一 | 协调器智能规划模块设计 | P0 | ✅ 已完成 |
| 文档二 | 物理执行补丁附录（#39/#40 + Proto补丁） | P0/P1 | ✅ 已完成 |
| 文档三 | 知识库版本化与运维回滚SOP | P1 | ✅ 已完成 |
| **文档四** | **安全、合规与开发者体验规约** | **P1** | **✅ 刚完成** |
| 文档五 | Benchmark & 竞品对比框架 | P2 | ⏳ 延后（M4验收后启动） |

**全部核心缺失文档已交付，可进入并行编码阶段。**
