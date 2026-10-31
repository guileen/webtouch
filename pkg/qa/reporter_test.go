package qa

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uws/pkg/vlm"
)

func createTestPNG(w, h int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestCalculateScoreAndVerdict(t *testing.T) {
	// Case 1: Perfect pass
	vpClean := []ViewportAuditResult{
		{
			Viewport:              ViewportMobile,
			HasHorizontalOverflow: false,
		},
		{
			Viewport:              ViewportDesktop,
			HasHorizontalOverflow: false,
		},
	}
	score, verdict, issues := CalculateScoreAndVerdict(vpClean, false)
	if score != 100 || verdict != VerdictPass || len(issues) != 0 {
		t.Errorf("Clean viewports got score=%d, verdict=%s, issues=%v", score, verdict, issues)
	}

	// Case 2: Horizontal overflow triggers CRITICAL
	vpOverflow := []ViewportAuditResult{
		{
			Viewport:              ViewportMobile,
			HasHorizontalOverflow: true,
			OverflowDelta:         25,
		},
	}
	score2, verdict2, issues2 := CalculateScoreAndVerdict(vpOverflow, false)
	if score2 > 70 || verdict2 != VerdictCritical || len(issues2) == 0 {
		t.Errorf("Overflow viewport got score=%d, verdict=%s, issues=%v", score2, verdict2, issues2)
	}

	// Case 3: Console errors trigger WARNING
	vpErrors := []ViewportAuditResult{
		{
			Viewport: ViewportDesktop,
			ConsoleErrors: []ConsoleError{
				{Type: "error", Message: "Uncaught TypeError: cannot read properties of undefined"},
			},
		},
	}
	score3, verdict3, issues3 := CalculateScoreAndVerdict(vpErrors, false)
	if score3 != 90 || verdict3 != VerdictWarning || len(issues3) == 0 {
		t.Errorf("Console error viewport got score=%d, verdict=%s, issues=%v", score3, verdict3, issues3)
	}
}

func TestComputeDiff(t *testing.T) {
	imgA := createTestPNG(100, 100, color.RGBA{255, 255, 255, 255})
	imgB := createTestPNG(100, 100, color.RGBA{255, 255, 255, 255})
	// Identical images
	diffPng, stats, err := ComputeDiff(imgA, imgB)
	if err != nil {
		t.Fatalf("ComputeDiff failed: %v", err)
	}
	if stats.DiffPixels != 0 || stats.Ratio != 0 {
		t.Errorf("expected 0 diff, got diff_pixels=%d, ratio=%f", stats.DiffPixels, stats.Ratio)
	}
	if len(diffPng) == 0 {
		t.Errorf("expected diff png bytes")
	}

	// Different images
	imgC := createTestPNG(100, 100, color.RGBA{0, 0, 0, 255})
	_, stats2, err2 := ComputeDiff(imgA, imgC)
	if err2 != nil {
		t.Fatalf("ComputeDiff failed: %v", err2)
	}
	if stats2.DiffPixels != 10000 || stats2.Ratio != 1.0 {
		t.Errorf("expected 100%% diff, got %f", stats2.Ratio)
	}
}

func TestRenderReports(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "qa_test_report_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	shot := createTestPNG(50, 50, color.RGBA{100, 150, 200, 255})
	shotPath := filepath.Join(tmpDir, "shot.png")
	if err := os.WriteFile(shotPath, shot, 0644); err != nil {
		t.Fatal(err)
	}

	report := &QAReport{
		URL:         "http://localhost:3000",
		Timestamp:   time.Now(),
		DurationMs:  1250,
		HealthScore: 95,
		Verdict:     VerdictPass,
		Viewports: []ViewportAuditResult{
			{
				Viewport:        ViewportMobile,
				ScreenshotBytes: shot,
				ScreenshotPath:  shotPath,
				ScrollWidth:     390,
				InnerWidth:      390,
				DiffStats: &vlm.DiffStats{
					Ratio: 0.01,
					BBox:  "10,10,20,20",
				},
				DiffBytes: shot,
				DiffPath:  filepath.Join(tmpDir, "diff.png"),
			},
		},
		SummaryIssues: []string{"交互元素热区轻微偏小"},
	}

	htmlPath, err := RenderHTMLReport(report, tmpDir)
	if err != nil {
		t.Fatalf("RenderHTMLReport failed: %v", err)
	}
	htmlContent, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(htmlContent), "WebTouch QA Studio") {
		t.Errorf("HTML report missing expected branding")
	}

	mdPath, err := RenderMarkdownReport(report, tmpDir)
	if err != nil {
		t.Fatalf("RenderMarkdownReport failed: %v", err)
	}
	mdContent, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mdContent), "WebTouch QA Studio") {
		t.Errorf("Markdown report missing expected title")
	}
}
