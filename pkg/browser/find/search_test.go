package find

import (
	"context"
	"errors"
	"testing"
)

type mockCDPCaller struct {
	calls []string
	// query -> (searchId, resultCount)
	searchResults map[string]struct {
		id    string
		count int
	}
	// searchId -> nodeIds
	searchNodes map[string][]int64
	// nodeId -> content quad
	nodeBoxes map[int64][]float64
	discarded []string
}

func (m *mockCDPCaller) Call(ctx context.Context, method string, params map[string]any, out any) error {
	m.calls = append(m.calls, method)
	switch method {
	case "DOM.performSearch":
		q, _ := params["query"].(string)
		res, ok := m.searchResults[q]
		if !ok {
			return nil
		}
		if p, ok := out.(*struct {
			SearchID    string `json:"searchId"`
			ResultCount int    `json:"resultCount"`
		}); ok {
			p.SearchID = res.id
			p.ResultCount = res.count
		}
		return nil
	case "DOM.getSearchResults":
		id, _ := params["searchId"].(string)
		nodes := m.searchNodes[id]
		if p, ok := out.(*struct {
			NodeIDs []int64 `json:"nodeIds"`
		}); ok {
			p.NodeIDs = nodes
		}
		return nil
	case "DOM.getBoxModel":
		nid, _ := params["nodeId"].(int64)
		quad, ok := m.nodeBoxes[nid]
		if !ok {
			return errors.New("node not visible or no box model")
		}
		if p, ok := out.(*struct {
			Model struct {
				Content []float64 `json:"content"`
			} `json:"model"`
		}); ok {
			p.Model.Content = quad
		}
		return nil
	case "DOM.discardSearchResults":
		id, _ := params["searchId"].(string)
		m.discarded = append(m.discarded, id)
		return nil
	default:
		return nil
	}
}

func TestSearchSemantic_AliasExpansionAndMatch(t *testing.T) {
	ctx := context.Background()
	caller := &mockCDPCaller{
		searchResults: map[string]struct {
			id    string
			count int
		}{
			// search_box 别名：["搜索", "搜尋", "search", "站内搜索", "关键字", "关键词", "search_box"]
			// 模拟 "搜索" 无结果，"搜尋" 无结果，"search" 命中
			"search": {id: "sid-search", count: 2},
		},
		searchNodes: map[string][]int64{
			"sid-search": {101, 102},
		},
		nodeBoxes: map[int64][]float64{
			// 101: 不可见（尺寸过小 1x1）
			101: {10, 10, 11, 10, 11, 11, 10, 11},
			// 102: 可见输入框 (x: 100~300, y: 50~80, w: 200, h: 30)
			102: {100, 50, 300, 50, 300, 80, 100, 80},
		},
	}

	match, err := SearchSemantic(ctx, caller, "search_box")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match == nil {
		t.Fatalf("expected match, got nil")
	}
	if match.NodeID != 102 {
		t.Errorf("expected NodeID 102, got %d", match.NodeID)
	}
	if match.Query != "search" {
		t.Errorf("expected Query 'search', got %q", match.Query)
	}
	if match.X != 200 || match.Y != 65 {
		t.Errorf("expected center (200, 65), got (%f, %f)", match.X, match.Y)
	}
	if match.Width != 200 || match.Height != 30 {
		t.Errorf("expected size (200, 30), got (%f, %f)", match.Width, match.Height)
	}

	// 确认 searchId 被 discard
	if len(caller.discarded) != 1 || caller.discarded[0] != "sid-search" {
		t.Errorf("expected sid-search discarded, got %v", caller.discarded)
	}
}

func TestSearchSemantic_ConcreteLiteral(t *testing.T) {
	ctx := context.Background()
	literal := "History of the World Wide Web"
	caller := &mockCDPCaller{
		searchResults: map[string]struct {
			id    string
			count int
		}{
			literal: {id: "sid-wiki", count: 1},
		},
		searchNodes: map[string][]int64{
			"sid-wiki": {279},
		},
		nodeBoxes: map[int64][]float64{
			// y = 4017, w = 320, h = 24
			279: {150, 4005, 470, 4005, 470, 4029, 150, 4029},
		},
	}

	match, err := SearchSemantic(ctx, caller, literal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match == nil {
		t.Fatalf("expected match, got nil")
	}
	if match.NodeID != 279 {
		t.Errorf("expected NodeID 279, got %d", match.NodeID)
	}
	if match.Y != 4017 {
		t.Errorf("expected Y 4017, got %f", match.Y)
	}
}

func TestSearchSemantic_NoMatchFailSoft(t *testing.T) {
	ctx := context.Background()
	caller := &mockCDPCaller{
		searchResults: map[string]struct {
			id    string
			count int
		}{},
	}
	match, err := SearchSemantic(ctx, caller, "nonexistent_target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match != nil {
		t.Errorf("expected nil match, got %+v", match)
	}
}

func TestSearchSemantic_EmptyInputs(t *testing.T) {
	ctx := context.Background()
	match, err := SearchSemantic(ctx, nil, "test")
	if err != nil || match != nil {
		t.Errorf("expected (nil, nil) for nil caller, got (%v, %v)", match, err)
	}
	caller := &mockCDPCaller{}
	match, err = SearchSemantic(ctx, caller, "")
	if err != nil || match != nil {
		t.Errorf("expected (nil, nil) for empty semantic, got (%v, %v)", match, err)
	}
}
