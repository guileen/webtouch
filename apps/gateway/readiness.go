// 端到端就绪读数（2026-10-01 用户口径：通道可用 = 进程 + 宿主 + 一次真实推理）。
//
// 网关进程活着、/v1/health 返回 200，并不代表链路可用——宿主（调试 Chrome）掉了、
// 或页面退回游客模式被锁模型，健康检查照样是 200。本文件把「一次真实推理」的
// 结果作为可用性证据记录下来，由 /v1/health 暴露 ready 字段。
package gateway

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultReadinessTTL 是一次成功推理的有效期（UWS_READY_TTL 秒覆盖）。
const DefaultReadinessTTL = 10 * time.Minute

type readinessState struct {
	mu       sync.Mutex
	lastOK   time.Time
	lastErr  string
	lastMs   int64
	deepRuns int
}

// ReadinessTTLFromEnv 读取就绪有效期（秒；<=0 或非法用默认）。
func ReadinessTTLFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv("UWS_READY_TTL"))
	if v == "" {
		return DefaultReadinessTTL
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec <= 0 {
		return DefaultReadinessTTL
	}
	return time.Duration(sec) * time.Second
}

// RecordInferenceOK 记录一次成功的真实推理（端到端证据）。
func (g *Gateway) RecordInferenceOK(took time.Duration) {
	if g == nil {
		return
	}
	g.readiness.mu.Lock()
	defer g.readiness.mu.Unlock()
	g.readiness.lastOK = time.Now()
	g.readiness.lastErr = ""
	g.readiness.lastMs = took.Milliseconds()
	g.readiness.deepRuns++
}

// RecordInferenceErr 记录一次失败的端到端探测（不清空此前的成功时间，只标记当前不可用）。
func (g *Gateway) RecordInferenceErr(err error) {
	if g == nil || err == nil {
		return
	}
	g.readiness.mu.Lock()
	defer g.readiness.mu.Unlock()
	g.readiness.lastErr = err.Error()
	g.readiness.deepRuns++
}

// Readiness 返回端到端可用性：最近一次真实推理是否成功且在有效期内。
func (g *Gateway) Readiness(ttl time.Duration) (ready bool, lastOK string, lastErr string, ms int64) {
	if g == nil {
		return false, "", "gateway 未初始化", 0
	}
	if ttl <= 0 {
		ttl = DefaultReadinessTTL
	}
	g.readiness.mu.Lock()
	defer g.readiness.mu.Unlock()
	lastErr = g.readiness.lastErr
	ms = g.readiness.lastMs
	if !g.readiness.lastOK.IsZero() {
		lastOK = g.readiness.lastOK.Format(time.RFC3339)
		ready = lastErr == "" && time.Since(g.readiness.lastOK) <= ttl
	}
	return ready, lastOK, lastErr, ms
}

// StartBrowserSupervisor 启动宿主守护循环（attach-cdp 为空时为空操作）。
func (g *Gateway) StartBrowserSupervisor(ctx context.Context) {
	if g == nil || g.browserHost == nil {
		return
	}
	g.browserHost.Start(ctx)
}

// StopBrowserSupervisor 停止宿主守护循环（幂等，供测试与优雅退出使用）。
func (g *Gateway) StopBrowserSupervisor() {
	if g == nil || g.browserHost == nil {
		return
	}
	g.browserHost.Stop()
}

// BrowserHostStatus 返回宿主运行态（未纳管时 managed=false）。
func (g *Gateway) BrowserHostStatus() BrowserHostStatus {
	if g == nil || g.browserHost == nil {
		return BrowserHostStatus{}
	}
	return g.browserHost.Snapshot()
}

// DeepCheck 跑一次真实推理作为端到端证据（最小 prompt，走既有 chat 主流程）。
// model 为空时取模型表第一个 web 通道模型。
func (g *Gateway) DeepCheck(ctx context.Context, model, prompt string) (string, error) {
	if g == nil || g.cfg == nil {
		return "", &ChatError{Reason: "gateway 未初始化"}
	}
	if strings.TrimSpace(model) == "" {
		for _, m := range g.cfg.ModelTable() {
			model = m.ID
			break
		}
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "ping"
	}
	ch, _, err := g.cfg.ChannelByModel(model)
	if err != nil {
		return "", err
	}
	start := time.Now()
	out, err := g.Chat(ctx, ChatRequest{
		ChannelID: ch.ID,
		Model:     model,
		SessionID: "health-deep",
		Messages:  []Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		g.RecordInferenceErr(err)
		return "", err
	}
	g.RecordInferenceOK(time.Since(start))
	if len(out.Choices) == 0 {
		return "", nil
	}
	return out.Choices[0].Message.Content, nil
}
