package provider

import (
	"context"
	"time"
)

// Provider 向调用方（如 Agent Hub）暴露有界、确定性、经过脱敏的 WebTouch 浏览器执行能力。
type Provider interface {
	InspectPage(ctx context.Context, req InspectRequest) (*EvidenceBundle, error)
	CaptureDiff(ctx context.Context, req DiffRequest) (*EvidenceBundle, error)
}

// ViewportSpec 是 Provider 对外的视口数据契约（Provider 自有 DTO）。
// 调用方（Agent Hub / CLI）只依赖本类型，不通过 Provider 依赖 pkg/qa 的内部实现类型：
// 视口在 Provider 内部被翻译成 qa.Viewport 交给 QA Runner（适配层收在这里）。
type ViewportSpec struct {
	Name   string  `json:"name"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Scale  float64 `json:"scale"`
	Mobile bool    `json:"mobile"`
}

// session 是 Provider 内部消费的会话能力契约（宿主注入，仅测试与适配层使用）。
// 方法集与 qa.BrowserSession 一致，但类型留在 Provider 自己的边界内——
// 外部调用方不需要 import pkg/qa 就能理解 Provider 的请求结构。
type session interface {
	Navigate(ctx context.Context, url string) error
	SetViewport(ctx context.Context, width, height int, scale float64, mobile bool) error
	EvaluateScript(ctx context.Context, expr string) ([]byte, error)
	CaptureScreenshot(ctx context.Context) (png []byte, width, height int, err error)
	Close() error
}

// InspectRequest 描述页面体检入参，受到视口、超时与权限的有界约束。
type InspectRequest struct {
	URL       string         `json:"url"`
	Viewports []ViewportSpec `json:"viewports,omitempty"`
	Timeout   time.Duration  `json:"timeout,omitempty"`
	Headless  bool           `json:"headless"`
	// Isolated 是 Agent Hub 一类的无人消费方要用的强隔离模式：
	// 禁止自动探测 127.0.0.1:9222 附着个人 Chrome，只用临时 Profile 自起浏览器。
	// 显式 AttachCDP 与 Isolated 互斥（歧义即报错，见 qa.RunnerOptions）。
	Isolated  bool   `json:"isolated,omitempty"`
	OutputDir string `json:"output_dir,omitempty"`
	AttachCDP string `json:"attach_cdp,omitempty"`

	// sessionOverride 仅供测试注入桩会话；它不是对外契约的一部分
	// （不导出、不参与 JSON），避免把 QA 会话实现细节扩散给消费方。
	sessionOverride session
}

// DiffRequest 描述页面体检与基准图对比入参。
type DiffRequest struct {
	URL           string         `json:"url"`
	BaselineImage []byte         `json:"-"`
	BaselinePath  string         `json:"baseline_path,omitempty"`
	Viewports     []ViewportSpec `json:"viewports,omitempty"`
	Timeout       time.Duration  `json:"timeout,omitempty"`
	Headless      bool           `json:"headless"`
	Isolated      bool           `json:"isolated,omitempty"`
	OutputDir     string         `json:"output_dir,omitempty"`
	AttachCDP     string         `json:"attach_cdp,omitempty"`

	sessionOverride session
}

// ViewportMeta 描述单个视口下的脱敏物理审计指标（不包含大尺寸 base64 截图或原始网络流）。
type ViewportMeta struct {
	Name                  string `json:"name"`
	Width                 int    `json:"width"`
	Height                int    `json:"height"`
	HasHorizontalOverflow bool   `json:"has_horizontal_overflow"`
	OverflowDelta         int    `json:"overflow_delta"`
	SmallTargetCount      int    `json:"small_target_count"`
	ConsoleErrorCount     int    `json:"console_error_count"`
	BrokenResourceCount   int    `json:"broken_resource_count"`
	AuditDurationMs       int64  `json:"audit_duration_ms"`
}

// DiffStatsMeta 描述量化差分指标。
type DiffStatsMeta struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	DiffPixels  int     `json:"diff_pixels"`
	TotalPixels int     `json:"total_pixels"`
	Ratio       float64 `json:"ratio"`
	MaxChannel  int     `json:"max_channel"`
	BBox        string  `json:"bbox,omitempty"`
}

// SanityCheckMeta 包含各维度的语义健全性判定。
type SanityCheckMeta struct {
	LayoutIntegrity bool     `json:"layout_integrity"` // 布局完整（无横向溢出）
	ConsoleClean    bool     `json:"console_clean"`    // 控制台无未处理错误
	ResourcesOK     bool     `json:"resources_ok"`     // 关键静态资源无 404/失效
	TouchFriendly   bool     `json:"touch_friendly"`   // 触控热区达标（无过小按钮）
	FlaggedIssues   []string `json:"flagged_issues,omitempty"`
}

// EvidenceBundle 是确定性、防篡改、无机密凭据的高层浏览器证据包。
type EvidenceBundle struct {
	EvidenceID      string          `json:"evidence_id"`
	URL             string          `json:"url"` // 已脱敏 URL
	Timestamp       time.Time       `json:"timestamp"`
	ExecutionStatus string          `json:"execution_status"` // EXECUTED / NOT_RUN
	Verdict         string          `json:"verdict"`          // PASS / FAIL / BLOCKED / NEEDS_EVIDENCE
	HealthScore     int             `json:"health_score"`
	Viewports       []ViewportMeta  `json:"viewports"`
	DiffStats       *DiffStatsMeta  `json:"diff_stats,omitempty"`
	SanityCheck     SanityCheckMeta `json:"sanity_check"`
	RedactedCount   int             `json:"redacted_count"`
	BundleDigestSHA string          `json:"bundle_digest_sha256"`
	Summary         string          `json:"summary"`
}
