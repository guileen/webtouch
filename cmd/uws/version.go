package main

import (
	"fmt"

	"uws/pkg/engine"
)

// buildVersion 是 webtouch CLI 自身的构建版本。构建脚本用
// -ldflags "-X main.buildVersion=<VERSION>+<sha>" 注入；未注入时回落到 engine.EngineVersion，
// 这样手工 go build 也能和引擎版本天然对齐（同一源码，同一默认版本）。
var buildVersion = engine.EngineVersion

// runVersion 打印 CLI/引擎/知识库兼容基准版本（`webtouch version` 或 `webtouch --version`）。
func runVersion() {
	fmt.Printf("webtouch CLI  : %s\n", buildVersion)
	fmt.Printf("引擎基准     : %s\n", engine.EngineVersion)
	fmt.Println("兼容别名     : uws（已弃用，同一二进制）")
	fmt.Println("用法提示     : webtouch daemon status 可核对运行中引擎的实际 HealthCheck 版本；")
	fmt.Println("              `make build && webtouch daemon restart` 可确保引擎与源码版本一致。")
}
