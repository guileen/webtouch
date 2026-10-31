// 真实 CDP 会话（生产物理执行层）——包内导航页。
//
// 实现 browser.CDPSession + ActionSession + VerifierEvents + CandidateSource：
//   - 启动模式：每上下文拉起独立 headless Chrome（临时 user-data-dir，Cookie/缓存物理隔离）；
//   - 附着模式（AttachCDPURL）：连接已运行的 Chrome，新建标签页共享登录态（TDD-ENG-08）；
//   - 物理动作：DOM 语义定位 + 遮挡检测 + Fitts 三次贝塞尔鼠标轨迹（TDD-ENG-03/04）；
//   - 异步验证：Page.navigatedWithinDocument（SPA pushState）+ Network XHR/Fetch
//     在途计数 + MutationObserver 采样（TDD-ENG-06/07）；
//   - 页面候选：DOM 交互节点采集（真实 MATCH-05/06 输入通道）。
//
// 2026-09-26 复杂度治理（`T-24` 拆分批次·独立立项）：本文件原 4430 行 / 180KB（超硬限），
// 按职责拆为同包 12 个文件；**纯搬运、零语义改动**（同 package 内声明顺序无关，调用方零感知）。
// 分片索引（改代码请进对应分片，勿在本页堆砌）：
//
//	cdp_session_factory.go     CDPFactory、会话状态类型、Chrome 启动/偏好/端口/wait
//	cdp_session_core.go        cdpSession 结构体、窗口策略与几何、target 启动（initTarget）
//	cdp_session_fingerprint.go 目标域启用、指纹与视口注入
//	cdp_session_events.go      事件订阅（subscribe）、在途请求/challenge 头
//	cdp_session_state.go       Probe、状态导出导入（Export/ImportStateBundle）
//	cdp_session_control.go     导航、Cookie、Close、screencast、teardown
//	cdp_session_action.go      ExecuteAction* 与派发主链（executeActionInner）
//	cdp_session_find.go        语义/选择器定位与节点解析（findElementMode 等）
//	cdp_session_input.go       鼠标/指针派发、人机输入、截图
//	cdp_session_eval.go        JS eval、导航态、target 切换、候选采集
//	cdp_session_snapshot.go    pageSnapshotJS 与 PageSnapshot（页面候选投影）
//	cdp_js_helpers.go          JS 字面量辅助与贝塞尔路径、ChromePath
package browser
