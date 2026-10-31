package tier2

import "context"

// Session 是 Tier2 runner 与 CDP 会话之间的最小契约面。
//
// 只列出 runner 实际用到的原语（导航 / 三种求值 / 取当前 URL / 点击 / 原始 CDP 调用），
// 由 pkg/browser 内的薄适配器把 *cdpSession 接上来。不能直接复用 *cdpSession：
// Go 的未导出方法名带包限定，跨包无法构成接口实现。
type Session interface {
	Navigate(ctx context.Context, url string) error
	EvalBool(ctx context.Context, expr string) (bool, error)
	EvalValue(ctx context.Context, expr string, dst any) error
	EvalResult(ctx context.Context, expr string) (string, error)
	PageURL(ctx context.Context) string
	ClickPoint(ctx context.Context, x, y float64) error
	CallSession(ctx context.Context, method string, params, out any) error
}
