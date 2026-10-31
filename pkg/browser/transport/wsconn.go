// 最小 WebSocket 客户端（RFC 6455，纯标准库零依赖）。
//
// 仅实现 CDP 需要的子集：客户端掩码、文本/二进制帧、分片聚合、
// ping/pong/close 控制帧；不做扩展协商（permessage-deflate 等）。
package transport

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// wsGUID 是 RFC 6455 握手校验用的固定 GUID。
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket 帧 opcode。
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// Conn 是一条已握手完成的 WebSocket 连接。
// 并发安全：单写锁串行写帧；读由唯一 reader goroutine 消费。
type Conn struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex // 写锁
}

// DialWS 建立 WebSocket 连接（ws:// 或 wss://）。
func DialWS(ctx context.Context, rawURL string) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ws: parse url: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("ws: unsupported scheme %q", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("ws: dial %s: %w", host, err)
	}
	if u.Scheme == "wss" {
		tconn := tls.Client(conn, &tls.Config{
			ServerName: u.Hostname(),
			MinVersion: tls.VersionTLS12,
		})
		if err := tconn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("ws: tls handshake: %w", err)
		}
		conn = tconn
	}
	br := bufio.NewReader(conn)
	if err := wsHandshake(ctx, conn, br, u); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Conn{conn: conn, r: br}, nil
}

// wsHandshake 发送客户端握手并校验 101 响应。
// br 必须与 Conn 共享，避免握手期到达的数据帧被缓冲区吞掉。
func wsHandshake(ctx context.Context, conn net.Conn, br *bufio.Reader, u *url.URL) error {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return fmt.Errorf("ws: rand: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, u.Host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("ws: write handshake: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
		defer conn.SetReadDeadline(time.Time{})
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return fmt.Errorf("ws: read handshake response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("ws: handshake status %d", resp.StatusCode)
	}
	if !headerHasToken(resp.Header, "Upgrade", "websocket") ||
		!headerHasToken(resp.Header, "Connection", "upgrade") {
		return fmt.Errorf("ws: handshake missing upgrade headers")
	}
	expect := wsAccept(key)
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != expect {
		return fmt.Errorf("ws: invalid Sec-WebSocket-Accept (got %q)", got)
	}
	return nil
}

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// WriteText 发送一个完整的文本帧（客户端必须掩码）。
func (c *Conn) WriteText(payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeFrame(wsOpText, payload)
}

// writeFrame 编码并写出单个帧（控制帧由调用方保证 ≤125 字节）。
func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer c.conn.SetWriteDeadline(time.Time{})
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode) // FIN=1
	mask := true
	ln := len(payload)
	switch {
	case ln <= 125:
		header = append(header, byte(ln)|0x80)
	case ln <= 0xFFFF:
		header = append(header, 126|0x80, byte(ln>>8), byte(ln))
	default:
		header = append(header, 127|0x80)
		header = binary.BigEndian.AppendUint64(header, uint64(ln))
	}
	if !mask {
		header[len(header)-1] &^= 0x80
	}
	if _, err := c.conn.Write(header); err != nil {
		return fmt.Errorf("ws: write header: %w", err)
	}
	if mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return fmt.Errorf("ws: rand: %w", err)
		}
		if _, err := c.conn.Write(key[:]); err != nil {
			return fmt.Errorf("ws: write mask: %w", err)
		}
		masked := make([]byte, len(payload))
		for i, b := range payload {
			masked[i] = b ^ key[i%4]
		}
		payload = masked
	}
	if _, err := c.conn.Write(payload); err != nil {
		return fmt.Errorf("ws: write payload: %w", err)
	}
	return nil
}

// ReadMessage 读取下一条完整数据消息（自动聚合分片、应答 ping/close）。
// 返回 opcode 与 payload；连接关闭或协议错误时返回非 nil error。
func (c *Conn) ReadMessage() (byte, []byte, error) {
	var opcode byte
	var out []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case wsOpPing:
			if err := c.WriteTextPong(payload); err != nil {
				return 0, nil, err
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			_ = c.writeFrame(wsOpClose, payload)
			_ = c.conn.Close()
			return 0, nil, io.EOF
		case wsOpContinuation:
			out = append(out, payload...)
			if fin {
				return opcode, out, nil
			}
		case wsOpText, wsOpBinary:
			opcode = op
			out = append(out, payload...)
			if fin {
				return opcode, out, nil
			}
		default:
			return 0, nil, fmt.Errorf("ws: unknown opcode 0x%x", op)
		}
	}
}

// WriteTextPong 应答 ping（控制帧载荷原样返回）。
func (c *Conn) WriteTextPong(payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeFrame(wsOpPong, payload)
}

// readFrame 读取一帧：返回 FIN 标志、opcode 与载荷。
func (c *Conn) readFrame() (bool, byte, []byte, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c.r, hdr); err != nil {
		return false, 0, nil, err
	}
	fin := hdr[0]&0x80 != 0
	opcode := hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	ln := uint64(hdr[1] & 0x7F)
	switch ln {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		ln = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		ln = binary.BigEndian.Uint64(ext[:])
	}
	if ln > 1<<30 {
		return false, 0, nil, fmt.Errorf("ws: frame too large: %d", ln)
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.r, maskKey[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, int(ln))
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return fin, opcode, payload, nil
}

// Close 关闭底层连接（阻塞中的读立即返回）。
func (c *Conn) Close() error {
	_ = c.writeFrame(wsOpClose, nil)
	return c.conn.Close()
}
