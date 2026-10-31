package engine

import (
	"testing"

	"uws/pkg/classifier"
	"uws/pkg/matcher"
)

func TestProtectedRankerCandidatesKeepsHighScoreAndGlobalTop4(t *testing.T) {
	ranked := []matcher.Candidate{
		{NodeText: "exact target", Confidence: 0.95, Source: "page_onnx"},
		{NodeText: "high scorer", Confidence: 0.80, Source: "page_onnx"},
		{NodeText: "another high scorer", Confidence: classifier.ThresholdLow, Source: "page_onnx"},
		{NodeText: "low score but Top-4", Confidence: 0.49, Source: "page_onnx"},
		{NodeText: "low score outside Top-4", Confidence: 0.48, Source: "page_onnx"},
		{NodeText: "probe score is not Ranker", Confidence: 0.99, Source: "page_probe"},
	}
	rejected := []matcher.Candidate{ranked[1], ranked[2], ranked[3], ranked[4], ranked[5]}
	got := protectedRankerCandidates(ranked, rejected)
	if len(got) != 3 {
		t.Fatalf("expected two high-score candidates plus low-score global Top-4, got %+v", got)
	}
	want := map[string]bool{
		"high scorer": true, "another high scorer": true, "low score but Top-4": true,
	}
	for _, c := range got {
		if !want[c.NodeText] {
			t.Errorf("unexpected candidate protected from L3: %q", c.NodeText)
		}
		delete(want, c.NodeText)
	}
	if len(want) != 0 {
		t.Fatalf("missing protected candidates: %v", want)
	}
}
