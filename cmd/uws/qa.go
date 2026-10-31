package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uws/pkg/qa"
)

// ANSI 颜色常量（终端自适应与高亮展示）
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiCyan   = "\033[36m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiGray   = "\033[90m"
)

func runQA(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(qaHelp())
		return nil
	}

	fs := flag.NewFlagSet("qa", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		urlRaw    string
		refImg    string
		viewports string
		outputDir string
		cdpURL    string
		timeout   time.Duration
		headless  bool
	)

	fs.StringVar(&urlRaw, "url", "", "待验收的前端页面预览/Staging URL（必填）")
	fs.StringVar(&refImg, "ref-img", "", "参考设计稿或基准截图路径（可选，用于像素级差分对比）")
	fs.StringVar(&viewports, "viewports", "mobile,tablet,desktop", "扫描视口列表（支持 390,768,1440 或 命名别名）")
	fs.StringVar(&outputDir, "output", filepath.Join("build", "qa"), "验收产物（截屏、Diff、交付单）输出目录")
	fs.StringVar(&cdpURL, "cdp-url", "", "Chrome CDP 调试端点（默认自动探测 127.0.0.1:9222）")
	fs.DurationVar(&timeout, "timeout", 60*time.Second, "总体检等待超时上限")
	fs.BoolVar(&headless, "headless", true, "是否以无头静默模式启动 Chrome")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if urlRaw == "" {
		// 允许直接传入裸 URL 作为首个非 flag 参数：`webtouch qa http://localhost:3000`
		if fs.NArg() > 0 && (strings.HasPrefix(fs.Arg(0), "http://") || strings.HasPrefix(fs.Arg(0), "https://")) {
			urlRaw = fs.Arg(0)
		} else {
			return errors.New("缺少必填参数 --url <preview_url>（例如: webtouch qa --url http://localhost:3000）")
		}
	}

	// 解析视口规格
	vps, err := qa.ParseViewports(viewports)
	if err != nil {
		return fmt.Errorf("视口参数解析失败: %w", err)
	}

	// 加载基准参考图
	var refBytes []byte
	if refImg != "" {
		b, err := os.ReadFile(refImg)
		if err != nil {
			return fmt.Errorf("读取基准图片 %s 失败: %w", refImg, err)
		}
		refBytes = b
	}

	fmt.Printf("%s%s=== WebTouch QA Studio · 前端多端交付验收工作台 ===%s\n", ansiBold, ansiCyan, ansiReset)
	fmt.Printf("🎯 验收目标: %s%s%s\n", ansiBold, urlRaw, ansiReset)
	vpNames := make([]string, len(vps))
	for i, v := range vps {
		vpNames[i] = fmt.Sprintf("%s (%d×%d)", v.Name, v.Width, v.Height)
	}
	fmt.Printf("📐 扫描视口: %s\n", strings.Join(vpNames, ", "))
	if len(refBytes) > 0 {
		fmt.Printf("🔍 视觉差分: 已加载基准参考图 (%s, %d 字节)\n", filepath.Base(refImg), len(refBytes))
	}
	fmt.Printf("📁 产物目录: %s\n\n", outputDir)

	runnerOpts := qa.RunnerOptions{
		URL:        urlRaw,
		Viewports:  vps,
		RefImage:   refBytes,
		OutputDir:  outputDir,
		AttachCDP:  cdpURL,
		Headless:   headless,
		Timeout:    timeout,
		OnProgress: printProgress,
	}

	runner := qa.NewRunner(runnerOpts)
	ctx := context.Background()

	report, err := runner.Run(ctx)
	if err != nil {
		return fmt.Errorf("验收流程异常中断: %w", err)
	}

	printSummaryCard(report)

	if report.Verdict == qa.VerdictCritical {
		return fmt.Errorf("前端交付体检失败: 页面存在严重横向滚动溢出或关键缺陷 (Health Score: %d/100)", report.HealthScore)
	}

	return nil
}

func printProgress(step, total int, msg string) {
	fmt.Printf("  %s[%d/%d]%s %s\n", ansiCyan, step, total, ansiReset, msg)
}

