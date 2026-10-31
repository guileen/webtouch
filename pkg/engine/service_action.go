package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"uws/pkg/browser"
	"uws/pkg/challenge"
	"uws/pkg/classifier"
	"uws/pkg/matcher"
	"uws/pkg/vault"
	"uws/pkg/verifier"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ── ExecuteAction（TDD-API-02 幂等 / TDD-API-01 生命周期） ──

func (s *Server) ExecuteAction(ctx context.Context, req *uwspb.ActionIntentRequest) (*uwspb.ActionResultResponse, error) {
	if err := validateIntent(req); err != nil {
		return nil, err
	}
	return s.execute(ctx, req)
}

// defaultVaultResolve 读本机凭据库解引用 `@vault:<名>`（明文唯一的出口在 pkg/vault，
// 这里只把值交给执行层，不打印、不落日志）。
func defaultVaultResolve(name string) (string, error) {
	keyPath, filePath := vault.ResolvePaths(vault.DefaultDir())
	store, err := vault.Open(keyPath, filePath)
	if err != nil {
		return "", err
	}
	entry, ok := store.Get(name)
	if !ok {
		return "", fmt.Errorf("%w: %s（用 `uws vault list` 看有哪些）", vault.ErrNotFound, name)
	}
	return entry.Value, nil
}

// vaultUnresolvedPrefix 是「凭据直填」失败的稳定 ASCII 前缀（proto/uws_service.proto
// 错误码表 FAILED_PRECONDITION 行的子类）：协调器按它把这条失败与「Target 崩溃」区分开
// —— 重建上下文不会让缺失的凭据出现，故应 PAUSE 透传原话而不是 REINIT。
const vaultUnresolvedPrefix = "vault_unresolved: "

// resolveVaultRef 处理「凭据直填」：值写成 `@vault:<名>` 时，在本机解引用成真值再交给
// 执行层。写在执行最前面（候选择优之前），因为这条路径必须 **fail-closed** ——
// 取不到凭据就报错停下，绝不把 `@vault:xxx` 这串引用原样填进页面（否则用户看到的
// 是「密码填成了 @vault:xxx」这种更难察觉的失败）。
//
// evaluate 是 JS 源码通道，语义不是「填表」，不参与解引用。
func (s *Server) resolveVaultRef(req *uwspb.ActionIntentRequest) error {
	name, ok := vault.ReferenceName(req.GetValue())
	if !ok {
		return nil
	}
	if req.GetExpectedAction() == "evaluate" {
		return nil
	}
	resolve := s.vaultResolve
	if resolve == nil {
		resolve = defaultVaultResolve
	}
	value, err := resolve(name)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%s凭据直填失败 %s%s: %v", vaultUnresolvedPrefix, vault.ReferencePrefix, name, err)
	}
	if value == "" {
		return status.Errorf(codes.FailedPrecondition, "%s凭据直填失败 %s%s: 条目为空", vaultUnresolvedPrefix, vault.ReferencePrefix, name)
	}
	req.Value = proto.String(value)
	return nil
}

// sweepLedgerLocked 回收幂等账本里的过期项（调用方须持有 s.mu）：
//   - cache：超过 IdempotencyTTL 的响应副本；
//   - indeterminate：已经过了观察窗口 indeterminateGrace 的「派发后结果未知」，
//     连同它占着的 inflight 占位一起回收（后者只可能由前者泄漏出来：请求已经
//     带着错误返回，占位再也没有持有者）。
//
// 仍在执行的请求不受影响：它们自己的 inflight 占位由各自的返回路径 delete。
// 按 sweepInterval 节流，所以最坏情况是「一个清扫间隔内新产生的条目数」——
// 有界，且不需要额外 goroutine 或生命周期管理（2026-09-23 审查结论 item 3）。
func (s *Server) sweepLedgerLocked(now time.Time) {
	if now.Sub(s.lastSweep) < sweepInterval {
		return
	}
	s.lastSweep = now
	for k, v := range s.cache {
		if !v.expiresAt.After(now) {
			delete(s.cache, k)
		}
	}
	for k, ts := range s.indeterminate {
		if now.Sub(ts) >= indeterminateGrace {
			delete(s.indeterminate, k)
			delete(s.inflight, k)
		}
	}
}

