package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
	"uws/pkg/matcher"
)

// probeWasmCandidates 返回探针 wasm 候选路径：先相对候选（引擎 CWD = 仓库根、
// 包内测试 CWD = pkg/browser），再按可执行文件真实位置补「bin/ 布局」候选
// （bin/uws-engine → ../build/...；/usr/local/bin/uws-engine 软链也解析回仓库 bin）。
func probeWasmCandidates() []string {
	cands := []string{
		"build/probe_wasm_wasm/probe_wasm.wasm",
		"../../build/probe_wasm_wasm/probe_wasm.wasm",
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		if exeDir := filepath.Dir(exe); exeDir != "" {
			cands = append(cands,
				filepath.Join(exeDir, "..", "build", "probe_wasm_wasm", "probe_wasm.wasm"),
				filepath.Join(exeDir, "build", "probe_wasm_wasm", "probe_wasm.wasm"),
			)
		}
	}
	return cands
}

// probeWasmBase64 读取探针 wasm 字节并 base64（惰性缓存；缺失返回错误 → 降级）。
func (s *cdpSession) probeWasmBase64() (string, error) {
	s.probeOnce.Do(func() {
		var path string
		if s.probePath != "" {
			path = s.probePath
		} else {
			for _, p := range probeWasmCandidates() {
				if fileExists(p) {
					path = p
					break
				}
			}
		}
		if path == "" {
			s.probeErr = errors.New("probe: wasm 未找到（make probe-wasm 构建；或 --probe-wasm 指定路径）")
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			s.probeErr = fmt.Errorf("probe: %w", err)
			return
		}
		s.probeB64 = base64.StdEncoding.EncodeToString(b)
	})
	return s.probeB64, s.probeErr
}

// installPreProbeScript 注册文档级探针预实例化脚本（TDD-OPT-01）。脚本注册
// 失败静默返回（采集路径保留按需实例化兜底，会话不受影响）。
func (s *cdpSession) installPreProbeScript(ctx context.Context, b64 string) error {
	return s.client.CallSession(ctx, s.sessionID, "Page.addScriptToEvaluateOnNewDocument",
		map[string]any{"source": probePreinstallExpr(b64)}, nil)
}

