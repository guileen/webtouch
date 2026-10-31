// 浏览器宿主自愈（2026-10-01 用户口径「关窗 ≠ 断链路」）。
//
// 现象：用户只关掉调试 Chrome 窗口，网关进程仍活（/v1/health 200），但 9222 没了，
// 网页通道失去宿主 ⇒ 退回游客模式、模型被锁 ⇒ 推理 5xx。
// 现状：EnsureChrome 只在**启动那一刻**跑一次，之后宿主消失无人再拉起。
//
// 本文件把宿主纳入网关照管：周期探活 + 失联即用固定 user-data-dir profile 重新拉起
// （登录态在 profile 里，一次登录长期有效），并把宿主状态并进 /v1/health。
package gateway

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBrowserHostInterval 是宿主探活周期（UWS_BROWSER_HOST_INTERVAL 覆盖，秒）。
	DefaultBrowserHostInterval = 15 * time.Second
	browserHostProbeTimeout    = 1500 * time.Millisecond
	// browserHostRelaunchCooldown 限制连续拉起的最小间隔，避免探活抖动把 Chrome 反复拉起。
	browserHostRelaunchCooldown = 5 * time.Second
)

// BrowserHostStatus 是宿主运行态快照（进 /v1/health，字段 append-only）。
type BrowserHostStatus struct {
	Managed     bool   `json:"managed"`                // 是否由网关纳管（attach-cdp 非空）
	Endpoint    string `json:"endpoint,omitempty"`     // 探活的 CDP 端点
	Attached    bool   `json:"attached"`               // 最近一次探活是否通
	Relaunches  int    `json:"relaunches"`             // 累计自动拉起次数
	DownSince   string `json:"down_since,omitempty"`   // 最近一次失联时刻（RFC3339；恢复后清空）
	LastError   string `json:"last_error,omitempty"`   // 最近一次探活失败原因
	LaunchError string `json:"launch_error,omitempty"` // 最近一次自动拉起失败原因（成功后清空）
	LastLaunch  string `json:"last_launch,omitempty"`
	LastProbeOK string `json:"last_probe_ok,omitempty"`
	Profile     string `json:"profile,omitempty"`
}

// BrowserHost 是调试 Chrome 的守护者：周期探活，宿主消失即自动拉起。
type BrowserHost struct {
	endpoint   string
	profileDir string
	interval   time.Duration
	logger     *log.Logger

	// probe / launch 可注入，便于单测（生产分别为 ProbeChrome / EnsureChrome）。
	probe  func(context.Context, string) error
	launch func(context.Context, string, string, *log.Logger) error
	now    func() time.Time

	mu     sync.Mutex
	st     BrowserHostStatus
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewBrowserHost 构造宿主守护者；endpoint 为空表示宿主不归网关管（引擎自建隔离 Chrome）。
func NewBrowserHost(endpoint, profileDir string, interval time.Duration, logger *log.Logger) *BrowserHost {
	if logger == nil {
		logger = log.Default()
	}
	if interval <= 0 {
		interval = DefaultBrowserHostInterval
	}
	return &BrowserHost{
		endpoint:   strings.TrimSpace(endpoint),
		profileDir: profileDir,
		interval:   interval,
		logger:     logger,
		probe:      ProbeChrome,
		launch:     EnsureChrome,
		now:        time.Now,
		st:         BrowserHostStatus{Endpoint: strings.TrimSpace(endpoint), Profile: profileDir},
	}
}

// BrowserHostIntervalFromEnv 读取探活周期覆盖（秒；<=0 或非法用默认）。
func BrowserHostIntervalFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv("UWS_BROWSER_HOST_INTERVAL"))
	if v == "" {
		return DefaultBrowserHostInterval
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec <= 0 {
		return DefaultBrowserHostInterval
	}
	return time.Duration(sec) * time.Second
}

// Managed 表示宿主是否由网关纳管（false 时守护循环不启动）。
func (b *BrowserHost) Managed() bool { return b != nil && b.endpoint != "" }

