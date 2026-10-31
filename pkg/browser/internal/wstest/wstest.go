// Package wstest 是 WebSocket 相关的测试支撑：起一个只做握手 + 帧收发的假服务端，
// 供 pkg/browser 及其子包的测试共用（夹具原先散在 pkg/browser/wsconn_test.go，2026-09-27 抽出）。
//
// 客户端帧解析在本包内独立实现（不复用被测代码），因此可当作对实现的独立校验。
package wstest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
)

// WebSocket 帧 opcode（RFC 6455），测试侧独立声明，供断言使用。
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// wsGUID 是 RFC 6455 握手校验用的固定 GUID（故意独立声明，不引用实现侧常量）。
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Start 启动一个回显/协议测试用 WebSocket 服务端，返回可拨号的 ws:// 地址。
// handler 收到已握手连接（br 为 reader）；handler 返回后连接关闭。
func Start(t *testing.T, handler func(conn net.Conn, br *bufio.Reader)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.Header.Get("Upgrade") != "websocket" {
			return
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		h := sha1.Sum([]byte(key + wsGUID))
		accept := base64.StdEncoding.EncodeToString(h[:])
		resp := fmt.Sprintf(
			"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			accept)
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}
		handler(conn, br)
	}()
	return "ws://" + ln.Addr().String() + "/devtools/browser/test"
}

// WriteServerText 服务端发送一个文本帧（服务端帧不掩码）。
func WriteServerText(conn net.Conn, payload []byte) error {
	header := []byte{0x81}
	ln := len(payload)
	switch {
	case ln <= 125:
		header = append(header, byte(ln))
	case ln <= 0xFFFF:
		header = append(header, 126, byte(ln>>8), byte(ln))
	default:
		header = append(header, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(ln))
		header = append(header, ext[:]...)
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

// ReadServerFrame 服务端读一帧（客户端帧必须掩码；返回 opcode 与载荷）。
func ReadServerFrame(br *bufio.Reader) (byte, []byte, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return 0, nil, err
	}
	opcode := hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	ln := uint64(hdr[1] & 0x7F)
	switch ln {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		ln = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		ln = binary.BigEndian.Uint64(ext[:])
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(br, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(ln))
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, payload, nil
}
