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

// 本文件是 §5.0.10 条款 2 两个标称阈值的**标定仪器**（真机门禁同 TestPierceE2EPhase1）：
// 把派发窗口内的水合位移（几何窗口 32px）与延迟（租约时效 1500ms）参数化，
// 在边界两侧各取读数。读数与结论见
// docs/evidence/pilots/pierce-threshold-calibration-2026-09-25.md。
// 水位提醒：本仪器是**自建页**上的确定性边界标定，不替代线上动态站点的分布采样。
func sweepAttach(t *testing.T) (*cdpSession, context.Context) {
	t.Helper()
	if os.Getenv("UWS_RUN_CHROME_E2E") != "1" {
		t.Skip("未设置 UWS_RUN_CHROME_E2E=1")
	}
	attachURL := os.Getenv("UWS_E2E_ATTACH_URL")
	if attachURL == "" {
		t.Skip("未设置 UWS_E2E_ATTACH_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	t.Cleanup(cancel)
	pagePath := filepath.Join(t.TempDir(), "sweep.html")
	if err := os.WriteFile(pagePath, []byte(sweepPage), 0o644); err != nil {
		t.Fatalf("写测试页: %v", err)
	}
	f := &CDPFactory{Headless: true}
	sess, err := f.Create(ctx, Config{
		AttachCDPURL:     attachURL,
		Viewport:         &Viewport{Width: 1280, Height: 900, DeviceScaleFactor: 1},
		ViewportExplicit: true,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	s := sess.(*cdpSession)
	if err := s.Navigate(ctx, "file://"+pagePath); err != nil {
		t.Fatalf("导航: %v", err)
	}
	return s, ctx
}

func sweepRun(t *testing.T, s *cdpSession, ctx context.Context, dx, delay int, tag string) (bool, bool, string) {
	t.Helper()
	intent := fmt.Sprintf("%s-%d-%d", tag, dx, delay)
	if _, err := s.evalResult(ctx, fmt.Sprintf("window.__uwsResetC(%d, %d)", dx, delay)); err != nil {
		t.Fatalf("复位: %v", err)
	}
	var pt struct{ X, Y float64 }
	if err := s.evalValue(ctx, "window.__uwsCenterC()", &pt); err != nil {
		t.Fatalf("读坐标: %v", err)
	}
	sel, ok, obs := s.InputAt(ctx, int(pt.X), int(pt.Y), intent, "search_box")
	landed, err := s.evalResult(ctx, "window.__uwsReadC()")
	if err != nil {
		t.Fatalf("读值: %v", err)
	}
	hydrated, err := s.evalResult(ctx, "window.__uwsHydratedC()")
	if err != nil {
		t.Fatalf("读水合标记: %v", err)
	}
	t.Logf("EVIDENCE dx=%d delay=%d sel=%q ok=%v observed=%q hydratedNode=%q value=%q",
		dx, delay, sel, ok, obs, hydrated, landed)
	return ok && hydrated == "1" && landed == intent, ok, obs
}

// TestPierceSweepDisplacement 扫描位移：几何窗口边界是否精确落在 32px。
func TestPierceSweepDisplacement(t *testing.T) {
	s, ctx := sweepAttach(t)
	for _, dx := range []int{0, 8, 16, 24, 28, 30, 32, 33, 34, 36, 40, 48, 64, 128, 200} {
		healed, rupture := 0, 0
		for i := 1; i <= 3; i++ {
			h, ok, obs := sweepRun(t, s, ctx, dx, 0, fmt.Sprintf("dx%d", dx))
			switch {
			case h:
				healed++
			case !ok && strings.Contains(obs, "TARGET_TOPOLOGY_RUPTURE"):
				rupture++
			}
		}
		t.Logf("POINT dx=%d n=3 healed=%d rupture=%d", dx, healed, rupture)
	}
}

// TestPierceSweepLeaseTTL 扫描租约时效：派发前页内忙等 delayMs 再水合（位移固定 16px，在几何窗口内）。
func TestPierceSweepLeaseTTL(t *testing.T) {
	s, ctx := sweepAttach(t)
	for _, delay := range []int{0, 600, 1200, 1450, 1550, 1900} {
		healed, rupture := 0, 0
		reasons := map[string]int{}
		for i := 1; i <= 3; i++ {
			h, ok, obs := sweepRun(t, s, ctx, 16, delay, fmt.Sprintf("ttl%d", delay))
			switch {
			case h:
				healed++
			case !ok && strings.Contains(obs, "TARGET_TOPOLOGY_RUPTURE"):
				rupture++
				reasons[obs]++
			}
		}
		t.Logf("TTL delay=%dms n=3 healed=%d rupture=%d reasons=%v", delay, healed, rupture, reasons)
	}
}

// sweepPage 与 pierceE2EPage 场景 C 同构，但位移/延迟由 `__uwsResetC(dx, delay)` 参数化。
const sweepPage = `<!doctype html>
<html lang="zh"><head><meta charset="utf-8"><title>uws sweep</title>
<style>body{margin:0;font:14px/1.4 sans-serif}input{width:300px;height:40px;box-sizing:border-box}</style>
</head><body>
<input id="decoy" name="decoy" type="text" placeholder="诱饵受体" style="position:fixed;left:700px;top:20px;width:300px;height:40px">
<script>
(function () {
  var BASE_TOP = 300, LEFT = 60;
  var mk = function (top) {
    var el = document.createElement('input');
    el.id = 'real_c'; el.setAttribute('name', 'c'); el.setAttribute('type', 'text');
    el.setAttribute('placeholder', '水合前');
    el.style.cssText = 'position:fixed;left:' + LEFT + 'px;top:' + top + 'px;width:300px;height:40px;box-sizing:border-box';
    return el;
  };
  document.body.appendChild(mk(BASE_TOP));
  window.__uwsSweep = {qs: 0, dx: null, delay: 0, hydrated: false};
  var center = function (el) { var b = el.getBoundingClientRect(); return {x: b.left + b.width / 2, y: b.top + b.height / 2}; };
  window.__uwsCenterC = function () { return center(document.getElementById('real_c')); };
  window.__uwsReadC = function () { var el = document.getElementById('real_c'); return el ? (el.value || '') : ''; };
  window.__uwsHydratedC = function () { var el = document.getElementById('real_c'); return el ? (el.dataset.hydrated || '') : ''; };
  var replace = function (top, hydrated) {
    var old = document.getElementById('real_c');
    var nx = mk(top);
    if (hydrated) nx.dataset.hydrated = '1';
    old.parentNode.replaceChild(nx, old);
    window.__uwsSweep.hydrated = hydrated;
  };
  window.__uwsResetC = function (dx, delay) {
    window.__uwsSweep.qs = 0; window.__uwsSweep.dx = dx; window.__uwsSweep.delay = delay || 0;
    window.__uwsSweep.hydrated = false;
    replace(BASE_TOP, false);
    document.getElementById('decoy').value = '';
    return 'ok';
  };
  var realQS = Document.prototype.querySelector;
  document.querySelector = function (sel) {
    if (typeof sel === 'string' && sel.indexOf('[data-uws-target="1"]') >= 0) {
      window.__uwsSweep.qs++;
      if (window.__uwsSweep.qs === 2 && window.__uwsSweep.dx !== null) {
        var wait = window.__uwsSweep.delay || 0, t0 = performance.now();
        while (performance.now() - t0 < wait) {}
        replace(BASE_TOP + window.__uwsSweep.dx, true);
      }
    }
    return realQS.call(this, sel);
  };
})();
</script>
</body></html>
`
