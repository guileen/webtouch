package engine

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"log"
	"math"
	"sort"
	"strings"

	"uws/pkg/browser"
	"uws/pkg/classifier"
	"uws/pkg/matcher"
	"uws/pkg/vlm"

	uwspb "uws/proto/gen/go/proto/v1"
)

func needsDiscreteChoice(cands []matcher.Candidate) bool {
	if len(cands) == 0 {
		return false
	}
	ordered := append([]matcher.Candidate(nil), cands...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Confidence > ordered[j].Confidence })
	top1, top2 := ordered[0].Confidence, 0.0
	if len(ordered) > 1 {
		top2 = ordered[1].Confidence
	}
	return top1 >= classifier.ThresholdLow &&
		(top1 < classifier.ThresholdHigh || top1-top2 <= 0.15)
}

func macroSlicePositions(documentH, viewportH int) []int {
	if viewportH <= 0 || documentH <= 3*viewportH {
		return nil
	}
	count := int(math.Ceil(float64(documentH) / float64(viewportH)))
	if count < 3 {
		count = 3
	}
	if count > 5 {
		count = 5
	}
	maxY := documentH - viewportH
	positions := make([]int, 0, count)
	for i := 0; i < count; i++ {
		y := int(math.Round(float64(maxY) * float64(i) / float64(count-1)))
		if len(positions) == 0 || y != positions[len(positions)-1] {
			positions = append(positions, y)
		}
	}
	return positions
}

func captureDocumentCandidatePanels(ctx context.Context, longPage browser.LongPageScreenshotter,
	ordered []matcher.Candidate) ([]vlm.CandidateBlock, []vlm.ImagePanel, error) {
	viewportW, viewportH, documentW, documentH, err := longPage.DocumentMetrics(ctx)
	if err != nil {
		return nil, nil, err
	}
	type crop struct {
		rank int
		c    matcher.Candidate
		rect image.Rectangle
	}
	crops := make([]crop, 0, 4)
	for i, c := range ordered[:min(4, len(ordered))] {
		if c.W <= 0 || c.H <= 0 {
			continue
		}
		padX, padY := math.Max(24, c.W*0.2), math.Max(16, c.H*0.2)
		cropW, cropH := math.Max(120, c.W+2*padX), math.Max(48, c.H+2*padY)
		dx, dy := c.DocumentX, c.DocumentY
		x0 := int(math.Max(0, math.Floor(dx+c.W/2-cropW/2)))
		y0 := int(math.Max(0, math.Floor(dy+c.H/2-cropH/2)))
		x1 := int(math.Min(float64(min(documentW, viewportW)), math.Ceil(dx+c.W/2+cropW/2)))
		y1 := int(math.Min(float64(documentH), math.Ceil(dy+c.H/2+cropH/2)))
		if x1 <= x0 || y1 <= y0 || y0 >= documentH {
			continue
		}
		crops = append(crops, crop{rank: i, c: c, rect: image.Rect(x0, y0, x1, y1)})
	}
	if len(crops) == 0 {
		return nil, nil, fmt.Errorf("no capturable document-coordinate candidates")
	}
	// Batch nearby candidates into one native-resolution CDP clip. Repeated
	// offscreen Page.captureScreenshot calls on long, complex pages can each
	// trigger expensive compositor work; a shared strip preserves the no-scroll
	// contract while reducing round trips. Keep each strip bounded to two viewports.
	sort.SliceStable(crops, func(i, j int) bool { return crops[i].rect.Min.Y < crops[j].rect.Min.Y })
	type cropGroup struct {
		items  []crop
		bounds image.Rectangle
	}
	groups := make([]cropGroup, 0, len(crops))
	for _, item := range crops {
		if len(groups) == 0 || item.rect.Max.Y-groups[len(groups)-1].bounds.Min.Y > 2*viewportH {
			groups = append(groups, cropGroup{items: []crop{item}, bounds: item.rect})
			continue
		}
		g := &groups[len(groups)-1]
		g.items = append(g.items, item)
		g.bounds = g.bounds.Union(item.rect)
	}
	panelByRank := make(map[int][]byte, len(crops))
	for _, group := range groups {
		regionX := group.bounds.Min.X
		regionW := group.bounds.Dx()
		region, captureErr := longPage.CaptureDocumentRegion(ctx, regionX, group.bounds.Min.Y, regionW, group.bounds.Dy())
		if captureErr != nil {
			for _, item := range group.items {
				log.Printf("vlm: document candidate capture failed selector=%q: %v", item.c.Selector, captureErr)
			}
			continue
		}
		img, _, decodeErr := image.Decode(bytes.NewReader(region))
		if decodeErr != nil {
			return nil, nil, fmt.Errorf("decode shared candidate clip: %w", decodeErr)
		}
		for _, item := range group.items {
			rect := image.Rect(item.rect.Min.X-regionX, item.rect.Min.Y-group.bounds.Min.Y,
				item.rect.Max.X-regionX, item.rect.Max.Y-group.bounds.Min.Y).Intersect(img.Bounds())
			if rect.Empty() {
				continue
			}
			var panel bytes.Buffer
			if err := png.Encode(&panel, img.(interface {
				SubImage(image.Rectangle) image.Image
			}).SubImage(rect)); err != nil {
				return nil, nil, fmt.Errorf("encode candidate panel: %w", err)
			}
			panelByRank[item.rank] = panel.Bytes()
		}
	}
	blocks := make([]vlm.CandidateBlock, 0, len(panelByRank))
	panels := make([]vlm.ImagePanel, 0, len(panelByRank))
	for rank := 0; rank < min(4, len(ordered)); rank++ {
		pngBytes := panelByRank[rank]
		if len(pngBytes) == 0 {
			continue
		}
		c := ordered[rank]
		label := string(rune('A' + len(blocks)))
		blocks = append(blocks, vlm.CandidateBlock{Index: rank, Label: label, Selector: c.Selector,
			X: c.DocumentX, Y: c.DocumentY, W: c.W, H: c.H, Text: c.NodeText})
		panels = append(panels, vlm.ImagePanel{Label: label, PNG: pngBytes})
	}
	if len(blocks) == 0 {
		return nil, nil, fmt.Errorf("no capturable document-coordinate candidates")
	}
	return blocks, panels, nil
}

