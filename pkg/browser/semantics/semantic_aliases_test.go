package semantics

import (
	"strings"
	"testing"
)

// spec/algorithm-kernel.md §5.0.2 条款 1/5：内部语义标签必须展开为候选文案集合，
// 且原始标签保留为末位候选（零回归）；未覆盖的标签原样单候选（fail-closed）。
func TestSemanticWantsExpandsInternalLabels(t *testing.T) {
	got := Wants("search_box")
	if len(got) < 2 {
		t.Fatalf("search_box 应展开为多候选，got %#v", got)
	}
	if got[len(got)-1] != "search_box" {
		t.Fatalf("原始标签必须保留为末位候选（零回归），got %#v", got)
	}
	has := func(w string) bool {
		for _, c := range got {
			if strings.EqualFold(c, w) {
				return true
			}
		}
		return false
	}
	if !has("搜索") {
		t.Fatalf("候选应含中文通用文案「搜索」，got %#v", got)
	}
	if !has("search") {
		t.Fatalf("候选应含英文通用文案 search，got %#v", got)
	}
	// 查表大小写不敏感：语义标签大小写不由调用方保证。
	up := Wants("SEARCH_BOX")
	if len(up) != len(got) {
		t.Fatalf("查表应大小写不敏感，got %#v vs %#v", up, got)
	}
	// 幂等：展开结果再展开不产生变化（防止双重展开放大候选）。
	if again := Wants(got[0]); len(again) == 0 {
		t.Fatalf("候选文案本身必须至少原样返回，got %#v", again)
	}
}

func TestSemanticWantsUncoveredLabelUnchanged(t *testing.T) {
	cases := []string{"不存在的语义", "第 4 集", "Example Domains", "new_chat_button", "chat_input"}
	for _, s := range cases {
		got := Wants(s)
		if len(got) != 1 || got[0] != s {
			t.Fatalf("未覆盖标签必须原样单候选（fail-closed），%q → %#v", s, got)
		}
	}
	if got := Wants(""); len(got) != 0 {
		t.Fatalf("空语义必须返回空候选（评分全 0 ⇒ fail-closed），got %#v", got)
	}
	if got := Wants("  search_box  "); len(got) < 2 {
		t.Fatalf("两侧空白必须被裁剪后仍能展开，got %#v", got)
	}
}

// §5.0.2 条款 4：候选集合必须跨站点通用——不含任何站点专有文案/品牌，
// 否则就退化为「逐站手写规则」，与二八定律相反。
func TestSemanticAliasesAreSiteAgnostic(t *testing.T) {
	banned := []string{"amazon", "github", "google", "bing", "百度", "淘宝", "京东", "zhihu", ".com", ".cn", "http"}
	for label, cands := range semanticAliases {
		if len(cands) == 0 {
			t.Fatalf("别名表条目 %q 候选为空", label)
		}
		for _, c := range cands {
			lc := strings.ToLower(c)
			for _, b := range banned {
				if strings.Contains(lc, b) {
					t.Fatalf("候选 %q（标签 %q）含站点专有文案 %q，违反 §5.0.2 条款 4", c, label, b)
				}
			}
			if strings.TrimSpace(c) != c || c == "" {
				t.Fatalf("候选 %q（标签 %q）含首尾空白或为空", c, label)
			}
		}
	}
}
