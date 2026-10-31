# WebTouch gRPC API 参考文档

本文档定义 WebTouch (Universal Web Surrogate) 引擎的 gRPC 接口契约，单一事实源文件为 [uws_service.proto](../proto/uws_service.proto)。

- **Package**: `uws.v1`
- **协议版本**: v1.5.7（追加-only：v1.5.0 追加 HealthStatus 回滚标记；v1.5.1 追加
  CaptureScreenshot RPC + ExecuteAction drag 动作值；v1.5.2 追加 chat_stream_done；
  v1.5.3 追加 InitContextRequest.context_name + CloseContext RPC；v1.5.6 追加 ExportState；
  v1.5.7 追加 State Bundle 导入与指纹档案初始化字段）
- **Service**: `UwsEngine`
- **默认端口**: `50051`

---

## 1. RPC 方法定义

### 1.1 InitializeContext

初始化浏览器沙盒上下文（BrowserContext）。每个外部任务必须在调用 `ExecuteAction` 之前调用本接口分配独立上下文，物理阻断 Cookie / 存储交叉污染。

**方法签名**：
```protobuf
rpc InitializeContext(InitContextRequest) returns (InitContextResponse);
```

**请求字段 (`InitContextRequest`)**：

| 字段名 | 类型 | 必填 | 描述 |
|---|---|---|---|
| `task_id` | `string` | 是 | 协调器任务 ID（UUIDv7），关联全链路追踪日志 |
| `proxy` | `ProxyConfig` | 否 | 代理配置（`host`, `port`, `username`, `password`），缺省使用默认出口 |
| `viewport` | `ViewportConfig` | 否 | 视口配置（`width` 默认 1920, `height` 默认 1080, `device_scale_factor` 默认 1.0） |
| `cookies` | `repeated Cookie` | 否 | 预置 Cookie 列表（`name`, `value`, `domain`, `path`, `http_only`, `secure`, `expires`） |
| `timezone` | `string` | 否 | 时区（如 `"Asia/Shanghai"`） |
| `launch_overrides` | `BrowserLaunchOverrides` | 否 | 启动覆盖（`max_old_space_size_mb` 默认 4096, `user_agent`, `extra_args`） |
| `initial_url` | `optional string` | 否 | 初始导航 URL（Playwright 适配层使用） |
| `attach_cdp_url` | `optional string` | 否 | 调试端口 WebSocket URL，用于直接接管用户本地运行的 Chrome 实例 |
| `context_name` | `optional string` | 否 | 命名上下文（= 一个浏览器标签页，v1.5.3）。同一客户端可持有多个命名上下文（多会话/多 Tab 并行）；`ExecuteAction` 经 `context_hint["context"]=<context_name>` 路由。缺省 = 默认上下文（保留「每客户端单上下文、重复初始化替换」旧语义） |
| `profile_id` | `optional string` | 否 | 持久身份名；若同时给 `fingerprint_profile_json`，二者必须一致 |
| `fingerprint_profile_json` | `optional string` | 否 | 严格 FingerprintProfile JSON；引擎整体应用为启动/CDP 覆盖 |

**响应字段 (`InitContextResponse`)**：

| 字段名 | 类型 | 描述 |
|---|---|---|
| `target_id` | `string` | WebTouch 分配的 CDP Target ID |
| `context_id` | `string` | BrowserContext 唯一标识 |
| `session_url` | `string` | 调试用 DevTools 远程调试 URL |
| `ok` | `bool` | 是否初始化成功 |
| `error_reason` | `string` | 失败时的详细错误原因 |

**grpcurl 示例**：
```bash
grpcurl -plaintext -d '{
  "task_id": "018f8e70-0000-7000-8000-000000000001",
  "viewport": {"width": 1920, "height": 1080, "device_scale_factor": 1.0},
  "timezone": "Asia/Shanghai",
  "initial_url": "https://example.com/login"
}' localhost:50051 uws.v1.UwsEngine/InitializeContext
```

---

### 1.2 ExecuteAction

单步原子动作执行接口（推荐无状态协调器使用）。支持 60s TTL 幂等性缓存（相同 `intent_id` 重复提交直接返回缓存结果）。

