package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"uws/pkg/contract"
)

// chatPollInterval / chatStreamPollInterval 与 Python api_server 轮询节奏一致。
const (
	chatPollInterval       = 800 * time.Millisecond
	chatStreamPollInterval = 250 * time.Millisecond
	answerSemanticKey      = "answer"
)

type chatStep struct {
	action   string
	semantic string
	value    string
}

// userContent 取 messages 最后一条 user/system/tool 内容（web 通道只发增量最后一条）。
func userContent(messages []Message) (string, error) {
	last := ""
	found := false
	for _, m := range messages {
		if m.Role == "user" || m.Role == "system" {
			last = m.Content
			found = true
		} else if m.Role == "tool" {
			last = FormatExecResultForWeb(m.Name, m.Content)
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("messages 缺少 user 消息")
	}
	return last, nil
}

// modelChangedRequiresNewSession：DeepSeek 等站点模式单选组只在「新会话」落地页出现，
// 请求模式 ≠ 会话已锁定模式时，即使客户端没传 new_session 也必须重建 Tab 兑现模式
// （2026-08-26 用户实测「专家模式没选对」根因）。会话尚未锁定（Model=""）按站点
// 默认模式（模型目录首项）参与比较。
func (g *Gateway) modelChangedRequiresNewSession(p *ChatProvider, model string, sess *Session) bool {
	if sess == nil || !p.ModelOnNewChatOnly {
		return false
	}
	label := p.resolveModel(model)
	recorded := sess.Model
	if recorded == "" {
		recorded = p.defaultModel()
	}
	return label != "" && recorded != "" && label != recorded
}

// ensureModel 站点模型切换：请求模型 ≠ 页面当前模型时执行 model_switch_semantic 物理动作。
// 模型态按会话记录（每个 Tab 独立）；调用方必须持有会话锁。
func (g *Gateway) ensureModel(ctx context.Context, p *ChatProvider, model, sid string) error {
	if p.ModelSwitchSemantic == "" {
		return nil
	}
	sess := g.getSession(sid)
	if sess == nil {
		return nil
	}
	label := p.resolveModel(model)
	if label == "" || label == sess.Model {
		return nil
	}
	if _, err := g.executePreflightRetry(ctx, sid, p,
		fmt.Sprintf("intent-uws-api-model-%d", time.Now().UnixNano()),
		p.ModelSwitchSemantic, "click", label, nil); err != nil {
		// 失败即查（2026-09-26）：菜单/选择器会随站点改版变化，只留一句
		// 「菜单未出现」无法归因，故立刻采一份当时真实的菜单现场（见 modelmenu.go）。
		if len(p.MenuFamilies) > 0 {
			return fmt.Errorf("%w；%s", err, g.modelMenuDigest(ctx, sid))
		}
		return err
	}
	sess.Model = label
	return nil
}

// ensureThinking 站点可选思考开关（Gemini Extended thinking / DeepSeek 深度思考）。
func (g *Gateway) ensureThinking(ctx context.Context, p *ChatProvider, thinking *bool, sid string) error {
	if p.ThinkingSemantic == "" || thinking == nil {
		return nil
	}
	sess := g.getSession(sid)
	if sess == nil || (sess.Thinking != nil && *sess.Thinking == *thinking) {
		return nil
	}
	value := "off"
	if *thinking {
		value = "on"
	}
	if _, err := g.executePreflightRetry(ctx, sid, p,
		fmt.Sprintf("intent-uws-api-thinking-%d", time.Now().UnixNano()),
		p.ThinkingSemantic, "click", value, nil); err != nil {
		return err
	}
	sess.Thinking = thinking
	return nil
}

// ensureSearch 站点可选联网/智能搜索开关（DeepSeek 智能搜索；专家模式物理层显式报错）。
func (g *Gateway) ensureSearch(ctx context.Context, p *ChatProvider, search *bool, sid string) error {
	if p.SearchSemantic == "" || search == nil {
		return nil
	}
	sess := g.getSession(sid)
	if sess == nil || (sess.Search != nil && *sess.Search == *search) {
		return nil
	}
	// 模型不支持联网/智能搜索（如 DeepSeek 专家模式）时忽略开关请求（on/off 都跳过），
	// 避免把不存在于页面的开关语义下发到物理层报错；会话记录按请求值标记，后续轮次不再尝试。
	if !p.modelSearchable(sess.Model) {
		g.logger.Printf("会话 %s: 模型 %q 不支持联网/智能搜索，忽略 search=%v", sid, sess.Model, *search)
		sess.Search = search
		return nil
	}
	value := "off"
	if *search {
		value = "on"
	}
	if _, err := g.executePreflightRetry(ctx, sid, p,
		fmt.Sprintf("intent-uws-api-search-%d", time.Now().UnixNano()),
		p.SearchSemantic, "click", value, nil); err != nil {
		return err
	}
	sess.Search = search
	return nil
}

// executeWithRetry 只为读取或已证明未派发的动作有界重试；提交未知不重放。
// context_hint 自动注入 context=<session_id>，把动作路由到该会话绑定的标签页。
func (g *Gateway) executeWithRetry(ctx context.Context, sid string, p *ChatProvider,
	intentID, semantic, action, value string, hint map[string]string) (*ActionResult, error) {

	hints := make(map[string]string, len(hint)+1)
	for k, v := range hint {
		hints[k] = v
	}
	if sid != "" {
		hints["context"] = sid
	}
	var (
		eng EngineClient
		err error
	)
	if sess := g.getSession(sid); sess != nil && sess.EngineAddr != "" {
		eng, err = g.engineForSession(sess)
	} else {
		ch := g.cfg.Channel(providerChannelID(p, g))
		eng, err = g.engineFor(ch)
	}
	if err != nil {
		return nil, &ChatError{Reason: fmt.Sprintf("%s: 引擎不可用: %v", semantic, err), Semantic: semantic, Code: errCodeEngineUnavailable}
	}
	attempts := len(deadlineWaits) + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptIntent := intentID
		if attempt > 0 {
			attemptIntent = fmt.Sprintf("%s-retry-%d", intentID, attempt)
		}
		resp, err := eng.ExecuteAction(ctx, ActionRequest{
			IntentID:       attemptIntent,
			TargetSemantic: semantic,
			ExpectedAction: action,
			Value:          value,
			ContextHint:    hints,
			TimeoutMS:      int(g.chatTimeout / time.Millisecond),
		})
		if err != nil {
			ee, ok := err.(*EngineError)
			if ok && canRetryAction(ctx, action, ee) && ee.IsDeadlineExceeded() && attempt < attempts-1 {
				g.sleep(deadlineWaits[attempt])
				continue
			}
			if ok && canRetryAction(ctx, action, ee) && ee.Code == codeAborted && attempt == 0 {
				// 页面瞬时竞态（等一帧）：立即重试一次，仅当再次失败才向上抛。
				g.sleep(300 * time.Millisecond)
				continue
			}
			reason := err.Error()
			if ok && ee.Reason != "" {
				reason = ee.Reason
			}
			return nil, &ChatError{Reason: fmt.Sprintf("%s: %s", semantic, reason), Semantic: semantic}
		}
		if !resp.Success {
			reason := resp.ErrorReason
			if reason == "" {
				reason = fmt.Sprintf("%s 执行失败", action)
			}
			return nil, &ChatError{Reason: fmt.Sprintf("%s: %s", semantic, reason), Semantic: semantic}
		}
		return &resp, nil
	}
	return nil, &ChatError{Reason: fmt.Sprintf("%s: deadline exceeded", semantic), Semantic: semantic, Code: errCodeUpstreamTimeout}
}

