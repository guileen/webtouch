package transport

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"uws/pkg/browser/internal/wstest"
)

func TestWSHandshakeAndTextRoundtrip(t *testing.T) {
	url := wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
		op, payload, err := wstest.ReadServerFrame(br)
		if err != nil {
			return
		}
		if op != wsOpText || string(payload) != "ping-1" {
			return
		}
		_ = wstest.WriteServerText(conn, []byte("pong-1"))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialWS(ctx, url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.WriteText([]byte("ping-1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	op, payload, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != wsOpText || string(payload) != "pong-1" {
		t.Fatalf("roundtrip mismatch: op=%d payload=%q", op, payload)
	}
}

func TestWSPingAutoPong(t *testing.T) {
	gotPong := make(chan string, 1)
	url := wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
		// 服务端先发 ping，客户端应自动回 pong，然后返回后续文本帧。
		hdr := []byte{0x89, 0x03}
		if _, err := conn.Write(append(hdr, []byte("hey")...)); err != nil {
			return
		}
		// 读取客户端 pong（opcode 0xA）。
		op, payload, err := wstest.ReadServerFrame(br)
		if err != nil {
			return
		}
		if op == wsOpPong {
			gotPong <- string(payload)
		}
		_ = wstest.WriteServerText(conn, []byte("after-ping"))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialWS(ctx, url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	// ReadMessage 内部处理 ping（自动 pong）后返回文本帧。
	op, payload, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != wsOpText || string(payload) != "after-ping" {
		t.Fatalf("unexpected message: op=%d payload=%q", op, payload)
	}
	select {
	case p := <-gotPong:
		if p != "hey" {
			t.Fatalf("pong payload mismatch: %q", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not answer ping")
	}
}

func TestWSFragmentedMessage(t *testing.T) {
	url := wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
		_, _ = conn.Write([]byte{0x01, 0x03, 'a', 'b', 'c'}) // text, FIN=0
		_, _ = conn.Write([]byte{0x80, 0x02, 'd', 'e'})      // continuation, FIN=1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialWS(ctx, url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	op, payload, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != wsOpText || string(payload) != "abcde" {
		t.Fatalf("fragmented mismatch: op=%d payload=%q", op, payload)
	}
}

func TestWSRejectsBadScheme(t *testing.T) {
	if _, err := DialWS(context.Background(), "http://127.0.0.1:1/x"); err == nil ||
		!strings.Contains(err.Error(), "scheme") {
		t.Fatalf("expected scheme error, got %v", err)
	}
}
