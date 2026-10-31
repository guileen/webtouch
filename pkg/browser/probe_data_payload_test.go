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

// §5.0.6 条款 3（2026-09-25 扩写）：内联数据载体的文本源剔除。
//
// 靶点（Baidu 第十批实测）：`<style` 污染在此前批次已消除，但污染**换了载体**——
// 站点把整段数据块 JSON 塞进 textarea/内联脚本，探针仍会把它当作元素文本抽出来，
// 挤占正常输入框的召回。本页复刻该形状（保留 offscreen 但渲染盒非零，与 Baidu
// `[227] textarea "…"[offscreen]` 一致，避免被 §5.0.5 几何剪枝提前剔除而掩盖回归）。
func dataPayloadPage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><head>
  <script type="application/json" id="__NEXT_DATA__">{"props":{"hotsearch":[{"word":"webtouch-json-noise"}]}}</script>
  <script type="importmap">{"imports":{"x":"webtouch-importmap-noise"}}</script>
</head><body style="margin:0;">
  <textarea id="hotsearch-ta"
    style="position:absolute;left:-5000px;top:0;width:300px;height:40px;">{ "hotsearch": [ { "word": "webtouch-json-noise" } ] }</textarea>
  <textarea id="style-ta"
    style="position:absolute;left:-5000px;top:60px;width:300px;height:40px;"><style data-for="result">.s{color:red}</style></textarea>
  <input id="search" type="text" placeholder="搜索" style="display:block;width:320px;height:36px;margin:20px;">
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// 污染串必须整体消失：JSON 数据块、importmap、`<style` 模板都不得进入语义文本源。
var dataPayloadNoise = []string{
	"webtouch-json-noise", "hotsearch", "__NEXT_DATA__", "webtouch-importmap-noise", "style data-for",
}

func assertNoDataPayloadNoise(t *testing.T, texts []string) {
	t.Helper()
	for _, text := range texts {
		for _, noise := range dataPayloadNoise {
			if strings.Contains(text, noise) {
				t.Fatalf("数据载体串进了语义文本源（noise=%q）: %q", noise, text)
			}
		}
	}
}

// WASM 探针路径（主采集通道）：数据载体既不进交互候选，也不进文本源。
func TestCDPSessionWasmProbePrunesInlineDataPayload(t *testing.T) {
	pageURL := dataPayloadPage(t)
	s := newTestCDPSessionWithProbe(t, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	cands := s.PageCandidates()
	var texts []string
	var collect func([]PageCandidate)
	collect = func(list []PageCandidate) {
		for _, c := range list {
			texts = append(texts, c.Text)
			collect(c.Children)
		}
	}
	collect(cands)
	if len(texts) == 0 {
		t.Fatalf("探针未产出任何候选（stats=%+v）", s.ProbeStats())
	}
	assertNoDataPayloadNoise(t, texts)

	// 真实输入框必须仍然被召回（证明剪枝没有连坐正常控件）。
	found := false
	for _, text := range texts {
		if strings.Contains(text, "搜索") {
			found = true
		}
	}
	if !found {
		t.Fatalf("真实搜索输入框应仍在候选：%+v", texts)
	}
}

// 探针缺失时的 JS 降级采集路径：同口径剔除数据载体。
func TestCDPSessionJSCandidatesPrunesInlineDataPayload(t *testing.T) {
	pageURL := dataPayloadPage(t)
	s := newTestCDPSession(t, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	cands := s.collectJSCandidates(ctx)
	if len(cands) == 0 {
		t.Fatalf("降级采集未产出任何候选")
	}
	texts := make([]string, 0, len(cands))
	for _, c := range cands {
		texts = append(texts, c.Text)
	}
	assertNoDataPayloadNoise(t, texts)
}

func TestCDPSessionJSCandidatesCarryOffscreenGeometryAndSelector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body style="margin:0;height:2600px">
			<main style="position:absolute;top:1700px;left:40px">
				<button style="width:180px;height:36px">History archive</button>
			</main>
		</body></html>`)
	}))
	defer srv.Close()

	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	cands := s.collectJSCandidates(ctx)
	for _, c := range cands {
		if c.Text != "History archive" {
			continue
		}
		if c.Selector == "" || c.W <= 0 || c.H <= 0 || c.DocumentY < 1700 {
			t.Fatalf("fallback candidate metadata incomplete: %+v", c)
		}
		return
	}
	t.Fatalf("deep fallback candidate not found: %+v", cands)
}
