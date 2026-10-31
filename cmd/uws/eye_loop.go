package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"uws/pkg/vlm"
)

// eyeEvidenceCapabilities 是义眼主控循环可请求的证据原语清单（VLM 视角的描述）。
const eyeEvidenceCapabilities = `diff — 对前两张**字节图**做像素级对比，生成差异 overlay 图（洋红=差异、灰底=第二张）+ 量化统计（差异占比/bbox/最大信道差）
zoom — 对第一张图区域放大（参数 x,y,w,h,scale）
region_color — 对第一张图指定区域取平均色（参数 x,y,w,h）`

// eyeMaxRounds 义眼主控循环单次请求的最大轮数（超限安全收尾，防空转）。
const eyeMaxRounds = 4

// eyeMainControl 运行「义眼主控循环」：VLM 按任务自决是否调用证据原语（多轮），
// 但被强约束为优先单轮 conclude，避免过慢。返回最终 EyeResult。
func eyeMainControl(ctx context.Context, client *vlm.EyeClient, opts chatOptions, imgs *[]vlm.EyeImage, basePrompt string) (vlm.EyeResult, error) {
	for round := 1; round <= eyeMaxRounds; round++ {
		prompt := basePrompt + "\n\n【当前图片】\n" + describeImages(*imgs)
		req := vlm.EyeRequest{
			Question: prompt,
			Images:   *imgs,
			Detail:   opts.detail,
			JSON:     true,
			System:   vlm.EyeControlSystemPrompt(eyeEvidenceCapabilities),
		}
		text, err := client.Ask(ctx, req)
		if err != nil {
			// 网络/上游失败：安全降级，不把错误当结论。
			return vlm.EyeResult{Verdict: "unknown", Summary: "义眼主控调用失败: " + err.Error(), Confidence: 0, Raw: text}, nil
		}
		turn, perr := vlm.ParseEyeTurn(text)
		if perr != nil {
			// 模型没按 schema 输出：降级为包裹原文的 unknown（取保守结论）。
			return vlm.EyeResult{Verdict: "unknown", Summary: clip(text, 240), Confidence: 0, Raw: text}, nil
		}
		if turn.Action == "" || turn.Action == "conclude" {
			if turn.Verdict == "" {
				turn.Verdict = "unknown"
			}
			return turn.EyeResult, nil
		}
		// 请求证据原语 → 执行 → 追加证据 → 下一轮。
		evidence, err := runEyeEvidence(opts, turn, imgs)
		if err != nil {
			// 证据执行失败（如图片非字节/参数越界）：安全收尾，并把失败原因给用户。
			return vlm.EyeResult{Verdict: "unknown", Summary: "请求证据失败(" + turn.Action + "): " + err.Error(), Confidence: 0, Raw: text}, nil
		}
		basePrompt = basePrompt + "\n\n【你已请求的证据（第 " + fmt.Sprintf("%d", round) + " 轮）】" + evidence
	}
	// 达到上限：安全收尾，避免无限循环。
	return vlm.EyeResult{Verdict: "unknown", Summary: fmt.Sprintf("证据原语查询达 %d 轮上限，未能单轮收尾；建议用 --zoom/--region-color 锁定区域或细化问题。", eyeMaxRounds), Confidence: 0}, nil
}

// runEyeEvidence 执行一个证据原语，返回证据文本（并把生成的证据图 append 到 imgs）。
func runEyeEvidence(opts chatOptions, turn vlm.EyeTurn, imgs *[]vlm.EyeImage) (string, error) {
	switch turn.Action {
	case "diff":
		if len(*imgs) < 2 {
			return "", errors.New("diff 需要至少两张图")
		}
		if len((*imgs)[0].Data) == 0 || len((*imgs)[1].Data) == 0 {
			return "", errors.New("前两张图需为字节数据（本地/站点截图，远程 URL --img 无法本地 diff）")
		}
		diffPng, stats, err := vlm.DiffImages((*imgs)[0].Data, (*imgs)[1].Data)
		if err != nil {
			return "", err
		}
		*imgs = append(*imgs, vlm.EyeImage{Label: "像素差异图", Data: diffPng})
		return fmt.Sprintf("【像素差异证据】差异像素 %d/%d（%.1f%%），最大信道差 %d，差异区域 bbox=%s；洋红=差异、灰底=第二张。",
			stats.DiffPixels, stats.TotalPixels, stats.Ratio*100, stats.MaxChannel, orDash(stats.BBox)), nil
	case "zoom":
		if len(*imgs) == 0 {
			return "", errors.New("zoom 需要至少一张图")
		}
		if len((*imgs)[0].Data) == 0 {
			return "", errors.New("第一张图需为字节数据")
		}
		scale := turn.Scale
		if scale <= 0 {
			scale = 1
		}
		zoomPng, err := vlm.ZoomImage((*imgs)[0].Data, turn.X, turn.Y, turn.W, turn.H, scale)
		if err != nil {
			return "", err
		}
		*imgs = append(*imgs, vlm.EyeImage{Label: fmt.Sprintf("区域放大(%d,%d %dx%d x%d)", turn.X, turn.Y, turn.W, turn.H, scale), Data: zoomPng})
		return fmt.Sprintf("【区域放大证据】已放大第一张图区域 [%d,%d %dx%d x%d]，请细看该区域。", turn.X, turn.Y, turn.W, turn.H, scale), nil
	case "region_color":
		if len(*imgs) == 0 {
			return "", errors.New("region_color 需要至少一张图")
		}
		if len((*imgs)[0].Data) == 0 {
			return "", errors.New("第一张图需为字节数据")
		}
		avg, err := vlm.RegionAvgColor((*imgs)[0].Data, turn.X, turn.Y, turn.W, turn.H)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("【区域颜色证据】第一张图区域 [%d,%d %dx%d] 的平均色为 RGB(%s)。", turn.X, turn.Y, turn.W, turn.H, avg), nil
	default:
		return "", fmt.Errorf("未知证据原语 %q", turn.Action)
	}
}

// describeImages 生成每张图的简要清单，供主控提示里告知 VLM 当前手头有哪些图。
func describeImages(imgs []vlm.EyeImage) string {
	if len(imgs) == 0 {
		return "（无）"
	}
	var b strings.Builder
	for i, img := range imgs {
		label := img.Label
		if label == "" {
			label = fmt.Sprintf("图%d", i+1)
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, label)
	}
	return strings.TrimRight(b.String(), "\n")
}

// clip 按 rune 安全截断长文本（仅用于日志/降级 summary 展示）。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
