package engine

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"

	"uws/pkg/browser"
	"uws/pkg/classifier"
	"uws/pkg/matcher"
	"uws/pkg/vlm"

	uwspb "uws/proto/gen/go/proto/v1"
)

func TestNeedsDiscreteChoiceUsesConfidenceAndGap(t *testing.T) {
	tests := []struct {
		name string
		cs   []matcher.Candidate
		want bool
	}{
		{name: "empty", want: false},
		{name: "low confidence routes to full page", cs: []matcher.Candidate{{Confidence: classifier.ThresholdLow - 0.01}}, want: false},
		{name: "middle confidence", cs: []matcher.Candidate{{Confidence: 0.7}}, want: true},
		{name: "high confidence clear winner", cs: []matcher.Candidate{{Confidence: 0.9}, {Confidence: 0.7}}, want: false},
		{name: "high confidence narrow gap", cs: []matcher.Candidate{{Confidence: 0.9}, {Confidence: 0.8}}, want: true},
		{name: "input order does not matter", cs: []matcher.Candidate{{Confidence: 0.6}, {Confidence: 0.8}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsDiscreteChoice(tt.cs); got != tt.want {
				t.Fatalf("needsDiscreteChoice()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestMacroSlicePositionsGatesAndCaps(t *testing.T) {
	if got := macroSlicePositions(2700, 900); len(got) != 0 {
		t.Fatalf("exactly three viewports must not trigger macro routing: %v", got)
	}
	if got := macroSlicePositions(2701, 900); len(got) != 4 || got[0] != 0 || got[len(got)-1] != 1801 {
		t.Fatalf("just over three viewports should sample four evenly spaced slices: %v", got)
	}
	got := macroSlicePositions(9000, 900)
	if len(got) != 5 || got[0] != 0 || got[len(got)-1] != 8100 {
		t.Fatalf("very long documents should cap at five evenly spaced slices: %v", got)
	}
	if got := macroSlicePositions(9000, 0); len(got) != 0 {
		t.Fatalf("invalid viewport must not produce slices: %v", got)
	}
}

type macroSliceSession struct {
	browser.CDPSession
	positions []int
	selected  int
}

func (s *macroSliceSession) ID() string   { return "macro-test" }
func (s *macroSliceSession) Close() error { return nil }
func (s *macroSliceSession) DocumentMetrics(context.Context) (int, int, int, int, error) {
	return 800, 600, 800, 1801, nil
}
func (s *macroSliceSession) CaptureDocumentRegion(_ context.Context, _, y, _, _ int) ([]byte, error) {
	s.positions = append(s.positions, y)
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: uint8(y / 100), A: 255}}, image.Point{}, draw.Src)
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func (s *macroSliceSession) ScrollDocumentTo(_ context.Context, y int) error {
	s.selected = y
	return nil
}

type macroSliceFactory struct{ session *macroSliceSession }

func (f macroSliceFactory) Create(context.Context, browser.Config) (browser.CDPSession, error) {
	return f.session, nil
}

func TestMacroSliceRouteCapturesBeforeScrollingAndUsesChosenPanel(t *testing.T) {
	session := &macroSliceSession{}
	pool := browser.NewPool(1, macroSliceFactory{session: session})
	bc, err := pool.Acquire(context.Background(), browser.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	client := &vlm.MockClient{Results: []vlm.Result{{Choice: "D"}}}
	srv := &Server{vlmFb: &vlm.Fallback{Enabled: true, Client: client}}

	y, ok := srv.tryMacroSliceRoute(context.Background(), bc, "Settings", nil)
	if !ok {
		t.Fatal("expected the valid macro choice to route to a slice")
	}
	if len(session.positions) != 4 {
		t.Fatalf("expected four viewport captures just over three screens, got %v", session.positions)
	}
	if session.selected != session.positions[3] || y != session.positions[3] {
		t.Fatalf("choice D should select fourth captured slice: positions=%v selected=%d return=%d", session.positions, session.selected, y)
	}
	if client.Calls != 1 || !client.Reqs[0].Crop || client.Reqs[0].CustomPrompt == "" {
		t.Fatalf("expected one labeled macro choice request, got calls=%d reqs=%+v", client.Calls, client.Reqs)
	}
}

type candidateSliceSession struct {
	browser.CDPSession
	captures [][4]int
	selected string
	scrolled bool
	viewport int
}

func (s *candidateSliceSession) ID() string   { return "candidate-slice-test" }
func (s *candidateSliceSession) Close() error { return nil }
func (s *candidateSliceSession) DocumentMetrics(context.Context) (int, int, int, int, error) {
	return 800, 600, 800, 2400, nil
}
func (s *candidateSliceSession) CaptureDocumentRegion(_ context.Context, x, y, w, h int) ([]byte, error) {
	s.captures = append(s.captures, [4]int{x, y, w, h})
	return engineTestPNG(w, h), nil
}
func (s *candidateSliceSession) ScrollDocumentTo(context.Context, int) error {
	s.scrolled = true
	return nil
}
func (s *candidateSliceSession) CaptureScreenshot(context.Context) ([]byte, int, int, error) {
	s.viewport++
	return engineTestPNG(800, 600), 800, 600, nil
}
func (s *candidateSliceSession) ExecuteAction(context.Context, string, string, string) (bool, string) {
	return true, "clicked"
}
func (s *candidateSliceSession) ExecuteActionWithSelector(_ context.Context, _, _, _, selector string) (bool, string) {
	s.selected = selector
	return true, "clicked"
}

type candidateSliceFactory struct{ session *candidateSliceSession }

func (f candidateSliceFactory) Create(context.Context, browser.Config) (browser.CDPSession, error) {
	return f.session, nil
}

func engineTestPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 30, G: 60, B: 90, A: 255}}, image.Point{}, draw.Src)
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil
	}
	return out.Bytes()
}

