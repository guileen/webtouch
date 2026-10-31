// Package engine 是 UWS 引擎 gRPC 服务接线层：把已点亮的逻辑包
// （browser.Pool / knowledge.Store / matcher / classifier / verifier）
// 接到冻结的 proto v1.5.4 契约（uws_service.proto，追加-only），并承载安全红线
// （API Key 鉴权 + 输入边界校验，见 spec/security-dx.md §1-§2）。
//
// 本包同时提供零配置开发用的模拟 CDP 会话（SimFactory/SimCDPSession）；
// 真实 CDP 适配（chrome-remote-interface）待接入，见 USER_ACTION.md。
package engine
