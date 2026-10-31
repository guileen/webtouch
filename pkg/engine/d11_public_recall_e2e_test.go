package engine

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"uws/pkg/browser"
	"uws/pkg/classifier"
	"uws/pkg/matcher"
	"uws/pkg/vlm"

	uwspb "uws/proto/gen/go/proto/v1"
)

// TestD11PublicPageTop4Recall records the real DOM → 10D ONNX ranking path on a
// public long page. It is opt-in because it needs network access and headless Chrome.
func TestD11PublicPageTop4Recall(t *testing.T) {
	if os.Getenv("UWS_RUN_PUBLIC_RANKER_E2E") != "1" {
		t.Skip("set UWS_RUN_PUBLIC_RANKER_E2E=1 to run the public-page ranker check")
	}
	const target = "History of the World Wide Web"
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := browser.NewPool(1, &browser.CDPFactory{Headless: true})
	bc, err := pool.Acquire(ctx, browser.Config{})
	if err != nil {
		t.Fatalf("acquire headless Chrome: %v", err)
	}
	defer pool.Release(bc.ID)
	if err := bc.Session().Navigate(ctx, "https://en.wikipedia.org/wiki/World_Wide_Web"); err != nil {
		t.Fatalf("navigate public page: %v", err)
	}
	time.Sleep(2 * time.Second)

	emptyMatcher, err := matcher.New(":memory:")
	if err != nil {
		t.Fatalf("create empty public-page matcher: %v", err)
	}
	srv, err := NewServer(Options{Matcher: emptyMatcher})
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}
	defer srv.Close()
	scorer, err := classifier.NewOnnxScorer(onnxIntegrationLib(t), onnxIntegrationModel(t))
	if err != nil {
		t.Fatalf("load checked-in ONNX model: %v", err)
	}
	srv.scorerMu.Lock()
	srv.onnxScorer = scorer
	srv.scorerMu.Unlock()
	request := &uwspb.ActionIntentRequest{TargetSemantic: target, ExpectedAction: "click"}
	nodes := bc.Session().(browser.CandidateSource).PageCandidates()
	pageCount := 0
	var countNodes func([]browser.PageCandidate)
	countNodes = func(list []browser.PageCandidate) {
		for _, node := range list {
			pageCount++
			countNodes(node.Children)
		}
	}
	countNodes(nodes)
	ranked, _, err := srv.pageCandidatePool(ctx, nodes, request, nil)
	if err != nil {
		t.Fatalf("build production decision pool: %v", err)
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Confidence > ranked[j].Confidence })
	if len(ranked) < 4 {
		t.Fatalf("real ambiguity check requires four candidates, production decision pool has %d", len(ranked))
	}
	for i := 0; i < min(12, len(ranked)); i++ {
		t.Logf("rank=%d score=%.4f tag=%s y=%.0f document_y=%.0f selector=%q text=%q",
			i+1, ranked[i].Confidence, ranked[i].Tag, ranked[i].Y, ranked[i].DocumentY, ranked[i].Selector, ranked[i].NodeText)
	}
	targetRank := -1
	for i := 0; i < 4; i++ {
		if ranked[i].Selector == "" || ranked[i].W <= 0 || ranked[i].H <= 0 {
			t.Fatalf("rank-%d candidate lacks crop metadata: %+v", i+1, ranked[i])
		}
		if strings.Contains(ranked[i].NodeText, target) {
			targetRank = i + 1
		}
	}
	if targetRank < 0 {
		t.Fatalf("target %q absent from Top-4 of %d real DOM candidates", target, len(ranked))
	}
	longPage, ok := bc.Session().(browser.LongPageScreenshotter)
	if !ok {
		t.Fatal("public CDP session does not support offscreen document capture")
	}
	blocks, panels, err := captureDocumentCandidatePanels(ctx, longPage, ranked[:4])
	if err != nil || len(blocks) != 4 || len(panels) != 4 {
		t.Fatalf("capture actual public-page Top-4 panels: blocks=%d panels=%d err=%v", len(blocks), len(panels), err)
	}
	montage, montageW, montageH, err := vlm.BuildImagePanelMontage(panels)
	if err != nil || len(montage) == 0 || montageW <= 0 || montageH <= 0 {
		t.Fatalf("build actual public-page Top-4 montage: %dx%d bytes=%d err=%v", montageW, montageH, len(montage), err)
	}
	t.Logf("real public Top-4 crops and montage PASS: labels=%v size=%dx%d",
		[]string{blocks[0].Label, blocks[1].Label, blocks[2].Label, blocks[3].Label}, montageW, montageH)
	t.Logf("public-page Top-4 recall PASS: page_nodes=%d decision_pool=%d target_rank=%d score=%.4f",
		pageCount, len(ranked), targetRank, ranked[targetRank-1].Confidence)
}
