# WebTouch 知识库运维手册 (Knowledge Ops Manual)

本手册面向 SRE 与运维人员，指导 WebTouch 知识库（L1/L2 规则库 + ONNX 分类器）的版本查看、热更新、分级灰度、紧急回滚与故障处置。底层规格参见 [knowledge-versioning.md](../spec/knowledge-versioning.md)，接口定义参见 [uws_service.proto](../proto/uws_service.proto)。

---

## 1. 状态巡检与版本查看

### 1.1 查询当前知识库版本
```bash
grpcurl -plaintext localhost:50051 uws.v1.UwsEngine/GetKnowledgeVersion
# 响应：{"version": "v1.4.0", "schema_hash": "7f83b165...", "model_hash": "9a4f2c8e..."}
```

### 1.2 检查实例健康状态
```bash
grpcurl -plaintext localhost:50051 uws.v1.UwsEngine/HealthCheck
# 响应：{"ok": true, "version": "v1.4.0", "active_targets": 12, "max_targets": 100}
```

---

## 2. 知识库更新与分级灰度流程

知识库热更新采用**双缓冲架构**，零停机无缝切换。

### 2.0 启动接入（对象存储 HTTP 网关）

引擎通过 `--knowledge-base-url` 接入 artifact 目录（pipeline/exporter 产物布局：
`<版本>/manifest.json + checksums.sha256 + uws_l1.db + classifier.onnx +
uws_l2_default.db`），`s3://` 等对象键自动映射为 `baseURL/<version>`；
也可直传 `artifact_uri` 为 HTTP(S) 预签名 URL：

```bash
./bin/uws-engine \
  --grpc-addr=127.0.0.1:50051 \
  --api-key-file=./keys.json \
  --knowledge-base-url=http://127.0.0.1:8000/artifacts \
  --knowledge-version=v1.4.0 \
  --onnx-runtime-lib=/usr/local/lib/libonnxruntime.so.1.23.0
```

引擎启动即下载并 SHA256 校验该版本；加载失败拒绝启动（防带病上线）。
`--onnx-runtime-lib` 启用真实 ONNX 打分（A 案 ort-go CGO，与 KB artifact 的
`classifier.onnx` 同生命周期热更/回滚）；不配置时决策核降级为确定性 mock。

### 2.1 更新操作命令

```bash
# 步骤 1: 预检（Dry-run：仅下载与 SHA256/自检校验，不生效）
grpcurl -plaintext -d '{
  "version": "v1.4.1",
  "artifact_uri": "s3://uws-artifacts/v1.4.1",
  "dry_run": true
}' localhost:50051 uws.v1.UwsEngine/ReloadKnowledge

# 步骤 2: 正式热加载（原子指针切换，耗时 < 1ms）
grpcurl -plaintext -d '{
  "version": "v1.4.1",
  "artifact_uri": "s3://uws-artifacts/v1.4.1"
}' localhost:50051 uws.v1.UwsEngine/ReloadKnowledge
# 响应：{"ok": true, "active_version": "v1.4.1", "previous_version": "v1.4.0", "switch_time_ms": 1}
```

### 2.2 三阶段灰度 SOP

| 阶段 | 范围 | 观察周期 | 准出指标 | 异常处置 |
| :--- | :--- | :--- | :--- | :--- |
| **金丝雀 (Canary)** | 1 个 Pod (5% 流量) | 30 分钟 | 动作成功率 $\ge$ 基线 - 1%<br>LLM 兜底率 $\le$ 基线 + 2% | 自动/手动回滚该实例 |
| **小流量 (Stage)** | 20% 实例 | 2 小时 | P95 延迟 $\le$ 基线 + 50ms<br>未找到节点率 $\le$ 基线 + 1% | 回滚所有 Stage 实例 |
| **全量 (Prod)** | 100% 实例 | 4 小时 | 无 `VersionDegraded` 告警 | 滚动执行全量回滚 |

---

## 3. 紧急回滚 SOP

### 3.1 触发级别
- **P0（紧急）**：连续 50 次请求返回 `NOT_FOUND`，系统自动触发回滚（< 1分钟）。
- **P1（高）**：金丝雀指标连续 5 分钟超标，SRE 5 分钟内执行回滚。
- **P2（中）**：LLM 兜底率突增 50%，15 分钟内人工决策回滚。

### 3.2 回滚操作命令
```bash
# 正常回滚至指定稳定版本
grpcurl -plaintext -d '{"version": "v1.4.0"}' localhost:50051 uws.v1.UwsEngine/ReloadKnowledge

# 紧急强制回滚（跳过兼容性检查）
grpcurl -plaintext -d '{"version": "v1.4.0", "force": true}' localhost:50051 uws.v1.UwsEngine/ReloadKnowledge
```

---

## 4. 告警处理 SOP

| 告警项 | 触发原因 | 处理步骤 |
| :--- | :--- | :--- |
| **VersionDegraded** | 新版本指标超出基线阈值 | 1. 确认告警 Pod 流量与日志<br>2. 对故障实例下发 `ReloadKnowledge` 回滚<br>3. 收集 `/tmp/uws_cache/` 日志通知研发 |
| **RESOURCE_EXHAUSTED** | 实例可用内存 < 300MB | 1. 查看 Pod 内存占用<br>2. 扩容 Pod 规格或清理本地无用缓存 |
| **SwitchTimeout** | 切换写锁等待/耗时 > 10ms | 1. 排查宿主机高并发 CPU 争用<br>2. 检查是否有卡死的长事务 |

---

## 5. 版本生命周期与清理

- **对象存储 (S3/OSS)**：
  - `latest` 与最近 5 个大版本：永久保留。
  - 历史版本（> 90 天）：归档至冷存储。
  - Nightly 快照：自动保留 7 天。
- **本地节点缓存**：
  - 仅保留当前生效与上一个版本（约 100MB）。
  - 磁盘空间告警时手动清理旧版本缓存：
  ```bash
  rm -rf /tmp/uws_cache/v1.3.9
  ```

---

## 6. Grafana 监控面板指标

| 面板名称 | Prometheus Metric / PromQL | 告警阈值 |
| :--- | :--- | :--- |
| **知识库版本分布** | `count by (version) (uws_knowledge_version)` | 版本分裂超过发布窗口 |
| **切换耗时** | `histogram_quantile(0.99, rate(uws_reload_switch_duration_ms_bucket[5m]))` | P99 > 10ms |
| **回滚事件** | `increase(uws_rollback_triggered_total[1h])` | > 0 次即发通知 |
| **金丝雀偏差** | `abs(uws_action_success_rate - uws_baseline_action_success_rate)` | 偏差 > 1% 持续 3 个窗口 |

> **接线现状（2026-09-24 `T-14` 复核）**：本节面板**当前不可用**——引擎无 Prometheus 导出通道
> （无 `promhttp` 依赖、无 `/metrics` 端点）。版本、健康与回滚事实请走 gRPC：`GetKnowledgeVersion`、
> `HealthCheck`，以及回滚日志（`pkg/knowledge/knowledge.go` 的 `LastAutoRollback`，阈值
> `AutoRollbackThreshold = 50`）。用户 2026-08-20 已定案不引入 client 依赖，此处保留为部署期面板目标，
> 不是现役能力。
