package vlm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"regexp"
	"strings"
)

// CandidateBlock 承载一个待裁块候选的身份、盒模型几何与文本信息。
type CandidateBlock struct {
	Index    int     // 在原始候选池中的决策序索引
	Label    string  // "A", "B", "C", "D"
	Selector string  // 精确选择器
	X        float64 // CSS 物理像素 x
	Y        float64 // CSS 物理像素 y
	W        float64 // CSS 物理像素宽
	H        float64 // CSS 物理像素高
	Text     string  // 节点文本
}

// ImagePanel 是长页宏观切片的粗选面板；各图先分别裁自文档区域，再按标签拼版。
type ImagePanel struct {
	Label string
	PNG   []byte
}

// DiscreteChoiceResult 记录离散选择胜出候选与原始响应。
type DiscreteChoiceResult struct {
	Choice    string          // "A", "B", "C", "D" 或 "NONE"
	Candidate *CandidateBlock // 胜出候选指针，NONE 时为 nil
	Raw       string          // 模型原始输出文本
}

// 5x7 经典点阵字体，包含宏观切片使用的 A–E 大写字母（各占 7 行，低 5 位有效）。
var font5x7 = map[byte][7]byte{
	'A': {0b01110, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'B': {0b11110, 0b10001, 0b10001, 0b11110, 0b10001, 0b10001, 0b11110},
	'C': {0b01111, 0b10000, 0b10000, 0b10000, 0b10000, 0b10000, 0b01111},
	'D': {0b11110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b11110},
	'E': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b11111},
}

// drawBadge 在目标图像的 (ox, oy) 位置绘制带圆角感的高对比度标头（如红框白底黑字）。
func drawBadge(dst draw.Image, ox, oy, scale int, label byte) {
	if scale < 1 {
		scale = 1
	}
	// 绘制 28x28 的外围边框与白色衬底
	badgeW, badgeH := 28*scale, 28*scale
	red := color.RGBA{R: 225, G: 29, B: 72, A: 255} // #e11d48
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	black := color.RGBA{R: 15, G: 23, B: 42, A: 255} // #0f172a

	for y := 0; y < badgeH; y++ {
		for x := 0; x < badgeW; x++ {
			px, py := ox+x, oy+y
			// 边框 2px
			if x < 2*scale || x >= badgeW-2*scale || y < 2*scale || y >= badgeH-2*scale {
				dst.Set(px, py, red)
			} else {
				dst.Set(px, py, white)
			}
		}
	}

	// 绘制 5x7 点阵字；徽章随截图 DPR 放大，保持可读。
	matrix, ok := font5x7[label]
	if !ok {
		return
	}
	startX := ox + (badgeW-15*scale)/2
	startY := oy + (badgeH-21*scale)/2
	glyphScale := 3 * scale

	for row := 0; row < 7; row++ {
		bits := matrix[row]
		for col := 0; col < 5; col++ {
			// 最高位是 col 0
			if (bits>>(4-col))&1 == 1 {
				for sy := 0; sy < glyphScale; sy++ {
					for sx := 0; sx < glyphScale; sx++ {
						dst.Set(startX+col*glyphScale+sx, startY+row*glyphScale+sy, black)
					}
				}
			}
		}
	}
}

// DiscreteChoicePrompt 构造离散选择确定性提示词（对齐 §5.0.19 条款 3）。
func DiscreteChoicePrompt(target string, labels []string) string {
	ls := strings.Join(labels, "/")
	return fmt.Sprintf("看图作答，不要推理。图上把候选区域编号为 %s（每块左上角有对应大写字母）。\n"+
		"哪一块里有文字「%s」？只回那一块的字母。\n"+
		"只输出一个 JSON：{\"choice\":\"X\"}（X 取 %s 之一，若均无或无法确定则输出 {\"choice\":\"NONE\"}，不要解释、不要坐标）。\n",
		ls, target, ls)
}

// ParseDiscreteChoice 从模型原始文本中提取离散选择结果（容忍 markdown 围栏）。
func ParseDiscreteChoice(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	re := regexp.MustCompile(`\{[^{}]*"choice"\s*:\s*"([A-Za-z]+)"[^{}]*\}`)
	m := re.FindStringSubmatch(s)
	if len(m) >= 2 {
		return strings.ToUpper(strings.TrimSpace(m[1])), nil
	}

	// 备选容忍：直接按 JSON unmarshal
	var r struct {
		Choice string `json:"choice"`
	}
	a, b := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if a >= 0 && b > a {
		if err := json.Unmarshal([]byte(s[a:b+1]), &r); err == nil && r.Choice != "" {
			return strings.ToUpper(strings.TrimSpace(r.Choice)), nil
		}
	}

	return "", fmt.Errorf("discrete_choice: no valid choice in %q", trunc(raw, 100))
}

