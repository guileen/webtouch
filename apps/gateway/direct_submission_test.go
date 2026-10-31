package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDirectFailureNeverFallsBackOrCompletes(t *testing.T) {
	for _, state := range []string{"eval_lost", "network_error", "http_error", "invalid_start", "queue_lost", "partial_error", "empty_done", "invalid_poll", "cancel_wait"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", state, stream), func(t *testing.T) {
				activateRecipeFixture(t, "deepseek")
				e := newFakeEngine()
				e.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
					if strings.Contains(req.Value, "__uws_auth_probe") {
						return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
					}
					raw := `{"status":"streaming_started","ok":true}`
					if strings.Contains(req.Value, "window.fetch") {
						switch state {
						case "eval_lost":
							return ActionResult{}, fmt.Errorf("target_destroyed: evaluation result lost")
						case "network_error":
							raw = `{"status":"network_error","error":"connection lost"}`
						case "http_error":
							raw = `{"status":"error","httpStatus":500}`
						case "invalid_start":
							raw = `bad json`
						}
					} else if strings.Contains(req.Value, "chunks") {
						switch state {
						case "partial_error":
							raw = `{"chunks":["partial"],"done":true,"error":"connection lost"}`
						case "empty_done":
							raw = `{"chunks":[],"done":true}`
						case "invalid_poll":
							raw = `bad json`
						case "cancel_wait":
							raw = `{"chunks":["partial"],"done":false}`
						default:
							raw = `{"chunks":[],"done":true,"error":"queue_lost"}`
						}
					}
					return ActionResult{Success: true, ExtractedData: raw}, nil
				}
				g := newTestGateway(t, e)
				req := chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "one question"})
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				var err error
				if stream {
					events, startErr := g.ChatStream(ctx, req)
					err = startErr
					for ev := range events {
						if ev.Err != nil {
							err = ev.Err
						}
						if ev.FinishReason != "" {
							t.Errorf("partial answer marked finished: %+v", ev)
						}
					}
				} else {
					_, err = g.Chat(ctx, req)
				}
				if err == nil || !strings.Contains(err.Error(), "submission_outcome_unknown") {
					t.Errorf("err=%v", err)
				}
				for _, call := range e.callKinds() {
					if call.semantic == "chat_input" || call.semantic == "send_button" {
						t.Errorf("DOM redispatch: %+v", call)
					}
				}
				if len(e.initNames) != 1 {
					t.Errorf("replayed sessions: %v", e.initNames)
				}
			})
		}
	}
}

// Run the emitted JS so the no-dispatch guarantee does not rest on a fake receipt.
func TestDirectScriptSubmissionBoundary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	for _, dispatched := range []bool{false, true} {
		req := DirectTransportRequest{Endpoint: "/test", Method: "POST", Payload: map[string]any{"message": "test"}}
		if dispatched {
			req.Headers = map[string]string{}
		}
		script, err := BuildInPageFetchScript(req, "test_queue")
		if err != nil {
			t.Fatal(err)
		}
		source := `let calls=0;global.window={fetch:async()=>{calls++;throw new Error("connection lost")}};(async()=>{const result=await ` + script + `;process.stdout.write(JSON.stringify({result,calls}));})()`
		raw, err := exec.Command(node, "-e", source).CombinedOutput()
		if err != nil {
			t.Fatalf("JS: %v %s", err, raw)
		}
		var got struct {
			Result struct {
				Status string `json:"status"`
			} `json:"result"`
			Calls int `json:"calls"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		want, count := "not_dispatched", 0
		if dispatched {
			want, count = "network_error", 1
		}
		if got.Result.Status != want || got.Calls != count {
			t.Fatalf("dispatched=%v got=%s", dispatched, raw)
		}
	}
}
