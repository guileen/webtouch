package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ── 证据落盘 ────────────────────────────────────────────────────────

func auditWriteEvidence(outDir string, rep *auditReport) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "audit.json"), data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "audit.md"), []byte(auditMarkdown(rep)), 0o644)
}

func auditMarkdown(rep *auditReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 指纹外站验收（%s，%s）\n\n", rep.StartedAt, rep.Mode)
	fmt.Fprintf(&b, "- Chrome: `%s`（%s）\n- 系统: macOS %s\n- 档案来源: %s\n- 追加启动参数: %v\n- 硬失败: **%d**\n\n",
		rep.ChromePath, rep.ChromeVersion, rep.OSVersion, rep.ProfileSource, rep.ChromeArgs, rep.HardFails)
	b.WriteString("## 复跑命令\n\n```sh\ncd <webtouch>\nUWS_RUN_EXTERNAL_AUDIT=1 go test ./pkg/browser/ -run TestExternalFingerprintAudit -v -count=1\n# 有头（会开窗、碰桌面）: 再加 UWS_AUDIT_HEADED=1\n```\n\n")

	b.WriteString("## 对账表\n\n| 类别 | 检查 | 档案/期望 | 实际 | 结果 |\n|---|---|---|---|---|\n")
	for _, c := range rep.Checks {
		mark := "✅"
		if !c.OK {
			mark = "❌"
			if c.Kind != "硬" {
				mark = "⚠️"
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", c.Kind, c.Name, auditClip(c.Want), auditClip(c.Got), mark)
	}

	for _, run := range []*auditRun{rep.Baseline, rep.Profiled} {
		if run == nil {
			continue
		}
		fmt.Fprintf(&b, "\n## %s 会话（出口 IP %s，%s）\n\n", run.Session, run.EgressIP, run.BrowserBuild)
		fmt.Fprintf(&b, "自报身份：`%s`\n\n", auditClip(auditJSONLine(run.Self)))
		for _, site := range run.Sites {
			fmt.Fprintf(&b, "### %s（%s，等待 %dms）\n\n", site.Name, site.URL, site.WaitedMS)
			if site.Err != "" {
				fmt.Fprintf(&b, "采集失败：%s\n\n", site.Err)
				continue
			}
			if site.Shot != "" {
				fmt.Fprintf(&b, "截图：`%s`\n\n", site.Shot)
			}
			fmt.Fprintf(&b, "```\n%s\n```\n\n", auditClip(auditJSONLine(site.Raw)))
		}
	}

	if len(rep.Notes) > 0 {
		b.WriteString("\n## 备注\n\n")
		for _, n := range rep.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	return b.String()
}

func auditClip(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 700 {
		s = s[:700] + "…"
	}
	return s
}

func auditJSONLine(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(data)
}

// ── 小工具 ──────────────────────────────────────────────────────────

// auditExtraArgs 读 UWS_AUDIT_CHROME_ARGS（空格或逗号分隔），追加到 Chrome 启动参数：
// 「换个启动参数，探测面差在哪」这件事必须能不开分支就复跑出来。
func auditExtraArgs() []string {
	raw := strings.TrimSpace(os.Getenv("UWS_AUDIT_CHROME_ARGS"))
	if raw == "" {
		return nil
	}
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
}

func auditSelectedSites(session string) []string {
	if v := strings.TrimSpace(os.Getenv("UWS_AUDIT_SITES")); v != "" {
		out := []string{}
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if session == "baseline" {
		// 基线固定跑轻量两项：出口 IP（证明直连）与 sannysoft（给你一张裸奔对照表）。
		return []string{"egress", "sannysoft"}
	}
	return []string{"egress", "sannysoft", "creepjs", "browserleaks"}
}

func auditOutDir(t *testing.T, mode string) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("UWS_AUDIT_OUT")); v != "" {
		return v
	}
	root := auditRepoRoot(t)
	return filepath.Join(root, "docs", "evidence", "fingerprint-audit", time.Now().Format("2006-01-02")+"-"+mode)
}

func auditRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return "/Users/gl/hz/aliyun-terraform/labs/agent-tools/webtouch"
}

func auditEvalJSON(ctx context.Context, s *cdpSession, expr string, dst *map[string]any) error {
	payload, err := s.evalResult(ctx, expr)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(payload), dst)
}

func auditBrowserBuild(ctx context.Context, s *cdpSession) string {
	var out struct {
		Product string `json:"product"`
	}
	if err := s.client.Call(ctx, "Browser.getVersion", nil, &out); err != nil {
		return ""
	}
	return out.Product
}

func auditLocalChromeVersion(path string) string {
	if path == "" {
		return ""
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "Google Chrome "))
}

func auditLocalOSVersion() string {
	data, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist")
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`(?s)<key>ProductVersion</key>\s*<string>([^<]+)</string>`)
	if m := re.FindStringSubmatch(string(data)); len(m) > 1 {
		return m[1]
	}
	return ""
}

func auditDirectEgressIP(ctx context.Context) string {
	client := &http.Client{Timeout: 12 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org/?format=json", nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	var out struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(body, &out) == nil && out.IP != "" {
		return out.IP
	}
	return strings.TrimSpace(string(body))
}

var auditIPRe = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)

func auditExtractIP(res auditSiteResult) string {
	obj, ok := res.Raw.(map[string]any)
	if !ok {
		return ""
	}
	if text, ok := obj["text"].(string); ok {
		if ip := auditIPRe.FindString(text); ip != "" {
			return ip
		}
	}
	return ""
}

func auditPick(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

func auditNested(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func auditNum(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case bool:
		if x {
			return 1
		}
		return 0
	}
	return 0
}

func auditStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		data, _ := json.Marshal(x)
		return string(data)
	case map[string]any:
		data, _ := json.Marshal(x)
		return string(data)
	default:
		return fmt.Sprintf("%v", x)
	}
}