func printSummaryCard(r *qa.QAReport) {
	fmt.Println()
	fmt.Printf("%s================ 交付验收结论 (Acceptance Certificate) ================%s\n", ansiBold, ansiReset)

	var verdictColor string
	switch r.Verdict {
	case qa.VerdictPass:
		verdictColor = ansiGreen
	case qa.VerdictWarning:
		verdictColor = ansiYellow
	default:
		verdictColor = ansiRed
	}

	fmt.Printf("综合评级: %s%s%s%s (综合健康分: %s%d / 100%s)\n",
		ansiBold, verdictColor, r.Verdict, ansiReset,
		ansiBold, r.HealthScore, ansiReset)
	fmt.Printf("体检耗时: %s%dms%s | 扫描视口数: %d\n", ansiBold, r.DurationMs, ansiReset, len(r.Viewports))

	hasOverflow := false
	totalErrors := 0
	totalBroken := 0
	totalSmall := 0
	for _, v := range r.Viewports {
		if v.HasHorizontalOverflow {
			hasOverflow = true
		}
		totalErrors += len(v.ConsoleErrors)
		totalBroken += len(v.BrokenResources)
		totalSmall += len(v.SmallTargets)
	}

	if hasOverflow {
		fmt.Printf("横向溢出: %s发现溢出 (FAIL)%s\n", ansiRed, ansiReset)
	} else {
		fmt.Printf("横向溢出: %s零溢出 (PASS)%s\n", ansiGreen, ansiReset)
	}

	if totalErrors > 0 {
		fmt.Printf("控制台报错: %s%d 个未捕获异常%s\n", ansiRed, totalErrors, ansiReset)
	} else {
		fmt.Printf("控制台报错: %s0 报错%s\n", ansiGreen, ansiReset)
	}

	if totalBroken > 0 {
		fmt.Printf("资源死链: %s%d 处裂图/失败%s\n", ansiRed, totalBroken, ansiReset)
	} else {
		fmt.Printf("资源死链: %s0 裂图%s\n", ansiGreen, ansiReset)
	}

	if totalSmall > 0 {
		fmt.Printf("热区违规: %s%d 处小于 32x32px%s\n", ansiYellow, totalSmall, ansiReset)
	} else {
		fmt.Printf("热区违规: %s全部合格%s\n", ansiGreen, ansiReset)
	}

	if len(r.SummaryIssues) > 0 {
		fmt.Printf("\n%s待关注问题列表:%s\n", ansiBold, ansiReset)
		for _, issue := range r.SummaryIssues {
			fmt.Printf("  • %s\n", issue)
		}
	}

	fmt.Println()
	if r.HTMLReport != "" {
		fmt.Printf("📄 交互式多端交付验收单: %s%s%s\n", ansiBold, r.HTMLReport, ansiReset)
	}
	if r.MDReport != "" {
		fmt.Printf("📑 CI/CD Markdown 交付证据: %s%s%s\n", ansiBold, r.MDReport, ansiReset)
	}
	fmt.Printf("%s========================================================================%s\n", ansiBold, ansiReset)
}

func qaHelp() string {
	return `webtouch qa —— WebTouch QA Studio 前端多端交付验收工作台

用法:
  webtouch qa --url <preview_url> [选项]
  webtouch qa <preview_url> [选项]

选项:
  --url <url>              待体检的前端页面 URL（例如 http://localhost:3000）
  --ref-img <path>         Figma 设计稿或基准截图（自动触发洋红高亮像素差分）
  --viewports <list>       视口配置列表，默认: mobile,tablet,desktop
                           支持: 390,768,1440 或 390x844,768x1024,1440x900
  --output <dir>           报告与快照产物目录，默认: build/qa
  --cdp-url <addr>         Chrome CDP 地址（如未指定且 127.0.0.1:9222 开启则自动附着）
  --headless               是否无头运行（默认 true）
  --timeout <dur>          超时上限，默认 60s

示例:
  # 1. 零配置 10 秒快速多端体检（扫除溢出、控制台异常与裂图）
  webtouch qa --url http://localhost:3000

  # 2. 视觉基准比对体检（与 Figma 设计稿执行洋红高亮 Diff）
  webtouch qa --url http://localhost:3000 --ref-img figma.png

  # 3. 指定输出目录与自定义多视口
  webtouch qa --url http://localhost:3000 --viewports 390,768,1440 --output dist/qa
`
}
