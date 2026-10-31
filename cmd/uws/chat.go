// webtouch chat —— UWS 默认推荐入口（2026-09-03 用户定案）。
//
// 两种模式自动分流：
//  1. 目标模式（默认）：只给一句自然语言目标（不传 --img/--url/--page/--diff 等视觉参数）
//     → 委派 Python 协调器 goal 实现：DSL 语法有效自动走确定性 `run --plan`；
//     否则 VLM 每轮“截图 + AXTree → 自决下一步”，引擎确定性执行，登录/付费处停下
//     等用户，结束输出状态 + 当前页（URL/AXTree/截图）。
//  2. 义眼模式：显式给视觉输入（--img 对图 / --url 站点截图 / --page 当前页 / --diff 等）
//     → 视觉模型作为体验评审者返回判断（是否一致/满足需求/差异），不执行页面动作。
//     `webtouch chat "判断两个实现是否一致" --img a.png --img b.png`
//     `webtouch chat "对比这两个站点主屏" --url https://a.example --url https://b.example`
//     `webtouch -p "两个实现是否一致" --img a.png --img b.png   # -p 是 chat 的顶层别名
//
// 用法：
//
//	webtouch "发布微博：今天天气不错"                 # 目标模式（最短写法，等价 webtouch chat/uws -p）
//	webtouch chat "把 a.com 的标题改成 xxx 后截图给我看"
//	webtouch chat "判断两个实现是否一致" --img ref.png --img clone.png
//	webtouch -p "参考站 vs 我们的实现，按钮位置是否一致" --img ref.png --img clone.png --json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"uws/pkg/engine"
	"uws/pkg/vlm"
	uwspb "uws/proto/gen/go/proto/v1"
)

// chatOptions 是 webtouch chat 的解析后参数。
type chatOptions struct {
	question    string
	imageSrcs   []string // --img（本地路径/URL，runChat 里再 chatLoadImage）
	urls        []string // --url（站点截图）
	usePage     bool     // 不传图时用当前引擎会话截图
	plain       bool     // 输出自由文本（--plain）
	diff        bool     // --diff：对 ≥2 张图做像素 diff，差异图+统计作为证据
	zoom        string   // --zoom "x,y,w,h[,scale]"：对第一张图区域放大
	regionColor string   // --region-color "x,y,w,h"：对第一张图区域取平均色
	model       string
	baseURL     string
	timeout     time.Duration
	engineAddr  string
	engineKey   string
	detail      string
	outDir      string // --out-dir：把收集到的图（站点截图/证据图）落盘，便于人工核对与后续迭代
	vpW, vpH    int    // --viewport WxH：--url 开站点截图时的视口（默认交给引擎 1920x1080）
	debug       bool   // --debug：目标模式逐轮打印 VLM 中间过程（委派 goal --debug）
}

