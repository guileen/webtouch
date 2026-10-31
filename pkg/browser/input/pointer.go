// 指针拟人化（2026-09-22 用户现场反馈批次 #6，P2）
//
// 现状：语义点击的落点是元素包围盒**正中心**，物理移动虽然已经是 Fitts 贝塞尔
// 轨迹，但落点固定在正中、刹车精确、静默期指针一动不动——这三点在页面事件里
// 都是很硬的机器特征。本文件负责其中两件页面内策略：
//
//   - ClickLandingPoint：落点改为元素内**随机采样**（高斯偏移 + 边距，永不正中）；
//   - IdleDriftPath + Pointer：静默期按概率插入 1~3 次小幅漂移。
//
// 轨迹本身的「轻微过冲 + 回拉」与逐步抖动仍在宿主（贝塞尔轨迹在 `[]pt` 上循环，
// 见 `pkg/browser/cdp_js_helpers.go` 的 `overshootPath`/`pointerStepDelay`）。
//
// 边界（R1/R2）：全部是**页面内**的 `Input.dispatchMouseEvent`，不动系统指针、
// 不抢前台、不向 DOM 注入任何元素；漂移只发 mouseMoved（不按下），不影响页面
// 语义。纯函数部分 rng 固定即输出确定，便于单测。
//
// 复杂度治理·阶段 2·单元 2b（2026-09-29）：本文件是宿主 `pkg/browser/pointer_humanize.go`
// 的策略半边（另一边的轨迹数学留在宿主）；会话级 rng 与指针状态（原宿主字段
// `clickRand`/`lastPointer`/`pointerInit`/`lastInputAt`）收进 `Pointer` 组件，
// 宿主经 `pointerHumanizer()` 访问。
package input

import (
	"context"
	"math"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// PointerMarginRatio 是落点距元素边缘的最小相对边距（防贴边/命中边框）。
	PointerMarginRatio = 0.18
	// PointerMarginMax 是边距的绝对上限（像素）：大元素不必让出太多。
	PointerMarginMax = 12.0
	// pointerSigmaRatio 是高斯偏移相对「可用半径」的标准差比例：0.45 时
	// 约 68% 的落点落在可用半径的一半以内，仍会明显偏离正中。
	pointerSigmaRatio = 0.45
	// pointerCenterEpsilon 是「离正中太近」的判定阈值（像素）。
	pointerCenterEpsilon = 0.5
	// PointerDriftStepMax 是单步漂移的最大位移（像素）。
	PointerDriftStepMax = 1.5

	// DefaultDriftIdleMin 是「多久没有真输入才算静默」的默认值。
	DefaultDriftIdleMin = 3 * time.Second
	// DefaultDriftInterval 是静默期检查间隔的默认值。
	DefaultDriftInterval = 1500 * time.Millisecond
	// DefaultDriftProbability 是一个静默检查周期内插入一次漂移的默认概率。
	DefaultDriftProbability = 0.35
)

// Point 是页面视口 CSS 坐标（宿主 `pt` 的同形类型；跨界处显式转换）。
type Point struct{ X, Y float64 }

// DriftTuning 是静默漂移的节奏参数：宿主在建会话时把可调值（含取证/验收下调的
// 测试节奏）注入组件，组件本身不再读全局。
type DriftTuning struct {
	IdleMin     time.Duration
	Interval    time.Duration
	Probability float64
}

// DefaultDriftTuning 返回出厂节奏（宿主默认值即取此函数，避免两处默认值）。
func DefaultDriftTuning() DriftTuning {
	return DriftTuning{
		IdleMin:     DefaultDriftIdleMin,
		Interval:    DefaultDriftInterval,
		Probability: DefaultDriftProbability,
	}
}