func TestDiscreteChoiceCapturesOffscreenTop4AndMapsChoiceWithoutScrolling(t *testing.T) {
	session := &candidateSliceSession{}
	pool := browser.NewPool(1, candidateSliceFactory{session: session})
	bc, err := pool.Acquire(context.Background(), browser.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	client := &vlm.MockClient{Results: []vlm.Result{{Choice: "B"}}}
	srv := &Server{vlmFb: &vlm.Fallback{Enabled: true, Client: client}}
	req := &uwspb.ActionIntentRequest{ExpectedAction: "click", TargetSemantic: "World Wide Web history"}
	candidates := []matcher.Candidate{
		{NodeText: "History archive", Confidence: 0.74, Selector: "#archive", X: 20, Y: 1600, W: 100, H: 24, DocumentX: 20, DocumentY: 1600},
		{NodeText: "World Wide Web history", Confidence: 0.72, Selector: "#history", X: 140, Y: 2200, W: 180, H: 28, DocumentX: 140, DocumentY: 2200},
	}

	observed, ok := srv.tryDiscreteChoiceFallback(context.Background(), bc, req, req.GetTargetSemantic(), "https://public.test/article", candidates)
	if !ok {
		t.Fatalf("expected discrete choice to dispatch, observed=%q", observed)
	}
	if len(session.captures) != 1 || session.viewport != 0 {
		t.Fatalf("nearby candidates should share one offscreen document clip, crops=%v viewport=%d", session.captures, session.viewport)
	}
	if session.scrolled {
		t.Fatal("candidate capture/selection must not scroll the page")
	}
	if session.selected != "#history" {
		t.Fatalf("choice B should dispatch its own selector, got %q", session.selected)
	}
	if client.Calls != 1 || len(client.Reqs) != 1 || !client.Reqs[0].Crop {
		t.Fatalf("expected one cropped discrete-choice request, calls=%d reqs=%+v", client.Calls, client.Reqs)
	}
}