func (s *Server) execute(ctx context.Context, req *uwspb.ActionIntentRequest) (*uwspb.ActionResultResponse, error) {
	// 凭据直填的值在本函数内就被解引用成明文，Episode 需在解引用前记下「值来自哪条
	// 凭据」（§5.0.12 条款 2：明文与引用原文都不落盘，只留引用名）。
	vaultRef, _ := vault.ReferenceName(req.GetValue())
	if err := s.resolveVaultRef(req); err != nil {
		return nil, err
	}
	// v1.5.3：context_hint["context"] 选择命名上下文（= 浏览器标签页）。
	// 缺省 = 默认上下文（旧「每客户端单上下文」语义零变化）。
	name := req.GetContextHint()["context"]
	ckey := s.ckeyFor(ctx, name)
	cacheID := ckey + "\x00" + req.GetIntentId()

	s.mu.Lock()
	s.sweepLedgerLocked(time.Now())
	if hit, ok := s.cache[cacheID]; ok && hit.expiresAt.After(time.Now()) {
		s.mu.Unlock()
		return proto.Clone(hit.resp).(*uwspb.ActionResultResponse), nil
	}
	// 上一次同 intent 的物理动作已派发但结果未知：在观察窗口内既不能再打一次，
	// 也不能假装没发生过——如实告诉调用方「未知」，由它核对页面或换新 intent。
	if ts, ok := s.indeterminate[cacheID]; ok {
		if time.Since(ts) < indeterminateGrace {
			s.mu.Unlock()
			return nil, status.Errorf(codes.Aborted,
				"physical outcome unknown: intent %q 的物理事件已派发但结果未确认（%s 内不重放）；请核对页面，或改用新的 intent_id 显式重试",
				req.GetIntentId(), indeterminateGrace)
		}
		delete(s.indeterminate, cacheID)
		delete(s.inflight, cacheID)
	}
	if _, inFlight := s.inflight[cacheID]; inFlight {
		s.mu.Unlock()
		// 同 intent_id 正在执行：不重复物理执行，协调器按 DEADLINE_EXCEEDED 退避重试。
		return nil, status.Error(codes.DeadlineExceeded, "intent_id already in flight")
	}
	s.inflight[cacheID] = struct{}{}
	bc := s.contexts[ckey]
	s.mu.Unlock()

	if bc == nil {
		s.mu.Lock()
		delete(s.inflight, cacheID)
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition,
			"InitializeContext must be called before ExecuteAction")
	}
	// v1.5.6 共享浏览器各会话各管各的 Tab：协调器把本会话钉扎的 Target ID
	// 放进 context_hint["target_id"]，引擎校验其确实属于该会话；不一致即
	// NotFound——绝不悄悄把动作打到另一个进程/会话的标签页上。
	if tid := req.GetContextHint()["target_id"]; tid != "" && !contextMatchesTarget(bc, tid) {
		s.mu.Lock()
		delete(s.inflight, cacheID)
		s.mu.Unlock()
		return nil, status.Errorf(codes.NotFound,
			"会话 %q 当前 Target(%s) 与请求钉扎的 Target(%s) 不一致；请用该会话报告里的 Session/Target ID 续页",
			name, contextTargetID(bc), tid)
	}

	resp, err := s.runAction(ctx, bc, req, vaultRef)
	if err != nil {
		var ie *actionInterruptedErr
		s.mu.Lock()
		if errors.As(err, &ie) && ie.dispatched {
			// 物理事件已经出去、结果未知：保留去重状态（不动 inflight），
			// 在 indeterminateGrace 内让同 intent 的重试拿到明确的「未知」而不是再打一次。
			s.indeterminate[cacheID] = time.Now()
		} else {
			delete(s.inflight, cacheID)
		}
		s.mu.Unlock()
		return nil, err
	}

	s.mu.Lock()
	delete(s.inflight, cacheID)
	s.cache[cacheID] = cachedResult{expiresAt: time.Now().Add(IdempotencyTTL), resp: resp}
	s.mu.Unlock()
	return resp, nil
}

// runAction 执行「语义匹配 → 决策核 → 物理执行 → 局部验证」管线。
// readOnlyAction 判断动作是否「只读」——不改页面、不点击、不提交。
//
// 只读动作不参与防乱点熔断（Anti-thrashing Guard）的两侧判定：
//   - 不计入失败计数：extract 的 no_match 是确定性结论（页面上没有这段文本），
//     不是乱点征兆；
//   - 不被熔断拦截：熔断期间仍可读页面，否则一次误触发的熔断会把整条 session
//     连同只读取证一起冻死（2026-09-23 实测：IANA 公开页 3 次 no_match 冻结全 session）。
//
// 2026-09-23 与用户定案同向：熔断只锁「撞过的那个真实账号页 + 有副作用的动作」，
// 不锁只读观察、不锁引擎、不锁公开测试页。
func readOnlyAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "extract":
		return true
	default:
		return false
	}
}

