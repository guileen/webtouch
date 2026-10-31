package qa

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"
	"uws/pkg/browser"
)

// BrowserSession 抽象用于 QA 体检的会话能力。
type BrowserSession interface {
	Navigate(ctx context.Context, url string) error
	SetViewport(ctx context.Context, width, height int, scale float64, mobile bool) error
	EvaluateScript(ctx context.Context, expr string) ([]byte, error)
	CaptureScreenshot(ctx context.Context) (png []byte, width, height int, err error)
	Close() error
}

// SessionAdapter 把 browser.CDPSession 适配为 BrowserSession。
type SessionAdapter struct {
	Session browser.CDPSession
}

func (a *SessionAdapter) Navigate(ctx context.Context, url string) error {
	return a.Session.Navigate(ctx, url)
}

func (a *SessionAdapter) SetViewport(ctx context.Context, width, height int, scale float64, mobile bool) error {
	if vs, ok := a.Session.(browser.ViewportSetter); ok {
		return vs.SetViewport(ctx, width, height, scale, mobile)
	}
	return fmt.Errorf("session does not implement ViewportSetter")
}

func (a *SessionAdapter) EvaluateScript(ctx context.Context, expr string) ([]byte, error) {
	if de, ok := a.Session.(browser.DirectScriptEvaluator); ok {
		return de.EvaluateScript(ctx, expr)
	}
	return nil, fmt.Errorf("session does not implement DirectScriptEvaluator")
}

func (a *SessionAdapter) CaptureScreenshot(ctx context.Context) (png []byte, width, height int, err error) {
	if ss, ok := a.Session.(browser.Screenshotter); ok {
		return ss.CaptureScreenshot(ctx)
	}
	return nil, 0, 0, fmt.Errorf("session does not implement Screenshotter")
}

func (a *SessionAdapter) Close() error {
	return a.Session.Close()
}

// RunnerOptions 定义 QA 体检运行参数。
type RunnerOptions struct {
	URL             string
	Viewports       []Viewport
	RefImage        []byte
	OutputDir       string
	AttachCDP       string
	Headless        bool
	Timeout         time.Duration
	OnProgress      func(step int, total int, message string)
	SessionOverride BrowserSession
	// Isolated 是无人消费方（Agent Hub 体检）必须用的强隔离模式：
	// 不探测/不附着 127.0.0.1:9222 上可能存在的个人 Chrome，强制无头启动，
	// 且只用临时 Profile（ProfileID 为空 → browser.openProfile 建临时 user-data-dir）。
	// 与显式 AttachCDP 互斥：两者同时出现说明调用方在猜身份，直接报错而不是替它选。
	Isolated bool
}

const errorHookScript = `(() => {
if (!window.__uws_qa_hooked) {
    window.__uws_qa_hooked = true;
    window.__uws_qa_errors = [];
    window.addEventListener('error', function(e) {
        window.__uws_qa_errors.push({
            type: 'error',
            message: e.message || String(e),
            filename: e.filename || '',
            lineno: e.lineno || 0
        });
    });
    window.addEventListener('unhandledrejection', function(e) {
        window.__uws_qa_errors.push({
            type: 'unhandledrejection',
            message: (e.reason && (e.reason.message || String(e.reason))) || 'unhandled rejection'
        });
    });
}
return true;
})()`

const inspectScript = `(() => {
    const scrollW = document.documentElement.scrollWidth || document.body.scrollWidth || 0;
    const clientW = window.innerWidth || document.documentElement.clientWidth || 0;
    const hasOverflow = scrollW > (clientW + 1);
    let overflowElements = [];
    if (hasOverflow) {
        const all = document.querySelectorAll('*');
        for (let i = 0; i < all.length; i++) {
            const el = all[i];
            const rect = el.getBoundingClientRect();
            if (rect.right > clientW + 1) {
                overflowElements.push({
                    tag: el.tagName.toLowerCase(),
                    id: el.id || '',
                    class_name: el.className ? String(el.className).slice(0, 50) : '',
                    width: Math.round(rect.width),
                    right: Math.round(rect.right),
                    overflow_px: Math.round(rect.right - clientW)
                });
                if (overflowElements.length >= 10) break;
            }
        }
    }

    const targets = document.querySelectorAll('button, a, input, select, textarea, [role="button"]');
    let smallTargets = [];
    for (let i = 0; i < targets.length; i++) {
        const el = targets[i];
        const rect = el.getBoundingClientRect();
        if (rect.width > 0 && rect.height > 0) {
            const style = window.getComputedStyle(el);
            if (style.visibility !== 'hidden' && style.display !== 'none' && style.opacity !== '0') {
                if (rect.width < 32 || rect.height < 32) {
                    smallTargets.push({
                        tag: el.tagName.toLowerCase(),
                        id: el.id || '',
                        text: (el.innerText || el.getAttribute('aria-label') || el.value || '').trim().slice(0, 30),
                        width: Math.round(rect.width),
                        height: Math.round(rect.height)
                    });
                    if (smallTargets.length >= 15) break;
                }
            }
        }
    }

    let brokenResources = [];
    for (let i = 0; i < document.images.length; i++) {
        const img = document.images[i];
        if (!img.complete || img.naturalWidth === 0) {
            const src = img.src || img.getAttribute('src') || '';
            if (src) brokenResources.push(src);
        }
    }

    const consoleErrors = window.__uws_qa_errors || [];

    return {
        scroll_width: scrollW,
        inner_width: clientW,
        has_horizontal_overflow: hasOverflow,
        overflow_delta: Math.max(0, scrollW - clientW),
        overflow_elements: overflowElements,
        small_targets: smallTargets,
        broken_resources: brokenResources,
        console_errors: consoleErrors
    };
})()`