// tryMacroSliceRoute 仅在长页且所有前序候选低分/为空时，按文档纵深取 3–5 个只读切片，
// 让 VLM 先选宏观区域。选中后才滚动到区域并由常规 VLM 在该视口内精定位。
func (s *Server) tryMacroSliceRoute(ctx context.Context, bc *browser.BrowserContext, target string,
	cands []matcher.Candidate) (selectedY int, ok bool) {
	for _, c := range cands {
		if c.Confidence >= classifier.ThresholdLow {
			return 0, false
		}
	}
	longPage, ok := bc.Session().(browser.LongPageScreenshotter)
	if !ok {
		return 0, false
	}
	vw, vh, _, docH, err := longPage.DocumentMetrics(ctx)
	if err != nil || vw <= 0 || vh <= 0 || docH <= 3*vh {
		return 0, false
	}
	positions := macroSlicePositions(docH, vh)
	panels := make([]vlm.ImagePanel, 0, len(positions))
	labels := make([]string, 0, len(positions))
	for _, y := range positions {
		pngBytes, err := longPage.CaptureDocumentRegion(ctx, 0, y, vw, vh)
		if err != nil {
			log.Printf("vlm: macro slice capture failed y=%d: %v", y, err)
			return 0, false
		}
		label := string(rune('A' + len(panels)))
		panels = append(panels, vlm.ImagePanel{Label: label, PNG: pngBytes})
		labels = append(labels, label)
	}
	if len(panels) < 3 {
		return 0, false
	}
	montage, w, h, err := vlm.BuildImagePanelMontage(panels)
	if err != nil {
		log.Printf("vlm: macro slice montage failed: %v", err)
		return 0, false
	}
	tctx, cancel := context.WithTimeout(ctx, s.vlmTimeout())
	choice, err := vlm.ResolveLabeledChoice(tctx, s.vlmFb.Client, montage, w, h, target, labels)
	cancel()
	if err != nil || choice == "NONE" {
		log.Printf("vlm: macro slice rejected target=%q choice=%q err=%v", target, choice, err)
		return 0, false
	}
	if len(choice) != 1 || choice[0] < 'A' || choice[0] > 'E' {
		return 0, false
	}
	idx := int(choice[0] - 'A')
	if idx < 0 || idx >= len(positions) {
		return 0, false
	}
	if err := longPage.ScrollDocumentTo(ctx, positions[idx]); err != nil {
		log.Printf("vlm: macro slice scroll failed y=%d: %v", positions[idx], err)
		return 0, false
	}
	log.Printf("vlm: macro slice selected=%s y=%d doc_h=%d viewport_h=%d target=%q",
		choice, positions[idx], docH, vh, target)
	return positions[idx], true
}

