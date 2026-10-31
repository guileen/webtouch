package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"uws/pkg/provider"
)

func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		urlRaw    string
		diffRef   string
		viewports string
		timeout   time.Duration
		headless  bool
		isolated  bool
		outputDir string
		cdpURL    string
		stdinJSON bool
	)

	fs.StringVar(&urlRaw, "url", "", "待检查的目标 URL")
	fs.StringVar(&diffRef, "diff", "", "参考设计稿或基准截图路径（可选，开启视觉差分）")
	fs.StringVar(&viewports, "viewports", "mobile,desktop", "扫描视口列表（支持 mobile,desktop 或自定义规格）")
	fs.DurationVar(&timeout, "timeout", 15*time.Second, "单任务执行超时上限")
	fs.BoolVar(&headless, "headless", true, "是否以无头模式运行 Chrome（默认 true，保持零打扰）")
	fs.BoolVar(&isolated, "isolated", false,
		"强隔离模式：不探测/不附着本机 9222 上的个人 Chrome，只用临时 Profile 自起无头浏览器（无人消费方体检必须开启）")
	fs.StringVar(&outputDir, "output", "", "中间截屏与差分产物输出目录（默认使用系统临时目录）")
	fs.StringVar(&cdpURL, "cdp-url", "", "外部 Chrome CDP 调试端点（默认自动探测）")
	fs.BoolVar(&stdinJSON, "stdin", false, "从标准输入读取 JSON 请求体")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// 允许直接传入 bare URL: `uws inspect http://localhost:3000`
	if urlRaw == "" && fs.NArg() > 0 && (strings.HasPrefix(fs.Arg(0), "http://") || strings.HasPrefix(fs.Arg(0), "https://")) {
		urlRaw = fs.Arg(0)
	}

	var parsedVps []provider.ViewportSpec
	if viewports != "" {
		v, err := provider.ParseViewports(viewports)
		if err == nil {
			parsedVps = v
		}
	}

	// 支持标准输入读取 InspectRequest
	if stdinJSON {
		var req provider.InspectRequest
		data, err := io.ReadAll(os.Stdin)
		if err == nil && len(data) > 0 {
			_ = json.Unmarshal(data, &req)
			if req.URL != "" {
				urlRaw = req.URL
			}
			if len(req.Viewports) > 0 {
				parsedVps = req.Viewports
			}
		}
	}

	if urlRaw == "" {
		return fmt.Errorf("inspect: 缺少必填参数 --url <url>")
	}

	p := provider.NewProvider()
	// 取消要能被清理路径看见：Hub 超时会先发 SIGTERM，若这里不拦信号，
	// 进程会直接终止，QA Runner 的 defer session.Close() 不执行，
	// 自起的 Chrome 与临时 Profile 就成了孤儿（2026-09-23 审查结论）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var bundle *provider.EvidenceBundle
	var err error

	if diffRef != "" {
		bundle, err = p.CaptureDiff(ctx, provider.DiffRequest{
			URL:          urlRaw,
			BaselinePath: diffRef,
			Viewports:    parsedVps,
			Timeout:      timeout,
			Headless:     headless,
			Isolated:     isolated,
			OutputDir:    outputDir,
			AttachCDP:    cdpURL,
		})
	} else {
		bundle, err = p.InspectPage(ctx, provider.InspectRequest{
			URL:       urlRaw,
			Viewports: parsedVps,
			Timeout:   timeout,
			Headless:  headless,
			Isolated:  isolated,
			OutputDir: outputDir,
			AttachCDP: cdpURL,
		})
	}

	if err != nil && bundle == nil {
		return fmt.Errorf("inspect failed: %w", err)
	}

	outBytes, _ := json.MarshalIndent(bundle, "", "  ")
	fmt.Println(string(outBytes))
	return err
}
