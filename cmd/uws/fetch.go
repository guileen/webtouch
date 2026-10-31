package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const fetchContract = "webtouch.cli.fetch.v1"
const fetchLimit = 16 << 20

type fetchResult struct {
	OK       bool        `json:"ok"`
	Contract string      `json:"contract"`
	Backend  string      `json:"backend"`
	Format   string      `json:"format,omitempty"`
	Content  any         `json:"content,omitempty"`
	Artifact string      `json:"artifact,omitempty"`
	Error    *fetchError `json:"error,omitempty"`
}
type fetchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func fetchFail(code, message string) fetchResult {
	return fetchResult{Contract: fetchContract, Backend: "moli", Error: &fetchError{code, message}}
}

type cappedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) > fetchLimit-b.Len() {
		b.overflow = true
		return 0, errors.New("output limit")
	}
	return b.Buffer.Write(p)
}

func runFetch(args []string, out io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Fprintln(out, "用法: webtouch fetch --url <URL> [--format markdown|json|screenshot] [--timeout 毫秒] [--output 路径]")
		return 0
	}
	result := fetch(args)
	_ = json.NewEncoder(out).Encode(result)
	if !result.OK {
		return 1
	}
	return 0
}
func fetch(args []string) fetchResult {
	bad := func() fetchResult {
		return fetchFail("invalid_argument", "无效参数；请查看 webtouch fetch --help")
	}
	for _, a := range args {
		if strings.Contains(strings.ToLower(a), "@vault:") {
			return bad()
		}
	}
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rawURL := fs.String("url", "", "")
	format := fs.String("format", "markdown", "")
	timeout := fs.Int("timeout", 25000, "")
	output := fs.String("output", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return bad()
	}
	u, err := url.Parse(*rawURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || *timeout < 1 || *timeout > 120000 {
		return bad()
	}
	if *format != "markdown" && *format != "json" && *format != "screenshot" {
		return bad()
	}
	if (*format == "screenshot") != (*output != "") {
		return bad()
	}
	bin := os.Getenv("WEBTOUCH_MOLI_BIN")
	if bin == "" {
		bin = os.Getenv("MOLI_BIN")
	}
	if bin == "" {
		bin = "moli"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return fetchFail("backend_missing", "找不到可执行的 moli")
	}
	argv := []string{"fetch", "--dump", *format, "--timeout", strconv.Itoa(*timeout)}
	if *format == "screenshot" {
		argv = append(argv, "--layout")
	}
	argv = append(argv, "--", *rawURL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, argv...)
	var buf cappedBuffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	err = cmd.Run()
	if ctx.Err() != nil {
		return fetchFail("backend_timeout", "读取超时")
	}
	if buf.overflow {
		return fetchFail("output_too_large", "后端输出超过上限")
	}
	if err != nil {
		return fetchFail("backend_failed", "moli 执行失败")
	}
	data := buf.Bytes()
	result := fetchResult{OK: true, Contract: fetchContract, Backend: "moli", Format: *format}
	switch *format {
	case "markdown":
		if len(data) == 0 {
			return fetchFail("invalid_backend_output", "后端返回空内容")
		}
		result.Content = string(data)
	case "json":
		var value json.RawMessage
		if json.Unmarshal(data, &value) != nil {
			return fetchFail("invalid_backend_output", "后端返回无效 JSON")
		}
		result.Content = value
	case "screenshot":
		if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			return fetchFail("invalid_backend_output", "后端未返回 PNG")
		}
		if os.WriteFile(*output, data, 0600) != nil {
			return fetchFail("output_write_failed", "无法写入截图")
		}
		result.Artifact = *output
	}
	return result
}
