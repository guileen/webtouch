// DOM 拓扑结构哈希（ast_semantic_hash 的阻尼结构通道，Tier 1.5 向量 1）。
//
// 背景（research-零点线-三盲区实证 §2 + Phase 0 Tier 1 基准 §5）：当前 L3 用
// SimHash64(候选文本) 作单一特征通道，rename（文本漂移）时把「立即登录」vs「登录」
// 判结构正交（Hamming=28 > D_h=5）→ 纯启发式 rename 恢复率 0%。
// 修复：注入带深度衰减阻尼的 DOM 拓扑哈希流（实验一验证的 V2 阻尼设计）——
//   - 相对深度（自身 tag/role，无绝对深度）；
//   - 祖先语境只取最近「语义祖先」role（跳过 generic/none 幽灵包装）；
//   - 属性白名单仅 aria-*/data-*（排除 class/id/绝对位置）；
//   - 不含文本 token（结构通道专司结构，文本由文本通道 / ONNX 负责）。
//
// 文本正交（rename）时结构证据仍可确认「同一按钮位置」，填补结构证据缺口。
package matcher

import (
	"strings"
)

// StructAncestor 是目标节点的祖先（父→祖方向）。
type StructAncestor struct {
	Tag  string
	Role string
}

// StructFeature 是结构通道的输入（无文本/class/绝对深度）。
type StructFeature struct {
	Tag       string
	Role      string
	Attrs     []string         // 稳定属性名（aria-*/data-*）
	Ancestors []StructAncestor // 父→祖
}

// StructuralPhrases 生成阻尼结构特征短语流（供 SimHash64 逐位对齐）。
func StructuralPhrases(f StructFeature) []string {
	ph := make([]string, 0, 4+len(f.Attrs))
	if f.Tag != "" {
		ph = append(ph, "tag:"+f.Tag)
	}
	if f.Role != "" {
		ph = append(ph, "role:"+f.Role)
	}
	for _, a := range f.Attrs {
		if strings.HasPrefix(a, "aria-") || strings.HasPrefix(a, "data-") {
			ph = append(ph, "attr:"+a)
		}
	}
	// 拓扑阻尼：跳过无语义幽灵包装，只取最近语义祖先的 role。
	for _, anc := range f.Ancestors {
		if anc.Role == "" || anc.Role == "generic" || anc.Role == "none" {
			continue
		}
		ph = append(ph, "sem-anc-role:"+anc.Role)
		break
	}
	return ph
}

// StructuralSimHash64 返回结构通道的 64-bit SimHash（与文本通道同构、可比较 Hamming）。
func StructuralSimHash64(f StructFeature) uint64 {
	return SimHash64(strings.Join(StructuralPhrases(f), " "))
}

// StructRoleCompatible 是 Malhotra compatible tag/role：tag 相同或 role 相同。
func StructRoleCompatible(got StructFeature, oracle StructFeature) bool {
	if oracle.Tag == "" && oracle.Role == "" {
		return true // 无身份约束时不拦截
	}
	if got.Tag != "" && oracle.Tag != "" && got.Tag == oracle.Tag {
		return true
	}
	if got.Role != "" && oracle.Role != "" && got.Role == oracle.Role {
		return true
	}
	return false
}
