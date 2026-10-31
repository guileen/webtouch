package browser

import (
	"context"
	"fmt"
	"time"

	"uws/pkg/browser/tier2"
)

// Tier2 runner 的公开面（兼容层）。
//
// 实现已搬入子包 pkg/browser/tier2（依据 docs/complexity-budget-spec-2026-09-26.md 附录 A 阶段 1）。
// 本文件只保留三件事：① 公开类型别名（消费方零改动）② *cdpSession 的窄接口适配
// ③ RunTier2 入口（会话创建仍留在包内）。
type (
	Tier2Options = tier2.Options
	Tier2Report  = tier2.Report
	Tier2Dim0    = tier2.Dim0
	Tier2Dim1    = tier2.Dim1
	Tier2Dim2    = tier2.Dim2
	Tier2Dim3    = tier2.Dim3
)

// tier2SessionBridge 把包内 *cdpSession 适配成 tier2.Session。
//
// 未导出方法名在 Go 里是「包限定」标识符，跨包无法直接构成接口实现，故需要这层
// 纯转发的薄适配（每个方法只做一次调用搬运，不改变任何语义）。
type tier2SessionBridge struct{ s *cdpSession }

func (b tier2SessionBridge) Navigate(ctx context.Context, url string) error {
	return b.s.Navigate(ctx, url)
}

func (b tier2SessionBridge) EvalBool(ctx context.Context, expr string) (bool, error) {
	return b.s.evalBool(ctx, expr)
}

func (b tier2SessionBridge) EvalValue(ctx context.Context, expr string, dst any) error {
	return b.s.evalValue(ctx, expr, dst)
}

func (b tier2SessionBridge) EvalResult(ctx context.Context, expr string) (string, error) {
	return b.s.evalResult(ctx, expr)
}

func (b tier2SessionBridge) PageURL(ctx context.Context) string {
	return b.s.pageURL(ctx)
}

func (b tier2SessionBridge) ClickPoint(ctx context.Context, x, y float64) error {
	return b.s.clickPoint(ctx, x, y)
}

func (b tier2SessionBridge) CallSession(ctx context.Context, method string, params, out any) error {
	return b.s.client.CallSession(ctx, b.s.sessionID, method, params, out)
}

// RunTier2 使用真实 Chrome 执行四维动态沙盒测量（实现见 pkg/browser/tier2）。
func RunTier2(ctx context.Context, opts Tier2Options) (Tier2Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	factory := &CDPFactory{
		ChromePath:    opts.ChromePath,
		Headless:      true,
		ProbeWasmPath: opts.ProbeWasmPath,
	}
	sess, err := factory.Create(ctx, Config{})
	if err != nil {
		return Tier2Report{}, fmt.Errorf("tier2: create cdp session: %w", err)
	}
	cs, ok := sess.(*cdpSession)
	if !ok {
		_ = sess.Close()
		return Tier2Report{}, fmt.Errorf("tier2: session type %T is not *cdpSession", sess)
	}
	defer cs.Close()

	return tier2.Run(ctx, opts, tier2SessionBridge{s: cs})
}
