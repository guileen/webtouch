// Package screencast 是单个 flattened CDP session 的 latest-frame 画面流（Page.startScreencast）。
//
// 2026-09-27 从 pkg/browser/screencast.go 出包（复杂度治理·阶段 1，单元 5）：本包不拥有 Chrome、
// WebSocket 或 UI——命令与事件订阅由宿主会话注入（Command/Ack），帧事件由宿主把 CDP 事件的
// sessionId/params 交给 HandleFrame。pkg/browser 保留同名类型别名与薄包装，既有消费方
// （pkg/engine、pkg/viewerhub、cmd/uws-viewer-demo）零改动。
package screencast

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrClosed 表示宿主 CDP 会话或其 target 已关闭，不能再开始画面流。
var ErrClosed = errors.New("screencast closed")

// Options 是 Page.startScreencast 的可配置缩略参数。零值使用保守的
// JPEG 缩略图默认值；这些数值是实现默认值，不代表已经过真机性能验证。
type Options struct {
	Format        string
	Quality       int
	MaxWidth      int
	MaxHeight     int
	EveryNthFrame int
}

// DefaultOptions 返回 P1 Viewer 的保守缩略图参数。
func DefaultOptions() Options {
	return Options{
		Format:        "jpeg",
		Quality:       60,
		MaxWidth:      640,
		MaxHeight:     480,
		EveryNthFrame: 1,
	}
}

// NormalizeOptions 把零值字段补成默认值，并拒绝非法取值。
func NormalizeOptions(in Options) (Options, error) {
	def := DefaultOptions()
	if in.Format == "" {
		in.Format = def.Format
	}
	if in.Quality == 0 {
		in.Quality = def.Quality
	}
	if in.MaxWidth == 0 {
		in.MaxWidth = def.MaxWidth
	}
	if in.MaxHeight == 0 {
		in.MaxHeight = def.MaxHeight
	}
	if in.EveryNthFrame == 0 {
		in.EveryNthFrame = def.EveryNthFrame
	}
	if in.Format != "jpeg" {
		return Options{}, fmt.Errorf("screencast: format must be jpeg, got %q", in.Format)
	}
	if in.Quality < 1 || in.Quality > 100 {
		return Options{}, fmt.Errorf("screencast: quality must be in [1,100], got %d", in.Quality)
	}
	if in.MaxWidth < 1 || in.MaxHeight < 1 || in.EveryNthFrame < 1 {
		return Options{}, errors.New("screencast: dimensions and everyNthFrame must be positive")
	}
	return in, nil
}

// Metadata 是 Chrome Page.screencastFrame 的页面坐标元数据。
// 坐标仍由调用方按自身壳层的缩放和内容边距换算；本层不引入 UI 类型。
type Metadata struct {
	OffsetTop       float64 `json:"offsetTop"`
	PageScaleFactor float64 `json:"pageScaleFactor"`
	DeviceWidth     int     `json:"deviceWidth"`
	DeviceHeight    int     `json:"deviceHeight"`
	ScrollOffsetX   float64 `json:"scrollOffsetX"`
	ScrollOffsetY   float64 `json:"scrollOffsetY"`
	Timestamp       float64 `json:"timestamp"`
}

// Frame 是一帧 JPEG 及其 Chrome 元数据。CDPSessionID 和 TargetID 让未来
// Viewer 在多会话情况下能验证来源；ScreencastSessionID 是本帧的 Chrome ACK 标识。
// JPEG 只属于当前 latest-frame 槽位。
type Frame struct {
	JPEG                []byte
	Metadata            Metadata
	CDPSessionID        string
	ScreencastSessionID int64
	TargetID            string
}

// Source 是宿主会话可选提供的画面流能力。它不改变 CDPSession 的
// 基础契约，现有模拟会话无需实现；未来 IPC / 原生 Viewer 可经此接口订阅并轮询最新帧。
type Source interface {
	StartScreencast(ctx context.Context, options Options) error
	StopScreencast(ctx context.Context) error
	LatestScreencastFrame() (Frame, bool)
}

// Command 是宿主注入的 CDP 会话命令通道（等响应）。
type Command func(ctx context.Context, method string, params any) error

// Ack 是宿主注入的免响应命令通道：Page.screencastFrameAck 必须在 reader 回调里
// 立刻发出、且不能等待 CDP 响应，否则 reader 自己无法路由响应而死锁。
type Ack func(method string, params any) error

