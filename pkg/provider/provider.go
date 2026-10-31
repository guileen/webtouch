package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uws/pkg/qa"
)

// DefaultProvider 是 Provider 接口的标准实现。
type DefaultProvider struct {
	sanitizer      *Sanitizer
	defaultTimeout time.Duration
	maxTimeout     time.Duration
}

// NewProvider 创建默认 WebTouch Provider。
func NewProvider() *DefaultProvider {
	return &DefaultProvider{
		sanitizer:      NewSanitizer(),
		defaultTimeout: 15 * time.Second,
		maxTimeout:     60 * time.Second,
	}
}

// InspectPage 执行页面有界体检，返回脱敏且确定性的 EvidenceBundle。
func (p *DefaultProvider) InspectPage(ctx context.Context, req InspectRequest) (*EvidenceBundle, error) {
	if req.URL == "" {
		return nil, fmt.Errorf("provider: url cannot be empty")
	}

	sanitizedURL, redactedCount := p.sanitizer.SanitizeURL(req.URL)

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = p.defaultTimeout
	} else if timeout > p.maxTimeout {
		timeout = p.maxTimeout
	}

	outDir := req.OutputDir
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), fmt.Sprintf("webtouch_provider_%d", time.Now().UnixNano()))
	}
	_ = os.MkdirAll(outDir, 0755)

	viewports := req.Viewports
	if len(viewports) == 0 {
		viewports = DefaultViewportSpecs()
	}

	runnerOpts := qa.RunnerOptions{
		URL:             req.URL,
		Viewports:       toQaViewports(viewports),
		OutputDir:       outDir,
		AttachCDP:       req.AttachCDP,
		Timeout:         timeout,
		Headless:        req.Headless,
		Isolated:        req.Isolated,
		SessionOverride: req.sessionOverride,
	}

	runner := qa.NewRunner(runnerOpts)
	report, err := runner.Run(ctx)
	if err != nil {
		bundle := &EvidenceBundle{
			EvidenceID:      generateEvidenceID(),
			URL:             sanitizedURL,
			Timestamp:       time.Now(),
			ExecutionStatus: string(qa.ExecutionStatusNotRun),
			Verdict:         string(qa.VerdictBlocked),
			HealthScore:     0,
			RedactedCount:   redactedCount,
			Summary:         fmt.Sprintf("浏览器执行中断或受阻: %v", err),
		}
		bundle.BundleDigestSHA = computeDigest(bundle)
		return bundle, fmt.Errorf("provider inspect failed: %w", err)
	}

	return p.buildEvidenceBundle(sanitizedURL, redactedCount, report, nil), nil
}

// CaptureDiff 执行页面体检并与基准图像素级对齐，返回包含差分指标的 EvidenceBundle。
func (p *DefaultProvider) CaptureDiff(ctx context.Context, req DiffRequest) (*EvidenceBundle, error) {
	if req.URL == "" {
		return nil, fmt.Errorf("provider: url cannot be empty")
	}

	sanitizedURL, redactedCount := p.sanitizer.SanitizeURL(req.URL)

	var refBytes []byte
	if len(req.BaselineImage) > 0 {
		refBytes = req.BaselineImage
	} else if req.BaselinePath != "" {
		b, err := os.ReadFile(req.BaselinePath)
		if err != nil {
			return nil, fmt.Errorf("provider: failed to read baseline path %s: %w", req.BaselinePath, err)
		}
		refBytes = b
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = p.defaultTimeout
	} else if timeout > p.maxTimeout {
		timeout = p.maxTimeout
	}

	outDir := req.OutputDir
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), fmt.Sprintf("webtouch_diff_%d", time.Now().UnixNano()))
	}
	_ = os.MkdirAll(outDir, 0755)

	viewports := req.Viewports
	if len(viewports) == 0 {
		viewports = DefaultViewportSpecs()
	}

	runnerOpts := qa.RunnerOptions{
		URL:             req.URL,
		RefImage:        refBytes,
		Viewports:       toQaViewports(viewports),
		OutputDir:       outDir,
		AttachCDP:       req.AttachCDP,
		Timeout:         timeout,
		Headless:        req.Headless,
		Isolated:        req.Isolated,
		SessionOverride: req.sessionOverride,
	}

	runner := qa.NewRunner(runnerOpts)
	report, err := runner.Run(ctx)
	if err != nil {
		bundle := &EvidenceBundle{
			EvidenceID:      generateEvidenceID(),
			URL:             sanitizedURL,
			Timestamp:       time.Now(),
			ExecutionStatus: string(qa.ExecutionStatusNotRun),
			Verdict:         string(qa.VerdictBlocked),
			HealthScore:     0,
			RedactedCount:   redactedCount,
			Summary:         fmt.Sprintf("视觉差分执行中断: %v", err),
		}
		bundle.BundleDigestSHA = computeDigest(bundle)
		return bundle, fmt.Errorf("provider capture diff failed: %w", err)
	}

	var diffMeta *DiffStatsMeta
	for _, vp := range report.Viewports {
		if vp.DiffStats != nil {
			diffMeta = &DiffStatsMeta{
				Width:       vp.DiffStats.Width,
				Height:      vp.DiffStats.Height,
				DiffPixels:  vp.DiffStats.DiffPixels,
				TotalPixels: vp.DiffStats.TotalPixels,
				Ratio:       vp.DiffStats.Ratio,
				MaxChannel:  vp.DiffStats.MaxChannel,
				BBox:        vp.DiffStats.BBox,
			}
			break
		}
	}

	return p.buildEvidenceBundle(sanitizedURL, redactedCount, report, diffMeta), nil
}

