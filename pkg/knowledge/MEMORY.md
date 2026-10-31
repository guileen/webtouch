# pkg/knowledge MEMORY（M5：知识库双缓冲热更 / 回滚）

## 已点亮（2026-08-19，Go 逻辑层）
- TDD-KB-01（dry_run 仅下载校验）、02（原子切换 + 写锁 <1ms + 旧句柄 TTL 60s）、
  03（Sanity Check 失败保旧版）、04（内存超限 RESOURCE_EXHAUSTED）、
  05（版本兼容 + force 跳过）、07（回滚一致性：ACTIVE→STAGING）。
- TDD-KB-06（P0 自动回滚）为 E2E，留 M7，仍为 skip。

## 设计要点
- `Store` = active/previous 双句柄；切换写锁内仅指针交换（实测 SwitchTimeMs=0ms，远 <1ms）。
- 旧句柄在切换时续 TTL（60s），`Reap()` 过期释放；异步验证器可先 `AcquireHandle` 再切。
- Sanity：`KeyRuleIndexCount>0` + `OnnxReady`；内存：新旧估算和 >300MB → `RESOURCE_EXHAUSTED`。
- 回滚一致性在 `Rollback` 内修复：新版本 ACTIVE 且目标版本无记录的规则 → STAGING。
- `Fetcher` 接口抽象下载 + SHA256 校验 + 加载（测试注入内存实现）。

## 踩坑
- `Version` 是 `Artifact + handle` 组合，没有 `Version` 字段；取版本号统一用
  `v.version()` 方法，测试里直接 `.Version` 会编译报错。
- semver 比较要容忍 `v` 前缀、`x.y` 缺省段；`compatible` 双边界校验（2026-08-20 续7）：
  `min_engine_version ≤ 引擎 ≤ max_engine_version`；max 支持 `x` 通配
  （`v1.4.x`=v1.4 任意 patch、`v1.x`=v1 任意 minor/patch、空=无上限）。
- exporter 默认 `max_engine_version` 必须与当前 `EngineVersion` 对齐（现 v1.5.x）；
  升级引擎版本时同步改 exporter 默认，否则产物会被自己引擎拒载。
- fakeFetcher 必须深拷贝 `Rules` map，否则 Rollback 的一致性修复会污染全局样本。

## 后续
- ~~KB-06 自动回滚（连续 50 次 NOT_FOUND 触发）需与引擎 ExecuteAction 统计打通，M7 做 E2E。~~
  （2026-08-20 已点亮，见下节）

## P0 自动回滚（2026-08-20，TDD-KB-06）

- `Store.AutoRollback()`：复用双缓冲 previous 句柄（免重新下载，<1 分钟语义）——
  **重建**回滚目标 Artifact + 新 handle（不可复用旧句柄：handle 持有创建时规则快照，
  而 `demoteUnknownActive` 一致性修复会修改规则集；旧句柄仍可由在途异步验证器持有
  至 TTL 60s）。
- 回滚后 `previous=nil`：坏版本被丢弃，防止二次连续失败乒乓回滚到坏版本。
- `lastAutoRollback` 是 HealthCheck 回滚标记数据源（proto v1.5
  `rolled_back`/`rollback_reason`）；人工 Reload/Rollback 成功后清除。

## HTTP 对象存储 fetcher（2026-08-19 深夜续，USER_ACTION #7）

### 已点亮
- `httpfetcher.go`：HTTP(S) fetcher 落地，契约层 KB-02/04/05/07 点亮
  （`tests/integration/test_knowledge.py` http_engine 夹具：exporter 四版本产物 +
  本地 HTTP 服务 + `--knowledge-base-url`）。
- exporter manifest 自动写 `memory_estimate_mb`（8MB 基础 + 产物字节向上取整 MB；
  调用方显式提供时以调用方为准）→ KB-04 门禁输入。

### 设计要点
- 传输层 = HTTP(S) 网关/CDN/预签名 URL；`s3://` 对象键映射 `baseURL/version`；
  S3 SigV4/OSS 原生签名留待真实部署（GLOBAL.md §5 裁决 #10）。
- 下载保护：单文件 ≤512MB；文件名拒绝 `/`、`\`、`..`。
- 校验顺序：404 → 大小不符 → SHA256 不符；错误文案 `artifact not found` 与
  ReloadResponse 契约层断言对齐。
- `Artifact` 元信息来自 uws_l1.db：schema/model 哈希取 `knowledge_versions` 行；
  规则状态表 key = `rule_type:semantic_label`（缺省回退 `rule_type:text_pattern`），
  status 统一 ToUpper（exporter 可写 active/ACTIVE 混合大小写）；ACTIVE 计数用
  `LOWER(status)='active'`。

### 踩坑
- `json.Unmarshal` 到 `map[string]any` 的数值是 float64，内存估算取值需兼容
  float64/int/int64/json.Number。
- manifest 版本必须与请求版本严格一致（防拿错目录当最新版）。
- uws_l1.db 必须写临时文件再以 SQLite 打开（`:memory:` 无法跨进程读取文件字节）；
  加载完立即清理，避免缓存目录残留。
