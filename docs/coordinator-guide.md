# WebTouch 协调器使用指南 (Coordinator Guide)

> ⚠️ 历史定位：本指南称协调器为「任务编排大脑」。按 2026-09-02 分层定稿
> （`docs/evidence/archive/web-touch-architecture-2026-09-02.md` 与 `docs/README.md`），编排调度属外层应用，
> 协调器是 WebTouch 的调用方；指南内的 gRPC/模板/DAG 细节仍有效，仅角色描述按定稿理解。

WebTouch 协调器（Coordinator）是系统的任务编排大脑。它接收高层级业务目标，将其转化为 DAG 动作流，通过 gRPC 协议驱动 WebTouch 引擎执行，并在步骤异常时自主触发重试、反思或重规划。

详细设计原理参见 [spec/coordinator.md](../spec/coordinator.md) 与接口定义 [proto/uws_service.proto](../proto/uws_service.proto)。

---

## 1. 概述与核心工作流

协调器遵循**确定性优先**原则：
- **L1/L2 模板命中**：优先匹配 YAML 预定义模板，CPU 单核纳秒级插值，单步延迟 < 50ms。
- **L3/L4 动态规划**：无模板或执行断裂时，调用 LLM 补全/生成 DAG，Token 消耗降低 70% 以上。

```
[任务目标/参数] ──▶ [模板引擎/Planner] ──▶ 生成 DAG (ActionNode 队列)
                                                  │
 ┌────────────────────────────────────────────────┘
 ▼
[gRPC: InitializeContext] ──▶ 创建独立沙盒 Target
 ▼
[执行循环: ExecuteAction] ──▶ 检查 ActionResultResponse & ContextState
 │   ├── 成功 (STABLE)       ──▶ 拓扑流转至下一动作
 │   ├── 软路由 (NAVIGATING)  ──▶ 等待网络与 DOM 静息 (自动等待 NAVIGATED)
 │   └── 异常/失败            ──▶ 触发 Reflector (重试/反思/回退/重规划)
```

---

## 2. 任务模板 YAML 编写规范

模板统一以 YAML 格式组织，支持变量插值、依赖拓扑、条件分支、循环与错误控制策略。

### 2.1 核心字段规范

| 字段 | 类型 | 说明 |
|------|------|------|
| `template_id` | string | 模板唯一标识，如 `login_form_v1` |
| `parameters` | list | 任务入参声明列表 |
| `steps` | list | 动作节点序列（DAG 节点） |
| `steps[].step_id` | string | 步骤唯一 ID（拓扑节点名） |
| `steps[].action_type` | enum | `click` \| `input` \| `select` \| `focus` \| `hover` \| `scroll` \| `wait` \| `extract` |
| `steps[].target_semantic`| string | 语义标签（如 `username_input`、`submit_btn`） |
| `steps[].value` | string | 输入值或等待条件，支持 `{{ var }}` 插值 |
| `steps[].depend_on` | list | 前置依赖步骤 ID 列表 |
| `steps[].condition` | string | 分支执行表达式（如 `{{ has_captcha }} == true`） |
| `steps[].loop` | object | 循环配置 `{ items: "{{ list }}", max_iterations: 10 }` |
| `steps[].on_failure` | enum | 失败策略：`abort`（终止）\| `retry`（局部重试）\| `skip`（跳过）\| `replan`（触发 LLM 重规划） |
| `steps[].max_retries` | int | on_failure=retry 的局部语义重试预算（默认 2；仅在 DEADLINE_EXCEEDED 路径消费，受 Fibonacci 1s/2s 最多 2 次钳制，可调低；ABORTED/REINIT 为物理层/崩溃恢复固定上限，不受本字段影响） |

---

## 3. 完整模板示例

### 示例 1：标准线性登录流程

