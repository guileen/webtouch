// ref 行 href 增强（v1.5.6，2026-09-03）：同文「文章页链接 vs 页内 # 锚点」
// 靠解析后 href 区分，VLM 不再盲点同名 node（react.dev 复现修复）。
package browser

import (
	"strings"
	"testing"
)

func TestRefRowID(t *testing.T) {
	id, ok := refRowID(`[7] a(link) "Scaling Up with Reducer and Context"`)
	if !ok || id != 7 {
		t.Fatalf("refRowID 应解析出 7，got %d ok=%v", id, ok)
	}
	if _, ok := refRowID(`- link "无 node_id 的内容行"`); ok {
		t.Fatalf("非 ref 行不应解析出 node_id")
	}
}

func TestAnnotateRefTreeHrefsKeepsTextAndAddsHref(t *testing.T) {
	tree := strings.Join([]string{
		`[7] a(link) "Scaling Up with Reducer and Context"`,
		`[12] a(link) "Scaling Up with Reducer and Context" [offscreen]`,
		`[3] button(button) "下一步"`,
		`[9] link(link) "无 href 的链接"`,
	}, "\n")
	got := annotateRefTreeHrefs(tree, map[uint32]string{
		7:  "https://react.dev/learn/scaling-up-with-reducer-and-context",
		12: "https://react.dev/learn/managing-state#scaling-up-with-reducer-and-context",
	})
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[0], `href="https://react.dev/learn/scaling-up-with-reducer-and-context"`) {
		t.Fatalf("行 1 应带文章页 href:\n%s", lines[0])
	}
	if !strings.Contains(lines[1], `href="https://react.dev/learn/managing-state#scaling-up-with-reducer-and-context"`) {
		t.Fatalf("行 2 应带锚点 href:\n%s", lines[1])
	}
	if strings.Contains(lines[2], "href=") {
		t.Fatalf("按钮行不应带 href: %s", lines[2])
	}
	if strings.Contains(lines[3], "href=") {
		t.Fatalf("无 href 的节点不应追加: %s", lines[3])
	}
	// 行内首个引号文本必须原样保留（协调器 parse_tree_nodes 依赖该格式）。
	if !strings.Contains(lines[0], `"Scaling Up with Reducer and Context"`) {
		t.Fatalf("文本引号段被破坏: %s", lines[0])
	}
}

func TestAnnotateRefTreeHrefsEscapesQuotes(t *testing.T) {
	got := annotateRefTreeHrefs(`[1] a(link) "weird"`, map[uint32]string{
		1: `https://example.com/a?q="x"`,
	})
	if !strings.Contains(got, `href="https://example.com/a?q=%22x%22"`) {
		t.Fatalf("href 引号未转义: %s", got)
	}
}
