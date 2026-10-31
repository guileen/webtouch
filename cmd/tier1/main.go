// Tier 1 / Tier 1.5 零点标定 runner（Phase 0）。
//
// 输入：build/tier1/drift_set.json（500 案例，页面无 is_truth 作弊通道）
//   - build/tier1/golden_truth.json（oracle 身份 + oracle_struct 参考结构）。
//
// 输出（每 case 三轨，供 Python judge 判分）：
//
//	heuristic —— P_base 基线：L1 AC 文本命中 + L3 文本 SimHash 单通道，页面 BFS 序取首个；
//	tier15    —— Tier 1.5：L1 + 双通道 L3（文本 OR 带阻尼的 DOM 拓扑结构哈希，且
//	             Malhotra tag/role 兼容）+ 结构距离择优 + 身份门禁（不兼容不执行）；
//	nodeid    —— node_id 降级（original BFS 序回查，隔离报告，不进综合）。
//
// 结构通道（matcher.StructuralSimHash64）：相对深度 + 跳过 generic/none 幽灵包装 +
// 最近语义祖先 role + aria-/data-* 属性白名单，不含文本 token（文本由文本通道负责）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"uws/pkg/matcher"
)

type node struct {
	NodeID  int               `json:"node_id"`
	Text    string            `json:"text"`
	Tag     string            `json:"tag"`
	Role    string            `json:"role"`
	Attrs   map[string]string `json:"attrs"`
	Classes []string          `json:"classes"`
	PID     int               `json:"pid"`
}

type mutationSpec struct {
	Type string `json:"type"`
}

type t1case struct {
	CaseID         string       `json:"case_id"`
	Category       string       `json:"category"`
	Semantic       string       `json:"semantic"`
	Mutation       mutationSpec `json:"mutation"`
	OriginalNodeID int          `json:"original_node_id"`
	OriginalPage   []node       `json:"original_page"`
	MutatedPage    []node       `json:"mutated_page"`
}

type oracleEntry struct {
	CaseID string `json:"case_id"`
	Oracle struct {
		Text string `json:"text"`
		Tag  string `json:"tag"`
		Role string `json:"role"`
	} `json:"oracle"`
	OracleStruct struct {
		Tag       string                   `json:"tag"`
		Role      string                   `json:"role"`
		Attrs     []string                 `json:"attrs"`
		Ancestors []matcher.StructAncestor `json:"ancestors"`
	} `json:"oracle_struct"`
}

type foundNode struct {
	NodeID     int    `json:"node_id"`
	Text       string `json:"text"`
	Tag        string `json:"tag"`
	Role       string `json:"role"`
	TextDist   int    `json:"text_hamming"`
	StructDist int    `json:"struct_hamming"`
}

// cand 是 L1 命中的页面候选（含文本/结构双通道距离，供 L3 双通道与择优）。
type cand struct {
	n          *node
	textDist   int
	structDist int
	roleCompat bool
}

// poolCand 是 L1 检索候选池的完整快照（BFS 序，未经双通道过滤），供离线复算任意
// 阈值下的候选质量口径（recall@k / MRR / Top-k 落点相关性）。不含真值，仅供判分。
type poolCand struct {
	NodeID     int    `json:"node_id"`
	Text       string `json:"text"`
	Tag        string `json:"tag"`
	Role       string `json:"role"`
	TextDist   int    `json:"text_hamming"`
	StructDist int    `json:"struct_hamming"`
	RoleCompat bool   `json:"role_compat"`
}

// stageTiming 是单阶段累计墙钟（离线合成语料，单机；不计入字节可回放口径）。
type stageTiming struct {
	Stage     string  `json:"stage"`
	TotalMs   float64 `json:"total_ms"`
	PerCaseUs float64 `json:"per_case_us"`
}

