package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
	"uws/pkg/browser/internal/wstest"
)

// fakeCDPServer 是一个最小 CDP 服务器：命令回显 + 事件注入。
func fakeCDPServer(t *testing.T) (url string, events chan string) {
	t.Helper()
	events = make(chan string, 8)
	url = wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
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
			method, _ := msg["method"].(string)
			id, _ := msg["id"].(float64)
			switch method {
			case "Target.attachToTarget":
				resp := map[string]any{"id": int64(id), "result": map[string]any{"sessionId": "sess-1"}}
				b, _ := json.Marshal(resp)
				_ = wstest.WriteServerText(conn, b)
			case "Test.emitEvent":
				ev := map[string]any{"method": "Test.event", "params": map[string]any{"from": "emit"}}
				b, _ := json.Marshal(ev)
				_ = wstest.WriteServerText(conn, b)
				resp := map[string]any{"id": int64(id), "result": map[string]any{}}
				b2, _ := json.Marshal(resp)
				_ = wstest.WriteServerText(conn, b2)
			case "Test.boom":
				resp := map[string]any{
					"id":    int64(id),
					"error": map[string]any{"code": -32601, "message": "method not found"},
				}
				b, _ := json.Marshal(resp)
				_ = wstest.WriteServerText(conn, b)
			default:
				resp := map[string]any{"id": int64(id), "result": map[string]any{
					"echo": method, "sessionId": msg["sessionId"],
				}}
				b, _ := json.Marshal(resp)
				_ = wstest.WriteServerText(conn, b)
			}
		}
	})
	return url, events
}

func TestCDPSendAndCall(t *testing.T) {
	url, _ := fakeCDPServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := newCDPClient(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	var out struct {
		Echo      string `json:"echo"`
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "Test.echo", map[string]any{"a": 1}, &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.Echo != "Test.echo" {
		t.Fatalf("echo mismatch: %+v", out)
	}
}

func TestCDPSessionRouting(t *testing.T) {
	url, _ := fakeCDPServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	if sess != "sess-1" {
		t.Fatalf("sessionId mismatch: %q", sess)
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.CallSession(ctx, sess, "Test.targetEcho", nil, &out); err != nil {
		t.Fatalf("session call: %v", err)
	}
	if out.SessionID != "sess-1" {
		t.Fatalf("session not routed: %+v", out)
	}
}

func TestCDPErrorResponse(t *testing.T) {
	url, _ := fakeCDPServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := newCDPClient(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Call(ctx, "Test.boom", nil, nil); err == nil {
		t.Fatal("expected error response")
	} else if !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCDPEventDispatch(t *testing.T) {
	url, _ := fakeCDPServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := newCDPClient(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	got := make(chan cdpEvent, 1)
	c.On("", "Test.event", func(ev cdpEvent) {
		select {
		case got <- ev:
		default:
		}
	})
	if err := c.Call(ctx, "Test.emitEvent", nil, nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	select {
	case ev := <-got:
		if ev.Method != "Test.event" {
			t.Fatalf("event method mismatch: %q", ev.Method)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event not dispatched")
	}
}
