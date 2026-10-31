package provider

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"
)

// TestParseViewports_ProviderOwnedDTO 守住 Provider 对外契约：
// 调用方拿到的是 provider 自己的视口 DTO，不需要 import pkg/qa 才能构造请求。
func TestParseViewports_ProviderOwnedDTO(t *testing.T) {
	specs, err := ParseViewports("mobile,1440x900")
	if err != nil {
		t.Fatalf("ParseViewports: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}
	if specs[0].Name != "mobile" || specs[0].Width != 390 || specs[0].Height != 844 {
		t.Errorf("mobile spec unexpected: %+v", specs[0])
	}
	if specs[1].Width != 1440 || specs[1].Height != 900 {
		t.Errorf("1440x900 spec unexpected: %+v", specs[1])
	}

	def := DefaultViewportSpecs()
	if len(def) != 2 || def[0].Name != "mobile" || def[1].Name != "desktop" {
		t.Errorf("DefaultViewportSpecs unexpected: %+v", def)
	}
}

func TestSanitizer_URL(t *testing.T) {
	s := NewSanitizer()

	raw := "https://user:password123@example.com/api?token=secret-token-abc&auth=Bearer%20123&query=golang&key=api-key-xyz"
	sanitized, count := s.SanitizeURL(raw)

	if count != 4 { // userinfo + token + auth + key
		t.Fatalf("expected 4 redacted items, got %d (url: %s)", count, sanitized)
	}
	if strings.Contains(sanitized, "password123") {
		t.Errorf("expected password123 to be removed: %s", sanitized)
	}
	if strings.Contains(sanitized, "secret-token-abc") {
		t.Errorf("expected secret-token-abc to be replaced: %s", sanitized)
	}
	if !strings.Contains(sanitized, "token=%5BREDACTED%5D") && !strings.Contains(sanitized, "token=[REDACTED]") {
		t.Errorf("expected [REDACTED] in token query: %s", sanitized)
	}
	if !strings.Contains(sanitized, "query=golang") {
		t.Errorf("expected non-sensitive query parameter 'query=golang' to be preserved: %s", sanitized)
	}
}

func TestSanitizer_Headers(t *testing.T) {
	s := NewSanitizer()

	headers := map[string]string{
		"Cookie":        "session_id=123456; uid=admin",
		"Authorization": "Bearer super-secret-jwt",
		"Content-Type":  "application/json",
		"User-Agent":    "WebTouch/1.0",
		"X-Auth-Token":  "top-secret",
	}

	clean, redacted := s.SanitizeHeaders(headers)
	if redacted != 3 {
		t.Fatalf("expected 3 redacted headers, got %d", redacted)
	}
	if _, ok := clean["Cookie"]; ok {
		t.Errorf("Cookie header should have been stripped")
	}
	if _, ok := clean["Authorization"]; ok {
		t.Errorf("Authorization header should have been stripped")
	}
	if _, ok := clean["X-Auth-Token"]; ok {
		t.Errorf("X-Auth-Token header should have been stripped")
	}
	if clean["Content-Type"] != "application/json" {
		t.Errorf("expected Content-Type to be preserved")
	}
}

func TestSanitizer_DOM(t *testing.T) {
	s := NewSanitizer()

	html := `<html><head><style>.btn { color: red; }</style><script>alert(document.cookie);</script></head><body><input type="password" value="mysecretpwd123"><img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="></body></html>`

	clean, redacted := s.SanitizeDOM(html)
	if redacted != 4 {
		t.Fatalf("expected 4 redacted items in DOM, got %d", redacted)
	}
	if strings.Contains(clean, "alert(document.cookie)") {
		t.Errorf("script contents must be redacted: %s", clean)
	}
	if strings.Contains(clean, "color: red") {
		t.Errorf("style contents must be redacted: %s", clean)
	}
	if strings.Contains(clean, "mysecretpwd123") {
		t.Errorf("password value must be redacted: %s", clean)
	}
	if strings.Contains(clean, "iVBORw0KGgo") {
		t.Errorf("base64 image payload must be redacted: %s", clean)
	}
}

// mockBrowserSession 供离线快速验证
type mockBrowserSession struct {
	overflow bool
}

func (m *mockBrowserSession) Navigate(ctx context.Context, url string) error {
	return nil
}

func (m *mockBrowserSession) SetViewport(ctx context.Context, width, height int, scale float64, mobile bool) error {
	return nil
}

func (m *mockBrowserSession) EvaluateScript(ctx context.Context, expr string) ([]byte, error) {
	if m.overflow {
		return []byte(`{
			"scroll_width": 450,
			"inner_width": 390,
			"has_horizontal_overflow": true,
			"overflow_delta": 60,
			"overflow_elements": [{"tag":"div","id":"banner","width":450,"right":450,"overflow_px":60}],
			"small_targets": [{"tag":"button","id":"btn-ok","text":"OK","width":24,"height":24}],
			"console_errors": [{"type":"error","message":"Uncaught TypeError: cannot read undefined"}],
			"broken_resources": ["https://example.com/logo.png"]
		}`), nil
	}
	return []byte(`{
		"scroll_width": 390,
		"inner_width": 390,
		"has_horizontal_overflow": false,
		"overflow_delta": 0,
		"overflow_elements": [],
		"small_targets": [],
		"console_errors": [],
		"broken_resources": []
	}`), nil
}

func (m *mockBrowserSession) CaptureScreenshot(ctx context.Context) ([]byte, int, int, error) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes(), 100, 100, nil
}

func (m *mockBrowserSession) Close() error {
	return nil
}

func TestProvider_InspectPage_Clean(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()

	mockSess := &mockBrowserSession{overflow: false}
	req := InspectRequest{
		URL:             "http://localhost:8080/landing?token=123",
		sessionOverride: mockSess,
		Viewports: []ViewportSpec{
			{Name: "mobile", Width: 390, Height: 844},
		},
		Headless: true,
	}

	bundle, err := p.InspectPage(ctx, req)
	if err != nil {
		t.Fatalf("InspectPage failed: %v", err)
	}

	if bundle.ExecutionStatus != "EXECUTED" {
		t.Errorf("expected EXECUTED, got %s", bundle.ExecutionStatus)
	}
	if bundle.Verdict != "PASS" {
		t.Errorf("expected PASS, got %s", bundle.Verdict)
	}
	if !bundle.SanityCheck.LayoutIntegrity {
		t.Errorf("expected LayoutIntegrity to be true")
	}
	if !bundle.SanityCheck.ConsoleClean {
		t.Errorf("expected ConsoleClean to be true")
	}
	if bundle.RedactedCount != 1 {
		t.Errorf("expected 1 redacted item for token query, got %d", bundle.RedactedCount)
	}
	if bundle.BundleDigestSHA == "" {
		t.Errorf("expected non-empty BundleDigestSHA")
	}
}

func TestProvider_InspectPage_WithIssues(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()

	mockSess := &mockBrowserSession{overflow: true}
	req := InspectRequest{
		URL:             "http://localhost:8080/landing",
		sessionOverride: mockSess,
		Viewports: []ViewportSpec{
			{Name: "mobile", Width: 390, Height: 844},
		},
		Headless: true,
	}

	bundle, err := p.InspectPage(ctx, req)
	if err != nil {
		t.Fatalf("InspectPage failed: %v", err)
	}

	if bundle.SanityCheck.LayoutIntegrity {
		t.Errorf("expected LayoutIntegrity to be false due to overflow")
	}
	if bundle.SanityCheck.TouchFriendly {
		t.Errorf("expected TouchFriendly to be false due to small targets")
	}
	if bundle.SanityCheck.ConsoleClean {
		t.Errorf("expected ConsoleClean to be false due to console error")
	}
	if len(bundle.SanityCheck.FlaggedIssues) == 0 {
		t.Errorf("expected flagged issues")
	}
	if bundle.HealthScore >= 100 {
		t.Errorf("expected health score degradation, got %d", bundle.HealthScore)
	}
}

func TestProvider_CaptureDiff(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()

	// 生成一张红色参考图
	refImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			refImg.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	var refBuf bytes.Buffer
	_ = png.Encode(&refBuf, refImg)

	mockSess := &mockBrowserSession{overflow: false}
	req := DiffRequest{
		URL:             "http://localhost:8080/diff-test",
		BaselineImage:   refBuf.Bytes(),
		sessionOverride: mockSess,
		Viewports: []ViewportSpec{
			{Name: "mobile", Width: 390, Height: 844},
		},
		Headless: true,
	}

	bundle, err := p.CaptureDiff(ctx, req)
	if err != nil {
		t.Fatalf("CaptureDiff failed: %v", err)
	}

	if bundle.DiffStats == nil {
		t.Fatalf("expected DiffStats in bundle, got nil")
	}
	if bundle.DiffStats.Ratio <= 0 {
		t.Errorf("expected diff ratio > 0 between red baseline and gray screenshot")
	}
}

func TestEvidenceBundle_DigestDeterminism(t *testing.T) {
	fixedTime := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	b1 := &EvidenceBundle{
		EvidenceID:      "wt_ev_test",
		URL:             "http://localhost:8080/page",
		Timestamp:       fixedTime,
		ExecutionStatus: "EXECUTED",
		Verdict:         "PASS",
		HealthScore:     95,
	}
	b2 := &EvidenceBundle{
		EvidenceID:      "wt_ev_test",
		URL:             "http://localhost:8080/page",
		Timestamp:       fixedTime,
		ExecutionStatus: "EXECUTED",
		Verdict:         "PASS",
		HealthScore:     95,
	}

	d1 := computeDigest(b1)
	d2 := computeDigest(b2)
	if d1 != d2 {
		t.Errorf("expected deterministic digest, got %s vs %s", d1, d2)
	}
}