// tryDiscreteChoiceFallback 尝试执行定位阶梯第 3 层：Top-4 候选块裁块与 VLM 离散选择（A/B/C/D）闭环（§5.0.19）。
// 若离散选择胜出并派发成功，返回 (观测串, true)；否则返回 ("", false) fail-soft 退回整页 VLM 兜底。
func (s *Server) tryDiscreteChoiceFallback(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest, semantic, url string, cands []matcher.Candidate) (string, bool) {
	if !s.vlmEnabled() || len(cands) == 0 {
		return "", false
	}
	// §5.0.19 条款 1：明确低分样本交整页 VLM；高分且 Top-1 明显领先时
	// 继续原确定性派发。只有中间置信度或 Top-1/2 歧义时才消耗裁块复选预算。
	ordered := append([]matcher.Candidate(nil), cands...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Confidence > ordered[j].Confidence })
	top1 := ordered[0].Confidence
	top2 := 0.0
	if len(ordered) > 1 {
		top2 = ordered[1].Confidence
	}
	if top1 < classifier.ThresholdLow || (top1 >= classifier.ThresholdHigh && top1-top2 > 0.15) {
		return "", false
	}
	sess := bc.Session()
	shot, ok := sess.(browser.Screenshotter)
	if !ok {
		return "", false
	}

	// 1) 组装 Top-4。长页优先按文档坐标独立截每个候选，避免把屏外真实目标
	// 因当前视口过滤掉；截图不滚动页面，只有候选获选后才派发动作。
	var blocks []vlm.CandidateBlock
	var panels []vlm.ImagePanel
	var err error
	longPage, supportsLongPage := sess.(browser.LongPageScreenshotter)
	if supportsLongPage {
		blocks, panels, err = captureDocumentCandidatePanels(ctx, longPage, ordered)
		if err != nil {
			log.Printf("vlm: document candidate preparation failed: %v", err)
			blocks, panels = nil, nil
		}
	}

	var montagePng []byte
	var mw, mh int
	if len(blocks) > 0 && len(panels) == len(blocks) {
		montagePng, mw, mh, err = vlm.BuildImagePanelMontage(panels)
	} else {
		// 旧会话或无法读取文档尺寸时，保留视口内候选路径。
		png, viewportW, viewportH, captureErr := shot.CaptureScreenshot(ctx)
		if captureErr != nil {
			log.Printf("vlm: discrete choice screenshot error: %v", captureErr)
			return "", false
		}
		blocks = blocks[:0]
		for _, c := range ordered {
			if c.W <= 0 || c.H <= 0 || c.X+c.W <= 0 || c.Y+c.H <= 0 || c.X >= float64(viewportW) || c.Y >= float64(viewportH) {
				continue
			}
			blocks = append(blocks, vlm.CandidateBlock{Index: len(blocks), Label: string(rune('A' + len(blocks))),
				Selector: c.Selector, X: c.X, Y: c.Y, W: c.W, H: c.H, Text: c.NodeText})
			if len(blocks) == 4 {
				break
			}
		}
		if len(blocks) == 0 {
			return "", false
		}
		montagePng, mw, mh, err = vlm.BuildCandidateMontageForViewport(png, viewportW, viewportH, blocks)
	}
	if err != nil {
		log.Printf("vlm: discrete choice montage error: %v", err)
		return "", false
	}

	// 4) 调度视觉模型执行离散选择
	action := req.GetExpectedAction()
	target := req.GetTargetSemantic()
	tctx, cancel := context.WithTimeout(ctx, s.vlmTimeout())
	defer cancel()

	var res vlm.DiscreteChoiceResult
	if len(panels) == len(blocks) && len(panels) > 0 {
		labels := make([]string, 0, len(blocks))
		for _, b := range blocks {
			labels = append(labels, b.Label)
		}
		res.Choice, err = vlm.ResolveLabeledChoice(tctx, s.vlmFb.Client, montagePng, mw, mh, target, labels)
		if err == nil && res.Choice != "NONE" {
			for i := range blocks {
				if blocks[i].Label == res.Choice {
					res.Candidate = &blocks[i]
					break
				}
			}
		}
	} else {
		res, err = vlm.ResolveDiscreteChoice(tctx, s.vlmFb.Client, montagePng, mw, mh, target, blocks)
	}
	if err != nil {
		log.Printf("vlm: discrete choice resolve error url=%s target=%q: %v", url, target, err)
		return "", false
	}

	if res.Choice == "NONE" || res.Candidate == nil {
		log.Printf("vlm: discrete choice rejected (NONE) url=%s target=%q", url, target)
		return "", false
	}

	win := res.Candidate
	log.Printf("vlm: discrete choice won choice=%s selector=%q text=%q url=%s target=%q",
		res.Choice, win.Selector, win.Text, url, target)

	// 5) 物理派发：优先选择器，兜底盒模型几何中心
	var dispatched bool
	var obs string
	learnedSelector := win.Selector
	if win.Selector != "" {
		// 选择器路径一旦尝试就不再改走坐标，避免无法证明「未派发」时重复执行动作。
		dispatched, obs = s.executeWithSelector(ctx, bc, action, req.GetValue(), semantic, win.Selector)
		if !dispatched {
			if strings.Contains(obs, "click_failed") || strings.Contains(obs, "input_failed") {
				return browser.FaultVLMlandingUnverified + ":复选派发结果不确定，停止后续重试", false
			}
			return obs, false
		}
	} else {
		// 无选择器时才以盒模型中心坐标派发。
		cx := int(win.X + win.W/2.0)
		cy := int(win.Y + win.H/2.0)
		if vp, ok := sess.(browser.VLMProbeSession); ok {
			switch action {
			case "click":
				learnedSelector, dispatched, obs = vp.ClickAt(ctx, cx, cy)
			case "input", "fill", "select":
				learnedSelector, dispatched, obs = vp.InputAt(ctx, cx, cy, req.GetValue(), semantic)
			case "extract":
				learnedSelector, obs, dispatched = vp.ExtractAt(ctx, cx, cy)
			}
		}
	}

	if !dispatched {
		log.Printf("vlm: discrete choice dispatch failed url=%s target=%q", url, target)
		if strings.Contains(obs, "click_failed") || strings.Contains(obs, "input_failed") {
			return browser.FaultVLMlandingUnverified + ":复选派发结果不确定，停止后续重试", false
		}
		return obs, false
	}

	// 6) 落点自证校验（§5.0.9 条款 6/7）
	if fault, rejected := s.learnedReuseFault(bc, action, url, target); rejected {
		log.Printf("vlm: discrete choice landing fault: %s", fault)
		return fault, false
	}

	// 7) 成功回写自进化学习存储（"一次复选，下次 0ms 直连"）
	entry := vlm.LearnedEntry{
		Selector: learnedSelector,
		NodeText: win.Text,
	}
	if learnedSelector == "" && win.W > 0 && win.H > 0 {
		entry.Semantic = win.Text
	}
	s.vlmLearnOnClick(bc, action, url, target, entry)

	if obs == "" {
		obs = fmt.Sprintf("discrete_choice:%s", res.Choice)
	}
	return obs, true
}