```yaml
template_id: "auth_password_login_v1"
name: "标准用户名密码登录"
parameters:
  - username
  - password
  - login_url

steps:
  - step_id: "nav_login"
    action_type: "wait"
    target_semantic: "NAVIGATED"
    value: "{{ login_url }}"
    depend_on: []

  - step_id: "input_user"
    action_type: "input"
    target_semantic: "username_input"
    value: "{{ username }}"
    depend_on: ["nav_login"]

  - step_id: "input_pwd"
    action_type: "input"
    target_semantic: "password_input"
    value: "{{ password }}"
    depend_on: ["input_user"]

  - step_id: "click_submit"
    action_type: "click"
    target_semantic: "login_submit_button"
    depend_on: ["input_pwd"]
    on_failure: "replan"

  - step_id: "wait_dashboard"
    action_type: "wait"
    target_semantic: "NAVIGATED"
    value: "5000"
    depend_on: ["click_submit"]
```

### 示例 2：含验证码条件分支与人工接管

```yaml
template_id: "login_with_conditional_captcha_v1"
name: "带验证码旁路分支的登录"
parameters:
  - username
  - password

steps:
  - step_id: "fill_credentials"
    action_type: "input"
    target_semantic: "username_input"
    value: "{{ username }}"
    depend_on: []

  - step_id: "submit"
    action_type: "click"
    target_semantic: "submit_btn"
    depend_on: ["fill_credentials"]

  # 条件分支：检测是否触发验证码挑战
  - step_id: "handle_captcha"
    condition: "{{ context_state }} == 'CHALLENGE_INTERCEPTED'"
    action_type: "wait"
    target_semantic: "captcha_resolved_signal"
    value: "30000" # 等待破解服务或人工介入
    depend_on: ["submit"]
    on_failure: "abort"

  - step_id: "verify_success"
    action_type: "wait"
    target_semantic: "NAVIGATED"
    value: "5000"
    depend_on: ["submit", "handle_captcha"]
```

### 示例 3：循环翻页与列表数据抽取

```yaml
template_id: "ecommerce_extract_products_v1"
name: "商品列表分页提取"
parameters:
  - max_pages

steps:
  - step_id: "extract_page_items"
    action_type: "extract"
    target_semantic: "product_card_list"
    depend_on: []
    loop:
      max_iterations: "{{ max_pages }}"
      exit_condition: "{{ next_page_disabled }} == true"

  - step_id: "click_next_page"
    action_type: "click"
    target_semantic: "pagination_next_button"
    depend_on: ["extract_page_items"]
    on_failure: "skip" # 末页无法点击时平滑退出
```

---

## 4. 错误处理与状态流转策略

协调器 Reflector 模块根据 WebTouch 返回的 gRPC 状态码与 `ContextState` 自动决策：

### 4.1 gRPC 错误码处理矩阵

| gRPC 状态码 | WebTouch 根因 | 协调器对策 |
|-------------|----------|------------|
| `OK (0)` | 动作成功并验证通过 | 推进状态机，执行下一就绪动作 |
| `NOT_FOUND (5)` | 无可信节点（置信度 $C < \theta_{low}$） | 禁止盲目重试；触发 LLM 页面重新定位或回退 |
| `DEADLINE_EXCEEDED (4)` | 探针/局部验证超时 | Fibonacci 退避重试（1s, 2s），最多 2 次 |
| `ABORTED (10)` | 节点脱离 DOM 或处于过渡动画遮挡 | 等待 200ms 后立即重试（最多 3 次） |
| `FAILED_PRECONDITION (9)` | Target 崩溃或 `intent_id` 冲突 | 重新初始化沙盒上下文并重放前置稳定状态 |
| `RESOURCE_EXHAUSTED (8)` | Target 池满或 LLM 配额超限 | 降级为等待排队或切入纯规则备用分支 |
| `UNAUTHENTICATED (16)` | `CHALLENGE_INTERCEPTED` 拦截 | 挂起任务，调用第三方破解服务或通知人工 |
| `INVALID_ARGUMENT (3)` | 参数校验失败（XSS/非法输入） | 终止当前分支，抛出格式化参数异常 |