// providerChannelID 由 Provider 反查通道 id（executeWithRetry 需要引擎地址；
// 会话已存 EngineAddr，优先直接用会话，避免反查）。
func providerChannelID(p *ChatProvider, g *Gateway) string {
	for id, pp := range g.providers {
		if pp == p {
			return id
		}
	}
	return ""
}

// engineForSession 按会话缓存的引擎地址取客户端（无需通道配置反查）。
func (g *Gateway) engineForSession(sess *Session) (EngineClient, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.engines[sess.EngineAddr]; ok {
		return e, nil
	}
	e, err := g.engineFactory(sess.EngineAddr)
	if err != nil {
		return nil, err
	}
	g.engines[sess.EngineAddr] = e
	return e, nil
}

// readAnswerLenient 发送前读取页面「当前最后一条回答」作为基线；异常时返回 ""。
// 基线用于多轮：发送后必须等到「不同于基线」的新回答（2026-08-25 实测真 bug）。
func (g *Gateway) readAnswerLenient(ctx context.Context, stamp int64, p *ChatProvider, sid string) (string, error) {
	resp, err := g.executeWithRetry(ctx, sid, p,
		fmt.Sprintf("intent-uws-api-%d-baseline", stamp),
		p.Semantics[answerSemanticKey], "extract", "",
		map[string]string{"prefer_sse": "1"})
	if err != nil {
		if ce, ok := err.(*ChatError); ok && ce.Semantic == p.Semantics[answerSemanticKey] {
			return "", nil
		}
		return "", err
	}
	return resp.ExtractedData, nil
}