// runChat 处理 `webtouch chat ...` 与顶层 `-p/--prompt ...`。
func runChat(args []string) error {
	opts, err := parseChatArgs(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout+vlm.DefaultEyeTimeout)
	defer cancel()

	// 自然语言自决（2026-08-28 用户明确）：调用方未显式给视觉输入，但问题自带 ≥2 个站点 URL
	// 且意图是「比较/查差异」时，自动抽取并转为站点采样。这样用户直接问
	// "帮我检查 a.com 和 b.com 的 differences" 也能走完整证据流程，无需知道 --url/--diff。
	applyNaturalLangRoute(&opts)

	// 默认（推荐）路径：目标模式。只有显式给了视觉输入/证据原语才走义眼。
	if !eyeRequested(opts) {
		return runGoalMode(opts)
	}

	// 收集图片：--img + --url(截图) + 可选当前会话截图。
	// 加载 --img（本地读盘 / URL 透传）；失败即中止，尽早暴露路径问题。
	var imgs []vlm.EyeImage
	for _, src := range opts.imageSrcs {
		img, err := chatLoadImage(src)
		if err != nil {
			return err
		}
		imgs = append(imgs, img)
	}
	if len(opts.urls) > 0 {
		shots, err := chatCaptureURLs(ctx, opts)
		if err != nil {
			return err
		}
		imgs = append(imgs, shots...)
		// sim 引擎对两个不同 URL 可能返回相同的模拟截图 → 让 VLM 别误判「一致」。
		if msg, ok := identicalShotsWarning(opts.urls, shots); ok {
			if opts.question == "" {
				opts.question = msg
			} else {
				opts.question = opts.question + "\n\n" + msg
			}
		}
	}
	if opts.usePage {
		shot, err := chatCapturePage(ctx, opts)
		if err != nil {
			return err
		}
		imgs = append(imgs, shot)
	}
	if len(imgs) == 0 {
		return errors.New("chat: 未提供任何视觉输入（用 --img 给图，--url 给站点，或不传参数让 webtouch 截当前页面）")
	}

	// 自然语言自决：对比类问题自动启用像素 diff 证据（确定性差异图+统计），不依赖显式 --diff。
	// 仅当前两张都有字节数据（本地/站点截图）才启用；远程 URL 的 --img 无法做本地像素 diff。
	if !opts.diff && len(imgs) >= 2 && detectCompareIntent(opts.question) &&
		len(imgs[0].Data) > 0 && len(imgs[1].Data) > 0 {
		opts.diff = true
	}

	// 证据编排：让 VLM 不只是裸眼看图，而是先拿到「像素/区域证据」再下结论。
	evidenceText, err := prepareEvidence(opts, &imgs)
	if err != nil {
		return err
	}
	question := opts.question
	if evidenceText != "" {
		if question == "" {
			question = evidenceText
		} else {
			question = question + "\n\n" + evidenceText
		}
	}
	// 落盘证据：把收集到的每一张字节图（站点截图 + 像素/区域证据图）写到 --out-dir，
	// 便于人工核对与后续迭代（复刻/评审场景尤其需要拿到证据原图）。
	if opts.outDir != "" {
		if err := saveEyeImages(opts.outDir, imgs); err != nil {
			return err
		}
	}

	client := vlm.NewEye(vlm.Config{
		BaseURL: opts.baseURL,
		Model:   opts.model,
		APIKey:  vlmAPIKey(),
		Timeout: opts.timeout,
	})
	if opts.plain {
		// 直接展示模型原始文本（自然语言回答）。
		text, err := client.Ask(ctx, vlm.EyeRequest{
			Question: question,
			Images:   imgs,
			Detail:   opts.detail,
			JSON:     false,
		})
		if err != nil {
			return err
		}
		fmt.Println(text)
		return nil
	}
	// 默认走义眼主控循环：VLM 按任务自决是否要多轮证据（优先单轮 conclude），
	// 返回给 Agent 消费的结构化 EyeResult；任何失败内部已降级为安全 unknown。
	result, err := eyeMainControl(ctx, client, opts, &imgs, question)
	if err != nil {
		return err
	}
	return printEyeResult(result)
}

// eyeRequested 判断 chat 是否义眼模式（显式视觉输入/证据原语）；否则为目标模式。
func eyeRequested(opts chatOptions) bool {
	return len(opts.imageSrcs) > 0 ||
		len(opts.urls) > 0 ||
		opts.usePage ||
		opts.diff ||
		opts.zoom != "" ||
		opts.regionColor != ""
}

