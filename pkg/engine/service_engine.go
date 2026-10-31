package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"uws/pkg/browser"
	"uws/pkg/classifier"
	"uws/pkg/matcher"
	"uws/pkg/vlm"
	uwspb "uws/proto/gen/go/proto/v1"
)

func (s *Server) executePhysical(ctx context.Context, bc *browser.BrowserContext, req *uwspb.ActionIntentRequest, semantic string) (bool, string) {
	// R-A：知识库规则选择器贯通物理层——命中规则携带 Selector 时（如 DeepSeek
	// chat 页的输入框/发送按钮/回答区），交给 CDP 会话按选择器精确定位；
	// 无选择器或会话不支持时降级为纯语义直查（原有行为零变化）。
	selector := ""
	if s.matcher != nil {
		if rules, err := s.matcher.RulesBySemantic(semantic); err == nil {
			for _, r := range rules {
				if strings.EqualFold(r.Status, "active") && r.Selector != "" {
					selector = r.Selector
					break
				}
			}
		}
	}
	return s.executeWithSelector(ctx, bc, req.GetExpectedAction(), req.GetValue(), semantic, selector)
}

// executeByNode 按探针 node_id 确定性执行（v1.5.5 混合指挥）：跳过语义匹配与
// 候选池，直接交由物理层 NodeActionSession 按 BFS 序回查定位器。会话不支持该
// 能力时返回显式错误（不静默降级到语义直查，避免 node_id 被误当文本语义）。
func (s *Server) executeByNode(ctx context.Context, bc *browser.BrowserContext, req *uwspb.ActionIntentRequest) (bool, string) {
	ns, ok := bc.Session().(browser.NodeActionSession)
	if !ok {
		return false, "node_id_unsupported"
	}
	ok2, observed := ns.ExecuteActionByNode(ctx, req.GetExpectedAction(), req.GetValue(), req.GetNodeId())
	if ok2 {
		s.learnNodeHit(bc, req)
	}
	return ok2, observed
}

// learnNodeHit 把 node_id 确定性执行成功回写自进化（v1.5.5 混合指挥）：
// 用命中元素证据（selector + 可见文本）记录「页面 + 元素文本 → 定位器」，
// 下次同页面同文本直接走 learned，跳过 VLM 指挥官。语义键优先元素可见文本
// （可被语义层命中）；文本为空/形如 "node:" 时退回 target_semantic。
func (s *Server) learnNodeHit(bc *browser.BrowserContext, req *uwspb.ActionIntentRequest) {
	if s.vlmFb == nil || s.vlmFb.Learned == nil {
		return
	}
	url := ""
	if vp, ok := bc.Session().(browser.VLMProbeSession); ok {
		url = vp.PageURL()
	}
	if url == "" {
		return
	}
	hs, ok := bc.Session().(browser.HitElementSource)
	if !ok {
		return
	}
	h, ok := hs.LastHitElement()
	if !ok || h.Selector == "" {
		return
	}
	semantic := strings.TrimSpace(h.Text)
	if semantic == "" || strings.HasPrefix(semantic, "node:") {
		semantic = strings.TrimSpace(req.GetTargetSemantic())
	}
	if semantic == "" {
		return
	}
	// §5.0.9 条款 6 落点自证：node_id 点击同样只在落点「相关」时写 learned。
	if allowed, why := clickLearnGate(bc, req.GetExpectedAction(), req.GetTargetSemantic()); !allowed {
		log.Printf("vlm: node learned skipped semantic=%q %s", req.GetTargetSemantic(), why)
		return
	}
	s.vlmLearn(url, semantic, vlm.LearnedEntry{Selector: h.Selector, NodeText: h.Text})
}

