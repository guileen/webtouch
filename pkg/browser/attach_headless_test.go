package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TDD-ENG-25：附着对象的形态必须按**它自己**的 UA 判定。
//
// 现场批次 #2（2026-09-22）：引擎启动默认 Headless=true，附着模式只在检测到无头时
// 升级、从不回落，于是附着到真 Chrome 的会话被当成无头 —— windowPolicy() 退化成 tab
// （标签塞进别人的窗口）、ShowViewerWindow 报 ErrNoViewerWindow，用户点小窗只能看到
// 一张点不动、滚不动的图。这里锁住三态：真 Chrome → 有头；HeadlessChrome → 无头；
// 读不到 → 不表态（known=false，调用方保留默认）。
func TestAttachedBrowserShapeComesFromItsOwnUserAgent(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		headless bool
		known    bool
	}{
		{
			name:     "真 Chrome 是有头的",
			status:   http.StatusOK,
			body:     `{"Browser":"Chrome/153.0.8010.53","User-Agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"}`,
			headless: false,
			known:    true,
		},
		{
			name:     "HeadlessChrome 是无头的",
			status:   http.StatusOK,
			body:     `{"Browser":"Chrome/153.0.8010.53","User-Agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/153.0.0.0 Safari/537.36"}`,
			headless: true,
			known:    true,
		},
		{
			name:   "UA 为空不表态",
			status: http.StatusOK,
			body:   `{"Browser":"Chrome/153.0.8010.53"}`,
			known:  false,
		},
		{
			name:   "端点不是 HTTP /version 时不表态",
			status: http.StatusNotFound,
			body:   `not found`,
			known:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/json/version" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			headless, known := browserHeadlessState(context.Background(), srv.URL)
			if known != tc.known {
				t.Fatalf("known = %v，想要 %v（读不到时必须不表态，让调用方保留默认）", known, tc.known)
			}
			if known && headless != tc.headless {
				t.Fatalf("headless = %v，想要 %v", headless, tc.headless)
			}
		})
	}
}

// 端点连不上（附着被中断/端口已关）同样不表态：不能凭「读不到」把会话判成有头。
func TestAttachedBrowserShapeIsUnknownWhenTheEndpointIsGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	if _, known := browserHeadlessState(context.Background(), url); known {
		t.Fatalf("端点已关闭时必须 known=false")
	}
}
