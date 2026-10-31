package qa

import (
	"time"
	"uws/pkg/vlm"
)

// OverflowElement 记录触发横向滚动条溢出的 DOM 节点物理特征。
type OverflowElement struct {
	Tag        string `json:"tag"`
	ID         string `json:"id,omitempty"`
	ClassName  string `json:"class_name,omitempty"`
	Width      int    `json:"width"`
	Right      int    `json:"right"`
	OverflowPx int    `json:"overflow_px"`
}

// SmallTarget 记录点击热区小于 32x32px 的核心交互元素（按钮/链接/表单）。
type SmallTarget struct {
	Tag    string `json:"tag"`
	ID     string `json:"id,omitempty"`
	Text   string `json:"text,omitempty"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// ConsoleError 记录运行时被捕获的未处理异常或 Promise Rejection。
type ConsoleError struct {
	Type     string `json:"type"` // error | unhandledrejection | network_error
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	LineNo   int    `json:"lineno,omitempty"`
}

// ViewportAuditResult 是单个视口下物理体检与差分分析的完备快照。
type ViewportAuditResult struct {
	Viewport              Viewport          `json:"viewport"`
	ScreenshotBytes       []byte            `json:"-"`
	ScreenshotPath        string            `json:"screenshot_path"`
	DiffBytes             []byte            `json:"-"`
	DiffPath              string            `json:"diff_path,omitempty"`
	DiffStats             *vlm.DiffStats    `json:"diff_stats,omitempty"`
	ScrollWidth           int               `json:"scroll_width"`
	InnerWidth            int               `json:"inner_width"`
	HasHorizontalOverflow bool              `json:"has_horizontal_overflow"`
	OverflowDelta         int               `json:"overflow_delta"`
	OverflowElements      []OverflowElement `json:"overflow_elements,omitempty"`
	SmallTargets          []SmallTarget     `json:"small_targets,omitempty"`
	ConsoleErrors         []ConsoleError    `json:"console_errors,omitempty"`
	BrokenResources       []string          `json:"broken_resources,omitempty"`
	AuditDurationMs       int64             `json:"audit_duration_ms"`
}

// ExecutionStatus 表示真实执行事实。
type ExecutionStatus string

const (
	ExecutionStatusExecuted ExecutionStatus = "EXECUTED"
	ExecutionStatusNotRun   ExecutionStatus = "NOT_RUN"
)

// OverallVerdict 是交付验收单的综合评级结论。
type OverallVerdict string

const (
	VerdictPass          OverallVerdict = "PASS"
	VerdictFail          OverallVerdict = "FAIL"
	VerdictBlocked       OverallVerdict = "BLOCKED"
	VerdictNeedsEvidence OverallVerdict = "NEEDS_EVIDENCE"

	// 保留向后兼容别名
	VerdictWarning  OverallVerdict = "WARNING"
	VerdictCritical OverallVerdict = "CRITICAL"
)

// QAFailureResponse 当未跑通或被阻断时的机器结构。
type QAFailureResponse struct {
	ExecutionStatus ExecutionStatus `json:"execution_status"`
	Verdict         OverallVerdict  `json:"verdict"`
	ReasonCode      string          `json:"reason_code"`
	Error           string          `json:"error"`
}

// QAReport 是面向多端看板、CI/CD 与人工验收的自包含交付审计单。
type QAReport struct {
	URL             string                `json:"url"`
	Timestamp       time.Time             `json:"timestamp"`
	DurationMs      int64                 `json:"duration_ms"`
	ExecutionStatus ExecutionStatus       `json:"execution_status"`
	Viewports       []ViewportAuditResult `json:"viewports"`
	HealthScore     int                   `json:"health_score"` // 0 ~ 100
	Verdict         OverallVerdict        `json:"verdict"`
	ReasonCode      string                `json:"reason_code,omitempty"`
	SummaryIssues   []string              `json:"summary_issues"`
	RefImageUsed    bool                  `json:"ref_image_used"`
	HTMLReport      string                `json:"html_report_path,omitempty"`
	MDReport        string                `json:"md_report_path,omitempty"`
}
