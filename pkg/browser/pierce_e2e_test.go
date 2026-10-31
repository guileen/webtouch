package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPierceE2EPhase1 是 WebTouch v2.0 Phase 1（穿透寻底 + Leased Token 同余微重锚）的
// **真机**门禁读数（spec/algorithm-kernel.md §5.0.10；RFC 推演 2 / 裁决 1）。
//
// 门禁：UWS_RUN_CHROME_E2E=1 + UWS_E2E_ATTACH_URL=<CDP 端点>（与其它真机用例同口径）。
// 用例自建测试页（t.TempDir 写入 → file:// 导航），只在本会话自己的 target 里跑；
// 不读、不动、不关用户既有窗口与标签。
//
// 场景（每个场景都断言「值落进真实受体」且「诱饵受体未被写入」）：
//
//	A 封闭 Shadow Root 宿主（自定义元素；页内 shadowRoot 恒为 null）→ 必须走宿主层穿透
//	B opacity:0 透明手势代理层压在真实输入框上 → 页内几何层叠剔除
//	C 派发窗口内水合重建（位移 20px、placeholder 变）→ 同余微重锚救回一次
//	D 派发窗口内水合重建 + 位移 200px → 必须熔断 TARGET_TOPOLOGY_RUPTURE
//	E 封闭根内的邮箱受体 + 非凭据意图 → 必须熔断 TARGET_ROLE_MISMATCH
//
// 水合的击发是**确定性仪器**：页面把 document.querySelector 包一层，在第 2 次读取
// `[data-uws-target="1"]`（＝租约捕获之后、派发前可编辑性检查之前）时重建节点。
// 真实水合在派发窗口内的落点不可复现，故用计数触发把「拓扑在派发窗口内变化」固定下来；
// 被验证的是引擎在该变化下的行为，不是触发时机本身。
func TestPierceE2EPhase1(t *testing.T) {
	if os.Getenv("UWS_RUN_CHROME_E2E") != "1" {
		t.Skip("未设置 UWS_RUN_CHROME_E2E=1，跳过会触碰真实 Chrome 的真机用例")
	}
	attachURL := os.Getenv("UWS_E2E_ATTACH_URL")
	if attachURL == "" {
		t.Skip("未设置 UWS_E2E_ATTACH_URL，跳过真机穿透寻底用例")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	pagePath := filepath.Join(t.TempDir(), "pierce-e2e.html")
	if err := os.WriteFile(pagePath, []byte(pierceE2EPage), 0o644); err != nil {
		t.Fatalf("写测试页: %v", err)
	}
	pageURL := "file://" + pagePath

	f := &CDPFactory{Headless: true}
	sess, err := f.Create(ctx, Config{
		AttachCDPURL:     attachURL,
		Viewport:         &Viewport{Width: 1280, Height: 900, DeviceScaleFactor: 1},
		ViewportExplicit: true,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sess.Close() }()
	s := sess.(*cdpSession)
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("导航测试页 %s: %v", pageURL, err)
	}

	center := func(t *testing.T, expr string) (int, int) {
		t.Helper()
		var pt struct{ X, Y float64 }
		if err := s.evalValue(ctx, expr, &pt); err != nil {
			t.Fatalf("读坐标 %s: %v", expr, err)
		}
		return int(pt.X), int(pt.Y)
	}
	value := func(t *testing.T, expr string) string {
		t.Helper()
		v, err := s.evalResult(ctx, expr)
		if err != nil {
			t.Fatalf("读值 %s: %v", expr, err)
		}
		return v
	}
	// arm 复位仪器：which 非空时把水合挂在第 2 次 `[data-uws-target="1"]` 读取上。
	arm := func(t *testing.T, which string) {
		t.Helper()
		at := "0"
		if which != "" {
			at = "2"
		}
		expr := fmt.Sprintf("(() => { window.__uwsState.qs = 0; window.__uwsState.hydrated = ''; "+
			"window.__uwsState.hydrateWhich = %q; window.__uwsState.hydrateAt = %s; "+
			"document.getElementById('decoy').value = ''; return 'ok'; })()", which, at)
		if _, err := s.evalResult(ctx, expr); err != nil {
			t.Fatalf("复位水合仪器: %v", err)
		}
	}
	type e2eState struct {
		QS           int    `json:"qs"`
		HydrateAt    int    `json:"hydrateAt"`
		HydrateWhich string `json:"hydrateWhich"`
		Hydrated     string `json:"hydrated"`
	}
	state := func(t *testing.T) e2eState {
		t.Helper()
		var st e2eState
		if err := s.evalValue(ctx, "window.__uwsState", &st); err != nil {
			t.Fatalf("读仪器状态: %v", err)
		}
		return st
	}
	decoy := func(t *testing.T) string {
		t.Helper()
		return value(t, "document.getElementById('decoy').value")
	}

	t.Run("A 封闭 Shadow Root 宿主", func(t *testing.T) {
		arm(t, "")
		x, y := center(t, "window.__uwsCenterA()")
		sel, ok, obs := s.InputAt(ctx, x, y, "pierce-e2e-A", "搜索框")
		t.Logf("EVIDENCE A coord=%d,%d sel=%q ok=%v observed=%q decoy=%q realA=%q",
			x, y, sel, ok, obs, decoy(t), value(t, "window.__uwsReadA()"))
		if !ok {
			t.Fatalf("A：封闭根内输入框必须被穿透寻底命中（observed=%q）", obs)
		}
		if !strings.Contains(obs, "pierced:") {
			t.Fatalf("A：必须由宿主层穿透出口完成（轴 AX/DOM 命中），observed=%q", obs)
		}
		if got := value(t, "window.__uwsReadA()"); got != "pierce-e2e-A" {
			t.Fatalf("A：值未落进封闭根真实受体，got=%q", got)
		}
		if got := decoy(t); got != "" {
			t.Fatalf("A：值落进了诱饵受体（写错元素），decoy=%q", got)
		}
	})

	t.Run("B opacity:0 透明手势代理层", func(t *testing.T) {
		arm(t, "")
		x, y := center(t, "window.__uwsCenterOf('real_b')")
		sel, ok, obs := s.InputAt(ctx, x, y, "pierce-e2e-B", "search_box")
		t.Logf("EVIDENCE B coord=%d,%d sel=%q ok=%v observed=%q decoy=%q realB=%q",
			x, y, sel, ok, obs, decoy(t), value(t, "window.__uwsValueOf('real_b')"))
		if !ok {
			t.Fatalf("B：opacity:0 覆盖层必须被页内几何层叠剔除（observed=%q）", obs)
		}
		if got := value(t, "window.__uwsValueOf('real_b')"); got != "pierce-e2e-B" {
			t.Fatalf("B：值未落进真实输入框，got=%q", got)
		}
		if got := decoy(t); got != "" {
			t.Fatalf("B：值落进了诱饵受体（写错元素），decoy=%q", got)
		}
	})

	t.Run("C 派发窗口内水合重建（位移 20px → 同余微重锚）", func(t *testing.T) {
		arm(t, "C")
		x, y := center(t, "window.__uwsCenterOf('real_c')")
		sel, ok, obs := s.InputAt(ctx, x, y, "pierce-e2e-C", "search_box")
		st := state(t)
		got := value(t, "window.__uwsValueOf('real_c')")
		hydrated := value(t, "document.getElementById('real_c').dataset.hydrated")
		t.Logf("EVIDENCE C coord=%d,%d sel=%q ok=%v observed=%q qs=%d hydratedFlag=%q hydratedNode=%q value=%q decoy=%q",
			x, y, sel, ok, obs, st.QS, st.Hydrated, hydrated, got, decoy(t))
		if st.Hydrated != "C" {
			t.Fatalf("C：水合未按预期击发（instrument=%+v）", st)
		}
		if !ok {
			t.Fatalf("C：同余微重锚应救回这一次派发（observed=%q）", obs)
		}
		if hydrated != "1" {
			t.Fatalf("C：值应落进水合**重建后**的新受体（新节点未见 data-hydrated），value=%q", got)
		}
		if got != "pierce-e2e-C" {
			t.Fatalf("C：值未落进水合后新受体，got=%q", got)
		}
		if d := decoy(t); d != "" {
			t.Fatalf("C：值落进了诱饵受体（写错元素），decoy=%q", d)
		}
	})

	t.Run("D 水合位移 200px → 必须熔断", func(t *testing.T) {
		arm(t, "D")
		x, y := center(t, "window.__uwsCenterOf('real_d')")
		sel, ok, obs := s.InputAt(ctx, x, y, "pierce-e2e-D", "search_box")
		t.Logf("EVIDENCE D coord=%d,%d sel=%q ok=%v observed=%q instrument=%+v value=%q",
			x, y, sel, ok, obs, state(t), value(t, "window.__uwsValueOf('real_d')"))
		if ok {
			t.Fatalf("D：位移 200px 超出 32px 局域窗口，必须熔断而不是猜（observed=%q）", obs)
		}
		// TARGET_TOPOLOGY_RUPTURE 见 §5.0.8 条款 1 代号表（此处用字面量，便于本用例在改动前代码上作对照）
		if !strings.Contains(obs, "TARGET_TOPOLOGY_RUPTURE") {
			t.Fatalf("D：必须给出 TARGET_TOPOLOGY_RUPTURE 代号，observed=%q", obs)
		}
		if !strings.Contains(obs, "displacement_exceeded") {
			t.Fatalf("D：熔断原因应为 displacement_exceeded，observed=%q", obs)
		}
		if got := value(t, "window.__uwsValueOf('real_d')"); got != "" {
			t.Fatalf("D：熔断后受体不得被写入，got=%q", got)
		}
	})

	t.Run("E 封闭根内敏感受体必须熔断", func(t *testing.T) {
		arm(t, "")
		x, y := center(t, "window.__uwsCenterE()")
		_, ok, obs := s.InputAt(ctx, x, y, "pierce-e2e-E", "搜索框")
		t.Logf("EVIDENCE E coord=%d,%d ok=%v observed=%q value=%q",
			x, y, ok, obs, value(t, "window.__uwsValueE()"))
		if ok {
			t.Fatalf("E：非凭据意图写入封闭根内的邮箱受体必须熔断（observed=%q）", obs)
		}
		if !strings.Contains(obs, "TARGET_ROLE_MISMATCH") {
			t.Fatalf("E：必须给出 TARGET_ROLE_MISMATCH 代号，observed=%q", obs)
		}
		if got := value(t, "window.__uwsValueE()"); got != "" {
			t.Fatalf("E：熔断后受体不得被写入，got=%q", got)
		}
	})
}

// pierceE2EPage 是自建测试页：诱饵受体（DOM 首个 input）在前，用于识别「值写给了
// 兄弟子树里的无关输入框」这一类假成功——旧实现（裸 elementFromPoint + 兄弟子树扫描）
// 会命中它；新实现必须命中各场景的真实受体。
const pierceE2EPage = `<!doctype html>
<html lang="zh"><head><meta charset="utf-8"><title>uws pierce e2e</title>
<style>
  body{margin:0;font:14px/1.4 sans-serif}
  input{width:300px;height:40px;box-sizing:border-box}
  #overlay{position:fixed;left:60px;top:220px;width:300px;height:40px;background:#000;opacity:0;z-index:99}
  #proxyC{position:fixed;left:60px;top:300px;width:300px;height:40px;background:#111;pointer-events:none;z-index:99}
  #proxyD{position:fixed;left:60px;top:380px;width:300px;height:40px;background:#111;pointer-events:none;z-index:99}
</style></head>
<body>
<input id="decoy" name="decoy" type="text" placeholder="诱饵受体" style="position:fixed;left:700px;top:20px;width:300px;height:40px">
<script>
(function () {
  var slot = function (top) { return 'position:fixed;left:60px;top:' + top + 'px;width:300px;height:40px;box-sizing:border-box'; };
  var center = function (el) { var b = el.getBoundingClientRect(); return {x: b.left + b.width / 2, y: b.top + b.height / 2}; };

  // ── 场景 A：closed Shadow Root 宿主（自定义元素；页内 shadowRoot 恒为 null）──
  var hostA = document.createElement('x-pierce-host');
  hostA.style.cssText = slot(60);
  document.body.appendChild(hostA);
  var rootA = hostA.attachShadow({mode: 'closed'});
  var inputA = document.createElement('input');
  inputA.id = 'real_a'; inputA.setAttribute('name', 'a'); inputA.setAttribute('type', 'search');
  inputA.setAttribute('aria-label', '搜索框 A'); inputA.setAttribute('placeholder', 'A 封闭根输入框');
  inputA.style.cssText = 'width:300px;height:40px;box-sizing:border-box';
  rootA.appendChild(inputA);

  // ── 场景 E：同一个封闭宿主形态，但根内是邮箱受体（意图不含凭据词 → 必须熔断）──
  var hostE = document.createElement('x-pierce-email');
  hostE.style.cssText = slot(140);
  document.body.appendChild(hostE);
  var rootE = hostE.attachShadow({mode: 'closed'});
  var inputE = document.createElement('input');
  inputE.id = 'hero_user_email'; inputE.setAttribute('name', 'hero_user_email');
  inputE.setAttribute('type', 'email'); inputE.setAttribute('aria-label', '注册邮箱');
  inputE.style.cssText = 'width:300px;height:40px;box-sizing:border-box';
  rootE.appendChild(inputE);

  // ── 场景 B/C/D：真实输入框 + 覆盖层 ──
  var mkInput = function (id, top, ph) {
    var el = document.createElement('input');
    el.id = id; el.setAttribute('name', id.slice(-1)); el.setAttribute('type', 'text');
    el.setAttribute('placeholder', ph); el.style.cssText = slot(top);
    document.body.appendChild(el);
    return el;
  };
  mkInput('real_b', 220, 'B 真实输入框');
  mkInput('real_c', 300, 'C 水合前');
  mkInput('real_d', 380, 'D 水合前');
  document.body.insertAdjacentHTML('beforeend', '<div id="overlay"></div><div id="proxyC"></div><div id="proxyD"></div>');

  // 水合重建：id/name/type/DOM 路径不变，placeholder 变、几何位移（C=20px / D=200px）
  var hydrate = function (id, top, ph) {
    var old = document.getElementById(id);
    var next = document.createElement('input');
    next.id = id; next.setAttribute('name', id.slice(-1)); next.setAttribute('type', 'text');
    next.setAttribute('placeholder', ph);
    next.style.cssText = slot(top);
    next.dataset.hydrated = '1';
    old.parentNode.replaceChild(next, old);
    window.__uwsState.hydrated = id.slice(-1).toUpperCase();
  };

  window.__uwsState = {qs: 0, hydrateAt: 0, hydrateWhich: '', hydrated: ''};
  window.__uwsReadA = function () { return rootA.getElementById('real_a').value; };
  window.__uwsValueE = function () { return rootE.getElementById('hero_user_email').value; };
  window.__uwsCenterA = function () { return center(rootA.getElementById('real_a')); };
  window.__uwsCenterE = function () { return center(rootE.getElementById('hero_user_email')); };
  window.__uwsCenterOf = function (id) { return center(document.getElementById(id)); };
  window.__uwsValueOf = function (id) { var el = document.getElementById(id); return el ? (el.value || '') : ''; };

  // 确定性水合仪器：第 N 次读取已标记受体时重建（N = 2：租约捕获之后、可编辑性检查之前）。
  // 真实水合在派发窗口内的落点不可复现，这里把「拓扑在派发窗口内变化」固定下来。
  var realQS = Document.prototype.querySelector;
  document.querySelector = function (sel) {
    if (typeof sel === 'string' && sel.indexOf('[data-uws-target="1"]') >= 0) {
      window.__uwsState.qs++;
      if (window.__uwsState.hydrateAt && window.__uwsState.qs === window.__uwsState.hydrateAt) {
        if (window.__uwsState.hydrateWhich === 'D') hydrate('real_d', 580, 'D 水合后');
        else hydrate('real_c', 320, 'C 水合后');
      }
    }
    return realQS.call(this, sel);
  };
})();
</script>
</body></html>
`
