package engine

import (
	"regexp"
	"strings"
	"unicode/utf8"

	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 输入边界（spec/security-dx.md §2.2 / tdd-checklist §5 TDD-SEC-06）。
const (
	maxSemanticLen   = 120
	maxValueLen      = 4096
	maxInputValueLen = 256 * 1024
	maxHintKeyLen    = 64
	maxHintValLen    = 256
)

var (
	// semanticRe 允许：ASCII 字母/数字/下划线/连字符/点号 + 汉字 + 空格 +
	// 全角/CJK 标点（：，。！？（）、《》「」『』【】〔〕、—、“”、… 等）。
	// 仍拒绝 ASCII 危险字符（< > " ' / 等，防 XSS / 注入，test_sec06 依赖）。
	// 修复：2026-08-27 全角冒号「：」（U+FF1A）曾被拒，drama 完整标题语义进不来。
	// 修复：2026-08-28 ASCII 点号「.」（U+002E）曾被拒，viewCount 语义「25.8M」（数字+点+字母）进不来。
	semanticRe = regexp.MustCompile(`^[a-zA-Z0-9_\-\p{Han}\.\s\x{3000}\x{A0}\x{B7}\x{3001}\x{3002}\x{3008}-\x{3011}\x{3014}\x{3015}\x{301C}-\x{301F}\x{2013}-\x{2027}\x{FE30}-\x{FE4F}\x{FF01}\x{FF08}\x{FF09}\x{FF0C}\x{FF0E}\x{FF1A}\x{FF1B}\x{FF1F}\x{30FB}]+$`)
	hintKeyRe  = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
)

// validateIntent 校验 ActionIntentRequest 的输入边界，超限返回 INVALID_ARGUMENT。
func validateIntent(req *uwspb.ActionIntentRequest) error {
	if strings.TrimSpace(req.GetIntentId()) == "" {
		return status.Error(codes.InvalidArgument, "intent_id required")
	}
	sem := req.GetTargetSemantic()
	// 规格为「≤120 字符」：按 rune 计数，避免中文语义被字节长度误拒（R7）。
	if len(sem) == 0 || utf8.RuneCountInString(sem) > maxSemanticLen {
		return status.Errorf(codes.InvalidArgument, "target_semantic length must be 1..%d", maxSemanticLen)
	}
	// node_id 确定性引用（v1.5.5）或 evaluate 动作时 target_semantic 只是可读标签，
	// 不参与语义匹配，跳过字符白名单校验（避免 `:` 被误拒，GLOBAL §5 #57）。
	if !semanticRe.MatchString(sem) && req.GetNodeId() <= 0 && req.GetExpectedAction() != "evaluate" {
		return status.Error(codes.InvalidArgument, "target_semantic contains invalid characters")
	}
	// node_id 确定性引用（v1.5.5）：非负；>0 表示按 BFS 序回查执行，0 表示未设置。
	if req.GetNodeId() < 0 {
		return status.Error(codes.InvalidArgument, "node_id must be non-negative")
	}
	if v := req.GetValue(); v != "" {
		if req.GetExpectedAction() == "evaluate" {
			const maxEvaluateValueLen = 1024 * 1024 // 1MB：支持注入完整 fetch 与 stream 抽取脚本
			if len(v) > maxEvaluateValueLen {
				return status.Errorf(codes.InvalidArgument, "evaluate value length must be <= %d", maxEvaluateValueLen)
			}
			// evaluate 意图本身即为 JS 源码，允许并跳过 javascript: 前缀拦截
		} else {
			limit := maxValueLen
			if req.GetExpectedAction() == "input" {
				limit = maxInputValueLen
			}
			if len(v) > limit {
				return status.Errorf(codes.InvalidArgument, "value length must be <= %d", limit)
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "javascript:") {
				return status.Error(codes.InvalidArgument, "value must not start with javascript:")
			}
		}
	}
	for k, v := range req.GetContextHint() {
		if len(k) == 0 || len(k) > maxHintKeyLen || !hintKeyRe.MatchString(k) {
			return status.Errorf(codes.InvalidArgument, "context_hint key %q invalid", k)
		}
		if len(v) > maxHintValLen {
			return status.Errorf(codes.InvalidArgument, "context_hint value for %q too long", k)
		}
	}
	return nil
}
