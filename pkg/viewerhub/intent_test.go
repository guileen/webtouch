package viewerhub

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

// shellExpandLine 是 Swift 壳（cmd/viewer/Sources/ViewerCore/ViewerSocketProtocol.swift）
// 真正会写出来的形状。两侧各留一份字面量断言，协议一改两边同时红。
const shellExpandLine = `{"version":1,"kind":"intent","intent":"expand","session":"ctx-1","size":{"width":960,"height":620}}`

func TestDecodeIntentAcceptsTheShellsExpandLine(t *testing.T) {
	in, ok := DecodeIntent([]byte(shellExpandLine))
	if !ok {
		t.Fatal("壳的 expand 行应当被接受")
	}
	if in.Session != "ctx-1" || in.Intent != IntentExpand {
		t.Fatalf("解码结果 = %+v", in)
	}
	if in.Size == nil || in.Size.Width != 960 || in.Size.Height != 620 {
		t.Fatalf("size = %+v", in.Size)
	}
	collapse := []byte(`{"version":1,"kind":"intent","intent":"collapse","session":"ctx-1"}`)
	if in, ok := DecodeIntent(collapse); !ok || in.Intent != IntentCollapse {
		t.Fatalf("collapse 行应当被接受: %+v ok=%v", in, ok)
	}
}

// 2026-09-22 现场批次 #2：用户点了小窗上的 ✕。壳侧真丢掉了那块面板，同时告诉引擎
// 「这个人不想再看这个会话了」，由引擎把它静默回收（不结束会话、不关真窗口）。
// 两侧各留一份字面量断言，协议一改两边同时红。
func TestDecodeIntentAcceptsTheShellsCloseLine(t *testing.T) {
	const shellCloseLine = `{"version":1,"kind":"intent","intent":"close","session":"ctx-1"}`
	in, ok := DecodeIntent([]byte(shellCloseLine))
	if !ok {
		t.Fatal("壳的 close 行应当被接受")
	}
	if in.Session != "ctx-1" || in.Intent != IntentClose {
		t.Fatalf("解码结果 = %+v", in)
	}
}

// A viewer is a local client, but still untrusted input: nothing here may make
// the engine act, and nothing here may panic.
func TestDecodeIntentRejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"empty":           ``,
		"junk":            `not json`,
		"wrong version":   `{"version":2,"intent":"expand","session":"s"}`,
		"missing session": `{"version":1,"intent":"expand","size":{"width":900,"height":600}}`,
		"unknown intent":  `{"version":1,"intent":"click","session":"s"}`,
		"foreign kind":    `{"version":1,"kind":"frame","intent":"expand","session":"s","size":{"width":900,"height":600}}`,
	}
	for name, line := range cases {
		if _, ok := DecodeIntent([]byte(line)); ok {
			t.Fatalf("%s 行不该被接受: %s", name, line)
		}
	}
}

// 2026-09-22 口径：展开不再带尺寸。这条曾经是「畸形输入」，真机复验里正是它把新壳的
// 展开意图静默丢掉（没有回执 ⇒ 用户只看到回落的自绘面板）。
func TestExpandIntentNoLongerRequiresASize(t *testing.T) {
	in, ok := DecodeIntent([]byte(`{"version":1,"kind":"intent","intent":"expand","session":"ctx-1"}`))
	if !ok {
		t.Fatal("不带 size 的 expand 意图应当被接受")
	}
	if in.Session != "ctx-1" || in.Intent != IntentExpand || in.Size != nil {
		t.Fatalf("解码结果 = %+v", in)
	}
	// 老壳照旧带尺寸：解出来放着就是了，引擎不再看它。
	legacy, ok := DecodeIntent([]byte(shellExpandLine))
	if !ok {
		t.Fatal("带 size 的老壳意图仍应被接受")
	}
	if legacy.Size == nil || legacy.Size.Width != 960 {
		t.Fatalf("老壳的 size 应被解出来（只是不再参与判定）: %+v", legacy)
	}
}

func TestEncodeIntentResultIsOneJsonLine(t *testing.T) {
	payload, err := EncodeIntentResult(IntentResult{Session: "s-1", Intent: IntentExpand, OK: true, BrowserPID: 4242})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if payload[len(payload)-1] != '\n' {
		t.Fatalf("回执必须以换行结尾: %q", payload)
	}
	var decoded struct {
		Version    int    `json:"version"`
		Kind       string `json:"kind"`
		Session    string `json:"session"`
		OK         bool   `json:"ok"`
		BrowserPID int    `json:"browserPID"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Version != IntentProtocolVersion || decoded.Kind != IntentResultKind || !decoded.OK || decoded.BrowserPID != 4242 {
		t.Fatalf("回执字段 = %+v", decoded)
	}
}

func TestViewerIntentReachesTheHandler(t *testing.T) {
	hub, _ := newTestHub(t, false)
	got := make(chan Intent, 4)
	hub.SetIntentHandler(func(in Intent) { got <- in })

	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)

	if _, err := viewerSide.Write([]byte(shellExpandLine + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case in := <-got:
		if in.Session != "ctx-1" || in.Intent != IntentExpand {
			t.Fatalf("handler 收到 %+v", in)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("意图没有到达 handler")
	}
}

// A viewer that expands a session and then vanishes must not leave a full-size
// Chrome window behind: the hub folds back what that viewer opened.
func TestViewerDisconnectFoldsBackWhatItExpanded(t *testing.T) {
	hub, _ := newTestHub(t, false)
	got := make(chan Intent, 8)
	hub.SetIntentHandler(func(in Intent) { got <- in })

	serverSide, viewerSide := net.Pipe()
	hub.addSubscriber(serverSide)
	if _, err := viewerSide.Write([]byte(shellExpandLine + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("expand 意图没有到达 handler")
	}
	_ = viewerSide.Close()
	select {
	case in := <-got:
		if in.Intent != IntentCollapse || in.Session != "ctx-1" || in.Reason != IntentReasonViewerGone {
			t.Fatalf("观看壳断开后应补发收回: %+v", in)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("观看壳断开后没有补发收回意图")
	}
}

// A viewer that collapsed before leaving has nothing to fold back.
func TestViewerDisconnectIsSilentAfterCollapse(t *testing.T) {
	hub, _ := newTestHub(t, false)
	got := make(chan Intent, 8)
	hub.SetIntentHandler(func(in Intent) { got <- in })

	serverSide, viewerSide := net.Pipe()
	hub.addSubscriber(serverSide)
	_, _ = viewerSide.Write([]byte(shellExpandLine + "\n"))
	<-got
	_, _ = viewerSide.Write([]byte(`{"version":1,"kind":"intent","intent":"collapse","session":"ctx-1"}` + "\n"))
	<-got
	_ = viewerSide.Close()
	select {
	case in := <-got:
		t.Fatalf("收回之后断开不该再补发意图: %+v", in)
	case <-time.After(300 * time.Millisecond):
	}
}

// One viewer's malformed line must not stop the next valid one.
func TestMalformedViewerLineIsSkipped(t *testing.T) {
	hub, _ := newTestHub(t, false)
	got := make(chan Intent, 4)
	hub.SetIntentHandler(func(in Intent) { got <- in })

	serverSide, viewerSide := net.Pipe()
	defer viewerSide.Close()
	hub.addSubscriber(serverSide)
	_, _ = viewerSide.Write([]byte("{garbage}\n" + shellExpandLine + "\n"))
	select {
	case in := <-got:
		if in.Session != "ctx-1" {
			t.Fatalf("收到 %+v", in)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("坏行之后的合法意图没有到达 handler")
	}
}