// collectProbeCandidates 执行注入 + 采集 + 截断补偿，返回候选与探针状态。
// 任何一步失败返回错误（调用方降级到 JS 文本采集）。
func (s *cdpSession) collectProbeCandidates(ctx context.Context) ([]PageCandidate, ProbeStats, error) {
	b64, err := s.probeWasmBase64()
	if err != nil {
		return nil, ProbeStats{}, err
	}
	var res struct {
		OK        bool   `json:"ok"`
		Kept      int    `json:"kept"`
		Truncated bool   `json:"truncated"`
		Overflow  int    `json:"overflow"`
		Fallback  bool   `json:"fallback"`
		Blocked   bool   `json:"blocked"`
		Err       string `json:"err"`
		Nodes     []struct {
			Text      string  `json:"t"`
			Hash      string  `json:"h"`
			SH        string  `json:"sh"`
			ID        uint32  `json:"id"`
			PID       int     `json:"pid"`
			Tag       string  `json:"tag"`
			Role      string  `json:"role"`
			PTag      string  `json:"ptag"`
			PRole     string  `json:"prole"`
			Selector  string  `json:"sel"`
			X         float64 `json:"x"`
			Y         float64 `json:"y"`
			DocumentX float64 `json:"dx"`
			DocumentY float64 `json:"dy"`
			W         float64 `json:"w"`
			H         float64 `json:"height"`
		} `json:"nodes"`
	}
	if err := s.evalValue(ctx, probeCollectExpr(b64), &res); err != nil {
		// §5.0.8 条款 3：CSP 拒绝 WASM（异常直接抛回 CDP）时也必须是显式降级，
		// 错误串携带 csp_wasm_blocked，禁止只留人类可读的「AXTree 不可用」。
		if code := classifyProbeDegradation(err.Error()); code != "" {
			s.setProbeDegradation(code)
			return nil, ProbeStats{Degradation: code}, fmt.Errorf("%s: %w", code, err)
		}
		return nil, ProbeStats{}, err
	}
	if !res.OK {
		if code := classifyProbeDegradation(res.Err); code != "" {
			s.setProbeDegradation(code)
			return nil, ProbeStats{Degradation: code},
				fmt.Errorf("%s: 页内 WASM 探针不可用（%s）", code, res.Err)
		}
		return nil, ProbeStats{}, errors.New("probe: bulk ingest failed")
	}
	// 保留全部 kept 节点（含语义文本为空的父容器）参与建树——空文本父节点
	// 是 Children 链路的锚点，建树后再剪除（子节点上提），避免父子链断裂。
	raw := make([]probeCandidate, 0, len(res.Nodes))
	for _, n := range res.Nodes {
		var sh uint64
		if n.SH != "" {
			sh, _ = strconv.ParseUint(n.SH, 10, 64)
		}
		raw = append(raw, probeCandidate{
			Text: n.Text, Hash: n.Hash, ID: n.ID, PID: n.PID,
			Tag: n.Tag, Role: n.Role, ParentTag: n.PTag, ParentRole: n.PRole,
			StructHash: sh,
			Selector:   n.Selector, X: n.X, Y: n.Y, DocumentX: n.DocumentX,
			DocumentY: n.DocumentY, W: n.W, H: n.H,
		})
	}
	// 真实 DOM 候选树（MATCH-06 真实侧输入通道）：按 BFS 父子链路重建
	// Children；ast_semantic_hash 仅对 JS 处理程序源计算，DOM 文本候选为 0，
	// 由引擎侧 SimHash64(Text) 文本代理补齐 L3 结构校验（GLOBAL.md §5 #13）。
	cands := buildProbeCandidates(raw)
	stats := ProbeStats{
		Kept:          res.Kept,
		Truncated:     res.Truncated,
		Overflow:      res.Overflow,
		NeedsFallback: res.Fallback,
	}
	if stats.Truncated && stats.NeedsFallback && stats.Overflow > 0 {
		cands = append(cands, s.compensateProbeCandidates(ctx)...)
	}
	return cands, stats, nil
}

// probeCandidate 是探针 kept 节点的最小可重建信息：文本 + AST 哈希 + BFS 序
// node_id + 父 BFS 索引（-1 = 根/未知）。父索引由 JS 采集器从 pIdx 直接给出，
// 与 C++ next_id_（按输入序分配）一一对应，无需 xpath/文本匹配。
type probeCandidate struct {
	Text       string
	Hash       string
	StructHash uint64
	ID         uint32
	PID        int
	Tag        string
	Role       string
	ParentTag  string
	ParentRole string
	Selector   string
	X          float64
	Y          float64
	DocumentX  float64
	DocumentY  float64
	W          float64
	H          float64
}

