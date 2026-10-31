package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	"uws/pkg/contract"
)

// preflightWaits 是「提交前」阶段遇到页面未就绪类失败时的重试间隔（次数少、间隔短）。
// 就绪竞态通常一两秒内自行消失；重试的意义是不把一次瞬时失败抛成 500，也不让它被
// 记账成站点级 15 分钟 unknown（见 authstatus.go 的 AuthCheckTTL）。
var preflightWaits = []time.Duration{400 * time.Millisecond, 900 * time.Millisecond}

// authSettleAfterRebuild 是就绪类失败后重建标签页、等待页面可用的时长。
const authSettleAfterRebuild = 1200 * time.Millisecond

// readinessMarkers 是「页面/标签页尚未就绪」类失败的特征串：标签页被销毁、CDP
// 会话未就绪、模型菜单尚未渲染、登录态探测未就绪。它们都发生在尚未提交的阶段，
// 重放安全（幂等或明确未派发）。
var readinessMarkers = []string{
	"target_destroyed",
	"cdp error -32001",
	"Session with given id not found",
	"模型菜单未出现",
	"model_switch_failed",
	"登录态探测未就绪",
}

// isSubmissionUnknown 判定错误是否属于「提交结果未知」——这类错误绝不重放
// （T-26 fail-closed；见 chat_submission.go 的 submissionError）。
func isSubmissionUnknown(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "submission_outcome_unknown") ||
		strings.Contains(msg, "physical_event_dispatched_outcome_unknown")
}

// isPreflightReadinessFailure 判定错误是否属于「页面/标签页尚未就绪」类。
// 提交结果未知一律排除在外，绝不重放。
func isPreflightReadinessFailure(err error) bool {
	if err == nil || isSubmissionUnknown(err) {
		return false
	}
	msg := err.Error()
	for _, m := range readinessMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	// 引擎 ABORTED 且没有提交语义：动作没有完成，提交前阶段可安全重放。
	var ee *EngineError
	return errors.As(err, &ee) && ee.Code == codeAborted
}

// isRetryableRoundFailure 判定是否值得「重建标签页后整轮重试一次」。
// 只覆盖提交前失败：标签页被销毁 / 就绪竞态 / 模型菜单未出现。
func isRetryableRoundFailure(err error) bool {
	if err == nil || isSubmissionUnknown(err) {
		return false
	}
	return IsTargetDestroyed(err) || isPreflightReadinessFailure(err)
}

// roundTimeoutMarkers 是「本轮在预算内没跑完」的特征串：整轮 ctx 到期（引擎
// code=4 context deadline exceeded）或回答轮询预算耗尽。它们**不是**站点返回的
// 业务错误，而是本网关自己的超时闸——2026-10-03 实测（派单 d1957d57b）同一 prompt、
// 同一会话连打 6 次，gemini-flash 单轮耗时在 31–85s 之间摆动，60s 预算会切掉一部分。
var roundTimeoutMarkers = []string{
	"context deadline exceeded",
	"超时预算内未确认新回答已完成",
}

// isRoundTimeout 判定错误是否属于「本轮超时」类。
func isRoundTimeout(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, m := range roundTimeoutMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// isRetryableChatFailure 是**整轮重试**（退避 → 重建页签 → 重放）的判据：
// ① 提交前就绪类失败（写操作未发生，T-26 允许）——默认口径；
// ② 本轮超时类失败——**仅在显式放宽时**纳入。
// ② 的重放对站点是「可能重复一轮」：超时不能证明写没发生（chat_submission.go）。
// 故默认 g.chatRetryOnTimeout＝false，只重试①，保住 T-26 fail-closed 不变量；
// 需要时用 UWS_CHAT_RETRY_ON_TIMEOUT=1 显式放宽（自担重复风险）。这是本函数与
// isRetryableRoundFailure 的差别所在。
func (g *Gateway) isRetryableChatFailure(err error) bool {
	if err == nil {
		return false
	}
	// ② 显式放宽（UWS_CHAT_RETRY_ON_TIMEOUT=1）：超时类即使同时带「提交结果未知」也重放，
	// 调用方自担重复风险（这是②优先于 T-26 硬边界的唯一口子，见上一段注释）。
	if g.chatRetryOnTimeout && isRoundTimeout(err) {
		return true
	}
	// ① 提交结果未知：T-26 硬边界，绝不重放。
	if isSubmissionUnknown(err) {
		return false
	}
	// 分类器是重试判据的主来源：page_not_ready / session_binding_dead / engine_unavailable
	// 都是提交前失败（写未发生或目标不存在），可安全重建重放。
	if classifyChatError(err).Retryable {
		return true
	}
	// 兜底保留旧 marker 判据（分类器未覆盖的形态）。
	if isRetryableRoundFailure(err) {
		return true
	}
	return false
}

// executePreflightRetry 与 executeWithRetry 相同，但对就绪类失败做有界重试。
// 只允许用于提交前动作（模型/思考/搜索开关、登录态探测、取基线）：这些动作要么
// 幂等、要么明确未派发，重放不会造成二次发送。
func (g *Gateway) executePreflightRetry(ctx context.Context, sid string, p *ChatProvider,
	intentID, semantic, action, value string, hint map[string]string) (*ActionResult, error) {

	var (
		res *ActionResult
		err error
	)
	for attempt := 0; ; attempt++ {
		res, err = g.executeWithRetry(ctx, sid, p, intentID, semantic, action, value, hint)
		if err == nil || attempt >= len(preflightWaits) || !isPreflightReadinessFailure(err) {
			return res, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, err
		}
		g.sleep(preflightWaits[attempt])
	}
}

// evalAuthProbe 执行只读登录态探测；就绪类失败时原地重试（不重建标签页）。
func (g *Gateway) evalAuthProbe(ctx context.Context, eval contract.DirectScriptEvaluator) ([]byte, error) {
	var (
		raw []byte
		err error
	)
	for attempt := 0; ; attempt++ {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		raw, err = eval.EvaluateScript(probeCtx, authProbeScript)
		cancel()
		if err == nil || attempt >= len(preflightWaits) || !isPreflightReadinessFailure(err) {
			return raw, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return raw, err
		}
		g.sleep(preflightWaits[attempt])
	}
}
