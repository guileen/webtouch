// Package contract 存放跨层共享的最小接口契约。
//
// 位置纪律：本包只允许依赖标准库，位于算法层与引擎层之下——
// 应用层（apps/）与算法层可引用这些接口，而不必把浏览器实现拉进依赖图。
// 依赖方向：apps / pkg/* 均可 import contract；contract 不得 import 任何 uws 包。
package contract

import "context"

// DirectScriptEvaluator 是 CDP 会话执行页内原生 JS 异步表达式的能力（In-Page Direct Transport 基础）。
// 支持跨框架直接在页面 Runtime 注入 fetch、读取 ReadableStream 或捕获内部接口响应，免除 DOM 点击脆性。
//
// 实现方：browser.CDPSession（真实/模拟 CDP 会话）、apps/gateway.EngineScriptEvaluator（gRPC 客户端）。
// 定义于 2026-09-23 由 pkg/browser 下沉至此：应用层原先为引用此接口而 import pkg/browser，
// 使整个浏览器内核进入网关二进制的依赖图。
type DirectScriptEvaluator interface {
	EvaluateScript(ctx context.Context, expr string) ([]byte, error)
}
