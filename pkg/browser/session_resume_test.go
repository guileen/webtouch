package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
	"uws/pkg/browser/internal/wstest"
)

// 会话续页（v1.5.7，GLOBAL #118）：引擎把「会话名 → Target」落盘，重启失忆后
// 用 ReuseTargetID 让本会话接管原来那扇窗口，而不是再开一扇新的。
//
// 这里钉住的是判定面：目标存在才接管、空壳/不存在一律回退新开（回退路径由
// initTarget 负责，见 TestInitTargetFallsBackWhenReuseTargetMissing）。

// fakeCDPResumeServer 是「能列出 target」的最小 CDP 服务端：getTargets 回显给定
// 列表，attachToTarget 可被脚本化成失败，其余命令一律成功。
func fakeCDPResumeServer(t *testing.T, infos []map[string]any, attachErr bool) string {
	t.Helper()
	return wstest.Start(t, func(conn net.Conn, br *bufio.Reader) {
		for {
			op, payload, err := wstest.ReadServerFrame(br)
			if err != nil {
				return
			}
			if op != wstest.OpText {
				continue
			}
			var msg map[string]any
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			method, _ := msg["method"].(string)
			id, _ := msg["id"].(float64)
			var resp map[string]any
			switch {
			case method == "Target.getTargets":
				resp = map[string]any{"id": int64(id), "result": map[string]any{"targetInfos": infos}}
			case method == "Target.attachToTarget" && attachErr:
				resp = map[string]any{"id": int64(id), "error": map[string]any{"code": -32000, "message": "No target with given id"}}
			case method == "Target.attachToTarget":
				resp = map[string]any{"id": int64(id), "result": map[string]any{"sessionId": "sess-resume"}}
			case method == "Target.createTarget":
				resp = map[string]any{"id": int64(id), "result": map[string]any{"targetId": "fresh-target"}}
			default:
				resp = map[string]any{"id": int64(id), "result": map[string]any{}}
			}
			b, _ := json.Marshal(resp)
			_ = wstest.WriteServerText(conn, b)
		}
	})
}

func resumeTestSession(t *testing.T, url string, cfg Config) *cdpSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := newCDPClient(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &cdpSession{
		id:       "cdp-resume-test",
		cfg:      cfg,
		client:   c,
		closedCh: make(chan struct{}),
		inflight: make(map[string]time.Time),
	}
}

func TestAdoptTargetRejectsMissingAndBlankTargets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 目标已经不在浏览器里（用户关了窗 / Chrome 重启过）：必须报错让调用方回退新开。
	url := fakeCDPResumeServer(t, []map[string]any{
		{"targetId": "other-target", "type": "page", "url": "https://example.com/"},
	}, false)
	s := resumeTestSession(t, url, Config{})
	if err := s.adoptTarget(ctx, "gone-target"); err == nil {
		t.Fatal("目标不存在时必须返回错误（调用方据此回退为新开窗口）")
	} else if !strings.Contains(err.Error(), "已不存在") {
		t.Fatalf("错误信息应说明目标已不存在，实际: %v", err)
	}

	// 目标在、但没有页面内容（Chrome 自己造的空壳）：同样不接管。
	url2 := fakeCDPResumeServer(t, []map[string]any{
		{"targetId": "blank-target", "type": "page", "url": ""},
	}, false)
	s2 := resumeTestSession(t, url2, Config{})
	if err := s2.adoptTarget(ctx, "blank-target"); err == nil {
		t.Fatal("空壳页面不得被接管")
	} else if !strings.Contains(err.Error(), "没有页面内容") {
		t.Fatalf("错误信息应说明没有页面内容，实际: %v", err)
	}
}

func TestAdoptTargetAcceptsExistingPage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := fakeCDPResumeServer(t, []map[string]any{
		{"targetId": "keep-me", "type": "page", "url": "https://www.autodl.com/console/instance/list"},
	}, false)
	s := resumeTestSession(t, url, Config{ReuseTargetID: "keep-me"})
	if err := s.adoptTarget(ctx, "keep-me"); err != nil {
		t.Fatalf("已存在的页面应可接管: %v", err)
	}
}

