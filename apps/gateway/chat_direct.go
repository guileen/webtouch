package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"uws/apps/gateway/recipe"
	"uws/pkg/browser/modelbridge"
)

// tryDirectChat returns handled=false only when fetch provably never started.
// 实测档位标记（契约 §13「逐层降级阶梯」可观测性）：L0 = 页内直发，L1 = 真实网页执行。
const (
	transportInPageDirect = "in_page_direct"
	transportDOM          = "dom"
)

// 返回 (answer, siteConvID, handled, err)：siteConvID 是站点会话 ID 的原始读数
// （契约 §9 取值来源② 响应体自报，② 读不到时取 ③ recipe 会话槽位的 echo 回显），
// 读不到为空串；校验与归一化由调用方
// 走 NormalizeSiteConversationID（此处不判形状，避免把站点耦合进传输层）。
func (g *Gateway) tryDirectChat(ctx context.Context, p *ChatProvider, req ChatRequest, content string, out chan<- ChatEvent) (string, string, bool, error) {
	if IsVideoModel(req.Model) {
		return "", "", false, nil
	}
	if !CanDirectTransport(p.Site) {
		return "", "", false, nil
	}
	eval := g.EvaluatorForSession(req.SessionID)
	if eval == nil {
		return "", "", false, nil
	}
	native, ok := BuildDirectTransportRequest(p.Site, content, req.Model)
	if !ok {
		return "", "", false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, g.TimeoutForModel(req.Model))
	defer cancel()
	chunks, err := ExecuteInPageDirectStream(ctx, eval, native)
	if errors.Is(err, errDirectNotDispatched) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", true, submissionError(err)
	}

	sink := modelbridge.NewSink(1)
	var text strings.Builder
	done := false
	var echoSlots map[string]string
	// 分帧判据：recipe 声明了 response.kind 就按声明分帧，不再靠首块前缀猜——
	// DeepSeek 的流首块是 "event: ready"，前缀猜测会让协议帧原样漏进回答。
	framedStream := native.Recipe != nil && strings.TrimSpace(native.Recipe.Response.Kind) != ""
	// 轮询补正文（ChatGPT）：主流只是「前奏」，正文以单条 JSON 行回传，不能喂给分帧器。
	pollStream := recipe.IsPollResponse(native.Recipe)

	for chunk := range chunks {
		if len(chunk.Slots) > 0 {
			echoSlots = chunk.Slots
		}
		if chunk.Error != "" {
			return "", "", true, submissionError(errors.New(chunk.Error))
		}
		if chunk.Delta != "" && pollStream {
			if perr, isErr := recipe.PollErrorFromChunk(chunk.Delta); isErr {
				return "", "", true, submissionError(fmt.Errorf("in_page_direct: %s", perr))
			}
			if answer, ok := recipe.PollAnswerFromChunk(chunk.Delta); ok {
				text.WriteString(answer)
				if out != nil {
					select {
					case out <- ChatEvent{Delta: answer, Role: true}:
					case <-ctx.Done():
						return "", "", true, submissionError(ctx.Err())
					}
				}
				done = true
			}
		} else if chunk.Delta != "" {
			trimmed := strings.TrimSpace(chunk.Delta)
			if !framedStream && (strings.HasPrefix(trimmed, "data:") ||
				strings.HasPrefix(trimmed, ")]}'") ||
				strings.Contains(chunk.Delta, "[[\"wrb.fr\"") ||
				strings.Contains(chunk.Delta, "[[\"e\"")) {
				framedStream = true
			}

			if framedStream {
				sink.Feed([]byte(chunk.Delta))
				for _, c := range sink.Drain() {
					if c.Content != "" {
						first := text.Len() == 0
						text.WriteString(c.Content)
						if out != nil {
							select {
							case out <- ChatEvent{Delta: c.Content, Role: first}:
							case <-ctx.Done():
								return "", "", true, submissionError(ctx.Err())
							}
						}
					}
				}
				if sink.Snapshot().Done {
					done = true
				}
			} else {
				first := text.Len() == 0
				text.WriteString(chunk.Delta)
				if out != nil {
					select {
					case out <- ChatEvent{Delta: chunk.Delta, Role: first}:
					case <-ctx.Done():
						return "", "", true, submissionError(ctx.Err())
					}
				}
			}
		}
		done = done || chunk.Done
	}
	if err := ctx.Err(); err != nil {
		return "", "", true, submissionError(err)
	}
	if !done || strings.TrimSpace(text.String()) == "" {
		return "", "", true, submissionError(fmt.Errorf("in_page_direct: incomplete or empty response"))
	}
	return text.String(), siteConversationIDFromDirect(native.Recipe, sink.Snapshot().ConversationID, echoSlots), true, nil
}

// siteConversationIDFromDirect 求 L0 直发的站点会话 ID 原始读数（契约 §9 取值来源②/③）：
// ② 响应体自报优先（Gemini 走这条）；② 读不到时取 ③ recipe 会话槽位（producer: session_mint
// 且 echo: true）在启动响应里的回显值——它是本轮请求体实际写进去的那个 ID（DeepSeek 走这条）。
// 两类都不判形状；校验与归一化由调用方走 NormalizeSiteConversationID（fail-closed）。
func siteConversationIDFromDirect(r *recipe.Recipe, fromResponse string, slots map[string]string) string {
	if v := strings.TrimSpace(fromResponse); v != "" {
		return v
	}
	for _, name := range r.SessionEchoSlotNames() {
		if v := strings.TrimSpace(slots[name]); v != "" {
			return v
		}
	}
	return ""
}
