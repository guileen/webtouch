package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEngineArgsDefaultAndOverride(t *testing.T) {
	t.Setenv("UWS_ENGINE_DIR", t.TempDir())
	t.Setenv("UWS_ENGINE_ARGS", "")

	args, err := engineArgs("127.0.0.1:50051")
	if err != nil {
		t.Fatalf("engineArgs 出错: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--browser cdp", "--grpc-addr 127.0.0.1:50051", "--log-path"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("engineArgs 缺少 %q，got: %s", want, joined)
		}
	}

	// 追加 UWS_ENGINE_ARGS 覆盖（flag 包后者胜出，用于切 sim / 加 learned 等）。
	t.Setenv("UWS_ENGINE_ARGS", "--browser sim --vlm-learn-path /tmp/learned.json")
	args, err = engineArgs("127.0.0.1:50052")
	if err != nil {
		t.Fatalf("engineArgs 覆盖出错: %v", err)
	}
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--browser sim") {
		t.Fatalf("UWS_ENGINE_ARGS 应为后值覆盖，got: %s", joined)
	}
	if !strings.Contains(joined, "--grpc-addr 127.0.0.1:50052") {
		t.Fatalf("grpc-addr 未随参数变化，got: %s", joined)
	}
	if !strings.Contains(joined, "--vlm-learn-path /tmp/learned.json") {
		t.Fatalf("UWS_ENGINE_ARGS 未追加，got: %s", joined)
	}
}

// TestEngineArgsLearnedDefaultAndDisable 固化「learned 自进化默认开」口径：
// daemon 托管默认带 --vlm-learn-path <stateDir>/learned.json（2026-09-30 起），
// UWS_LEARN=off 显式关闭，其他非空值可覆盖路径。
func TestEngineArgsLearnedDefaultAndDisable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UWS_ENGINE_DIR", dir)
	t.Setenv("UWS_ENGINE_ARGS", "")

	args, err := engineArgs("127.0.0.1:50051")
	if err != nil {
		t.Fatalf("engineArgs 出错: %v", err)
	}
	joined := strings.Join(args, " ")
	want := filepath.Join(dir, "learned.json")
	if !strings.Contains(joined, "--vlm-learn-path "+want) {
		t.Fatalf("默认应带 learned 路径 %q，got: %s", want, joined)
	}

	t.Setenv("UWS_LEARN", "off")
	args, err = engineArgs("127.0.0.1:50051")
	if err != nil {
		t.Fatalf("engineArgs 出错: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), "--vlm-learn-path") {
		t.Fatalf("UWS_LEARN=off 应关闭自进化，got: %s", strings.Join(args, " "))
	}

	t.Setenv("UWS_LEARN", "/tmp/custom-learned.json")
	args, err = engineArgs("127.0.0.1:50051")
	if err != nil {
		t.Fatalf("engineArgs 出错: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "--vlm-learn-path /tmp/custom-learned.json") {
		t.Fatalf("UWS_LEARN 应可覆盖路径，got: %s", strings.Join(args, " "))
	}
}

func TestCoordinatorEngineAddrParsesDefaultAndFlag(t *testing.T) {
	t.Setenv("UWS_ENGINE_ADDR", "127.0.0.1:57111")

	if got := coordinatorEngineAddr([]string{"run", "--attach", "http://127.0.0.1:9222"}); got != "127.0.0.1:57111" {
		t.Fatalf("无 --engine-addr 时应回退 UWS_ENGINE_ADDR，got %q", got)
	}
	if got := coordinatorEngineAddr([]string{"api", "--engine-addr", "127.0.0.1:57112", "--port", "8000"}); got != "127.0.0.1:57112" {
		t.Fatalf("应解析 --engine-addr，got %q", got)
	}
}

func TestEngineBinaryUsesEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "engine-bin")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UWS_ENGINE_BIN", binPath)
	got, err := engineBinary()
	if err != nil {
		t.Fatalf("engineBinary 出错: %v", err)
	}
	if got != binPath {
		t.Fatalf("应返回 UWS_ENGINE_BIN，got %q want %q", got, binPath)
	}
}

