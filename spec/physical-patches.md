
# 文档二：物理执行补丁附录（Physical Execution Addendum — #39 & #40）

**版本**：V1.0  
**状态**：冻结，可编码  
**依赖**：WebTouch内核V1.4 + Platform Boundary Manual V1.0（Part A & B）  
**生效日期**：2026-08-19

---

## 1. 补丁范围

本附录修正 `platform_bound.md` 的两个遗留工程缺口，并对 `Physical_Execution_Manual` 中悬而未决的外部依赖和降级策略进行封口：

| 编号 | 来源 | 问题描述 |
|------|------|----------|
| **#39** | Physical Execution Manual §3 | `CHALLENGE_INTERCEPTED` 状态下“交割现场至外部破解算力群”的通信契约未定义 |
| **#40** | Physical Execution Manual §4 | WASM探针16MB截断后，目标节点若被丢弃，无补偿机制 |
| **#PBM-1** | Platform Boundary Manual Part A | gRPC proto 缺少 `InitializeContext` 接口定义（但 Physical Execution §2 要求必须先调用） |
| **#PBM-2** | Platform Boundary Manual Part A | `CHALLENGE_INTERCEPTED` 状态未映射到 gRPC Status Code |

---

## 2. gRPC Proto 补丁（修复 #PBM-1 和 #PBM-2）

在 `uws_service.proto` 中**新增**以下 RPC 方法和状态枚举，与原有 `ExecuteAction` 等接口并列。

### 2.1 新增 `InitializeContext` RPC

```protobuf
service UwsEngine {
    // --- 原有接口保留 (ExecuteAction, ExecuteActionStream, ReloadKnowledge, HealthCheck) ---
    
    // ★ 新增：初始化浏览器沙盒上下文（必须在 ExecuteAction 之前调用）
    rpc InitializeContext(InitContextRequest) returns (InitContextResponse);
}

message InitContextRequest {
    string task_id = 1;                     // 协调器侧任务ID，用于关联日志
    
    // 四元组沙盒隔离（Physical Execution §2 强制要求）
    ProxyConfig proxy = 2;                  // 可选，若不填则使用默认出口
    ViewportConfig viewport = 3;            // 视口尺寸，默认 1920x1080
    repeated Cookie cookies = 4;            // 预置Cookie（用于保持会话）
    string timezone = 5;                    // 时区，如 "Asia/Shanghai"
    
    // 浏览器启动参数（可覆盖默认值）
    optional BrowserLaunchOverrides launch_overrides = 6;
    
    // 可选：初始导航 URL（Playwright 适配层使用）
    optional string initial_url = 7;
}

message InitContextResponse {
    string target_id = 1;                   // WebTouch分配的CDP Target ID
    string context_id = 2;                  // BrowserContext ID（用于后续关联）
    string session_url = 3;                 // 调试用，可选的DevTools URL
    bool ok = 4;
    string error_reason = 5;                // 若失败，返回具体原因
}

message ProxyConfig {
    string host = 1;
    int32 port = 2;
    optional string username = 3;
    optional string password = 4;
}

message ViewportConfig {
    int32 width = 1;    // 默认 1920
    int32 height = 2;   // 默认 1080
    float device_scale_factor = 3;  // 默认 1.0
}

message Cookie {
    string name = 1;
    string value = 2;
    optional string domain = 3;
    optional string path = 4;
    optional bool http_only = 5;
    optional bool secure = 6;
    optional int64 expires = 7;  // Unix timestamp
}
```

### 2.2 新增 `CHALLENGE_INTERCEPTED` 状态码映射

在 `ActionResultResponse` 中，原有 `ContextState` 枚举扩展为：

```protobuf
message ActionResultResponse {
    // ... 原有字段保持不变 ...
    ContextState context_state = 7;
    
    enum ContextState {
        STABLE = 0;
        NAVIGATING = 1;               // ★ 新增（V1.4合并回写）
        NAVIGATED = 2;                // 原值1，顺延
        NEW_WINDOW_OPENED = 3;        // 原值2，顺延
        CHALLENGE_INTERCEPTED = 4;    // ★ 新增：WAF拦截/验证码挑战，需外部破解
    }
}
```

**对应的 gRPC 错误码映射**（补充 Part A.2 表格）：