// runGoalMode 把目标文本委派给 Python 协调器 goal（DSL/护栏/证据单一实现）；
// 正常退出码直通，登录等待/护栏暂停以退出码 2 表达。
func runGoalMode(opts chatOptions) error {
	if err := ensureEngine(opts.engineAddr); err != nil {
		return err
	}
	args := []string{
		"goal",
		"--prompt", opts.question,
		"--engine-addr", opts.engineAddr,
		"--api-key", opts.engineKey,
	}
	if opts.outDir != "" {
		args = append(args, "--out-dir", opts.outDir)
	}
	if opts.debug {
		args = append(args, "--debug")
	}
	if err := execCoordinator(args); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// saveEyeImages 把 imgs 中带字节数据的图片写入 outDir（自动创建目录）。
// 文件名用「序号-标签」安全化；仅保留本地字节图（远程 URL 透传的无 Data 跳过）。
// 返回写盘数量；用于让 `webtouch chat --out-dir` 的证据可回看。
func saveEyeImages(outDir string, imgs []vlm.EyeImage) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("chat: 创建证据目录 %q: %w", outDir, err)
	}
	wrote := 0
	for i, img := range imgs {
		if len(img.Data) == 0 {
			continue
		}
		label := img.Label
		if label == "" {
			label = fmt.Sprintf("图%d", i+1)
		}
		// 标签安全化：去除路径分隔与空白，保留数字/字母/中文/常见符号。
		safe := safeLabel(label)
		name := fmt.Sprintf("%02d-%s.png", i+1, safe)
		if err := os.WriteFile(filepath.Join(outDir, name), img.Data, 0o644); err != nil {
			return fmt.Errorf("chat: 写证据图 %q: %w", name, err)
		}
		wrote++
	}
	if wrote == 0 {
		return fmt.Errorf("chat: --out-dir 未找到任何本地字节图（远程 URL --img 不落盘），请改用 --url 支持站点截图")
	}
	return nil
}

// safeLabel 把图标签处理为安全文件名片段（仅允许数字/字母/中文/'-'/'_'/'.'）。
func safeLabel(s string) string {
	var b strings.Builder
	for _, r := range []rune(s) {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= 0x4e00 && r <= 0x9fff, r == '-', r == '_', r == '.', r == '(' || r == ')' ||
				r == ',' || r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.TrimSpace(b.String())
}

// parseChatArgs 解析 chat 子命令参数（手动解析，避免与顶层 -p/--prompt 冲突）。
func parseChatArgs(args []string) (chatOptions, error) {
	opts := chatOptions{
		model:      vlm.DefaultModel,
		baseURL:    vlm.DefaultBaseURL,
		timeout:    vlm.DefaultEyeTimeout,
		engineAddr: "127.0.0.1:50051",
		engineKey:  engine.DevAPIKey,
		detail:     "low",
	}
	if v := os.Getenv("UWS_ENGINE_ADDR"); v != "" {
		opts.engineAddr = v
	}
	var questionParts []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			opts.plain = false
		case a == "--plain":
			opts.plain = true
		case a == "--img" || a == "-i":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --img 需要一个参数（本地路径或图片 URL）")
			}
			i++
			opts.imageSrcs = append(opts.imageSrcs, args[i])
		case a == "--url" || a == "-u":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --url 需要一个参数（站点 URL）")
			}
			i++
			opts.urls = append(opts.urls, args[i])
		case a == "--page" || a == "-p":
			opts.usePage = true
		case a == "--diff" || a == "-d":
			opts.diff = true
		case a == "--zoom":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --zoom 需要一个参数（x,y,w,h[,scale]）")
			}
			i++
			opts.zoom = args[i]
		case a == "--region-color":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --region-color 需要一个参数（x,y,w,h）")
			}
			i++
			opts.regionColor = args[i]
		case a == "--model" || a == "-m":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --model 需要一个参数")
			}
			i++
			opts.model = args[i]
		case a == "--base-url":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --base-url 需要一个参数")
			}
			i++
			opts.baseURL = args[i]
		case a == "--timeout":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --timeout 需要一个秒数参数")
			}
			i++
			var secs float64
			if _, err := fmt.Sscanf(args[i], "%f", &secs); err != nil || secs <= 0 {
				return opts, errors.New("chat: --timeout 需为正数（秒）")
			}
			opts.timeout = time.Duration(secs * float64(time.Second))
		case a == "--detail":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --detail 需要一个参数（low|high|original）")
			}
			i++
			opts.detail = args[i]
		case a == "--out-dir":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --out-dir 需要一个目录参数")
			}
			i++
			opts.outDir = args[i]
		case a == "--debug":
			opts.debug = true
		case a == "--viewport":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --viewport 需要一个 WxH 参数（如 390x844）")
			}
			i++
			w, h, err := parseViewport(args[i])
			if err != nil {
				return opts, err
			}
			opts.vpW, opts.vpH = w, h
		case a == "--engine-addr":
			if i+1 >= len(args) {
				return opts, errors.New("chat: --engine-addr 需要一个参数")
			}
			i++
			opts.engineAddr = args[i]
		case strings.HasPrefix(a, "-"):
			return opts, fmt.Errorf("chat: 未知参数 %q（支持 --img/--url/--page/--json/--plain/--model/--timeout/--debug 等）", a)
		default:
			// 非 flag 视为问题的一部分；多个分词拼接（如直接 `webtouch chat "a" "b"`）。
			if a != "-" {
				questionParts = append(questionParts, a)
			}
		}
	}
	opts.question = strings.Join(questionParts, " ")
	if strings.TrimSpace(opts.question) == "" && len(opts.imageSrcs) == 0 && len(opts.urls) == 0 && !opts.usePage {
		return opts, errors.New("chat: 请给出问题（如 `webtouch chat \"判断两个实现是否一致\" --img a.png --img b.png`）")
	}
	return opts, nil
}