**方法签名**：
```protobuf
rpc ExecuteAction(ActionIntentRequest) returns (ActionResultResponse);
```

**请求字段 (`ActionIntentRequest`)**：

| 字段名 | 类型 | 必填 | 描述 |
|---|---|---|---|
| `intent_id` | `string` | 是 | 唯一意图标识（UUIDv7），用于幂等去重 |
| `target_semantic` | `string` | 是 | 目标语义标签（如 `"username_input"`, `"submit_btn"`） |
| `expected_action` | `string` | 是 | 动作类型：`click` \| `input` \| `select` \| `focus` \| `hover` \| `scroll` \| `wait` \| `extract` \| `drag`（v1.5.1 追加：VLM 打码原生拖拽，坐标直发、bypass 语义匹配） |
| `value` | `optional string` | 否 | 动作填充值（`input` 输入文本 / `select` 选项值 / `drag` 时 `"x1,y1,x2,y2"` 起点→终点坐标） |
| `context_hint` | `map<string, string>` | 否 | 上下文辅助提示键值对 |
| `timeout_ms` | `int32` | 否 | 动作执行 + 局部验证总超时（ms），默认 `1500` |

> **上下文路由（v1.5.3）**：`context_hint["context"]=<InitContextRequest.context_name>`
> 选择动作落到哪个标签页（多会话/多 Tab 并行）；缺省走默认上下文（旧语义零变化）。
> 幂等缓存按「客户端 + 上下文名 + intent_id」隔离，跨 Tab 同 intent 不串缓存。
> 标签页被关闭（用户手动关 Tab / `Target.targetDestroyed`）时物理层显式返回
> `target_destroyed`，API 层可据此自动重建新 Tab 并重试。

**响应字段 (`ActionResultResponse`)**：

| 字段名 | 类型 | 描述 |
|---|---|---|
| `intent_id` | `string` | 对应的意图 ID |
| `success` | `bool` | 动作是否成功定位并派发执行 |
| `confidence` | `float` | 规则 / 模型定位置信度得分 ($0.0 \sim 1.0$) |
| `local_verification` | `LocalVerification` | 局部探针验证结果（见下文子表） |
| `decision_trace_id` | `string` | 决策日志链路 ID |
| `error_reason` | `string` | 错误详细原因 |
| `context_state` | `ContextState` | 动作执行后的页面上下文状态枚举 |
| `extracted_data` | `string` | 当 `expected_action="extract"` 时返回的提取数据（JSON字符串或文本） |
| `chat_stream_done` | `bool` | 当 `expected_action="extract"` 且 `context_hint["prefer_sse"]="1"` 时：聊天 SSE 流是否已结束（FINISHED / loadingFinished；未捕获或生成中 = false）。API 层用它判定完成，避免生成停顿 >1.5s 把半截回答当完整返回（v1.5.2 追加） |
| `hit_element` | `HitElement` | ★v1.5.4：实际命中元素证据回显（selector/rect/text）——判定层证据链第一切片，调用方据此区分「点中精确目标」vs「点中容器/推荐位」（假阳性），详见 `docs/evidence/research/uws-verdict-evidence-design-2026-08-27.md` §6.1 |
| `page_url` | `string` | ★v1.5.4：动作完成后当前页面 URL（确定性证据；会话不支持时为空） |
| `url_changed` | `bool` | ★v1.5.4：动作前后 URL 是否变化（点击导航证据；非导航点击 / 会话不支持时 = false） |