// Start 启动守护循环：先立即探一次，此后每 interval 一次；失联即拉起。
func (b *BrowserHost) Start(parent context.Context) {
	if !b.Managed() {
		return
	}
	b.mu.Lock()
	if b.cancel != nil {
		b.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	b.cancel = cancel
	b.st.Managed = true
	b.mu.Unlock()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.tick(ctx)
		ticker := time.NewTicker(b.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.tick(ctx)
			}
		}
	}()
	b.logger.Printf("浏览器宿主纳管: %s（探活 %s，profile=%q）", b.endpoint, b.interval, b.profileDir)
}

// Stop 停止守护循环（幂等）。
func (b *BrowserHost) Stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	cancel := b.cancel
	b.cancel = nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
		b.wg.Wait()
	}
}

// tick 是一次「探活 → 必要时拉起」。
func (b *BrowserHost) tick(ctx context.Context) {
	if !b.Managed() {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, browserHostProbeTimeout)
	err := b.probe(probeCtx, b.endpoint)
	cancel()
	if err == nil {
		b.markUp()
		return
	}
	b.markDown(err)
	if !chromeLaunchAllowed() {
		return // 未授权不启动 Chrome（fail-closed，与 EnsureChrome 同口径）
	}
	if !b.cooldownElapsed() {
		return
	}
	launchCtx, cancelLaunch := context.WithTimeout(ctx, 30*time.Second)
	defer cancelLaunch()
	lerr := b.launch(launchCtx, b.endpoint, b.profileDir, b.logger)
	b.noteLaunchAttempt()
	if lerr != nil {
		b.logger.Printf("浏览器宿主自愈失败: %v（端点 %s）", lerr, b.endpoint)
		b.setError(lerr)
		return
	}
	b.markRelaunched()
}

func (b *BrowserHost) markUp() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.st.Attached {
		b.logger.Printf("浏览器宿主已就绪: %s", b.endpoint)
	}
	b.st.Attached = true
	b.st.DownSince = ""
	b.st.LastError = ""
	b.st.LaunchError = ""
	b.st.LastProbeOK = b.now().Format(time.RFC3339)
}

func (b *BrowserHost) markDown(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Attached || b.st.DownSince == "" {
		b.logger.Printf("浏览器宿主失联: %s（%v），尝试自动拉起", b.endpoint, err)
	}
	b.st.Attached = false
	if b.st.DownSince == "" {
		b.st.DownSince = b.now().Format(time.RFC3339)
	}
	b.st.LastError = err.Error()
}

func (b *BrowserHost) setError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.LaunchError = err.Error()
}

// noteLaunchAttempt 记录一次拉起尝试时刻（成功与否都记，冷却据此生效）。
func (b *BrowserHost) noteLaunchAttempt() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.LastLaunch = b.now().Format(time.RFC3339)
}

func (b *BrowserHost) markRelaunched() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.Attached = true
	b.st.Relaunches++
	b.st.DownSince = ""
	b.st.LastError = ""
	b.st.LaunchError = ""
	b.st.LastProbeOK = b.now().Format(time.RFC3339)
	b.logger.Printf("浏览器宿主已自动恢复: %s（第 %d 次拉起）", b.endpoint, b.st.Relaunches)
}

func (b *BrowserHost) cooldownElapsed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.LastLaunch == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, b.st.LastLaunch)
	if err != nil {
		return true
	}
	return b.now().Sub(t) >= browserHostRelaunchCooldown
}

// Snapshot 返回宿主状态快照（零拷贝副本）。
func (b *BrowserHost) Snapshot() BrowserHostStatus {
	if b == nil {
		return BrowserHostStatus{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.st
	st.Managed = b.endpoint != ""
	return st
}

// Browser availability belongs to the executor; all model facades in Hub share
// this fail-closed check before invoking the browser engine.
func (h *Handler) browserProviderReady(w http.ResponseWriter, r *http.Request) bool {
	st := h.g.BrowserHostStatus()
	if !st.Managed || st.Attached {
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 800*time.Millisecond)
	defer cancel()
	if ProbeChrome(ctx, st.Endpoint) == nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{
		"type": "channel_unavailable", "code": "browser_host_down", "semantic": "browser_host_down",
		"message": "浏览器宿主不可达，请恢复对应浏览器", "retryable": false, "action": "abort",
	}})
	return false
}
