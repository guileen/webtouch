package browser

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"uws/pkg/fingerprint"
)

func TestCreateRejectsFingerprintProfileIDMismatch(t *testing.T) {
	profile, err := fingerprint.Parse([]byte(e2eProfileJSON))
	if err != nil {
		t.Fatalf("解析测试档案: %v", err)
	}
	_, err = (&CDPFactory{}).Create(context.Background(), Config{
		ProfileID:   "different-profile",
		Fingerprint: profile,
	})
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("档案与 profile id 不一致应当在启动前失败，got %v", err)
	}
}

// cdpProbe 一次性探测真实 Chrome 是否可启动（沙箱阻断时整组集成测试快速跳过）。
var cdpProbe = struct {
	sync.Once
	ok  bool
	err error
}{}

func requireCDP(t *testing.T) {
	t.Helper()
	if os.Getenv("UWS_RUN_CHROME_E2E") != "1" {
		t.Skip("跳过真实 Chrome 测试（需显式设置 UWS_RUN_CHROME_E2E=1，防止干扰用户日常使用与抢占键盘焦点）")
	}
	if os.Getenv("UWS_CDP_NO_PROBE") == "1" {
		if _, err := resolveChrome(""); err != nil {
			t.Skipf("无 Chrome: %v", err)
		}
		return
	}
	cdpProbe.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		s, err := (&CDPFactory{Headless: true}).Create(ctx, Config{})
		if err != nil {
			cdpProbe.err = err
			return
		}
		_ = s.Close()
		cdpProbe.ok = true
	})
	if !cdpProbe.ok {
		t.Skipf("真实 Chrome 不可用，跳过 CDP 集成测试: %v", cdpProbe.err)
	}
}

// ── 纯逻辑单测（无需 Chrome） ────────────────────────────────────────

func TestBezierPathNonCollinear(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	path := newBezierPath(pt{10, 10}, pt{210, 110}, 30, rng)
	if len(path) < 10 {
		t.Fatalf("path too short: %d", len(path))
	}
	// 起点/终点近似精确。
	if math.Abs(path[0].x-10) > 0.01 || math.Abs(path[0].y-10) > 0.01 {
		t.Fatalf("start mismatch: %+v", path[0])
	}
	last := path[len(path)-1]
	if math.Abs(last.x-210) > 0.01 || math.Abs(last.y-110) > 0.01 {
		t.Fatalf("end mismatch: %+v", last)
	}
	// 轨迹不得为直线：至少一点偏离起点→终点连线 > 1px。
	dx, dy := 200.0, 100.0
	lenD := math.Hypot(dx, dy)
	maxOff := 0.0
	for _, p := range path {
		// 点到直线的距离 |(p-start)×dir|。
		cross := math.Abs((p.x-10)*dy - (p.y-10)*dx)
		off := cross / lenD
		if off > maxOff {
			maxOff = off
		}
	}
	if maxOff < 1.0 {
		t.Fatalf("trajectory is a straight line (maxOff=%.2f)", maxOff)
	}
	// 确定性：同种子输出一致。
	path2 := newBezierPath(pt{10, 10}, pt{210, 110}, 30, rand.New(rand.NewSource(42)))
	if len(path) != len(path2) {
		t.Fatalf("determinism broken")
	}
	for i := range path {
		if path[i] != path2[i] {
			t.Fatalf("determinism broken at %d", i)
		}
	}
}

func TestFreshInflightTTL(t *testing.T) {
	now := time.Now()
	m := map[string]time.Time{
		"fresh":  now.Add(-5 * time.Second),
		"stale":  now.Add(-31 * time.Second),
		"border": now.Add(-30 * time.Second),
	}
	if n := freshInflight(m, now, maxInflightAge); n != 2 {
		t.Fatalf("expected 2 fresh connections, got %d (map=%v)", n, m)
	}
	if _, ok := m["stale"]; ok {
		t.Fatal("超过 TTL 的在途请求应被剪除")
	}
	if _, ok := m["border"]; !ok {
		t.Fatal("恰好等于 TTL 的请求应保留")
	}
}

