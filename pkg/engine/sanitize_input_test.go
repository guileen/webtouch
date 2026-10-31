package engine

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	uwspb "uws/proto/gen/go/proto/v1"
)

func TestLongInputBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, action, value string
		want                codes.Code
	}{
		{"debate_chinese", "input", strings.Repeat("讨论历史\n", 5000), codes.OK},
		{"input_exact", "input", strings.Repeat("x", 256*1024), codes.OK},
		{"input_over", "input", strings.Repeat("x", 256*1024+1), codes.InvalidArgument},
		{"input_utf8_over", "input", strings.Repeat("读", 256*1024/3+1), codes.InvalidArgument},
		{"other_exact", "click", strings.Repeat("x", 4096), codes.OK},
		{"other_over", "click", strings.Repeat("x", 4097), codes.InvalidArgument},
		{"script_prefix", "input", " JavaScript:alert(1)", codes.InvalidArgument},
		{"evaluate_exact", "evaluate", strings.Repeat("x", 1024*1024), codes.OK},
		{"evaluate_over", "evaluate", strings.Repeat("x", 1024*1024+1), codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIntent(&uwspb.ActionIntentRequest{IntentId: "long-input", TargetSemantic: "composer", ExpectedAction: tc.action, Value: &tc.value})
			if got := status.Code(err); got != tc.want {
				t.Fatalf("got %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}
