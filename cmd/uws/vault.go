// uws vault —— 本地凭据库（2026-09-23 用户定案：用 age；GLOBAL #119）。
//
// 口径：账号密码 / API Key 统一收口在这只本机保险箱里；**不联网、不做对外服务**，
// 明文只在显式 `get` 时输出一次。搬凭据给别的系统时，用 `get --fingerprint`
// 双端比对指纹，不把明文写进日志、对话或提交。
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"uws/pkg/vault"
)

// vaultPaths 解析 identity 与密文路径（默认 ~/.uws/vault.{key,age}，
// 可用 UWS_VAULT_KEY / UWS_VAULT_FILE 覆盖，便于测试与多库隔离）。
func vaultPaths() (keyPath, filePath string) {
	// 默认值与引擎侧共用 vault.ResolvePaths（2026-09-23 去重）：UWS_VAULT_KEY /
	// UWS_VAULT_FILE 优先，否则 <状态目录>/vault.{key,age}。
	return vault.ResolvePaths(daemonStateDir())
}

func runVault(args []string) error {
	if len(args) == 0 {
		fmt.Print(vaultGuide())
		return nil
	}
	keyPath, filePath := vaultPaths()
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(vaultGuide())
		return nil
	case "init":
		return vaultInit(args[1:], keyPath, filePath)
	case "list", "ls":
		return vaultList(keyPath, filePath)
	case "set", "add":
		return vaultSet(args[1:], keyPath, filePath)
	case "get", "show":
		return vaultGet(args[1:], keyPath, filePath)
	case "rm", "delete", "del":
		return vaultRemove(args[1:], keyPath, filePath)
	default:
		return fmt.Errorf("未知子命令 vault %s（支持 init | list | set | get | rm | help）", args[0])
	}
}

func vaultInit(args []string, keyPath, filePath string) error {
	force := false
	for _, a := range args {
		switch a {
		case "--force", "-f":
			force = true
		case "-h", "--help":
			fmt.Print(vaultGuide())
			return nil
		default:
			return fmt.Errorf("用法: uws vault init [--force]")
		}
	}
	store, recipient, err := vault.Init(keyPath, filePath, force)
	if err != nil {
		return err
	}
	fmt.Printf("已初始化本地凭据库\n  identity: %s（0600，只落本机，丢了就解不开密文）\n  密文:     %s\n  公开收件人: %s\n",
		keyPath, filePath, recipient)
	if force {
		fmt.Println("（--force 重建：旧密文已随旧 identity 一并作废）")
	}
	_ = store
	fmt.Println("下一步: uws vault set <条目名> 写入第一条（值从终端无回显读取，不进 argv/历史）")
	return nil
}

func vaultList(keyPath, filePath string) error {
	store, err := vault.Open(keyPath, filePath)
	if err != nil {
		return err
	}
	items := store.List()
	if len(items) == 0 {
		fmt.Printf("凭据库为空（%s）。用 `uws vault set <条目名>` 写入第一条。\n", filePath)
		return nil
	}
	fmt.Printf("凭据库 %s（%d 条；只显示指纹，不显示明文）\n", filePath, len(items))
	for _, it := range items {
		note := it.Note
		if note == "" {
			note = "-"
		}
		fmt.Printf("  %-24s sha256:%s  %s  %s\n", it.Name, it.Fingerprint, it.UpdatedAt.Format(time.DateTime), note)
	}
	return nil
}

func vaultSet(args []string, keyPath, filePath string) error {
	var name, note string
	fromStdin := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--note":
			if i+1 >= len(args) {
				return errors.New("用法: uws vault set <名> [--note <说明>] [--stdin]")
			}
			i++
			note = args[i]
		case "--stdin":
			fromStdin = true
		case "-h", "--help":
			fmt.Print(vaultGuide())
			return nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("未知参数 %s（用法: uws vault set <名> [--note <说明>] [--stdin]）", args[i])
			}
			if name != "" {
				return errors.New("用法: uws vault set <名> [--note <说明>] [--stdin]")
			}
			name = args[i]
		}
	}
	if name == "" {
		return errors.New("用法: uws vault set <名> [--note <说明>] [--stdin]")
	}
	value, err := readVaultSecret(fmt.Sprintf("请输入 %q 的值（不显示、不进 argv）: ", name), fromStdin)
	if err != nil {
		return err
	}
	store, err := vault.Open(keyPath, filePath)
	if err != nil {
		return err
	}
	if err := store.Set(name, value, note); err != nil {
		return err
	}
	// 只回指纹：双端比对「搬对了没有」，明文不落任何日志。
	fmt.Printf("已写入 %s（sha256:%s）\n", name, vault.Fingerprint(value))
	return nil
}

