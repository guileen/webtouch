package browser

import (
	"bytes"
	"context"
	"fmt"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPageSnapshot_Phase0_CoarsePointBootstrap 验收 Phase 0：
// Wikipedia 真实长页面场景下，无锚点时目标因文档序截断（K=64）丢失（在第 279 位）；
// 启用 CDP DOM.performSearch 粗锚点自举后，目标稳定排进 Top-64（且跃升至 Top-1），
// 且严格遵守 §5.0.16 条款 3：粗锚点绝不污染 GroundTruthIndex（保持 -1）。
func TestPageSnapshot_Phase0_CoarsePointBootstrap(t *testing.T) {
	requireCDP(t)

	const targetText = "History of the World Wide Web"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		sb.WriteString(`<!DOCTYPE html><html><head><style>
			body { margin: 0; padding: 0; font-family: sans-serif; }
			.item { display: block; height: 18px; margin-bottom: 2px; }
			.deep { margin-top: 2500px; height: 30px; display: block; }
		</style></head><body>`)
		// 构造前 278 个干扰候选节点（占据顶部区域）
		for i := 0; i < 278; i++ {
			fmt.Fprintf(&sb, `<a href="/item/%d" class="item">Wiki Navigation Item %d</a>`, i, i)
		}
		// 第 279 个候选：深水区目标
		fmt.Fprintf(&sb, `<a href="/wiki/History_of_the_World_Wide_Web" class="deep">%s</a>`, targetText)
		// 后面再跟 20 个干扰候选
		for i := 280; i < 300; i++ {
			fmt.Fprintf(&sb, `<a href="/item/%d" class="item">Wiki Footer Item %d</a>`, i, i)
		}
		sb.WriteString(`</body></html>`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, sb.String())
	}))
	defer srv.Close()

	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// ── 场景 1：无任何意图和锚点（旧行为对照）──
	// 期望：文档预序遍历截断到 64 条，目标在第 279 位被物理丢弃
	snapOld, ok := s.PageSnapshot(PageSnapshotPivot{})
	if !ok {
		t.Fatalf("PageSnapshot old failed")
	}
	if !snapOld.Truncated {
		t.Errorf("expected Truncated=true, got false")
	}
	if len(snapOld.Nodes) != 64 {
		t.Errorf("expected 64 kept nodes, got %d", len(snapOld.Nodes))
	}
	foundInOld := false
	for _, n := range snapOld.Nodes {
		if strings.Contains(n.Text, targetText) {
			foundInOld = true
			break
		}
	}
	if foundInOld {
		t.Fatalf("对照组异常：未提供锚点时目标竟然出现在前 64 条内")
	}
	if snapOld.GroundTruthIndex != -1 {
		t.Errorf("expected GT -1, got %d", snapOld.GroundTruthIndex)
	}

	// ── 场景 2：Phase 0 CDP 原生检索粗锚点自举（具象文本）──
	// 期望：DOM.performSearch 命中该节点，提取粗锚点坐标恢复欧氏距离，目标晋升为 Top-1（排在 index 0）
	snapNew, ok := s.PageSnapshot(PageSnapshotPivot{
		TargetSemantic: targetText,
	})
	if !ok {
		t.Fatalf("PageSnapshot new failed")
	}
	if !snapNew.Truncated {
		t.Errorf("expected Truncated=true, got false")
	}
	if len(snapNew.Nodes) != 64 {
		t.Errorf("expected 64 kept nodes, got %d", len(snapNew.Nodes))
	}
	targetRank := -1
	for idx, n := range snapNew.Nodes {
		if strings.Contains(n.Text, targetText) {
			targetRank = idx
			break
		}
	}
	if targetRank < 0 {
		t.Fatalf("Phase 0 自举失败：目标 %q 未能进入 Top-64 候选池", targetText)
	}
	t.Logf("Phase 0 验收成功：目标 %q 从第 279 位跃升至 Top-%d（位序 index %d）", targetText, targetRank+1, targetRank)
	if targetRank != 0 {
		t.Errorf("expected target at rank 0 (Top-1), got rank %d", targetRank)
	}

	// §5.0.16 条款 3② 守卫核验：粗锚点绝不作为 GT（GT 必须保持 -1，绝不污染训练集）
	if snapNew.GroundTruthIndex != -1 {
		t.Fatalf("红线违规：粗锚点自举不得标记 GT，实际 GroundTruthIndex=%d", snapNew.GroundTruthIndex)
	}
}