// Stream 是单个 flattened CDP session 的 latest-frame 画面流。
type Stream struct {
	sessionID string
	targetID  string
	command   Command
	ack       Ack

	controlMu sync.Mutex // 串行化 start/stop，防止重复协议命令。
	mu        sync.RWMutex
	running   bool
	closed    bool
	latest    *Frame
	// options 记住最近一次 start 的参数：会话切换 Target（新窗口接管）后观看流
	// 必须用同一组参数在新页上重开，否则展开/画面质量会莫名回落到默认值。
	options Options
}

// New 建一条画面流：命令与事件订阅由宿主会话注入。
func New(sessionID, targetID string, command Command, ack Ack) *Stream {
	return &Stream{
		sessionID: sessionID,
		targetID:  targetID,
		command:   command,
		ack:       ack,
	}
}

// Start 开始推流。重复调用是幂等的（不会重复发协议命令）。
func (s *Stream) Start(ctx context.Context, options Options) error {
	options, err := NormalizeOptions(options)
	if err != nil {
		return err
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	s.mu.RLock()
	closed, running := s.closed, s.running
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if running {
		return nil
	}
	if s.command == nil {
		return errors.New("screencast: command transport unavailable")
	}
	params := map[string]any{
		"format":        options.Format,
		"quality":       options.Quality,
		"maxWidth":      options.MaxWidth,
		"maxHeight":     options.MaxHeight,
		"everyNthFrame": options.EveryNthFrame,
	}
	if err := s.command(ctx, "Page.startScreencast", params); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.running = true
	s.options = options
	s.mu.Unlock()
	return nil
}

// OptionsSnapshot 返回最近一次 start 的参数与是否在跑（供会话切换 Target 后重开）。
func (s *Stream) OptionsSnapshot() (Options, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.options, s.running
}

// Stop 停止推流并释放保留帧。重复调用安全，不会重复发协议命令。
func (s *Stream) Stop(ctx context.Context) error {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	s.mu.Lock()
	if !s.running {
		s.latest = nil
		s.mu.Unlock()
		return nil
	}
	s.running = false
	s.latest = nil
	closed := s.closed
	s.mu.Unlock()
	if closed || s.command == nil {
		return nil
	}
	return s.command(ctx, "Page.stopScreencast", nil)
}

// Release 在 target/session 消失时清空帧而不发送命令：目标已不可用，不能为了
// 清理而延长 Close 或向已销毁的 target 写入。
func (s *Stream) Release() {
	s.controlMu.Lock()
	s.mu.Lock()
	s.running = false
	s.closed = true
	s.latest = nil
	s.mu.Unlock()
	s.controlMu.Unlock()
}

// LatestFrame 返回保留帧的防御性拷贝。
func (s *Stream) LatestFrame() (Frame, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.running || s.latest == nil {
		return Frame{}, false
	}
	frame := *s.latest
	frame.JPEG = append([]byte(nil), frame.JPEG...)
	return frame, true
}

// LatestScreencastFrame 暴露 LatestFrame 给被动 Viewer 桥（Source 接口用）。
// 它不改变流的归属，也不启动任何 Chrome/CDP 活动。
func (s *Stream) LatestScreencastFrame() (Frame, bool) {
	return s.LatestFrame()
}

// HandleFrame 只能由当前 flattened session 的事件订阅调用（由宿主把 CDP 事件的
// sessionId/params 原样交进来）。ACK 在解析成功与否都立即发出，且绝不依赖 Viewer
// 消费速度；坏帧只被丢弃，不阻塞自动化。
func (s *Stream) HandleFrame(sessionID string, params []byte) {
	if sessionID != s.sessionID {
		return
	}
	var payload struct {
		Data      string   `json:"data"`
		Metadata  Metadata `json:"metadata"`
		SessionID int64    `json:"sessionId"`
	}
	if json.Unmarshal(params, &payload) != nil {
		return
	}
	// Page.screencastFrameAck 必须在 reader 回调中完成，但不能等待 CDP 响应，
	// 否则该 reader 自己无法路由响应而死锁。
	if s.ack != nil {
		_ = s.ack("Page.screencastFrameAck", map[string]any{"sessionId": payload.SessionID})
	}
	jpeg, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.closed {
		return
	}
	// 替换而非排队：慢 Viewer 最多持有这一帧，不会反压真实自动化。
	s.latest = &Frame{
		JPEG:                jpeg,
		Metadata:            payload.Metadata,
		CDPSessionID:        s.sessionID,
		ScreencastSessionID: payload.SessionID,
		TargetID:            s.targetID,
	}
}
