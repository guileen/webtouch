package qa

import (
	"encoding/base64"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uws/pkg/vlm"
)

// CalculateScoreAndVerdict 综合计算多端健康度评分与权威裁决判定。
func CalculateScoreAndVerdict(viewports []ViewportAuditResult, hasRefImg bool) (int, OverallVerdict, []string) {
	score := 100
	var issues []string
	hasOverflow := false
	totalConsoleErrors := 0
	totalBrokenRes := 0
	totalSmallTargets := 0

	for _, vp := range viewports {
		if vp.HasHorizontalOverflow {
			hasOverflow = true
			score -= 30
			issues = append(issues, fmt.Sprintf("视口 [%s (%dx%d)] 发生严重横向滚动条溢出 (+%dpx)",
				vp.Viewport.Name, vp.Viewport.Width, vp.Viewport.Height, vp.OverflowDelta))
		}
		totalConsoleErrors += len(vp.ConsoleErrors)
		totalBrokenRes += len(vp.BrokenResources)
		totalSmallTargets += len(vp.SmallTargets)

		if hasRefImg && vp.DiffStats != nil && vp.DiffStats.Ratio > 0.05 {
			score -= 15
			issues = append(issues, fmt.Sprintf("视口 [%s] 与参考设计稿像素差异率过高 (%.2f%%)",
				vp.Viewport.Name, vp.DiffStats.Ratio*100))
		}
	}

	if totalConsoleErrors > 0 {
		deduct := min(totalConsoleErrors*10, 30)
		score -= deduct
		issues = append(issues, fmt.Sprintf("捕获到 %d 个运行时控制台未捕获异常/报错", totalConsoleErrors))
	}

	if totalBrokenRes > 0 {
		deduct := min(totalBrokenRes*10, 25)
		score -= deduct
		issues = append(issues, fmt.Sprintf("检测到 %d 处资源加载失败 (裂图/4xx/5xx 死链)", totalBrokenRes))
	}

	if totalSmallTargets > 0 {
		deduct := min(totalSmallTargets*2, 10)
		score -= deduct
		issues = append(issues, fmt.Sprintf("检测到 %d 个核心交互元素点击热区小于 32x32px", totalSmallTargets))
	}

	if score < 0 {
		score = 0
	} else if score > 100 {
		score = 100
	}

	var verdict OverallVerdict
	if hasOverflow || score < 70 {
		verdict = VerdictCritical
	} else if score < 90 || totalConsoleErrors > 0 || totalBrokenRes > 0 {
		verdict = VerdictWarning
	} else {
		verdict = VerdictPass
	}

	return score, verdict, issues
}

// ComputeDiff 对比基准图与视口截图，生成洋红高亮差异图与统计指标。
func ComputeDiff(refBytes, shotBytes []byte) ([]byte, *vlm.DiffStats, error) {
	if len(refBytes) == 0 || len(shotBytes) == 0 {
		return nil, nil, nil
	}
	diffPng, stats, err := vlm.DiffImages(refBytes, shotBytes)
	if err != nil {
		return nil, nil, err
	}
	return diffPng, &stats, nil
}

