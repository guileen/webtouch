package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"
	"uws/pkg/browser"
)

// ── 脱敏硬过滤（条款 3）──────────────────────────────────────────

// episodeRedactRule 是单条脱敏规则（命中换占位符，未命中不动字节）。
type episodeRedactRule struct {
	placeholder string
	re          *regexp.Regexp
}

// 顺序敏感：Authorization 头先于裸 Bearer，避免把 "Bearer" 当成 token 值而漏掉真值。
var episodeRedactRules = []episodeRedactRule{
	{
		// Authorization 头：整段值都是凭据（含 Basic base64 载荷），一次吃干净。
		placeholder: "[REDACTED:TOKEN]",
		re:          regexp.MustCompile(`(?i)authorization\s*[:=]\s*(?:[A-Za-z0-9._\-+/=]{4,}[\s,]*)+`),
	},
	{
		placeholder: "[REDACTED:TOKEN]",
		re:          regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-+/=]{6,}`),
	},
	{
		// 键值形态的凭据（`"password":"..."` / `token: xxx` / `api_key=...`）。
		// 要求 `[:=]` 分隔符：宁可漏，不在散文里误伤（误伤会破坏特征可重算性）。
		placeholder: "[REDACTED:TOKEN]",
		re:          regexp.MustCompile(`(?i:["']?(?:access[_-]?token|refresh[_-]?token|api[_-]?key|apikey|token|secret|passwd|password|pwd)["']?\s*[:=]\s*["']?[A-Za-z0-9._\-+/=]{3,}["']?)`),
	},
	{
		placeholder: "[REDACTED:JWT]",
		re:          regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\b`),
	},
	{
		placeholder: "[REDACTED:EMAIL]",
		re:          regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`),
	},
	{
		placeholder: "[REDACTED:UUID]",
		re:          regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`),
	},
	{
		placeholder: "[REDACTED:PHONE]",
		re:          regexp.MustCompile(`\b1[3-9]\d{9}\b`),
	},
	{
		placeholder: "[REDACTED:PHONE]",
		re:          regexp.MustCompile(`\b\d{3}[-.]\d{3}[-.]\d{4}\b`),
	},
	{
		placeholder: "[REDACTED:PHONE]",
		re:          regexp.MustCompile(`\+\d{1,3}[\s\-]?\d{2,4}[\s\-]?\d{3,4}[\s\-]?\d{3,4}\b`),
	},
}

// episodeWideNumericRe 是用户口径的宽数字串（§5.0.13 条款 4①）：10–18 位、可带 `+` 前缀。
var episodeWideNumericRe = regexp.MustCompile(`\+?[0-9]{10,18}`)

// episodeNumericAdjacent 判断一个字符是否属于「数字串的紧邻」（数字或小数点）。
func episodeNumericAdjacent(r rune) bool {
	return (r >= '0' && r <= '9') || r == '.'
}

// maskWideNumericRuns 执行 §5.0.13 条款 4③ 的宽数字串遮蔽：**最后**执行，且要求前后均为
// 非数字、非小数点（避免吞噬 UUID / 日期 / 小数片段）。Go RE2 无 lookaround，故按 rune
// 边界手工判定。已知代价（条款 4③ 显式声明）：13 位毫秒时间戳串会被遮蔽。
func maskWideNumericRuns(s string) (string, bool) {
	ms := episodeWideNumericRe.FindAllStringIndex(s, -1)
	if len(ms) == 0 {
		return s, false
	}
	var b strings.Builder
	hit, last := false, 0
	for _, m := range ms {
		if m[0] < last {
			continue
		}
		if m[0] > 0 {
			if r, _ := utf8.DecodeLastRuneInString(s[:m[0]]); episodeNumericAdjacent(r) {
				continue
			}
		}
		if m[1] < len(s) {
			if r, _ := utf8.DecodeRuneInString(s[m[1]:]); episodeNumericAdjacent(r) {
				continue
			}
		}
		b.WriteString(s[last:m[0]])
		b.WriteString("[REDACTED:PHONE]")
		last = m[1]
		hit = true
	}
	if !hit {
		return s, false
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// sanitizeEpisodeText 对单段文本执行脱敏硬过滤，返回过滤结果与是否命中。
// 未命中时原样返回（字节不变），保证 §5.0.1 特征可重算。
func sanitizeEpisodeText(s string) (string, bool) {
	out := s
	hit := false
	for _, rule := range episodeRedactRules {
		if !strings.Contains(out, " ") && out == "" {
			break
		}
		if !rule.re.MatchString(out) {
			continue
		}
		hit = true
		out = rule.re.ReplaceAllLiteralString(out, rule.placeholder)
	}
	// §5.0.13 条款 4①/③：宽数字串口径**最后**执行（顺序敏感）。
	if masked, wideHit := maskWideNumericRuns(out); wideHit {
		hit = true
		out = masked
	}
	return out, hit
}

// truncateEpisodeRunes 按 rune 截断到 n，返回截断结果与截断标记。
func truncateEpisodeRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	return string([]rune(s)[:n]), true
}

// episodeIdentityHash 生成元素身份短哈希（selector/class 只落哈希，不落原文）。
func episodeIdentityHash(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:12]
}

// ── 故障代号提取（§5.0.8 代号表口径）─────────────────────────────

// episodeFaultCodes 是拒识/失败口径的结构化代号白名单（顺序=特异性高→低）。
var episodeFaultCodes = []string{
	FaultVLMActionMismatch,
	browser.FaultVLMlandingUnverified,
	browser.FaultTargetRoleMismatch,
	browser.FaultTargetTopologyRupture,
	browser.FaultInputOccluded,
	browser.FaultTargetNotEditable,
	browser.FaultWriteNotPersisted,
	browser.FaultLocateNoCandidate,
	browser.FaultCSPWasmBlocked,
	"challenge_intercepted",
}

// episodeFailureCode 从观测串里提取结构化代号；没有代号时返回空串（不编造）。
// 条款 4：拒识事件只认物理层结构化代号，不得以 SUCCEEDED 计数。
func episodeFailureCode(observed string) string {
	if observed == "" {
		return ""
	}
	for _, code := range episodeFaultCodes {
		if strings.Contains(observed, code) {
			return code
		}
	}
	return ""
}
