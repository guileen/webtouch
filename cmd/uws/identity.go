package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"uws/pkg/fingerprint"
)

// webtouch identity —— 浏览器身份（指纹）注册表与账号绑定（Phase 2，2026-09-12）。
//
// 用户的原始需求：「哪怕迁移，也要保证浏览器指纹一致；账号和指纹是绑定的；会有多份指纹；
// 每份指纹要绑自己的 SOCKS5 代理」。这条命令就是那三句话的落点：
//   - 有哪些身份        → list / show
//   - 账号用哪份身份    → bind / unbind / resolve
//   - 这份身份怎么跑    → use（打印可直接执行的命令，含代理）
//   - 有没有埋雷        → check（共用代理出口、悬空绑定）
func runIdentity(args []string) error {
	registry, err := fingerprint.OpenRegistry("")
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Print(identityGuide(registry))
		return nil
	}
	switch args[0] {
	case "list", "ls":
		return identityList(registry)
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("用法: webtouch identity show <profile-id>")
		}
		profile, err := registry.Load(args[1])
		if err != nil {
			return err
		}
		data, err := fingerprintJSON(profile)
		if err != nil {
			return err
		}
		fmt.Print(data)
		return nil
	case "bind":
		if len(args) < 3 {
			return fmt.Errorf("用法: webtouch identity bind <账号> <profile-id>")
		}
		if err := registry.Bind(args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("已绑定: %s → %s\n", args[1], args[2])
		return nil
	case "unbind":
		if len(args) < 2 {
			return fmt.Errorf("用法: webtouch identity unbind <账号>")
		}
		if err := registry.Unbind(args[1]); err != nil {
			return err
		}
		fmt.Printf("已解绑: %s\n", args[1])
		return nil
	case "resolve":
		if len(args) < 2 {
			return fmt.Errorf("用法: webtouch identity resolve <账号>")
		}
		profile, err := registry.Resolve(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("%s → %s\n%s", args[1], profile.ProfileID, identityUseHint(registry, profile))
		return nil
	case "use":
		if len(args) < 2 {
			return fmt.Errorf("用法: webtouch identity use <profile-id>")
		}
		profile, err := registry.Load(args[1])
		if err != nil {
			return err
		}
		fmt.Print(identityUseHint(registry, profile))
		return nil
	case "check":
		problems, err := registry.Conflicts()
		if err != nil {
			return err
		}
		if len(problems) == 0 {
			fmt.Printf("身份目录 %s：没有发现问题\n", registry.Root)
			return nil
		}
		fmt.Printf("身份目录 %s 有 %d 处要注意：\n", registry.Root, len(problems))
		for _, problem := range problems {
			fmt.Printf("  - %s\n", problem)
		}
		return nil
	case "help", "-h", "--help":
		fmt.Print(identityGuide(registry))
		return nil
	default:
		return fmt.Errorf("未知子命令 identity %s（支持 list | show | bind | unbind | resolve | use | check）", args[0])
	}
}

func identityGuide(registry *fingerprint.Registry) string {
	return fmt.Sprintf(`webtouch identity —— 浏览器身份（指纹）注册表

  身份目录: %s（UWS_IDENTITY_ROOT 可覆盖）

  webtouch identity list                     看有哪些身份（平台/时区/代理/绑了哪个账号）
  webtouch identity show <profile-id>        看某份身份的档案（代理凭据脱敏）
  webtouch identity bind <账号> <profile-id>  把账号钉在这份身份上（1:1，改绑要先 unbind）
  webtouch identity unbind <账号>             解除绑定
  webtouch identity resolve <账号>            按账号反查身份，并打印怎么用它跑起来
  webtouch identity use <profile-id>          打印可直接执行的运行命令
  webtouch identity check                     查共用代理出口 / 悬空绑定

不变式：一个账号一份身份、一份身份一个账号——两个账号共用同一份指纹，等于直接告诉
平台「这俩是同一个人」。入口档案是 <profile-id>.json，只放档案、不放绑定。
`, registry.Root)
}

func identityList(registry *fingerprint.Registry) error {
	profiles, err := registry.List()
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		fmt.Printf("身份目录 %s 里还没有身份。\n", registry.Root)
		fmt.Println("放一份档案进去即可，例如 fp_mac_01.json（字段见 docs/fingerprint-and-viewer-final-spec-2026-09-12.md §2.1）。")
		return nil
	}
	fmt.Printf("%-20s %-10s %-16s %-28s %s\n", "身份", "平台", "时区", "代理", "账号")
	for _, profile := range profiles {
		accounts, err := registry.AccountsOf(profile.ProfileID)
		if err != nil {
			return err
		}
		proxy := fingerprint.RedactProxyURL(profile.Network.ProxyURL)
		if proxy == "" {
			proxy = "（直连）"
		}
		sort.Strings(accounts)
		fmt.Printf("%-20s %-10s %-16s %-28s %s\n",
			profile.ProfileID, profile.Emulation.UserAgentMetadata.Platform,
			profile.Emulation.Locale.TimezoneID, proxy, strings.Join(accounts, ","))
	}
	return nil
}

// identityUseHint 打印「用这份身份跑一次观看壳」的完整命令——把身份名、档案路径与代理
// 都钉在一条命令里，避免人手抄错（抄错就等于两个身份串号）；代理凭据只展示脱敏值。
func identityUseHint(registry *fingerprint.Registry, profile *fingerprint.Profile) string {
	path, err := registry.PathOf(profile.ProfileID)
	if err != nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "运行（在 webtouch 根目录）：\n")
	fmt.Fprintf(&b, "  ./uws-viewer-demo --url https://example.com --seconds 0 \\\n")
	fmt.Fprintf(&b, "      --profile-id %s --fingerprint %s\n", profile.ProfileID, path)
	if profile.Network.ProxyURL != "" {
		fmt.Fprintf(&b, "代理（由档案带进浏览器）: %s\n", fingerprint.RedactProxyURL(profile.Network.ProxyURL))
		fmt.Fprintf(&b, "注意：代理只在 UWS 自己拉起 Chrome 时生效（--proxy-server）；附着已有 Chrome（--attach）时\n")
		fmt.Fprintf(&b, "      档案代理完全不生效，流量走本机真实出口。\n")
		if warn := profile.WebRTCLeakWarning(); warn != "" {
			fmt.Fprintf(&b, "      %s。\n", warn)
		}
	} else {
		fmt.Fprintf(&b, "代理: 未设置（直连出口）——多身份直连同一个 IP 同样会被关联\n")
	}
	return b.String()
}

func fingerprintJSON(profile *fingerprint.Profile) (string, error) {
	if profile == nil {
		return "", fmt.Errorf("身份档案为空")
	}
	display := *profile
	display.Network.ProxyURL = fingerprint.RedactProxyURL(display.Network.ProxyURL)
	data, err := fingerprint.MarshalIndent(&display)
	if err != nil {
		return "", err
	}
	_, _ = os.Stdout.WriteString("")
	return data, nil
}