// BuildCandidateMontage 将完整视口截图根据 Top-K 候选几何裁剪并拼接为多宫格拼版。
// 输出组合图像 PNG 字节流、宽度和高度。
func BuildCandidateMontage(fullPng []byte, cands []CandidateBlock) ([]byte, int, int, error) {
	srcImg, err := png.Decode(bytes.NewReader(fullPng))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("discrete_choice: decode png: %w", err)
	}
	bounds := srcImg.Bounds()
	return BuildCandidateMontageForViewport(fullPng, bounds.Dx(), bounds.Dy(), cands)
}

// BuildCandidateMontageForViewport 将 CSS 像素几何映射到截图像素后裁块。
// CDP 返回的 PNG 可能因 DPR 大于 CSS 视口尺寸；两种坐标不能直接混用。
func BuildCandidateMontageForViewport(fullPng []byte, viewportW, viewportH int, cands []CandidateBlock) ([]byte, int, int, error) {
	if len(cands) == 0 {
		return nil, 0, 0, errors.New("discrete_choice: no candidates to montage")
	}
	srcImg, err := png.Decode(bytes.NewReader(fullPng))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("discrete_choice: decode png: %w", err)
	}
	bounds := srcImg.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if viewportW <= 0 || viewportH <= 0 {
		return nil, 0, 0, fmt.Errorf("discrete_choice: invalid CSS viewport %dx%d", viewportW, viewportH)
	}
	scaleX := float64(srcW) / float64(viewportW)
	scaleY := float64(srcH) / float64(viewportH)
	badgeScale := int(math.Round((scaleX + scaleY) / 2))
	if badgeScale < 1 {
		badgeScale = 1
	}

	// 截取各候选的局部视野
	type croppedBlock struct {
		block CandidateBlock
		img   image.Image
		w, h  int
	}
	var cropped []croppedBlock

	for i, c := range cands {
		label := string(rune('A' + i))
		c.Label = label

		// §5.0.19 条款 2：带 Padding 局部视野
		padX := math.Max(24.0, c.W*0.2)
		padY := math.Max(16.0, c.H*0.2)

		minW, minH := 120.0, 48.0
		boxW := math.Max(c.W+2*padX, minW)
		boxH := math.Max(c.H+2*padY, minH)

		cx := c.X + c.W/2.0
		cy := c.Y + c.H/2.0

		// Padding 与钳制先在 CSS 视口坐标中完成，再按 PNG/CSS 比例映射。
		cssX0 := math.Max(0, cx-boxW/2)
		cssY0 := math.Max(0, cy-boxH/2)
		cssX1 := math.Min(float64(viewportW), cx+boxW/2)
		cssY1 := math.Min(float64(viewportH), cy+boxH/2)
		x0 := int(math.Floor(cssX0 * scaleX))
		y0 := int(math.Floor(cssY0 * scaleY))
		x1 := int(math.Ceil(cssX1 * scaleX))
		y1 := int(math.Ceil(cssY1 * scaleY))

		rect := image.Rect(x0, y0, x1, y1).Intersect(bounds)
		subW, subH := rect.Dx(), rect.Dy()
		if subW <= 0 || subH <= 0 {
			continue
		}

		// 创建子画布并拷贝
		subDst := image.NewRGBA(image.Rect(0, 0, subW, subH))
		draw.Draw(subDst, subDst.Bounds(), srcImg, rect.Min, draw.Src)

		// 左上角打上选项徽章
		drawBadge(subDst, 4*badgeScale, 4*badgeScale, badgeScale, label[0])

		cropped = append(cropped, croppedBlock{
			block: c,
			img:   subDst,
			w:     subW,
			h:     subH,
		})
	}

	if len(cropped) == 0 {
		return nil, 0, 0, errors.New("discrete_choice: all candidate crops empty")
	}

	// 自适应拼版：
	// K=1: 单块
	// K=2: 纵向拼版
	// K>=3: 2x2 四宫格
	var outW, outH int
	gap := 8
	bgColor := color.RGBA{R: 241, G: 245, B: 249, A: 255} // #f1f5f9

	var finalImg *image.RGBA

	if len(cropped) == 1 {
		outW, outH = cropped[0].w, cropped[0].h
		finalImg = image.NewRGBA(image.Rect(0, 0, outW, outH))
		draw.Draw(finalImg, finalImg.Bounds(), cropped[0].img, image.Point{}, draw.Src)
	} else if len(cropped) == 2 {
		outW = int(math.Max(float64(cropped[0].w), float64(cropped[1].w)))
		outH = cropped[0].h + cropped[1].h + gap
		finalImg = image.NewRGBA(image.Rect(0, 0, outW, outH))
		// 填充背景
		draw.Draw(finalImg, finalImg.Bounds(), &image.Uniform{C: bgColor}, image.Point{}, draw.Src)
		draw.Draw(finalImg, image.Rect(0, 0, cropped[0].w, cropped[0].h), cropped[0].img, image.Point{}, draw.Src)
		draw.Draw(finalImg, image.Rect(0, cropped[0].h+gap, cropped[1].w, cropped[0].h+gap+cropped[1].h), cropped[1].img, image.Point{}, draw.Src)
	} else {
		// 2x2 田字格
		colW := int(math.Max(float64(cropped[0].w), float64(cropped[1].w)))
		if len(cropped) > 2 {
			colW = int(math.Max(float64(colW), float64(cropped[2].w)))
		}
		if len(cropped) > 3 {
			colW = int(math.Max(float64(colW), float64(cropped[3].w)))
		}

		row0H := int(math.Max(float64(cropped[0].h), float64(cropped[1].h)))
		row1H := 0
		if len(cropped) > 2 {
			row1H = cropped[2].h
		}
		if len(cropped) > 3 && cropped[3].h > row1H {
			row1H = cropped[3].h
		}

		outW = colW*2 + gap
		outH = row0H + row1H + gap
		finalImg = image.NewRGBA(image.Rect(0, 0, outW, outH))
		draw.Draw(finalImg, finalImg.Bounds(), &image.Uniform{C: bgColor}, image.Point{}, draw.Src)

		// A: (0, 0)
		draw.Draw(finalImg, image.Rect(0, 0, cropped[0].w, cropped[0].h), cropped[0].img, image.Point{}, draw.Src)
		// B: (colW+gap, 0)
		draw.Draw(finalImg, image.Rect(colW+gap, 0, colW+gap+cropped[1].w, cropped[1].h), cropped[1].img, image.Point{}, draw.Src)
		// C: (0, row0H+gap)
		if len(cropped) > 2 {
			draw.Draw(finalImg, image.Rect(0, row0H+gap, cropped[2].w, row0H+gap+cropped[2].h), cropped[2].img, image.Point{}, draw.Src)
		}
		// D: (colW+gap, row0H+gap)
		if len(cropped) > 3 {
			draw.Draw(finalImg, image.Rect(colW+gap, row0H+gap, colW+gap+cropped[3].w, row0H+gap+cropped[3].h), cropped[3].img, image.Point{}, draw.Src)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, finalImg); err != nil {
		return nil, 0, 0, fmt.Errorf("discrete_choice: encode png: %w", err)
	}

	return buf.Bytes(), outW, outH, nil
}