### 4.2 ContextState 状态流转机制

- **`STABLE (0)`**：页面静止，安全执行下一原子动作。
- **`NAVIGATING (1)`**：SPA 路由触发中，协调器暂停派发，等待引擎上报网络与 DOM 静息。
- **`NAVIGATED (2)`**：页面完成硬/软跳转，递增 `navigation_counter` 并刷新页面快照。
- **`NEW_WINDOW_OPENED (3)`**：检测到 `target="_blank"` 新增标签页，协调器自动切换绑定新 `target_id`。
- **`CHALLENGE_INTERCEPTED (4)`**：触发 WAF/滑块验证，暂停流水线等待旁路解锁。

### 4.3 VLM 打码求解配置（CHALLENGE_INTERCEPTED 自动解锁）

收到 `CHALLENGE_INTERCEPTED` 后，协调器执行流：`CaptureScreenshot`（真实 PNG）→
VLM/打码服务计算坐标 → `ExecuteAction drag`（`challenge_slider`，坐标直发）→
解锁后重试原步骤无缝继续；单任务求解 `vlm.max_solves` 次封顶（默认 3）。

求解器由 `~/.uws/config.yaml` 的 `vlm` 段构造（`uws config set vlm_*` 写入，
见 cli-and-config §2.2）：

- 未配置 → 无求解器，保持既有 PAUSED 降级语义（行为兼容）；
- `provider=mock` → 固定坐标确定性求解（测试/本地演示）；
- `provider=http` + `endpoint` → `HttpVlmSolver`（纯 stdlib POST，Bearer 鉴权，
  响应 `{x1,y1,x2,y2,action}`）；生产可替换为 DeepSeek-VL 等打码服务。

---

## 5. 崩溃恢复机制 (Crash Recovery)

当浏览器 Target 发生异常奔溃（OOM 或进程退出）时，协调器通过**事务性幂等快照**实施零丢失恢复：

```
[Target 崩溃] ──▶ 检查历史动作记录
                     │
                     ├── 无 NAVIGATED 步骤 ──▶ 重新 InitializeContext 完整重跑
                     │
                     └── 存在稳定快照 ───────▶ 1. 重新 InitializeContext
                                              2. 导航至最近一次成功 NAVIGATED URL
                                              3. 幂等重放无副作用步骤 (表单填写/选项切换)
                                              4. 跳过不可逆动作 (支付/确认)，继续执行中断点
```

> **注意**：具有外部副作用的操作（如点击支付、发帖）会被标记为 `non_idempotent`，崩溃恢复时绝不自动重放，转为安全挂起。

---

## 6. 常见问题 (FAQ)

### Q1: 模板如何与 LLM 规划混合使用？
A: 协调器支持**模板片段插槽**。在模板步骤中若某步 `on_failure: "replan"`，协调器会将当前 DOM 语义切片与未完成目标打包交由 LLM 生成子计划，动态合入当前执行流。

### Q2: 如何保证网络抖动下的请求幂等性？
A: 每个动作请求必须携带 UUIDv7 的 `intent_id`。WebTouch 引擎在 60s TTL 内自动缓存并幂等返回同一结果，避免重复点击产生重复提交。

### Q3: 为什么单步执行前必须先调用 `InitializeContext`？
A: `InitializeContext` 会在 Chromium 内部创建独立的隔离沙盒（`BrowserContext`），阻断 Cookie 与 Storage 污染，并为当前任务绑定视口与代理配置。

### Q4: 如何定位执行失败的详细原因？
A: 检查 `ActionResultResponse.decision_trace_id`，结合 WebTouch 决策日志可获取 AC-BM25 召回分值、ONNX 推理概率及探针 DOM 变更轨迹。
