// Package scasttest 是 screencast 的测试支撑：假命令/ACK 通道与帧事件 params 构造。
//
// 夹具原先散在 pkg/browser/screencast_test.go，被 pkg/browser 与子包共用
// （2026-09-27 复杂度治理·阶段 1 单元 5 抽出，对齐 internal/wstest 的做法）。
package scasttest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"

	"uws/pkg/browser/screencast"
)

// Recorded 是一条被记录下来的 CDP 命令（或 ACK）。
type Recorded struct {
	Method string
	Params any
}

// Transport 是假命令通道：记录收到的命令/ACK，可注入错误。
type Transport struct {
	mu       sync.Mutex
	commands []Recorded
	acks     []Recorded
	err      error
}

// Command 是注入给 screencast.New 的命令通道。
func (f *Transport) Command(_ context.Context, method string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, Recorded{Method: method, Params: params})
	return f.err
}

// Ack 是注入给 screencast.New 的免响应通道。
func (f *Transport) Ack(method string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, Recorded{Method: method, Params: params})
	return nil
}

// SetErr 让后续命令返回该错误。
func (f *Transport) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Snapshot 返回已记录的命令与 ACK 的拷贝。
func (f *Transport) Snapshot() (commands, acks []Recorded) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Recorded(nil), f.commands...), append([]Recorded(nil), f.acks...)
}

// NewStream 建一条挂在固定 flattened session / target 上的画面流及其假通道。
func NewStream(sessionID, targetID string) (*screencast.Stream, *Transport) {
	t := &Transport{}
	return screencast.New(sessionID, targetID, t.Command, t.Ack), t
}

// FrameParams 构造 Page.screencastFrame 事件的 params（与 Chrome 线上格式一致）。
// 宿主侧测试把它交给 Stream.HandleFrame(sessionID, params)。
func FrameParams(t *testing.T, id int64, jpeg []byte, metadata screencast.Metadata) []byte {
	t.Helper()
	payload, err := json.Marshal(struct {
		Data      string              `json:"data"`
		Metadata  screencast.Metadata `json:"metadata"`
		SessionID int64               `json:"sessionId"`
	}{
		Data:      base64.StdEncoding.EncodeToString(jpeg),
		Metadata:  metadata,
		SessionID: id,
	})
	if err != nil {
		t.Fatalf("marshal screencast event: %v", err)
	}
	return payload
}
