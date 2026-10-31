package provider

import "uws/pkg/qa"

// DefaultViewportSpecs 是 Provider 的默认体检视口（与历史行为一致：
// mobile 与 desktop 两档、不带缩放）。新增视口只改这里，避免默认值散落在两处。
func DefaultViewportSpecs() []ViewportSpec {
	return []ViewportSpec{
		{Name: "mobile", Width: 390, Height: 844},
		{Name: "desktop", Width: 1440, Height: 900},
	}
}

// ParseViewports 解析逗号分隔的视口规格（`mobile,desktop` / `1440x900` / `390` 等），
// 返回 Provider 自有 DTO。解析规则复用 pkg/qa 的唯一一份实现——这里只做类型翻译，
// 让调用方（CLI/Agent Hub）不必 import pkg/qa。
func ParseViewports(raw string) ([]ViewportSpec, error) {
	vps, err := qa.ParseViewports(raw)
	if err != nil {
		return nil, err
	}
	return fromQaViewports(vps), nil
}

// toQaViewports 把 Provider DTO 翻译为 QA Runner 的领域类型（适配层唯一出口）。
func toQaViewports(specs []ViewportSpec) []qa.Viewport {
	if len(specs) == 0 {
		return nil
	}
	out := make([]qa.Viewport, 0, len(specs))
	for _, s := range specs {
		out = append(out, qa.Viewport{
			Name:   s.Name,
			Width:  s.Width,
			Height: s.Height,
			Scale:  s.Scale,
			Mobile: s.Mobile,
		})
	}
	return out
}

// fromQaViewports 是 toQaViewports 的反向翻译（解析结果回灌 Provider DTO）。
func fromQaViewports(vps []qa.Viewport) []ViewportSpec {
	if len(vps) == 0 {
		return nil
	}
	out := make([]ViewportSpec, 0, len(vps))
	for _, v := range vps {
		out = append(out, ViewportSpec{
			Name:   v.Name,
			Width:  v.Width,
			Height: v.Height,
			Scale:  v.Scale,
			Mobile: v.Mobile,
		})
	}
	return out
}
