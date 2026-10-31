package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"uws/pkg/browser"
	uwspb "uws/proto/gen/go/proto/v1"
)

// stubShadowScorer 是影子打分替身：不依赖本机 libonnxruntime（用户契约 §5.0.14
// 条款 3① 的「无 lib 时真机矩阵乘法不可验收」约束下，正交性仍必须可测）。
type stubShadowScorer struct {
	mu    sync.Mutex
	calls int
	score float64
}

func (s *stubShadowScorer) Score(_ context.Context, features []float32) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.score, nil
}

func (s *stubShadowScorer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// withStubShadowBuilder 替换影子构造注入点（仅测试允许）。
func withStubShadowBuilder(t *testing.T, stub episodeShadowScorer) {
	t.Helper()
	old := newShadowOnnxScorer
	newShadowOnnxScorer = func(string, []byte) (episodeShadowScorer, error) { return stub, nil }
	t.Cleanup(func() { newShadowOnnxScorer = old })
}

func writeShadowModel(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shadow.onnx")
	if err := os.WriteFile(p, []byte("fake-onnx-model-bytes-for-orthogonality-test"), 0o600); err != nil {
		t.Fatalf("写影子模型失败: %v", err)
	}
	return p
}

// TestShadowScorerNeverTouchesDecisionScorer 是**硬断言**（§5.0.14 条款 3①）：
// 构造影子打分器后 s.onnxScorer 仍为 nil，且影子结果只落 shadow_inference。
func TestShadowScorerNeverTouchesDecisionScorer(t *testing.T) {
	stub := &stubShadowScorer{score: 0.75}
	withStubShadowBuilder(t, stub)

	srv, err := NewServer(Options{
		OnnxRuntimeLib:         "/nonexistent/libonnxruntime-test.dylib",
		ShadowOnnxModelPath:    writeShadowModel(t),
		ShadowOnnxModelVersion: "shadow-test-v1",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.onnxScorer != nil {
		t.Fatalf("影子构造写入了 s.onnxScorer（决策带被污染）")
	}
	if srv.shadowScorer == nil {
		t.Fatalf("影子 scorer 未构造（注入替身后应就位）")
	}

	cands := []EpisodeCandidate{{Index: 0, Text: "Search"}, {Index: 1, Text: "Sign in"}}
	sh := srv.shadowInference(cands, "search")
	if sh == nil {
		t.Fatalf("影子打分返回 nil")
	}
	if len(sh.Scores) != len(cands) {
		t.Fatalf("影子分基数 = %d，期望 %d", len(sh.Scores), len(cands))
	}
	if sh.Scores[0] != 0.75 {
		t.Fatalf("影子分 = %v，期望 0.75", sh.Scores[0])
	}
	if sh.ModelVersion != "shadow-test-v1" {
		t.Fatalf("model_version = %q，期望 shadow-test-v1", sh.ModelVersion)
	}
	if sh.ModelHash == "" {
		t.Fatalf("model_hash 为空（影子模型不可溯源）")
	}
	if stub.callCount() != len(cands) {
		t.Fatalf("替身调用次数 = %d，期望 %d", stub.callCount(), len(cands))
	}
	if srv.onnxScorer != nil {
		t.Fatalf("影子打分后 s.onnxScorer 被写入")
	}
}

// TestShadowScorerNotConsultedByDecisionPath：决策带（pageCandidatePool）在
// s.onnxScorer == nil 时不得借用影子 scorer（§5.0.14 条款 3①②：只约束决策接线）。
func TestShadowScorerNotConsultedByDecisionPath(t *testing.T) {
	stub := &stubShadowScorer{score: 0.99}
	withStubShadowBuilder(t, stub)

	srv, err := NewServer(Options{
		OnnxRuntimeLib:      "/nonexistent/libonnxruntime-test.dylib",
		ShadowOnnxModelPath: writeShadowModel(t),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	pool, _, err := srv.pageCandidatePool(context.Background(),
		[]browser.PageCandidate{{Text: "Search", Confidence: 0}},
		&uwspb.ActionIntentRequest{TargetSemantic: "search"}, nil)
	if err != nil {
		t.Fatalf("pageCandidatePool: %v", err)
	}
	if len(pool) != 0 {
		t.Fatalf("决策带产出了候选（影子不该入池）：%+v", pool)
	}
	if stub.callCount() != 0 {
		t.Fatalf("决策路径调用了影子 scorer %d 次（越门）", stub.callCount())
	}
}

// TestShadowDegradesToNullWithoutLibOrModel：无 lib / 无模型 ⇒ 影子如实 null，
// 且其它链路零变化（§5.0.14 条款 3③）。
func TestShadowDegradesToNullWithoutLibOrModel(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{name: "无 lib", opts: Options{ShadowOnnxModelPath: writeShadowModel(t)}},
		{name: "无模型路径", opts: Options{OnnxRuntimeLib: "/nonexistent/libonnxruntime.dylib"}},
		{name: "模型文件不存在", opts: Options{
			OnnxRuntimeLib:      "/nonexistent/libonnxruntime.dylib",
			ShadowOnnxModelPath: filepath.Join(t.TempDir(), "absent.onnx"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubShadowScorer{score: 0.5}
			withStubShadowBuilder(t, stub)
			srv, err := NewServer(tc.opts)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			if srv.shadowScorer != nil {
				t.Fatalf("影子 scorer 不该被构造")
			}
			if srv.onnxScorer != nil {
				t.Fatalf("决策 scorer 不该被构造")
			}
			if got := srv.shadowInference([]EpisodeCandidate{{Index: 0, Text: "x"}}, "x"); got != nil {
				t.Fatalf("影子未启用时应为 null，得到 %+v", got)
			}
			if stub.callCount() != 0 {
				t.Fatalf("降级路径仍调用了 scorer %d 次", stub.callCount())
			}
		})
	}
}
