package main

import (
	"fmt"

	"uws/pkg/engine"
)

// buildVersion 是 uws CLI 自身的构建版本。构建脚本用
// -ldflags "-X main.buildVersion=<VERSION>+<sha>" 注入；未注入时回落到 engine.EngineVersion，
// 这样手工 go build 也能和引擎版本天然对齐（同一源码，同一默认版本）。
var buildVersion = engine.EngineVersion

// runVersion 打印 CLI/引擎/知识库兼容基准版本（`uws version` 或 `uws --version`）。
func runVersion() {
	fmt.Printf("uws CLI  : %s\n", buildVersion)
	fmt.Printf("引擎基准 : %s\n", engine.EngineVersion)
	fmt.Println("用法提示 : uws daemon status 可核对运行中引擎的实际 HealthCheck 版本；")
	fmt.Println("          `make build && uws daemon restart` 可确保引擎与源码版本一致。")
}
