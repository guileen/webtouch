package browser

import (
	"math"
	"math/rand"
	"strconv"
	"time"
	"uws/pkg/browser/modelbridge"
)

const (
	// pointerOvershootRatio / pointerOvershootMax 控制轨迹末尾过冲幅度。
	pointerOvershootRatio = 0.12
	pointerOvershootMax   = 6.0
	// pointerStepDelayMin/Max 是轨迹每步之间的间隔（毫秒）：逐步抖动 ⇒ 速度非匀速。
	pointerStepDelayMin = 4.0
	pointerStepDelayMax = 13.0
)

// quoteJS 生成 JS 字符串字面量（含引号/反斜杠转义）。
func quoteJS(s string) string { return modelbridge.QuoteJS(s) }

// boolJS 生成 JS 布尔字面量（页内脚本占位符替换用）。
func boolJS(v bool) string { return strconv.FormatBool(v) }

// floatJS 生成 JS 数字字面量（页内脚本占位符替换用；NaN/Inf 一律回落 0）。
func floatJS(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// newBezierPath 生成三次贝塞尔鼠标轨迹（Fitts 时长 + 随机垂直偏移，非直线）。
// 纯函数：rng 固定时输出确定，便于单测（TDD-ENG-03）。
func newBezierPath(start, end pt, size float64, rng *rand.Rand) []pt {
	dist := math.Hypot(end.x-start.x, end.y-start.y)
	// Fitts: T = a + b·log2(1 + D/W)；取 a=60ms, b=90ms/bit。
	duration := 60.0 + 90.0*math.Log2(1+dist/math.Max(size, 1))
	n := int(duration / 14) // 约 14ms/步
	if n < 10 {
		n = 10
	}
	if n > 80 {
		n = 80
	}
	// 方向单位向量 + 垂直偏移控制点（轨迹随机弯曲）。
	dx, dy := end.x-start.x, end.y-start.y
	lenD := math.Max(dist, 1e-6)
	ux, uy := dx/lenD, dy/lenD
	px, py := -uy, ux // 垂直方向
	jitter := 0.08 * math.Min(dist, 240)
	if jitter < 6 {
		jitter = 6
	}
	c1 := pt{start.x + ux*lenD*0.35 + px*(rng.Float64()-0.5)*2*jitter,
		start.y + uy*lenD*0.35 + py*(rng.Float64()-0.5)*2*jitter}
	c2 := pt{start.x + ux*lenD*0.7 + px*(rng.Float64()-0.5)*2*jitter,
		start.y + uy*lenD*0.7 + py*(rng.Float64()-0.5)*2*jitter}
	path := make([]pt, 0, n)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(n-1)
		mt := 1 - t
		x := mt*mt*mt*start.x + 3*mt*mt*t*c1.x + 3*mt*t*t*c2.x + t*t*t*end.x
		y := mt*mt*mt*start.y + 3*mt*mt*t*c1.y + 3*mt*t*t*c2.y + t*t*t*end.y
		path = append(path, pt{x: x, y: y})
	}
	return path
}

// pointerStepDelay 返回轨迹下一步的间隔（非匀速：每步在 4~13ms 之间抖动）。
// 这也是「多步移动」在真机上看得见的前提——同一时刻到达的移动会被 Chrome 合并，
// 页面上就只剩几跳，等于没走轨迹。
func pointerStepDelay(rng *rand.Rand) time.Duration {
	ms := pointerStepDelayMin
	if rng != nil {
		ms += rng.Float64() * (pointerStepDelayMax - pointerStepDelayMin)
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// overshootPath 在轨迹末尾追加一段「轻微过冲 + 回拉」：沿最后一段的方向多走
// 一点（不超过剩余距离的 12%、上限 6px），再回落到落点。这样按下前的最后一
// 段是「越过再回正」，而不是精确刹车。轨迹过短（<8px）时原样返回。
//
// 复杂度治理·阶段 2·单元 2b：与 newBezierPath/pointerStepDelay 同属「轨迹数学」，
// 自 pointer_humanize.go 折进本文件（落点采样与静默漂移已入 pkg/browser/input）。
func overshootPath(path []pt, end pt, rng *rand.Rand) []pt {
	if rng == nil || len(path) < 2 {
		return path
	}
	prev := path[len(path)-2]
	dx, dy := end.x-prev.x, end.y-prev.y
	d := math.Hypot(dx, dy)
	if d < 8 {
		return path
	}
	ux, uy := dx/d, dy/d
	over := math.Min(d*pointerOvershootRatio, pointerOvershootMax)
	peak := pt{x: end.x + ux*over, y: end.y + uy*over}
	back := pt{x: end.x - ux*over*0.35, y: end.y - uy*over*0.35}
	out := make([]pt, 0, len(path)+3)
	out = append(out, path...)
	return append(out, peak, back, end)
}

// ChromePath 默认探测路径（供 CLI 展示/测试）。
func ChromePath() string {
	p, _ := resolveChrome("")
	return p
}
