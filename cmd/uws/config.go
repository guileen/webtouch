package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// configKey 是 ~/.uws/config.yaml 中一个可设置键（section.key 形式）。
// 文档结构见 docs/cli-and-config.md §4：core / llm / knowledge / vlm 四段。
type configKey struct {
	section string
	key     string
}

func (k configKey) String() string { return k.section + "." + k.key }

// knownConfigKeys 是 V1.4 支持的全部配置键（保持文档顺序）。
var knownConfigKeys = []configKey{
	{"core", "max_targets"},
	{"core", "headless"},
	{"llm", "fallback_enabled"},
	{"llm", "provider"},
	{"llm", "api_base"},
	{"llm", "api_key"},
	{"llm", "model"},
	{"knowledge", "local_db_path"},
	{"vlm", "provider"},
	{"vlm", "endpoint"},
	{"vlm", "api_key"},
	{"vlm", "timeout_sec"},
	{"vlm", "max_solves"},
}

// configAliases 提供 CLI 文档中的傻瓜化别名（docs/cli-and-config.md §2.1）。
var configAliases = map[string]configKey{
	"fallback_model":    {"llm", "model"},
	"fallback_api_base": {"llm", "api_base"},
	"fallback_api_key":  {"llm", "api_key"},
	"fallback_enabled":  {"llm", "fallback_enabled"},
	"provider":          {"llm", "provider"},
	"max_targets":       {"core", "max_targets"},
	"headless":          {"core", "headless"},
	"local_db_path":     {"knowledge", "local_db_path"},
	"vlm_provider":      {"vlm", "provider"},
	"vlm_endpoint":      {"vlm", "endpoint"},
	"vlm_api_key":       {"vlm", "api_key"},
	"vlm_timeout":       {"vlm", "timeout_sec"},
	"vlm_max_solves":    {"vlm", "max_solves"},
}

// sensitiveConfigKeys 的值在任何输出中都不得出现明文。
var sensitiveConfigKeys = map[configKey]bool{
	{"llm", "api_key"}: true,
	{"vlm", "api_key"}: true,
}

// runConfig 处理 `webtouch config set <key> <value>`。
func runConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("config: 缺少子命令（当前支持 set, get）")
	}
	switch args[0] {
	case "set":
		if len(args) != 3 {
			return errors.New("config: 用法 `webtouch config set <key> <value>`")
		}
		return configSet(args[1], args[2])
	case "get":
		if len(args) != 2 {
			return errors.New("config: 用法 `webtouch config get <key>`")
		}
		return configGet(args[1])
	default:
		return fmt.Errorf("config: 未知子命令 %q（当前支持 set, get）", args[0])
	}
}

// configGet 读取指定配置键。若未配置或敏感键，输出脱敏/默认占位并返回 0。
func configGet(name string) error {
	key, ok := parseConfigKey(name)
	if !ok {
		return fmt.Errorf("config: 未知配置键 %q", name)
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	values, err := readConfig(path)
	if err != nil {
		return err
	}
	val, exists := values[key.String()]
	if !exists {
		fmt.Printf("%s = <unset>\n", key)
		return nil
	}
	if sensitiveConfigKeys[key] {
		fmt.Printf("%s = <redacted>\n", key)
	} else {
		fmt.Printf("%s = %s\n", key, val)
	}
	return nil
}

// configSet 解析键名（支持 section.key 与别名），更新 ~/.uws/config.yaml（0600）。
func configSet(name, value string) error {
	key, ok := parseConfigKey(name)
	if !ok {
		return fmt.Errorf("config: 未知配置键 %q", name)
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	values, err := readConfig(path)
	if err != nil {
		return err
	}
	values[key.String()] = value
	if err := writeConfig(path, values); err != nil {
		return err
	}
	if sensitiveConfigKeys[key] {
		fmt.Printf("ok: %s 已更新（值已隐藏）\n", key)
	} else {
		fmt.Printf("ok: %s = %s\n", key, value)
	}
	return nil
}

func parseConfigKey(name string) (configKey, bool) {
	if key, ok := configAliases[name]; ok {
		return key, true
	}
	if strings.Contains(name, ".") {
		parts := strings.SplitN(name, ".", 2)
		key := configKey{section: parts[0], key: parts[1]}
		for _, known := range knownConfigKeys {
			if known == key {
				return key, true
			}
		}
	}
	return configKey{}, false
}

// configPath 返回 ~/.uws/config.yaml；UWS_CONFIG_DIR 环境变量可覆盖目录（测试用）。
func configPath() (string, error) {
	if dir := os.Getenv("UWS_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: 无法确定 HOME: %w", err)
	}
	return filepath.Join(home, ".uws", "config.yaml"), nil
}

// readConfig 解析极简 YAML 子集（仅支持本 CLI 写出的 section/key: value 结构）。
func readConfig(path string) (map[string]string, error) {
	values := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return values, nil
		}
		return nil, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && strings.HasSuffix(trimmed, ":") {
			section = strings.TrimSuffix(trimmed, ":")
			continue
		}
		if section == "" {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, `"'`)
		values[section+"."+key] = val
	}
	return values, nil
}

// writeConfig 以 0600 权限写回配置（目录 0700）；仅写出已知键，保持确定性排序。
func writeConfig(path string, values map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: 创建目录失败: %w", err)
	}
	var b strings.Builder
	section := ""
	for _, key := range knownConfigKeys {
		value, ok := values[key.String()]
		if !ok {
			continue
		}
		if key.section != section {
			if section != "" {
				b.WriteString("\n")
			}
			b.WriteString(key.section + ":\n")
			section = key.section
		}
		b.WriteString("  " + key.key + ": " + yamlScalar(value) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("config: 写入 %s 失败: %w", path, err)
	}
	return nil
}

// yamlScalar 将字符串编码为 YAML 标量（含特殊字符时用双引号包裹）。
func yamlScalar(value string) string {
	if value == "" || strings.ContainsAny(value, ":#\"'{}[],&*?|>%@` \t") || strings.HasPrefix(value, "-") {
		return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return value
}
