package gateway

import (
	"context"
	"time"
)

// runChatWithRetry 执行一轮网页对话，并按可配常量做有界整轮重试。
//
// 重试判据见 isRetryableChatFailure：默认只覆盖「提交前就绪类失败」（写操作未发生，
// 可安全重放）；「本轮超时类失败」默认不重试，因为超时不能证明写未发生，重放可能让
// 站点重复答一轮（chat_submission.go 的 T-26 fail-closed 不变量），需显式
// UWS_CHAT_RETRY_ON_TIMEOUT=1 放宽。
//
// 每一次尝试的结局都落日志（http 语义码恒为 502/0，所以同时记 dur 与 err 原文）——
// 2026-10-03 派单 d1957d57b 的观测要求「逐次尝试的 http 与耗时落日志」。
//
// 上限/退避/是否重试超时均为可配常量，见 gateway.go 的 chatRetry*FromEnv 与
// defaultChatRetry* 常量（Options.ChatRetryAttempts/Backoff 可覆盖）。
func (g *Gateway) runChatWithRetry(ctx context.Context, req ChatRequest, p *ChatProvider) (*ChatCompletion, error) {
	var out *ChatCompletion
	var err error
	totalAttempts := g.chatRetryAttempts + 1
	for attempt := 1; ; attempt++ {
		startedAt := time.Now()
		out, err = g.runChatRound(ctx, req, p)
		if err == nil {
			if attempt > 1 {
				g.logger.Printf("chat 重试成功 sid=%s model=%s attempt=%d/%d dur=%.1fs",
					req.SessionID, req.Model, attempt, totalAttempts, time.Since(startedAt).Seconds())
			}
			return out, nil
		}
		retryable := g.isRetryableChatFailure(err)
		g.logger.Printf("chat 失败 sid=%s model=%s attempt=%d/%d http=%d dur=%.1fs retryable=%v err=%v",
			req.SessionID, req.Model, attempt, totalAttempts, chatErrorStatus(err), time.Since(startedAt).Seconds(), retryable, err)
		if attempt >= totalAttempts || !retryable {
			return out, err
		}
		if backoff := g.chatRetryBackoff * time.Duration(attempt); backoff > 0 {
			g.logger.Printf("chat 退避 %.1fs 后重建页签重试 sid=%s attempt=%d→%d",
				backoff.Seconds(), req.SessionID, attempt, attempt+1)
			g.sleep(backoff)
		}
		g.closeSession(ctx, req.SessionID)
		// 绑定失活时网关没有内存会话，closeSession 只收尾账本、不会动引擎落盘绑定
		// （见其注释）——不显式清就会拿同一条死绑定再失败一次。其余就绪类失败走
		// closeSession 的 CloseContext 即可，不必多打一次引擎。
		if classifyChatError(err).Code == errCodeSessionBindingDead {
			g.dropEngineBindings(ctx, req.SessionID)
		}
		if _, e := g.ensureSessionForRetry(ctx, req); e != nil {
			g.logger.Printf("chat 重建会话失败 sid=%s attempt=%d/%d err=%v",
				req.SessionID, attempt+1, totalAttempts, e)
			return out, err
		}
		if p.NewChatSettleSec > 0 {
			g.sleep(durationSec(p.NewChatSettleSec))
		}
	}
}

// ensureSessionForRetry 在整轮重试里重建会话，并对「网关→引擎这一段不可达」做**有界快重拨**：
// 引擎短暂重启（秒级）会被自动吸收，不必把失败立刻推给调用方；其它错误（含绑定失活、
// 页签问题）立即返回，交回原重试路径处理。重拨只重连引擎、不重放对话，故不触碰 T-26。
func (g *Gateway) ensureSessionForRetry(ctx context.Context, req ChatRequest) (*Session, error) {
	ch := g.cfg.Channel(req.ChannelID)
	var lastErr error
	for i := 1; i <= engineUnavailableRetries; i++ {
		sess, err := g.ensureSession(ctx, ch, req.SessionID, req.SiteConversationID)
		if err == nil {
			return sess, nil
		}
		lastErr = err
		if classifyChatError(err).Code != errCodeEngineUnavailable || i == engineUnavailableRetries {
			return nil, err
		}
		g.logger.Printf("chat 引擎仍不可达 sid=%s 重拨 %d/%d 退避 %.0fms",
			req.SessionID, i+1, engineUnavailableRetries, float64(engineUnavailableBackoff.Milliseconds()))
		g.sleep(engineUnavailableBackoff)
	}
	return nil, lastErr
}