// waitForNewAnswer 发送后轮询 extract，直到「不同于基线且连续稳定 answerStable」或 SSE done。
// 完成判定与 Python _wait_for_new_answer 一致：SSE 已接入（ChatStreamActive）时只信
// chat_stream_done；未接入用文本稳定性；预算耗尽做最后一次不带 prefer_sse 的 DOM 兜底。
func (g *Gateway) waitForNewAnswer(ctx context.Context, stamp int64, baseline string, p *ChatProvider, sid string) (string, error) {
	timeout := g.chatTimeout
	if sess := g.getSession(sid); sess != nil && sess.Model != "" {
		timeout = g.TimeoutForModel(sess.Model)
	}
	deadline := time.Now().Add(timeout)
	maxPolls := int(math.Ceil(float64(timeout)/float64(chatPollInterval))) + 1
	if maxPolls < 1 {
		maxPolls = 1
	}
	lastText := ""
	lastSince := time.Time{}
	for poll := 0; poll < maxPolls; poll++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		resp, err := g.executeWithRetry(ctx, sid, p,
			fmt.Sprintf("intent-uws-api-%d-poll-%d", stamp, poll),
			p.Semantics[answerSemanticKey], "extract", "",
			map[string]string{"prefer_sse": "1"})
		if err != nil {
			if time.Now().Before(deadline) {
				g.sleep(clamp(200*time.Millisecond, time.Second, deadline.Sub(time.Now())))
				continue
			}
			if ce, ok := err.(*ChatError); ok && ce.Semantic == p.Semantics[answerSemanticKey] {
				break
			}
			return "", err
		}
		text := resp.ExtractedData
		if text != "" && text != baseline {
			if resp.ChatStreamActive {
				if resp.ChatStreamDone {
					return text, nil
				}
			} else if g.answerStable <= 0 {
				return text, nil
			} else {
				if text == lastText && !lastSince.IsZero() && time.Since(lastSince) >= g.answerStable {
					if sess := g.getSession(sid); sess != nil && IsVideoModel(sess.Model) && IsVideoStillGenerating(text) {
						g.sleep(clamp(500*time.Millisecond, 2*time.Second, deadline.Sub(time.Now())))
						continue
					}
					return text, nil
				}
				if text != lastText {
					lastText, lastSince = text, time.Now()
				}
			}
		}
		if !time.Now().Before(deadline) {
			break
		}

		g.sleep(clamp(200*time.Millisecond, time.Second, deadline.Sub(time.Now())))
	}
	return "", &ChatError{
		Reason:   "chat_answer: 超时预算内未确认新回答已完成",
		Semantic: p.Semantics[answerSemanticKey],
		Code:     errCodeUpstreamTimeout,
	}
}

// finishEvent 解析本轮回答的收尾事件；tools 用于把「模型写成文本的工具调用」结构化
// （见 tool_text.go 的 ParseToolCallsFromText）。
func finishEvent(text string, tools []Tool) ChatEvent {
	_, body := ExtractReasoningAndAnswer(text)
	_, calls := ParseToolCallsFromText(body, tools)
	if len(calls) > 0 {
		return ChatEvent{FinishReason: "tool_calls", ToolCalls: calls}
	}
	return ChatEvent{FinishReason: "stop"}
}

