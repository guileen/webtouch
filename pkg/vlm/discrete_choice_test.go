package vlm

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"
)

func createTestPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// 填充浅蓝色背景
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 200, G: 220, B: 240, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestDiscreteChoicePromptAndParse(t *testing.T) {
	prompt := DiscreteChoicePrompt("Sign In", []string{"A", "B", "C", "D"})
	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}

	// 测试标准 JSON 解析
	choice, err := ParseDiscreteChoice(`{"choice":"B"}`)
	if err != nil || choice != "B" {
		t.Fatalf("expected B, got %q, err: %v", choice, err)
	}

	// 测试带 markdown 围栏与空白
	choice, err = ParseDiscreteChoice("```json\n{\"choice\": \"a\"}\n```")
	if err != nil || choice != "A" {
		t.Fatalf("expected A, got %q, err: %v", choice, err)
	}

	// 测试 NONE 选项
	choice, err = ParseDiscreteChoice(`{"choice":"NONE"}`)
	if err != nil || choice != "NONE" {
		t.Fatalf("expected NONE, got %q, err: %v", choice, err)
	}
}

func TestBuildCandidateMontage_1To4Blocks(t *testing.T) {
	fullPng := createTestPNG(1000, 800)

	// 测试 1 块
	cands1 := []CandidateBlock{
		{Index: 0, Selector: "#b1", X: 100, Y: 100, W: 80, H: 30, Text: "Btn1"},
	}
	m1, w1, h1, err := BuildCandidateMontage(fullPng, cands1)
	if err != nil || len(m1) == 0 || w1 <= 0 || h1 <= 0 {
		t.Fatalf("montage 1 failed: w=%d, h=%d, err=%v", w1, h1, err)
	}

	// 测试 2 块
	cands2 := []CandidateBlock{
		{Index: 0, Selector: "#b1", X: 100, Y: 100, W: 80, H: 30, Text: "Btn1"},
		{Index: 1, Selector: "#b2", X: 100, Y: 200, W: 120, H: 40, Text: "Btn2"},
	}
	m2, w2, h2, err := BuildCandidateMontage(fullPng, cands2)
	if err != nil || len(m2) == 0 || w2 <= 0 || h2 <= 0 {
		t.Fatalf("montage 2 failed: w=%d, h=%d, err=%v", w2, h2, err)
	}

	// 测试 4 块（四宫格）
	cands4 := []CandidateBlock{
		{Index: 0, Selector: "#b1", X: 100, Y: 100, W: 80, H: 30, Text: "Btn1"},
		{Index: 1, Selector: "#b2", X: 300, Y: 100, W: 80, H: 30, Text: "Btn2"},
		{Index: 2, Selector: "#b3", X: 100, Y: 400, W: 80, H: 30, Text: "Btn3"},
		{Index: 3, Selector: "#b4", X: 300, Y: 400, W: 80, H: 30, Text: "Btn4"},
	}
	m4, w4, h4, err := BuildCandidateMontage(fullPng, cands4)
	if err != nil || len(m4) == 0 || w4 <= 0 || h4 <= 0 {
		t.Fatalf("montage 4 failed: w=%d, h=%d, err=%v", w4, h4, err)
	}
}

func TestBuildCandidateMontageForViewportMapsCSSAtDPR2(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(192, 92, 208, 108), &image.Uniform{C: color.RGBA{R: 255, A: 255}}, image.Point{}, draw.Src)
	var src bytes.Buffer
	if err := png.Encode(&src, img); err != nil {
		t.Fatal(err)
	}
	montage, w, h, err := BuildCandidateMontageForViewport(src.Bytes(), 200, 100, []CandidateBlock{
		{X: 95, Y: 45, W: 10, H: 10, Text: "target"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w != 240 || h != 96 {
		t.Fatalf("expected a 240x96 native-pixel crop at DPR2, got %dx%d", w, h)
	}
	crop, err := png.Decode(bytes.NewReader(montage))
	if err != nil {
		t.Fatal(err)
	}
	redPixels := 0
	for y := 0; y < crop.Bounds().Dy(); y++ {
		for x := 0; x < crop.Bounds().Dx(); x++ {
			r, g, b, _ := crop.At(x, y).RGBA()
			if r > 60000 && g < 1000 && b < 1000 {
				redPixels++
			}
		}
	}
	if redPixels < 200 {
		t.Fatalf("DPR2 CSS-to-PNG mapping lost the target patch: only %d red pixels", redPixels)
	}
}

func TestBuildImagePanelMontageFiveSlices(t *testing.T) {
	panels := make([]ImagePanel, 5)
	for i := range panels {
		panels[i] = ImagePanel{Label: string(rune('A' + i)), PNG: createTestPNG(120, 80)}
	}
	out, w, h, err := BuildImagePanelMontage(panels)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 || w != 248 || h != 256 {
		t.Fatalf("expected five-panel 2x3 montage 248x256, got bytes=%d size=%dx%d", len(out), w, h)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	darkGlyphPixels := 0
	for y := 184; y < 240; y++ {
		for x := 8; x < 64; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if r>>8 == 15 && g>>8 == 23 && b>>8 == 42 {
				darkGlyphPixels++
			}
		}
	}
	if darkGlyphPixels == 0 {
		t.Fatal("fifth slice label E is missing its glyph")
	}
}

func TestResolveDiscreteChoice_MockClient(t *testing.T) {
	fullPng := createTestPNG(800, 600)
	cands := []CandidateBlock{
		{Index: 0, Selector: "#cancel", X: 50, Y: 50, W: 60, H: 25, Text: "Cancel"},
		{Index: 1, Selector: "#submit", X: 200, Y: 50, W: 80, H: 30, Text: "Submit"},
		{Index: 2, Selector: "#help", X: 50, Y: 200, W: 50, H: 20, Text: "Help"},
	}
	montagePng, w, h, err := BuildCandidateMontage(fullPng, cands)
	if err != nil {
		t.Fatalf("montage build failed: %v", err)
	}

	// 模拟返回 B
	mock := &MockClient{
		Results: []Result{
			{Choice: "B", Raw: `{"choice":"B"}`},
		},
	}

	res, err := ResolveDiscreteChoice(context.Background(), mock, montagePng, w, h, "Submit", cands)
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if res.Choice != "B" {
		t.Fatalf("expected choice B, got %q", res.Choice)
	}
	if res.Candidate == nil || res.Candidate.Selector != "#submit" {
		t.Fatalf("expected candidate #submit, got %+v", res.Candidate)
	}
}