> **方案 1（2026-08-25）**：聊天类 `extract`（如 `chat_answer`）可在
> `context_hint["prefer_sse"]="1"` 时优先返回 CDP Network 层捕获的 SSE 干净
> Markdown（`browser.ChatStreamSource` 快照）。Epoch>0 时按捕获状态分派：
> 快照有内容 → 干净 SSE 文本 + `chat_stream_done`；捕获轮失败（loadingFailed/
> 中止/拉取失败）或已空响应收尾 → 回退 DOM observed；捕获进行中但内容为空
> （如深度思考阶段）→ 显式空串继续等 SSE，不用 DOM 思考链/旧回答冒充新回答
> （多轮基线由 API 层处理）。未捕获/未开 hint 时保持 DOM observed 回退。
> **2026-08-26 修复**：失败/空响应捕获轮不再永久霸占 extract（此前会导致
> 「页面已答但 API 轮询到超时」）；API 预算耗尽时另有最后一次不带 prefer_sse
> 的 DOM extract 兜底。
>
> **协议兼容（2026-08-25 深夜续）**：DeepSeek Web 端 SSE 已升级为补丁式协议——
> 初始快照 `{"v":{"response":{"fragments":[...]}}}` + 首个显式补丁
> `{"p":"response/fragments/-1/content","o":"APPEND","v":"..."}` 确立路径 +
> 后续隐式增量 `{"v":"..."}`（省略 p/o）+ 收尾 `FINISHED`/BATCH `quasi_status`；
> 解析器同时兼容旧 `text_delta`/`choices[].delta` 形态。`extracted_data` 为纯净
> Markdown，无 DOM 引用角标（`-1-4-7` 类）污染。

*`LocalVerification` 字段表*：
- `observed_changes` (`repeated string`): 观测到的 DOM 属性变化（如 `["aria-expanded:false->true"]`）
- `value_updated` (`bool`): 输入值是否已反映在 DOM 节点中
- `event_triggered` (`bool`): 目标事件是否成功触发
- `probe_status` (`string`): 探针状态（`mutated` \| `timeout` \| `interrupted_by_navigation` \| `soft_navigated`）
- `elapsed_ms` (`int32`): 局部验证耗时（ms）

*`HitElement` 字段表（v1.5.4）*：
- `selector` (`string`): 命中元素的 CSS 路径（如 `div#chapter-grid > li:nth-of-type(4)`；以点击点 `elementFromPoint` 真身为准）
- `tag` (`string`): 元素标签（小写）
- `x` / `y` (`float`): 视口命中点（CSS px）
- `width` / `height` (`float`): 元素尺寸（CSS px）
- `text` (`string`): 可见文本（截断 ≤120）

> **证据链用法（v1.5.4）**：`success=true + event_triggered=true` 只代表「动作派发
> 且事件触发」，不代表「目标达成」。drama-h5「第 4 集」/ skincare「第 1 个搜索结果」
> 实测：点中容器也报成功。调用方请用 `hit_element`（命中真身）+ `url_changed`
> （导航证据）做判定：`assertion` 模式下命中非目标或 URL 未变即判失败冻结；
> `task` 模式下可作为重试/换策略的依据。

**grpcurl 示例**：
```bash
grpcurl -plaintext -d '{
  "intent_id": "018f8e70-0000-7000-8000-000000000002",
  "target_semantic": "login_submit_button",
  "expected_action": "click",
  "timeout_ms": 2000
}' localhost:50051 uws.v1.UwsEngine/ExecuteAction
```

---

### 1.3 ExecuteActionStream

双向流式动作执行接口（推荐有状态规划器 / AGI Planner 使用）。在单个长连接会话内连续派发动作与接收即时结果。

**方法签名**：
```protobuf
rpc ExecuteActionStream(stream ActionIntentRequest) returns (stream ActionResultResponse);
```