// streamAnswer 以短轮询消费 extract（prefer_sse），把累计文本的增量按
// choices[].delta.content 或 delta.reasoning_content 逐块写入 out；SSE done / 文本稳定后发 finish_reason=stop 或 tool_calls。
func (g *Gateway) streamAnswer(ctx context.Context, stamp int64, baseline string, p *ChatProvider, sid string, tools []Tool, out chan<- ChatEvent) error {
	timeout := g.chatTimeout
	if sess := g.getSession(sid); sess != nil && sess.Model != "" {
		timeout = g.TimeoutForModel(sess.Model)
	}
	deadline := time.Now().Add(timeout)
	maxPolls := int(math.Ceil(float64(timeout)/float64(chatStreamPollInterval))) + 1
	if maxPolls < 1 {
		maxPolls = 1
	}
	lastEmitted := ""
	lastText := ""
	lastSince := time.Time{}
	for poll := 0; poll < maxPolls; poll++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := g.executeWithRetry(ctx, sid, p,
			fmt.Sprintf("intent-uws-api-%d-spoll-%d", stamp, poll),
			p.Semantics[answerSemanticKey], "extract", "",
			map[string]string{"prefer_sse": "1"})
		if err != nil {
			if time.Now().Before(deadline) {
				g.sleep(clamp(100*time.Millisecond, 500*time.Millisecond, deadline.Sub(time.Now())))
				continue
			}
			if ce, ok := err.(*ChatError); ok && ce.Semantic == p.Semantics[answerSemanticKey] {
				break
			}
			return err
		}
		text := resp.ExtractedData
		if text != "" && text != baseline && text != lastEmitted {
			reasoning, answer := ExtractReasoningAndAnswer(text)
			lastR, lastA := ExtractReasoningAndAnswer(lastEmitted)

			// 分流发射思考增量 (reasoning_content)
			if len(reasoning) > len(lastR) {
				rDelta := strings.TrimPrefix(reasoning, lastR)
				out <- ChatEvent{ReasoningDelta: rDelta, Role: lastEmitted == ""}
			}
			// 分流发射正文增量 (content)
			if len(answer) > len(lastA) {
				aDelta := strings.TrimPrefix(answer, lastA)
				out <- ChatEvent{Delta: aDelta, Role: lastEmitted == "" && lastR == ""}
			}
			lastEmitted = text
		}
		if text != "" && text != baseline {
			if isAccountBanned(errors.New(text)) {
				g.TripSiteBreaker(p.Site, 24*time.Hour, fmt.Sprintf("页面内容提示禁言: %s", text))
				return &ChatError{Code: errCodeAccountBanned, Reason: fmt.Sprintf("站点 %s 页面提示已被禁言: %s", p.Site, text), Semantic: "chat_answer"}
			}
			if isRateLimited(errors.New(text)) {
				g.TripSiteBreaker(p.Site, 10*time.Minute, fmt.Sprintf("页面内容提示操作频繁: %s", text))
				return &ChatError{Code: errCodeRateLimited, Reason: fmt.Sprintf("站点 %s 页面提示操作过于频繁: %s", p.Site, text), Semantic: "chat_answer"}
			}
			if resp.ChatStreamActive {
				if resp.ChatStreamDone {
					out <- g.finishEventWithIdentity(ctx, sid, text, "", tools)
					return nil
				}
			} else if g.answerStable <= 0 {
				out <- g.finishEventWithIdentity(ctx, sid, text, "", tools)
				return nil
			} else {
				if text == lastText && !lastSince.IsZero() && time.Since(lastSince) >= g.answerStable {
					if sess := g.getSession(sid); sess != nil && IsVideoModel(sess.Model) && IsVideoStillGenerating(text) {
						g.sleep(clamp(200*time.Millisecond, time.Second, deadline.Sub(time.Now())))
						continue
					}
					out <- g.finishEventWithIdentity(ctx, sid, text, "", tools)
					return nil
				}
				if text != lastText {
					lastText, lastSince = text, time.Now()
				}
			}
		}
		if !time.Now().Before(deadline) {
			break
		}

		g.sleep(clamp(150*time.Millisecond, 500*time.Millisecond, deadline.Sub(time.Now())))
	}
	return &ChatError{
		Reason:   "chat_answer: 超时预算内未确认新回答已完成",
		Semantic: p.Semantics[answerSemanticKey],
		Code:     errCodeUpstreamTimeout,
	}
}

func (g *Gateway) makeCompletion(stamp int64, model, sid, answer, content string, tools []Tool) *ChatCompletion {
	reasoning, bodyText := ExtractReasoningAndAnswer(answer)
	cleanAnswer, toolCalls := ParseToolCallsFromText(bodyText, tools)
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	out := &ChatCompletion{
		ID:        fmt.Sprintf("chatcmpl-uws-%d", stamp),
		Object:    "chat.completion",
		Model:     model,
		SessionID: sid,
		ContextID: g.contextID(sid),
		TargetID:  g.targetID(sid),
	}
	if out.Model == "" {
		out.Model = "uws-llm_chat"
	}
	out.Choices = append(out.Choices, Choice{
		Index: 0,
		Message: ChoiceMessage{
			Role:             "assistant",
			Content:          cleanAnswer,
			ReasoningContent: reasoning,
			ToolCalls:        toolCalls,
		},
		FinishReason: finishReason,
	})
	out.Usage.PromptTokens = len(content)
	out.Usage.CompletionTokens = len(answer)
	out.Usage.TotalTokens = len(content) + len(answer)
	return out
}

// fillSiteConversation 回填非流式响应的站点会话身份（契约 §9 S1）。
// directConvID ＝ 本轮 L0 直发拿到的站点会话 ID 原始读数（响应体自报 ② 或槽位回显 ③；
// DOM 链路传空串）。
func (g *Gateway) fillSiteConversation(ctx context.Context, sid, directConvID string, comp *ChatCompletion) {
	if comp == nil {
		return
	}
	comp.SiteConversationID, comp.PageURL = g.siteConversationIdentity(ctx, sid, directConvID)
}

// finishEventWithIdentity 生成流式完成帧并回填站点会话身份（契约 §9 S1）。
func (g *Gateway) finishEventWithIdentity(ctx context.Context, sid, text, directConvID string, tools []Tool) ChatEvent {
	ev := finishEvent(text, tools)
	ev.SiteConversationID, ev.PageURL = g.siteConversationIdentity(ctx, sid, directConvID)
	return ev
}