type result struct {
	CaseID       string      `json:"case_id"`
	Semantic     string      `json:"semantic"`
	MutationType string      `json:"mutation_type"`
	Heuristic    *foundNode  `json:"heuristic"`
	Tier15       *foundNode  `json:"tier15"`
	NodeID       *foundNode  `json:"nodeid"`
	Candidates   []foundNode `json:"candidates"` // 双通道 L3 保留的候选集（供 ONNX 重排）
	L1Pool       []poolCand  `json:"l1_pool"`    // L1 全量候选池（诊断口径，不参与选优）
}

// nodeAncestors 沿 pid 链收集父→祖祖先（用于阻尼结构流）。
func nodeAncestors(pages []node, n *node) []matcher.StructAncestor {
	byID := make(map[int]*node, len(pages))
	for i := range pages {
		byID[pages[i].NodeID] = &pages[i]
	}
	var out []matcher.StructAncestor
	cur := n
	seen := map[int]bool{}
	for cur != nil && cur.PID >= 0 && !seen[cur.PID] {
		seen[cur.PID] = true
		p, ok := byID[cur.PID]
		if !ok {
			break
		}
		out = append(out, matcher.StructAncestor{Tag: p.Tag, Role: p.Role})
		cur = p
	}
	return out
}

func structFeature(n *node, pages []node) matcher.StructFeature {
	attrs := make([]string, 0, len(n.Attrs))
	for k := range n.Attrs {
		attrs = append(attrs, k)
	}
	sort.Strings(attrs)
	return matcher.StructFeature{
		Tag:       n.Tag,
		Role:      n.Role,
		Attrs:     attrs,
		Ancestors: nodeAncestors(pages, n),
	}
}

