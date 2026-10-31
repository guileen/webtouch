package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
	"uws/pkg/browser/internal/wstest"
	"uws/pkg/browser/modelbridge"
)

// 单元 2b（2026-09-29）：重试语义（挂起 → 重放输入就绪 → 重试）的用例已随实现迁入
// `pkg/browser/input/reliability_test.go`，用最小 `Caller` 假体驱动；本文件只留
// 宿主仍需要的两样东西：
//   - `inputFakeCDP` + `newInputTestSession`：一套能被宿主会话直连的伪 CDP（
//     `viewer_window_test.go`、`pointer_humanize_test.go` 复用）；
//   - `TestInitTargetEnablesFocusEmulation`：建会话时的焦点仿真接线（宿主侧行为）。

// ── 伪 CDP 服务器：所有输入命令都返回成功，其余命令返回空结果 ──

type inputFakeCDP struct {
	mu     sync.Mutex
	calls  []string
	params []map[string]any
}

func (f *inputFakeCDP) record(method string, params map[string]any) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.params = append(f.params, params)
	f.mu.Unlock()
}

func (f *inputFakeCDP) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *inputFakeCDP) paramsFor(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for i, m := range f.calls {
		if m == method {
			out = append(out, f.params[i])
		}
	}
	return out
}

func (f *inputFakeCDP) count(method string) int {
	n := 0
	for _, m := range f.snapshot() {
		if m == method {
			n++
		}
	}
	return n
}

// serve 启动伪服务器（输入命令返回成功；建会话/探视口等必要命令返回可用结果）。
func (f *inputFakeCDP) serve(t *testing.T) string {
	t.Helper()
	return wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
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
			var params map[string]any
			if p, ok := msg["params"].(map[string]any); ok {
				params = p
			}
			f.record(method, params)

			switch method {
			case "Runtime.evaluate":
				// 让 initTarget 的「视口跟随窗口」拿到可用尺寸。
				resp := map[string]any{"id": int64(id), "result": map[string]any{
					"result": map[string]any{"type": "object",
						"value": map[string]any{"w": 500, "h": 540}}}}
				b, _ := json.Marshal(resp)
				_ = wstest.WriteServerText(conn, b)
				continue
			case "Target.createTarget":
				resp := map[string]any{"id": int64(id), "result": map[string]any{"targetId": "T1"}}
				b, _ := json.Marshal(resp)
				_ = wstest.WriteServerText(conn, b)
				continue
			case "Target.attachToTarget":
				resp := map[string]any{"id": int64(id), "result": map[string]any{"sessionId": "sess-1"}}
				b, _ := json.Marshal(resp)
				_ = wstest.WriteServerText(conn, b)
				continue
			case "Browser.getWindowForTarget":
				resp := map[string]any{"id": int64(id), "result": map[string]any{"windowId": 7}}
				b, _ := json.Marshal(resp)
				_ = wstest.WriteServerText(conn, b)
				continue
			}
			resp := map[string]any{"id": int64(id), "result": map[string]any{}}
			b, _ := json.Marshal(resp)
			_ = wstest.WriteServerText(conn, b)
		}
	})
}

func newInputTestSession(t *testing.T, url string) *cdpSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := newCDPClient(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &cdpSession{
		id:        "input-test",
		client:    c,
		sessionID: "sess-1",
		closedCh:  make(chan struct{}),
		inflight:  make(map[string]time.Time),
		chat:      modelbridge.NewStream(),
	}
}

// TestInitTargetEnablesFocusEmulation 断言有头会话在建会话时就打开焦点仿真，
// 且无头会话（无窗口）不下发该命令。
func TestInitTargetEnablesFocusEmulation(t *testing.T) {
	t.Setenv("UWS_WINDOW_POS", "none") // 不移动窗口，避免依赖 Browser.setWindowBounds
	for _, headless := range []bool{false, true} {
		f := &inputFakeCDP{}
		s := newInputTestSession(t, f.serve(t))
		s.headless = headless
		// 附着模式标记（本用例的 CDP 连接由 newInputTestSession 注入的伪服务器提供）。
		// 显式走 window（专属小窗）模式：默认的 shared 会把锚页作为**第二个 session**
		// 也装上同一层护栏（shared_window.go::armAnchorInput），那层由
		// TestOpenTabInSharedWindowArmsAnchorSession 单独覆盖；本用例只量「会话自己的
		// target 在建会话时装了护栏」，两者混在一起就没法按次数断言。
		s.cfg = Config{AttachCDPURL: "http://127.0.0.1:9222", WindowMode: "window"}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.initTarget(ctx)
		cancel()
		if err != nil {
			t.Fatalf("initTarget(headless=%v): %v", headless, err)
		}
		got := f.count("Emulation.setFocusEmulationEnabled")
		if headless && got != 0 {
			t.Fatalf("无头会话不应下发焦点仿真，got %d 次", got)
		}
		if !headless && got != 1 {
			t.Fatalf("有头会话应在初始化时下发焦点仿真，got %d 次: %v", got, f.snapshot())
		}
	}
}
