# 状态导出 / 凭证延续（State Export v1.5.6，2026-09-02 实测；仅相关任务必读）

- 用途：真实 Chrome 完成初始化/挑战解锁后，调 `ExportState(target_id, target_origin)` 导出
  当前上下文全量 Cookies JSON（含 HttpOnly 的 cf_clearance 类票据，原生 Network.getAllCookies
  序列化数组）+ 指定 origin 的 LocalStorage，供跨任务凭证延续/后续轻量化，避免重走反爬。
- 契约唯一事实源：`proto/uws_service.proto` 的 `ExportStateRequest/ExportStateResponse`；
  服务端实现 `pkg/engine/service.go` → `pkg/browser/cdp_session.go`。
- 链路（与用户构想一致）：InitializeContext →（若 403+cf-chl：普通 ExecuteAction 抛 gRPC
  `UNAUTHENTICATED` = CHALLENGE_INTERCEPTED → `drag:x1,y1,x2,y2` 坐标直发解锁）→ 抵达受保护
  界面 → ExportState → 凭证 JSON 落给后续爬虫/扫描配置。
- 开发/回归验证 = 维护会话内执行（`docs/tdd-checklist.md` + `tests/e2e/`），
  不属于任务 SOP；分步截图样例 `webtouch/tmp/export-state-demo/*.png`。
- 经验细节与后续边界：`uws help`（cmd/uws/agent-guide.md）最佳实践 #9-10；挑战拦截分支以本地
  403+cf-chl 页确定性覆盖，外部 CF 站点（chatgpt.com）策略随环境漂移不做常绿门禁；
  CLI/网关尚无 ExportState 子命令（当前走引擎 gRPC）。
