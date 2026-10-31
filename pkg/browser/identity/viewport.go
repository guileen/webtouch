package identity

import (
	"context"
	"fmt"
	"time"
)

// SetDeviceMetrics 是 Emulation.setDeviceMetricsOverride 的单项能力（「视口设备指标」
// 原语）：宿主 `applyViewportMetrics` 与 `SetViewport` 两处重复的字面量合并到这一处，
// 策略（参数守卫、scale 缺省、写回 `cfg.Viewport`）留宿主 ⇒ 合并后逐分支等价。
func SetDeviceMetrics(ctx context.Context, s Session, sessionID string, width, height int, scale float64, mobile bool) error {
	return s.CallSession(ctx, sessionID, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             width,
		"height":            height,
		"deviceScaleFactor": scale,
		"mobile":            mobile,
	}, nil)
}

// AdoptWindowViewport 附着模式未显式指定视口时，读取页面实际布局视口
// （window.innerWidth/innerHeight，兜底 outerWidth/outerHeight）——宿主
// `adoptWindowViewport` 的逐行搬移；`dsf` 由宿主解析，结果返回宿主写入 `cfg.Viewport`。
func AdoptWindowViewport(ctx context.Context, s Session, dsf float64) (int, int, error) {
	var v struct {
		W int `json:"w"`
		H int `json:"h"`
	}
	read := func(expr string) (int, int, error) {
		v = struct {
			W int `json:"w"`
			H int `json:"h"`
		}{}
		if err := s.EvalValue(ctx, expr, &v); err != nil {
			return 0, 0, err
		}
		return v.W, v.H, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		w, h, err := read(`({w: window.innerWidth, h: window.innerHeight})`)
		if err != nil {
			return 0, 0, err
		}
		if w > 0 && h > 0 {
			return w, h, nil
		}
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	w, h, err := read(`({w: window.outerWidth, h: window.outerHeight})`)
	if err == nil && w > 0 && h > 0 {
		return w, h, nil
	}
	return 0, 0, fmt.Errorf("window viewport unavailable (inner/outer empty)")
}
