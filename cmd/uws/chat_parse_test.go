package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseChatArgsPlain(t *testing.T) {
	opts, err := parseChatArgs([]string{"--plain", "--timeout", "25", "--img", "/tmp/a.png"})
	if err != nil {
		t.Fatalf("parseChatArgs: %v", err)
	}
	if !opts.plain {
		t.Fatalf("plain = false, want true")
	}
	if opts.timeout.Seconds() != 25 {
		t.Fatalf("timeout = %v", opts.timeout)
	}
	if len(opts.imageSrcs) != 1 {
		t.Fatalf("imageSrcs = %d, want 1", len(opts.imageSrcs))
	}
}

func TestParseChatArgsQuestionOnly(t *testing.T) {
	// `uws chat "问题" --img a` → args = ["问题", "--img", "a"]（wrapPromptArgs 已去子命令 token）
	dir := t.TempDir()
	a := filepath.Join(dir, "a.png")
	b := filepath.Join(dir, "b.png")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := parseChatArgs([]string{"对比两张图", "--img", a, "--img", b})
	if err != nil {
		t.Fatalf("parseChatArgs: %v", err)
	}
	if opts.question != "对比两张图" {
		t.Fatalf("question = %q", opts.question)
	}
	if len(opts.imageSrcs) != 2 {
		t.Fatalf("imageSrcs = %d, want 2", len(opts.imageSrcs))
	}
}

func TestParseChatArgsRequiresInput(t *testing.T) {
	if _, err := parseChatArgs([]string{}); err == nil {
		t.Fatal("want error for no input")
	}
}

func TestParseChatArgsDebugFlag(t *testing.T) {
	opts, err := parseChatArgs([]string{"发布微博：今天天气不错", "--debug"})
	if err != nil {
		t.Fatalf("parseChatArgs: %v", err)
	}
	if !opts.debug {
		t.Fatal("debug = false, want true")
	}
	if opts.question != "发布微博：今天天气不错" {
		t.Fatalf("question = %q", opts.question)
	}
}

func TestParseChatArgsEvidenceFlags(t *testing.T) {
	opts, err := parseChatArgs([]string{
		"对比差异", "--diff", "--zoom", "20,20,50,50,4", "--region-color", "1,2,3,4", "--img", "/tmp/a.png",
	})
	if err != nil {
		t.Fatalf("parseChatArgs: %v", err)
	}
	if !opts.diff {
		t.Fatal("diff = false, want true")
	}
	if opts.zoom != "20,20,50,50,4" {
		t.Fatalf("zoom = %q", opts.zoom)
	}
	if opts.regionColor != "1,2,3,4" {
		t.Fatalf("regionColor = %q", opts.regionColor)
	}
}

func TestChatSettlePromptDriven(t *testing.T) {
	cases := []struct {
		q      string
		wantMs int
	}{
		{"对比两个实现是否一致", 600},
		{"对比参考站与线上站落地页", 600},
		{"对比这两个站点的详情页，等 2 秒再截图", 2000},
		{"wait 1.5s 之后再截图对比 video", 1500},
		{"settle 2 再看评论区", 2000},
		{"对比详情播放态（视频、播放器）布局", 1200},
		{"看看剧集列表是否一致", 1200},
		{"等 99 秒（超上限，忽略显式值）", 600},
	}
	for _, c := range cases {
		got := chatSettle(c.q)
		if int(got.Milliseconds()) != c.wantMs {
			t.Errorf("chatSettle(%q) = %dms, want %dms", c.q, got.Milliseconds(), c.wantMs)
		}
	}
}

func TestParseRect(t *testing.T) {
	cases := []struct {
		in    string
		x, y  int
		w, h  int
		scale int
	}{
		{"10,20,30,40", 10, 20, 30, 40, 1},
		{"10,20,30,40,3", 10, 20, 30, 40, 3},
	}
	for _, c := range cases {
		x, y, w, h, scale, err := parseRect(c.in)
		if err != nil {
			t.Fatalf("parseRect(%q): %v", c.in, err)
		}
		if x != c.x || y != c.y || w != c.w || h != c.h || scale != c.scale {
			t.Fatalf("parseRect(%q) = %d,%d %dx%d x%d, want %d,%d %dx%d x%d",
				c.in, x, y, w, h, scale, c.x, c.y, c.w, c.h, c.scale)
		}
	}
	if _, _, _, _, _, err := parseRect("bad"); err == nil {
		t.Fatal("want error for bad rect")
	}
}

func TestExtractSiteURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "bare domains with CJK connective",
			in:   "帮我检查 a.com 和 b.com 的 differences",
			want: []string{"https://a.com", "https://b.com"},
		},
		{
			name: "full scheme URLs",
			in:   "对比 https://example.org 和 https://sample.com 首页",
			want: []string{"https://example.org", "https://sample.com"},
		},
		{
			name: "scheme URL not duplicated by bare domain",
			in:   "https://a.com 和 a.com 的差异",
			want: []string{"https://a.com"},
		},
		{
			name: "no URLs",
			in:   "这个页面按钮在哪",
			want: nil,
		},
		{
			name: "single URL",
			in:   "打开 a.com 看看",
			want: []string{"https://a.com"},
		},
		{
			name: "trailing punctuation trimmed",
			in:   "对比 a.com、b.com。",
			want: []string{"https://a.com", "https://b.com"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractSiteURLs(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("extractSiteURLs(%q) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("extractSiteURLs(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestDetectCompareIntent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"帮我检查 a.com 和 b.com 的 differences", true},
		{"判断两个实现是否一致", true},
		{"对比这两个站点主屏", true},
		{"分析一下这两个页面的区别", true},
		{"这个页面按钮在哪", false},
		{"打开 a.com 看看", false},
		{"VS 对比", true},
	}
	for _, c := range cases {
		if got := detectCompareIntent(c.in); got != c.want {
			t.Errorf("detectCompareIntent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestApplyNaturalLangRoute(t *testing.T) {
	// 自然语言自带 ≥2 个 URL 且比较意图 → 自动路由为站点采样。
	opts, err := parseChatArgs([]string{"帮我检查 a.com 和 b.com 的 differences"})
	if err != nil {
		t.Fatalf("parseChatArgs: %v", err)
	}
	if !applyNaturalLangRoute(&opts) {
		t.Fatal("applyNaturalLangRoute: want routed true")
	}
	if len(opts.urls) != 2 || opts.urls[0] != "https://a.com" || opts.urls[1] != "https://b.com" {
		t.Fatalf("opts.urls = %v", opts.urls)
	}

	// 单 URL、无比较意图 → 不路由。
	opts, err = parseChatArgs([]string{"打开 a.com 看看"})
	if err != nil {
		t.Fatalf("parseChatArgs: %v", err)
	}
	if applyNaturalLangRoute(&opts) {
		t.Fatal("applyNaturalLangRoute: want not routed for single no-compare URL")
	}
	if len(opts.urls) != 0 {
		t.Fatalf("opts.urls = %v, want empty", opts.urls)
	}

	// 已显式给 --img → 不再自动路由。
	opts, err = parseChatArgs([]string{"对比站点", "--img", "/tmp/a.png"})
	if err != nil {
		t.Fatalf("parseChatArgs: %v", err)
	}
	if applyNaturalLangRoute(&opts) {
		t.Fatal("applyNaturalLangRoute: want not routed when explicit --img")
	}
}

func TestParseChatArgsOutDir(t *testing.T) {
	opts, err := parseChatArgs([]string{"对比", "--out-dir", "/tmp/eye-shots", "--img", "/tmp/a.png"})
	if err != nil {
		t.Fatalf("parseChatArgs: %v", err)
	}
	if opts.outDir != "/tmp/eye-shots" {
		t.Fatalf("outDir = %q, want /tmp/eye-shots", opts.outDir)
	}
}

func TestSafeLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"参考站 https://drama.b25reel.com/x", "参考站 https___drama.b25reel.com_x"},
		{"图1", "图1"},
		{"像素差异图", "像素差异图"},
		{"a/b\\c", "a_b_c"},
		{"区域放大(20,20 30x40 x4)", "区域放大(20,20 30x40 x4)"},
	}
	for _, c := range cases {
		if got := safeLabel(c.in); got != c.want {
			t.Errorf("safeLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseViewport(t *testing.T) {
	w, h, err := parseViewport("390x844")
	if err != nil {
		t.Fatalf("parseViewport: %v", err)
	}
	if w != 390 || h != 844 {
		t.Fatalf("parseViewport = %dx%d, want 390x844", w, h)
	}
	if _, _, err := parseViewport("bad"); err == nil {
		t.Fatal("want error for bad viewport")
	}
	if _, _, err := parseViewport("0x0"); err == nil {
		t.Fatal("want error for non-positive viewport")
	}
}
