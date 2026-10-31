package find

import (
	"context"
	"strings"

	"uws/pkg/browser/semantics"
)

// CDPCaller 是 search 依赖的 CDP 命令下发能力（§5.0.16 条款 1）。
type CDPCaller interface {
	Call(ctx context.Context, method string, params map[string]any, out any) error
}

// SearchMatch 记录一次 CDP 原生文本搜索命中的节点几何与身份（spec/algorithm-kernel.md §5.0.16）。
type SearchMatch struct {
	NodeID int64   `json:"node_id"`
	X      float64 `json:"x"`     // 视口中心点 X（CSS 像素）
	Y      float64 `json:"y"`     // 视口中心点 Y（CSS 像素）
	Width  float64 `json:"w"`     // 宽度
	Height float64 `json:"h"`     // 高度
	Query  string  `json:"query"` // 命中时的检索词/别名
}

// boxFromQuad 将 CDP DOM.getBoxModel 返回的 8 元 content quad 转为视口矩形。
func boxFromQuad(q []float64) (x, y, w, h float64, ok bool) {
	if len(q) < 8 {
		return 0, 0, 0, 0, false
	}
	minX, maxX := q[0], q[0]
	minY, maxY := q[1], q[1]
	for i := 0; i+1 < 8; i += 2 {
		if q[i] < minX {
			minX = q[i]
		}
		if q[i] > maxX {
			maxX = q[i]
		}
		if q[i+1] < minY {
			minY = q[i+1]
		}
		if q[i+1] > maxY {
			maxY = q[i+1]
		}
	}
	w = maxX - minX
	h = maxY - minY
	if w < 2 || h < 2 {
		return 0, 0, 0, 0, false
	}
	return minX, minY, w, h, true
}

// searchSingleQuery 执行单个字面词的 DOM.performSearch 检索并获取首个可见盒模型（单次搜索释放 searchId）。
func searchSingleQuery(ctx context.Context, caller CDPCaller, query string) (*SearchMatch, error) {
	_ = caller.Call(ctx, "DOM.enable", nil, nil)
	_ = caller.Call(ctx, "DOM.getDocument", map[string]any{"depth": 1}, nil)
	var performRes struct {
		SearchID    string `json:"searchId"`
		ResultCount int    `json:"resultCount"`
	}
	err := caller.Call(ctx, "DOM.performSearch", map[string]any{
		"query":                     query,
		"includeUserAgentShadowDOM": true,
	}, &performRes)
	if err != nil {
		return nil, err
	}
	if performRes.SearchID == "" {
		return nil, nil
	}
	// 无论成功还是失败，均主动释放 Blink 内部搜索句柄，严防内存泄漏
	defer func() {
		_ = caller.Call(ctx, "DOM.discardSearchResults", map[string]any{
			"searchId": performRes.SearchID,
		}, nil)
	}()

	if performRes.ResultCount <= 0 {
		return nil, nil
	}

	// 仅拉取前 10 个命中节点做盒模型可见性探测，兼顾性能与召回
	toIndex := performRes.ResultCount
	if toIndex > 10 {
		toIndex = 10
	}
	var getRes struct {
		NodeIDs []int64 `json:"nodeIds"`
	}
	if err := caller.Call(ctx, "DOM.getSearchResults", map[string]any{
		"searchId":  performRes.SearchID,
		"fromIndex": 0,
		"toIndex":   toIndex,
	}, &getRes); err != nil {
		return nil, err
	}

	for _, nodeID := range getRes.NodeIDs {
		var boxRes struct {
			Model struct {
				Content []float64 `json:"content"`
			} `json:"model"`
		}
		if err := caller.Call(ctx, "DOM.getBoxModel", map[string]any{
			"nodeId": nodeID,
		}, &boxRes); err != nil {
			continue
		}
		x, y, w, h, ok := boxFromQuad(boxRes.Model.Content)
		if !ok {
			continue
		}
		return &SearchMatch{
			NodeID: nodeID,
			X:      x + w/2,
			Y:      y + h/2,
			Width:  w,
			Height: h,
			Query:  query,
		}, nil
	}
	return nil, nil
}

// SearchSemantic 依据意图语义与 §5.0.2 别名展开集进行 CDP 原生检索，返回首个有效命中作为粗锚点（§5.0.16 条款 2）。
// 零 Token、<2ms；若所有别名均未命中或不可见，返回 nil 且无错（fail-soft 维持文档序）。
func SearchSemantic(ctx context.Context, caller CDPCaller, semantic string) (*SearchMatch, error) {
	if caller == nil {
		return nil, nil
	}
	s := strings.TrimSpace(semantic)
	if s == "" {
		return nil, nil
	}
	wants := semantics.Wants(s)
	if len(wants) == 0 {
		wants = []string{s}
	}
	for _, want := range wants {
		q := strings.TrimSpace(want)
		if q == "" {
			continue
		}
		match, err := searchSingleQuery(ctx, caller, q)
		if err == nil && match != nil {
			return match, nil
		}
	}
	return nil, nil
}
