package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// §5.0.8 条款 3 的纯分类：CSP 拒绝 WASM 的失败文本必须被识别为 csp_wasm_blocked，
// 其余失败一律不误判为降级代号。
func TestClassifyProbeDegradation(t *testing.T) {
	// 真实文本（案卷 §E5 / §10.7：github 实测原文）。
	githubCSP := `WebAssembly.instantiate(): Compiling or instantiating WebAssembly module violates ` +
		`the following Content Security policy directive because 'unsafe-eval' is not an allowed ` +
		`source of script: "script-src github.githubassets.com 'sha256-abc'"`

	cases := []struct {
		msg  string
		want string
	}{
		{githubCSP, FaultCSPWasmBlocked},
		{`Refused to compile WebAssembly module because 'wasm-unsafe-eval' is not an allowed source`, FaultCSPWasmBlocked},
		{`Content-Security-Policy: The page's settings blocked the loading of a resource`, FaultCSPWasmBlocked},
		{"", ""},
		{"probe: bulk ingest failed", ""},
		{"WebAssembly.instantiate(): CompileError: invalid magic word", ""},
		{"ax tree: 无可见语义节点（页面空白或 canvas/纯图形站点）", ""},
	}
	for _, c := range cases {
		if got := classifyProbeDegradation(c.msg); got != c.want {
			t.Fatalf("classifyProbeDegradation(%.60q) = %q，want %q", c.msg, got, c.want)
		}
	}
}

// §5.0.8 条款 3 的真机证据：严格 CSP 页面上页内 WASM 探针不可用，
// 降级必须显式可判别——错误串、采集状态快照、PageCandidates 回退路径三处都要带 csp_wasm_blocked。
func TestCDPSessionProbeCSPDegradationMarked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 复刻 github 的严格 CSP：script-src 不含 unsafe-eval / wasm-unsafe-eval。
		w.Header().Set("Content-Security-Policy", "script-src 'self'; object-src 'none'")
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body style="margin:0;">
  <input id="search" type="text" placeholder="搜索" style="display:block;width:320px;height:36px;">
</body></html>`)
	}))
	defer srv.Close()

	s := newTestCDPSessionWithProbe(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	_, stats, err := s.collectProbeCandidates(ctx)
	if err == nil {
		t.Skip("本机 Chrome 未对 CDP 注入的 WASM 施加页面 CSP —— 该站不复现 E5，跳过")
	}
	if !strings.Contains(err.Error(), FaultCSPWasmBlocked) {
		t.Fatalf("探针失败必须显式携带 %s 代号，got %v", FaultCSPWasmBlocked, err)
	}
	if stats.Degradation != FaultCSPWasmBlocked {
		t.Fatalf("采集状态快照必须带降级代号，got %+v", stats)
	}

	// ExportPageTree 的错误串必须可判别（不得只剩「AXTree 不可用」）。
	if _, terr := s.ExportPageTree(ctx); terr == nil || !strings.Contains(terr.Error(), FaultCSPWasmBlocked) {
		t.Fatalf("ExportPageTree 错误串必须携带 %s，got %v", FaultCSPWasmBlocked, terr)
	}

	// PageCandidates 走 JS 降级通道，但降级标记不得随失败丢失。
	if cands := s.PageCandidates(); len(cands) == 0 {
		t.Fatalf("降级通道仍应产出 JS 采集候选")
	}
	if got := s.ProbeStats().Degradation; got != FaultCSPWasmBlocked {
		t.Fatalf("降级后 ProbeStats().Degradation = %q，want %s", got, FaultCSPWasmBlocked)
	}
}
