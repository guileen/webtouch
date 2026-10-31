// Command uws-engine 是 UWS 在线执行引擎守护进程入口。
// 启动 proto/uws_service.proto 定义的 UwsEngine gRPC 服务；
// 接线 pkg/engine（鉴权 + 生命周期 + 幂等 + 知识库热更 + 健康/版本）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"uws/pkg/browser"
	"uws/pkg/engine"
	"uws/pkg/fingerprint"
	"uws/pkg/knowledge"
	"uws/pkg/viewerhub"
	"uws/pkg/vlm"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc"
)

// viewerSocketDisabled 识别「显式关闭推流」的哨兵值（与 cmd/uws 同一约定）。
// defaultSessionStorePath 是会话续页绑定的默认落盘位置（与引擎其它状态同目录）。
func defaultSessionStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".uws", "sessions.json")
}

func viewerSocketDisabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off", "none", "no", "false", "-":
		return true
	default:
		return false
	}
}

// learnPathDisabled 识别「显式关闭 VLM 自进化」的哨兵值（与 cmd/uws 侧 UWS_LEARN=off
// 同一约定）；空串由调用方判空，不在此列。
func learnPathDisabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off", "none", "no", "false", "0", "-":
		return true
	default:
		return false
	}
}

func main() {
	grpcAddr := flag.String("grpc-addr", "127.0.0.1:50051", "gRPC 监听地址（默认 127.0.0.1:50051）")
	debugPort := flag.Int("debug-port", 8080, "调试 UI 端口（M6A 接入，当前仅占位）")
	maxTargets := flag.Int("max-targets", 0, "Target 池上限；0=CPU×1.5")
	apiKeyFile := flag.String("api-key-file", "", `API Key JSON：{"keys":[{"key":"...","role":"coordinator|operator|monitor"}]}`)
	tlsCert := flag.String("tls-cert", "", "mTLS 服务端证书路径（与 tls-key 同时指定即开启 TLS）")
	tlsKey := flag.String("tls-key", "", "mTLS 服务端私钥路径")
	tlsCA := flag.String("tls-ca", "", "mTLS 客户端 CA 证书路径（指定后强制客户端证书）")
	knowledgeBaseURL := flag.String("knowledge-base-url", "",
		"知识库 artifact 的 HTTP(S) 基础 URL（如 http://127.0.0.1:8000/artifacts；不配置则用内置内存知识库）")
	knowledgeVersion := flag.String("knowledge-version", engine.EngineVersion,
		"启动时加载的知识库版本（配合 --knowledge-base-url 使用）")
	logPath := flag.String("log-path", "",
		"online_logs SQLite 落库路径（algorithm-kernel.md §4.4；空 = 不记录动作日志）")
	episodeDir := flag.String("episode-dir", "",
		"在线 Episode 的 append-only JSONL 落盘目录（algorithm-kernel.md §5.0.12；空 = 不采集）")
	browserMode := flag.String("browser", "sim",
		`物理执行层：sim = 模拟 CDP 会话（默认）；cdp = 真实 Chrome（启动或 --attach 附着）`)
	chromePath := flag.String("chrome-path", "", "真实 Chrome 可执行文件路径（--browser=cdp；空 = 自动探测）")
	cdpPort := flag.Int("cdp-port", 0, "启动模式 Chrome 调试端口（0 = 自动挑选空闲端口）")
	headed := flag.Bool("headed", false, "启动模式使用有头 Chrome（默认 headless）")
	probeWasm := flag.String("probe-wasm", "",
		"WASM 探针 .wasm 路径（--browser=cdp；空 = 自动探测 build/probe_wasm_wasm/probe_wasm.wasm，缺失时页面候选降级为 JS 文本采集）")
	onnxRuntimeLib := flag.String("onnx-runtime-lib", "",
		"libonnxruntime 共享库路径（A 案 ort-go CGO；空 = 不启用真实 ONNX，决策核降级 mock）")
	shadowOnnxModel := flag.String("shadow-onnx-model", "",
		"只读影子通道的 ONNX 模型文件路径（spec §5.0.14 条款 3；共享库复用 --onnx-runtime-lib；空 = 影子未接线）")
	shadowOnnxModelVersion := flag.String("shadow-onnx-model-version", "",
		"影子模型版本串（空 = 取模型文件名；只作 shadow_inference 溯源，不改任何判定）")
	vlmFallback := flag.Bool("vlm-fallback", true,
		"VLM 视觉感知兜底（默认开启：语义/选择器失败时截图给视觉模型重新决策；--vlm-fallback=false 可关闭；API Key 自动读 UWS_VLM_API_KEY || DEEPSEEK_API_KEY）")
	vlmModel := flag.String("vlm-model", vlm.DefaultModel, "VLM 模型名（默认 deepseek-v4-flash-vision-exp）")
	vlmBaseURL := flag.String("vlm-base-url", vlm.DefaultBaseURL, "VLM OpenAI 兼容基底 URL")
	vlmTimeoutMs := flag.Int("vlm-timeout-ms", int(vlm.DefaultTimeout/time.Millisecond), "单次 VLM 调用超时（毫秒）")
	vlmLearnPath := flag.String("vlm-learn-path", "",
		"VLM 自进化 learned 存储 JSON 路径（空/off/none/false/no/0/- = 不持久化学习）")
	vlmRefine := flag.Bool("vlm-refine", true,
		"VLM 兜底启用「先粗后精」针对性截图/缩放（全页粗定位 → 裁剪区域 → 放大精修坐标）")
	vlmMaxRounds := flag.Int("vlm-max-rounds", vlm.DefaultMaxRounds,
		"VLM 主控会话单次轮数上限（crop_zoom/back 循环上限，防死循环）")
	viewerSocket := flag.String("viewer-socket", os.Getenv("UWS_VIEWER_SOCKET"),
		"观看小窗 Unix socket 路径（空/off/none/no/false/- = 不推流，零开销）")
	leaseRoot := flag.String("lease-root", "", "本地/共享 POSIX 租约目录（与 --lease-endpoint 二选一）")
	leaseEndpoint := flag.String("lease-endpoint", "", "远程 HTTPS 租约服务地址（与 --lease-root 二选一）")
	leaseTokenFile := flag.String("lease-token-file", "", "远程租约服务 bearer token 文件")
	leaseHolderID := flag.String("lease-holder-id", "", "本引擎节点的稳定唯一标识")
	leaseTTL := flag.Duration("lease-ttl", 60*time.Second, "Profile Lease TTL")
	sessionStorePath := flag.String("session-store", defaultSessionStorePath(),
		"会话续页绑定落盘路径（默认 ~/.uws/sessions.json）：同一 Session ID 在引擎重启后接管原窗口；空 = 不持久化")
	sessionTTL := flag.Duration("session-ttl", engine.DefaultSessionTTL,
		"会话绑定的有效期（默认 24h，0 = 永不过期）：过期只是不再接管那扇窗，**不关窗**（用户可能还要看）")
	flag.Parse()

	// 观看小窗的「显式关闭」哨兵（UWS_VIEWER_SOCKET=off/空/none/no/false/-）：
	// 归一化成空路径 = 不监听、不推流，而不是真去 bind 一个叫 "off" 的 socket。
	if viewerSocketDisabled(*viewerSocket) {
		*viewerSocket = ""
	}

	keys := []engine.APIKey{{Key: engine.DevAPIKey, Role: engine.RoleCoordinator}}
	if *apiKeyFile != "" {
		loaded, err := engine.LoadKeyFile(*apiKeyFile)
		if err != nil {
			log.Fatalf("加载 API Key 文件失败: %v", err)
		}
		keys = loaded
	} else {
		log.Printf("警告: 未提供 --api-key-file，使用开发内置 Key %q；生产环境必须配置", engine.DevAPIKey)
	}

	engineOpts := engine.Options{
		Keys:           keys,
		MaxTargets:     *maxTargets,
		LogPath:        *logPath,
		EpisodeDir:     *episodeDir,
		OnnxRuntimeLib: *onnxRuntimeLib,
		// 只读影子通道（§5.0.14 条款 3）：独立入口，绝不并入决策 scorer。
		ShadowOnnxModelPath:    *shadowOnnxModel,
		ShadowOnnxModelVersion: *shadowOnnxModelVersion,
	}
	// 会话续页（v1.5.7，GLOBAL #118）：绑定落盘，引擎重启后同名会话接管原窗口。
	// 存储打不开（坏文件/权限）只降级为「不持久化」，绝不让引擎起不来。
	if store, err := engine.OpenSessionStore(*sessionStorePath); err != nil {
		log.Printf("警告: 会话续页绑定不可用（%v）；同一会话在引擎重启后会新开窗口", err)
		engineOpts.Sessions = engine.NewSessionStoreInMemory()
	} else if store != nil {
		store.SetTTL(*sessionTTL)
		engineOpts.Sessions = store
		log.Printf("会话续页已启用: %s（绑定有效期 %s）", store.Path(), *sessionTTL)
	}
	if *leaseRoot != "" && *leaseEndpoint != "" {
		log.Fatal("--lease-root 与 --lease-endpoint 不能同时使用")
	}
	if *leaseRoot != "" || *leaseEndpoint != "" {
		if *leaseHolderID == "" {
			log.Fatal("启用 Profile Lease 必须提供 --lease-holder-id")
		}
		var backend fingerprint.LeaseBackend
		var err error
		if *leaseRoot != "" {
			backend, err = fingerprint.OpenLeaseStore(*leaseRoot)
			if err == nil {
				log.Printf("Profile Lease 已启用：共享文件系统 root=%s", *leaseRoot)
			}
		} else {
			if *leaseTokenFile == "" {
				log.Fatal("远程 Profile Lease 必须提供 --lease-token-file")
			}
			token, readErr := os.ReadFile(*leaseTokenFile)
			if readErr != nil {
				log.Fatalf("读取租约 token 文件失败: %v", readErr)
			}
			backend, err = fingerprint.NewHTTPLeaseBackend(*leaseEndpoint, strings.TrimSpace(string(token)), nil)
			if err == nil {
				log.Printf("Profile Lease 已启用：远程 endpoint=%s", *leaseEndpoint)
			}
		}
		if err != nil {
			log.Fatalf("初始化 Profile Lease 失败: %v", err)
		}
		engineOpts.LeaseStore = backend
		engineOpts.LeaseHolderID = *leaseHolderID
		engineOpts.LeaseTTL = *leaseTTL
	}
	if *onnxRuntimeLib != "" {
		log.Printf("真实 ONNX 已启用（ort-go CGO）: lib=%s", *onnxRuntimeLib)
	}
	if *vlmFallback {
		key := vlmAPIKey()
		if key == "" {
			log.Printf("警告: VLM 兜底默认开启，但 UWS_VLM_API_KEY / DEEPSEEK_API_KEY 均未设置，VLM 兜底禁用")
		} else {
			vlmTimeout := time.Duration(*vlmTimeoutMs) * time.Millisecond
			fb := &vlm.Fallback{
				Enabled: true,
				Client: vlm.NewDeepSeek(vlm.Config{
					BaseURL: *vlmBaseURL, Model: *vlmModel, APIKey: key, Timeout: vlmTimeout,
				}),
				Timeout:   vlmTimeout,
				Refine:    *vlmRefine,
				MaxRounds: *vlmMaxRounds,
			}
			if p := strings.TrimSpace(*vlmLearnPath); p != "" && !learnPathDisabled(p) {
				ls, err := vlm.NewLearnedStore(p)
				if err != nil {
					log.Fatalf("加载 VLM learned 存储失败: %v", err)
				}
				fb.Learned = ls
				log.Printf("VLM 自进化已启用: learn=%s", p)
			}
			engineOpts.VLM = fb
			log.Printf("VLM 感知兜底已启用: model=%s base=%s", *vlmModel, *vlmBaseURL)
		}
	}
	if *browserMode == "cdp" {
		engineOpts.SessionFactory = &browser.CDPFactory{
			ChromePath:    *chromePath,
			Port:          *cdpPort,
			Headless:      !*headed,
			ProbeWasmPath: *probeWasm,
		}
		log.Printf("物理执行层: 真实 CDP (chrome=%s port=%d headless=%v)",
			browser.ChromePath(), *cdpPort, !*headed)
	} else if *browserMode != "sim" {
		log.Fatalf("未知 --browser=%q（支持 sim|cdp）", *browserMode)
	}
	if *knowledgeBaseURL != "" {
		fetcher, err := knowledge.NewHTTPFetcher(*knowledgeBaseURL, nil)
		if err != nil {
			log.Fatalf("配置知识库 fetcher 失败: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		art, err := fetcher.Fetch(ctx, *knowledgeVersion, "")
		cancel()
		if err != nil {
			log.Fatalf("加载初始知识库 %s 失败: %v", *knowledgeVersion, err)
		}
		engineOpts.Knowledge = knowledge.NewStore(fetcher, art)
		log.Printf("知识库已加载: version=%s uri=%s onnx_ready=%v",
			art.Version, art.ArtifactURI, art.OnnxReady)
	}

	// 观看小窗推流（P2）：引擎作为 Unix socket 服务端，原生 Viewer 作为客户端。
	// 未配置路径时不监听、不推流；Viewer 不在线时发布是非阻塞的，绝不影响自动化。
	if *viewerSocket != "" {
		hub := viewerhub.NewHub(*viewerSocket, viewerhub.WithLogger(log.Printf))
		viewerCtx, stopViewer := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		go func() {
			if err := hub.Serve(viewerCtx); err != nil {
				log.Printf("观看小窗 socket 未启用: %v", err)
			}
		}()
		engineOpts.ViewerHub = hub
		defer func() {
			stopViewer()
			_ = hub.Close()
		}()
		log.Printf("观看小窗推流已启用: socket=%s", *viewerSocket)
	}

	srv, err := engine.NewServer(engineOpts)
	if err != nil {
		log.Fatalf("初始化引擎失败: %v", err)
	}
	defer srv.Close()

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", *grpcAddr, err)
	}

	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(srv.Auth().UnaryInterceptor),
		grpc.StreamInterceptor(srv.Auth().StreamInterceptor),
	}
	if *tlsCert != "" || *tlsKey != "" || *tlsCA != "" {
		creds, err := engine.ServerTLS(*tlsCert, *tlsKey, *tlsCA)
		if err != nil {
			log.Fatalf("配置 mTLS 失败: %v", err)
		}
		opts = append(opts, grpc.Creds(creds))
		log.Printf("mTLS 已启用（客户端证书强制=%v）", *tlsCA != "")
	}

	gs := grpc.NewServer(opts...)
	uwspb.RegisterUwsEngineServer(gs, srv)

	if *debugPort > 0 && *logPath != "" {
		debugSrv := engine.NewDebugServer(srv.LogStore())
		debugAddr := fmt.Sprintf("127.0.0.1:%d", *debugPort)
		if err := debugSrv.Start(debugAddr); err != nil {
			log.Printf("启动调试 HTTP 服务 (%s) 失败: %v", debugAddr, err)
		} else {
			log.Printf("调试 HTTP 服务已监听: http://%s/debug/task/{request_id}", debugAddr)
		}
	}

	log.Printf("uws-engine v%s 启动: grpc=%s debug-port=%d max-targets=%d",
		engine.EngineVersion, *grpcAddr, *debugPort, *maxTargets)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("gRPC 服务退出: %v", err)
	}
}
