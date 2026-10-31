// L1 文本粗筛：Aho-Corasick 自动机（spec/algorithm-kernel.md §5）。
// 规则文本命中节点即入围候选池（TDD-MATCH-01）。
package matcher

// acNode 是自动机节点：子节点 + 失配指针 + 以该节点结尾的规则输出。
type acNode struct {
	children map[rune]*acNode
	fail     *acNode
	output   []Rule
}

// ACAutomaton 是编译完成后的多模式串匹配器。
type ACAutomaton struct {
	root *acNode
}

// NewACAutomaton 依据规则构建自动机（空 text_pattern 自动跳过）。
func NewACAutomaton(patterns []Rule) *ACAutomaton {
	root := &acNode{children: make(map[rune]*acNode)}
	for _, r := range patterns {
		if r.TextPattern == "" {
			continue
		}
		n := root
		for _, ch := range r.TextPattern {
			child, ok := n.children[ch]
			if !ok {
				child = &acNode{children: make(map[rune]*acNode)}
				n.children[ch] = child
			}
			n = child
		}
		n.output = append(n.output, r)
	}

	// BFS 构建失配指针；根的直接子节点 fail=root。
	queue := make([]*acNode, 0)
	for _, child := range root.children {
		child.fail = root
		queue = append(queue, child)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for ch, child := range cur.children {
			queue = append(queue, child)
			f := cur.fail
			for f != nil && f.children[ch] == nil {
				f = f.fail
			}
			if f == nil {
				child.fail = root
			} else {
				child.fail = f.children[ch]
			}
			// 继承失配路径上的输出（保证多模式全部命中）。
			if len(child.fail.output) > 0 {
				child.output = append(child.output, child.fail.output...)
			}
		}
	}
	return &ACAutomaton{root: root}
}

// Match 扫描文本，返回所有命中的规则（可能重复，调用方去重）。
func (a *ACAutomaton) Match(text string) []Rule {
	var out []Rule
	n := a.root
	for _, ch := range text {
		for n != a.root && n.children[ch] == nil {
			n = n.fail
		}
		if next, ok := n.children[ch]; ok {
			n = next
		}
		out = append(out, n.output...)
	}
	return out
}
