package main

import (
	"strings"
	"testing"
)

func TestDeprecatedAliasNotice(t *testing.T) {
	if got := deprecatedAliasNotice("uws"); !strings.Contains(got, "已弃用") || !strings.Contains(got, "webtouch") {
		t.Fatalf("uws argv0 应提示弃用, got %q", got)
	}
	if got := deprecatedAliasNotice("/usr/local/bin/uws"); !strings.Contains(got, "已弃用") {
		t.Fatalf("路径形式的 uws 也应提示, got %q", got)
	}
	if got := deprecatedAliasNotice("uws.exe"); !strings.Contains(got, "已弃用") {
		t.Fatalf("uws.exe 也应提示, got %q", got)
	}
	if got := deprecatedAliasNotice("webtouch"); got != "" {
		t.Fatalf("webtouch 不应提示, got %q", got)
	}
	if got := deprecatedAliasNotice("uws-engine"); got != "" {
		t.Fatalf("引擎文件名不应被当成 CLI 别名, got %q", got)
	}
}