func (p *DefaultProvider) buildEvidenceBundle(sanitizedURL string, redactedCount int, report *qa.QAReport, diffMeta *DiffStatsMeta) *EvidenceBundle {
	vpMetas := make([]ViewportMeta, 0, len(report.Viewports))
	var flagged []string

	layoutOK := true
	consoleClean := true
	resourcesOK := true
	touchFriendly := true

	for _, vp := range report.Viewports {
		if vp.HasHorizontalOverflow {
			layoutOK = false
			flagged = append(flagged, fmt.Sprintf("%s 视口存在横向溢出 (+%dpx)", vp.Viewport.Name, vp.OverflowDelta))
		}
		if len(vp.SmallTargets) > 0 {
			touchFriendly = false
			flagged = append(flagged, fmt.Sprintf("%s 视口包含 %d 个过小点击热区 (<32px)", vp.Viewport.Name, len(vp.SmallTargets)))
		}
		if len(vp.ConsoleErrors) > 0 {
			consoleClean = false
			flagged = append(flagged, fmt.Sprintf("%s 视口捕获 %d 个控制台异常", vp.Viewport.Name, len(vp.ConsoleErrors)))
		}
		if len(vp.BrokenResources) > 0 {
			resourcesOK = false
			flagged = append(flagged, fmt.Sprintf("%s 视口包含 %d 个资源加载失败", vp.Viewport.Name, len(vp.BrokenResources)))
		}

		vpMetas = append(vpMetas, ViewportMeta{
			Name:                  vp.Viewport.Name,
			Width:                 vp.Viewport.Width,
			Height:                vp.Viewport.Height,
			HasHorizontalOverflow: vp.HasHorizontalOverflow,
			OverflowDelta:         vp.OverflowDelta,
			SmallTargetCount:      len(vp.SmallTargets),
			ConsoleErrorCount:     len(vp.ConsoleErrors),
			BrokenResourceCount:   len(vp.BrokenResources),
			AuditDurationMs:       vp.AuditDurationMs,
		})
	}

	if diffMeta != nil && diffMeta.Ratio > 0.05 {
		flagged = append(flagged, fmt.Sprintf("视觉差分率超过阈值 (%.2f%%)", diffMeta.Ratio*100))
	}

	summary := fmt.Sprintf("WebTouch 证据体检完成: 健康分 %d, 结论 %s", report.HealthScore, report.Verdict)
	if len(flagged) > 0 {
		summary += "；存在问题: " + strings.Join(flagged, "，")
	}

	bundle := &EvidenceBundle{
		EvidenceID:      generateEvidenceID(),
		URL:             sanitizedURL,
		Timestamp:       report.Timestamp,
		ExecutionStatus: string(report.ExecutionStatus),
		Verdict:         string(report.Verdict),
		HealthScore:     report.HealthScore,
		Viewports:       vpMetas,
		DiffStats:       diffMeta,
		SanityCheck: SanityCheckMeta{
			LayoutIntegrity: layoutOK,
			ConsoleClean:    consoleClean,
			ResourcesOK:     resourcesOK,
			TouchFriendly:   touchFriendly,
			FlaggedIssues:   flagged,
		},
		RedactedCount: redactedCount,
		Summary:       summary,
	}

	bundle.BundleDigestSHA = computeDigest(bundle)
	return bundle
}

func generateEvidenceID() string {
	now := time.Now().UnixNano()
	h := sha256.Sum256([]byte(fmt.Sprintf("wt_ev_%d", now)))
	return fmt.Sprintf("wt_ev_%s", hex.EncodeToString(h[:8]))
}

func computeDigest(bundle *EvidenceBundle) string {
	clone := *bundle
	clone.BundleDigestSHA = "" // 哈希计算时不包含自身哈希字段
	data, _ := json.Marshal(clone)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