// 前置（2026-08-27）：executeAction 不得再把「JS 抛异常 / eval 空返回 / 无匹配」
// 统一 mask 成 target_not_found——失败原因必须可区分，并带出当前页面 URL 辅助
// 判断「eval 到了错误 Tab/页面」。classifyFindFailure 是纯函数，零 Chrome 单测。
func TestClassifyFindFailure(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		found   bool
		pageURL string
		want    string
	}{
		{
			name:  "无匹配",
			err:   nil,
			found: false,
			want:  "no_match",
		},
		{
			name:    "无匹配带当前 URL",
			err:     nil,
			found:   false,
			pageURL: "https://myread.top/store",
			want:    "no_match|url=https://myread.top/store",
		},
		{
			name:  "JS 抛异常（exceptionDetails 原样透传）",
			err:   errors.New(`cdp: evaluate exception: {"text":"TypeError: x is not a function"}`),
			found: false,
			want:  `eval_error:cdp: evaluate exception: {"text":"TypeError: x is not a function"}`,
		},
		{
			name:  "eval 成功但 value 为空",
			err:   errEvalEmpty,
			found: false,
			want:  "eval_empty",
		},
		{
			name:    "异常带当前 URL",
			err:     errors.New("cdp: context destroyed"),
			found:   false,
			pageURL: "https://myread.top/store",
			want:    "eval_error:cdp: context destroyed|url=https://myread.top/store",
		},
		{
			// 防御性兜底：err=nil 且 found=true 不应走到分类，但函数也不得 panic。
			name:  "兜底",
			err:   nil,
			found: true,
			want:  "target_not_found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFindFailure(tc.err, tc.found, tc.pageURL); got != tc.want {
				t.Fatalf("classifyFindFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

// 前置（2026-08-27）：session_url 必须返回真实 CDP targetId 的页面级调试端点
// （ws://host:port/devtools/page/<targetId>），不再用 Pool 内部 ID ctx-N。
func TestWsBaseFromEndpoint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ws://127.0.0.1:9222/devtools/browser/abc", "ws://127.0.0.1:9222"},
		{"ws://127.0.0.1:9222", "ws://127.0.0.1:9222"},
		{"wss://host:9333/devtools/browser/x", "wss://host:9333"},
		{"http://127.0.0.1:9222", "ws://127.0.0.1:9222"},
		{"https://127.0.0.1:9222/", "wss://127.0.0.1:9222"},
		{"garbage", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := wsBaseFromEndpoint(tc.in); got != tc.want {
			t.Fatalf("wsBaseFromEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── 真实 Chrome 集成测试（无 Chrome / 沙箱阻断时自动跳过） ────────────

func cdpTestPage(t *testing.T) (string, string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <button id="login" onclick="window.__clicked=true">登录</button>
  <input id="user" placeholder="用户名">
  <a href="/next" id="reg">注册</a>
  <button id="spa" onclick="history.pushState({}, '', '/next')">跳转</button>
  <script>window.__clicked = false;</script>
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, srv.URL + "/", srv.URL + "/next"
}

func cdpOverlayPage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <button id="hidden" style="position:relative;z-index:1">被遮按钮</button>
  <div style="position:fixed;top:0;left:0;width:100%;height:100%;z-index:9999;background:#fff"></div>
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func cdpNewWindowPage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
  <button id="login" onclick="window.__clicked=true">登录</button>
  <a href="/next" id="newtab" target="_blank">新标签</a>
  <script>window.__clicked = false;</script>
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func cdpScrollPage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
<div style="height:2400px"></div>
<button id="far" onclick="window.__clicked=true">底部按钮</button>
<script>window.__clicked = false;</script>
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func cdpSliderPage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html>
<html><body>
<input type="range" id="slider" min="0" max="100" value="50"
  style="position:absolute;left:100px;top:192px;width:200px;height:16px">
<script>
  window.__dragValue = 0;
  document.getElementById('slider').addEventListener('mouseup', function () {
    window.__dragValue = this.value;
  });
</script>
</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newTestCDPSession(t *testing.T, cfg Config) *cdpSession {
	t.Helper()
	requireCDP(t)
	f := &CDPFactory{Headless: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := f.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("create cdp session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess.(*cdpSession)
}