**请求与响应**：
- 请求流项：[`ActionIntentRequest`](#12-executeaction)
- 响应流项：[`ActionResultResponse`](#12-executeaction)

**grpcurl 示例**：
```bash
grpcurl -plaintext -d @ localhost:50051 uws.v1.UwsEngine/ExecuteActionStream <<EOF
{"intent_id": "018f8e70-0000-7000-8000-000000000010", "target_semantic": "search_input", "expected_action": "input", "value": "WebTouch Engine"}
{"intent_id": "018f8e70-0000-7000-8000-000000000011", "target_semantic": "search_button", "expected_action": "click"}
EOF
```

---

### 1.4 ReloadKnowledge

知识库零停机原子热更新接口。通过双缓冲机制加载新版 L1/L2 规则库与 ONNX 模型。

**方法签名**：
```protobuf
rpc ReloadKnowledge(ReloadRequest) returns (ReloadResponse);
```

**请求字段 (`ReloadRequest`)**：

| 字段名 | 类型 | 必填 | 描述 |
|---|---|---|---|
| `version` | `string` | 是 | 目标版本号（如 `"v1.4.0"`） |
| `artifact_uri` | `string` | 否 | 产物存储 URI，缺省使用 `s3://uws-artifacts/{version}` |
| `dry_run` | `bool` | 否 | `true` = 仅下载与校验签名，不实际切换（预检） |
| `force` | `bool` | 否 | `true` = 跳过兼容性校验强制生效（用于紧急回滚） |

**响应字段 (`ReloadResponse`)**：

| 字段名 | 类型 | 描述 |
|---|---|---|
| `ok` | `bool` | 热更新是否成功 |
| `active_version` | `string` | 当前生效的知识库版本 |
| `previous_version` | `string` | 切换前版本号 |
| `switch_time_ms` | `int64` | 内存双缓冲切换耗时（ms，不含文件下载时间） |
| `reason` | `string` | 失败原因说明 |

**grpcurl 示例**：
```bash
grpcurl -plaintext -d '{
  "version": "v1.4.0",
  "dry_run": false,
  "force": false
}' localhost:50051 uws.v1.UwsEngine/ReloadKnowledge
```

---

### 1.5 HealthCheck

物理层与内核层双重健康检查接口。检查 CDP 连接池、Target 活跃度及知识库就绪状态。

**方法签名**：
```protobuf
rpc HealthCheck(Empty) returns (HealthStatus);
```

**请求字段 (`Empty`)**：无字段 `{}`。

**响应字段 (`HealthStatus`)**：

| 字段名 | 类型 | 描述 |
|---|---|---|
| `ok` | `bool` | 引擎整体健康状态 |
| `version` | `string` | 当前加载的知识库版本 |
| `active_targets` | `int32` | 当前活跃的 CDP Target 数量 |
| `max_targets` | `int32` | 引擎 Target 容量上限 |
| `rolled_back` | `bool`（optional，v1.5.0） | 引擎最近一次发生过 P0 自动回滚（未被人为切换覆盖） |
| `rollback_reason` | `string`（optional，v1.5.0） | 自动回滚说明，如 `"auto_rollback v1.4.0 → v1.3.9"` |

**grpcurl 示例**：
```bash
grpcurl -plaintext -d '{}' localhost:50051 uws.v1.UwsEngine/HealthCheck
```

---

### 1.6 GetKnowledgeVersion

查询在线引擎当前加载的知识库版本及静态哈希值，供协调器或发布系统对齐校验。

**方法签名**：
```protobuf
rpc GetKnowledgeVersion(Empty) returns (KnowledgeVersionResponse);
```

**请求字段 (`Empty`)**：无字段 `{}`。

**响应字段 (`KnowledgeVersionResponse`)**：

| 字段名 | 类型 | 描述 |
|---|---|---|
| `version` | `string` | 知识库版本标识（如 `"v1.4.0"`） |
| `schema_hash` | `string` | 规则库 SQLite Schema 的 SHA-256 哈希 |
| `model_hash` | `string` | 分类器 ONNX 模型的 SHA-256 哈希 |

**grpcurl 示例**：
```bash
grpcurl -plaintext -d '{}' localhost:50051 uws.v1.UwsEngine/GetKnowledgeVersion
```

---

### 1.7 CaptureScreenshot（v1.5.1 追加）

捕获当前上下文页面截图（PNG 字节），VLM 视觉打码求解输入（TDD-COORD-04）。
生命周期与 `ExecuteAction` 一致：必须先 `InitializeContext`；`target_id` 必须匹配当前上下文。

**方法签名**：
```protobuf
rpc CaptureScreenshot(CaptureScreenshotRequest) returns (CaptureScreenshotResponse);
```

**请求字段 (`CaptureScreenshotRequest`)**：

| 字段名 | 类型 | 必填 | 描述 |
|---|---|---|---|
| `target_id` | `string` | 是 | `InitializeContext` 返回的 Target ID |

**响应字段 (`CaptureScreenshotResponse`)**：

| 字段名 | 类型 | 描述 |
|---|---|---|
| `png` | `bytes` | PNG 编码截图 |
| `width` | `int32` | 视口宽度（CSS px） |
| `height` | `int32` | 视口高度（CSS px） |
| `error_reason` | `string` | 失败原因（可空；失败时以 gRPC 错误码为准） |

**grpcurl 示例**：
```bash
grpcurl -plaintext -d '{"target_id": "cdp-1"}' localhost:50051 uws.v1.UwsEngine/CaptureScreenshot
```

---

### 1.8 CloseContext（v1.5.3 追加）

关闭命名上下文（释放 Target，对应浏览器标签页随之关闭）。API 层 `DELETE /v1/sessions/{id}`
消费；幂等（上下文不存在也返回 `ok`，标签已被手动关闭/引擎重启不算错误）。

**方法签名**：
```protobuf
rpc CloseContext(CloseContextRequest) returns (CloseContextResponse);
```

**请求字段 (`CloseContextRequest`)**：

| 字段名 | 类型 | 必填 | 描述 |
|---|---|---|---|
| `context_name` | `string` | 否 | 与 `InitContextRequest.context_name` 对应；空 = 默认上下文 |

**响应字段 (`CloseContextResponse`)**：

| 字段名 | 类型 | 描述 |
|---|---|---|
| `ok` | `bool` | 是否已释放 |
| `error_reason` | `string` | 失败原因（可空） |

**grpcurl 示例**：
```bash
grpcurl -plaintext -d '{"context_name": "session-42"}' localhost:50051 uws.v1.UwsEngine/CloseContext
```

---

### 1.9 ImportState（v1.5.7 追加）

在**首次导航前**恢复可信客户端解封后的 State Bundle。引擎立即调用 CDP 写入 Cookie，
并把 LocalStorage 与当前 tab 的 SessionStorage 注册为对应 origin 的新文档脚本；未初始化、目标不匹配、上下文未声明
`profile_id`、Bundle 身份不一致、首次导航已经发生、模拟会话或不支持的会话均返回 `FAILED_PRECONDITION`，绝不伪报恢复成功。

```protobuf
rpc ImportState(ImportStateRequest) returns (ImportStateResponse);
```

| 字段名 | 类型 | 必填 | 描述 |
|---|---|---|---|
| `target_id` | `string` | 否 | `InitializeContext` 返回的 target；空 = 默认上下文 |
| `bundle.profile_id` | `string` | 是 | 与目标持久身份一致的合法身份名 |
| `bundle.created_at` | `string` | 是 | RFC3339 创建时刻 |
| `bundle.cookies_json` | `string` | 是 | `Network.getAllCookies` 的 JSON 数组 |
| `bundle.origins` | `repeated StorageOriginContext` | 否 | 各 origin 的 LocalStorage 与当前 tab 的 SessionStorage 键值对 |

AES-256-GCM 密钥和密文**不通过此 RPC**；它们应留在客户端密钥存储，解封后的结构化数据才可提交。

---

### 1.10 ExportPageTree（v1.5.5 追加）

导出当前上下文的**极简 AXTree 文本**（含 `node_id`），供协调器外发 VLM 指挥官做混合指挥时
用确定性 `node_id` 引用页面节点。与 `ExecuteAction` 同期生命周期：必须先 `InitializeContext`。

**方法签名**：
```protobuf
rpc ExportPageTree(ExportPageTreeRequest) returns (ExportPageTreeResponse);
```

**请求字段 (`ExportPageTreeRequest`)**：

| 字段名 | 类型 | 必填 | 描述 |
|---|---|---|---|
| `target_id` | `string` | 否 | `InitializeContext` 返回的 Target ID；空 = 默认上下文 |

**响应字段 (`ExportPageTreeResponse`)**：

| 字段名 | 类型 | 描述 |
|---|---|---|
| `tree` | `string` | 极简 AXTree 文本（多行，含 `node_id`） |

**错误语义**：`target_id` 非空但本客户端无该上下文 → `NOT_FOUND`；从未初始化 → `FAILED_PRECONDITION`；
会话不支持页面树导出（如模拟会话）→ `UNIMPLEMENTED`。

**grpcurl 示例**：
```bash
grpcurl -plaintext -d '{"target_id": "cdp-1"}' localhost:50051 uws.v1.UwsEngine/ExportPageTree
```

---

### 1.11 ExportState（v1.5.6 追加）

导出当前上下文的浏览器状态（Cookie + 指定 origin 的 LocalStorage 与当前 tab 的 SessionStorage），
供可信客户端封装为 State Bundle 做跨端同步。**明文导出**：引擎只负责取，不加密、不落盘。

**方法签名**：
```protobuf
rpc ExportState(ExportStateRequest) returns (ExportStateResponse);
```

**请求字段 (`ExportStateRequest`)**：

| 字段名 | 类型 | 必填 | 描述 |
|---|---|---|---|
| `target_id` | `string` | 否 | `InitializeContext` 返回的 Target ID；空 = 默认上下文 |
| `target_origin` | `string` | 否 | 形如 `https://chatgpt.com`；指定后提取该 origin 的 LocalStorage / 当前 tab SessionStorage |

**响应字段 (`ExportStateResponse`)**：

| 字段名 | 类型 | 描述 |
|---|---|---|
| `cookies_json` | `string` | `Network.getAllCookies` 原生列表的 JSON 序列化 |
| `origins` | `repeated StorageOriginContext` | 每个 origin 的 `local_storage` / `session_storage` 键值对 |

**错误语义**：`target_id` 非空但未初始化或已销毁 → `NOT_FOUND`；从未初始化、CDP 未就绪 → `FAILED_PRECONDITION`；
导出过程失败 → `INTERNAL`。

**grpcurl 示例**：
```bash
grpcurl -plaintext -d '{"target_id": "cdp-1", "target_origin": "https://chatgpt.com"}' \
  localhost:50051 uws.v1.UwsEngine/ExportState
```

---

## 2. ContextState 枚举

`ContextState` 用于在动作执行后向协调器通告页面物理生命周期状态（从 0 开始连续编号）：

| 枚举名 | 编号 | 语义说明 | 协调器建议行为 |
|---|---|---|---|
| `STABLE` | 0 | 页面处于静息状态，DOM 与网络均稳定 | 立即执行下一规划动作 |
| `NAVIGATING` | 1 | SPA 软路由或轻量异步跳转触发中 | 等待 Network Idle 及 DOM Mutation 静息 |
| `NAVIGATED` | 2 | 页面导航完成（硬导航或路由稳定） | 重新评估当前页面语义骨架 |
| `NEW_WINDOW_OPENED` | 3 | 触发打开新标签页或弹窗 | 发起 CDP Attach 切换至新 Target |
| `CHALLENGE_INTERCEPTED` | 4 | 触发 Cloudflare/WAF/验证码人机拦截 | 挂起任务并交割外部破解服务或人工接管 |

---

## 3. gRPC 错误码映射表

WebTouch 引擎与 gRPC 标准 Canonical Code 的映射规则及协调器对策如下：

| gRPC 状态码 | 数值 | WebTouch 触发场景 | 协调器对策 |
|---|---|---|---|
| `OK` | 0 | 动作执行与验证通过 | 继续执行后续流程 |
| `INVALID_ARGUMENT` | 3 | 请求参数非法（字段缺失、XSS 注入或长度超限） | 修复输入参数，不可盲目重试 |
| `DEADLINE_EXCEEDED` | 4 | WASM 探针感知或局部验证超时（> `timeout_ms`） | 采用 Fibonacci 退避重试 1 次 |
| `NOT_FOUND` | 5 | 无可信候选节点（置信度 $C < \theta_{low}$） | 不可重试，触发 LLM 规划兜底或人工介入 |
| `RESOURCE_EXHAUSTED` | 8 | LLM 配额耗尽或引擎 Target 池已满 | 降级阻塞，拒绝低优先级操作 |
| `FAILED_PRECONDITION` | 9 | CDP Target 崩溃、会话失效或 `intent_id` 冲突 | 重新调用 `InitializeContext` 重建沙盒或丢弃流 |
| `ABORTED` | 10 | 动作派发时目标节点已脱离 DOM 或被动画遮罩遮挡 | 立即重试（等待下一 requestAnimationFrame） |
| `UNAUTHENTICATED` | 16 | 命中 `CHALLENGE_INTERCEPTED` 且无法穿透 | 调用外部解密/打码平台；失败则升级人工接管 |
