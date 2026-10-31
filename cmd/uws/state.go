package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"uws/pkg/browser"
	"uws/pkg/engine"
	"uws/pkg/fingerprint"
	uwspb "uws/proto/gen/go/proto/v1"
)

// runState exposes the deliberate State Bundle boundary to operators. It never
// prints the bundle, Cookie values, or encryption material: those are session
// credentials, not diagnostics.
func runState(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(stateGuide())
		return nil
	}
	switch args[0] {
	case "export":
		return stateExport(args[1:])
	case "import":
		return stateImport(args[1:])
	case "restore":
		return stateRestore(args[1:])
	case "inspect":
		return stateInspect(args[1:])
	default:
		return fmt.Errorf("未知 state 子命令 %q（支持 export | import | inspect）", args[0])
	}
}

func stateGuide() string {
	return `webtouch state —— State Bundle 会话迁移

  webtouch state export <profile-id> <origin> --target-id <id> --out <bundle.json> [--key-file <32-byte-key>]
  webtouch state inspect <bundle.json> [--key-file <32-byte-key>]
  webtouch state import <bundle.json> --target-id <clean-id> [--key-file <32-byte-key>]
  webtouch state restore <bundle.json> --profile-file <FingerprintProfile.json> --session <new-session> [--key-file <32-byte-key>]

流程：源端登录完成后 export；把文件与密钥通过你认可的安全通道传到目标端；目标端以同一
FingerprintProfile 初始化一个尚未导航的干净上下文，然后 import，最后才导航到站点。restore
把“初始化 + 导入”合成一条命令；它默认拒绝已有数据的本地 Profile，随后用同一 --session
的 webtouch run/goal 继续。仅在你已核对旧数据时才可显式追加 --allow-existing-profile。

--key-file 是一个权限受控的文件，内容只能是 32 个原始字节或 64 位十六进制 AES-256 密钥。
不提供它会导出明文 JSON（仅适合受控的临时本地环境）。命令不会打印 Cookie、Bundle 内容或密钥。
引擎地址取 --engine-addr 或 UWS_ENGINE_ADDR；密钥取 UWS_ENGINE_API_KEY，未设置时仅使用本地开发默认值。
`
}

type stateRPCOptions struct {
	targetID             string
	engineAddr           string
	apiKey               string
	keyFile              string
	output               string
	profileFile          string
	session              string
	allowExistingProfile bool
}