// BuildImagePanelMontage 将 1–5 张独立截图按标签拼版，用于长页宏观切片粗选。
func BuildImagePanelMontage(panels []ImagePanel) ([]byte, int, int, error) {
	if len(panels) == 0 || len(panels) > 5 {
		return nil, 0, 0, fmt.Errorf("discrete_choice: macro panels count %d outside [1,5]", len(panels))
	}
	type decodedPanel struct {
		label string
		img   image.Image
		w, h  int
	}
	decoded := make([]decodedPanel, 0, len(panels))
	for i, p := range panels {
		label := strings.ToUpper(strings.TrimSpace(p.Label))
		if label == "" {
			label = string(rune('A' + i))
		}
		if len(label) != 1 || label[0] < 'A' || label[0] > 'E' {
			return nil, 0, 0, fmt.Errorf("discrete_choice: invalid panel label %q", p.Label)
		}
		img, err := png.Decode(bytes.NewReader(p.PNG))
		if err != nil {
			return nil, 0, 0, fmt.Errorf("discrete_choice: decode panel %s: %w", label, err)
		}
		b := img.Bounds()
		decoded = append(decoded, decodedPanel{label: label, img: img, w: b.Dx(), h: b.Dy()})
	}
	if len(decoded) == 1 {
		dst := image.NewRGBA(image.Rect(0, 0, decoded[0].w, decoded[0].h))
		draw.Draw(dst, dst.Bounds(), decoded[0].img, decoded[0].img.Bounds().Min, draw.Src)
		drawBadge(dst, 8, 8, 2, decoded[0].label[0])
		var out bytes.Buffer
		if err := png.Encode(&out, dst); err != nil {
			return nil, 0, 0, err
		}
		return out.Bytes(), dst.Bounds().Dx(), dst.Bounds().Dy(), nil
	}
	cols := 2
	rows := (len(decoded) + cols - 1) / cols
	gap := 8
	cellW, cellH := 0, 0
	for _, p := range decoded {
		if p.w > cellW {
			cellW = p.w
		}
		if p.h > cellH {
			cellH = p.h
		}
	}
	outW := cellW*cols + gap*(cols-1)
	outH := cellH*rows + gap*(rows-1)
	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.RGBA{R: 241, G: 245, B: 249, A: 255}}, image.Point{}, draw.Src)
	for i, p := range decoded {
		x := (i % cols) * (cellW + gap)
		y := (i / cols) * (cellH + gap)
		draw.Draw(dst, image.Rect(x, y, x+p.w, y+p.h), p.img, p.img.Bounds().Min, draw.Src)
		drawBadge(dst, x+8, y+8, 2, p.label[0])
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, 0, 0, fmt.Errorf("discrete_choice: encode macro montage: %w", err)
	}
	return out.Bytes(), outW, outH, nil
}

