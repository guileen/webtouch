package engine

import (
	"context"
	"log"
	"strings"
	"time"
	"uws/pkg/browser"
	"uws/pkg/challenge"
	"uws/pkg/verifier"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sessionPageURL 读取会话当前页面 URL（browser.PageURLSource；缺失时返回空串）。
func sessionPageURL(sess browser.CDPSession) string {
	if ps, ok := sess.(browser.PageURLSource); ok {
		return ps.PageURL()
	}
	return ""
}

// hitElementOf 把会话最近命中元素证据映射为契约消息（缺失时返回 false）。
func (s *Server) hitElementOf(sess browser.CDPSession) (*uwspb.HitElement, bool) {
	hs, ok := sess.(browser.HitElementSource)
	if !ok {
		return nil, false
	}
	h, ok := hs.LastHitElement()
	if !ok {
		return nil, false
	}
	return &uwspb.HitElement{
		Selector: h.Selector,
		Tag:      h.Tag,
		X:        float32(h.X), Y: float32(h.Y),
		Width: float32(h.W), Height: float32(h.H),
		Text: h.Text,
	}, true
}

// WasmTruncationTotal 返回 WASM 探针截断累计次数
// （uws_wasm_truncation_total 数据源；Prometheus 导出待指标依赖定案后接线）。
func (s *Server) WasmTruncationTotal() uint64 {
	return s.wasmTruncTotal.Load()
}

// ── CaptureScreenshot（v1.5.1 追加-only RPC，TDD-COORD-04 引擎原语） ──

// CaptureScreenshot 捕获当前上下文页面截图（PNG 字节），供 VLM 视觉打码求解。
// 生命周期与 ExecuteAction 一致：必须先 InitializeContext；target_id 校验防串上下文。
func (s *Server) CaptureScreenshot(ctx context.Context, req *uwspb.CaptureScreenshotRequest) (*uwspb.CaptureScreenshotResponse, error) {
	key := s.auth.ClientKey(ctx)
	s.mu.Lock()
	bc := s.findContextLocked(key, req.GetTargetId())
	hasAny := s.hasAnyContextLocked(key)
	s.mu.Unlock()
	if bc == nil {
		if req.GetTargetId() != "" && hasAny {
			return nil, status.Error(codes.NotFound, "target_id not found for this client")
		}
		return nil, status.Error(codes.FailedPrecondition,
			"InitializeContext must be called before CaptureScreenshot")
	}
	sess, ok := bc.Session().(browser.Screenshotter)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "session does not support screenshots")
	}
	png, w, h, err := sess.CaptureScreenshot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "capture screenshot: %v", err)
	}
	return &uwspb.CaptureScreenshotResponse{Png: png, Width: int32(w), Height: int32(h)}, nil
}

// ── ExportPageTree（v1.5.5 追加-only RPC，混合指挥） ──

// ExportPageTree 导出当前上下文页面的极简 AXTree（ExportToVLMFormat 文本），
// 供协调器 commander / uws run --plan 外发 VLM 指挥官做 node_id 确定性引用。
// 生命周期与 CaptureScreenshot 一致：必须先 InitializeContext；target_id 校验防串上下文。
func (s *Server) ExportPageTree(ctx context.Context, req *uwspb.ExportPageTreeRequest) (*uwspb.ExportPageTreeResponse, error) {
	key := s.auth.ClientKey(ctx)
	s.mu.Lock()
	bc := s.findContextLocked(key, req.GetTargetId())
	hasAny := s.hasAnyContextLocked(key)
	s.mu.Unlock()
	if bc == nil {
		if req.GetTargetId() != "" && hasAny {
			return nil, status.Error(codes.NotFound, "target_id not found for this client")
		}
		return nil, status.Error(codes.FailedPrecondition,
			"InitializeContext must be called before ExportPageTree")
	}
	ex, ok := bc.Session().(browser.PageTreeExporter)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "session does not support page tree export")
	}
	tree, err := ex.ExportPageTree(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "export page tree: %v", err)
	}
	return &uwspb.ExportPageTreeResponse{Tree: tree}, nil
}

