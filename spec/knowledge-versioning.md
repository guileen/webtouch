# 文档三：知识库版本化与运维回滚标准操作程序（Knowledge Versioning & Rollback SOP）

**版本**：V1.0  
**状态**：冻结，可编码  
**依赖**：WebTouch内核V1.4（`ReloadKnowledge` RPC已定义），Platform Boundary Manual V1.0（Part A）  
**生效日期**：2026-08-19

---

## 1. 设计目标与原则

### 1.1 目标
确保 WebTouch 知识库（L1 SQLite + L2 SQLite + ONNX 模型）的**发布、热加载、灰度验证和紧急回滚**全过程：
- **零停机**：在线执行引擎不中断现有会话。
- **原子切换**：新知识库加载失败时，旧知识库保持有效，不出现半更新状态。
- **可审计**：每次切换记录版本哈希、操作人、时间戳。
- **可逆**：任何版本可在 5 秒内回滚至任意历史版本。

### 1.2 适用范围
- L1 通用语义知识库（`uws_rules` + `classifier.onnx`）
- L2 站点特化知识库（`site_synonyms` + `l2_rules`，按站点分表）
- 组件库基因基线规则（常驻 L1 白名单，版本随 L1 发布）

---

## 2. 对象存储布局（Artifact Layout）

所有知识库产物统一存储于对象存储（S3/OSS/GCS），路径规范如下：

```
s3://uws-artifacts/
├── latest/                                    # 软链接指向最新稳定版本
│   ├── manifest.json                          # 指向当前生产版本
│   └── (实际为symlink，由CI/CD更新)
├── v1.3.0/                                    # 版本目录（语义化版本）
│   ├── manifest.json                          # 该版本元数据
│   ├── uws_l1.db                              # L1 SQLite（压缩前）
│   ├── classifier.onnx                        # ONNX模型
│   ├── uws_l2_default.db                      # L2空模板（首次发布时种子）
│   ├── checksums.sha256                       # 各文件SHA256
│   └── changelog.md                           # 变更说明
├── v1.3.1/
│   └── ...
├── v1.4.0/
│   └── ...
└── rollback_logs/                              # 回滚操作审计日志（JSON Lines）
    └── 2026-08-19.jsonl
```

### 2.1 `manifest.json` 结构（关键契约）

```json
{
  "version": "v1.4.0",
  "release_date": "2026-08-19T10:00:00Z",
  "artifact_uri_base": "s3://uws-artifacts/v1.4.0",
  "files": {
    "uws_l1.db": {
      "hash": "sha256:7f83b1657ff1fc53b92dc18148a1d65dfc2d4b1f...",
      "size_bytes": 52428800
    },
    "classifier.onnx": {
      "hash": "sha256:9a4f2c8e...",
      "size_bytes": 1048576
    },
    "uws_l2_default.db": {
      "hash": "sha256:3b5d...",
      "size_bytes": 102400
    }
  },
  "compatibility": {
    "min_engine_version": "v1.4.0",        // WebTouch引擎最低版本要求
    "max_engine_version": "v1.4.x"         // 兼容的引擎版本范围
  },
  "rollback_from": "v1.3.9",               // 本版本修复了谁（便于追溯）
  "deploy_metadata": {
    "triggered_by": "ci/cc_pipeline",
    "cc_dataset_id": "2026-08-18_cc_snapshot",
    "shadow_eval_pass_rate": 0.96
  }
}
```

---

## 3. 热加载与原子切换机制（WebTouch 引擎侧）

### 3.1 双缓冲（Double Buffer）架构

WebTouch 引擎内部维护两个独立的**知识库读取句柄**：

| 句柄 | 状态 | 说明 |
|------|------|------|
| `active_handle` | **生产流量** | 当前所有 `ExecuteAction` 请求读取此句柄。 |
| `staging_handle` | **就绪/加载中** | 接收 `ReloadKnowledge` 新版本，加载过程中不影响 `active_handle`。 |

**切换流程（原子化）**：

```
1. 协调器调用 ReloadKnowledge(version="v1.4.0", artifact_uri="s3://...")
   ↓
2. WebTouch引擎从对象存储下载新文件至本地缓存目录（/tmp/uws_cache/v1.4.0/）
   ↓ (校验SHA256)
3. 在内存中加载 staging_handle（加载SQLite索引 + ONNX模型到内存）
   ↓
4. 执行自检（Sanity Check）：
   - 关键规则索引是否可查（如"登录"）
   - ONNX推理是否正常（跑一次空特征）
   - 若自检失败 → 返回 ReloadResponse{ok=false, reason="sanity_check_failed"}
   ↓
5. 原子指针切换（由读写锁保护）：
   - 获取全局写锁（阻塞新请求 < 1ms）
   - active_handle = staging_handle
   - 释放写锁
   ↓
6. 新请求立即使用新知识库。
   ↓
7. 旧 active_handle 保持存活（但仍可读），等待 TTL（60秒）后释放内存，期间若有已持有旧句柄的异步任务（如正在执行的验证器），仍可安全使用旧知识。
```

