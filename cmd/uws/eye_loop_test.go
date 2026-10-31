package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"uws/pkg/vlm"
)

func solidPNG(w, h int, c color.RGBA) []byte {
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

func TestRunEyeEvidenceDiffAppends(t *testing.T) {
	imgs := []vlm.EyeImage{
		{Label: "a", Data: solidPNG(10, 10, color.RGBA{255, 0, 0, 255})},
		{Label: "b", Data: solidPNG(10, 10, color.RGBA{0, 0, 255, 255})},
	}
	ev, err := runEyeEvidence(chatOptions{}, vlm.EyeTurn{Action: "diff"}, &imgs)
	if err != nil {
		t.Fatalf("runEyeEvidence(diff): %v", err)
	}
	if !strings.Contains(ev, "像素差异证据") {
		t.Fatalf("evidence = %q, want to contain 像素差异证据", ev)
	}
	if len(imgs) != 3 {
		t.Fatalf("imgs len = %d, want 3 (diff image appended)", len(imgs))
	}
}

func TestRunEyeEvidenceZoom(t *testing.T) {
	imgs := []vlm.EyeImage{{Label: "a", Data: solidPNG(50, 50, color.RGBA{10, 20, 30, 255})}}
	ev, err := runEyeEvidence(chatOptions{}, vlm.EyeTurn{Action: "zoom", X: 0, Y: 0, W: 10, H: 10, Scale: 3}, &imgs)
	if err != nil {
		t.Fatalf("runEyeEvidence(zoom): %v", err)
	}
	if !strings.Contains(ev, "区域放大证据") {
		t.Fatalf("evidence = %q, want 区域放大证据", ev)
	}
	if len(imgs) != 2 {
		t.Fatalf("imgs len = %d, want 2", len(imgs))
	}
}

func TestRunEyeEvidenceRegionColor(t *testing.T) {
	imgs := []vlm.EyeImage{{Label: "a", Data: solidPNG(10, 10, color.RGBA{200, 100, 50, 255})}}
	ev, err := runEyeEvidence(chatOptions{}, vlm.EyeTurn{Action: "region_color", X: 0, Y: 0, W: 10, H: 10}, &imgs)
	if err != nil {
		t.Fatalf("runEyeEvidence(region_color): %v", err)
	}
	if !strings.Contains(ev, "200,100,50") {
		t.Fatalf("evidence = %q, want avg color 200,100,50", ev)
	}
}

func TestRunEyeEvidenceUnknownAction(t *testing.T) {
	imgs := []vlm.EyeImage{{Label: "a", Data: solidPNG(4, 4, color.RGBA{1, 2, 3, 255})}}
	if _, err := runEyeEvidence(chatOptions{}, vlm.EyeTurn{Action: "click"}, &imgs); err == nil {
		t.Fatal("want error for unknown action click")
	}
}

func TestEyeMainControlMultiRound(t *testing.T) {
	// 模拟 VLM：第 1 轮请求 zoom（证据原语），第 2 轮 conclude。用于证明「引擎支持多轮，
	// 且由 VLM 自决、单轮优先、不空转」的整条主控路径（无需真实模型/费用）。
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var content string
		if n == 1 {
			content = `{"action":"zoom","reason":"要看细节","x":0,"y":0,"w":8,"h":8,"scale":2}`
		} else {
			content = `{"action":"conclude","verdict":"pass","summary":"放大后确认一致","checks":[],"confidence":0.7}`
		}
		w.Header().Set("Content-Type", "application/json")
		enc, _ := json.Marshal(content)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(enc) + `}}]}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := vlm.NewEye(vlm.Config{BaseURL: ts.URL, Model: "mock", APIKey: "test", Timeout: 5 * time.Second})
	imgs := []vlm.EyeImage{{Label: "a", Data: solidPNG(20, 20, color.RGBA{50, 60, 70, 255})}}
	res, err := eyeMainControl(context.Background(), client, chatOptions{detail: "low"}, &imgs, "这个区域和参考是否一致？")
	if err != nil {
		t.Fatalf("eyeMainControl: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("calls = %d, want 2 (round1 zoom + round2 conclude)", calls)
	}
	if res.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", res.Verdict)
	}
	if len(imgs) != 2 {
		t.Fatalf("imgs len = %d, want 2 (zoom evidence appended)", len(imgs))
	}
}
