package browser

import (
	"context"
	"encoding/base64"
	"fmt"
)

// DocumentMetrics returns CSS viewport and full document dimensions using read-only layout queries.
func (s *cdpSession) DocumentMetrics(ctx context.Context) (int, int, int, int, error) {
	var metrics struct {
		ViewportW int `json:"viewportW"`
		ViewportH int `json:"viewportH"`
		DocumentW int `json:"documentW"`
		DocumentH int `json:"documentH"`
	}
	expr := `({viewportW: window.innerWidth, viewportH: window.innerHeight,
		documentW: Math.max(document.documentElement.scrollWidth, document.body ? document.body.scrollWidth : 0),
		documentH: Math.max(document.documentElement.scrollHeight, document.body ? document.body.scrollHeight : 0)})`
	if err := s.evalValue(ctx, expr, &metrics); err != nil {
		return 0, 0, 0, 0, err
	}
	if metrics.ViewportW <= 0 || metrics.ViewportH <= 0 || metrics.DocumentW <= 0 || metrics.DocumentH <= 0 {
		return 0, 0, 0, 0, fmt.Errorf("invalid document metrics viewport=%dx%d document=%dx%d",
			metrics.ViewportW, metrics.ViewportH, metrics.DocumentW, metrics.DocumentH)
	}
	return metrics.ViewportW, metrics.ViewportH, metrics.DocumentW, metrics.DocumentH, nil
}

// CaptureDocumentRegion captures a document-coordinate slice without scrolling the page.
func (s *cdpSession) CaptureDocumentRegion(ctx context.Context, x, y, w, h int) ([]byte, error) {
	_, _, documentW, documentH, err := s.DocumentMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("cdp: read document metrics: %w", err)
	}
	if x < 0 || y < 0 || w < 1 || h < 1 || x >= documentW || y >= documentH {
		return nil, fmt.Errorf("cdp: document clip outside page: %d,%d %dx%d page=%dx%d",
			x, y, w, h, documentW, documentH)
	}
	if x+w > documentW {
		w = documentW - x
	}
	if y+h > documentH {
		h = documentH - y
	}
	var raw struct {
		Data string `json:"data"`
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Page.captureScreenshot", map[string]any{
		"format": "png", "fromSurface": true, "captureBeyondViewport": true, "optimizeForSpeed": true,
		"clip": map[string]any{"x": x, "y": y, "width": w, "height": h, "scale": 1},
	}, &raw); err != nil {
		return nil, err
	}
	pngBytes, err := base64.StdEncoding.DecodeString(raw.Data)
	if err != nil {
		return nil, fmt.Errorf("cdp: decode document screenshot: %w", err)
	}
	return pngBytes, nil
}

// ScrollDocumentTo brings the selected slice top into the viewport without focusing or typing.
func (s *cdpSession) ScrollDocumentTo(ctx context.Context, y int) error {
	if y < 0 {
		y = 0
	}
	_, _, _, documentH, err := s.DocumentMetrics(ctx)
	if err != nil {
		return err
	}
	if y >= documentH {
		y = documentH - 1
	}
	_, err = s.evalBool(ctx, fmt.Sprintf(`(() => { window.scrollTo(0, %d); return true; })()`, y))
	return err
}
