package engine

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"

	"uws/pkg/browser"
)

// axShadowLookup 导出页面 AX 树并记录影子统计。与之前纯旁路观测不同，函数现在
// 还返回目标相关、受限且移除属性块的 AX 内容摘录，供视觉 VLM 辅助理解；不返回
// 可交互 ref 树，避免把表单当前值等内容附加到 VLM 请求。坐标仍由截图决定。
// 聚合统计：scripts/ax-shadow-report.sh。
func (s *Server) axShadowLookup(ctx context.Context, bc *browser.BrowserContext, semantic, reason string, candCount int) string {
	exporter, ok := bc.Session().(browser.PageTreeExporter)
	if !ok {
		log.Printf("[AX-SHADOW] semantic=%q reason=%s cand=%d ax=unsupported", semantic, reason, candCount)
		return ""
	}
	start := time.Now()
	tree, err := exporter.ExportPageTree(ctx)
	elapsed := time.Since(start)
	if err != nil {
		log.Printf("[AX-SHADOW] semantic=%q reason=%s cand=%d ax=error err=%q ms=%d",
			semantic, reason, candCount, err.Error(), elapsed.Milliseconds())
		return ""
	}
	st := axTreeStats(tree, semantic)
	log.Printf("[AX-SHADOW] semantic=%q reason=%s cand=%d ax=ok lines=%d ref_lines=%d hit=%t hit_lines=%d ref_hit=%t ms=%d",
		semantic, reason, candCount, st.lines, st.refLines, st.hitLines > 0, st.hitLines, st.refHit, elapsed.Milliseconds())
	return axEvidenceExcerpt(tree, semantic)
}

const axEvidenceMaxChars = 1800

// axEvidenceExcerpt 从现有 AX 内容树中提取目标命中行和最近的前置标题。
// 不包含交互 ref 树（其中 input 行可能带当前输入值），并移除内容节点的属性块
// （例如 textbox 的 value=...）。摘录只帮助理解语义；坐标仍必须来自截图。
func axEvidenceExcerpt(tree, semantic string) string {
	needle := strings.ToLower(strings.TrimSpace(semantic))
	if needle == "" {
		return ""
	}
	content := tree
	if i := strings.Index(content, "\n# 可交互节点"); i >= 0 {
		content = content[:i]
	}
	lines := strings.Split(content, "\n")
	var heading string
	var selected []string
	seen := map[string]bool{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if axRefLineRe.MatchString(line) {
			continue
		}
		if strings.HasPrefix(trimmed, "- heading ") {
			heading = line
		}
		if !strings.Contains(strings.ToLower(trimmed), needle) {
			continue
		}
		if heading != "" && !seen[heading] {
			selected = append(selected, heading)
			seen[heading] = true
		}
		// AX 内容树的属性在可访问名后以方括号附加；整段移除可避免把
		// textbox value、链接 URL 等额外字段发送给 VLM。
		if i := strings.Index(line, " ["); i >= 0 {
			line = line[:i]
		}
		if !seen[line] {
			selected = append(selected, line)
			seen[line] = true
		}
	}
	if len(selected) == 0 {
		return ""
	}
	var b strings.Builder
	for _, line := range selected {
		if b.Len()+len(line)+1 > axEvidenceMaxChars {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

// axRefLineRe 匹配可交互 ref 行前缀（`[node_id] tag(role) "text"`，
// 见 pkg/browser/probe.go 的 ExportToVLMFormat）。
var axRefLineRe = regexp.MustCompile(`^\s*\[\d+\]`)

// axTreeStats 统计语义树行数，以及目标语义命中的行（内容行 / 可交互 ref 行）。
// 目标语义做大小写不敏感子串匹配（中文无大小写，英文 ASCII 折叠），
// 与 matcher 的文本命中口径一致，属于「确定性可读」的下界估计。
type axTreeStat struct {
	lines    int  // 有效行（非标题、非空行）
	refLines int  // 可交互 ref 行（带 [node_id]，可确定性引用）
	hitLines int  // 命中目标语义的行
	refHit   bool // 命中行里是否含可交互 ref 行
}

func axTreeStats(tree, semantic string) axTreeStat {
	var st axTreeStat
	needle := strings.ToLower(strings.TrimSpace(semantic))
	for _, ln := range strings.Split(tree, "\n") {
		if strings.TrimSpace(ln) == "" || strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		st.lines++
		isRef := axRefLineRe.MatchString(ln)
		if isRef {
			st.refLines++
		}
		if needle == "" {
			continue
		}
		if strings.Contains(strings.ToLower(ln), needle) {
			st.hitLines++
			if isRef {
				st.refHit = true
			}
		}
	}
	return st
}