func main() {
	in := flag.String("in", "build/tier1/drift_set.json", "输入 drift_set.json")
	golden := flag.String("golden", "build/tier1/golden_truth.json", "输入 golden_truth.json")
	out := flag.String("out", "build/tier1/runner_output.json", "输出 runner 结果")
	flag.Parse()

	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读输入失败:", err)
		os.Exit(1)
	}
	var cases []t1case
	if err := json.Unmarshal(raw, &cases); err != nil {
		fmt.Fprintln(os.Stderr, "解析 drift_set 失败:", err)
		os.Exit(1)
	}
	graw, err := os.ReadFile(*golden)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读 golden 失败:", err)
		os.Exit(1)
	}
	var entries []oracleEntry
	if err := json.Unmarshal(graw, &entries); err != nil {
		fmt.Fprintln(os.Stderr, "解析 golden 失败:", err)
		os.Exit(1)
	}
	oracleByID := make(map[string]oracleEntry, len(entries))
	for _, e := range entries {
		oracleByID[e.CaseID] = e
	}

	// 内存规则库：500 语义 → L1 text_pattern 规则（纯启发式；无同义词/无 A*/ONNX）。
	rules := make([]matcher.Rule, 0, len(cases))
	seen := make(map[string]bool)
	for _, c := range cases {
		if seen[c.Semantic] {
			continue
		}
		seen[c.Semantic] = true
		rules = append(rules, matcher.Rule{
			RuleType:      "text_pattern",
			TextPattern:   c.Semantic,
			Action:        "click",
			SemanticLabel: c.Semantic,
			Confidence:    0.9,
			Source:        "tier1",
			Status:        "active",
		})
	}
	m, err := matcher.New(":memory:")
	if err != nil {
		fmt.Fprintln(os.Stderr, "matcher 初始化失败:", err)
		os.Exit(1)
	}
	defer m.Close()
	if err := m.ReplaceAll(rules, nil, nil); err != nil {
		fmt.Fprintln(os.Stderr, "规则装载失败:", err)
		os.Exit(1)
	}

	// 分段耗时累加（离线合成语料，单机墙钟；不计入字节可回放口径）。
	var tL1, tText, tStruct, tGate, tSelect time.Duration
	results := make([]result, 0, len(cases))
	for _, c := range cases {
		oc, ok := oracleByID[c.CaseID]
		if !ok {
			fmt.Fprintln(os.Stderr, "缺 oracle:", c.CaseID)
			os.Exit(1)
		}
		refText := matcher.SimHash64(c.Semantic)
		refStruct := matcher.StructuralSimHash64(matcher.StructFeature{
			Tag:       oc.OracleStruct.Tag,
			Role:      oc.OracleStruct.Role,
			Attrs:     oc.OracleStruct.Attrs,
			Ancestors: oc.OracleStruct.Ancestors,
		})
		oracleF := matcher.StructFeature{Tag: oc.Oracle.Tag, Role: oc.Oracle.Role}

		// L1 候选池（页面 BFS 序）。
		var pool []cand
		for i := range c.MutatedPage {
			n := &c.MutatedPage[i]
			if n.Text == "" {
				continue
			}
			t0 := time.Now()
			cands, err := m.MatchNode(n.Text)
			tL1 += time.Since(t0)
			if err != nil || len(cands) == 0 {
				continue
			}
			t0 = time.Now()
			textDist := matcher.HammingDistance(matcher.SimHash64(n.Text), refText)
			tText += time.Since(t0)
			t0 = time.Now()
			sf := structFeature(n, c.MutatedPage)
			structDist := matcher.HammingDistance(matcher.StructuralSimHash64(sf), refStruct)
			tStruct += time.Since(t0)
			pool = append(pool, cand{
				n:          n,
				textDist:   textDist,
				structDist: structDist,
				roleCompat: matcher.StructRoleCompatible(sf, oracleF),
			})
		}

		// P_base 基线：文本单通道 L3（textDist ≤ D_h），BFS 序取首个，无身份门禁。
		var h *foundNode
		for i := range pool {
			if pool[i].textDist <= matcher.SimHashDistanceThreshold {
				h = &foundNode{NodeID: pool[i].n.NodeID, Text: pool[i].n.Text,
					Tag: pool[i].n.Tag, Role: pool[i].n.Role,
					TextDist: pool[i].textDist, StructDist: pool[i].structDist}
				break
			}
		}

		// Tier 1.5：双通道 L3（文本 OR 结构[且 role 兼容]）+ 结构距离择优 + 身份门禁。
		tGate0 := time.Now()
		var kept []cand
		for _, cd := range pool {
			textOK := cd.textDist <= matcher.SimHashDistanceThreshold
			compat := matcher.StructRoleCompatible(
				matcher.StructFeature{Tag: cd.n.Tag, Role: cd.n.Role}, oracleF)
			structOK := cd.structDist <= matcher.SimHashDistanceThreshold && compat
			if textOK || structOK {
				kept = append(kept, cd)
			}
		}
		// A* 拓扑剪枝门（[State:D] 决策桥）：仅结构 Hamming ≤ 软阈值的节点进入 ONNX 重排区，
		// 防止全页候选图无条件全量灌入 ONNX 触发算力雪崩。
		{
			gated := kept[:0]
			for _, cd := range kept {
				if cd.structDist <= matcher.TopologicalPruneSoftThreshold {
					gated = append(gated, cd)
				}
			}
			kept = gated
		}
		tGate += time.Since(tGate0)
		tSel0 := time.Now()
		sort.SliceStable(kept, func(i, j int) bool {
			if kept[i].structDist != kept[j].structDist {
				return kept[i].structDist < kept[j].structDist
			}
			if kept[i].textDist != kept[j].textDist {
				return kept[i].textDist < kept[j].textDist
			}
			return kept[i].n.NodeID < kept[j].n.NodeID
		})
		var t15 *foundNode
		if len(kept) > 0 {
			sel := kept[0]
			// 身份门禁：tag/role 与 oracle 不兼容 → 拒绝执行（miss），防 false-heal。
			if matcher.StructRoleCompatible(
				matcher.StructFeature{Tag: sel.n.Tag, Role: sel.n.Role}, oracleF) {
				t15 = &foundNode{NodeID: sel.n.NodeID, Text: sel.n.Text,
					Tag: sel.n.Tag, Role: sel.n.Role,
					TextDist: sel.textDist, StructDist: sel.structDist}
			}
		}
		tSelect += time.Since(tSel0)

		// node_id 降级：original BFS 序回查（隔离报告，不进综合）。
		var nd *foundNode
		for i := range c.MutatedPage {
			if c.MutatedPage[i].NodeID == c.OriginalNodeID {
				n := &c.MutatedPage[i]
				nd = &foundNode{NodeID: n.NodeID, Text: n.Text, Tag: n.Tag, Role: n.Role}
				break
			}
		}

		results = append(results, result{
			CaseID:       c.CaseID,
			Semantic:     c.Semantic,
			MutationType: c.Mutation.Type,
			Heuristic:    h,
			Tier15:       t15,
			NodeID:       nd,
			Candidates:   candList(kept),
			L1Pool:       poolCandList(pool),
		})
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "创建输出目录失败:", err)
		os.Exit(1)
	}
	data, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写输出失败:", err)
		os.Exit(1)
	}
	nCase := len(results)
	timingDoc := map[string]any{
		"note":  "离线合成语料 build/tier1/drift_set.json（SEED=42）单机墙钟；随机器负载波动，不计入字节可回放口径。",
		"cases": nCase,
		"stages": []stageTiming{
			{Stage: "l1_rule_match", TotalMs: msOf(tL1), PerCaseUs: usOf(tL1, nCase)},
			{Stage: "text_channel_hash", TotalMs: msOf(tText), PerCaseUs: usOf(tText, nCase)},
			{Stage: "struct_channel_hash", TotalMs: msOf(tStruct), PerCaseUs: usOf(tStruct, nCase)},
			{Stage: "gate_and_prune", TotalMs: msOf(tGate), PerCaseUs: usOf(tGate, nCase)},
			{Stage: "rank_and_select", TotalMs: msOf(tSelect), PerCaseUs: usOf(tSelect, nCase)},
		},
	}
	td, _ := json.MarshalIndent(timingDoc, "", "  ")
	timingPath := filepath.Join(filepath.Dir(*out), "stage_timing.json")
	if err := os.WriteFile(timingPath, td, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写分段计时失败:", err)
		os.Exit(1)
	}
	fmt.Printf("Tier1 runner 完成：%d case → %s（规则库 %d 条 L1；三轨：heuristic/tier15/nodeid）\n",
		len(results), *out, len(rules))
}