// candidatesFor 收集本次动作的候选池（R1/MATCH-05/06 集成层）：
//   - 页面候选通道存在（SimCDPSession 已注入候选节点，模拟 WASM 探针 AXTree 输出）
//     时以页面节点为准：逐节点 L1/L2 匹配 + L3 SimHash 结构校验；
//   - 否则回退到语义直查（MatchQuery(semantic)）。
//
// 返回（候选池, L3 全剔除标记, 错误）。
func (s *Server) candidatesFor(ctx context.Context, bc *browser.BrowserContext,
	req *uwspb.ActionIntentRequest) ([]matcher.Candidate, bool, error) {
	hints := req.GetContextHint()
	if src, ok := bc.Session().(browser.CandidateSource); ok {
		nodes := src.PageCandidates()
		if len(nodes) > 0 {
			return s.pageCandidatePool(ctx, nodes, req, hints)
		}
	}
	cands, err := s.matcher.MatchQuery(req.GetTargetSemantic(), hints)
	// §5.0.15 条款 1.4：无页面候选时退化为语义直查，来源如实标注（不改匹配行为）。
	for i := range cands {
		if cands[i].Source == "" {
			cands[i].Source = "semantic_direct"
		}
	}
	return cands, false, err
}

// pageCandidatePool 把页面候选节点匹配为候选池并执行 L3 物理校验：
// L1 候选（含探针显式置信度）做 SimHash 结构比对（汉明距离 > D_h 剔除）；
// L2 同义词候选为离线知识库已背书，跳过 L3 文本代理校验（防误伤跨语言同义）。
func (s *Server) pageCandidatePool(ctx context.Context, nodes []browser.PageCandidate,
	req *uwspb.ActionIntentRequest,
	hints map[string]string) ([]matcher.Candidate, bool, error) {
	var cands []matcher.Candidate
	// 真实 DOM 候选树（WASM 探针 Children 填充后，MATCH-06 真实侧输入）预序遍历：
	// 父容器与嵌套子节点全部直接入围匹配（目标按钮通常是容器内层节点），
	// 不能只匹配根；Children 结构保留给 A* 多步搜索消费。
	var ingest func(n browser.PageCandidate) error
	ingest = func(n browser.PageCandidate) error {
		if n.Confidence > 0 {
			// 探针/特征打分显式置信度：直接入围（模拟真实 ONNX 特征打分）。
			cands = append(cands, matcher.Candidate{
				NodeText:   n.Text,
				Confidence: n.Confidence,
				Stage:      "L1",
				StructHash: n.StructHash,
				Tag:        n.Tag,
				Role:       n.Role,
				Source:     "page_probe",
				Selector:   n.Selector,
				X:          n.X,
				Y:          n.Y,
				W:          n.W,
				H:          n.H,
				DocumentX:  n.DocumentX,
				DocumentY:  n.DocumentY,
			})
			return nil
		}
		// 真实 CDP DOM 采集节点（Confidence=0，未打分）：仅吸收 L2 同义词候选
		// （离线知识库已背书、跳过 L3 防误伤）；L1 由语义直查路径覆盖——物理层
		// findElement 按目标语义精确定位，避免页面上无关节点（如其他按钮）误入
		// 候选池后被 L3 结构校验全剔成假 NOT_FOUND（真实 CDP E2E 实测）。
		mc, err := s.matcher.MatchQuery(n.Text, hints)
		if err != nil {
			if errors.Is(err, matcher.ErrBlockedByNegativeKeyword) {
				return nil // 黑名单节点不入池（含 ONNX 打分通道，N3 记录不修项收口）
			}
			return err
		}
		l2Hit := false
		for _, c := range mc {
			if c.Stage == "L2" {
				l2Hit = true
				// §5.0.15：来源＝页面候选 × 离线背书同义词。
				c.Source = "page_l2_kb"
				c.Selector = n.Selector
				c.X = n.X
				c.Y = n.Y
				c.W = n.W
				c.H = n.H
				c.DocumentX = n.DocumentX
				c.DocumentY = n.DocumentY
				cands = append(cands, c)
			}
		}
		// 真实 ONNX（Phase 2 / §5.0.18）：对未打分的页面节点按「10 维多模态特征」打分，
		// 决策核输入涵盖文本、Tag/Role、盒模型几何显著度、上下文回响、AX 地标先验。
		// L2 同义词为离线背书（0.7 落 EXPLORE 区间），不再被 ONNX 覆盖。
		if !l2Hit {
			s.scorerMu.RLock()
			scorer := s.onnxScorer
			s.scorerMu.RUnlock()
			if scorer != nil {
				nodeCtx := classifier.NodeContext{
					Tag:              n.Tag,
					Role:             n.Role,
					Width:            n.W,
					Height:           n.H,
					ContextText:      n.Context,
					Landmark:         n.Landmark,
					DistanceToAnchor: -1.0,
				}
				score, err := scorer.Score(ctx, classifier.Features10(n.Text, req.GetTargetSemantic(), nodeCtx))
				if err != nil {
					return fmt.Errorf("onnx score(%q): %w", n.Text, err)
				}
				cands = append(cands, matcher.Candidate{
					NodeText:   n.Text,
					Confidence: score,
					Stage:      "L1",
					StructHash: n.StructHash,
					Tag:        n.Tag,
					Role:       n.Role,
					Source:     "page_onnx",
					Selector:   n.Selector,
					X:          n.X,
					Y:          n.Y,
					W:          n.W,
					H:          n.H,
					DocumentX:  n.DocumentX,
					DocumentY:  n.DocumentY,
				})
			}
		}
		return nil
	}
	var walk func([]browser.PageCandidate) error
	walk = func(list []browser.PageCandidate) error {
		for _, n := range list {
			if err := ingest(n); err != nil {
				return err
			}
			if err := walk(n.Children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(nodes); err != nil {
		return nil, false, err
	}
	if len(cands) == 0 {
		return nil, false, nil
	}
	var l1, l2 []matcher.Candidate
	for _, c := range cands {
		if c.Stage == "L2" {
			l2 = append(l2, c)
		} else {
			l1 = append(l1, c)
		}
	}
	ref := matcher.SimHash64(req.GetTargetSemantic())
	// Tier 1.5 双通道 L3 接线：L1 候选（ONNX/置信度打分）不带 Matches，
	// bestStructRule(cands) 取不到参考结构 → 由 KB 规则库显式取参考。
	refStruct, oracleRole, hasStructRef := s.matcher.StructuralReference(req.GetTargetSemantic())
	var kept, rejected []matcher.Candidate
	if hasStructRef {
		// 双通道：文本 OR 结构[且 role 兼容]；结构参考来自规则库（component_gene 写入）。
		kept, rejected = matcher.RefineByStructureDual(l1, ref, refStruct, "", oracleRole, 0)
	} else {
		// 规则库未提供结构参考（ComponentGene 未校准）：保持文本单通道既有语义。
		kept, rejected = matcher.RefineByStructure(l1, ref, 0)
	}
	// ★Phase 2（§5.0.18 条款 3）：Ranker 高分通道与全局 Top-4 都不得仅因
	// 文本 SimHash 不同而被 L3 剔除；不能等 L3 把池清空后再救，否则高分目标
	// 一旦已保留，歧义竞争候选就会被剪掉，Phase 3 永远看不到完整 Top-4。
	kept = append(kept, protectedRankerCandidates(l1, rejected)...)
	if len(kept) == 0 && len(rejected) > 0 && len(l2) == 0 {
		return nil, true, nil
	}
	return append(kept, l2...), false, nil
}

type rankerCandidateIdentity struct {
	text, selector, tag, role string
	x, y, w, h                float64
	documentX, documentY      float64
}

func rankerIdentity(c matcher.Candidate) rankerCandidateIdentity {
	return rankerCandidateIdentity{
		text: c.NodeText, selector: c.Selector, tag: c.Tag, role: c.Role,
		x: c.X, y: c.Y, w: c.W, h: c.H, documentX: c.DocumentX, documentY: c.DocumentY,
	}
}

// protectedRankerCandidates restores rejected ONNX/heuristic candidates when
// their score clears the low threshold or they were in the full-page Ranker Top-4.
func protectedRankerCandidates(ranked, rejected []matcher.Candidate) []matcher.Candidate {
	ordered := make([]matcher.Candidate, 0, len(ranked))
	for _, c := range ranked {
		if c.Source == "page_onnx" || c.Source == "page_heuristic" {
			ordered = append(ordered, c)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Confidence > ordered[j].Confidence })
	if len(ordered) > 4 {
		ordered = ordered[:4]
	}
	top4 := make(map[rankerCandidateIdentity]struct{}, len(ordered))
	for _, c := range ordered {
		top4[rankerIdentity(c)] = struct{}{}
	}

	var protected []matcher.Candidate
	for _, c := range rejected {
		if c.Source != "page_onnx" && c.Source != "page_heuristic" {
			continue
		}
		_, isTop4 := top4[rankerIdentity(c)]
		if c.Confidence >= classifier.ThresholdLow || isTop4 {
			protected = append(protected, c)
		}
	}
	return protected
}

// astarSelect 对中间置信度候选执行 A* 多步搜索（TDD-MATCH-06 集成层）：
// 候选池 → 根节点；页面候选图（PageCandidate.Children）提供子节点；
// 搜索成功返回路径末步节点文本（可执行目标）与该路径长度（§5.0.9 条款 7：
// 长度 ≥2 才算「多步换步」，长度 1 的路径根即答案）。物理黑名单跨请求驻留（服务级）。
func (s *Server) astarSelect(bc *browser.BrowserContext, req *uwspb.ActionIntentRequest,
	cands []matcher.Candidate) (string, int, bool) {
	ref := matcher.SimHash64(req.GetTargetSemantic())
	graph := pageSearchGraph(s.pageCandidates(bc))
	roots := make([]*matcher.SearchNode, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		if seen[c.NodeText] {
			continue
		}
		seen[c.NodeText] = true
		root := &matcher.SearchNode{
			Text:       c.NodeText,
			Confidence: c.Confidence,
			SimHash:    matcher.SimHash64(c.NodeText),
		}
		if n, ok := graph[c.NodeText]; ok {
			root = n
			if root.Confidence <= 0 {
				root.Confidence = c.Confidence
			}
			if root.SimHash == 0 {
				root.SimHash = matcher.SimHash64(c.NodeText)
			}
		}
		roots = append(roots, root)
	}
	res := matcher.AStarSearch(roots, matcher.SearchOptions{RefHash: ref, Blacklist: s.astarBlacklist})
	if len(res.Path) == 0 {
		return "", 0, false
	}
	return res.Path[len(res.Path)-1].Node.Text, len(res.Path), true
}

func (s *Server) pageCandidates(bc *browser.BrowserContext) []browser.PageCandidate {
	if src, ok := bc.Session().(browser.CandidateSource); ok {
		return src.PageCandidates()
	}
	return nil
}

// pageSearchGraph 把页面候选图（PageCandidate 树）构建为 text → SearchNode 映射，
// 供 A* 根节点接子图（测试手工构造；真实 CDP 后由 DOM/AXTree 特征填充）。
func pageSearchGraph(nodes []browser.PageCandidate) map[string]*matcher.SearchNode {
	out := make(map[string]*matcher.SearchNode)
	var build func([]browser.PageCandidate) []*matcher.SearchNode
	build = func(list []browser.PageCandidate) []*matcher.SearchNode {
		var kids []*matcher.SearchNode
		for _, n := range list {
			node := &matcher.SearchNode{
				Text:       n.Text,
				Confidence: n.Confidence,
				SimHash:    n.SimHash,
				Flow:       n.Flow,
				HTTPStatus: n.HTTPStatus,
				Children:   build(n.Children),
			}
			if n.SimHash == 0 {
				node.SimHash = matcher.SimHash64(n.Text)
			}
			out[n.Text] = node
			kids = append(kids, node)
		}
		return kids
	}
	build(nodes)
	return out
}