// parseViewport 解析 "WxH"（如 390x844 或 1280x800），返回宽/高。
func parseViewport(s string) (int, int, error) {
	parts := strings.SplitN(strings.TrimSpace(s), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("chat: --viewport 格式应为 WxH（如 390x844），收到 %q", s)
	}
	var w, h int
	if _, err := fmt.Sscanf(parts[0], "%d", &w); err != nil || w <= 0 {
		return 0, 0, fmt.Errorf("chat: --viewport 宽度 %q 非法", parts[0])
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &h); err != nil || h <= 0 {
		return 0, 0, fmt.Errorf("chat: --viewport 高度 %q 非法", parts[1])
	}
	return w, h, nil
}

// chatLoadImage 加载 --img：本地文件读 bytes，URL 原样透传。
func chatLoadImage(src string) (vlm.EyeImage, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return vlm.EyeImage{}, errors.New("chat: 图片源为空")
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return vlm.EyeImage{Label: src, URL: src}, nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return vlm.EyeImage{}, fmt.Errorf("chat: 读取图片 %q: %w", src, err)
	}
	return vlm.EyeImage{Label: src, Data: data}, nil
}

// chatSettle 依据「问题（提示词）」决定站点截图前的稳定等待时长，避免 --url 采样的页面未
// 渲染完就被截（P1：详情/视频/剧集等重内容易空白/黑屏）。
//
// 优先级（提示词驱动，不加 --settle-ms CLI flag）：
//  1. 提示词显式给出时长："等 2 秒" / "wait 1.5s" / "settle 2" → 用显式值（上限 30s）；
//  2. 提示词含重内容信号（详情/视频/播放/剧集/加载/评论/detail/video/player/loading…）→ 1.2s；
//  3. 默认 600ms。
func chatSettle(question string) time.Duration {
	q := strings.ToLower(strings.TrimSpace(question))
	if m := settleRe.FindStringSubmatch(q); m != nil {
		var secs float64
		if _, err := fmt.Sscanf(m[1], "%f", &secs); err == nil && secs > 0 && secs <= 30 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	for _, kw := range settleHeavyKeywords {
		if strings.Contains(q, kw) {
			return 1200 * time.Millisecond
		}
	}
	return 600 * time.Millisecond
}

// settleRe 匹配提示词里的显式等待时长（"等 2 秒"/"wait 1.5s"/"settle 2"）。
var settleRe = regexp.MustCompile(`(?:等\s*|wait\s*|settle\s*)(\d+(?:\.\d+)?)(?:\s*(?:秒|s|sec|second))?`)

// settleHeavyKeywords 提示词里的「重内容」信号，命中即延长稳定等待。
var settleHeavyKeywords = []string{
	"详情", "视频", "播放", "剧集", "加载", "评论区", "详情页", "剧列表",
	"detail", "video", "player", "play", "loading", "episode", "chapter", "album",
	"waterfall", "lazy",
}

// chatCaptureURLs 经引擎打开 --url 站点并截图（每站点一个独立命名上下文，避免串 Cookie）。
func chatCaptureURLs(ctx context.Context, opts chatOptions) ([]vlm.EyeImage, error) {
	conn, err := dialEngine(ctx, opts.engineAddr)
	if err != nil {
		return nil, fmt.Errorf("chat: 打开站点需要引擎在运行（%s，先 `webtouch daemon start`）; 失败: %w", opts.engineAddr, err)
	}
	defer conn.Close()
	stub := uwspb.NewUwsEngineClient(conn)
	authCtx := metadata.AppendToOutgoingContext(ctx, "x-api-key", opts.engineKey)
	var out []vlm.EyeImage
	for i, u := range opts.urls {
		ctxName := fmt.Sprintf("eye-%d-%d", time.Now().UnixNano(), i)
		init, err := stub.InitializeContext(authCtx, &uwspb.InitContextRequest{
			TaskId:      ctxName,
			ContextName: &ctxName,
			InitialUrl:  &u,
			Viewport:    chatViewport(opts),
		})
		if err != nil {
			return nil, fmt.Errorf("chat: 打开 %q: %w", u, err)
		}
		// 稳定等待：让导航/渲染有一帧稳定时间（引擎本身有稳定性等待，此处兜底）。
		// P1（2026-08-28）：时长由「问题（提示词）」驱动——用户可在提示词里显式
		// "等 N 秒"/"settle N"，或含重内容信号（详情/视频/播放…）时自动延长，避免
		// --url 采样详情/视频等重页面在未渲染完就截图（黑屏/空白）。未加 --settle-ms
		// CLI flag：按用户约定优先让提示词指导，而非暴露参数。
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(chatSettle(opts.question)):
		}
		shot, err := stub.CaptureScreenshot(authCtx, &uwspb.CaptureScreenshotRequest{TargetId: init.GetTargetId()})
		if err != nil {
			_, _ = stub.CloseContext(authCtx, &uwspb.CloseContextRequest{ContextName: ctxName})
			return nil, fmt.Errorf("chat: 截图 %q: %w", u, err)
		}
		_, _ = stub.CloseContext(authCtx, &uwspb.CloseContextRequest{ContextName: ctxName})
		out = append(out, vlm.EyeImage{Label: u, Data: shot.GetPng()})
	}
	return out, nil
}

