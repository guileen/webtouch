package gateway

import (
	"context"
	"strings"
)

// A transport timeout is not evidence that a browser write never happened.
func canRetryAction(ctx context.Context, action string, err *EngineError) bool {
	if ctx.Err() != nil || err == nil || err.IsTargetDestroyed() {
		return false
	}
	if strings.Contains(err.Reason, "physical_event_dispatched_outcome_unknown") {
		return false
	}
	return action == "extract" || strings.Contains(err.Reason, "physical_action_not_dispatched")
}

// Keep send/observation failures out of target-destroyed whole-round recovery.
// The caller must inspect the original page before deciding to submit again.
func submissionError(err error) error {
	if err == nil {
		return err
	}
	semantic := ""
	if e, ok := err.(*ChatError); ok {
		semantic = e.Semantic
	}
	return &ChatError{Reason: "submission_outcome_unknown: " + err.Error(), Semantic: semantic, Code: errCodeSubmissionUnknown}
}
