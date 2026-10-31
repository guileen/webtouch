package vlm

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func solidPNG(w, h int, c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestDiffImagesDetectsDiff(t *testing.T) {
	red := solidPNG(100, 80, color.RGBA{220, 40, 40, 255})
	blue := solidPNG(100, 80, color.RGBA{40, 80, 220, 255})
	diff, stats, err := DiffImages(red, blue)
	if err != nil {
		t.Fatalf("DiffImages: %v", err)
	}
	if len(diff) == 0 {
		t.Fatal("diff 产物为空")
	}
	if stats.DiffPixels != 100*80 {
		t.Fatalf("DiffPixels = %d, want %d", stats.DiffPixels, 100*80)
	}
	if stats.Ratio != 1.0 {
		t.Fatalf("Ratio = %v, want 1", stats.Ratio)
	}
	if stats.BBox != "0,0,99,79" {
		t.Fatalf("BBox = %q, want 0,0,99,79", stats.BBox)
	}
	if stats.MaxChannel < 100 {
		t.Fatalf("MaxChannel = %d, want big", stats.MaxChannel)
	}
}

func TestDiffImagesIdentical(t *testing.T) {
	c := color.RGBA{10, 20, 30, 255}
	a := solidPNG(50, 50, c)
	b := solidPNG(50, 50, c)
	_, stats, err := DiffImages(a, b)
	if err != nil {
		t.Fatalf("DiffImages: %v", err)
	}
	if stats.DiffPixels != 0 {
		t.Fatalf("DiffPixels = %d, want 0", stats.DiffPixels)
	}
	if stats.BBox != "" {
		t.Fatalf("BBox = %q, want empty", stats.BBox)
	}
}

func TestDiffImagesDifferentSizeUsesMin(t *testing.T) {
	// 尺寸不一致时按最小宽高比较（左上角对齐）。
	a := solidPNG(120, 90, color.RGBA{0, 0, 0, 255})
	b := solidPNG(60, 60, color.RGBA{255, 255, 255, 255})
	_, stats, err := DiffImages(a, b)
	if err != nil {
		t.Fatalf("DiffImages: %v", err)
	}
	if stats.Width != 60 || stats.Height != 60 {
		t.Fatalf("size = %dx%d, want 60x60", stats.Width, stats.Height)
	}
	if stats.TotalPixels != 60*60 {
		t.Fatalf("TotalPixels = %d", stats.TotalPixels)
	}
}

func TestRegionAvgColor(t *testing.T) {
	img := solidPNG(40, 40, color.RGBA{100, 150, 200, 255})
	got, err := RegionAvgColor(img, 0, 0, 40, 40)
	if err != nil {
		t.Fatalf("RegionAvgColor: %v", err)
	}
	if got != "100,150,200" {
		t.Fatalf("got %q, want 100,150,200", got)
	}
}

func TestZoomImage(t *testing.T) {
	img := solidPNG(10, 10, color.RGBA{5, 6, 7, 255})
	zoomed, err := ZoomImage(img, 2, 2, 3, 3, 4)
	if err != nil {
		t.Fatalf("ZoomImage: %v", err)
	}
	dec, err := png.Decode(bytes.NewReader(zoomed))
	if err != nil {
		t.Fatalf("decode zoomed: %v", err)
	}
	if dec.Bounds().Dx() != 12 || dec.Bounds().Dy() != 12 {
		t.Fatalf("zoom size = %dx%d, want 12x12", dec.Bounds().Dx(), dec.Bounds().Dy())
	}
}

func TestDiffImagesRejectsBadImage(t *testing.T) {
	if _, _, err := DiffImages([]byte("not a png"), []byte("not a png")); err == nil {
		t.Fatal("want error for bad image")
	}
}

func TestStatsStringReadable(t *testing.T) {
	stats := DiffStats{Width: 100, Height: 50, DiffPixels: 500, TotalPixels: 5000, Ratio: 0.1, MaxChannel: 200, BBox: "1,2,99,48"}
	s := fmt.Sprintf("%+v", stats)
	if !strings.Contains(s, "Ratio:0.1") {
		t.Fatalf("stats 未含 ratio: %s", s)
	}
}