// findContextLocked 按 target_id 在全部上下文中查找（调用方持 s.mu）。
// target_id 为空 = 默认上下文（旧语义）。
func (s *Server) findContextLocked(clientKey, targetID string) *browser.BrowserContext {
	if targetID == "" {
		return s.contexts[contextKey(clientKey, defaultContextName)]
	}
	// v1.5.6：具名会话（Session ID）跨连接存续；Target ID 是真实 CDP targetId，
	// 全局唯一，任何同 Key 的新连接都应能凭它定位回同一上下文（会话续页）。
	for _, bc := range s.contexts {
		if bc.ID == targetID {
			return bc
		}
		// 兼容 v1.5.3+ 客户端：InitializeContext 返回真实 CDP targetId，
		// CaptureScreenshot 等按该值回传也必须能路由回同一上下文。
		if dbg, ok := bc.Session().(browser.TargetDebugInfo); ok && dbg.TargetID() == targetID {
			return bc
		}
		// 新窗口接管（2026-09-22 P0-1）后：调用方可能仍拿接管前的钉扎值
		// （PinnedTarget），也可能已改用接管后的新 Target（ActiveTargetID）。
		if bc.PinnedTarget() == targetID || activeContextTargetID(bc) == targetID {
			return bc
		}
	}
	return nil
}

// contextTargetID 返回上下文的对外 Target ID（真实 CDP 会话=页面 targetId，
// 模拟/兜底=Pool 内部 Context ID），供错误信息与钉扎校验展示。
func contextTargetID(bc *browser.BrowserContext) string {
	if bc == nil {
		return ""
	}
	if dbg, ok := bc.Session().(browser.TargetDebugInfo); ok {
		if tid := dbg.TargetID(); tid != "" {
			return tid
		}
	}
	return bc.ID
}

// contextMatchesTarget 判断请求钉扎的 Target ID 是否属于该上下文
// （与 findContextLocked 同一匹配口径：真实 targetId 或 Pool Context ID）。
func contextMatchesTarget(bc *browser.BrowserContext, targetID string) bool {
	if bc == nil {
		return targetID == ""
	}
	if targetID == "" {
		return true
	}
	if targetID == bc.ID {
		return true
	}
	// 新窗口接管（2026-09-22 P0-1）后：调用方钉的是接管前的 Target（PinnedTarget），
	// 会话作用面已前移到新 Target（ActiveTargetID）——两者都算「属于本会话」。
	if bc.PinnedTarget() == targetID || activeContextTargetID(bc) == targetID {
		return true
	}
	return contextTargetID(bc) == targetID
}

