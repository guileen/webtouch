package browser

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
	"uws/pkg/browser/input"
)

// TDD-ENG-27（真机）：指针拟人化的现场证据（2026-09-22 用户现场反馈批次 #6）。
//
// 门禁与其它真机用例相同（UWS_RUN_CHROME_E2E=1 + UWS_E2E_ATTACH_URL）。用例附着
// 真 Chrome，在自建页面上装 document 级监听，记下整条 mousemove 轨迹与 click 落点，
// 再走**被改动的语义点击路径**（`clickAt`），最后读回证据断言三件事：
//  1. 落点不在元素正中，且仍在矩形内留出边距；
//  2. 轨迹是多步曲线 + 末尾「过冲再回拉」（不再精确刹车）；
//  3. 静默期会插入小幅漂移，且只发 mouseMoved。
//
// 第 3 条要让秒级判定成立，用例在**建会话之前**把漂移周期与概率调到测试值。
func TestCDPSessionHumanizedPointerEvidence(t *testing.T) {
	if os.Getenv("UWS_RUN_CHROME_E2E") != "1" {
		t.Skip("未设置 UWS_RUN_CHROME_E2E=1，跳过会触碰真实 Chrome 的回归用例")
	}
	attachURL := os.Getenv("UWS_E2E_ATTACH_URL")
	if attachURL == "" {
		t.Skip("未设置 UWS_E2E_ATTACH_URL，跳过真机指针拟人化用例")
	}
	oldIdle, oldInterval, oldProb := pointerDriftIdleMin, pointerDriftInterval, pointerDriftProbability
	pointerDriftIdleMin, pointerDriftInterval, pointerDriftProbability = 300*time.Millisecond, 200*time.Millisecond, 1.0
	t.Cleanup(func() {
		pointerDriftIdleMin, pointerDriftInterval, pointerDriftProbability = oldIdle, oldInterval, oldProb
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, humanizedPointerPage)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := &CDPFactory{Headless: true} // 引擎现场那套配置（见 TDD-ENG-25）
	sess, err := f.Create(ctx, Config{AttachCDPURL: attachURL})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sess.Close() }()
	s := sess.(*cdpSession)
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	target, err := findElementOpt(ctx, s, "指针证据按钮", "", false)
	if err != nil || !target.Found {
		t.Fatalf("语义定位失败: %+v err=%v", target, err)
	}
	if target.W < 100 || target.H < 40 {
		t.Fatalf("用例要求一个足够大的元素（200x80），拿到 %+v", target)
	}
	if err := s.clickAt(ctx, target); err != nil {
		t.Fatalf("clickAt: %v", err)
	}
	// 静默期漂移：把周期调到 200ms 后等一会儿，必然插进来。
	time.Sleep(1200 * time.Millisecond)

	raw, err := s.evalResult(ctx, "JSON.stringify(window.__uwsPointerEvidence)")
	if err != nil {
		t.Fatalf("读回证据失败: %v", err)
	}
	var ev struct {
		Rect   [4]float64   `json:"rect"`
		Moves  [][2]float64 `json:"moves"`
		Clicks [][2]float64 `json:"clicks"`
		Mark   int          `json:"mark"` // 点击发生时已记录的 mousemove 数量
	}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("解析证据失败: %v (%s)", err, raw)
	}
	if len(ev.Clicks) != 1 {
		t.Fatalf("应恰好发生一次点击: %d（证据 %s）", len(ev.Clicks), raw)
	}
	click := ev.Clicks[0]
	left, top, w, h := ev.Rect[0], ev.Rect[1], ev.Rect[2], ev.Rect[3]
	cx, cy := left+w/2, top+h/2
	if off := math.Hypot(click[0]-cx, click[1]-cy); off < 0.3 {
		t.Fatalf("落点落在元素正中（%.3fpx），没有随机采样：click=%v rect=%v", off, click, ev.Rect)
	}
	marginX := math.Min(w*input.PointerMarginRatio, input.PointerMarginMax)
	marginY := math.Min(h*input.PointerMarginRatio, input.PointerMarginMax)
	if math.Abs(click[0]-cx) > w/2-marginX+1 || math.Abs(click[1]-cy) > h/2-marginY+1 {
		t.Fatalf("落点越出「矩形减边距」：click=%v rect=%v", click, ev.Rect)
	}
	if ev.Mark < 8 {
		t.Fatalf("真机上只收到 %d 个 mousemove，轨迹被合并或没走多步：%v", ev.Mark, ev.Moves)
	}
	// 过冲再回拉：点击之前的末尾若干个点里，最远点必须**越过**落点，且最后一点回到落点上。
	trail := ev.Moves[:ev.Mark]
	last := trail[len(trail)-1]
	if back := math.Hypot(last[0]-click[0], last[1]-click[1]); back > 1.0 {
		t.Fatalf("轨迹最后一点没有回到落点（差 %.3fpx）：last=%v click=%v", back, last, click)
	}
	// 过冲 = 轨迹里出现「沿前进方向越过落点」的采样点（离落点很近，但在前方）。
	// 方向不能用前一个采样点算（过冲点的前一点往往就是落点本身），所以回看 4 个采样点。
	overshootAt, overshootPx := -1, 0.0
	for i := 1; i < len(trail); i++ {
		base := i - 4
		if base < 0 {
			base = 0
		}
		prevPt, cur := trail[base], trail[i]
		dirX, dirY := click[0]-prevPt[0], click[1]-prevPt[1]
		n := math.Hypot(dirX, dirY)
		if n < 8 {
			continue
		}
		beyond := ((cur[0]-click[0])*dirX + (cur[1]-click[1])*dirY) / n
		near := math.Hypot(cur[0]-click[0], cur[1]-click[1])
		if beyond > 0.3 && near <= pointerOvershootMax+2 && beyond > overshootPx {
			overshootAt, overshootPx = i, beyond
		}
	}
	if overshootAt < 0 {
		t.Fatalf("轨迹里没有「越过落点再回拉」的采样点（mousemove %d 个）：%v", len(trail), trail)
	}
	// 漂移证据：点击之后的点全是静默期漂移——小幅（单步 ≤1.5px）、紧贴落点。
	drift := ev.Moves[ev.Mark:]
	if len(drift) == 0 {
		t.Fatalf("静默期没有插入漂移：%v", ev.Moves)
	}
	prevDrift := click
	for _, p := range drift {
		// Chrome 把合成鼠标坐标取整到像素，所以单步阈值留一格取整余量。
		if d := math.Hypot(p[0]-prevDrift[0], p[1]-prevDrift[1]); d > input.PointerDriftStepMax+1.2 {
			t.Fatalf("静默期漂移单步过大（%.2fpx）：%v", d, p)
		}
		if d := math.Hypot(p[0]-click[0], p[1]-click[1]); d > 8 {
			t.Fatalf("静默期漂移离落点太远（%.1fpx）：%v", d, p)
		}
		prevDrift = p
	}
	t.Logf("EVIDENCE 落点 %v（中心 %v，偏离 %.2fpx），mousemove %d 个，过冲 %.2fpx，漂移 %d 点",
		click, [2]float64{cx, cy}, math.Hypot(click[0]-cx, click[1]-cy), ev.Mark, overshootPx, len(drift))
}

// humanizedPointerPage 是证据页：按钮居中（保证落点随机后仍在视口内），
// document 级监听记录整条轨迹与点击落点。
const humanizedPointerPage = `<!doctype html><html><head><meta charset="utf-8">
<title>pointer evidence</title>
<style>html,body{margin:0;height:100%}button{position:absolute;left:50%;top:50%;
transform:translate(-50%,-50%);width:200px;height:80px;font-size:18px}</style></head>
<body><button id="btn">指针证据按钮</button>
<script>
(function () {
  var ev = {rect: [0, 0, 0, 0], moves: [], clicks: [], mark: 0};
  window.__uwsPointerEvidence = ev;
  var btn = document.getElementById('btn');
  function snapshot() {
    var r = btn.getBoundingClientRect();
    ev.rect = [r.left, r.top, r.width, r.height];
  }
  snapshot();
  document.addEventListener('mousemove', function (e) {
    if (ev.moves.length < 600) ev.moves.push([e.clientX, e.clientY]);
  });
  document.addEventListener('click', function (e) {
    snapshot();
    ev.mark = ev.moves.length;
    if (ev.clicks.length < 20) ev.clicks.push([e.clientX, e.clientY]);
  });
})();
</script></body></html>`