func msOf(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

func usOf(d time.Duration, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(d.Nanoseconds()) / 1e3 / float64(n)
}

// poolCandList 把 L1 全量候选池转成 JSON 输出（BFS 序，含双通道距离与 role 兼容位；
// 供 Python 侧离线复算任意阈值下的候选质量口径）。
func poolCandList(pool []cand) []poolCand {
	out := make([]poolCand, 0, len(pool))
	for _, cd := range pool {
		out = append(out, poolCand{
			NodeID:     cd.n.NodeID,
			Text:       cd.n.Text,
			Tag:        cd.n.Tag,
			Role:       cd.n.Role,
			TextDist:   cd.textDist,
			StructDist: cd.structDist,
			RoleCompat: cd.roleCompat,
		})
	}
	return out
}

// candList 把双通道保留候选转成 JSON 输出（供 Python 侧 ONNX 重排）。
func candList(kept []cand) []foundNode {
	out := make([]foundNode, 0, len(kept))
	for _, cd := range kept {
		out = append(out, foundNode{
			NodeID:     cd.n.NodeID,
			Text:       cd.n.Text,
			Tag:        cd.n.Tag,
			Role:       cd.n.Role,
			TextDist:   cd.textDist,
			StructDist: cd.structDist,
		})
	}
	return out
}
