package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"uws/pkg/contract"
)

const (
	// DefaultVideoTimeout 是视频生成模型的单轮总超时预算（默认 300 秒，5分钟）。
	DefaultVideoTimeout = 300 * time.Second
)

// IsVideoModel 检查模型 ID 或别名是否对应视频生成模型。
func IsVideoModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	switch m {
	case "gemini-veo", "veo-3.1", "veo-3", "veo", "veo-2", "gemini-video",
		"jimeng-video", "seedance-2.0", "seedance-2", "seedance", "dreamina-video", "seedance-video":
		return true
	default:
		return false
	}
}

// TimeoutForModel 返回指定模型的单轮操作超时。对于视频模型放宽至 videoTimeout（默认 300s）。
func (g *Gateway) TimeoutForModel(model string) time.Duration {
	if IsVideoModel(model) {
		if env := os.Getenv("UWS_VIDEO_TIMEOUT"); env != "" {
			if d, err := time.ParseDuration(env); err == nil && d > 0 {
				return d
			}
		}
		if g != nil && g.videoTimeout > 0 {
			return g.videoTimeout
		}
		return DefaultVideoTimeout
	}
	if g != nil && g.chatTimeout > 0 {
		return g.chatTimeout
	}
	return DefaultChatTimeout
}

// AdaptVideoPrompt 当请求为视频生成模型时，若提示词缺少显式视频意图触发词，自动注入前缀。
func AdaptVideoPrompt(model, prompt string) string {
	if !IsVideoModel(model) {
		return prompt
	}
	p := strings.TrimSpace(prompt)
	if p == "" {
		return prompt
	}
	lower := strings.ToLower(p)
	videoKeywords := []string{
		"video", "视频", "veo", "seedance", "animate", "animated", "animation",
		"generate video", "create video", "make a video", "生成视频", "制作视频", "生成一段视频",
	}
	for _, kw := range videoKeywords {
		if strings.Contains(lower, kw) {
			return prompt
		}
	}
	if strings.Contains(strings.ToLower(model), "gemini") || strings.Contains(strings.ToLower(model), "veo") {
		return "Generate a high-quality video using Veo: " + p
	}
	return "Generate a video: " + p
}

// IsVideoStillGenerating 检查页面回答是否仍处于“视频正在生成”的中间态。
func IsVideoStillGenerating(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "generating your video") ||
		strings.Contains(lower, "正在生成你的视频") ||
		strings.Contains(lower, "正在生成视频") ||
		strings.Contains(lower, "check back to see when your video is ready") {
		// 如果出现已就绪文案（排除 "when your video is ready" 提示语自身）
		if strings.Contains(lower, "your video is ready!") ||
			(strings.Contains(lower, "your video is ready") && !strings.Contains(lower, "when your video is ready")) ||
			strings.Contains(lower, "视频已生成") ||
			strings.Contains(lower, "视频已就绪") {
			return false
		}
		return true
	}
	return false
}

var (
	videoURLRegexes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)<video[^>]+src=["']([^"']+)["']`),
		regexp.MustCompile(`(?i)<source[^>]+src=["']([^"']+)["']`),
		regexp.MustCompile(`(?i)\[(?:Generated Video|Video|video)\]\((https?://[^\s)]+)\)`),
		regexp.MustCompile(`https?://[^\s"'<>]+\.(?:mp4|webm)[\w\-._~:/?#\[\]@!$&'()*+,;=]*`),
	}
)

// ExtractVideoURL 从纯文本、HTML 片段或 Markdown 中识别提取视频链接。
func ExtractVideoURL(text string) (videoURL string, cleanText string) {
	cleanText = text
	for _, re := range videoURLRegexes {
		if m := re.FindStringSubmatch(text); len(m) > 1 {
			return m[1], cleanText
		} else if m := re.FindString(text); m != "" {
			return m, cleanText
		}
	}
	return "", cleanText
}

type domVideoResult struct {
	URL   string `json:"url"`
	B64   string `json:"b64,omitempty"`
	Error string `json:"error,omitempty"`
}

