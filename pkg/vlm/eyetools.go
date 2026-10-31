// eyetools.go —— UWS「义眼」的图像证据原语（确定性、纯标准库，不依赖模型）。
//
// 分工：这些是 VLM 可「调用」的 UWS 内置能力——义眼不满足于裸眼看一张图就给结论，
// 而是先由 VLM（或调用方显式 --diff/--zoom）触发这些原语生成「证据」（差异图、区域统计、
// 放大图），再基于证据做判断。本质上是把「人类评审者拿放大镜/比色卡」的动作用代码落地。
//
// 裁剪一致性：所有函数直接处理传入的 PNG 字节（解码 → 运算 → 重新编码），
// 不依赖引擎/浏览器会话，因此 --img 本地图、--img URL、--url 站点截图均可复用。
package vlm

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// DiffStats 是一场像素级 diff 的量化结果（供 VLM 与调用方理解差异程度）。
type DiffStats struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	DiffPixels  int     `json:"diff_pixels"`  // 被视为「不同」的像素数
	TotalPixels int     `json:"total_pixels"` // 参与比较的像素总数
	Ratio       float64 `json:"ratio"`        // DiffPixels / TotalPixels（0~1）
	MaxChannel  int     `json:"max_channel"`  // 任一像素任一信道的最大绝对差（0~255）
	BBox        string  `json:"bbox"`         // 差异区域包围盒 "x1,y1,x2,y2"（像素坐标）
}

// DiffImages 对两张 PNG 做像素级对比（若尺寸不一致，以左上角对齐、按最小宽高比较）。
// 返回一张「差异 overlay」PNG：把 b 中相对 a 不同的像素标记为高亮（默认洋红），
// 其余像素淡化为 b 的灰度，便于 VLM 一眼看到差异落点；并返回量化 DiffStats。
func DiffImages(aPng, bPng []byte) ([]byte, DiffStats, error) {
	a, err := decodePNG(aPng)
	if err != nil {
		return nil, DiffStats{}, fmt.Errorf("eye: diff decode a: %w", err)
	}
	b, err := decodePNG(bPng)
	if err != nil {
		return nil, DiffStats{}, fmt.Errorf("eye: diff decode b: %w", err)
	}
	w := min(a.Bounds().Dx(), b.Bounds().Dx())
	h := min(a.Bounds().Dy(), b.Bounds().Dy())
	if w <= 0 || h <= 0 {
		return nil, DiffStats{}, fmt.Errorf("eye: diff empty image")
	}

	// 产物画布：默认把 b 转成低饱和灰度垫底，差异像素覆盖为洋红高亮。
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	stats := DiffStats{Width: w, Height: h, TotalPixels: w * h}
	minX, minY := w, h
	maxX, maxY := -1, -1
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			ra, ga, ba, _ := a.At(x, y).RGBA()
			rb, gb, bb, _ := b.At(x, y).RGBA()
			ra8, ga8, ba8 := uint8(ra>>8), uint8(ga>>8), uint8(ba>>8)
			rb8, gb8, bb8 := uint8(rb>>8), uint8(gb>>8), uint8(bb>>8)
			dR := absInt(int(rb8) - int(ra8))
			dG := absInt(int(gb8) - int(ga8))
			dB := absInt(int(bb8) - int(ba8))
			md := max3(dR, dG, dB)
			if md > stats.MaxChannel {
				stats.MaxChannel = md
			}
			if isDiffPixel(md) {
				stats.DiffPixels++
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
				out.Set(x, y, color.RGBA{255, 0, 255, 255}) // 洋红高亮
			} else {
				// 垫底灰 = b 的亮度
				lum := (uint16(rb8)*299 + uint16(gb8)*587 + uint16(bb8)*114) / 1000
				out.Set(x, y, color.RGBA{uint8(lum), uint8(lum), uint8(lum), 255})
			}
		}
	}
	if stats.DiffPixels > 0 {
		stats.BBox = fmt.Sprintf("%d,%d,%d,%d", minX, minY, maxX, maxY)
	} else {
		stats.BBox = ""
	}
	stats.Ratio = float64(stats.DiffPixels) / float64(stats.TotalPixels)

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, DiffStats{}, fmt.Errorf("eye: diff encode: %w", err)
	}
	return buf.Bytes(), stats, nil
}

// RegionAvgColor 计算 PNG 指定区域（像素坐标，可越出则钳制）的平均色，返回 "R,G,B"。
// 可作 VLM 的「比色卡」：判断某区域主色调是否一致。
func RegionAvgColor(pngBytes []byte, x, y, rw, rh int) (string, error) {
	img, err := decodePNG(pngBytes)
	if err != nil {
		return "", fmt.Errorf("eye: region color decode: %w", err)
	}
	b := img.Bounds()
	x0 := clampInt(x, 0, b.Max.X-1)
	y0 := clampInt(y, 0, b.Max.Y-1)
	x1 := clampInt(x+rw, x0+1, b.Max.X)
	y1 := clampInt(y+rh, y0+1, b.Max.Y)
	if x1 <= x0 || y1 <= y0 {
		return "", fmt.Errorf("eye: region color empty region %d,%d %dx%d", x, y, rw, rh)
	}
	var rSum, gSum, bSum uint64
	var n uint64
	for yy := y0; yy < y1; yy++ {
		for xx := x0; xx < x1; xx++ {
			r, g, bl, _ := img.At(xx, yy).RGBA()
			rSum += uint64(r >> 8)
			gSum += uint64(g >> 8)
			bSum += uint64(bl >> 8)
			n++
		}
	}
	if n == 0 {
		return "", fmt.Errorf("eye: region color no pixels")
	}
	return fmt.Sprintf("%d,%d,%d", uint8(rSum/n), uint8(gSum/n), uint8(bSum/n)), nil
}

// ZoomImage 裁剪 PNG 的指定区域并放大（最近邻，scale 倍），供 VLM「放大镜」细看局部细节。
func ZoomImage(pngBytes []byte, x, y, rw, rh, scale int) ([]byte, error) {
	img, err := decodePNG(pngBytes)
	if err != nil {
		return nil, fmt.Errorf("eye: zoom decode: %w", err)
	}
	b := img.Bounds()
	x0 := clampInt(x, 0, b.Max.X-1)
	y0 := clampInt(y, 0, b.Max.Y-1)
	x1 := clampInt(x+rw, x0+1, b.Max.X)
	y1 := clampInt(y+rh, y0+1, b.Max.Y)
	if x1 <= x0 || y1 <= y0 {
		return nil, fmt.Errorf("eye: zoom empty region %d,%d %dx%d", x, y, rw, rh)
	}
	if scale <= 0 {
		scale = 1
	}
	cw, ch := x1-x0, y1-y0
	out := image.NewRGBA(image.Rect(0, 0, cw*scale, ch*scale))
	for yy := 0; yy < ch*scale; yy++ {
		sy := y0 + yy/scale
		for xx := 0; xx < cw*scale; xx++ {
			sx := x0 + xx/scale
			out.Set(xx, yy, img.At(sx, sy))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("eye: zoom encode: %w", err)
	}
	return buf.Bytes(), nil
}

func decodePNG(data []byte) (image.Image, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return img, nil
}

// isDiffPixel 判定像素是否「差异显著」（任一信道差 > 阈值；默认 24）。
func isDiffPixel(md int) bool {
	return md > diffThreshold
}

const diffThreshold = 24

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func max3(a, b, c int) int {
	if a >= b && a >= c {
		return a
	}
	if b >= c {
		return b
	}
	return c
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