// siteConversationIdentity 求该会话的站点会话身份（契约 §9「取值」条，2026-10-01/10-02 修订）：
// 来源按序第一个非空——① 页面真实地址归一化；② 本次 L0 直发响应体自报的会话 ID；
// ③ 本次 L0 直发实际使用的会话槽位回显（② 读不到时；两者由 chat_direct 先合并为一个读数）。
// directConvID 也必须过站点模板校验（NormalizeSiteConversationID，fail-closed）。
// 读不到即返回 nil：不猜 ID、不拿通道首页凑数；页地址读不到单独为 nil。
func (g *Gateway) siteConversationIdentity(ctx context.Context, sid, directConvID string) (*string, *string) {
	sess := g.getSession(sid)
	if sess == nil {
		return nil, nil
	}
	ch := g.cfg.Channel(sess.ChannelID)
	if ch == nil {
		return nil, nil
	}
	// 来源② 先备好：直发帧的 ID 不依赖页面可读性。
	var fallback *string
	if cid, ok := NormalizeSiteConversationID(ch.Site, directConvID); ok {
		fallback = &cid
	}
	// 站点自报的 ID 即「本会话自己绑定的对话」：清账目标只认这个记录，不认页签地址。
	bindFallback := func() {
		if fallback != nil {
			g.bindSessionConversation(sid, *fallback)
		}
	}
	eval := g.EvaluatorForSession(sid)
	if eval == nil {
		bindFallback()
		return fallback, nil
	}
	raw, err := eval.EvaluateScript(ctx, "location.href")
	if err != nil {
		bindFallback()
		return fallback, nil
	}
	var pageURL string
	if err := json.Unmarshal(raw, &pageURL); err != nil || strings.TrimSpace(pageURL) == "" {
		bindFallback()
		return fallback, nil
	}
	if cid, ok := SiteConversationFromURL(ch.Site, pageURL); ok { // 来源① 优先
		g.bindSessionConversation(sid, cid)
		return &cid, &pageURL
	}
	bindFallback()
	return fallback, &pageURL
}

// alignSiteConversationPage 把页签地址对齐到刚学到的站点会话页（契约 §9「取值」修订配套，
// 2026-10-01）。背景：L0 页内直发**不驱动站点 SPA 路由**，页地址恒为通道首页，于是 recipe 的
// `from_url` 会话槽位下一轮读空 ⇒ 站点每轮新开一个对话（真机实测：三轮三个 ID、侧栏越堆越多）。
// 学到 ID（来源②③）后把地址对齐过去，使请求侧与回传侧共用同一个事实源。
//
// 只做地址对齐（history.replaceState，不重载、不触发渲染、不改站点状态）；失败不阻塞本轮回答。
func (g *Gateway) alignSiteConversationPage(ctx context.Context, eval contract.DirectScriptEvaluator, site, rawConvID string) {
	if eval == nil {
		return
	}
	convID, ok := NormalizeSiteConversationID(site, rawConvID)
	if !ok {
		return
	}
	pageURL, ok := SiteConversationPageURL(site, convID)
	if !ok {
		return
	}
	if raw, err := eval.EvaluateScript(ctx, "location.href"); err == nil {
		var cur string
		if json.Unmarshal(raw, &cur) == nil {
			if cid, ok := SiteConversationFromURL(site, cur); ok && cid == convID {
				return // 已在对话页，幂等返回
			}
		}
	}
	u, err := url.Parse(pageURL)
	if err != nil || u.Path == "" {
		return
	}
	target := u.Path
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	script := fmt.Sprintf("(() => { history.replaceState(null, '', %s); return location.href; })()", strconv.Quote(target))
	if _, err := eval.EvaluateScript(ctx, script); err != nil && g.logger != nil {
		g.logger.Printf("站点会话地址对齐失败（不影响本轮回答）: %v", err)
	}
}

// requiresNewSession 判定本轮是否必须重建页签（不得沿用内存里的那个会话）。
// 除显式 new_session／没有会话／模型要求新会话外，**通道变了也必须重建**：同一
// session_id 先后打到不同站点时（agent-hub 多角色共用 session：router=deepseek-chat
// → synthesizer=gemini-flash），旧页签是**别的站点**，在它上面跑本通道的语义只会
// 连环 no_match，白等一轮再 502——真机复现见 2026-10-03 派单 d613c7908：deepseek
// 之后用同一 sid 打 gemini-flash，单请求 51.3s，引擎日志把 `gemini_model_switch`
// 打在 `chat.deepseek.com` 页上（模型选择器按钮未找到）。
func (g *Gateway) requiresNewSession(req ChatRequest, p *ChatProvider, sess *Session) bool {
	if req.NewSession || sess == nil || sess.ContextID == "" {
		return true
	}
	if req.ChannelID != "" && sess.ChannelID != req.ChannelID {
		return true
	}
	return g.modelChangedRequiresNewSession(p, req.Model, sess)
}