### 3.2 零停机热更的硬件约束

- **网络**：下载新版本 artifact 必须 < 10秒（若超时，回退至"异步后台加载"，且本次调用不切换）。
- **内存**：新旧两个知识库同时驻留的内存开销（SQLite内存映射 + ONNX模型）必须小于 300MB（设计上限）。若新版本加载时内存不足，引擎拒绝切换并返回 `RESOURCE_EXHAUSTED`。

### 3.3 `ReloadKnowledge` 接口契约（细化）

```protobuf
message ReloadRequest {
    string version = 1;                // 要加载的版本号，如 "v1.4.0"
    string artifact_uri = 2;           // 可选，若不填则使用 s3://uws-artifacts/{version}
    bool dry_run = 3;                  // 若 true，仅下载+校验，不执行切换（用于预检）
    bool force = 4;                    // 若 true，跳过兼容性检查（紧急场景）
}

message ReloadResponse {
    bool ok = 1;
    string active_version = 2;         // 当前生效的版本号
    string previous_version = 3;       // 切换前的版本号
    int64 switch_time_ms = 4;          // 实际切换耗时（不含下载）
    string reason = 5;                 // 失败时的具体原因
}
```

---

## 4. 灰度发布与金丝雀验证（Canary Analysis）

### 4.1 灰度策略（生产环境）

**禁止**将新版本直接全量推送给所有 WebTouch 实例。必须遵循以下分阶段流程：

| 阶段 | 实例比例 | 验证周期 | 通过条件 | 失败回滚 |
|------|----------|----------|----------|----------|
| **金丝雀（Canary）** | 1 个实例（或 5% 流量） | 30 分钟 | 错误率 ≤ 基线 + 5%；LLM兜底率无明显上升（≤ +2%） | 自动回滚该实例 |
| **小范围（Stage）** | 20% 实例 | 2 小时 | 同金丝雀，且影子评估毕业率不下降 | 自动回滚所有 Stage 实例 |
| **全量（Prod）** | 100% 实例 | 4 小时 | 无 SRE 告警触发 | 手动 / 自动回滚 |

### 4.2 自动化金丝雀分析指标

WebTouch 引擎在版本切换后，持续监控以下指标并与**切换前 1 小时的基线**进行对比：

- `uws_action_success_rate`：动作成功率（目标：≥ 基线 - 1%）
- `uws_llm_fallback_rate`：LLM 兜底率（目标：≤ 基线 + 2%）
- `uws_action_latency_ms P95`：延迟（目标：≤ 基线 + 50ms）
- `uws_not_found_rate`：节点找不到率（目标：≤ 基线 + 1%）

若任何指标在 **连续 3 个采样窗口（每 10 秒一个窗口）** 超出阈值，WebTouch 引擎**触发 `VersionDegraded` 告警**，并建议协调器或运维执行回滚。

---

## 5. 回滚操作标准程序（Rollback SOP）

### 5.1 触发条件（什么情况下必须回滚）

| 级别 | 条件 | 响应时间 | 动作 |
|------|------|----------|------|
| **P0（紧急）** | 连续 50 个 `ExecuteAction` 返回 `NOT_FOUND`（大面积失明） | **立即（< 1分钟）** | 自动回滚至上一个稳定版本 |
| **P1（高）** | 金丝雀指标连续 5 分钟超出阈值 | **5 分钟内** | 人工确认或自动回滚 |
| **P2（中）** | `uws_llm_fallback_rate` 异常飙升 50% 以上 | **15 分钟内** | 人工决策回滚 |
| **手动** | 运维人员发现任何异常（如日志中大量 `POISONED` 同义词） | **按需** | 执行 `grpcurl` 手动回滚命令 |

### 5.2 自动回滚执行流程（P0 级别）

当 WebTouch 引擎内部监控触发 P0 级条件时，**无需人工干预**，自动执行以下步骤：

```
1. 触发自动回滚信号（内部事件）。
   ↓
2. 从本地缓存或对象存储中加载上一个版本的 manifest.json。
   ↓ （通过 knowledge_versions 表中的 rollback_to 字段，或直接取 "latest" 软链接指向的旧版本）
3. 执行原子切换（同第 3 节双缓冲切换）。
   ↓
4. 切换完成后，立即对健康检查接口（HealthCheck）返回状态，附带回滚标记。
   ↓
5. 记录回滚事件到对象存储的 rollback_logs/ 中。
   ↓
6. 触发 PagerDuty 告警："WebTouch 知识库自动回滚已执行，版本 v1.4.0 → v1.3.9，请人工排查原因。"
```

### 5.3 手动回滚命令（运维人员操作）