// followNewTargetEnabled 判断本次动作是否启用「新窗口接管」（默认开启）。
// 关闭方式：context_hint.follow_new_target = "0"/"false"/"off"/"no"。
func followNewTargetEnabled(req *uwspb.ActionIntentRequest) bool {
	switch strings.ToLower(strings.TrimSpace(req.GetContextHint()["follow_new_target"])) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// activeContextTargetID 返回上下文当前实际作用的 Target：新窗口接管
// （browser.TargetSwitcher）后作用面前移，响应里的 active_target_id 必须跟着走，
// 否则会骗调用方继续操作旧页。
func activeContextTargetID(bc *browser.BrowserContext) string {
	if bc == nil {
		return ""
	}
	if sw, ok := bc.Session().(browser.TargetSwitcher); ok {
		if tid := sw.ActiveTargetID(); tid != "" {
			return tid
		}
	}
	return contextTargetID(bc)
}

// hasAnyContextLocked 判断该客户端是否持有至少一个上下文（调用方持 s.mu）。
func (s *Server) hasAnyContextLocked(clientKey string) bool {
	prefix := clientKey + contextSep
	for k := range s.contexts {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// CloseContext（v1.5.3 追加-only RPC）：关闭命名上下文（释放 Target，
// 对应浏览器标签页随之关闭）。API 层 DELETE /v1/sessions/{id} 消费。
// 幂等：上下文不存在也返回 ok（标签已被用户手动关闭/引擎重启）。
func (s *Server) CloseContext(ctx context.Context, req *uwspb.CloseContextRequest) (*uwspb.CloseContextResponse, error) {
	ckey := s.ckeyFor(ctx, req.GetContextName())
	s.mu.Lock()
	bc := s.contexts[ckey]
	delete(s.contexts, ckey)
	// 互斥槽：拥有者关闭 ⇒ 该键归还空闲、借用者一并解绑（否则指向已释放的
	// Target）；关闭者若只是借用者 ⇒ 只解自己的绑定，不关掉共享窗口。
	borrower := false
	if bc != nil {
		for k, slot := range s.mutexSlots {
			if slot.bc != bc {
				continue
			}
			if slot.owner == ckey {
				delete(s.mutexSlots, k)
				for k2, v2 := range s.contexts {
					if v2 == bc {
						delete(s.contexts, k2)
					}
				}
				continue
			}
			borrower = true
		}
	}
	s.mu.Unlock()
	// 会话这条线结束了：绑定一起删掉，否则下次同名会话会去接管一扇已经关掉的窗。
	// 幂等：引擎重启后内存里没有上下文（bc == nil）时也照样删——运维因此不必为
	// 「死绑定」重启引擎，DELETE /v1/sessions/{id} 一步到位。
	if err := s.sessions.Delete(req.GetContextName()); err != nil {
		log.Printf("engine: 会话 %q 绑定删除失败: %v", req.GetContextName(), err)
	}
	if bc == nil {
		return &uwspb.CloseContextResponse{Ok: true}, nil
	}
	if borrower {
		return &uwspb.CloseContextResponse{Ok: true}, nil
	}
	s.dropPage(bc.ID)
	s.detachViewer(bc.ID)
	s.pool.Release(bc.ID)
	return &uwspb.CloseContextResponse{Ok: true}, nil
}

// noteNotFound 累计连续 NOT_FOUND（大面积失明信号）；达到阈值触发 P0 自动回滚
// （TDD-KB-06，spec/knowledge-versioning.md §5.2）。触发后无论成功与否均清零，
// 避免无 previous 可回滚时每 50 次重复探测。
func (s *Server) noteNotFound() {
	if s.notFoundStreak.Add(1) < AutoRollbackThreshold {
		return
	}
	s.notFoundStreak.Store(0)
	if resp := s.kb.AutoRollback(); resp.OK {
		// R1：回滚后 matcher 必须同步到回滚版本，否则「恢复大面积失明」不成立。
		if err := s.syncMatcherWithKnowledge(); err != nil {
			log.Printf("auto rollback matcher sync: %v", err)
		}
		// A 案：模型随 artifact 同生命周期回滚（scorer 不得停留在坏版本权重）。
		s.syncScorerWithKnowledge()
	}
}

// pageFor 返回指定 Target 的页面时序包装（惰性创建，STABLE 起步）。
func (s *Server) pageFor(id string) *pageVerifier {
	s.pageMu.Lock()
	defer s.pageMu.Unlock()
	p, ok := s.pages[id]
	if !ok {
		p = &pageVerifier{v: verifier.NewVerifier(nil)}
		s.pages[id] = p
	}
	return p
}

// dropPage 移除指定 Target 的页面时序状态。
func (s *Server) dropPage(id string) {
	s.pageMu.Lock()
	delete(s.pages, id)
	s.pageMu.Unlock()
}

// breakerFor 返回指定会话的防乱点熔断器（惰性创建）。
func (s *Server) breakerFor(id string) *challenge.CircuitBreaker {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	if s.breakers == nil {
		s.breakers = make(map[string]*challenge.CircuitBreaker)
	}
	cb, ok := s.breakers[id]
	if !ok {
		cb = challenge.NewCircuitBreaker()
		s.breakers[id] = cb
	}
	return cb
}

// CircuitBreakerFor 暴露指定会话的熔断器（供监控与 Web 控制面查询/复位）。
func (s *Server) CircuitBreakerFor(id string) *challenge.CircuitBreaker {
	return s.breakerFor(id)
}

func verifyTimeout(timeoutMs int32) time.Duration {
	if timeoutMs <= 0 {
		return defaultVerifyTimeout
	}
	return time.Duration(timeoutMs) * time.Millisecond
}

// waitForStable 等待 NAVIGATING 稳定为 NAVIGATED（Network Idle 500ms + DOM 静息）。
// 超时或 verifier 判定超时 → gRPC DEADLINE_EXCEEDED；稳定后重置验证器并标记
// navCompleted，供本次响应向协调器通告 NAVIGATED。
func (s *Server) waitForStable(bc *browser.BrowserContext, page *pageVerifier, timeout time.Duration) (uwspb.ContextState, error) {
	deadline := time.Now().Add(timeout)
	for page.v.Snapshot() == verifier.StateNavigating {
		if ev, ok := bc.Session().(browser.VerifierEvents); ok {
			_, active, mutated := ev.NavigationState()
			page.v.FeedNetwork(active)
			if mutated {
				page.v.FeedDOMMutation()
			}
		}
		switch page.v.Advance() {
		case verifier.StateNavigated:
			page.v = verifier.NewVerifier(nil)
			page.navCompleted = true
			return uwspb.ContextState_NAVIGATED, nil
		case verifier.StateDeadlineExceeded:
			page.v = verifier.NewVerifier(nil)
			return 0, status.Error(codes.DeadlineExceeded, "verification timeout: page did not stabilize")
		}
		if !time.Now().Before(deadline) {
			page.v = verifier.NewVerifier(nil)
			return 0, status.Error(codes.DeadlineExceeded, "verification timeout: page did not stabilize")
		}
		time.Sleep(verifyPollInterval)
	}
	return uwspb.ContextState_STABLE, nil
}
