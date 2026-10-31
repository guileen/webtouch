package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigGet(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("UWS_CONFIG_DIR", tmp)

	// 1. 未配置读取
	var out bytes.Buffer
	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runConfig([]string{"get", "headless"})
	w.Close()
	os.Stdout = origStdout
	if err != nil {
		t.Fatalf("get 未配置键失败: %v", err)
	}
	io.Copy(&out, r)
	if !strings.Contains(out.String(), "<unset>") {
		t.Errorf("期望包含 <unset>, 实际: %s", out.String())
	}

	// 2. 写入后读取
	if err := runConfig([]string{"set", "fallback_api_key", "secret-test-key"}); err != nil {
		t.Fatalf("set 失败: %v", err)
	}

	out.Reset()
	r, w, _ = os.Pipe()
	os.Stdout = w
	err = runConfig([]string{"get", "fallback_api_key"})
	w.Close()
	os.Stdout = origStdout
	if err != nil {
		t.Fatalf("get 敏感键失败: %v", err)
	}
	io.Copy(&out, r)
	if !strings.Contains(out.String(), "<redacted>") {
		t.Errorf("敏感键期望隐藏为 <redacted>, 实际: %s", out.String())
	}
}

func TestConfigSetPersistAndSecure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UWS_CONFIG_DIR", dir)

	if err := configSet("fallback_model", "deepseek-chat"); err != nil {
		t.Fatalf("configSet fallback_model: %v", err)
	}
	path := filepath.Join(dir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if !strings.Contains(string(data), "model: deepseek-chat") {
		t.Fatalf("配置未包含 model: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("配置权限应为 0600，实际 %o", info.Mode().Perm())
	}

	secret := "sk-super-secret-123456"
	if err := configSet("fallback_api_key", secret); err != nil {
		t.Fatalf("configSet fallback_api_key: %v", err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "api_key:") || !strings.Contains(string(data), secret) {
		t.Fatalf("api_key 未持久化: %s", data)
	}
	if !strings.Contains(string(data), "deepseek-chat") {
		t.Fatalf("既有字段被覆盖: %s", data)
	}
}

func TestConfigSetUnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UWS_CONFIG_DIR", dir)
	if err := configSet("no_such_key", "x"); err == nil {
		t.Fatal("未知配置键应报错")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatal("失败时不应创建配置文件")
	}
}

func TestConfigSetVlmKeysPersistAndSecure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UWS_CONFIG_DIR", dir)

	if err := configSet("vlm_provider", "http"); err != nil {
		t.Fatalf("configSet vlm_provider: %v", err)
	}
	if err := configSet("vlm_endpoint", "https://solver.example.com/v1/solve"); err != nil {
		t.Fatalf("configSet vlm_endpoint: %v", err)
	}
	secret := "vlm-sk-super-secret-123456"
	if err := configSet("vlm_api_key", secret); err != nil {
		t.Fatalf("configSet vlm_api_key: %v", err)
	}
	if err := configSet("vlm_timeout", "10"); err != nil {
		t.Fatalf("configSet vlm_timeout: %v", err)
	}
	if err := configSet("vlm_max_solves", "5"); err != nil {
		t.Fatalf("configSet vlm_max_solves: %v", err)
	}

	path := filepath.Join(dir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("配置权限应为 0600，实际 %o", info.Mode().Perm())
	}
	for _, want := range []string{
		"provider: http",
		`endpoint: "https://solver.example.com/v1/solve"`,
		"timeout_sec: 10",
		"max_solves: 5",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("配置缺少 %q: %s", want, data)
		}
	}
	// api_key 必须持久化（配置是唯一事实源），但 stdout 输出必须隐藏明文。
	if !strings.Contains(string(data), secret) {
		t.Fatalf("vlm.api_key 未持久化: %s", data)
	}
}

func TestParseConfigKey(t *testing.T) {
	key, ok := parseConfigKey("llm.api_key")
	if !ok || key.String() != "llm.api_key" {
		t.Fatalf("点分键解析失败: %v %v", key, ok)
	}
	key, ok = parseConfigKey("fallback_model")
	if !ok || key.String() != "llm.model" {
		t.Fatalf("别名解析失败: %v %v", key, ok)
	}
	key, ok = parseConfigKey("vlm.api_key")
	if !ok || key.String() != "vlm.api_key" {
		t.Fatalf("vlm.api_key 点分键解析失败: %v %v", key, ok)
	}
	key, ok = parseConfigKey("vlm_provider")
	if !ok || key.String() != "vlm.provider" {
		t.Fatalf("vlm_provider 别名解析失败: %v %v", key, ok)
	}
	if _, ok := parseConfigKey("unknown.key"); ok {
		t.Fatal("未知点分键不应通过")
	}
}