func parseStateRPCFlags(name string, args []string) (stateRPCOptions, []string, error) {
	fs := flag.NewFlagSet("state "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts stateRPCOptions
	fs.StringVar(&opts.targetID, "target-id", "", "initialized browser target ID")
	fs.StringVar(&opts.engineAddr, "engine-addr", daemonAddr(), "engine address")
	fs.StringVar(&opts.apiKey, "api-key", stateAPIKey(), "engine API key")
	fs.StringVar(&opts.keyFile, "key-file", "", "AES-256 key file")
	fs.StringVar(&opts.output, "out", "", "state bundle output file")
	fs.StringVar(&opts.profileFile, "profile-file", "", "FingerprintProfile JSON file")
	fs.StringVar(&opts.session, "session", "", "new engine context/session name")
	fs.BoolVar(&opts.allowExistingProfile, "allow-existing-profile", false, "allow restore into an existing profile directory")
	if err := fs.Parse(normalizeStateArgs(args)); err != nil {
		return stateRPCOptions{}, nil, err
	}
	return opts, fs.Args(), nil
}

// flag.FlagSet stops parsing at the first non-flag. The public CLI examples
// deliberately read naturally (subcommand, positional values, options), so
// normalize only this closed set of known options before handing them to flag.
func normalizeStateArgs(args []string) []string {
	needsValue := map[string]bool{
		"--target-id": true, "--engine-addr": true, "--api-key": true,
		"--key-file": true, "--out": true, "--profile-file": true, "--session": true,
	}
	boolFlag := map[string]bool{"--allow-existing-profile": true}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if needsValue[arg] {
			flags = append(flags, arg)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if boolFlag[arg] || strings.HasPrefix(arg, "--allow-existing-profile=") {
			flags = append(flags, arg)
			continue
		}
		if strings.HasPrefix(arg, "--target-id=") || strings.HasPrefix(arg, "--engine-addr=") ||
			strings.HasPrefix(arg, "--api-key=") || strings.HasPrefix(arg, "--key-file=") || strings.HasPrefix(arg, "--out=") ||
			strings.HasPrefix(arg, "--profile-file=") || strings.HasPrefix(arg, "--session=") {
			flags = append(flags, arg)
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
}

func stateAPIKey() string {
	if key := strings.TrimSpace(os.Getenv("UWS_ENGINE_API_KEY")); key != "" {
		return key
	}
	return engine.DevAPIKey
}

func stateExport(args []string) error {
	opts, positional, err := parseStateRPCFlags("export", args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return fmt.Errorf("用法: webtouch state export <profile-id> <origin> --target-id <id> --out <bundle.json> [--key-file <key>]")
	}
	if opts.targetID == "" {
		return fmt.Errorf("state export: 必须提供 --target-id（InitializeContext 返回的 target_id）")
	}
	if strings.TrimSpace(opts.output) == "" {
		return fmt.Errorf("state export: 必须提供 --out <bundle.json>")
	}
	key, err := stateKey(opts.keyFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := dialEngine(ctx, opts.engineAddr)
	if err != nil {
		return fmt.Errorf("state export: 无法连接引擎 %s: %w", opts.engineAddr, err)
	}
	defer conn.Close()
	response, err := uwspb.NewUwsEngineClient(conn).ExportState(
		metadata.AppendToOutgoingContext(ctx, "x-api-key", opts.apiKey),
		&uwspb.ExportStateRequest{TargetId: opts.targetID, TargetOrigin: positional[1]},
	)
	if err != nil {
		return fmt.Errorf("state export: 引擎拒绝导出: %w", err)
	}
	bundle, err := stateBundleFromExport(positional[0], response, time.Now())
	if err != nil {
		return err
	}
	var data []byte
	if key != nil {
		data, err = fingerprint.SealStateBundle(bundle, key)
	} else {
		data, err = bundle.Marshal()
	}
	if err != nil {
		return fmt.Errorf("state export: 编码 Bundle: %w", err)
	}
	if err := writeStateBundle(opts.output, data); err != nil {
		return err
	}
	mode := "明文"
	if key != nil {
		mode = "AES-256-GCM 加密"
	}
	fmt.Printf("State Bundle 已导出到 %s（%s；未回显凭证）。\n", opts.output, mode)
	return nil
}

func stateImport(args []string) error {
	opts, positional, err := parseStateRPCFlags("import", args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("用法: webtouch state import <bundle.json> --target-id <clean-id> [--key-file <key>]")
	}
	if opts.targetID == "" {
		return fmt.Errorf("state import: 必须提供 --target-id（已初始化、尚未导航的干净上下文）")
	}
	bundle, encrypted, err := readStateBundle(positional[0], opts.keyFile)
	if err != nil {
		return err
	}
	_ = encrypted // retained for the inspect path; import never logs its secret-bearing contents.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := dialEngine(ctx, opts.engineAddr)
	if err != nil {
		return fmt.Errorf("state import: 无法连接引擎 %s: %w", opts.engineAddr, err)
	}
	defer conn.Close()
	response, err := uwspb.NewUwsEngineClient(conn).ImportState(
		metadata.AppendToOutgoingContext(ctx, "x-api-key", opts.apiKey),
		&uwspb.ImportStateRequest{TargetId: opts.targetID, Bundle: stateBundleProto(bundle)},
	)
	if err != nil {
		return fmt.Errorf("state import: 引擎拒绝恢复: %w", err)
	}
	if !response.GetOk() {
		return fmt.Errorf("state import: 引擎未恢复状态: %s", response.GetErrorReason())
	}
	fmt.Println("State Bundle 已写入目标上下文；现在才可导航到对应站点。")
	return nil
}

// stateRestore is the safe target-side happy path: create the named context
// with the supplied fingerprint profile, restore before navigation, then hand
// the exact session name back to the normal CLI. It does not navigate on the
// caller's behalf, so no restored credential is sent to an unintended origin.
func stateRestore(args []string) error {
	opts, positional, err := parseStateRPCFlags("restore", args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || opts.profileFile == "" || opts.session == "" {
		return fmt.Errorf("用法: webtouch state restore <bundle.json> --profile-file <FingerprintProfile.json> --session <new-session> [--key-file <key>]")
	}
	bundle, _, err := readStateBundle(positional[0], opts.keyFile)
	if err != nil {
		return err
	}
	rawProfile, err := os.ReadFile(opts.profileFile)
	if err != nil {
		return fmt.Errorf("state restore: 读取 FingerprintProfile 失败: %w", err)
	}
	profile, err := fingerprint.Parse(rawProfile)
	if err != nil {
		return fmt.Errorf("state restore: FingerprintProfile 非法: %w", err)
	}
	if profile.ProfileID != bundle.ProfileID {
		return fmt.Errorf("state restore: Bundle profile_id %q 与 FingerprintProfile %q 不一致", bundle.ProfileID, profile.ProfileID)
	}
	if !opts.allowExistingProfile {
		if err := requireFreshProfile(profile.ProfileID); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := dialEngine(ctx, opts.engineAddr)
	if err != nil {
		return fmt.Errorf("state restore: 无法连接引擎 %s: %w", opts.engineAddr, err)
	}
	defer conn.Close()
	client := uwspb.NewUwsEngineClient(conn)
	authCtx := metadata.AppendToOutgoingContext(ctx, "x-api-key", opts.apiKey)
	init, err := client.InitializeContext(authCtx, &uwspb.InitContextRequest{
		TaskId:                 opts.session,
		ContextName:            proto.String(opts.session),
		ProfileId:              proto.String(bundle.ProfileID),
		FingerprintProfileJson: proto.String(string(rawProfile)),
	})
	if err != nil {
		return fmt.Errorf("state restore: 初始化目标上下文失败: %w", err)
	}
	response, err := client.ImportState(authCtx, &uwspb.ImportStateRequest{TargetId: init.GetTargetId(), Bundle: stateBundleProto(bundle)})
	if err != nil {
		return fmt.Errorf("state restore: 引擎拒绝恢复: %w", err)
	}
	if !response.GetOk() {
		return fmt.Errorf("state restore: 引擎未恢复状态: %s", response.GetErrorReason())
	}
	fmt.Printf("State Bundle 已恢复到新会话 %s（target=%s）。现在用同一 --session 导航；命令未回显凭证。\n", opts.session, init.GetTargetId())
	return nil
}

// requireFreshProfile makes the convenient restore path conservative. A State
// Bundle is an explicit migration into a clean browser identity, not a hidden
// merge with unknown prior Cookies or storage. Advanced operators can opt out
// only with an explicit flag after inspecting that directory themselves.
func requireFreshProfile(profileID string) error {
	root, err := browser.DefaultProfileRoot()
	if err != nil {
		return fmt.Errorf("state restore: 定位 Profile 目录失败: %w", err)
	}
	dir, err := browser.ProfileDir(root, profileID)
	if err != nil {
		return fmt.Errorf("state restore: Profile 路径非法: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("state restore: 检查 Profile 目录失败: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("state restore: Profile %q 已有本地数据；恢复只允许干净目标，确认后才可显式使用 --allow-existing-profile", profileID)
	}
	return nil
}

func stateInspect(args []string) error {
	opts, positional, err := parseStateRPCFlags("inspect", args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("用法: webtouch state inspect <bundle.json> [--key-file <key>]")
	}
	bundle, encrypted, err := readStateBundle(positional[0], opts.keyFile)
	if err != nil {
		return err
	}
	var cookies []json.RawMessage
	if err := json.Unmarshal(bundle.Cookies, &cookies); err != nil {
		return fmt.Errorf("state inspect: cookies 计数失败: %w", err)
	}
	mode := "明文"
	if encrypted {
		mode = "AES-256-GCM 加密"
	}
	fmt.Printf("State Bundle：profile=%s，创建=%s，Cookies=%d，Origins=%d，%s。\n",
		bundle.ProfileID, bundle.CreatedAt, len(cookies), len(bundle.Origins), mode)
	return nil
}

func stateBundleFromExport(profileID string, response *uwspb.ExportStateResponse, now time.Time) (*fingerprint.StateBundle, error) {
	bundle, err := fingerprint.NewStateBundle(profileID, now)
	if err != nil {
		return nil, err
	}
	bundle.Cookies = json.RawMessage(response.GetCookiesJson())
	for _, origin := range response.GetOrigins() {
		out := fingerprint.StateBundleOrigin{Origin: origin.GetOrigin()}
		for _, kv := range origin.GetLocalStorage() {
			out.LocalStorage = append(out.LocalStorage, fingerprint.StateBundleKVPair{Key: kv.GetKey(), Value: kv.GetValue()})
		}
		for _, kv := range origin.GetSessionStorage() {
			out.SessionStorage = append(out.SessionStorage, fingerprint.StateBundleKVPair{Key: kv.GetKey(), Value: kv.GetValue()})
		}
		bundle.Origins = append(bundle.Origins, out)
	}
	if err := bundle.Validate(); err != nil {
		return nil, fmt.Errorf("state export: 引擎返回的状态不合法: %w", err)
	}
	return bundle, nil
}

func stateBundleProto(bundle *fingerprint.StateBundle) *uwspb.StateBundle {
	pb := &uwspb.StateBundle{ProfileId: bundle.ProfileID, CreatedAt: bundle.CreatedAt, CookiesJson: string(bundle.Cookies)}
	for _, origin := range bundle.Origins {
		out := &uwspb.StorageOriginContext{Origin: origin.Origin}
		for _, kv := range origin.LocalStorage {
			out.LocalStorage = append(out.LocalStorage, &uwspb.StorageOriginContext_KVPair{Key: kv.Key, Value: kv.Value})
		}
		for _, kv := range origin.SessionStorage {
			out.SessionStorage = append(out.SessionStorage, &uwspb.StorageOriginContext_KVPair{Key: kv.Key, Value: kv.Value})
		}
		pb.Origins = append(pb.Origins, out)
	}
	return pb
}

func stateKey(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("state: 读取 key 文件失败: %w", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) == 64 {
		key, err := hex.DecodeString(trimmed)
		if err == nil && len(key) == 32 {
			return key, nil
		}
	}
	if len(data) == 32 {
		return data, nil
	}
	return nil, fmt.Errorf("state: key 文件必须是 32 个原始字节或 64 位十六进制 AES-256 密钥")
}

func readStateBundle(path, keyFile string) (*fingerprint.StateBundle, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("state: 读取 Bundle 失败: %w", err)
	}
	key, err := stateKey(keyFile)
	if err != nil {
		return nil, false, err
	}
	if key != nil {
		bundle, err := fingerprint.OpenStateBundle(data, key)
		if err != nil {
			return nil, true, fmt.Errorf("state: 解密 Bundle 失败: %w", err)
		}
		return bundle, true, nil
	}
	bundle, err := fingerprint.ParseStateBundle(data)
	if err != nil {
		return nil, false, fmt.Errorf("state: 解析明文 Bundle 失败（加密 Bundle 请提供 --key-file）: %w", err)
	}
	return bundle, false, nil
}

func writeStateBundle(path string, data []byte) error {
	if path == "" {
		return fmt.Errorf("state: 输出路径为空")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: 创建输出目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-bundle-*")
	if err != nil {
		return fmt.Errorf("state: 创建临时 Bundle 失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("state: 设置 Bundle 权限失败: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("state: 写入 Bundle 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: 关闭 Bundle 失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("state: 原子发布 Bundle 失败: %w", err)
	}
	return nil
}