func TestInitTargetAdoptsReuseTargetInsteadOfOpeningWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := fakeCDPResumeServer(t, []map[string]any{
		{"targetId": "keep-me", "type": "page", "url": "https://www.autodl.com/home"},
	}, false)
	// 续页接管来的窗口按原策略记回 ownWindow，否则后续「展开/停靠」会把它当普通标签。
	s := resumeTestSession(t, url, Config{ReuseTargetID: "keep-me", ReuseOwnWindow: true})
	if err := s.initTarget(ctx); err != nil {
		t.Fatalf("initTarget: %v", err)
	}
	if s.targetID != "keep-me" {
		t.Fatalf("应接管原 target，实际 %q（说明又开了新窗口）", s.targetID)
	}
	if !s.adopted {
		t.Fatal("接管路径必须标记 adopted（用于跳过重新停靠）")
	}
	if !s.ownWindow {
		t.Fatal("续页接管来的专属窗口必须按原策略记回 ownWindow")
	}
}

func TestInitTargetFallsBackWhenReuseTargetMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 落盘绑定指向的目标已经没了：必须静默回退为新开，不能让整个任务失败。
	url := fakeCDPResumeServer(t, []map[string]any{
		{"targetId": "some-other", "type": "page", "url": "https://example.com/"},
	}, false)
	s := resumeTestSession(t, url, Config{ReuseTargetID: "gone-target", ReuseOwnWindow: true})
	if err := s.initTarget(ctx); err != nil {
		t.Fatalf("目标失效时必须回退新开而不是报错: %v", err)
	}
	if s.targetID != "fresh-target" || s.adopted {
		t.Fatalf("应回退为新开 target，实际 targetID=%q adopted=%v", s.targetID, s.adopted)
	}
}

// TestInitTargetRequireReuseFallsBackOnlyWithInitialURL 钉住 2026-10-03 的例外口径：
// 空闲回收把页签关掉后持久绑定即失效（chat 502 复发的根因）。默认仍失败关闭；只有
// 调用方显式给了 initial_url（愿意从目标页重新开始）时才允许回退新开。
func TestInitTargetRequireReuseFallsBackOnlyWithInitialURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 显式给了 initial_url：回退新开，任务不该因为死绑定整条 502。
	urlWithURL := fakeCDPResumeServer(t, []map[string]any{
		{"targetId": "some-other", "type": "page", "url": "https://example.com/"},
	}, false)
	withURL := resumeTestSession(t, urlWithURL, Config{
		ReuseTargetID:      "gone-target",
		RequireReuseTarget: true,
		InitialURL:         "https://chat.deepseek.com/",
	})
	if err := withURL.initTarget(ctx); err != nil {
		t.Fatalf("给了 initial_url 时应回退新开而不是失败: %v", err)
	}
	if withURL.targetID != "fresh-target" || withURL.adopted {
		t.Fatalf("应回退为新开 target，实际 targetID=%q adopted=%v", withURL.targetID, withURL.adopted)
	}

	// 没给 initial_url：行为不变，必须失败关闭（不得悄悄落到别的标签上）。
	urlWithoutURL := fakeCDPResumeServer(t, []map[string]any{
		{"targetId": "some-other", "type": "page", "url": "https://example.com/"},
	}, false)
	withoutURL := resumeTestSession(t, urlWithoutURL, Config{
		ReuseTargetID:      "gone-target",
		RequireReuseTarget: true,
	})
	if err := withoutURL.initTarget(ctx); err == nil {
		t.Fatal("无 initial_url 时必须维持失败关闭")
	} else if !strings.Contains(err.Error(), "refusing a different target") {
		t.Fatalf("应给出失败关闭的明确错误，实际: %v", err)
	}
}