// buildProbeCandidates 把探针 kept 节点重建为 PageCandidate 树
// （真实 DOM 候选 Children 填充，USER_ACTION.md #12/#14）：
//   - 按 node_id（BFS 输入序）稳定排序，兄弟按 DOM 顺序排列（确定性）；
//   - 父节点在 kept 集内 → 挂到其 Children；父被驱逐/截断 → 提升为根
//     （induced subgraph，保留可达结构，不伪造父链）；
//   - 根列表保持 BFS 序。
func buildProbeCandidates(nodes []probeCandidate) []PageCandidate {
	if len(nodes) == 0 {
		return nil
	}
	type candNode struct {
		id         uint32
		pid        int
		text       string
		simHash    uint64
		tag        string
		role       string
		selector   string
		x, y       float64
		documentX  float64
		documentY  float64
		w, h       float64
		parent     *candNode
		structHash uint64
		children   []*candNode
	}
	byID := make(map[uint32]*candNode, len(nodes))
	order := make([]*candNode, 0, len(nodes))
	for _, n := range nodes {
		var h uint64
		if n.Hash != "" {
			h, _ = strconv.ParseUint(n.Hash, 10, 64)
		}
		cn := &candNode{
			id: n.ID, pid: n.PID, text: n.Text, simHash: h, tag: n.Tag, role: n.Role,
			selector: n.Selector, x: n.X, y: n.Y, documentX: n.DocumentX, documentY: n.DocumentY, w: n.W, h: n.H,
		}
		cn.structHash = n.StructHash // Tier 1.5：优先用探针内置（O(1)、截断稳健）
		byID[n.ID] = cn
		order = append(order, cn)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].id < order[j].id })
	var roots []*candNode
	for _, cn := range order {
		if cn.pid >= 0 {
			if p, ok := byID[uint32(cn.pid)]; ok && p != cn {
				p.children = append(p.children, cn)
				cn.parent = p
				continue
			}
		}
		roots = append(roots, cn)
	}

	// 结构签名：优先用探针内置 structural_hash（非 0，O(1)）；为 0 时回退
	// 沿父链收集最近语义祖先 role（跳过 generic/none 幽灵包装）用阻尼结构流
	// 计算 StructuralSimHash64（截断致祖先丢失的兜底路径）。
	for _, cn := range order {
		if cn.structHash != 0 {
			continue
		}
		anc := []matcher.StructAncestor{}
		for p := cn.parent; p != nil; p = p.parent {
			anc = append(anc, matcher.StructAncestor{Tag: p.tag, Role: p.role})
		}
		feat := matcher.StructFeature{Tag: cn.tag, Role: cn.role, Ancestors: anc}
		cn.structHash = matcher.StructuralSimHash64(feat)
	}

	// 剪除语义文本为空的节点（父容器/非交互节点），其子节点上提到父位：
	// 保持「非空候选」之间的诱导子树（induced subgraph），不伪造父子关系。
	var prune func([]*candNode) []*candNode
	prune = func(list []*candNode) []*candNode {
		out := make([]*candNode, 0, len(list))
		for _, cn := range list {
			kids := prune(cn.children)
			if cn.text == "" {
				out = append(out, kids...)
			} else {
				cn.children = kids
				out = append(out, cn)
			}
		}
		return out
	}
	roots = prune(roots)
	var serialize func([]*candNode) []PageCandidate
	serialize = func(list []*candNode) []PageCandidate {
		out := make([]PageCandidate, 0, len(list))
		for _, cn := range list {
			out = append(out, PageCandidate{
				Text:       cn.text,
				SimHash:    cn.simHash,
				StructHash: cn.structHash,
				Tag:        cn.tag,
				Role:       cn.role,
				Selector:   cn.selector,
				X:          cn.x,
				Y:          cn.y,
				DocumentX:  cn.documentX,
				DocumentY:  cn.documentY,
				W:          cn.w,
				H:          cn.h,
				Children:   serialize(cn.children),
			})
		}
		return out
	}
	return serialize(roots)
}

// compensateProbeCandidates 在 200ms 硬预算内补充溢出队列 BFS 前 200 节点；
// 超时/异常只返回 nil（截断结果不阻塞主流程，TDD-WASM-07）。
func (s *cdpSession) compensateProbeCandidates(parent context.Context) []PageCandidate {
	ctx, cancel := context.WithTimeout(parent, 200*time.Millisecond)
	defer cancel()
	var texts []string
	if err := s.evalValue(ctx, probeCompensateExpr(), &texts); err != nil {
		return nil
	}
	out := make([]PageCandidate, 0, len(texts))
	for _, t := range texts {
		out = append(out, PageCandidate{Text: t})
	}
	return out
}

// ProbeStats 返回最近一次采集的探针状态（browser.ProbeStatsSource）。
func (s *cdpSession) ProbeStats() ProbeStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probeStats
}

// setProbeDegradation 记录感知降级代号（§5.0.8 条款 3）。降级在探针失败路径上
// 发生，此时采集可能回退到 JS 通道，故必须独立落盘——否则降级标记会随失败一起丢掉。
func (s *cdpSession) setProbeDegradation(code string) {
	if code == "" {
		return
	}
	s.mu.Lock()
	s.probeStats.Degradation = code
	s.mu.Unlock()
	log.Printf("uws probe degradation=%s（§5.0.8 条款 3：感知降级必须显式标记）", code)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