// runChatRound 执行一轮网页对话（is_new 判定 / 模型开关 / input+send / 等待回答）。
// 调用方必须持有会话锁。
func (g *Gateway) runChatRound(ctx context.Context, req ChatRequest, p *ChatProvider) (*ChatCompletion, error) {
	if err := g.authLatchError(req.ChannelID); err != nil {
		return nil, err
	}
	// 同站点低频门（契约 §9）：同站串行 + 最小间隔；同时刷新空闲回收判据。
	if release, err := g.siteTurn(ctx, g.channelSite(req.ChannelID)); err != nil {
		return nil, err
	} else {
		defer release()
	}
	g.touchSession(req.SessionID)
	if g.pacer != nil {
		if err := g.pacer.BeforeTurn(ctx, req.SessionID); err != nil {
			return nil, err
		}
		defer g.pacer.AfterTurn(req.SessionID)
	}
	content, err := userContent(req.Messages)
	if err != nil {
		return nil, &BadRequestError{Reason: err.Error()}
	}
	content = AdaptVideoPrompt(req.Model, content)
	ch := g.cfg.Channel(req.ChannelID)
	stamp := time.Now().UnixNano()
	sess := g.getSession(req.SessionID)
	isNew := g.requiresNewSession(req, p, sess)
	if isNew && len(req.Tools) > 0 {
		content = BuildStealthInstruction(req.Instructions, req.Tools) + "\n\n---\n\n" + content
	}
	var baseline string
	if isNew {
		g.closeSession(ctx, req.SessionID)
		if _, err := g.ensureSession(ctx, ch, req.SessionID, req.SiteConversationID); err != nil {
			return nil, err
		}
		if p.NewChatSettleSec > 0 {
			g.sleep(durationSec(p.NewChatSettleSec))
		}
		baseline = ""
	} else {
		if _, err := g.ensureSession(ctx, ch, req.SessionID, req.SiteConversationID); err != nil {
			return nil, err
		}
		if p.InputSettleSec > 0 {
			g.sleep(durationSec(p.InputSettleSec))
		}
		if baseline, err = g.readAnswerLenient(ctx, stamp, p, req.SessionID); err != nil {
			return nil, err
		}
	}
	if err := g.ensureAuthenticated(ctx, ch, req.SessionID, req.SiteConversationID); err != nil {
		return nil, err
	}
	// DeepSeek 等站点模式只在「新会话」落地页可选：只在 is_new 时切换。
	if isNew || !p.ModelOnNewChatOnly {
		if err := g.ensureModel(ctx, p, req.Model, req.SessionID); err != nil {
			return nil, err
		}
	}
	if err := g.ensureThinking(ctx, p, req.Thinking, req.SessionID); err != nil {
		return nil, err
	}
	if err := g.ensureSearch(ctx, p, req.Search, req.SessionID); err != nil {
		return nil, err
	}

	if answer, directConvID, handled, err := g.tryDirectChat(ctx, p, req, content, nil); handled {
		if err != nil {
			return nil, err
		}
		if IsVideoModel(req.Model) {
			videoURL, _ := ExtractVideoURL(answer)
			if videoURL == "" {
				if domURL, _ := ExtractVideoFromDOM(ctx, g.EvaluatorForSession(req.SessionID)); domURL != "" {
					videoURL = domURL
				}
			}
			answer = FormatVideoAnswer(answer, videoURL)
		}
		comp := g.makeCompletion(stamp, req.Model, req.SessionID, answer, content, req.Tools)
		comp.Transport = transportInPageDirect
		g.alignSiteConversationPage(ctx, g.EvaluatorForSession(req.SessionID), p.Site, directConvID)
		g.fillSiteConversation(ctx, req.SessionID, directConvID, comp)
		return comp, nil
	}
	transport := transportDOM

	steps := []chatStep{
		{"input", p.Semantics["input"], content},
		{"click", p.Semantics["send"], ""},
	}
	for i, s := range steps {
		if _, err := g.executeWithRetry(ctx, req.SessionID, p,
			fmt.Sprintf("intent-uws-api-%d-%d", stamp, i),
			s.semantic, s.action, s.value, nil); err != nil {
			if s.semantic == p.Semantics["send"] {
				err = submissionError(err)
			}
			return nil, err
		}
		if s.action == "input" {
			if g.pacer != nil {
				_ = g.pacer.AfterInputSettle(ctx, p.InputSettleSec)
			} else if p.InputSettleSec > 0 {
				g.sleep(durationSec(p.InputSettleSec))
			}
		}
	}
	answer, err := g.waitForNewAnswer(ctx, stamp, baseline, p, req.SessionID)
	if err != nil {
		if classifyChatError(err).Code == errCodeAccountBanned {
			g.TripSiteBreaker(p.Site, 24*time.Hour, err.Error())
		} else if classifyChatError(err).Code == errCodeRateLimited {
			g.TripSiteBreaker(p.Site, 10*time.Minute, err.Error())
		}
		return nil, submissionError(err)
	}
	// 检查抓取回来的回答文本是否为风控禁言/限频特征（站点未报错但直接渲染提示）
	if isAccountBanned(errors.New(answer)) {
		g.TripSiteBreaker(p.Site, 24*time.Hour, fmt.Sprintf("页面内容提示禁言: %s", answer))
		return nil, &ChatError{Code: errCodeAccountBanned, Reason: fmt.Sprintf("站点 %s 页面提示已被禁言: %s", p.Site, answer), Semantic: "chat_answer"}
	}
	if isRateLimited(errors.New(answer)) {
		g.TripSiteBreaker(p.Site, 10*time.Minute, fmt.Sprintf("页面内容提示操作频繁: %s", answer))
		return nil, &ChatError{Code: errCodeRateLimited, Reason: fmt.Sprintf("站点 %s 页面提示操作过于频繁: %s", p.Site, answer), Semantic: "chat_answer"}
	}
	if IsVideoModel(req.Model) {
		videoURL, _ := ExtractVideoURL(answer)
		var localPath string
		if domURL, local, _ := ExtractAndDownloadVideoFromDOM(ctx, g.EvaluatorForSession(req.SessionID), ""); domURL != "" {
			if videoURL == "" {
				videoURL = domURL
			}
			localPath = local
		}
		answer = FormatVideoAnswerWithLocal(answer, videoURL, localPath)
	}
	comp := g.makeCompletion(stamp, req.Model, req.SessionID, answer, content, req.Tools)
	comp.Transport = transport
	g.fillSiteConversation(ctx, req.SessionID, "", comp)
	return comp, nil
}

