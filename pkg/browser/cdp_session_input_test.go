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

// §5.0.3 条款 1/3（先红后绿）：页面只有不可编辑的「搜索」出口（button）时，input 必须
// 明确失败并给出 input_not_editable —— 旧行为「元素原生文案非空 ⇒ 报成功」在此必红
// （12 站实采 github 那格：命中 `span`/`button` «Search» 仍报 SUCCEEDED）。
func TestCDPSessionInputGateRejectsNonEditableTarget(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <header><button type="button" aria-label="Search or jump to, type / to search">Search</button></header>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	ok, observed := s.ExecuteAction(ctx, "input", "webtouch-gate", "search_box")
	if ok {
		t.Fatalf("非可编辑目标不得报成功（假成功根因），observed=%q", observed)
	}
	if !strings.Contains(observed, "input_not_editable") {
		t.Fatalf("失败原因应为 input_not_editable，observed=%q", observed)
	}
}

// §5.0.3 条款 2：赢家不可编辑、但页面存在可编辑候选（placeholder 命中）时必须在可编辑
// 候选内重选 —— 不因门禁丢掉真目标（wikipedia 形态：面积更小的 button 先赢评分平局）。
func TestCDPSessionInputGateReselectsEditableCandidate(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <form action="/search">
    <button type="submit">Search</button>
    <input id="q" name="query" placeholder="Search Wikipedia">
  </form>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	ok, observed := s.ExecuteAction(ctx, "input", "wikipedia-gate", "search_box")
	if !ok {
		t.Fatalf("存在可编辑候选时必须重选命中，observed=%q", observed)
	}
	val, err := s.evalResult(ctx, `(document.getElementById('q') || {}).value || ''`)
	if err != nil {
		t.Fatalf("eval value: %v", err)
	}
	if val != "wikipedia-gate" {
		t.Fatalf("值必须落进可编辑候选且不得叠写，got %q", val)
	}
}

// §5.0.3 条款 4/5：落地判据是「回读值包含输入值」；连续两次 input 必须整体替换而不是
// 追加（旧行为在非可编辑命中时会把同一值写两遍，ddg 实测 `webtouchwebtouch`）。
func TestCDPSessionInputGateLandsValueInEditableTarget(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <form action="/search"><input id="q" name="q" placeholder="Search Amazon"></form>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	for _, want := range []string{"webtouch-one", "webtouch-two"} {
		if ok, observed := s.ExecuteAction(ctx, "input", want, "search_box"); !ok {
			t.Fatalf("input(%q) 失败: %s", want, observed)
		}
		val, err := s.evalResult(ctx, `(document.getElementById('q') || {}).value || ''`)
		if err != nil {
			t.Fatalf("eval value: %v", err)
		}
		if val != want {
			t.Fatalf("写入 %q 后回读 %q（不得追加/叠写）", want, val)
		}
	}
}

// §5.0.3 条款 1 召回保真：富文本编辑器（contenteditable + aria-label 命中）必须仍被接受，
// 否则等于把 Gemini/DeepSeek 一类编辑器的输入通道一起砍掉。
func TestCDPSessionInputGateAcceptsContentEditable(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <div id="editor" contenteditable="true" aria-label="Search" style="min-height:48px;border:1px solid #ccc"></div>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	ok, observed := s.ExecuteActionWithSelector(ctx, "input", "contenteditable-ok", "search_box", "div#editor")
	if !ok {
		t.Fatalf("contenteditable 目标必须被接受，observed=%q", observed)
	}
	text, err := s.evalResult(ctx, `(document.getElementById('editor') || {}).innerText || ''`)
	if err != nil {
		t.Fatalf("eval innerText: %v", err)
	}
	if !strings.Contains(text, "contenteditable-ok") {
		t.Fatalf("值必须真的落进 contenteditable，got %q", text)
	}
}

// §5.0.4 角色线性效用启发式与比较域约束（先红后绿）：
// 高仿百度搜索场景——并列一个小图标 <div style="width:20px;height:20px">语音搜索</div>
// 与一个大搜索框 <input placeholder="百度搜索" style="width:400px;height:40px">。
// 双方文本匹配度一致（80 分）。
//   - 改动前：无角色加成，且平分时按 min(area) 取胜，导致微型 div (400px²) 战胜大 input (16000px²)，
//     随后触发门禁报错 input_not_editable；
//   - 改动后：输入框获得 +20 角色效用加成（100 > 80），在首轮打分中绝对胜出，成功输入。
func TestCDPSessionInputRoleUtilityBeatsMicroDiv(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <div style="display:flex;align-items:center;">
    <div id="voice-icon" style="width:20px;height:20px;background:#ccc;" title="语音搜索">语音搜索</div>
    <input id="kw" name="wd" placeholder="百度搜索" style="width:400px;height:40px;">
  </div>
</body></html>`)
	}))
	defer srv.Close()
	s := newTestCDPSession(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	ok, observed := s.ExecuteAction(ctx, "input", "webtouch-baidu-win", "search_box")
	if !ok {
		t.Fatalf("可编辑搜索框应凭借角色加成战胜微型div，observed=%q", observed)
	}
	val, err := s.evalResult(ctx, `(document.getElementById('kw') || {}).value || ''`)
	if err != nil {
		t.Fatalf("eval kw.value: %v", err)
	}
	if val != "webtouch-baidu-win" {
		t.Fatalf("值必须成功落入主输入框，got %q", val)
	}
}