func TestRepoRootFromDir(t *testing.T) {
	root := t.TempDir()
	cliFile := filepath.Join(root, "coordinator", "cli.py")
	if err := os.MkdirAll(filepath.Dir(cliFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliFile, []byte("# stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		start string
		want  string
	}{
		{"仓库根本身", root, root},
		{"bin 子目录（bin/webtouch 部署形态）", filepath.Join(root, "bin"), root},
		{"嵌套子目录", filepath.Join(root, "bin", "nested"), root},
		{"仓库外目录", t.TempDir(), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := repoRootFromDir(c.start); got != c.want {
				t.Fatalf("repoRootFromDir(%q) = %q, want %q", c.start, got, c.want)
			}
		})
	}
}

func TestDaemonStateDirEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("UWS_ENGINE_DIR", tmp)
	if got := daemonStateDir(); got != tmp {
		t.Fatalf("daemonStateDir 应返回 UWS_ENGINE_DIR，got %q want %q", got, tmp)
	}

	t.Setenv("UWS_ENGINE_DIR", "")
	t.Setenv("WEBTOUCH_STATE_DIR", "")
	if got := daemonStateDir(); filepath.Base(got) != ".uws" {
		t.Fatalf("默认目录应为 ~/.uws，got %q", got)
	}
}

func TestWebTouchStateDirOverridesEngineDir(t *testing.T) {
	primary := t.TempDir()
	legacy := t.TempDir()
	t.Setenv("WEBTOUCH_STATE_DIR", primary)
	t.Setenv("UWS_ENGINE_DIR", legacy)
	if got := daemonStateDir(); got != primary {
		t.Fatalf("WEBTOUCH_STATE_DIR 应优先，got %q", got)
	}
}

func TestWebTouchEngineBinOverridesUWS(t *testing.T) {
	tmp := t.TempDir()
	primary := filepath.Join(tmp, "webtouch-engine")
	legacy := filepath.Join(tmp, "uws-engine")
	if err := os.WriteFile(primary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBTOUCH_ENGINE_BIN", primary)
	t.Setenv("UWS_ENGINE_BIN", legacy)
	got, err := engineBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != primary {
		t.Fatalf("应返回 WEBTOUCH_ENGINE_BIN，got %q", got)
	}
}

func TestEngineBinaryCandidatesPreferOfficialName(t *testing.T) {
	got := engineBinaryCandidates("/usr/local/bin")
	if len(got) < 2 {
		t.Fatalf("候选过少: %#v", got)
	}
	if got[0] != "/usr/local/bin/webtouch-engine" || got[1] != "/usr/local/bin/uws-engine" {
		t.Fatalf("同目录应先正式名再兼容名，got %#v", got)
	}
}

func TestPortOpenDetectsListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if !portOpen(ln.Addr().String()) {
		t.Fatalf("监听中地址应判定为已打开: %s", ln.Addr())
	}
	if portOpen("127.0.0.1:0") {
		t.Fatalf("任意空端口不应被判定为打开")
	}
}

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Fatalf("当前进程 pid 应视为存活")
	}
	if pidAlive(0) || pidAlive(-1) {
		t.Fatalf("非正 pid 不应视为存活")
	}
	// 一个极不可能存在的 pid（最大 pid 范围），不应存活。用超大值避免误撞。
	if pidAlive(1 << 30) {
		t.Fatalf("超大 pid 不应视为存活")
	}
}

func TestHealthVersionNoListener(t *testing.T) {
	// 借用临时监听端口释放后的端口：无监听 → 健康检查应快速返回 ""（不 hang、不 panic）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if got := healthVersion(addr); got != "" {
		t.Fatalf("无监听时 healthVersion 应为空串，got %q", got)
	}
}

func TestPidByPortNoListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if got := pidByPort(addr); got != 0 {
		t.Fatalf("无监听时 pidByPort 应为 0，got %d", got)
	}
}

func TestPidByPortInvalidAddr(t *testing.T) {
	if got := pidByPort("not-an-addr"); got != 0 {
		t.Fatalf("非法地址 pidByPort 应为 0，got %d", got)
	}
}