| gRPC Status Code | WebTouch 内部原因 | 状态语义 | 协调器对策 |
|------------------|--------------|----------|------------|
| `UNAUTHENTICATED (16)` | `CHALLENGE_INTERCEPTED` | 被 WAF/验证码拦截，需外部破解 | **调用外部破解服务**（见第3节契约）；若破解失败，**不可重试**，抛出异常通知人工介入 |

> 选择 `UNAUTHENTICATED` 而非自定义码，是因为它最贴近“需要额外凭证/验证”的语义，且 gRPC 标准库对其有原生支持，无需自定义传输层。

---

## 3. 外部破解算力群接口契约（修复 #39）

当 WebTouch 引擎检测到 WAF 拦截或验证码挑战（如 Cloudflare Turnstile / DataDome 探针触发），且内部 AC 自动机命中 `CHALLENGE_INTERCEPTED` 模式时，**不得盲目重试或直接返回失败**，必须将现场交割给外部破解集群。

### 3.1 触发条件

WebTouch 引擎在以下任一条件满足时，主动触发 `CHALLENGE_INTERCEPTED`：

1. `Network.responseReceived` 返回 HTTP 403 / 429，且响应体含 `captcha`、`challenge`、`cf-challenge` 等关键词。
2. 页面 AXTree 中出现 `iframe[src*="challenges.cloudflare.com"]` 或 `div[class*="turnstile"]`，且交互节点数骤降（< 3）。
3. 探针检测到 `navigator.webdriver` 属性虽被剥离，但 `window._cf_chl_opt` 等 Cloudflare 特有变量存在。

### 3.2 回调契约（Webhook 模式）

WebTouch 引擎以 **异步 Webhook** 方式将现场交割给外部破解服务，不阻塞当前 gRPC 流（若为流式，则挂起当前动作，等待回调）。

**请求（WebTouch → 破解服务）**：

```json
POST https://{external-solver-endpoint}/v1/solve
Content-Type: application/json
X-API-Key: {pre-shared-key}

{
  "task_id": "0192f3d4-...",
  "target_id": "ABC123",
  "challenge_type": "cloudflare_turnstile" | "recaptcha_v2" | "recaptcha_v3" | "datadome",
  "page_url": "https://example.com/login",
  "site_key": "0x4AAAAAAA...",          // 若可提取
  "proxy": {                            // 复用当前会话的代理
    "host": "proxy.example.com",
    "port": 8080,
    "username": "...",
    "password": "..."
  },
  "screenshot_base64": "data:image/png;base64,...",  // 当前视口截图（压缩<500KB）
  "html_snapshot_base64": "..."          // zstd压缩的HTML片段（可选）
}
```

**响应（破解服务 → WebTouch，同步返回 ack，异步回调结果）**：

```json
// 同步ACK（<100ms）
{
  "solver_task_id": "solver-xxx",
  "status": "accepted",
  "estimated_ms": 15000,           // 预估破解耗时
  "callback_url": "https://uws-engine/callback/solve-result"
}

// 异步回调（破解完成后，破解服务 → WebTouch）
POST {callback_url}
{
  "solver_task_id": "solver-xxx",
  "status": "solved" | "failed",
  "solution": {
    "token": "xxx.yyy.zzz",        // 若为Turnstile/Recaptcha
    "cookies": [                   // 若破解过程自动设置，返回更新后的Cookie
      {"name": "cf_clearance", "value": "...", "domain": ".example.com"}
    ]
  },
  "failure_reason": "timeout | unknown_site_key | proxy_error"
}
```

### 3.3 超时与重试策略

| 参数 | 取值 | 说明 |
|------|------|------|
| 同步ACK超时 | 2s | 破解服务无响应则视为不可用 |
| 破解等待总超时 | 60s | 超过则放弃，返回 `DEADLINE_EXCEEDED` 给协调器 |
| 重试次数 | 1次 | 仅当破解服务返回 `status=accepted` 但回调超时时，换备用破解实例重试 |
| 降级路径 | 若所有破解尝试失败 | 返回 `UNAUTHENTICATED` 给协调器，由人工接管或任务失败 |

### 3.4 安全约束

- WebTouch 与破解服务之间必须使用 **mTLS** 双向认证。
- 回调接口必须验证请求头中的 `X-Solver-Signature`（HMAC-SHA256，预共享密钥），防止伪造回调污染状态机。
- 截图和 HTML 快照传输过程中使用 HTTPS，且仅保留至破解完成即删除。

---

## 4. WASM 探针截断补偿机制（修复 #40）

### 4.1 截断发生条件