// ResolveLabeledChoice 让 VLM 在一组固定标签中作离散选择，支持宏观切片 A–E。
func ResolveLabeledChoice(ctx context.Context, client Client, pngBytes []byte, w, h int, target string, labels []string) (string, error) {
	if len(labels) == 0 || len(labels) > 5 {
		return "", fmt.Errorf("discrete_choice: label count %d outside [1,5]", len(labels))
	}
	prompt := DiscreteChoicePrompt(target, labels)
	res, err := client.Resolve(ctx, pngBytes, w, h, ResolveRequest{
		Target: target, Action: "click", Crop: true, Detail: DetailHigh,
		CustomPrompt: prompt,
	})
	if err != nil {
		return "", fmt.Errorf("discrete_choice: client resolve: %w", err)
	}
	choice := strings.ToUpper(strings.TrimSpace(res.Choice))
	if choice == "" {
		if parsed, parseErr := ParseDiscreteChoice(res.Raw); parseErr == nil {
			choice = parsed
		} else if parsed, parseErr := ParseDiscreteChoice(res.Text); parseErr == nil {
			choice = parsed
		}
	}
	if choice == "NONE" {
		return choice, nil
	}
	for _, label := range labels {
		if choice == strings.ToUpper(strings.TrimSpace(label)) {
			return choice, nil
		}
	}
	return "NONE", nil
}

// ResolveDiscreteChoice 使用视觉模型对拼接后的候选块进行离散选择（A/B/C/D）。
func ResolveDiscreteChoice(ctx context.Context, client Client, montagePng []byte, w, h int, target string, cands []CandidateBlock) (DiscreteChoiceResult, error) {
	if len(cands) == 0 {
		return DiscreteChoiceResult{Choice: "NONE"}, errors.New("discrete_choice: empty candidates")
	}
	var labels []string
	for i := range cands {
		label := string(rune('A' + i))
		cands[i].Label = label
		labels = append(labels, label)
	}

	prompt := DiscreteChoicePrompt(target, labels)
	req := ResolveRequest{
		Target:       target,
		Action:       "click",
		Capabilities: prompt,
		Crop:         true,
		Detail:       DetailHigh, // 确保清晰度
		CustomPrompt: prompt,
	}

	res, err := client.Resolve(ctx, montagePng, w, h, req)
	if err != nil {
		return DiscreteChoiceResult{Choice: "NONE"}, fmt.Errorf("discrete_choice: client resolve: %w", err)
	}

	choice := strings.ToUpper(strings.TrimSpace(res.Choice))
	if choice == "" {
		// 尝试从 Text 或 Selector 或 Raw 中解析
		parsed, perr := ParseDiscreteChoice(res.Raw)
		if perr == nil {
			choice = parsed
		} else if res.Text != "" {
			if parsed, perr := ParseDiscreteChoice(res.Text); perr == nil {
				choice = parsed
			}
		}
	}

	if choice == "NONE" || choice == "" {
		return DiscreteChoiceResult{Choice: "NONE", Raw: res.Raw}, nil
	}

	for i := range cands {
		if cands[i].Label == choice {
			return DiscreteChoiceResult{
				Choice:    choice,
				Candidate: &cands[i],
				Raw:       res.Raw,
			}, nil
		}
	}

	return DiscreteChoiceResult{Choice: "NONE", Raw: res.Raw}, nil
}