// runStreamRound 流式版 runChatRound（增量写入 out）。
func (g *Gateway) runStreamRound(ctx context.Context, req ChatRequest, p *ChatProvider, out chan<- ChatEvent) error {
	if err := g.authLatchError(req.ChannelID); err != nil {
		return err
	}
	// 同站点低频门（契约 §9）：同站串行 + 最小间隔；同时刷新空闲回收判据。
	if release, err := g.siteTurn(ctx, g.channelSite(req.ChannelID)); err != nil {
		return err
	} else {
		defer release()
	}
	g.touchSession(req.SessionID)
	if g.pacer != nil {
		if err := g.pacer.BeforeTurn(ctx, req.SessionID); err != nil {
			return err
		}
		defer g.pacer.AfterTurn(req.SessionID)
	}
	content, err := userContent(req.Messages)
	if err != nil {
		return &BadRequestError{Reason: err.Error()}
	}
	content = AdaptVideoPrompt(req.Model, content)
	ch := g.cfg.Channel(req.ChannelID)
	stamp := time.Now().UnixNano()
	sess := g.getSession(req.SessionID)
	isNew := req.NewSession || sess == nil || sess.ContextID == "" ||
		g.modelChangedRequiresNewSession(p, req.Model, sess)
	if isNew && len(req.Tools) > 0 {
		content = BuildStealthInstruction(req.Instructions, req.Tools) + "\n\n---\n\n" + content
	}
	var baseline string
	if isNew {
		g.closeSession(ctx, req.SessionID)
		if _, err := g.ensureSession(ctx, ch, req.SessionID, req.SiteConversationID); err != nil {
			return err
		}
		if p.NewChatSettleSec > 0 {
			g.sleep(durationSec(p.NewChatSettleSec))
		}
		baseline = ""
	} else {
		if _, err := g.ensureSession(ctx, ch, req.SessionID, req.SiteConversationID); err != nil {
			return err
		}
		if p.InputSettleSec > 0 {
			g.sleep(durationSec(p.InputSettleSec))
		}
		if baseline, err = g.readAnswerLenient(ctx, stamp, p, req.SessionID); err != nil {
			return err
		}
	}
	if err := g.ensureAuthenticated(ctx, ch, req.SessionID, req.SiteConversationID); err != nil {
		return err
	}
	if isNew || !p.ModelOnNewChatOnly {
		if err := g.ensureModel(ctx, p, req.Model, req.SessionID); err != nil {
			return err
		}
	}
	if err := g.ensureThinking(ctx, p, req.Thinking, req.SessionID); err != nil {
		return err
	}
	if err := g.ensureSearch(ctx, p, req.Search, req.SessionID); err != nil {
		return err
	}

	if answer, directConvID, handled, err := g.tryDirectChat(ctx, p, req, content, out); handled {
		if err != nil {
			return err
		}
		fin := finishEvent(answer, req.Tools)
		fin.Transport = transportInPageDirect
		g.alignSiteConversationPage(ctx, g.EvaluatorForSession(req.SessionID), p.Site, directConvID)
		fin.SiteConversationID, fin.PageURL = g.siteConversationIdentity(ctx, req.SessionID, directConvID)
		select {
		case out <- fin:
			return nil
		case <-ctx.Done():
			return submissionError(ctx.Err())
		}
	}
	out <- ChatEvent{Transport: transportDOM}

	steps := []chatStep{
		{"input", p.Semantics["input"], content},
		{"click", p.Semantics["send"], ""},
	}
	for i, s := range steps {
		if _, err := g.executeWithRetry(ctx, req.SessionID, p,
			fmt.Sprintf("intent-uws-api-%d-%d", stamp, i),
			s.semantic, s.action, s.value, nil); err != nil {
			if s.semantic == p.Semantics["send"] {
				err = submissionError(err)
			}
			return err
		}
		if s.action == "input" {
			if g.pacer != nil {
				_ = g.pacer.AfterInputSettle(ctx, p.InputSettleSec)
			} else if p.InputSettleSec > 0 {
				g.sleep(durationSec(p.InputSettleSec))
			}
		}
	}
	err = g.streamAnswer(ctx, stamp, baseline, p, req.SessionID, req.Tools, out)
	if err != nil {
		if classifyChatError(err).Code == errCodeAccountBanned {
			g.TripSiteBreaker(p.Site, 24*time.Hour, err.Error())
		} else if classifyChatError(err).Code == errCodeRateLimited {
			g.TripSiteBreaker(p.Site, 10*time.Minute, err.Error())
		}
		return submissionError(err)
	}
	return nil
}

