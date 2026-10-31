package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeMoli(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "moli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestFetchMappingAndJSON(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARG_LOG", log)
	t.Setenv("WEBTOUCH_MOLI_BIN", fakeMoli(t, `printf '%s\n' "$@" > "$ARG_LOG"; printf '{"title":"ok"}'`))
	var out bytes.Buffer
	if code := runFetch([]string{"--url", "https://example.com/a", "--format", "json", "--timeout", "3000"}, &out); code != 0 {
		t.Fatal(out.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["contract"] != fetchContract || got["backend"] != "moli" || got["content"].(map[string]any)["title"] != "ok" {
		t.Fatalf("%v %s", err, out.String())
	}
	args, _ := os.ReadFile(log)
	if string(args) != "fetch\n--dump\njson\n--timeout\n3000\n--\nhttps://example.com/a\n" {
		t.Fatalf("args: %s", args)
	}
}
func TestFetchFailures(t *testing.T) {
	cases := []struct {
		name, script string
		args         []string
		code         string
	}{
		{"exit", `echo secret >&2; exit 7`, nil, "backend_failed"},
		{"bad json", `printf no`, []string{"--format", "json"}, "invalid_backend_output"},
		{"empty", `:`, nil, "invalid_backend_output"},
		{"timeout", `exec sleep 2`, []string{"--timeout", "30"}, "backend_timeout"},
		{"session", `:`, []string{"--session", "abc"}, "invalid_argument"},
		{"vault", `:`, []string{"--url", "https://example.com/@vault:secret"}, "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WEBTOUCH_MOLI_BIN", fakeMoli(t, tc.script))
			args := append([]string{"--url", "https://example.com"}, tc.args...)
			var out bytes.Buffer
			if runFetch(args, &out) != 1 {
				t.Fatal(out.String())
			}
			var got fetchResult
			if json.Unmarshal(out.Bytes(), &got) != nil || got.Error == nil || got.Error.Code != tc.code || strings.Contains(out.String(), "secret") {
				t.Fatal(out.String())
			}
		})
	}
	t.Setenv("WEBTOUCH_MOLI_BIN", filepath.Join(t.TempDir(), "missing"))
	if got := fetch([]string{"--url", "https://example.com"}); got.Error.Code != "backend_missing" {
		t.Fatal(got)
	}
}
func TestFetchScreenshot(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARG_LOG", log)
	t.Setenv("WEBTOUCH_MOLI_BIN", fakeMoli(t, `printf '%s\n' "$@" > "$ARG_LOG"; printf '\211PNG\r\n\032\nimage'`))
	path := filepath.Join(t.TempDir(), "page.png")
	got := fetch([]string{"--url", "https://example.com", "--format", "screenshot", "--output", path})
	if !got.OK || got.Artifact != path {
		t.Fatal(got)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "--layout\n--\n") {
		t.Fatal(string(args))
	}
}