// chatViewport 依据 --viewport 构造引擎视口；未给则用 nil 交由引擎默认（1920x1080）。
func chatViewport(opts chatOptions) *uwspb.ViewportConfig {
	if opts.vpW > 0 && opts.vpH > 0 {
		return &uwspb.ViewportConfig{Width: int32(opts.vpW), Height: int32(opts.vpH)}
	}
	return nil
}

// chatCapturePage 经引擎截当前（默认）会话页面；无启动会话时报错。
func chatCapturePage(ctx context.Context, opts chatOptions) (vlm.EyeImage, error) {
	conn, err := dialEngine(ctx, opts.engineAddr)
	if err != nil {
		return vlm.EyeImage{}, fmt.Errorf("chat: 截取页面需要引擎在运行（%s，先 `webtouch daemon start`）; 失败: %w", opts.engineAddr, err)
	}
	defer conn.Close()
	stub := uwspb.NewUwsEngineClient(conn)
	authCtx := metadata.AppendToOutgoingContext(ctx, "x-api-key", opts.engineKey)
	shot, err := stub.CaptureScreenshot(authCtx, &uwspb.CaptureScreenshotRequest{})
	if err != nil {
		return vlm.EyeImage{}, fmt.Errorf("chat: 截图当前页面失败: %w", err)
	}
	return vlm.EyeImage{Label: "当前页面", Data: shot.GetPng()}, nil
}

// dialEngine 建立到引擎的明文 gRPC 连接。
func dialEngine(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	return grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
}

// vlmAPIKey 读取义眼 API Key（与引擎 VLM 兜底同一来源）。
func vlmAPIKey() string {
	if v := os.Getenv("UWS_VLM_API_KEY"); v != "" {
		return v
	}
	return os.Getenv("DEEPSEEK_API_KEY")
}

// printEyeResult 打印结构化 EyeResult（给 Agent 消费的 JSON）。
func printEyeResult(r vlm.EyeResult) error {
	return printJSON(r)
}