// RenderHTMLReport 生成自包含、排版精美的现代 HTML 交付验收单。
func RenderHTMLReport(report *QAReport, outDir string) (string, error) {
	var verdictColor, verdictBg string
	switch report.Verdict {
	case VerdictPass:
		verdictColor = "#10b981"
		verdictBg = "rgba(16, 185, 129, 0.15)"
	case VerdictWarning:
		verdictColor = "#f59e0b"
		verdictBg = "rgba(245, 158, 11, 0.15)"
	default:
		verdictColor = "#ef4444"
		verdictBg = "rgba(239, 68, 68, 0.15)"
	}

	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>WebTouch QA Studio 交付验收单 - ` + html.EscapeString(report.URL) + `</title>
<style>
:root {
  --bg: #0d1117;
  --surface: #161b22;
  --border: #30363d;
  --text: #c9d1d9;
  --text-muted: #8b949e;
  --heading: #f0f6fc;
  --accent: #58a6ff;
  --danger: #f85149;
  --warning: #d29922;
  --success: #3fb950;
  --magenta: #ff00ff;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
  background: var(--bg);
  color: var(--text);
  line-height: 1.6;
  padding: 24px;
}
.container { max-width: 1280px; margin: 0 auto; }
.header {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 24px;
  margin-bottom: 24px;
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 16px;
}
.title-group h1 { font-size: 22px; color: var(--heading); margin-bottom: 6px; }
.meta-info { font-size: 13px; color: var(--text-muted); }
.score-badge {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 12px 20px;
  border-radius: 10px;
  border: 2px solid ` + verdictColor + `;
  background: ` + verdictBg + `;
}
.score-number { font-size: 36px; font-weight: 800; color: ` + verdictColor + `; }
.score-label { font-size: 13px; text-transform: uppercase; font-weight: 700; color: ` + verdictColor + `; }
.summary-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 16px;
  margin-bottom: 24px;
}
.summary-card {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 16px;
}
.summary-card .val { font-size: 24px; font-weight: 700; color: var(--heading); }
.summary-card .lbl { font-size: 13px; color: var(--text-muted); margin-top: 4px; }
.issues-section {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 20px;
  margin-bottom: 24px;
}
.issues-section h3 { font-size: 16px; color: var(--heading); margin-bottom: 12px; }
.issue-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 12px;
  margin-bottom: 6px;
  border-radius: 6px;
  background: rgba(239, 68, 68, 0.1);
  color: #fca5a5;
  font-size: 14px;
}
.viewports-grid {
  display: flex;
  flex-direction: column;
  gap: 32px;
}
.viewport-card {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 20px;
}
.viewport-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 16px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--border);
}
.viewport-title { font-size: 18px; font-weight: 600; color: var(--heading); }
.viewport-tags { display: flex; gap: 8px; font-size: 12px; }
.tag { padding: 4px 8px; border-radius: 4px; background: #21262d; color: var(--text-muted); }
.tag.danger { background: rgba(239, 68, 68, 0.2); color: #f85149; font-weight: 600; }
.tag.success { background: rgba(63, 185, 80, 0.2); color: #3fb950; }
.screens-row {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
  gap: 20px;
  align-items: start;
}
.screen-col h4 { font-size: 14px; color: var(--text-muted); margin-bottom: 8px; }
.screen-img {
  width: 100%;
  max-height: 520px;
  object-fit: contain;
  background: #000;
  border-radius: 8px;
  border: 1px solid var(--border);
}
.metrics-table {
  width: 100%;
  margin-top: 16px;
  border-collapse: collapse;
  font-size: 13px;
}
.metrics-table th, .metrics-table td {
  padding: 8px 12px;
  text-align: left;
  border-bottom: 1px solid var(--border);
}
.metrics-table th { color: var(--text-muted); font-weight: 600; }
.sign-off {
  margin-top: 32px;
  padding: 24px;
  background: var(--surface);
  border: 1px dashed var(--border);
  border-radius: 12px;
  text-align: center;
  font-size: 14px;
  color: var(--text-muted);
}
</style>
</head>
<body>
<div class="container">
  <div class="header">
    <div class="title-group">
      <h1>WebTouch QA Studio · 前端多端交付验收单</h1>
      <div class="meta-info">目标 URL: <strong>` + html.EscapeString(report.URL) + `</strong> | 耗时: ` + fmt.Sprintf("%dms", report.DurationMs) + ` | 签发时间: ` + report.Timestamp.Format("2006-01-02 15:04:05") + `</div>
    </div>
    <div class="score-badge">
      <div class="score-number">` + fmt.Sprintf("%d", report.HealthScore) + `</div>
      <div>
        <div class="score-label">` + string(report.Verdict) + `</div>
        <div style="font-size: 11px; color: var(--text-muted);">综合交付健康分</div>
      </div>
    </div>
  </div>

  <div class="summary-grid">
    <div class="summary-card">
      <div class="val">` + fmt.Sprintf("%d", len(report.Viewports)) + `</div>
      <div class="lbl">扫描视口数 (Mobile / Tablet / Desktop)</div>
    </div>
    <div class="summary-card">
      <div class="val" style="color: ` + map[bool]string{true: "var(--danger)", false: "var(--success)"}[report.HealthScore < 70] + `;">` + map[bool]string{true: "发现异常", false: "无溢出 (PASS)"}[hasAnyOverflow(report)] + `</div>
      <div class="lbl">响应式物理溢出拦截状态</div>
    </div>
    <div class="summary-card">
      <div class="val">` + fmt.Sprintf("%d", countTotalErrors(report)) + `</div>
      <div class="lbl">运行时暗病 (Console / Promise 异常)</div>
    </div>
    <div class="summary-card">
      <div class="val">` + fmt.Sprintf("%d", countTotalSmallTargets(report)) + `</div>
      <div class="lbl">人机工效热区违规 (&lt; 32x32px)</div>
    </div>
  </div>
`)

	if len(report.SummaryIssues) > 0 {
		sb.WriteString(`  <div class="issues-section">
    <h3>⚠️ 交付拦截项与体检问题清单</h3>
`)
		for _, issue := range report.SummaryIssues {
			sb.WriteString(`    <div class="issue-item"><span>✕</span> ` + html.EscapeString(issue) + `</div>` + "\n")
		}
		sb.WriteString(`  </div>` + "\n")
	}

	sb.WriteString(`  <div class="viewports-grid">` + "\n")
	for _, vp := range report.Viewports {
		sb.WriteString(`    <div class="viewport-card">
      <div class="viewport-header">
        <div class="viewport-title">📱 ` + html.EscapeString(strings.ToUpper(vp.Viewport.Name)) + ` 视口 (` + fmt.Sprintf("%d × %d", vp.Viewport.Width, vp.Viewport.Height) + `)</div>
        <div class="viewport-tags">
`)
		if vp.HasHorizontalOverflow {
			sb.WriteString(`          <span class="tag danger">横向溢出 +` + fmt.Sprintf("%dpx", vp.OverflowDelta) + `</span>` + "\n")
		} else {
			sb.WriteString(`          <span class="tag success">布局无溢出 (PASS)</span>` + "\n")
		}
		if len(vp.ConsoleErrors) > 0 {
			sb.WriteString(`          <span class="tag danger">` + fmt.Sprintf("%d 个控制台错误", len(vp.ConsoleErrors)) + `</span>` + "\n")
		}
		sb.WriteString(`        </div>
      </div>
      <div class="screens-row">
        <div class="screen-col">
          <h4>📷 页面快照 (` + fmt.Sprintf("%d × %d", vp.Viewport.Width, vp.Viewport.Height) + `)</h4>
`)
		if len(vp.ScreenshotBytes) > 0 {
			b64 := base64.StdEncoding.EncodeToString(vp.ScreenshotBytes)
			sb.WriteString(`          <img class="screen-img" src="data:image/png;base64,` + b64 + `" alt="Screenshot" />` + "\n")
		} else if vp.ScreenshotPath != "" {
			rel, _ := filepath.Rel(outDir, vp.ScreenshotPath)
			sb.WriteString(`          <img class="screen-img" src="` + html.EscapeString(rel) + `" alt="Screenshot" />` + "\n")
		}
		sb.WriteString(`        </div>` + "\n")

		if len(vp.DiffBytes) > 0 {
			b64 := base64.StdEncoding.EncodeToString(vp.DiffBytes)
			ratioStr := "0%"
			if vp.DiffStats != nil {
				ratioStr = fmt.Sprintf("%.2f%% (BBox: %s)", vp.DiffStats.Ratio*100, vp.DiffStats.BBox)
			}
			sb.WriteString(`        <div class="screen-col">
          <h4>🔍 视觉基准像素差分 (洋红高亮 Overlay: ` + ratioStr + `)</h4>
          <img class="screen-img" src="data:image/png;base64,` + b64 + `" alt="Diff Overlay" />
        </div>` + "\n")
		}
		sb.WriteString(`      </div>` + "\n")

		// 详细物理指标表
		sb.WriteString(`      <table class="metrics-table">
        <thead>
          <tr><th>指标分类</th><th>测量读数</th><th>合格基准</th><th>结论</th></tr>
        </thead>
        <tbody>
          <tr>
            <td>几何溢出判定 (scrollWidth vs innerWidth)</td>
            <td>` + fmt.Sprintf("scrollWidth: %dpx / innerWidth: %dpx", vp.ScrollWidth, vp.InnerWidth) + `</td>
            <td>scrollWidth &le; innerWidth</td>
            <td>` + map[bool]string{true: "<span style='color:var(--danger);font-weight:bold;'>FAIL</span>", false: "<span style='color:var(--success);'>PASS</span>"}[vp.HasHorizontalOverflow] + `</td>
          </tr>
          <tr>
            <td>控制台暗病监听 (Console / Exception)</td>
            <td>` + fmt.Sprintf("%d 个报错", len(vp.ConsoleErrors)) + `</td>
            <td>0 报错</td>
            <td>` + map[bool]string{true: "<span style='color:var(--danger);font-weight:bold;'>FAIL</span>", false: "<span style='color:var(--success);'>PASS</span>"}[len(vp.ConsoleErrors) > 0] + `</td>
          </tr>
          <tr>
            <td>静态资源加载状态 (Dead links / 裂图)</td>
            <td>` + fmt.Sprintf("%d 处异常", len(vp.BrokenResources)) + `</td>
            <td>0 裂图</td>
            <td>` + map[bool]string{true: "<span style='color:var(--danger);font-weight:bold;'>FAIL</span>", false: "<span style='color:var(--success);'>PASS</span>"}[len(vp.BrokenResources) > 0] + `</td>
          </tr>
          <tr>
            <td>人机工效热区规范 (&ge; 32x32px)</td>
            <td>` + fmt.Sprintf("%d 个违规元素", len(vp.SmallTargets)) + `</td>
            <td>核心交互元素 &ge; 32x32px</td>
            <td>` + map[bool]string{true: "<span style='color:var(--warning);font-weight:bold;'>WARN</span>", false: "<span style='color:var(--success);'>PASS</span>"}[len(vp.SmallTargets) > 0] + `</td>
          </tr>
        </tbody>
      </table>
    </div>` + "\n")
	}

	sb.WriteString(`  </div>

  <div class="sign-off">
    <p>WebTouch QA Studio · 由亚毫秒拓扑感知核、差分张量分析与多模态义眼联合驱动</p>
    <p style="margin-top: 4px; font-size: 12px;">符合《WebTouch 物理不变量与交付质量工程规范》· 自动化签发凭证</p>
  </div>
</div>
</body>
</html>`)

	reportPath := filepath.Join(outDir, "qa_report.html")
	if err := os.WriteFile(reportPath, []byte(sb.String()), 0644); err != nil {
		return "", fmt.Errorf("qa: write html report: %w", err)
	}
	return reportPath, nil
}

// RenderMarkdownReport 生成面向 CI/CD PR 评论与终端查看的 Markdown 交付报告。
func RenderMarkdownReport(report *QAReport, outDir string) (string, error) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# WebTouch QA Studio · 前端多端交付验收单\n\n"))
	sb.WriteString(fmt.Sprintf("- **目标 URL**: `%s`\n", report.URL))
	sb.WriteString(fmt.Sprintf("- **交付评级**: `%s`\n", report.Verdict))
	sb.WriteString(fmt.Sprintf("- **综合健康分**: `%d / 100`\n", report.HealthScore))
	sb.WriteString(fmt.Sprintf("- **总耗时**: `%dms`\n", report.DurationMs))
	sb.WriteString(fmt.Sprintf("- **签发时间**: `%s`\n\n", report.Timestamp.Format(time.RFC3339)))

	sb.WriteString("## 1. 物理不变量体检概览\n\n")
	sb.WriteString("| 视口名称 | 视口尺寸 | 横向溢出状态 | 控制台错误数 | 裂图/死链数 | 点击热区过小数 |\n")
	sb.WriteString("|---|---|---|---|---|---|\n")
	for _, vp := range report.Viewports {
		overflowStr := "✅ PASS"
		if vp.HasHorizontalOverflow {
			overflowStr = fmt.Sprintf("❌ FAIL (+%dpx)", vp.OverflowDelta)
		}
		sb.WriteString(fmt.Sprintf("| %s | %dx%d | %s | %d | %d | %d |\n",
			vp.Viewport.Name, vp.Viewport.Width, vp.Viewport.Height,
			overflowStr, len(vp.ConsoleErrors), len(vp.BrokenResources), len(vp.SmallTargets)))
	}
	sb.WriteString("\n")

	if len(report.SummaryIssues) > 0 {
		sb.WriteString("## 2. 缺陷与风险清单\n\n")
		for _, issue := range report.SummaryIssues {
			sb.WriteString(fmt.Sprintf("- ⚠️ %s\n", issue))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## 3. 多端截屏与交付物\n\n")
	for _, vp := range report.Viewports {
		sb.WriteString(fmt.Sprintf("### %s (%dx%d)\n", strings.ToUpper(vp.Viewport.Name), vp.Viewport.Width, vp.Viewport.Height))
		if vp.ScreenshotPath != "" {
			rel, _ := filepath.Rel(outDir, vp.ScreenshotPath)
			sb.WriteString(fmt.Sprintf("- 页面截屏: `%s`\n", rel))
		}
		if vp.DiffPath != "" {
			rel, _ := filepath.Rel(outDir, vp.DiffPath)
			ratio := 0.0
			bbox := "N/A"
			if vp.DiffStats != nil {
				ratio = vp.DiffStats.Ratio * 100
				bbox = vp.DiffStats.BBox
			}
			sb.WriteString(fmt.Sprintf("- 像素差分图: `%s` (差异率: `%.2f%%`, BBox: `%s`)\n", rel, ratio, bbox))
		}
		sb.WriteString("\n")
	}

	mdPath := filepath.Join(outDir, "qa_report.md")
	if err := os.WriteFile(mdPath, []byte(sb.String()), 0644); err != nil {
		return "", fmt.Errorf("qa: write markdown report: %w", err)
	}
	return mdPath, nil
}

func hasAnyOverflow(r *QAReport) bool {
	for _, v := range r.Viewports {
		if v.HasHorizontalOverflow {
			return true
		}
	}
	return false
}

func countTotalErrors(r *QAReport) int {
	total := 0
	for _, v := range r.Viewports {
		total += len(v.ConsoleErrors)
	}
	return total
}

func countTotalSmallTargets(r *QAReport) int {
	total := 0
	for _, v := range r.Viewports {
		total += len(v.SmallTargets)
	}
	return total
}
