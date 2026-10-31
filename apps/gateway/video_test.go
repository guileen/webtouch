package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsVideoModel(t *testing.T) {
	videoModels := []string{
		"gemini-veo",
		"veo-3.1",
		"veo-3",
		"veo",
		"veo-2",
		"gemini-video",
		"jimeng-video",
		"seedance-2.0",
		"seedance-2",
		"seedance",
	}
	for _, m := range videoModels {
		if !IsVideoModel(m) {
			t.Errorf("IsVideoModel(%q) should be true", m)
		}
	}

	nonVideoModels := []string{
		"gemini-flash",
		"gemini-3.1-pro",
		"deepseek-chat",
		"deepseek-reasoner",
		"gemini-imagen",
		"doubao-pro",
	}
	for _, m := range nonVideoModels {
		if IsVideoModel(m) {
			t.Errorf("IsVideoModel(%q) should be false", m)
		}
	}
}

func TestTimeoutForModel(t *testing.T) {
	gw := &Gateway{
		chatTimeout:  60 * time.Second,
		videoTimeout: 300 * time.Second,
	}

	if got := gw.TimeoutForModel("gemini-flash"); got != 60*time.Second {
		t.Errorf("TimeoutForModel(gemini-flash) = %v, want 60s", got)
	}

	if got := gw.TimeoutForModel("gemini-veo"); got != 300*time.Second {
		t.Errorf("TimeoutForModel(gemini-veo) = %v, want 300s", got)
	}

	// Environment variable override
	os.Setenv("UWS_VIDEO_TIMEOUT", "450s")
	defer os.Unsetenv("UWS_VIDEO_TIMEOUT")

	if got := gw.TimeoutForModel("gemini-veo"); got != 450*time.Second {
		t.Errorf("TimeoutForModel(gemini-veo with env) = %v, want 450s", got)
	}
}

func TestAdaptVideoPrompt(t *testing.T) {
	// 1. Raw prompt without video keywords for gemini-veo
	raw := "A futuristic sports car drifting on a neon street in Tokyo"
	adapted := AdaptVideoPrompt("gemini-veo", raw)
	want := "Generate a high-quality video using Veo: " + raw
	if adapted != want {
		t.Errorf("AdaptVideoPrompt(gemini-veo, raw) = %q, want %q", adapted, want)
	}

	// 2. Already contains video keywords
	hasKeyword := "Create a cinematic video of a sunset over the ocean"
	if got := AdaptVideoPrompt("gemini-veo", hasKeyword); got != hasKeyword {
		t.Errorf("AdaptVideoPrompt with keyword changed unexpectedly: got %q", got)
	}

	// Chinese keyword
	hasChinese := "生成一段关于雨天的视频"
	if got := AdaptVideoPrompt("gemini-veo", hasChinese); got != hasChinese {
		t.Errorf("AdaptVideoPrompt with Chinese keyword changed: got %q", got)
	}

	// Non-video model
	if got := AdaptVideoPrompt("gemini-flash", raw); got != raw {
		t.Errorf("AdaptVideoPrompt for gemini-flash changed: got %q", got)
	}
}

func TestExtractVideoURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantURL string
	}{
		{
			name:    "HTML video tag",
			input:   `Here is the video: <video src="https://storage.googleapis.com/veo-output/video1.mp4" controls></video>`,
			wantURL: "https://storage.googleapis.com/veo-output/video1.mp4",
		},
		{
			name:    "HTML source tag",
			input:   `<video><source src="https://lh3.googleusercontent.com/veo/clip.webm" type="video/webm"></video>`,
			wantURL: "https://lh3.googleusercontent.com/veo/clip.webm",
		},
		{
			name:    "Markdown video link",
			input:   `Done! [Generated Video](https://cdn.example.com/generated/veo_sample.mp4)`,
			wantURL: "https://cdn.example.com/generated/veo_sample.mp4",
		},
		{
			name:    "Raw MP4 link",
			input:   `Download at https://example.com/media/ocean_waves.mp4 for your project.`,
			wantURL: "https://example.com/media/ocean_waves.mp4",
		},
		{
			name:    "No video link",
			input:   `I am thinking about videos, but haven't generated one yet.`,
			wantURL: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, _ := ExtractVideoURL(tt.input)
			if gotURL != tt.wantURL {
				t.Errorf("ExtractVideoURL() got = %q, want %q", gotURL, tt.wantURL)
			}
		})
	}
}

type fakeScriptEvaluator struct {
	scriptResult string
	err          error
}

func (f *fakeScriptEvaluator) EvaluateScript(ctx context.Context, script string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return json.Marshal(f.scriptResult)
}