// contextID 返回会话绑定的引擎 context_id。
func (g *Gateway) contextID(sid string) string {
	if s := g.getSession(sid); s != nil {
		return s.ContextID
	}
	return ""
}

// targetID 返回会话绑定的真实 CDP 标签页 target_id。
func (g *Gateway) targetID(sid string) string {
	if s := g.getSession(sid); s != nil {
		return s.TargetID
	}
	return ""
}

// Chat 执行一轮网页对话，返回 OpenAI 兼容 chat.completion。
// 仅提交前标签被关闭可重建一次；提交/观察结果未知时禁止整轮重放。
func (g *Gateway) Chat(ctx context.Context, req ChatRequest) (*ChatCompletion, error) {
	if req.SessionID == "" {
		req.SessionID = DefaultSessionID
	}
	p := g.providers[req.ChannelID]
	if p == nil {
		return nil, &BadRequestError{Reason: fmt.Sprintf("通道 %q 不是 web 通道", req.ChannelID)}
	}
	if err := validateSiteConversationRequest(g.cfg.Channel(req.ChannelID), req); err != nil {
		return nil, err
	}
	unlock := g.sessionLock(req.SessionID)
	out, err := g.runChatWithRetry(ctx, req, p)
	unlock()
	if req.CloseAfter {
		// 一次性会话（契约 §9）：回答已交付，立即收口；失败轮不收口，留给重试与空闲兜底。
		g.finishOneShot(ctx, req.SessionID, err)
	}
	return out, err
}

// ChatStream 流式版 Chat：返回事件通道（handler 序列化为 SSE）。
func (g *Gateway) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatEvent, error) {
	if req.SessionID == "" {
		req.SessionID = DefaultSessionID
	}
	p := g.providers[req.ChannelID]
	if p == nil {
		return nil, &BadRequestError{Reason: fmt.Sprintf("通道 %q 不是 web 通道", req.ChannelID)}
	}
	if err := validateSiteConversationRequest(g.cfg.Channel(req.ChannelID), req); err != nil {
		return nil, err
	}
	out := make(chan ChatEvent, 16)
	go func() {
		defer close(out)
		unlock := g.sessionLock(req.SessionID)
		err := g.runStreamRound(ctx, req, p, out)
		if err != nil && isRetryableRoundFailure(err) {
			g.closeSession(ctx, req.SessionID)
			if _, e := g.ensureSession(ctx, g.cfg.Channel(req.ChannelID), req.SessionID, req.SiteConversationID); e == nil {
				if p.NewChatSettleSec > 0 {
					g.sleep(durationSec(p.NewChatSettleSec))
				}
				err = g.runStreamRound(ctx, req, p, out)
			}
		}
		unlock()
		if err != nil {
			out <- ChatEvent{Err: err}
		} else if req.CloseAfter {
			g.finishOneShot(ctx, req.SessionID, nil)
		}
	}()
	return out, nil
}

// clamp 把 d 限制在 [min, max]。
func clamp(min, max, d time.Duration) time.Duration {
	if d < min {
		return min
	}
	if d > max {
		return max
	}
	return d
}

// durationSec 把秒浮点转为 time.Duration（站点沉降时间）。
func durationSec(sec float64) time.Duration {
	return time.Duration(sec * float64(time.Second))
}
