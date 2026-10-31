package vlm

import (
	"path/filepath"
	"testing"
)

func TestLearnedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "learned.json")
	s, err := NewLearnedStore(path)
	if err != nil {
		t.Fatal(err)
	}
	u := "https://myread.top/store?utm=1#frag"
	sem := "Godfall"
	if err := s.Learn(u, sem, LearnedEntry{Selector: "div.feed:nth-of-type(2) > div.title", NodeText: "Godfall: Helios' Inverse Scale"}); err != nil {
		t.Fatal(err)
	}
	// 重新打开：持久化 + URL 归一化（query/fragment 不参与键）
	s2, err := NewLearnedStore(path)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := s2.Lookup("https://myread.top/store", sem)
	if !ok {
		t.Fatal("lookup after reload: not found")
	}
	if e.Selector != "div.feed:nth-of-type(2) > div.title" {
		t.Fatalf("selector = %q", e.Selector)
	}
	if e.Hits != 1 {
		t.Fatalf("hits = %d", e.Hits)
	}
	// 命中累加
	if err := s2.Learn(u, sem, LearnedEntry{Selector: e.Selector}); err != nil {
		t.Fatal(err)
	}
	e2, _ := s2.Lookup(u, sem)
	if e2.Hits != 2 {
		t.Fatalf("hits after learn = %d", e2.Hits)
	}
}

// TestLearnedForget 固化「失效条目删除」口径（§5.0.9 条款 6 复用路径判 miss 后清条目）：
// 删除后 Lookup 必须落空、且落盘文件同步更新（另一进程重开也看不到）。
func TestLearnedForget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "learned.json")
	s, err := NewLearnedStore(path)
	if err != nil {
		t.Fatal(err)
	}
	u, sem := "https://myread.top/store", "Godfall"
	if err := s.Learn(u, sem, LearnedEntry{Selector: "div.title"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(u, sem); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(u, sem); ok {
		t.Fatal("Forget 后 Lookup 应落空")
	}
	s2, err := NewLearnedStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Lookup(u, sem); ok {
		t.Fatal("Forget 应同步落盘：重开后仍应落空")
	}
	// 幂等：删不存在的条目不算错。
	if err := s.Forget(u, sem); err != nil {
		t.Fatalf("重复 Forget 不应报错: %v", err)
	}
}

func TestLearnedLookupMiss(t *testing.T) {
	s, _ := NewLearnedStore("")
	if _, ok := s.Lookup("https://example.com", "x"); ok {
		t.Fatal("want miss")
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"https://myread.top/store?a=1#x": "https://myread.top/store",
		"https://myread.top/store":       "https://myread.top/store",
		"not-a-url":                      "not-a-url",
	}
	for in, want := range cases {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}
