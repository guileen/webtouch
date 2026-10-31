package main

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"uws/pkg/engine"
	uwspb "uws/proto/gen/go/proto/v1"
)

// healthVersion 查询 addr 上引擎的 HealthCheck，返回运行中的实际引擎版本。
// 引擎默认（daemon 启动未指定 --api-key-file）使用开发内置 DevAPIKey，故此处带该头；
// 若引擎用自定义 key file 启动导致鉴权失败，返回 ""（调用方降级为「无法读取运行版本」）。
func healthVersion(addr string) string {
	if !portOpen(addr) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return ""
	}
	defer conn.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel2()
	ctx2 = metadata.AppendToOutgoingContext(ctx2, "x-api-key", engine.DevAPIKey)
	resp, err := uwspb.NewUwsEngineClient(conn).HealthCheck(ctx2, &uwspb.Empty{})
	if err != nil || resp == nil {
		return ""
	}
	return resp.GetVersion()
}

// pidByPort 用 lsof 反查监听 addr 的进程 pid（用于回收「未托管在 meta 的孤儿/外部引擎」）。
// lsof 不可用或查不到返回 0（调用方降级为只提示、不回收）。
func pidByPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return 0
	}
	out, err := exec.Command("lsof", "-ti", "tcp:"+port, "-sTCP:LISTEN").Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0
	}
	pid, err := strconv.Atoi(strings.SplitN(line, "\n", 2)[0])
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