// printJSON 输出结构化 JSON（缩进、不转义 HTML）。
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// prepareEvidence 处理 --diff / --zoom / --region-color，把「像素/区域证据」注入 images 与问题，
// 返回拼进问题末尾的证据文本（可空）。
func prepareEvidence(opts chatOptions, imgs *[]vlm.EyeImage) (string, error) {
	var text strings.Builder
	// 1) --diff：两张及以上图做逐像素对比，差异图追加为证据图。
	if opts.diff {
		if len(*imgs) < 2 {
			return "", errors.New("chat: --diff 需要至少两张图（--img 两次 或 --img + --url）")
		}
		diffPng, stats, err := vlm.DiffImages((*imgs)[0].Data, (*imgs)[1].Data)
		if err != nil {
			return "", fmt.Errorf("chat: 像素 diff: %w", err)
		}
		*imgs = append(*imgs, vlm.EyeImage{Label: "像素差异图", Data: diffPng})
		fmt.Fprintf(&text, "【像素差异证据】两图逐像素对比：差异像素 %d/%d（%.1f%%），最大信道差 %d，差异区域 bbox=%s。最后一张「像素差异图」中洋红=差异像素、灰底=第二张。请结合差异图给出量化判据。",
			stats.DiffPixels, stats.TotalPixels, stats.Ratio*100, stats.MaxChannel, orDash(stats.BBox))
	}
	// 2) --zoom：对第一张图指定区域放大，替换/新增证据图。
	if opts.zoom != "" {
		if len(*imgs) == 0 {
			return "", errors.New("chat: --zoom 需要至少一张图")
		}
		x, y, rw, rh, scale, err := parseRect(opts.zoom)
		if err != nil {
			return "", err
		}
		zoomPng, err := vlm.ZoomImage((*imgs)[0].Data, x, y, rw, rh, scale)
		if err != nil {
			return "", fmt.Errorf("chat: 放大区域: %w", err)
		}
		*imgs = append(*imgs, vlm.EyeImage{Label: fmt.Sprintf("区域放大(%d,%d %dx%d x%d)", x, y, rw, rh, scale), Data: zoomPng})
		fmt.Fprintf(&text, "【区域放大证据】已将第一张图的区域 [%d,%d 起点，%dx%d，放大 %d 倍] 追加为证据图，请细看该区域细节。", x, y, rw, rh, scale)
	}
	// 3) --region-color：对第一张图指定区域取平均色。
	if opts.regionColor != "" {
		if len(*imgs) == 0 {
			return "", errors.New("chat: --region-color 需要至少一张图")
		}
		x, y, rw, rh, _, err := parseRect(opts.regionColor)
		if err != nil {
			return "", err
		}
		avg, aerr := vlm.RegionAvgColor((*imgs)[0].Data, x, y, rw, rh)
		if aerr != nil {
			return "", fmt.Errorf("chat: 区域取色: %w", aerr)
		}
		fmt.Fprintf(&text, "【区域颜色证据】第一张图区域 [%d,%d %dx%d] 的平均色为 RGB(%s)。", x, y, rw, rh, avg)
	}
	return text.String(), nil
}

// parseRect 解析 "x,y,w,h" 或 "x,y,w,h,scale"（坐标/尺寸/放大倍数）。
func parseRect(s string) (x, y, rw, rh, scale int, err error) {
	parts := strings.SplitN(s, ",", 5)
	if len(parts) < 4 {
		return 0, 0, 0, 0, 1, fmt.Errorf("chat: 区域格式应为 x,y,w,h 或 x,y,w,h,scale，收到 %q", s)
	}
	vals := make([]int, 0, 5)
	for _, p := range parts {
		var v int
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%d", &v); err != nil {
			return 0, 0, 0, 0, 1, fmt.Errorf("chat: 区域参数 %q 非整数", p)
		}
		vals = append(vals, v)
	}
	x, y, rw, rh = vals[0], vals[1], vals[2], vals[3]
	scale = 1
	if len(vals) == 5 {
		scale = vals[4]
	}
	return x, y, rw, rh, scale, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- 自然语言自决（2026-08-28）----