func (s *Server) runAction(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest, vaultRef string) (out *uwspb.ActionResultResponse, runErr error) {
	start := time.Now()
	// v1.5.4 证据链：动作前 URL 快照（点击导航判定用；会话不支持时为空串）。
	urlBefore := sessionPageURL(bc.Session())
	confidence := 0.0
	candCount := 0
	// execSemantic 是物理执行层实际作用的节点语义：默认取请求语义；
	// A* 多步搜索命中更深路径时取路径末步节点文本。
	execSemantic := req.GetTargetSemantic()
	// aStarDepth 是 A* 选中路径的长度（§5.0.9 条款 7：≥2 才算「多步换步」，落点自证
	// 的比对基准随之换成 execSemantic；长度 1 的路径根即答案，仍按目标语义判）。
	aStarDepth := 0
	// cands/execOK/observed 提前声明：Episode 记录器（§5.0.12，T-22）需要在
	// **任何**返回路径（含拒识与失败）收尾落盘，而它们仍是同一条数据流。
	var (
		cands         []matcher.Candidate
		execOK        bool
		observed      string
		vlmAttempted  bool
		vlmObserved   string
		vlmTryOutcome vlmOutcome
		// §5.0.15：是否走过候选池匹配（drag / node_id 直连 / evaluate / 无匹配器时为 false）。
		poolEvaluated bool
	)
	epDecision := ""
	ep := s.beginEpisode(req, bc, urlBefore, vaultRef)
	if ep != nil {
		defer func() {
			ep.finish(episodeOutcome{
				Resp:          out,
				Err:           runErr,
				Decision:      epDecision,
				ExecSemantic:  execSemantic,
				Candidates:    cands,
				Confidence:    confidence,
				CandCount:     candCount,
				Observed:      observed,
				PoolEvaluated: poolEvaluated,
			})
		}()
	}

	// SPA 时序：若页面仍处于 NAVIGATING，先等待稳定（≤ timeout_ms）。
	// 未稳返回 DEADLINE_EXCEEDED（spec §7.2：协调器 Fibonacci 退避重试 1 次）。
	page := s.pageFor(bc.ID)
	// 同 Target 动作串行化：页面状态机与 navCompleted 不允许并发读写（R5）。
	page.mu.Lock()
	defer page.mu.Unlock()

	cb := s.breakerFor(bc.ID)
	// 0-Token 探测当前页面是否已陷入人机挑战态
	if prober, ok := bc.Session().(browser.ChallengeProber); ok {
		if sig, detected := prober.ProbeChallenge(ctx); detected {
			cb.Trip(sig)
		}
	}

	// drag 是 VLM 挑战解锁原语（v1.5.1）：坐标直发物理层，bypass 语义匹配/决策带/候选池
	dragDirect := req.GetExpectedAction() == "drag"

	// 防频繁乱点物理熔断护栏门禁 (Anti-thrashing Guard)。
	// 只读动作（extract）不受熔断拦截：它不点击、不提交、不改页面，
	// 不是「乱点」风险，没有理由被冻结（见 readOnlyAction 注释）。
	if !dragDirect && !readOnlyAction(req.GetExpectedAction()) {
		if allowed, err := cb.AllowAction(); !allowed {
			sig := cb.LastSignal()
			if !sig.IsHumanChallenge() {
				// 熔断由「连续失败/人工拉起」触发，页面并没有人机验证：上报
				// UNAUTHENTICATED 会让协调器误判为 WAF/CAPTCHA 并走「外部破解 →
				// 人工接管」流程（2026-09-23 实测：空白页连点 3 次不存在元素即
				// 报 challenge_intercepted）。降级阻塞语义（RESOURCE_EXHAUSTED）
				// 与防乱点熔断的真实含义一致：拒绝动作，不假装有挑战。
				s.publishViewerAction(bc.ID, browser.ViewerAction{
					Kind:    "guard_challenge",
					Summary: "防乱点熔断已激活（连续失败，非人机验证），拒绝动作: " + err.Error(),
				})
				return nil, status.Errorf(codes.ResourceExhausted,
					"anti-thrashing guard OPEN (not a human challenge): %v", err)
			}
			s.publishViewerAction(bc.ID, browser.ViewerAction{
				Kind:    "guard_challenge",
				Summary: "防乱点熔断已激活，等待人工介入: " + err.Error(),
			})
			return nil, status.Errorf(codes.Unauthenticated, "challenge intercepted (circuit breaker OPEN): %v", err)
		}
	}

	s.publishViewerAction(bc.ID, browser.ViewerAction{
		Kind:    "thinking",
		Summary: fmt.Sprintf("执行 %s: %s", req.GetExpectedAction(), req.GetTargetSemantic()),
	})
	if page.v.Snapshot() == verifier.StateNavigating {
		if _, err := s.waitForStable(bc, page, verifyTimeout(req.GetTimeoutMs())); err != nil {
			if req.GetExpectedAction() == "wait" {
				return nil, err
			}
			// 实际物理交互动作（click/input 等）：SPA 软路由静息超时（如存在长期流式/后台连接）不阻断派发
			page.v = verifier.NewVerifier(nil)
		}
	}

	probeTruncated := false // WASM 探针截断标记（真实 CDP 路径；模拟会话由 SetProbeStats 脚本化）
	// node_id 确定性引用（v1.5.5 混合指挥）：node_id>0 时同样跳过语义/选择器派发，
	// 执行层按 BFS 序回查定位器（GLOBAL §5 #57）。VLM 指挥官返回的 node_id 已经
	// commander 校验存在性，引擎侧仍以「会话不支持 NodeActionSession」显式失败兜底。
	nodeDirect := req.GetNodeId() > 0
	// evaluate 是 In-Page Direct Transport 原语（2026-09-14）：直接在 Runtime 执行 JS 闭包
	evaluateDirect := req.GetExpectedAction() == "evaluate"
	if !dragDirect && !nodeDirect && !evaluateDirect && s.matcher != nil {
		poolEvaluated = true // §5.0.15：决策池评估确实发生（即便结果为空池，也与「未评估」不同号）
		var allRejected bool
		var err error
		cands, allRejected, err = s.candidatesFor(ctx, bc, req)
		if errors.Is(err, matcher.ErrBlockedByNegativeKeyword) {
			// 命中 negative_keywords：不得进入候选池，也不得 LLM 兜底（TDD-MATCH-04）。
			s.noteNotFound()
			return nil, status.Error(codes.NotFound, "blocked by negative keyword")
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "matcher: %v", err)
		}
		if allRejected {
			// L3 物理校验：页面候选结构全部与目标参考不一致（TDD-MATCH-05 集成层）
			// ——页面上无此节点，按 NOT_FOUND 处理。
			s.noteNotFound()
			if resp, err, handled := s.vlmAtNotFound(ctx, bc, req, page, urlBefore, cands); handled {
				return resp, err
			}
			return nil, status.Error(codes.NotFound, "no trusted candidate (L3 structure mismatch)")
		}
		// WASM 探针截断信号：
		// 累计 uws_wasm_truncation_total 并在 decision_trace 标记降级
		// （spec/algorithm-kernel.md §5）。
		if ps, ok := bc.Session().(browser.ProbeStatsSource); ok {
			if st := ps.ProbeStats(); st.Truncated {
				s.wasmTruncTotal.Add(1)
				probeTruncated = true
			}
		}
		candCount = len(cands)
		if len(cands) > 0 {
			for _, c := range cands {
				if c.Confidence > confidence {
					confidence = c.Confidence
				}
			}
			// 决策带（spec/algorithm-kernel.md §5）：C≥θ_high 直接执行；
			// θ_low≤C<θ_high A* 多步搜索；C<θ_low LLM 兜底（NOT_FOUND）。
			// 真实 ONNX 接线后由 classifier 对候选重打分；mock 阶段直接采用候选置信度
			// （TENTATIVE 同义词 0.5+0.2=0.7 落 EXPLORE 区间，TDD-MATCH-02）。
			decision := matcher.Decide(confidence, 0, nil)
			epDecision = episodeMatcherDecision(decision)
			switch decision {
			case matcher.DecisionLLMFallback:
				s.noteNotFound()
				if resp, err, handled := s.vlmAtNotFound(ctx, bc, req, page, urlBefore, cands); handled {
					return resp, err
				}
				return nil, status.Error(codes.NotFound, "no trusted candidate (C < θ_low)")
			case matcher.DecisionAStar:
				selected, depth, ok := s.astarSelect(bc, req, cands)
				if !ok {
					s.noteNotFound()
					if resp, err, handled := s.vlmAtNotFound(ctx, bc, req, page, urlBefore, cands); handled {
						return resp, err
					}
					return nil, status.Error(codes.NotFound, "A* search found no feasible path")
				}
				execSemantic, aStarDepth = selected, depth
			}
		} else {
			decision, score, err := s.clf.Evaluate(ctx, featuresFor(req.GetTargetSemantic()))
			if err != nil {
				return nil, status.Errorf(codes.Internal, "classifier: %v", err)
			}
			confidence = score
			epDecision = episodeClassifierDecision(decision)
			switch decision {
			case classifier.DecisionFallback:
				// C < θ_low：无可信节点（gRPC NOT_FOUND，协调器触发 LLM 规划兜底）。
				s.noteNotFound()
				if resp, err, handled := s.vlmAtNotFound(ctx, bc, req, page, urlBefore, cands); handled {
					return resp, err
				}
				return nil, status.Error(codes.NotFound, "no trusted candidate (C < θ_low)")
			case classifier.DecisionExplore:
				// θ_low ≤ C < θ_high：ε 探索/A*（本次直接执行，记录中间置信度）。
			}
		}
	}

	// WSTN 消费端 Shadow 旁路审计：严格只作比对记录，绝不打断生产 A* 逻辑。
	s.wstnShadowAudit(ctx, bc, req, cands, execSemantic)

	// §5.0.14 决议 0/6：**采样下移到物理层「受体已解析、物理事件未发出」处**。
	// 具备 browser.PreDispatchReceptorSink 能力的会话会在 disp.mark() 之前回传物理受体；
	// 采样在该回调内发生（pivot 解析 → 页内候选快照 → GT 锚定 → 影子打分），故拿到的仍是
	// **决策时**的页面状态（而非导航后的新页面），且仍唯一（首次受体即幂等槽）。
	// 无该能力、或走直发通道（drag/evaluate/node_id，物理层不解析可锚受体）的会话，
	// 维持此处旧点采样一次——行为与上一批一致，不回退。
	if sink, sinkOK := bc.Session().(browser.PreDispatchReceptorSink); sinkOK &&
		!evaluateDirect && !nodeDirect && !dragDirect {
		var (
			recMu   sync.Mutex
			first   browser.PreDispatchReceptor
			haveRec bool
		)
		sink.SetPreDispatchReceptorSink(func(rec browser.PreDispatchReceptor) {
			// fail-soft（§5.0.14 决议 0③）：回调异常绝不得影响决策与物理派发。
			defer func() {
				if r := recover(); r != nil {
					log.Printf("uws episode receptor sink panic: %v", r)
				}
			}()
			recMu.Lock()
			dup := haveRec || !rec.Found
			if !dup {
				first, haveRec = rec, true
			}
			recMu.Unlock()
			if dup {
				return // 幂等：同一 Episode 只按**首次**受体采样一次
			}
			rep := first
			ep.capturePreDispatch(cands, execSemantic, &rep)
		})
		defer sink.SetPreDispatchReceptorSink(nil)
	} else {
		ep.capturePreDispatch(cands, execSemantic, nil)
	}

	// 派发标记（§5.0.11 派发标记时点）：进入执行层调用即算「已派发」，
	// 直发通道（drag/evaluate/node_id）不经过决策核，标签只标注通道。
	if epDecision == "" {
		epDecision = episodeDirectDecision(evaluateDirect, nodeDirect, dragDirect)
	}
	ep.markDispatched()
	if evaluateDirect {
		eval, ok := bc.Session().(browser.DirectScriptEvaluator)
		if !ok {
			s.publishViewerAction(bc.ID, browser.ViewerAction{
				Kind:    "idle",
				Summary: "会话不支持 DirectScriptEvaluator",
			})
			return nil, status.Error(codes.Unimplemented, "session does not support DirectScriptEvaluator")
		}
		s.publishViewerAction(bc.ID, browser.ViewerAction{
			Kind:    "direct_evaluate",
			Summary: "执行页内直发评估脚本",
		})
		resBytes, evalErr := eval.EvaluateScript(ctx, req.GetValue())
		if evalErr != nil {
			s.publishViewerAction(bc.ID, browser.ViewerAction{
				Kind:    "idle",
				Summary: "页内评估失败: " + evalErr.Error(),
			})
			return nil, status.Errorf(codes.Aborted, "evaluate script error: %v", evalErr)
		}
		s.publishViewerAction(bc.ID, browser.ViewerAction{
			Kind:    "idle",
			Summary: "页内评估完成",
		})
		resStr := string(resBytes)
		resp := &uwspb.ActionResultResponse{
			IntentId:   req.GetIntentId(),
			Success:    true,
			Confidence: 1.0,
			LocalVerification: &uwspb.LocalVerification{
				ObservedChanges: []string{"evaluate_success"},
				EventTriggered:  true,
				ProbeStatus:     "evaluated",
				ElapsedMs:       int32(time.Since(start).Milliseconds()),
			},
			DecisionTraceId: fmt.Sprintf("trace-%d", s.traceSeq.Add(1)),
			ContextState:    uwspb.ContextState_STABLE,
			ExtractedData:   &resStr,
			PageUrl:         sessionPageURL(bc.Session()),
		}
		s.notFoundStreak.Store(0)
		cb.RecordSuccess()
		if s.logs != nil {
			_ = s.logs.Record(entryFromAction(req, resp, 0,
				time.Since(start).Milliseconds(), time.Now(), false))
		}
		return resp, nil
	} else if nodeDirect {
		execOK, observed = s.executeByNode(ctx, bc, req)
	} else {
		if s.vlmEnabled() && needsDiscreteChoice(cands) {
			// 模糊候选先复选；不得先派发 A* / 单候选猜测，再用视觉模型复核已发生的动作。
			// vlmTry 自己按「learned → Top-4 复选 → 整页 VLM」逐层降级。
			ep.markVLM()
			vlmAttempted = true
			vlmObserved, vlmTryOutcome = s.vlmTry(ctx, bc, req, execSemantic, "ambiguous_ranker", cands)
			observed = vlmObserved
		} else {
			execOK, observed = s.executePhysical(ctx, bc, req, execSemantic)
			if execOK {
				// §5.0.9 条款 7：确定性点击出口同样判落点自证——判 miss 即硬失败
				// （不写 learned、不换出口），调用方据此跳过下面的 VLM 兜底重问。
				if fault, rejected := s.deterministicLandingFault(bc, req.GetExpectedAction(),
					req.GetTargetSemantic(), execSemantic, aStarDepth); rejected {
					execOK, observed = false, fault
				}
			}
		}
	}
	// 中断（取消/超时）先于任何兜底与重试路径处理：ctx 已经没了，再跑 VLM 兜底
	// 只会在死 ctx 上做无意义的物理尝试，还会把「结果未知」这件事盖掉。
	// 取消不能撤销已经发给 Chrome 的事件，因此这里必须区分「未派发」与「已派发」。
	if canceled, dispatched := browser.IsCanceledObserved(observed); canceled {
		return nil, actionInterruptedError(ctx, dispatched)
	}
	// VLM 兜底：语义/选择器都没找到（no_match / eval 异常/空结果）时看一眼截图，
	// 重新决策并执行；成功样本回写 learned，下次同页面免 VLM（2026-08-27）。
	// 落点自证判 miss 属硬失败：不得换出口（§5.0.9 条款 6/7），故跳过兜底直接走失败出口。
	if !execOK && observed != "challenge_intercepted" && !landingHardFailObserved(observed) && s.vlmEnabled() {
		if !vlmAttempted {
			ep.markVLM()
			vlmAttempted = true
			vlmObserved, vlmTryOutcome = s.vlmTry(ctx, bc, req, execSemantic, observed, cands)
		}
		obs2, outcome := vlmObserved, vlmTryOutcome
		// VLM 路径的物理原语（ClickAt/InputAt/按 learned 选择器执行）同样受调用方 ctx
		// 生命周期约束：中断必须走同一个分类点，不能被当成「候选都试过了」。
		// 已派发后中断 = 结果未知，交 execute 的幂等账本保留去重状态（不盲目重放）。
		if canceled, dispatched := browser.IsCanceledObserved(obs2); canceled {
			return nil, actionInterruptedError(ctx, dispatched)
		}
		if outcome == vlmOutcomeSuccess {
			// §5.0.9 条款 5：vlmOutcomeSuccess 是生产者自报的结局标记，出口必须再校验
			// 观测串是否携带**请求动作对应**的页面侧证据（终局红线「只认页面侧证据」）。
			if vlmObservationBacksAction(req.GetExpectedAction(), obs2) {
				execOK, observed = true, obs2
			} else {
				log.Printf("vlm: success without page-side evidence action=%q obs=%q (%s)",
					req.GetExpectedAction(), obs2, browser.FaultVLMlandingUnverified)
				observed = browser.FaultVLMlandingUnverified + ":成功出口缺请求动作对应的页面侧证据"
			}
		}
	}
	if !execOK {
		if observed == "challenge_intercepted" {
			cb.Trip(&challenge.ChallengeSignal{
				Type:     challenge.ChallengeTypeGenericWAF,
				Provider: "PhysicalLayer",
				Reason:   "物理执行层遭遇安全挑战拦截",
			})
			s.publishViewerAction(bc.ID, browser.ViewerAction{
				Kind:    "guard_challenge",
				Summary: "遭遇安全拦截挑战，触发防乱点熔断保护，等待人工接管",
			})
			// 403 + Turnstile 特征：不可死锁，立即返回 UNAUTHENTICATED
			// （协调器映射为 CHALLENGE_INTERCEPTED，TDD-ENG-05）。
			return nil, status.Error(codes.Unauthenticated, "challenge intercepted")
		}
		// 只读动作（extract）失败不计入防乱点熔断：语义 miss 是确定性结论
		// （「页面上没有这段文本」），不是乱点征兆。2026-09-23 实测：IANA 公开页上
		// 3 次 extract no_match 把整条 session 冻结成无意义空转，与熔断的本意相反。
		tripped := false
		if !readOnlyAction(req.GetExpectedAction()) {
			tripped = cb.RecordFailure(observed)
		}
		if tripped {
			s.publishViewerAction(bc.ID, browser.ViewerAction{
				Kind:    "guard_challenge",
				Summary: "连续失败达到阈值，触发防频繁乱点熔断保护",
			})
		} else {
			s.publishViewerAction(bc.ID, browser.ViewerAction{
				Kind:    "idle",
				Summary: "动作中止",
			})
		}
		if observed != "" {
			// 物理层失败原因原样透传（如模型切换锁定、发送未提交），
			// 比泛化 "action aborted" 更可诊断；Aborted 语义不变。
			return nil, status.Error(codes.Aborted, observed)
		}
		return nil, status.Error(codes.Aborted, "action aborted by physical layer")
	}

	cb.RecordSuccess()

	return s.finishAction(ctx, start, confidence, candCount, probeTruncated, page, bc, req, observed, urlBefore)
}