// QARunner 负责驱动多视口物理体检与差分报告生成。
type QARunner struct {
	opts RunnerOptions
}

// NewRunner 构造 QA 体检执行器。
func NewRunner(opts RunnerOptions) *QARunner {
	if len(opts.Viewports) == 0 {
		opts.Viewports = DefaultViewports()
	}
	if opts.OutputDir == "" {
		opts.OutputDir = filepath.Join("build", "qa")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	return &QARunner{opts: opts}
}

// isPortListening 探测指定 host:port 是否在监听。
func isPortListening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// isPortListeningFn 是 9222 探测的测试缝（默认就是真实探测）。
var isPortListeningFn = isPortListening

// resolveAttachURL 决定本次体检要不要附着外部 Chrome。隔离模式**从不**探测 9222：
// Agent Hub 体检不得因为「本机恰好开着调试端口的个人 Chrome」就把用户的登录态、
// Cookie 和桌面窗口卷进一次自动化体检（2026-09-23 审查结论）。
// 返回空串 = 自起浏览器（临时 Profile）。
func (r *QARunner) resolveAttachURL() string {
	if r.opts.Isolated {
		return ""
	}
	if r.opts.AttachCDP != "" {
		return r.opts.AttachCDP
	}
	if isPortListeningFn("127.0.0.1:9222") {
		return "http://127.0.0.1:9222"
	}
	return ""
}

// createSession 初始化真实浏览器会话。
func (r *QARunner) createSession(ctx context.Context) (BrowserSession, error) {
	if r.opts.SessionOverride != nil {
		return r.opts.SessionOverride, nil
	}

	if r.opts.Isolated && r.opts.AttachCDP != "" {
		return nil, fmt.Errorf("qa: isolated 模式禁止附着外部 Chrome：AttachCDP(%q) 与 Isolated 互斥，"+
			"无人消费方体检必须用临时 Profile 自起浏览器", r.opts.AttachCDP)
	}

	attachURL := r.resolveAttachURL()

	// 隔离模式固定无头：有头窗口会抢用户桌面前台，与「无人消费方」语义冲突。
	headless := r.opts.Headless
	if r.opts.Isolated {
		headless = true
	}

	factory := &browser.CDPFactory{
		Headless: headless,
	}

	cfg := browser.Config{
		AttachCDPURL: attachURL,
	}

	sess, err := factory.Create(ctx, cfg)
	if err != nil {
		if r.opts.Isolated {
			return nil, fmt.Errorf("qa: isolated 会话启动失败: %w (隔离模式不附着外部 Chrome，请检查本机 Chrome 可执行文件)", err)
		}
		return nil, fmt.Errorf("qa: failed to connect to browser: %w (提示: 请启动 Chrome 带 --remote-debugging-port=9222 或开启无头模式)", err)
	}

	return &SessionAdapter{Session: sess}, nil
}

// Run 执行完整的 QA 体检与报告签发流程。
func (r *QARunner) Run(ctx context.Context) (*QAReport, error) {
	startTime := time.Now()

	if err := os.MkdirAll(r.opts.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("qa: create output dir: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	notify := func(step, total int, msg string) {
		if r.opts.OnProgress != nil {
			r.opts.OnProgress(step, total, msg)
		}
	}

	totalSteps := len(r.opts.Viewports) + 2
	notify(1, totalSteps, "初始化浏览器连接并导航到目标页面: "+r.opts.URL)

	// 隔离模式每次开跑前先回收孤儿临时 Profile（上一轮若被 SIGKILL 掉会留下
	// 孤儿 headless Chrome + 临时目录；口径见 browser.ReapStaleTempProfiles）。
	// 放在这里而不是 Run 之外：只有真的要用临时 Profile 时才值得付这次扫描。
	if r.opts.Isolated {
		if n := browser.ReapStaleTempProfiles(30 * time.Minute); n > 0 {
			log.Printf("qa: isolated 启动前回收 %d 个孤儿临时 profile", n)
		}
	}

	session, err := r.createSession(ctx)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	if err := session.Navigate(ctx, r.opts.URL); err != nil {
		return nil, fmt.Errorf("qa: navigate to %s failed: %w", r.opts.URL, err)
	}

	// 注入全局异常监听
	_, _ = session.EvaluateScript(ctx, errorHookScript)

	var results []ViewportAuditResult

	for i, vp := range r.opts.Viewports {
		stepIdx := i + 2
		notify(stepIdx, totalSteps, fmt.Sprintf("体检视口 [%s] (%d × %d)...", vp.Name, vp.Width, vp.Height))
		vpStart := time.Now()

		if err := session.SetViewport(ctx, vp.Width, vp.Height, vp.Scale, vp.Mobile); err != nil {
			return nil, fmt.Errorf("qa: switch viewport %s failed: %w", vp.Name, err)
		}

		// 等待响应式重排与稳定
		time.Sleep(150 * time.Millisecond)

		rawMetrics, err := session.EvaluateScript(ctx, inspectScript)
		if err != nil {
			return nil, fmt.Errorf("qa: inspect metrics for %s failed: %w", vp.Name, err)
		}

		var metrics struct {
			ScrollWidth           int               `json:"scroll_width"`
			InnerWidth            int               `json:"inner_width"`
			HasHorizontalOverflow bool              `json:"has_horizontal_overflow"`
			OverflowDelta         int               `json:"overflow_delta"`
			OverflowElements      []OverflowElement `json:"overflow_elements"`
			SmallTargets          []SmallTarget     `json:"small_targets"`
			BrokenResources       []string          `json:"broken_resources"`
			ConsoleErrors         []ConsoleError    `json:"console_errors"`
		}
		if err := json.Unmarshal(rawMetrics, &metrics); err != nil {
			return nil, fmt.Errorf("qa: parse metrics json: %w", err)
		}

		shotBytes, _, _, err := session.CaptureScreenshot(ctx)
		if err != nil {
			return nil, fmt.Errorf("qa: capture screenshot for %s failed: %w", vp.Name, err)
		}

		shotFilename := fmt.Sprintf("screenshot_%s.png", vp.Name)
		shotPath := filepath.Join(r.opts.OutputDir, shotFilename)
		if err := os.WriteFile(shotPath, shotBytes, 0644); err != nil {
			return nil, fmt.Errorf("qa: write screenshot %s: %w", shotPath, err)
		}

		auditRes := ViewportAuditResult{
			Viewport:              vp,
			ScreenshotBytes:       shotBytes,
			ScreenshotPath:        shotPath,
			ScrollWidth:           metrics.ScrollWidth,
			InnerWidth:            metrics.InnerWidth,
			HasHorizontalOverflow: metrics.HasHorizontalOverflow,
			OverflowDelta:         metrics.OverflowDelta,
			OverflowElements:      metrics.OverflowElements,
			SmallTargets:          metrics.SmallTargets,
			ConsoleErrors:         metrics.ConsoleErrors,
			BrokenResources:       metrics.BrokenResources,
			AuditDurationMs:       time.Since(vpStart).Milliseconds(),
		}

		// 执行基准图像素差分
		if len(r.opts.RefImage) > 0 {
			diffPng, diffStats, err := ComputeDiff(r.opts.RefImage, shotBytes)
			if err == nil && len(diffPng) > 0 {
				diffFilename := fmt.Sprintf("diff_%s.png", vp.Name)
				diffPath := filepath.Join(r.opts.OutputDir, diffFilename)
				if err := os.WriteFile(diffPath, diffPng, 0644); err == nil {
					auditRes.DiffBytes = diffPng
					auditRes.DiffPath = diffPath
					auditRes.DiffStats = diffStats
				}
			}
		}

		results = append(results, auditRes)
	}

	notify(totalSteps, totalSteps, "渲染交付报告与签发凭证...")

	score, verdict, issues := CalculateScoreAndVerdict(results, len(r.opts.RefImage) > 0)

	report := &QAReport{
		URL:             r.opts.URL,
		Timestamp:       startTime,
		DurationMs:      time.Since(startTime).Milliseconds(),
		ExecutionStatus: ExecutionStatusExecuted,
		Viewports:       results,
		HealthScore:     score,
		Verdict:         verdict,
		SummaryIssues:   issues,
		RefImageUsed:    len(r.opts.RefImage) > 0,
	}

	htmlPath, err := RenderHTMLReport(report, r.opts.OutputDir)
	if err != nil {
		return nil, err
	}
	report.HTMLReport = htmlPath

	mdPath, err := RenderMarkdownReport(report, r.opts.OutputDir)
	if err != nil {
		return nil, err
	}
	report.MDReport = mdPath

	return report, nil
}