// PointerDriftEnabled 读 `UWS_POINTER_DRIFT`（默认开）：=0 关掉静默期漂移，
// 只留落点随机与过冲回拉。
func PointerDriftEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("UWS_POINTER_DRIFT"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// ClickLandingPoint 在元素矩形内随机采样一个落点（纯函数）。
// 规则：先按 18%（上限 12px）的边距收窄矩形，再在收窄后的矩形内取高斯偏移；
// 偏移过小（离正中 <0.5px）时沿随机方向推开，保证「永不落在正中」。
// 拿不到几何（宽或高 <2px）时退回落点中心——宁可按老办法点，也不能点空。
func ClickLandingPoint(t ElementTarget, rng *rand.Rand) Point {
	center := Point{X: t.X, Y: t.Y}
	if rng == nil || t.W < 2 || t.H < 2 {
		return center
	}
	mx := math.Min(t.W*PointerMarginRatio, PointerMarginMax)
	my := math.Min(t.H*PointerMarginRatio, PointerMarginMax)
	hx := math.Max(t.W/2-mx, 0.5)
	hy := math.Max(t.H/2-my, 0.5)
	dx := clampPointerOffset(rng.NormFloat64()*hx*pointerSigmaRatio, hx)
	dy := clampPointerOffset(rng.NormFloat64()*hy*pointerSigmaRatio, hy)
	if math.Abs(dx) < pointerCenterEpsilon && math.Abs(dy) < pointerCenterEpsilon {
		angle := rng.Float64() * 2 * math.Pi
		dx = math.Cos(angle) * math.Min(hx, 1)
		dy = math.Sin(angle) * math.Min(hy, 1)
	}
	return Point{X: center.X + dx, Y: center.Y + dy}
}

// clampPointerOffset 把偏移夹到 [-limit, limit]。
func clampPointerOffset(v, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	if v > limit {
		return limit
	}
	if v < -limit {
		return -limit
	}
	return v
}

// IdleDriftPath 生成静默期的小幅漂移（纯函数）：返回 1~3 个微移动点，每个点距
// 前一点不超过 PointerDriftStepMax（1.5px）。rng 为空返回 nil。
// 「这个周期要不要漂」的概率判定在调用方（Pointer.DriftIfIdle）。
func IdleDriftPath(cur Point, rng *rand.Rand) []Point {
	if rng == nil {
		return nil
	}
	n := 1 + rng.Intn(3)
	out := make([]Point, 0, n)
	p := cur
	for i := 0; i < n; i++ {
		angle := rng.Float64() * 2 * math.Pi
		step := 0.4 + rng.Float64()*(PointerDriftStepMax-0.4)
		p = Point{X: p.X + math.Cos(angle)*step, Y: p.Y + math.Sin(angle)*step}
		out = append(out, p)
	}
	return out
}

// Pointer 是会话级指针拟人化状态：点击落点 rng ＋ 静默漂移调度。
//
// 它替宿主持有四个原字段：`clickRand`（落点采样 rng，mu 保护）、`lastPointer`/
// `pointerInit`（最近一次**派发**的指针位置）、`lastInputAt`（最近一次真输入时刻，
// 静默漂移以它为判据）。
type Pointer struct {
	closedCh <-chan struct{}
	dispatch func(ctx context.Context, p Point) error
	tuning   DriftTuning

	mu     sync.Mutex
	rng    *rand.Rand
	last   Point
	inited bool
	lastAt time.Time
}

// NewPointer 建一个会话级指针组件。closedCh 为 nil 表示该会话不参与生命周期
// （字面量会话/测试）；dispatch 由宿主提供，负责把一次漂移真正发到页面。
func NewPointer(closedCh <-chan struct{}, dispatch func(ctx context.Context, p Point) error, tuning DriftTuning) *Pointer {
	return &Pointer{closedCh: closedCh, dispatch: dispatch, tuning: tuning}
}

// ClickTarget 取本会话的点击落点（元素内随机采样）。
// rng 在会话内共享，所以这里连着锁一起取，避免并发点击/漂移同时用同一个 rng。
func (p *Pointer) ClickTarget(t ElementTarget) Point {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rng == nil {
		p.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return ClickLandingPoint(t, p.rng)
}

// NoteDispatch 记录一次已成功的指针派发（真输入与漂移都算），供静默判定使用。
func (p *Pointer) NoteDispatch(at Point, when time.Time) {
	p.mu.Lock()
	p.last = at
	p.inited = true
	p.lastAt = when
	p.mu.Unlock()
}

// StartDrift 起会话级 ticker：静默（≥IdleMin 没有真输入）时按概率把指针小幅挪一下。
// 只发 mouseMoved，不影响页面语义；同时会喂给观看小窗的指针观察者，屏幕上看起来就是
// 「指针自己轻轻动了一下」。会话关闭即退出。只在工厂建会话时调用（测试里的字面量
// 会话不会起这个 goroutine）。
func (p *Pointer) StartDrift() {
	if !PointerDriftEnabled() || p.closedCh == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(p.tuning.Interval)
		defer ticker.Stop()
		rng := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(os.Getpid())<<20))
		for {
			select {
			case <-p.closedCh:
				return
			case <-ticker.C:
				p.DriftIfIdle(rng)
			}
		}
	}()
}

// DriftIfIdle 真输入的静默期已够久时，按概率派发一次小幅漂移。
func (p *Pointer) DriftIfIdle(rng *rand.Rand) {
	p.mu.Lock()
	if p.isClosed() || !p.inited || time.Since(p.lastAt) < p.tuning.IdleMin {
		p.mu.Unlock()
		return
	}
	cur := p.last
	p.mu.Unlock()
	if rng == nil || rng.Float64() > p.tuning.Probability {
		return
	}
	path := IdleDriftPath(cur, rng)
	if len(path) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, pt := range path {
		if err := p.dispatch(ctx, pt); err != nil {
			return
		}
	}
}

// isClosed 报告会话是否已关闭（closedCh 为 nil 表示不参与生命周期）。
func (p *Pointer) isClosed() bool {
	if p.closedCh == nil {
		return false
	}
	select {
	case <-p.closedCh:
		return true
	default:
		return false
	}
}