// ExtractAndDownloadVideoFromDOM 在页面真实 DOM 中查找视频元素并通过浏览器内原生 fetch 抽取 Authenticated 视频流保存到本地。
func ExtractAndDownloadVideoFromDOM(ctx context.Context, eval contract.DirectScriptEvaluator, destDir string) (videoURL, localPath string, err error) {
	if eval == nil {
		return "", "", nil
	}
	script := `(async () => {
		const v = document.querySelector('video[src], video > source[src], a[download*=".mp4"], div[data-test-id*="video"] video, video');
		let src = "";
		if (v) {
			src = v.currentSrc || v.src || v.getAttribute('src') || v.href || "";
		}
		if (!src) {
			const media = document.querySelector('[data-video-url], [data-media-url]');
			if (media) {
				src = media.getAttribute('data-video-url') || media.getAttribute('data-media-url') || "";
			}
		}
		if (!src) return { url: "" };
		try {
			const resp = await fetch(src, { credentials: "include" });
			if (!resp.ok) return { url: src, error: "HTTP " + resp.status };
			const buf = await resp.arrayBuffer();
			const bytes = new Uint8Array(buf);
			let binary = "";
			const len = bytes.byteLength;
			const chunkSize = 0x8000;
			for (let i = 0; i < len; i += chunkSize) {
				binary += String.fromCharCode.apply(null, bytes.subarray(i, Math.min(i + chunkSize, len)));
			}
			return { url: src, b64: btoa(binary) };
		} catch (e) {
			return { url: src, error: e.toString() };
		}
	})()`
	resBytes, err := eval.EvaluateScript(ctx, script)
	if err != nil {
		return "", "", err
	}
	var res domVideoResult
	if err := json.Unmarshal(resBytes, &res); err != nil {
		// 尝试作为纯字符串反序列化，或字符串化的 JSON 对象
		var single string
		if json.Unmarshal(resBytes, &single) == nil && single != "" {
			if strings.HasPrefix(strings.TrimSpace(single), "{") {
				_ = json.Unmarshal([]byte(single), &res)
			} else {
				res.URL = single
			}
		} else {
			return "", "", nil
		}
	}
	videoURL = strings.TrimSpace(res.URL)
	if videoURL == "" {
		return "", "", nil
	}
	if destDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			destDir = os.TempDir()
		} else {
			destDir = filepath.Join(home, ".uws", "artifacts", "videos")
		}
	}
	_ = os.MkdirAll(destDir, 0755)

	if res.B64 != "" {
		data, decErr := base64.StdEncoding.DecodeString(res.B64)
		if decErr == nil && len(data) > 0 {
			hash := sha256.Sum256([]byte(videoURL))
			filename := fmt.Sprintf("veo_%s_%s.mp4", time.Now().Format("20060102_150405"), hex.EncodeToString(hash[:4]))
			localPath = filepath.Join(destDir, filename)
			if err := os.WriteFile(localPath, data, 0644); err == nil {
				return videoURL, localPath, nil
			}
		}
	}

	// 降级尝试外部下载
	saved, dlErr := DownloadVideo(ctx, videoURL, destDir)
	if dlErr == nil && saved != "" {
		return videoURL, saved, nil
	}
	return videoURL, "", nil
}

// ExtractVideoFromDOM 在页面真实 DOM 中查找已渲染的 video 标签或下载链接。
func ExtractVideoFromDOM(ctx context.Context, eval contract.DirectScriptEvaluator) (string, error) {
	if eval == nil {
		return "", nil
	}
	script := `(() => {
		const v = document.querySelector('video[src], video > source[src], a[download*=".mp4"], div[data-test-id*="video"] video');
		if (v) {
			const src = v.src || v.getAttribute('src') || v.href;
			if (src && !src.startsWith('blob:')) return src;
			if (src) return src;
		}
		const media = document.querySelector('[data-video-url], [data-media-url]');
		if (media) {
			return media.getAttribute('data-video-url') || media.getAttribute('data-media-url') || "";
		}
		return "";
	})()`
	resBytes, err := eval.EvaluateScript(ctx, script)
	if err != nil {
		return "", err
	}
	var src string
	if err := json.Unmarshal(resBytes, &src); err != nil {
		return "", nil
	}
	return strings.TrimSpace(src), nil
}

// FormatVideoAnswer 将视频链接标准化追加到模型回答末尾。
func FormatVideoAnswer(answer, videoURL string) string {
	if videoURL == "" {
		return answer
	}
	ans := strings.TrimSpace(answer)
	if strings.Contains(ans, videoURL) {
		return ans
	}
	if ans == "" {
		return fmt.Sprintf("[Generated Video](%s)", videoURL)
	}
	return fmt.Sprintf("%s\n\n[Generated Video](%s)", ans, videoURL)
}

// FormatVideoAnswerWithLocal 将视频远程链接与本地已保存文件标准化追加到模型回答末尾。
func FormatVideoAnswerWithLocal(answer, videoURL, localPath string) string {
	ans := FormatVideoAnswer(answer, videoURL)
	if localPath == "" {
		return ans
	}
	if strings.Contains(ans, localPath) {
		return ans
	}
	return fmt.Sprintf("%s\n\nLocal Video: %s", ans, localPath)
}

// DownloadVideo 将远程视频 URL 安全下载到本地 artifacts 存储中。
func DownloadVideo(ctx context.Context, videoURL, destDir string) (string, error) {
	if videoURL == "" || (!strings.HasPrefix(videoURL, "http://") && !strings.HasPrefix(videoURL, "https://")) {
		return "", fmt.Errorf("invalid video URL: %s", videoURL)
	}
	if destDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			destDir = os.TempDir()
		} else {
			destDir = filepath.Join(home, ".uws", "artifacts", "videos")
		}
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir artifacts failed: %w", err)
	}
	hash := sha256.Sum256([]byte(videoURL))
	filename := fmt.Sprintf("veo_%s_%s.mp4", time.Now().Format("20060102_150405"), hex.EncodeToString(hash[:4]))
	targetPath := filepath.Join(destDir, filename)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, videoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download video failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("video server returned HTTP %d", resp.StatusCode)
	}

	tmpFile := targetPath + ".tmp"
	f, err := os.Create(tmpFile)
	if err != nil {
		return "", err
	}
	defer func() {
		f.Close()
		_ = os.Remove(tmpFile)
	}()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("copy video bytes failed: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpFile, targetPath); err != nil {
		return "", err
	}
	return targetPath, nil
}
