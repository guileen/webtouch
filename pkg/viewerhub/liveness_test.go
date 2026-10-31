package viewerhub

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
)

// dyingSource 是一个「用户可能手动关掉那扇真窗口」的画面源。
type dyingSource struct {
	*fakeSource
	mu    sync.Mutex
	alive bool
}

func (d *dyingSource) ViewerAlive() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.alive
}

func (d *dyingSource) setAlive(alive bool) {
	d.mu.Lock()
	d.alive = alive
	d.mu.Unlock()
}

// 用户手动关掉真窗口（target 销毁）时，观看壳必须收到 sessionClosed 并收起面板：
// 2026-09-12 真机复跑现场是壳停在一张永不再更新的截图上，用户以为那是浏览器窗口。
func TestStepClosesTheSessionWhenThePageIsGone(t *testing.T) {
	hub, _ := newTestHub(t, false)
	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)

	source := &dyingSource{fakeSource: &fakeSource{}, alive: true}
	source.pushFrame("jpeg-1", 1)
	session, err := NewSession(hub, SessionSpec{ID: "ctx-1", AgentName: "A"}, source, nil)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	ctx := context.Background()
	if !session.Step(ctx) {
		t.Fatal("第一帧应当发布出去")
	}
	// 用户把那扇真窗口关掉了。
	source.setAlive(false)
	session.Step(ctx)

	lines := readLines(t, viewerSide, 2)
	if !strings.Contains(lines[0], `"frame"`) {
		t.Fatalf("第一行应当是页面帧: %s", lines[0])
	}
	if !strings.Contains(lines[1], viewerSessionClosedKind) {
		t.Fatalf("页面消失后应当告诉观看壳收起面板: %s", lines[1])
	}
	if source.stops == 0 {
		t.Fatal("会话关闭时应停掉 screencast")
	}
}
