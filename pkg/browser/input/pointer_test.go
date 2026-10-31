package input

import (
	"context"
	"math"
	"math/rand"
	"testing"
	"time"
)

// 单元 2b（2026-09-29）：落点采样与静默漂移的纯函数、以及会话级 `Pointer` 组件的
// 闸门，从宿主 `pkg/browser/pointer_humanize_test.go` 迁入。宿主只留「轨迹数学」
// （贝塞尔 + 过冲回拉，在 `[]pt` 热路径上）与一套接线集成用例。

// TestClickLandingPointStaysInsideTheElementAndOffCenter 覆盖落点的两条硬约束：
// 都在「矩形减边距」内，且永不落在正中（那是「完美机器」特征）。
func TestClickLandingPointStaysInsideTheElementAndOffCenter(t *testing.T) {
	// 400x120 的典型按钮/链接。
	target := ElementTarget{Found: true, X: 200, Y: 60, W: 400, H: 120}
	rng := rand.New(rand.NewSource(7))
	marginX := math.Min(target.W*PointerMarginRatio, PointerMarginMax)
	marginY := math.Min(target.H*PointerMarginRatio, PointerMarginMax)
	var sumAbsDX, sumAbsDY float64
	for i := 0; i < 2000; i++ {
		p := ClickLandingPoint(target, rng)
		dx, dy := p.X-target.X, p.Y-target.Y
		if math.Abs(dx) > target.W/2-marginX+1e-9 || math.Abs(dy) > target.H/2-marginY+1e-9 {
			t.Fatalf("落点越出「矩形减边距」: dx=%.3f dy=%.3f (%+v)", dx, dy, p)
		}
		if p.X <= 0 || p.X >= target.W || p.Y <= 0 || p.Y >= target.H {
			t.Fatalf("落点越出元素矩形: %+v", p)
		}
		if math.Hypot(dx, dy) < 0.1 {
			t.Fatalf("落点落在正中（必须避开）: %+v", p)
		}
		sumAbsDX += math.Abs(dx)
		sumAbsDY += math.Abs(dy)
	}
	// 分布不能塌缩在中心附近：平均偏移要明显大于 1px。
	if avg := sumAbsDX / 2000; avg < 5 {
		t.Fatalf("横向平均偏移 %.2fpx 太小，落点几乎等于正中", avg)
	}
	if avg := sumAbsDY / 2000; avg < 2 {
		t.Fatalf("纵向平均偏移 %.2fpx 太小，落点几乎等于正中", avg)
	}
}

// TestClickLandingPointIsDeterministicAndDegenerateSafe 同种子输出相同；拿不到几何
// （无头/元素过小）或 rng 为空时退回中心——宁可按老办法点，也不能点空。
func TestClickLandingPointIsDeterministicAndDegenerateSafe(t *testing.T) {
	target := ElementTarget{Found: true, X: 10, Y: 20, W: 80, H: 40}
	a := ClickLandingPoint(target, rand.New(rand.NewSource(42)))
	b := ClickLandingPoint(target, rand.New(rand.NewSource(42)))
	if a != b {
		t.Fatalf("同种子应输出相同落点: %+v vs %+v", a, b)
	}
	for _, degenerate := range []ElementTarget{
		{Found: true, X: 5, Y: 6},
		{Found: true, X: 5, Y: 6, W: 1, H: 40},
		{Found: true, X: 5, Y: 6, W: 80, H: 1},
	} {
		if got := ClickLandingPoint(degenerate, rand.New(rand.NewSource(1))); got != (Point{X: degenerate.X, Y: degenerate.Y}) {
			t.Fatalf("退化几何应退回中心: got %+v want %+v", got, degenerate)
		}
	}
	if got := ClickLandingPoint(target, nil); got != (Point{X: target.X, Y: target.Y}) {
		t.Fatalf("rng 为空应退回中心: %+v", got)
	}
}

// TestIdleDriftPathIsSmallAndProbabilistic 一次漂移 1~3 步、单步不超上限；rng 为空不漂移。
func TestIdleDriftPathIsSmallAndProbabilistic(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	cur := Point{X: 50, Y: 50}
	moves := 0
	for i := 0; i < 2000; i++ {
		steps := IdleDriftPath(cur, rng)
		if len(steps) == 0 {
			continue
		}
		moves++
		if len(steps) < 1 || len(steps) > 3 {
			t.Fatalf("一次漂移应是 1~3 步: %d", len(steps))
		}
		prev := cur
		for _, p := range steps {
			if step := math.Hypot(p.X-prev.X, p.Y-prev.Y); step > PointerDriftStepMax+1e-9 {
				t.Fatalf("单步漂移过大: %.3f px", step)
			}
			prev = p
		}
	}
	if moves == 0 {
		t.Fatal("确定性 rng 应至少给出一次漂移")
	}
	if got := IdleDriftPath(cur, nil); got != nil {
		t.Fatalf("rng 为空不应漂移: %+v", got)
	}
}