运维人员通过 `grpcurl` 对任意一个 WebTouch 引擎实例发起回滚命令（所有实例共享同一个对象存储，但需逐实例执行；或通过集群管理工具批量下发）：

```bash
# 1. 查看当前版本
grpcurl -plaintext localhost:50051 uws.v1.UwsEngine/GetKnowledgeVersion
# 输出: {"version": "v1.4.0"}

# 2. 回滚到指定版本（v1.3.9）
grpcurl -plaintext -d '{"version": "v1.3.9"}' localhost:50051 uws.v1.UwsEngine/ReloadKnowledge
# 输出: {"ok": true, "active_version": "v1.3.9", "previous_version": "v1.4.0"}

# 3. （可选）验证健康状态
grpcurl -plaintext localhost:50051 uws.v1.UwsEngine/HealthCheck
# 输出: {"ok": true, "version": "v1.3.9"}
```

**回滚一致性保证**：
- 若 WebTouch 引擎部署了多副本（K8s Deployment），建议通过 **滚动更新（Rolling Update）** 方式逐 Pod 回滚，或通过配置管理（如 ConfigMap）将 `UWS_KNOWLEDGE_VERSION` 环境变量改回旧版本，触发 Pod 重启加载。
- 若采用双缓冲热加载（单实例），则直接执行上述 `ReloadKnowledge` 命令即可，**无需重启进程**。

### 5.4 回滚后的长尾清理（数据修复）

回滚版本后，若新版本在缓存中已生成部分 L2 规则（`site_synonyms` 标记为 ACTIVE），但旧版本不包含这些规则，可能出现不一致。执行以下清理：

- **TTL 自动失效**：回滚后，L2 规则的 `status` 若为 `ACTIVE` 且在回滚版本中无对应记录，系统自动将其置为 `STAGING` 并触发重新评估（避免污染旧知识）。
- **手动清理**：运维人员可执行 SQL 删除缓存目录中的新版本文件（`/tmp/uws_cache/v1.4.0/`），防止下次误加载。

---

## 6. 版本存储生命周期管理（Retention）

| 版本类型 | 保留时间 | 存储位置 |
|----------|----------|----------|
| 最新稳定版本（latest） | 永久（始终保留软链接指向） | 对象存储主桶 |
| 最近 5 个大版本（如 v1.3.x ~ v1.4.x） | 永久 | 对象存储主桶 |
| 更早的历史版本（v1.2.x 及以下） | 90 天 | 对象存储冷存储（如 Glacier） |
| 每日生成的自动快照（nightly） | 7 天 | 对象存储临时桶 |
| 本地缓存（WebTouch引擎节点磁盘） | 仅保留当前版本 + 上一个版本 | `/tmp/uws_cache/`，磁盘满时 LRU 驱逐 |

---

## 7. 监控仪表盘（Grafana）新增面板

运维团队在 Grafana 中需增加以下视图，用于巡检版本切换健康状况：

| 面板名称 | 指标来源 | 用途 |
|----------|----------|------|
| **知识库版本热力图** | `uws_knowledge_version`（Gauge） | 展示当前所有 WebTouch 实例正在使用的版本分布（用于确认全量发布进度） |
| **切换耗时瀑布** | `uws_reload_switch_duration_ms`（Histogram） | 观察双缓冲切换是否卡顿（P99 > 10ms 需告警） |
| **回滚事件时间线** | `uws_rollback_triggered`（Counter） | 标记回滚发生的时间，与业务错误率叠加分析 |
| **金丝雀指标偏离度** | `abs(current_error_rate - baseline_error_rate)`（Gauge） | 直观展示新版本与基线的偏差，便于快速决策 |

---

## 8. 异常场景与容灾预案

| 异常场景 | 预案 |
|----------|------|
| **对象存储不可用（S3 故障）** | 所有 WebTouch 节点启动时已缓存当前版本。若加载新版本失败，保持现有版本运行，不阻塞业务。 |
| **新版本加载时内存超限（OOM）** | 引擎在加载前检查可用内存。若可用内存 < 300MB，直接拒绝加载并返回 `RESOURCE_EXHAUSTED`。 |
| **双缓冲切换期间进程崩溃** | WebTouch 引擎重启后，从 `version.json` 文件中读取上次持久化的版本号，并从本地缓存或对象存储加载该版本。该文件在每次切换成功后更新。 |
| **回滚到某个版本时，该版本 artifact 已被冷存储归档** | 若回滚目标为超过 90 天的历史版本，引擎提示错误并拒绝回滚；运维需先从冷存储恢复（耗时约 1-2 小时）。因此建议将最近 5 个版本保留在热存储中。 |

---

**文档结束。下一份待补文档为：文档四（安全/合规/DX 补充 —— 不含已提前内建的 PII 脱敏）**。内容聚焦 gRPC 认证、XSS 防护、Playwright 适配层轻量设计、以及可视化调试工具 MVP 规格。请指示继续输出。