// applyNaturalLangRoute 把自然语言里 ≥2 个站点 URL 且带比较意图的问题，自动路由为 opts.urls
// 站点采样。仅在调用方未显式给视觉输入时触发，避免误开引擎。返回是否发生了路由。
func applyNaturalLangRoute(opts *chatOptions) bool {
	if len(opts.urls) > 0 || len(opts.imageSrcs) > 0 || opts.usePage {
		return false
	}
	urls := extractSiteURLs(opts.question)
	if len(urls) >= 2 && detectCompareIntent(opts.question) {
		opts.urls = urls
		return true
	}
	return false
}

// extractSiteURLs 从自然语言问题中抽取站点 URL：优先完整 http(s) 地址，其次裸域名。
// 裸域名自动补 https:// 前缀，便于引擎 InitializeContext 直接导航。返回去重后的 URL 列表。
func extractSiteURLs(question string) []string {
	added := map[string]bool{}
	var out []string
	add := func(u string) {
		u = strings.TrimRight(u, ".。，,;；'\"")
		if u == "" {
			return
		}
		if !strings.Contains(u, "://") {
			u = "https://" + u
		}
		if added[u] {
			return
		}
		added[u] = true
		out = append(out, u)
	}
	for _, m := range schemeURLRe.FindAllString(question, -1) {
		add(m)
	}
	for _, m := range bareDomainRe.FindAllString(question, -1) {
		m = strings.TrimRight(m, ".。，,;；'\"")
		covered := false
		for _, u := range out {
			// 裸域名已被完整 URL 覆盖（如 https://a.com 里的 a.com），跳过避免重复。
			if strings.Contains(u, m) {
				covered = true
				break
			}
		}
		if !covered {
			add(m)
		}
	}
	return out
}

// detectCompareIntent 判断问题是否含比较/差异/一致性类意图（仅作自决门控，不裁剪问题）。
func detectCompareIntent(question string) bool {
	return compareIntentRe.MatchString(question)
}

var (
	// schemeURLRe 匹配完整 http(s) URL。
	schemeURLRe = regexp.MustCompile(`https?://[^\s,，。;；()（）\[\]{}"'<>]+`)
	// bareDomainRe 匹配裸域名（用于「对比 a.com 和 b.com」这类无 scheme 输入）。
	bareDomainRe = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|io|cn|dev|app|ai|co|me|cc|info|biz|xyz|site|top|store)\b`)
	// compareIntentRe 比较/差异/一致性意图关键词。
	compareIntentRe = regexp.MustCompile(`(?i)对比|比较|差异|区别|不同|相同|一致|一样|相差|差别|对照|diff|difference|compare|comparison|identical|consistent|alike|same|\bvs\.?\b|\bversus\b`)
)

// identicalShotsWarning 检测多站点截图是否「几乎相同」（sim 引擎不真实导航的假阳性）：
// 相邻两张 diff 占比 < 0.1% 时，返回给 VLM 的警示文本。调用方把它拼进问题，避免模型
// 把「截图相同」当「站点一致」。
func identicalShotsWarning(urls []string, shots []vlm.EyeImage) (string, bool) {
	if len(urls) < 2 || len(shots) < 2 {
		return "", false
	}
	// chatCaptureURLs 按序 append：shots[i].Data 即 urls[i] 的截图。
	if len(shots[0].Data) == 0 || len(shots[1].Data) == 0 {
		return "", false
	}
	_, stats, err := vlm.DiffImages(shots[0].Data, shots[1].Data)
	if err != nil {
		return "", false
	}
	if stats.Ratio < 0.001 {
		uA, uB := urls[0], urls[1]
		msg := fmt.Sprintf(
			"【注意】站点“%s”与“%s”的截图像素几乎相同（差异 <0.1%%），可能是浏览器未真实导航（sim 模式/加载失败）。请**不要**据此判定两站一致，若无法确认请给 verdict=unknown 并说明原因。", uA, uB)
		return msg, true
	}
	return "", false
}