WASM 探针在注入后分配 **16MB 线性连续 Arrow Buffer**。若当前页面的交互节点（可交互 DOM 元素）序列化后超过该阈值，按以下规则截断：

1. **按广度优先（BFS）遍历**，优先保留可视区域（`getBoundingClientRect` 在视口内）的交互节点。
2. 若可视区域节点仍超限，再按**交互优先级**丢弃：`button/input/select` > `a[href]` > `div[role]` > 其他。
3. 截断动作记录到 Prometheus 指标 `uws_wasm_truncation_total{domain="..."}`。

### 4.2 降级补偿流程

截断发生后，WebTouch 引擎**并非直接丢弃低权重节点**，而是执行二级降级：

```
┌──────────────────────────────────────────────────────────────┐
│                    WASM 探针 (16MB Buffer)                   │
│  BFS遍历全量节点 → 填满Buffer → 剩余节点标记为"溢出队列"     │
└──────────────────────────────────────────────────────────────┘
                         ↓
              截断发生 → 记录指标
                         ↓
        ┌────────────────────────────────────┐
        │  降级慢速遍历（JavaScript Fallback）│
        │  - 仅对"溢出队列"中BFS排名前200节点  │
        │  - 超时保护 200ms                   │
        │  - 使用CDP Runtime.evaluate逐批提取 │
        └────────────────────────────────────┘
                         ↓
        ┌────────────────────────────────────┐
        │ 合并结果 → 完整候选节点池           │
        │ （WASM全量 + 降级补充）             │
        └────────────────────────────────────┘
```

**降级逻辑核心参数**：

| 参数 | 取值 | 说明 |
|------|------|------|
| 降级超时 | 200ms | 超过则放弃降级，仅使用 WASM 截断结果 |
| 降级节点数上限 | 200 | 仅处理溢出队列中优先级最高的 200 个节点 |
| 降级触发阈值 | Buffer 使用率 > 95% | 预留 5% 余量防止边界溢出 |
| 域名暂存名单 | 截断发生即记录 | 该域名进入“高复杂度名单”，离线蒸馏管道针对其分配更多预算（LLM 批量蒸馏权重提升） |

### 4.3 日志与可观测性补充

在 `online_logs` 表中新增字段（无需修改表结构，可存入 `decision_trace` JSON 的扩展字段）：

```json
{
  // ... 原有字段 ...
  "wasm_truncated": true,
  "wasm_node_count": 1530,
  "wasm_buffer_util": 0.96,
  "fallback_node_count": 87,
  "fallback_elapsed_ms": 45
}
```

Prometheus 新增指标：
- `uws_wasm_truncation_total{domain}`：Counter，按域名累计截断次数。
- `uws_wasm_fallback_success_rate`：Gauge，降级慢速遍历成功补充节点的比例。
- `uws_wasm_buffer_utilization`：Histogram，Buffer 使用率分布，用于动态调整 16MB 是否合理。

### 4.4 长尾优化（离线补偿）

对于连续 3 次以上发生截断的域名（`uws_wasm_truncation_total{domain} >= 3`），自动标记该域名的页面为 **高复杂度**：
- 离线 CC 蒸馏管道针对该域名增加采样权重（从默认 1.0 提升至 3.0）。
- 组件库基因注入时，针对该站点的特殊框架（如超大表格、无限滚动）生成额外的基线规则。
- 影子评估 L2 毕业条件中，对该域名的规则降低 `matched_count` 要求（从 20 降至 10），加速其自进化。

---

## 5. 对 Platform Boundary Manual 的回写说明

以下内容应并入 `platform_bound.md` Part A 和 Part B 的对应章节（本补丁发布后，开发团队应将它们直接合并到主手册中）：

| 合并位置 | 合并内容 |
|----------|----------|
| Part A §A.1 | 新增 `InitializeContext` RPC 定义（见 2.1） |
| Part A §A.2 错误码映射表 | 新增 `UNAUTHENTICATED` 行 + `CHALLENGE_INTERCEPTED` 状态枚举（见 2.2） |
| Part B §B.2 Target池 | 补充：若协调器未调用 `InitializeContext` 直接调用 `ExecuteAction`，引擎必须返回 `FAILED_PRECONDITION` 并拒绝执行 |
| Part E §E.2 指标 | 新增 `uws_wasm_truncation_total` 和 `uws_wasm_fallback_success_rate` |

---

**文档结束。下一份待补文档为：文档三（知识库版本化与回滚SOP）**。请指示继续输出，或先对当前文档提审核意见。