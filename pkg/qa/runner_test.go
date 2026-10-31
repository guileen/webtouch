package qa

import (
	"context"
	"encoding/json"
	"image/color"
	"os"
	"strings"
	"testing"
)

type mockBrowserSession struct {
	navigatedURL string
	viewports    []Viewport
	mockMetrics  map[string]any
	shotBytes    []byte
	closed       bool
}

func (m *mockBrowserSession) Navigate(ctx context.Context, url string) error {
	m.navigatedURL = url
	return nil
}

func (m *mockBrowserSession) SetViewport(ctx context.Context, width, height int, scale float64, mobile bool) error {
	m.viewports = append(m.viewports, Viewport{
		Width:  width,
		Height: height,
		Scale:  scale,
		Mobile: mobile,
	})
	return nil
}

func (m *mockBrowserSession) EvaluateScript(ctx context.Context, expr string) ([]byte, error) {
	if strings.Contains(expr, "window.__uws_qa_hooked") {
		return []byte("true"), nil
	}
	return json.Marshal(m.mockMetrics)
}

func (m *mockBrowserSession) CaptureScreenshot(ctx context.Context) ([]byte, int, int, error) {
	return m.shotBytes, 100, 100, nil
}

func (m *mockBrowserSession) Close() error {
	m.closed = true
	return nil
}

func TestQARunnerWithMockSession(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "qa_runner_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	testPNG := createTestPNG(100, 100, color.RGBA{50, 100, 150, 255})
	refPNG := createTestPNG(100, 100, color.RGBA{50, 100, 150, 255})

	mockSession := &mockBrowserSession{
		shotBytes: testPNG,
		mockMetrics: map[string]any{
			"scroll_width":            390,
			"inner_width":             390,
			"has_horizontal_overflow": false,
			"overflow_delta":          0,
			"overflow_elements":       []OverflowElement{},
			"small_targets":           []SmallTarget{},
			"broken_resources":        []string{},
			"console_errors":          []ConsoleError{},
		},
	}

	progressEvents := 0
	opts := RunnerOptions{
		URL:       "http://localhost:3000/preview",
		Viewports: []Viewport{ViewportMobile, ViewportTablet, ViewportDesktop},
		RefImage:  refPNG,
		OutputDir: tmpDir,
		OnProgress: func(step, total int, msg string) {
			progressEvents++
		},
		SessionOverride: mockSession,
	}

	runner := NewRunner(opts)
	ctx := context.Background()

	report, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("runner.Run failed: %v", err)
	}

	if !mockSession.closed {
		t.Errorf("expected mockSession to be closed")
	}
	if mockSession.navigatedURL != opts.URL {
		t.Errorf("navigatedURL = %s, want %s", mockSession.navigatedURL, opts.URL)
	}
	if len(mockSession.viewports) != 3 {
		t.Errorf("expected 3 viewport switches, got %d", len(mockSession.viewports))
	}
	if report.HealthScore != 100 || report.Verdict != VerdictPass {
		t.Errorf("expected PASS with 100 score, got verdict=%s, score=%d", report.Verdict, report.HealthScore)
	}
	if progressEvents == 0 {
		t.Errorf("expected progress events to be emitted")
	}

	// Verify screenshots and reports exist
	for _, vp := range report.Viewports {
		if _, err := os.Stat(vp.ScreenshotPath); err != nil {
			t.Errorf("missing screenshot file: %s", vp.ScreenshotPath)
		}
		if _, err := os.Stat(vp.DiffPath); err != nil {
			t.Errorf("missing diff file: %s", vp.DiffPath)
		}
	}
	if _, err := os.Stat(report.HTMLReport); err != nil {
		t.Errorf("missing HTML report: %s", report.HTMLReport)
	}
	if _, err := os.Stat(report.MDReport); err != nil {
		t.Errorf("missing Markdown report: %s", report.MDReport)
	}
}

func TestQARunnerDetectsOverflow(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "qa_runner_test_overflow_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	testPNG := createTestPNG(100, 100, color.RGBA{200, 50, 50, 255})

	mockSession := &mockBrowserSession{
		shotBytes: testPNG,
		mockMetrics: map[string]any{
			"scroll_width":            450,
			"inner_width":             390,
			"has_horizontal_overflow": true,
			"overflow_delta":          60,
			"overflow_elements": []OverflowElement{
				{Tag: "div", ClassName: "overflowing-banner", Width: 450, Right: 450, OverflowPx: 60},
			},
			"small_targets":    []SmallTarget{},
			"broken_resources": []string{},
			"console_errors":   []ConsoleError{},
		},
	}

	opts := RunnerOptions{
		URL:             "http://localhost:3000/bad-page",
		Viewports:       []Viewport{ViewportMobile},
		OutputDir:       tmpDir,
		SessionOverride: mockSession,
	}

	runner := NewRunner(opts)
	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("runner.Run failed: %v", err)
	}

	if report.Verdict != VerdictCritical {
		t.Errorf("expected VerdictCritical for overflow, got %s", report.Verdict)
	}
	if report.HealthScore > 70 {
		t.Errorf("expected score <= 70, got %d", report.HealthScore)
	}
	if len(report.SummaryIssues) == 0 {
		t.Errorf("expected overflow issue in SummaryIssues")
	}
}