func TestExtractVideoFromDOM(t *testing.T) {
	eval := &fakeScriptEvaluator{
		scriptResult: "https://storage.googleapis.com/veo-test/result.mp4",
	}
	got, err := ExtractVideoFromDOM(context.Background(), eval)
	if err != nil {
		t.Fatalf("ExtractVideoFromDOM failed: %v", err)
	}
	if got != "https://storage.googleapis.com/veo-test/result.mp4" {
		t.Errorf("ExtractVideoFromDOM got %q, want test URL", got)
	}

	// Empty evaluator
	emptyGot, err := ExtractVideoFromDOM(context.Background(), nil)
	if err != nil || emptyGot != "" {
		t.Errorf("ExtractVideoFromDOM(nil) got %q, err %v", emptyGot, err)
	}
}

func TestIsVideoStillGenerating(t *testing.T) {
	generatingTexts := []string{
		"I'm generating your video. This could take a few minutes, so check back to see when your video is ready.",
		"正在生成你的视频，这可能需要几分钟...",
		"Video is being created, please check back to see when your video is ready.",
	}
	for _, text := range generatingTexts {
		if !IsVideoStillGenerating(text) {
			t.Errorf("IsVideoStillGenerating(%q) should be true", text)
		}
	}

	readyTexts := []string{
		"Your video is ready!\n0:00 / 0:10\nI'm generating your video.",
		"视频已生成，请在上方播放器中预览。",
		"Here is the text answer without video generation.",
	}
	for _, text := range readyTexts {
		if IsVideoStillGenerating(text) {
			t.Errorf("IsVideoStillGenerating(%q) should be false", text)
		}
	}
}

func TestFormatVideoAnswer(t *testing.T) {
	answer := "Here is your Veo video."
	videoURL := "https://example.com/v.mp4"

	formatted := FormatVideoAnswer(answer, videoURL)
	want := fmt.Sprintf("%s\n\n[Generated Video](%s)", answer, videoURL)
	if formatted != want {
		t.Errorf("FormatVideoAnswer got %q, want %q", formatted, want)
	}

	// Idempotent if already contains url
	if FormatVideoAnswer(formatted, videoURL) != formatted {
		t.Errorf("FormatVideoAnswer not idempotent")
	}

	// Empty URL
	if FormatVideoAnswer(answer, "") != answer {
		t.Errorf("FormatVideoAnswer with empty URL changed answer")
	}
}

func TestDownloadVideo(t *testing.T) {
	testData := []byte("FAKE_MP4_HEADER_CONTENT_BYTES")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testData)
	}))
	defer server.Close()

	tempDir := t.TempDir()
	savedPath, err := DownloadVideo(context.Background(), server.URL+"/test.mp4", tempDir)
	if err != nil {
		t.Fatalf("DownloadVideo failed: %v", err)
	}
	if filepath.Dir(savedPath) != tempDir {
		t.Errorf("Saved path directory %q != %q", filepath.Dir(savedPath), tempDir)
	}
	data, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("Failed to read saved video file: %v", err)
	}
	if string(data) != string(testData) {
		t.Errorf("Downloaded bytes mismatch: got %q, want %q", string(data), string(testData))
	}

	// Invalid URL
	if _, err := DownloadVideo(context.Background(), "invalid://url", tempDir); err == nil {
		t.Errorf("DownloadVideo with invalid URL should fail")
	}
}

func TestFormatVideoAnswerWithLocal(t *testing.T) {
	ans := "Here is your video."
	remote := "https://example.com/v.mp4"
	local := "/tmp/video.mp4"

	res := FormatVideoAnswerWithLocal(ans, remote, local)
	want := fmt.Sprintf("%s\n\n[Generated Video](%s)\n\nLocal Video: %s", ans, remote, local)
	if res != want {
		t.Errorf("FormatVideoAnswerWithLocal got %q, want %q", res, want)
	}

	// Idempotent
	if FormatVideoAnswerWithLocal(res, remote, local) != res {
		t.Errorf("FormatVideoAnswerWithLocal should be idempotent")
	}
}

func TestExtractAndDownloadVideoFromDOM(t *testing.T) {
	tempDir := t.TempDir()
	fakePayload := domVideoResult{
		URL: "https://example.com/test.mp4",
		B64: "RkFLRV9WSURFT19CWVRFUw==", // "FAKE_VIDEO_BYTES"
	}
	payloadBytes, _ := json.Marshal(fakePayload)
	eval := &fakeScriptEvaluator{
		scriptResult: string(payloadBytes),
	}

	videoURL, localPath, err := ExtractAndDownloadVideoFromDOM(context.Background(), eval, tempDir)
	if err != nil {
		t.Fatalf("ExtractAndDownloadVideoFromDOM failed: %v", err)
	}
	if videoURL != fakePayload.URL {
		t.Errorf("videoURL = %q, want %q", videoURL, fakePayload.URL)
	}
	if localPath == "" || filepath.Dir(localPath) != tempDir {
		t.Errorf("localPath = %q, want in %q", localPath, tempDir)
	}
	content, _ := os.ReadFile(localPath)
	if string(content) != "FAKE_VIDEO_BYTES" {
		t.Errorf("local file content = %q, want %q", string(content), "FAKE_VIDEO_BYTES")
	}
}