func vaultGet(args []string, keyPath, filePath string) error {
	var name string
	onlyFingerprint := false
	for _, a := range args {
		switch a {
		case "--fingerprint", "--fp":
			onlyFingerprint = true
		case "-h", "--help":
			fmt.Print(vaultGuide())
			return nil
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("未知参数 %s（用法: uws vault get <名> [--fingerprint]）", a)
			}
			if name != "" {
				return errors.New("用法: uws vault get <名> [--fingerprint]")
			}
			name = a
		}
	}
	if name == "" {
		return errors.New("用法: uws vault get <名> [--fingerprint]")
	}
	store, err := vault.Open(keyPath, filePath)
	if err != nil {
		return err
	}
	entry, ok := store.Get(name)
	if !ok {
		return fmt.Errorf("%w: %s（用 `uws vault list` 看有哪些）", vault.ErrNotFound, name)
	}
	if onlyFingerprint {
		fmt.Printf("%s\n", vault.Fingerprint(entry.Value))
		return nil
	}
	fmt.Printf("%s\n", entry.Value)
	return nil
}

func vaultRemove(args []string, keyPath, filePath string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("用法: uws vault rm <名>")
	}
	store, err := vault.Open(keyPath, filePath)
	if err != nil {
		return err
	}
	ok, err := store.Delete(args[0])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", vault.ErrNotFound, args[0])
	}
	fmt.Printf("已删除 %s\n", args[0])
	return nil
}

// readVaultSecret 读取一个凭据值：TTY 下关回显读取；管道/`--stdin` 则从 stdin 读。
// 值永远不经 argv，也不回显——避免落进 shell history / ps / 日志。
func readVaultSecret(prompt string, forceStdin bool) (string, error) {
	fi, err := os.Stdin.Stat()
	interactive := err == nil && fi.Mode()&os.ModeCharDevice != 0
	if forceStdin || !interactive {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("读取 stdin: %w", err)
		}
		value := strings.TrimRight(string(data), "\r\n")
		if value == "" {
			return "", errors.New("stdin 里没有读到值")
		}
		return value, nil
	}
	fmt.Fprint(os.Stderr, prompt)
	restore, err := disableTerminalEcho()
	if err == nil {
		defer restore()
	}
	line, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Fprintln(os.Stderr)
	if readErr != nil && readErr != io.EOF {
		return "", fmt.Errorf("读取输入: %w", readErr)
	}
	value := strings.TrimRight(line, "\r\n")
	if value == "" {
		return "", errors.New("没有输入值")
	}
	return value, nil
}

// disableTerminalEcho 临时关闭终端回显（返回恢复函数）。
func disableTerminalEcho() (func(), error) {
	off := exec.Command("stty", "-echo")
	off.Stdin = os.Stdin
	if err := off.Run(); err != nil {
		return func() {}, err
	}
	return func() {
		on := exec.Command("stty", "echo")
		on.Stdin = os.Stdin
		_ = on.Run()
	}, nil
}

func vaultGuide() string {
	keyPath, filePath := vaultPaths()
	return fmt.Sprintf(`uws vault —— 本地凭据库（age 加密，只落本机、不联网、不做对外服务）

  uws vault init [--force]           初始化（生成 identity + 空库；--force 重建并作废旧密文）
  uws vault set <名> [--note <说明>]  写入/覆盖一条（TTY 无回显；管道时读 stdin，也可 --stdin）
  uws vault get <名> [--fingerprint]  读出明文（唯一的明文出口）；--fingerprint 只回 sha256 前 12 位
  uws vault list                     列出条目名 + 指纹 + 更新时间（**不显示明文**）
  uws vault rm <名>                  删除一条
  uws vault help                     本说明

路径：identity %s、密文 %s（UWS_VAULT_KEY / UWS_VAULT_FILE 覆盖；0600）
口径：明文只经 get 输出一次；搬凭据给别的系统时用 --fingerprint 双端比对，
      不要把明文写进日志、对话、脚本参数或提交。
边界：identity 与密文同目录，防的是误传/误提交/备份泄露；本机已被读取时它防不住
      （硬化方向：identity 进系统钥匙串 + 生物解锁）。
`, keyPath, filePath)
}
