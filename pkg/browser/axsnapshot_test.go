package browser

import (
	"encoding/json"
	"strings"
	"testing"
)

func val(t *testing.T, s string) axValueObj {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return axValueObj{Type: "string", Value: raw}
}

func propVal(t *testing.T, b bool) axValueObj {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return axValueObj{Type: "boolean", Value: raw}
}

// 模拟 Google 结果页形态：导航 + 标题(heading) + 结果链接 + 摘要文本。
func TestSerializeAXContentKeepsSemanticRows(t *testing.T) {
	nodes := []axNodeRaw{
		{NodeID: "1", Role: val(t, "generic")},
		{NodeID: "2", ParentID: "1", Role: val(t, "navigation"), Name: val(t, "主导航"),
			ChildIDs: []string{"3"}},
		{NodeID: "3", ParentID: "2", Role: val(t, "link"), Name: val(t, "Google 首页")},
		{NodeID: "4", ParentID: "1", Role: val(t, "main"), ChildIDs: []string{"5", "7"}},
		{NodeID: "5", ParentID: "4", Role: val(t, "heading"), Name: val(t, "today news 搜索结果"),
			Properties: []axPropRaw{{Name: "level", Value: val(t, "2")}},
			ChildIDs:   []string{"6"}},
		{NodeID: "6", ParentID: "5", Role: val(t, "staticText"), Name: val(t, "today news 搜索结果")},
		{NodeID: "7", ParentID: "4", Role: val(t, "link"), Name: val(t, "今日要闻：某某突发新闻 - 新闻来源"),
			Properties: []axPropRaw{
				{Name: "disabled", Value: propVal(t, false)},
				{Name: "url", Value: val(t, "https://news.example/today")},
			}},
		{NodeID: "8", ParentID: "1", Role: val(t, "generic"), ChildIDs: []string{"9"}},
		{NodeID: "9", ParentID: "8", Role: val(t, "staticText"), Name: val(t, "页面摘要正文内容。")},
		{NodeID: "10", ParentID: "1", Role: val(t, "generic"), Name: val(t, "hidden"), Ignored: true,
			ChildIDs: []string{"11"}},
		{NodeID: "11", ParentID: "10", Role: val(t, "link"), Name: val(t, "不应出现"), Ignored: true},
	}
	got := serializeAXContent(nodes)
	for _, want := range []string{
		`- heading "today news 搜索结果" [level=2]`,
		`- link "今日要闻：某某突发新闻 - 新闻来源"`,
		`[href=https://news.example/today]`,
		`- navigation "主导航"`,
		`- text "页面摘要正文内容。"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("期望内容树包含 %q，实际:\n%s", want, got)
		}
	}
	if strings.Contains(got, "不应出现") {
		t.Fatalf("ignored 子树不应输出:\n%s", got)
	}
	// heading 可访问名已含 staticText，不应重复输出子文本行。
	if strings.Count(got, "today news 搜索结果") > 1 {
		t.Fatalf("staticText 与 heading 名重复输出:\n%s", got)
	}
}

func TestSerializeAXContentEmpty(t *testing.T) {
	if got := serializeAXContent([]axNodeRaw{{NodeID: "1", Ignored: true}}); got != "" {
		t.Fatalf("全忽略树应为空，got %q", got)
	}
}