// TestLongPageScreenshotRouteUsesOffscreenCaptureThenSelectedScroll verifies that
// document slices are captured without moving the page, then only an explicit
// selection moves the viewport.
func TestLongPageScreenshotRouteUsesOffscreenCaptureThenSelectedScroll(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body style="margin:0;height:4200px;background:#2463eb">
			<div style="position:absolute;top:2600px;left:0;width:100%;height:1600px;background:#dc2626"></div>
		</body></html>`)
	}))
	defer srv.Close()

	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	vw, vh, _, docH, err := s.DocumentMetrics(ctx)
	if err != nil {
		t.Fatalf("document metrics: %v", err)
	}
	if docH <= 3*vh {
		t.Fatalf("fixture must exceed three viewports: document=%d viewport=%d", docH, vh)
	}
	var initialY int
	if err := s.evalValue(ctx, "window.scrollY", &initialY); err != nil {
		t.Fatal(err)
	}
	if initialY != 0 {
		t.Fatalf("fixture unexpectedly starts scrolled: y=%d", initialY)
	}

	y := 2800
	shot, err := s.CaptureDocumentRegion(ctx, 0, y, vw, vh)
	if err != nil {
		t.Fatalf("capture offscreen region: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(shot))
	if err != nil {
		t.Fatalf("decode screenshot: %v", err)
	}
	if img.Bounds().Dx() != vw || img.Bounds().Dy() != vh {
		t.Fatalf("expected CSS-sized document crop %dx%d, got %v", vw, vh, img.Bounds())
	}
	r, g, b, _ := img.At(10, 10).RGBA()
	want := color.RGBA{R: 220, G: 38, B: 38, A: 255}
	if uint8(r>>8) != want.R || uint8(g>>8) != want.G || uint8(b>>8) != want.B {
		t.Fatalf("offscreen crop sampled wrong document region: got #%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
	}
	if err := s.evalValue(ctx, "window.scrollY", &initialY); err != nil || initialY != 0 {
		t.Fatalf("read-only capture moved page: scrollY=%d err=%v", initialY, err)
	}

	if err := s.ScrollDocumentTo(ctx, y); err != nil {
		t.Fatalf("scroll selected region into view: %v", err)
	}
	var selectedY int
	if err := s.evalValue(ctx, "window.scrollY", &selectedY); err != nil {
		t.Fatal(err)
	}
	if selectedY != y {
		t.Fatalf("expected selected slice y=%d in viewport, got scrollY=%d", y, selectedY)
	}
}

// TestPageSnapshot_Phase0_AliasExpansion 验收别名展开与宏标签粗锚点自举
func TestPageSnapshot_Phase0_AliasExpansion(t *testing.T) {
	requireCDP(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		sb.WriteString(`<!DOCTYPE html><html><head><style>
			.item { display: block; height: 18px; margin-bottom: 2px; }
			.search-box { margin-top: 3000px; width: 220px; height: 32px; display: block; }
		</style></head><body>`)
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&sb, `<button class="item">Button %d</button>`, i)
		}
		// 深水区搜索框，文案为 "Search"（命中 search_box 别名集）
		fmt.Fprintf(&sb, `<input type="text" placeholder="Search" class="search-box" />`)
		sb.WriteString(`</body></html>`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, sb.String())
	}))
	defer srv.Close()

	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// 以宏标签 "search_box" 触发，别名集展开命中 placeholder="Search"
	snap, ok := s.PageSnapshot(PageSnapshotPivot{
		TargetSemantic: "search_box",
	})
	if !ok {
		t.Fatalf("PageSnapshot failed")
	}
	found := false
	for idx, n := range snap.Nodes {
		if n.Tag == "input" && strings.Contains(n.Text, "Search") {
			found = true
			t.Logf("search_box 别名命中成功排在 index %d", idx)
			break
		}
	}
	if !found {
		t.Fatalf("search_box 别名自举未能将深水区 input 排入 Top-64")
	}
}

// TestPageSnapshot_Phase1_LandmarkQuotaAndStratification 验收 §5.0.17 条款 1/3：
// 无锚点时，主内容区 <main> 保留 48/64 配额，并进行纵向空间等距分层采样，
// 确保深水区（y > 4000px）候选稳定出现在快照内，消灭顶部 10.7% 截断塌陷。
func TestPageSnapshot_Phase1_LandmarkQuotaAndStratification(t *testing.T) {
	requireCDP(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		sb.WriteString(`<!DOCTYPE html><html><head><style>
			body { margin: 0; padding: 0; font-family: sans-serif; }
			.nav-link { display: block; height: 16px; margin: 1px; }
			.main-item { display: block; height: 20px; }
		</style></head><body>`)
		// 1. 顶部导航与页眉（100 个外围链接，占据 y = 0 ~ 400px）
		sb.WriteString(`<header><nav>`)
		for i := 0; i < 100; i++ {
			fmt.Fprintf(&sb, `<a href="/nav/%d" class="nav-link">Top Nav Link %d</a>`, i, i)
		}
		sb.WriteString(`</nav></header>`)

		// 2. 主内容区 <main>（200 个项目，垂直跨度从 y = 500px 延伸至 y = 5000px）
		sb.WriteString(`<main style="margin-top: 100px;">`)
		for i := 0; i < 200; i++ {
			topOffset := i * 22
			fmt.Fprintf(&sb, `<div style="position: absolute; top: %dpx;"><button class="main-item">Main Content Action %d</button></div>`, 500+topOffset, i)
		}
		sb.WriteString(`</main>`)

		sb.WriteString(`</body></html>`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, sb.String())
	}))
	defer srv.Close()

	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// 无锚点（完全无受体、无粗锚点、无语义）
	snap, ok := s.PageSnapshot(PageSnapshotPivot{})
	if !ok {
		t.Fatalf("PageSnapshot failed")
	}

	if len(snap.Nodes) != 64 {
		t.Fatalf("expected exactly 64 nodes, got %d", len(snap.Nodes))
	}

	var mainCount, otherCount int
	var minY, maxY float64
	minY = 999999
	for _, n := range snap.Nodes {
		if n.Landmark == "main" {
			mainCount++
			if n.Y < minY {
				minY = n.Y
			}
			if n.Y > maxY {
				maxY = n.Y
			}
		} else {
			otherCount++
		}
	}

	t.Logf("Phase 1 配额验证：mainCount=%d, otherCount=%d, mainYSpan=[%.1f, %.1f]", mainCount, otherCount, minY, maxY)

	// 条款 1 判据：main 区保底配额 48，other 区配额 16
	if mainCount != 48 {
		t.Errorf("expected 48 main candidates, got %d", mainCount)
	}
	if otherCount != 16 {
		t.Errorf("expected 16 other candidates, got %d", otherCount)
	}

	// 条款 3 判据：分层采样必须覆盖全深度，最大 Y 必须进入深水区 (>4000px)
	if maxY < 4000 {
		t.Errorf("深水区覆盖失效：最大 Y 仅为 %.1f，未深入 >4000px 区域", maxY)
	}
	if maxY-minY < 3500 {
		t.Errorf("纵深跨度不足：跨度仅为 %.1f", maxY-minY)
	}
}

// TestPageSnapshot_Phase1_HeadingAnchorBootstrap 验收 §5.0.17 条款 2：
// 标题树提取与目标语义匹配自举空间路标锚点，恢复局部性排序，且保持 GT=-1。
func TestPageSnapshot_Phase1_HeadingAnchorBootstrap(t *testing.T) {
	requireCDP(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		sb.WriteString(`<!DOCTYPE html><html><head><style>
			body { margin: 0; padding: 0; font-family: sans-serif; }
			.btn { display: block; height: 18px; margin: 2px; }
		</style></head><body>`)
		sb.WriteString(`<h1>Developer Documentation</h1>`)

		// 章节 1：顶部干扰
		sb.WriteString(`<h2>Getting Started</h2>`)
		for i := 0; i < 50; i++ {
			fmt.Fprintf(&sb, `<button class="btn">Getting Started Link %d</button>`, i)
		}

		// 章节 2：位于深水区 y = 3500px 的 Configuration Guide
		sb.WriteString(`<div style="margin-top: 3000px;">`)
		sb.WriteString(`<h2>Configuration Guide</h2>`)
		sb.WriteString(`<button class="btn" id="target-btn">Edit Core Settings</button>`)
		sb.WriteString(`</div>`)

		sb.WriteString(`</body></html>`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, sb.String())
	}))
	defer srv.Close()

	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// 目标语义为章节标题 "Configuration Guide"
	snap, ok := s.PageSnapshot(PageSnapshotPivot{
		TargetSemantic: "Configuration Guide",
	})
	if !ok {
		t.Fatalf("PageSnapshot failed")
	}

	// 1. 验证标题树成功提取
	if len(snap.Headings) < 2 {
		t.Fatalf("expected at least 2 headings, got %d", len(snap.Headings))
	}
	foundHeading := false
	for _, h := range snap.Headings {
		if strings.Contains(h.Text, "Configuration Guide") {
			foundHeading = true
			if h.Level != 2 {
				t.Errorf("expected level 2 for h2, got %d", h.Level)
			}
			break
		}
	}
	if !foundHeading {
		t.Errorf("未能从快照中提取到 Configuration Guide 标题")
	}

	// 2. 验证位于该章节下的按钮跃升至 Top 排位
	targetRank := -1
	for idx, n := range snap.Nodes {
		if n.Text == "Edit Core Settings" {
			targetRank = idx
			break
		}
	}
	t.Logf("标题空间锚点自举排位：Edit Core Settings rank = %d", targetRank)
	if targetRank < 0 || targetRank > 2 {
		t.Fatalf("expected target in Top-3 under heading, got rank %d", targetRank)
	}

	// 3. 验证 GT 隔离铁律（不得污染样本）
	if snap.GroundTruthIndex != -1 {
		t.Fatalf("红线违规：标题空间路标不得标记 GT，实际 GroundTruthIndex=%d", snap.GroundTruthIndex)
	}
}