// TestPointerClickTargetUsesTheSessionRNGAndStaysInTheElement 会话级落点 rng 共享且
// 散得开（正中永远不取）。
func TestPointerClickTargetUsesTheSessionRNGAndStaysInTheElement(t *testing.T) {
	p := NewPointer(nil, nil, DefaultDriftTuning())
	target := ElementTarget{Found: true, X: 300, Y: 150, W: 120, H: 60}
	seen := map[Point]bool{}
	for i := 0; i < 40; i++ {
		got := p.ClickTarget(target)
		seen[got] = true
		if math.Abs(got.X-target.X) > 60 || math.Abs(got.Y-target.Y) > 30 {
			t.Fatalf("落点越出元素: %+v", got)
		}
		if got == (Point{X: target.X, Y: target.Y}) {
			t.Fatalf("落点不应等于正中: %+v", got)
		}
	}
	if len(seen) < 20 {
		t.Fatalf("落点应在元素内散开: 40 次只有 %d 个不同点", len(seen))
	}
}

// TestPointerDriftIfIdleGate 漂移闸门：没派发过不漂、刚派发过不漂、静默够久且概率
// 命中才漂（只挪 PointerDriftStepMax 以内）。漂移节奏来自注入的 DriftTuning，
// 组件自己不读全局。
func TestPointerDriftIfIdleGate(t *testing.T) {
	var got []Point
	p := NewPointer(nil, func(_ context.Context, q Point) error {
		got = append(got, q)
		return nil
	}, DriftTuning{IdleMin: 50 * time.Millisecond, Interval: time.Millisecond, Probability: 1.0})

	// 尚未派发过任何指针位置：无从漂起。
	p.DriftIfIdle(rand.New(rand.NewSource(1)))
	if len(got) != 0 {
		t.Fatalf("未初始化不应漂移: %+v", got)
	}

	// 刚派发过：静默期不足。
	p.NoteDispatch(Point{X: 10, Y: 10}, time.Now())
	p.DriftIfIdle(rand.New(rand.NewSource(1)))
	if len(got) != 0 {
		t.Fatalf("非静默期不应漂移: %+v", got)
	}

	// 静默够久 + 概率=1：必然漂移，且每步都在上限内。
	p.NoteDispatch(Point{X: 10, Y: 10}, time.Now().Add(-time.Second))
	p.DriftIfIdle(rand.New(rand.NewSource(1)))
	if len(got) == 0 {
		t.Fatal("静默期应漂移一次")
	}
	prev := Point{X: 10, Y: 10}
	for _, q := range got {
		if step := math.Hypot(q.X-prev.X, q.Y-prev.Y); step > PointerDriftStepMax+1e-9 {
			t.Fatalf("单步漂移过大: %.3f px", step)
		}
		prev = q
	}

	// rng 为空不漂移。
	before := len(got)
	p.NoteDispatch(Point{X: 10, Y: 10}, time.Now().Add(-time.Second))
	p.DriftIfIdle(nil)
	if len(got) != before {
		t.Fatalf("rng 为空不应漂移: %+v", got[before:])
	}
}

// TestPointerDriftStopsWhenSessionClosed 会话关闭后（closedCh 已关）不再漂移——
// 这是漂移 goroutine 的退出条件，也是「关了还动」这类幽灵行为的护栏。
func TestPointerDriftStopsWhenSessionClosed(t *testing.T) {
	closedCh := make(chan struct{})
	close(closedCh)
	dispatched := 0
	p := NewPointer(closedCh, func(context.Context, Point) error {
		dispatched++
		return nil
	}, DriftTuning{IdleMin: 0, Interval: time.Millisecond, Probability: 1.0})
	p.NoteDispatch(Point{X: 1, Y: 1}, time.Now().Add(-time.Minute))
	p.DriftIfIdle(rand.New(rand.NewSource(2)))
	if dispatched != 0 {
		t.Fatalf("会话已关闭不应漂移: %d 次", dispatched)
	}
	// StartDrift 对已关闭的会话直接不起 goroutine（也无 closedCh 时不起）。
	p.StartDrift()
}

// TestDefaultDriftTuningMatchesExportedDefaults 默认节奏与导出常量同源（宿主默认值
// 取自本函数，两处默认值不允许漂移）。
func TestDefaultDriftTuningMatchesExportedDefaults(t *testing.T) {
	d := DefaultDriftTuning()
	if d.IdleMin != DefaultDriftIdleMin || d.Interval != DefaultDriftInterval ||
		d.Probability != DefaultDriftProbability {
		t.Fatalf("默认节奏与导出常量不一致: %+v", d)
	}
}
