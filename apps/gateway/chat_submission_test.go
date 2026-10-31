package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSubmissionErrorCannotUndoEarlierSend(t *testing.T) {
	// A later read that was not dispatched says nothing about the earlier send.
	err := submissionError(errors.New("physical_action_not_dispatched: target_destroyed"))
	if !strings.Contains(err.Error(), "submission_outcome_unknown") || IsTargetDestroyed(err) {
		t.Fatalf("unsafe whole-round recovery: %v", err)
	}
}

func TestActionRetryRequiresNoDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		action       string
		calls        int
	}{
		{"unknown_click", "physical_event_dispatched_outcome_unknown", "click", 1},
		{"plain_timeout", "deadline exceeded", "click", 1},
		{"input_unknown", "physical_event_dispatched_outcome_unknown", "input", 1},
		{"before_dispatch", "physical_action_not_dispatched", "click", 2},
		{"readonly", "deadline exceeded", "extract", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFakeEngine()
			e.failOnce["probe"] = &EngineError{Code: codeDeadlineExceeded, Reason: tc.reason}
			g := newTestGateway(t, e)
			_, _ = g.executeWithRetry(context.Background(), DefaultSessionID, g.providers["deepseek-web"], "one", "probe", tc.action, "", nil)
			if got := len(e.callKinds()); got != tc.calls {
				t.Fatalf("dispatches=%d want=%d", got, tc.calls)
			}
		})
	}
}

func TestCanceledActionDoesNotDispatch(t *testing.T) {
	e := newFakeEngine()
	g := newTestGateway(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.executeWithRetry(ctx, DefaultSessionID, g.providers["deepseek-web"], "cancel", "probe", "click", "", nil)
	if err == nil || len(e.callKinds()) != 0 {
		t.Fatalf("err=%v calls=%v", err, e.callKinds())
	}
}

func TestTimeoutNeverCompletesPartialAnswer(t *testing.T) {
	for _, active := range []bool{true, false} {
		for _, streaming := range []bool{false, true} {
			e := newFakeEngine()
			e.extractPlan = []extractStep{{text: "partial", active: active}, {text: "partial", active: active}}
			g := newTestGateway(t, e)
			g.chatTimeout = time.Nanosecond
			g.answerStable = time.Hour
			p := g.providers["deepseek-web"]
			out := make(chan ChatEvent, 16)
			var err error
			if streaming {
				err = g.streamAnswer(context.Background(), 1, "old", p, DefaultSessionID, nil, out)
			} else {
				_, err = g.waitForNewAnswer(context.Background(), 1, "old", p, DefaultSessionID)
			}
			if err == nil {
				t.Errorf("active=%v streaming=%v: incomplete answer accepted", active, streaming)
			}
			close(out)
			for event := range out {
				if event.FinishReason != "" {
					t.Errorf("incomplete stream marked finished: %+v", event)
				}
			}
		}
	}
}

func TestAnswerPollingNeverRegenerates(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		e := newFakeEngine()
		g := newTestGateway(t, e)
		g.chatTimeout = 5 * time.Second
		p := g.providers["deepseek-web"]
		p.Semantics["retry"] = "regenerate"
		if streaming {
			_ = g.streamAnswer(context.Background(), 1, "确定性应答", p, DefaultSessionID, nil, make(chan ChatEvent, 16))
		} else {
			_, _ = g.waitForNewAnswer(context.Background(), 1, "确定性应答", p, DefaultSessionID)
		}
		for _, c := range e.callKinds() {
			if c.action != "extract" {
				t.Fatalf("stream=%v unexpected action %+v", streaming, c)
			}
		}
	}
}

func TestDestroyedDuringSendDoesNotReplayRound(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		e := newFakeEngine()
		e.incrementAnswers = true
		e.failOnce["send_button"] = &EngineError{Code: codeAborted, Reason: "target_destroyed: outcome unknown"}
		g := newTestGateway(t, e)
		req := chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "one question"})
		var err error
		if streaming {
			events, startErr := g.ChatStream(context.Background(), req)
			err = startErr
			for ev := range events {
				if ev.Err != nil {
					err = ev.Err
				}
			}
		} else {
			_, err = g.Chat(context.Background(), req)
		}
		if err == nil || !strings.Contains(err.Error(), "submission_outcome_unknown") {
			t.Fatalf("stream=%v err=%v", streaming, err)
		}
		if len(e.initNames) != 1 || countCalls(e.callKinds(), "send_button") != 1 {
			t.Fatalf("stream=%v replayed: init=%v calls=%v", streaming, e.initNames, e.callKinds())
		}
	}
}

type interruptedAfterSendEngine struct {
	*fakeEngine
	sent   bool
	cancel context.CancelFunc
}

func (e *interruptedAfterSendEngine) ExecuteAction(ctx context.Context, req ActionRequest) (ActionResult, error) {
	if e.sent && req.ExpectedAction == "extract" {
		return ActionResult{}, &EngineError{Code: codeAborted, Reason: "target_destroyed: waiting for answer"}
	}
	r, err := e.fakeEngine.ExecuteAction(ctx, req)
	if req.TargetSemantic == "send_button" && err == nil {
		e.sent = true
		if e.cancel != nil {
			e.cancel()
		}
	}
	return r, err
}

func TestPostSubmissionInterruptionNeverResubmits(t *testing.T) {
	for _, cancelAfterSend := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			e := &interruptedAfterSendEngine{fakeEngine: newFakeEngine()}
			if cancelAfterSend {
				e.cancel = cancel
			}
			g := newTestGateway(t, e.fakeEngine)
			g.engineFactory = func(string) (EngineClient, error) { return e, nil }
			g.chatTimeout = 2 * time.Millisecond
			req := chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "only once"})
			var err error
			if streaming {
				ch, startErr := g.ChatStream(ctx, req)
				err = startErr
				for event := range ch {
					if event.Err != nil {
						err = event.Err
					}
				}
			} else {
				_, err = g.Chat(ctx, req)
			}
			cancel()
			if err == nil || !strings.Contains(err.Error(), "submission_outcome_unknown") {
				t.Fatalf("cancel=%v stream=%v err=%v", cancelAfterSend, streaming, err)
			}
			if len(e.initNames) != 1 || countCalls(e.callKinds(), "send_button") != 1 {
				t.Fatalf("resubmitted: %+v", e.callKinds())
			}
		}
	}
}
