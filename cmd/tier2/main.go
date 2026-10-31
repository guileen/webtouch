// Tier 2 动态沙盒 Go 入口：用真实 Chrome/CDP 跑四维测量，输出 JSON 报告。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"uws/pkg/browser"
)

func main() {
	var (
		chromePath    = flag.String("chrome", "", "Chrome/Chromium 可执行文件路径（默认自动探测）")
		probeWasmPath = flag.String("probe-wasm", "", "WASM 探针路径（默认自动探测）")
		out           = flag.String("out", "build/tier2/tier2_metrics_go.json", "JSON 输出路径")
		timeoutSec    = flag.Int("timeout", 120, "整体运行超时秒数")
	)
	flag.Parse()

	report, err := browser.RunTier2(context.Background(), browser.Tier2Options{
		ChromePath:    *chromePath,
		ProbeWasmPath: *probeWasmPath,
		Timeout:       time.Duration(*timeoutSec) * time.Second,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tier2 run:", err)
		os.Exit(1)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal report:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir output:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
	fmt.Printf("Tier2 Go report written to %s\n", *out)
}
