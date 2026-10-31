# WebTouch QA Studio Fail-Closed 与 Cloud Direct 纵向切片执行计划

> **状态**：批准执行
> **日期**：2026-09-16
> **定位**：`agent-tools/webtouch` 内部工程落地的最小原子切片执行计划。
> **核心原则**：
> 1. **分仓置于 HOLD**：不建立 `labs/webtouch-cloud/`，不建立 `labs/repos/webtouch/`，暂不做多仓拆分，当前全部实现留在 `agent-tools/webtouch/`；
> 2. **排除过早复杂度**：不做 Tunnel，不做跨端复杂反向代理，首期 1–3 单（US$99 礼宾）聚焦支持人工准入的 **Cloud Direct 纯公开 URL** 模式；
> 3. **最小不可拆原子闭环**：杜绝单文件浮躁修改，按切片推进并附带回归测试。

---

## 一、 四个严密递进的垂直切片

```
[切片 1: Fail-Closed 最小原子闭环] ────► [切片 2: 现仓 Cloud Direct 真实挂载] ────► [切片 3: 隔离执行与单节点压测] ────► [切片 4: 首单演练与退款闭环]
   • 拔除 server.go 假数据                • 挂载真实无头 CDP Runner              • 一任务一沙箱无 docker.sock          • 2小时时钟与失败退款模拟
   • 页面默认 NOT RUN / --                • 单并发锁定 (MaxConcurrent=1)         • 退出后外部提取产物                  • 真实公开 URL 全链走查
   • 统一 EXECUTED/NOT_RUN + 4态          • 复合页面稳定判据 (非裸 NetworkIdle)   • 资源硬限制与 tmpfs 清理             • 交付物 HTML+JSON+修复Prompt
   • 503/BLOCKED 显式渲染与单测锁死       • SSRF/RFC1918 运行时硬阻断
```

---

## 二、 切片 1：Fail-Closed 详细执行定义（当前进行中）

### 1. 涉及文件与联动边界
- **契约与模型层**：`pkg/qa/types.go`
  - 增加运行事实：`ExecutionStatus = "EXECUTED" | "NOT_RUN"`；
  - 统一定义四态裁决：`Verdict = "PASS" | "FAIL" | "BLOCKED" | "NEEDS_EVIDENCE"`（保留向后兼容别名 `WARNING`/`CRITICAL`）；
  - 定义错误返回结构：`QAFailureResponse`（包含 `execution_status`, `verdict`, `reason_code`, `error`）。
- **服务端处理层**：`pkg/webcontrol/server.go`
  - 彻底删除 `s.qaRunner == nil` 时的 Mock 报告生成逻辑；
  - 当 Runner 未配置或报错时，严格返回 `HTTP 503 Service Unavailable`，响应体输出规范 JSON：
    ```json
    {
      "execution_status": "NOT_RUN",
      "verdict": "BLOCKED",
      "reason_code": "RUNNER_UNAVAILABLE",
      "error": "QA Engine Runner not initialized or configured"
    }
    ```
- **前端展示与异常分支**：`pkg/webcontrol/assets/index.html`
  - DOM 初始静态值由写死的 `PASS` / `98` 改为 `NOT RUN` / `--`；
  - 修改 `triggerQARun()`：当 `!resp.ok`（如收到 503）时，不再静默忽略，而是正确解析错误 JSON，将 Badge 更新为 `BLOCKED`，得分显示 `--`，并在日志控制台输出清晰原因。
- **回归测试验证**：`pkg/webcontrol/server_test.go`
  - 新增 `TestWebControlQARunAPI_RunnerNil_FailClosed`：
    - 针对 `QARunner == nil` 发起 POST `/api/qa/run`；
    - 断言 HTTP 返回码为 503；
    - 断言响应体为 `execution_status: NOT_RUN`, `verdict: BLOCKED`, `reason_code: RUNNER_UNAVAILABLE`；
    - 保证未挂载 Runner 时任何路径都不会产生 PASS。

---

## 三、 验收标准 (Definition of Done for Slice 1)

1. `go test ./pkg/webcontrol/...` 与 `go test ./pkg/qa/...` 必须 100% 绿灯通过；
2. 无论何时未接真实 Runner，前端控制台与接口绝不出现 PASS 或假分数；
3. 不产生新文件、新仓库或占位目录；
4. 更新 `agent-tools/webtouch/.mem/handoff.md` 与 `labs/handoff.md`。
