package browser

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
	"uws/pkg/browser/modelbridge"

	"uws/pkg/browser/internal/wstest"
)

func b64(t *testing.T, s string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func TestCDPSessionSSECaptureWiring(t *testing.T) {
	push := make(chan string, 16)
	var writeMu sync.Mutex
	url := wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
		go func() {
			for {
				op, payload, err := wstest.ReadServerFrame(br)
				if err != nil {
					return
				}
				if op != wstest.OpText {
					continue
				}
				var msg map[string]any
				if err := json.Unmarshal(payload, &msg); err != nil {
					continue
				}
				id, _ := msg["id"].(float64)
				method, _ := msg["method"].(string)
				writeMu.Lock()
				switch method {
				case "Target.attachToTarget":
					resp := map[string]any{"id": int64(id), "result": map[string]any{"sessionId": "sess-1"}}
					b, _ := json.Marshal(resp)
					_ = wstest.WriteServerText(conn, b)
				default:
					// 其它命令（含 Network.streamResourceContent）返回空 result。
					resp := map[string]any{"id": int64(id), "result": map[string]any{}}
					b, _ := json.Marshal(resp)
					_ = wstest.WriteServerText(conn, b)
				}
				writeMu.Unlock()
			}
		}()
		for ev := range push {
			writeMu.Lock()
			_ = wstest.WriteServerText(conn, []byte(ev))
			writeMu.Unlock()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, err := newCDPClient(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	sess, err := c.attachTarget(ctx, "target-1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	s := &cdpSession{
		id:        "test-sse",
		client:    c,
		sessionID: sess,
		closedCh:  make(chan struct{}),
		inflight:  make(map[string]time.Time),
		chat:      modelbridge.NewStream(),
	}
	s.subscribe(ctx)

	// 1) requestWillBeSent 命中聊天端点（武装 streamResourceContent）。
	pushEvent(t, push, "Network.requestWillBeSent", map[string]any{
		"requestId": "req-1",
		"type":      "Fetch",
		"request":   map[string]any{"url": "https://chat.deepseek.com/api/v0/chat/completion"},
	})
	// 2) 武装完成前的首块增量（走 preFeed 队列）。
	pushEvent(t, push, "Network.dataReceived", map[string]any{
		"requestId": "req-1",
		"data":      b64(t, `data: {"delta":{"content":"你好"}}`+"\n\n"),
	})
	time.Sleep(200 * time.Millisecond) // 等武装 goroutine 返回
	// 3) 后续增量 + 收尾。
	pushEvent(t, push, "Network.dataReceived", map[string]any{
		"requestId": "req-1",
		"data":      b64(t, `data: {"delta":{"content":"，世界"}}`+"\n\ndata: [DONE]\n\n"),
	})
	pushEvent(t, push, "Network.loadingFinished", map[string]any{"requestId": "req-1"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap := s.ChatStreamSnapshot(); snap.Done && snap.Content == "你好，世界" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	snap := s.ChatStreamSnapshot()
	t.Fatalf("SSE 捕获未完成: %+v", snap)
}

func pushEvent(t *testing.T, push chan<- string, method string, params any) {
	t.Helper()
	env := map[string]any{
		"method":    method,
		"params":    params,
		"sessionId": "sess-1",
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	push <- string(b)
}
