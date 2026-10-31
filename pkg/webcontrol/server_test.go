package webcontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"uws/pkg/challenge"
	"uws/pkg/qa"

	"golang.org/x/net/websocket"
)

type mockForwarder struct {
	mu     sync.Mutex
	events []challenge.HumanInputEvent
}

func (m *mockForwarder) DispatchHumanInput(_ context.Context, ev challenge.HumanInputEvent) error {
	m.mu.Lock()
	m.events = append(m.events, ev)
	m.mu.Unlock()
	return nil
}

func (m *mockForwarder) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

func TestWebControlIndexHTML(t *testing.T) {
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "WebTouch QA Studio") {
		t.Fatalf("expected page to contain 'WebTouch QA Studio', got:\n%s", body[:200])
	}
	if !strings.Contains(body, "Figma") {
		t.Fatalf("expected page to contain 'Figma', got:\n%s", body[:200])
	}
}

func TestWebControlStatusAPI(t *testing.T) {
	cb := challenge.NewCircuitBreaker(challenge.CircuitBreakerOptions{
		MaxConsecutiveFailures: 3,
	})
	cb.Trip(&challenge.ChallengeSignal{
		Type:   challenge.ChallengeTypeCloudflareTurnstile,
		Reason: "Cloudflare Turnstile Detected",
	})

	srv, err := NewServer(Options{
		EngineConnected: true,
		BreakersProvider: func() map[string]*challenge.CircuitBreaker {
			return map[string]*challenge.CircuitBreaker{
				"test-ctx": cb,
			}
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var res StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal StatusResponse: %v", err)
	}

	if !res.EngineConnected {
		t.Errorf("expected EngineConnected=true")
	}
	if len(res.Circuits) != 1 {
		t.Fatalf("expected 1 circuit, got %d", len(res.Circuits))
	}
	if res.Circuits[0].State != "OPEN" {
		t.Errorf("expected OPEN circuit, got %s", res.Circuits[0].State)
	}
}

func TestWebControlQARunAPI(t *testing.T) {
	customCalled := false
	srv, err := NewServer(Options{
		QARunner: func(_ context.Context, url string) (*qa.QAReport, error) {
			customCalled = true
			return &qa.QAReport{
				URL:         url,
				HealthScore: 99,
				Verdict:     qa.VerdictPass,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	payload := `{"url":"https://test-site.example"}`
	req := httptest.NewRequest("POST", "/api/qa/run", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !customCalled {
		t.Errorf("expected custom QARunner to be called")
	}

	var rep qa.QAReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal QAReport: %v", err)
	}
	if rep.HealthScore != 99 || rep.Verdict != qa.VerdictPass {
		t.Errorf("unexpected report: %+v", rep)
	}
}

func TestWebControlQARunAPI_RunnerNil_FailClosed(t *testing.T) {
	// QARunner 为 nil，未初始化任何执行引擎
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	payload := `{"url":"https://test-site.example"}`
	req := httptest.NewRequest("POST", "/api/qa/run", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// 必须 Fail-Closed 返回 503
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when runner is nil, got %d", w.Code)
	}

	var failResp qa.QAFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &failResp); err != nil {
		t.Fatalf("unmarshal QAFailureResponse: %v", err)
	}

	if failResp.ExecutionStatus != qa.ExecutionStatusNotRun {
		t.Errorf("expected execution_status=NOT_RUN, got %s", failResp.ExecutionStatus)
	}
	if failResp.Verdict != qa.VerdictBlocked {
		t.Errorf("expected verdict=BLOCKED, got %s", failResp.Verdict)
	}
	if failResp.ReasonCode != "RUNNER_UNAVAILABLE" {
		t.Errorf("expected reason_code=RUNNER_UNAVAILABLE, got %s", failResp.ReasonCode)
	}
	// 铁律：绝不允许出现 PASS
	if failResp.Verdict == qa.VerdictPass {
		t.Fatalf("CRITICAL: fail-closed violation, verdict cannot be PASS when runner is nil")
	}
}

func TestWebControlQARunAPI_InvalidInput(t *testing.T) {
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// 1. 空 URL
	req := httptest.NewRequest("POST", "/api/qa/run", strings.NewReader(`{"url":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty url, got %d", w.Code)
	}

	var failResp qa.QAFailureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &failResp); err != nil {
		t.Fatalf("unmarshal QAFailureResponse: %v", err)
	}
	if failResp.Verdict != qa.VerdictBlocked || failResp.ReasonCode != "URL_REQUIRED" {
		t.Errorf("unexpected failure response: %+v", failResp)
	}

	// 2. 非法 JSON
	req2 := httptest.NewRequest("POST", "/api/qa/run", strings.NewReader(`invalid-json`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid json, got %d", w2.Code)
	}
}

func TestWebControlViewerInputAndReset(t *testing.T) {
	fwd := &mockForwarder{}
	cb := challenge.NewCircuitBreaker()
	cb.Trip(&challenge.ChallengeSignal{
		Type:   challenge.ChallengeTypeArkose,
		Reason: "Arkose Labs Challenge",
	})

	srv, err := NewServer(Options{
		Forwarder: fwd,
		BreakersProvider: func() map[string]*challenge.CircuitBreaker {
			return map[string]*challenge.CircuitBreaker{"test-ctx": cb}
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// 1. 发送模拟人类点击
	clickPayload := `{"type":"click","x":150,"y":300}`
	req := httptest.NewRequest("POST", "/api/viewer/input", strings.NewReader(clickPayload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for viewer input, got %d", w.Code)
	}
	if fwd.count() != 1 {
		t.Fatalf("expected 1 forwarder event, got %d", fwd.count())
	}
	ev := fwd.events[0]
	if ev.Type != challenge.HumanInputClick || ev.X != 150 || ev.Y != 300 {
		t.Fatalf("unexpected event: %+v", ev)
	}

	// 2. 复位熔断器
	resetPayload := `{"circuit_id":"test-ctx"}`
	req2 := httptest.NewRequest("POST", "/api/circuits/reset", strings.NewReader(resetPayload))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 for circuit reset, got %d", w2.Code)
	}

	if cb.State() != challenge.CircuitClosed {
		t.Fatalf("expected circuit to be CLOSED after reset, got %s", cb.State())
	}
}

func TestWebControlWebSocketStreaming(t *testing.T) {
	fwd := &mockForwarder{}
	srv, err := NewServer(Options{
		Forwarder: fwd,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wsURL := "ws://" + ts.Listener.Addr().String() + "/api/viewer/stream"
	ws, err := websocket.Dial(wsURL, "", "http://localhost/")
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer ws.Close()

	// 1. 客户端向 WebSocket 发送输入动作
	inputMsg := `{"type":"click","x":250,"y":450}`
	if err := websocket.Message.Send(ws, inputMsg); err != nil {
		t.Fatalf("send websocket message: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if fwd.count() != 1 {
		t.Fatalf("expected forwarder to receive websocket click, got %d", fwd.count())
	}

	// 2. 服务端广播新帧
	pngDummy := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDRtest")
	srv.BroadcastFrame("sess-1", pngDummy)

	// 3. 客户端接收广播帧
	_ = ws.SetReadDeadline(time.Now().Add(1 * time.Second))
	var received string
	if err := websocket.Message.Receive(ws, &received); err != nil {
		t.Fatalf("receive websocket frame: %v", err)
	}

	if !strings.Contains(received, "frame") || !strings.Contains(received, "sess-1") {
		t.Fatalf("unexpected frame received: %s", received)
	}
}
