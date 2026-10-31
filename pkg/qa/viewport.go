package qa

import (
	"fmt"
	"strconv"
	"strings"
)

// Viewport 定义视口物理尺寸与模拟参数。
type Viewport struct {
	Name   string  `json:"name"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Scale  float64 `json:"scale"`
	Mobile bool    `json:"mobile"`
}

var (
	// ViewportMobile: iPhone 标准物理尺寸 390x844, DPR 3.0, mobile=true
	ViewportMobile = Viewport{
		Name:   "mobile",
		Width:  390,
		Height: 844,
		Scale:  3.0,
		Mobile: true,
	}
	// ViewportTablet: iPad 标准物理尺寸 768x1024, DPR 2.0, mobile=false
	ViewportTablet = Viewport{
		Name:   "tablet",
		Width:  768,
		Height: 1024,
		Scale:  2.0,
		Mobile: false,
	}
	// ViewportDesktop: MacBook 标准物理尺寸 1440x900, DPR 2.0, mobile=false
	ViewportDesktop = Viewport{
		Name:   "desktop",
		Width:  1440,
		Height: 900,
		Scale:  2.0,
		Mobile: false,
	}
)

// DefaultViewports 返回标准移动/平板/桌面三视口配置。
func DefaultViewports() []Viewport {
	return []Viewport{ViewportMobile, ViewportTablet, ViewportDesktop}
}

// ParseViewports 解析由逗号分隔的视口字符串。
// 支持格式：
// - 命名别名：mobile, tablet, desktop
// - 宽度整数：390, 768, 1440（按标准高度与缩放自动填充）
// - 宽x高：390x844, 768x1024, 1440x900
func ParseViewports(raw string) ([]Viewport, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultViewports(), nil
	}

	parts := strings.Split(raw, ",")
	var result []Viewport

	for _, p := range parts {
		item := strings.TrimSpace(p)
		if item == "" {
			continue
		}
		switch strings.ToLower(item) {
		case "mobile", "phone", "iphone":
			result = append(result, ViewportMobile)
		case "tablet", "pad", "ipad":
			result = append(result, ViewportTablet)
		case "desktop", "pc", "mac", "laptop":
			result = append(result, ViewportDesktop)
		default:
			// 尝试解析 WxH 或 W
			if strings.Contains(item, "x") || strings.Contains(item, "X") {
				dims := strings.FieldsFunc(item, func(r rune) bool {
					return r == 'x' || r == 'X'
				})
				if len(dims) != 2 {
					return nil, fmt.Errorf("qa: invalid viewport format %q, expected WxH", item)
				}
				w, err := strconv.Atoi(strings.TrimSpace(dims[0]))
				if err != nil || w <= 0 {
					return nil, fmt.Errorf("qa: invalid viewport width in %q", item)
				}
				h, err := strconv.Atoi(strings.TrimSpace(dims[1]))
				if err != nil || h <= 0 {
					return nil, fmt.Errorf("qa: invalid viewport height in %q", item)
				}
				isMobile := w < 600
				result = append(result, Viewport{
					Name:   fmt.Sprintf("%dx%d", w, h),
					Width:  w,
					Height: h,
					Scale:  2.0,
					Mobile: isMobile,
				})
			} else {
				w, err := strconv.Atoi(item)
				if err != nil || w <= 0 {
					return nil, fmt.Errorf("qa: unknown viewport specification %q", item)
				}
				// 自动推断高度与形态
				if w <= 480 {
					result = append(result, Viewport{
						Name:   fmt.Sprintf("%d", w),
						Width:  w,
						Height: 844,
						Scale:  3.0,
						Mobile: true,
					})
				} else if w <= 1024 {
					result = append(result, Viewport{
						Name:   fmt.Sprintf("%d", w),
						Width:  w,
						Height: 1024,
						Scale:  2.0,
						Mobile: false,
					})
				} else {
					result = append(result, Viewport{
						Name:   fmt.Sprintf("%d", w),
						Width:  w,
						Height: 900,
						Scale:  2.0,
						Mobile: false,
					})
				}
			}
		}
	}

	if len(result) == 0 {
		return DefaultViewports(), nil
	}
	return result, nil
}
