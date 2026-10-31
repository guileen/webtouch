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

// §5.0.9 条款 1 + 条款 2 的真机证据：VLM 坐标路径
// ① 敏感受体（注册邮箱框）+ 非凭据意图 ⇒ 不派发、回执带 TARGET_ROLE_MISMATCH、值不落地；
// ② 同坐标 + 凭据语义 ⇒ 正常落地（守卫不误杀合法写入）；
// ③ 搜索受体（input#search）+ 搜索意图 ⇒ 正常落地（搜索受体优先）。
func TestInputAtGuardBlocksSensitiveReceptor(t *testing.T) {
	requireCDP(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body style="margin:0;">
  <div style="padding:40px;">
    <input id="hero_user_email" type="email" name="user[email]" placeholder="Email address"
           style="display:block;width:320px;height:36px;">
    <input id="search" type="text" placeholder="搜索" style="display:block;width:320px;height:36px;margin-top:20px;">
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

	center := func(id string) (int, int) {
		t.Helper()
		var pos struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		}
		js := fmt.Sprintf(`(() => { const r = document.getElementById(%q).getBoundingClientRect();
		  return {x: Math.round(r.left + r.width/2), y: Math.round(r.top + r.height/2)}; })()`, id)
		if err := s.evalValue(ctx, js, &pos); err != nil {
			t.Fatalf("读取 %s 中心坐标: %v", id, err)
		}
		return int(pos.X), int(pos.Y)
	}
	valueOf := func(id string) string {
		t.Helper()
		v, err := s.evalResult(ctx, fmt.Sprintf(`(document.getElementById(%q) || {}).value || ''`, id))
		if err != nil {
			t.Fatalf("回读 %s.value: %v", id, err)
		}
		return v
	}

	// ① 邮箱受体 + 搜索意图 ⇒ 熔断，且不得派发。
	ex, ey := center("hero_user_email")
	_, ok, observed := s.InputAt(ctx, ex, ey, "webtouch-guard-probe", "search_box")
	if ok {
		t.Fatalf("敏感受体 + 搜索意图不得报成功，observed=%q", observed)
	}
	if !strings.Contains(observed, FaultTargetRoleMismatch) {
		t.Fatalf("observed 必须透传 %s 代号，got %q", FaultTargetRoleMismatch, observed)
	}
	if v := valueOf("hero_user_email"); v != "" {
		t.Fatalf("熔断后邮箱框必须保持空（不得派发），got %q", v)
	}

	// ② 同坐标 + 凭据语义 ⇒ 放行并过落地回读。
	sel, ok, observed := s.InputAt(ctx, ex, ey, "webtouch-guard-probe", "登录邮箱")
	if !ok {
		t.Fatalf("凭据语义应放行，observed=%q", observed)
	}
	if sel == "" {
		t.Fatalf("成功必须回传命中选择器")
	}
	if v := valueOf("hero_user_email"); !strings.Contains(v, "webtouch-guard-probe") {
		t.Fatalf("凭据语义下值必须落地，got %q", v)
	}

	// ③ 搜索受体 + 搜索意图 ⇒ 不得被 user 一族关键词误伤（同一坐标机制）。
	sx, sy := center("search")
	if _, ok, observed := s.InputAt(ctx, sx, sy, "webtouch-search-probe", "search_box"); !ok {
		t.Fatalf("合法搜索受体不得熔断，observed=%q", observed)
	}
	if v := valueOf("search"); !strings.Contains(v, "webtouch-search-probe") {
		t.Fatalf("搜索值必须落地，got %q", v)
	}
}

// §5.0.9 条款 1 的 CI 阻断门禁（不需要 Chrome，走伪 CDP 服务器，默认即跑）：
// 意图 search_box 命中 input#hero_user_email 时，必须在**任何物理事件之前**熔断——
// 不派发鼠标事件（不做探测性点击/聚焦）、不派发键盘事件，表单值零污染。
// 这是对真机 e2e 的确定性补充：e2e 需 UWS_RUN_CHROME_E2E=1，CI 默认会跳过。
func TestInputAtGuardBlocksBeforeAnyDispatch(t *testing.T) {
	fake := &cancelFakeCDP{
		// Runtime.evaluate 调用序：① 穿透寻底（§5.0.10 条款 1，返回 {ok,sel}）；
		// ② 派发前租约快照（§5.0.10 条款 2；无标记载体 ⇒ found=false）；
		// ③ 受体角色探针（命中邮箱框 ⇒ 熔断，早于任何物理事件）。
		evalSeq: []string{
			`{"ok":true,"sel":"input#hero_user_email","tag":"input"}`,
			`{"found":false}`,
			`{"found":true,"tag":"input","type":"email","id":"hero_user_email","name":"user[email]"}`,
		},
	}
	s := newCancelTestSession(t, fake.serve(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok, observed := s.InputAt(ctx, 10, 20, "webtouch-guard-probe", "search_box")
	if ok {
		t.Fatalf("敏感受体 + 非凭据意图不得报成功，observed=%q", observed)
	}
	if !strings.Contains(observed, FaultTargetRoleMismatch) {
		t.Fatalf("observed 必须透传 %s 代号，got %q", FaultTargetRoleMismatch, observed)
	}
	for _, method := range []string{"Input.dispatchMouseEvent", "Input.dispatchKeyEvent", "Input.insertText"} {
		if n := fake.count(method); n != 0 {
			t.Fatalf("守卫必须在派发前熔断：%s 已派发 %d 条", method, n)
		}
	}
}