// finishAction 构建动作成功响应：SPA 时序状态 + 新窗口 + extract（SSE 优先）+
// online_logs 落库。runAction 与 VLM 兜底成功路径共用。
func (s *Server) finishAction(ctx context.Context, start time.Time, confidence float64, candCount int, probeTruncated bool,
	page *pageVerifier, bc *browser.BrowserContext, req *uwspb.ActionIntentRequest, observed string,
	urlBefore string) (*uwspb.ActionResultResponse, error) {
	// 动作后局部验证：捕获 pushState → 立即置 NAVIGATING（异步交接，
	// 绝对禁止在 pushState 触发时立即判完成，spec/algorithm-kernel.md §6.2 step 5）。
	contextState := uwspb.ContextState_STABLE
	if page.navCompleted {
		contextState = uwspb.ContextState_NAVIGATED
		page.navCompleted = false
	}
	if ev, ok := bc.Session().(browser.VerifierEvents); ok {
		push, active, mutated := ev.NavigationState()
		if push {
			page.v.FeedPushState()
			page.v.FeedNetwork(active)
			if mutated {
				page.v.FeedDOMMutation()
			}
			page.navCompleted = false
			contextState = uwspb.ContextState_NAVIGATING
		}
	}
	// 新窗口/新标签页打开（target="_blank" / window.open）：返回 NEW_WINDOW_OPENED
	// 并保留原 Target 可用（TDD-ENG-13）；优先于 NAVIGATING 表达（原页未软路由）。
	newTargetID := ""
	if we, ok := bc.Session().(browser.WindowEvents); ok {
		if id, opened := we.NewWindowOpened(); opened {
			newTargetID = id
			contextState = uwspb.ContextState_NEW_WINDOW_OPENED
		}
	}
	// 新窗口接管（P0-1，2026-09-22 用户现场反馈批次）：默认把会话作用面切到新
	// target——微信快捷登录弹出的授权弹窗必须能被自动点到「允许」，而不是停下等人。
	// 上层若要保留「打开新窗口即暂停」的老行为，动作带 `context_hint.follow_new_target=0`
	// 即可（详见 USER_ACTION.md 2026-09-22 批次 #1）。
	if newTargetID != "" && followNewTargetEnabled(req) {
		if sw, ok := bc.Session().(browser.TargetSwitcher); ok {
			if _, err := sw.SwitchTarget(ctx, newTargetID); err != nil {
				log.Printf("engine: 新窗口接管失败 target=%s: %v（保持原 Target，仍报 NEW_WINDOW_OPENED）",
					newTargetID, err)
			} else {
				s.publishViewerAction(bc.ID, browser.ViewerAction{
					Kind:    "navigate",
					Summary: "已接管新窗口/新标签页，后续动作作用在新页",
				})
			}
		}
	}

	resp := &uwspb.ActionResultResponse{
		IntentId:   req.GetIntentId(),
		Success:    true,
		Confidence: float32(confidence),
		LocalVerification: &uwspb.LocalVerification{
			ObservedChanges: []string{observed},
			ValueUpdated:    req.GetExpectedAction() == "input" || req.GetExpectedAction() == "select",
			EventTriggered:  true,
			ProbeStatus:     "mutated",
			ElapsedMs:       int32(time.Since(start).Milliseconds()),
		},
		DecisionTraceId: fmt.Sprintf("trace-%d", s.traceSeq.Add(1)),
		ContextState:    contextState,
		NewTargetId:     newTargetID,
		ActiveTargetId:  activeContextTargetID(bc),
	}
	if req.GetExpectedAction() == "extract" {
		// 方案 1（聊天 SSE 网络层捕获）：context_hint["prefer_sse"]="1" 且会话
		// 实现 ChatStreamSource 时，extract 优先返回网络层解析的干净 Markdown
		// （无 DOM 引用角标污染）。决策规则（2026-08-26 实测修复：
		// 「页面已答但 API 超时」根因 = 失败/空响应捕获轮永久霸占 extract）：
		//   - Epoch=0（尚未捕获过聊天流）→ 回退 observed（DOM）；
		//   - 快照有内容 → 返回干净 SSE 文本，chat_stream_done 随响应带出
		//     （API 靠它区分「生成中停顿」与「流已结束」，v1.5.2 追加字段）；
		//   - 捕获轮已失败（Failed：loadingFailed/中止/响应体拉取失败）或已
		//     空响应收尾（Done 且无内容）→ 回退 observed：SSE 通道已不可用，
		//     页面可能已渲染完整回答，不能继续轮询空快照；
		//   - 捕获进行中但内容为空（如深度思考阶段）→ 返回空串继续等 SSE，
		//     绝不用 DOM 思考链/旧回答冒充新回答（多轮基线语义由 API 层处理）。
		extracted := observed
		if req.GetContextHint()["prefer_sse"] == "1" {
			if cs, ok := bc.Session().(browser.ChatStreamSource); ok {
				if snap := cs.ChatStreamSnapshot(); snap.Epoch > 0 {
					switch {
					case snap.Failed:
						// 捕获轮失败：SSE 快照不可信 → 保持 observed 回退。
					case snap.Content != "":
						extracted = snap.Content
						resp.ChatStreamDone = proto.Bool(snap.Done)
					case snap.Done:
						// 空响应收尾（空 body/重复请求被页面作废）：回退 DOM。
					default:
						// 捕获进行中（思考/生成中）：继续等 SSE，不拿 DOM 冒充。
						extracted = ""
					}
				}
			}
		}
		if extracted != "" {
			resp.ExtractedData = proto.String(extracted)
		} else if req.GetContextHint()["prefer_sse"] == "1" {
			// SSE 通道已激活但暂无内容：显式空串（区别于「未捕获」的占位）。
			resp.ExtractedData = proto.String("")
		} else {
			resp.ExtractedData = proto.String(`{"semantic":"` + req.GetTargetSemantic() + `"}`)
		}
	}
	// 动作成功 = 失明信号中断；落库 online_logs（PII 脱敏，SEC-01/03）。
	s.notFoundStreak.Store(0)
	// v1.5.4 证据链：回显实际命中元素 + 动作后 URL 与「URL 是否变化」确定性证据。
	// 目的：调用方据此识破「点击命中容器却报成功」假阳性（drama-h5 §3.2 /
	// docs/evidence/research/uws-verdict-evidence-design-2026-08-27.md §6.1）；判定留给调用方
	// （task/assertion 模式由上层按需求校准，引擎只提供证据不替调用方下结论）。
	if he, ok := s.hitElementOf(bc.Session()); ok {
		resp.HitElement = he
	}
	resp.PageUrl = sessionPageURL(bc.Session())
	if urlBefore != "" && resp.PageUrl != "" && urlBefore != resp.PageUrl {
		resp.UrlChanged = true
	}
	if s.logs != nil {
		if err := s.logs.Record(entryFromAction(req, resp, candCount,
			time.Since(start).Milliseconds(), time.Now(), probeTruncated)); err != nil {
			log.Printf("online_logs: %v", err) // 日志失败可观测，不阻断动作
		}
	}
	s.publishViewerAction(bc.ID, browser.ViewerAction{
		Kind:    "idle",
		Summary: "动作完成",
	})
	return resp, nil
}
