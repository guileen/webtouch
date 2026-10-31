package browser

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"
	"uws/pkg/browser/input"
)

// 批次 #6（指针拟人化）在宿主侧只剩两类断言：
//   - 轨迹数学：贝塞尔 + 末尾「过冲再回拉」——它在 `[]pt` 热路径上循环，故意留在宿主；
//   - 接线集成：会话级落点 rng、静默漂移闸门与事件标记。
// 落点采样/漂移的纯函数与组件闸门已随实现迁入 `pkg/browser/input` 的用例。

func TestOvershootPathOvershootsThenReturnsToTheTarget(t *testing.T) {
	start, end := pt{x: 100, y: 100}, pt{x: 300, y: 200}
	rng := rand.New(rand.NewSource(3))
	base := newBezierPath(start, end, 20, rng)
	full := overshootPath(base, end, rng)
	if len(full) != len(base)+3 {
		t.Fatalf("过冲应追加三个点（过冲峰 / 回拉 / 落点）: base=%d full=%d", len(base), len(full))
	}
	if full[len(full)-1] != end {
		t.Fatalf("轨迹必须精确收在落点: %+v", full[len(full)-1])
	}
	peak := full[len(base)]
	off := math.Hypot(peak.x-end.x, peak.y-end.y)
	if off <= 0 || off > pointerOvershootMax+1e-9 {
		t.Fatalf("过冲幅度应在 (0, %.1f] 内: %.3f", pointerOvershootMax, off)
	}
	// 过冲方向 = 接近方向的延长线（越过落点，而不是拐向别处）。
	prev := base[len(base)-2]
	dirX, dirY := end.x-prev.x, end.y-prev.y
	d := math.Hypot(dirX, dirY)
	if dot := (peak.x-end.x)*(dirX/d) + (peak.y-end.y)*(dirY/d); dot <= 0 {
		t.Fatalf("过冲方向应与接近方向一致: dot=%.3f", dot)
	}
	// 轨迹太短（<8px）没有过冲的意义，原样返回。
	short := []pt{{x: 0, y: 0}, {x: 3, y: 0}}
	if got := overshootPath(short, pt{x: 3, y: 0}, rng); len(got) != len(short) {
		t.Fatalf("短轨迹不应追加过冲点: %d", len(got))
	}
}

// TestHumanizedClickTargetUsesTheSessionRNGAndStaysInTheElement 走宿主的组件访问器：
// 会话级 rng 共享、落点散开且永不正中。
func TestHumanizedClickTargetUsesTheSessionRNGAndStaysInTheElement(t *testing.T) {
	s := &cdpSession{}
	target := elementTarget{Found: true, X: 300, Y: 150, W: 120, H: 60}
	seen := map[input.Point]bool{}
	for i := 0; i < 40; i++ {
		got := s.pointerHumanizer().ClickTarget(target)
		seen[got] = true
		if math.Abs(got.X-target.X) > 60 || math.Abs(got.Y-target.Y) > 30 {
			t.Fatalf("落点越出元素: %+v", got)
		}
		if got == (input.Point{X: target.X, Y: target.Y}) {
			t.Fatalf("落点不应等于正中: %+v", got)
		}
	}
	if len(seen) < 20 {
		t.Fatalf("落点应在元素内散开: 40 次只有 %d 个不同点", len(seen))
	}
}

// pointerEventRecorder 收集 PointerObserver 收到的事件，用于断言「这一跳是不是
// 被标记成静默漂移」。
type pointerEventRecorder struct {
	mu     sync.Mutex
	events []ViewerPointer
}

func (r *pointerEventRecorder) ObservePointer(p ViewerPointer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, p)
}

func (r *pointerEventRecorder) snapshot() []ViewerPointer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ViewerPointer(nil), r.events...)
}

// TestPointerDriftOnlyFiresWhenIdle 覆盖漂移的开关条件：静默够久才发 mouseMoved，
// 刚发生过真输入时一个字节都不发（否则会干扰进行中的动作）。
// 判据「最近一次派发时刻」由子包组件持有，测试用 NoteDispatch 把它回拨。
func TestPointerDriftOnlyFiresWhenIdle(t *testing.T) {
	fake := &inputFakeCDP{}
	s := newInputTestSession(t, fake.serve(t))
	recorder := &pointerEventRecorder{}
	s.SetPointerObserver(recorder)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.dispatchMouseWithButtons(ctx, "mousePressed", pt{x: 10, y: 20}, "left", 1, 0); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// 真输入不算漂移：观看小窗据此才会重置面板的静默时钟。
	if got := recorder.snapshot(); len(got) != 1 || got[0].Idle {
		t.Fatalf("真输入不应带 idle 标记: %+v", got)
	}
	before := fake.count("Input.dispatchMouseEvent")

	pointer := s.pointerHumanizer()
	// 刚输入过：不漂移。
	pointer.DriftIfIdle(rand.New(rand.NewSource(5)))
	if got := fake.count("Input.dispatchMouseEvent"); got != before {
		t.Fatalf("非静默期不应漂移: %d → %d", before, got)
	}

	// 静默够久：按概率漂移，且只发 mouseMoved（不按下）。
	pointer.NoteDispatch(input.Point{X: 10, Y: 20}, time.Now().Add(-time.Minute))
	drifted := false
	for i := 0; i < 200 && !drifted; i++ {
		pointer.DriftIfIdle(rand.New(rand.NewSource(int64(i + 1))))
		if fake.count("Input.dispatchMouseEvent") > before {
			drifted = true
		}
	}
	if !drifted {
		t.Fatal("静默期应至少漂移一次")
	}
	moves := fake.paramsFor("Input.dispatchMouseEvent")
	last := moves[len(moves)-1]
	if last["type"] != "mouseMoved" {
		t.Fatalf("漂移只应发 mouseMoved: %v", last["type"])
	}
	if dx := math.Abs(last["x"].(float64) - 10); dx > input.PointerDriftStepMax*3 {
		t.Fatalf("漂移幅度过大: dx=%.3f", dx)
	}

	// 漂移要带 idle 标记：光标照画，但不算 Agent 活动（否则面板永远收不掉）。
	driftEvents := 0
	for _, ev := range recorder.snapshot()[1:] {
		if ev.Kind != ViewerPointerMoved {
			t.Fatalf("漂移只应是 mouseMoved: %+v", ev)
		}
		if !ev.Idle {
			t.Fatalf("漂移必须带 idle 标记，否则会顶住面板的静默时钟: %+v", ev)
		}
		driftEvents++
	}
	if driftEvents == 0 {
		t.Fatal("静默期应至少产生一次带 idle 标记的漂移事件")
	}

	// 会话关闭后不再漂移（closedCh 是漂移 goroutine 的退出条件）。
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		t.Fatal("Close 后应标记 closed")
	}
}
