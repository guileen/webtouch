// 结构特征位图序列化（bit-packed Serialization，[State:D] RPC 通道硬约束）。
//
// 背景：Tier 1.5 向量① 的阻尼 DOM 拓扑哈希（ast_semantic_hash 结构通道）需要从
// WASM/CGO 边界流出到引擎 L3/ONNX 决策链。为避免用 JSON 或明文字符串跨边界传递
// （大页面下 De/Ser 会击穿 P99 < 15ms 探针通信上限），本文件定义固定宽度
// ≤64 Bytes 的位图载荷 PackStructFeature / UnpackStructFeature：
//   - 载荷只携带「结构签名」与「身份兼容码」，不携带原始字符串；
//   - tag/role/最近语义祖先 role 用固定词表索引（12-bit）编码，未知值落保留槽；
//   - 稳定属性（aria-*/data-*）用 40-bit 白名单位图压缩；
//   - 相对深度用 8-bit 编码（阻尼流要求相对深度而非绝对深度）。
//
// 约束（State:D）：序列化后的单节点特征载荷 ≤ 64 Bytes，且严禁以 JSON/明文字符串
// 传递；本文件即该契约的 Go 侧实现，C++ 侧镜像见 pkg/probe_wasm/src/struct_payload.h。
package matcher

import (
	"encoding/binary"
	"errors"
	"math"
)

// StructPayloadSize 是结构特征位图载荷的固定字节数（≤64，State:D 硬约束）。
const StructPayloadSize = 64

// StructPayloadVersion 标记位图布局版本（追加-only，未来可扩载荷区）。
const StructPayloadVersion = 1

// 词表内字段用 12-bit（0..4095）编码；0xFFF 表示「未知/缺省」。
const (
	structCodeNone = uint16(0x0FFF)
	structCodeMax  = uint16(0x0FFE) // 词表内最大可用索引
)

// tagVocab 是高频 HTML 标签词表（索引 = 词表下标；仅在枚举内可还原为字符串）。
var tagVocab = []string{
	"button", "a", "input", "select", "textarea", "div", "span", "ul", "li",
	"nav", "aside", "main", "header", "footer", "section", "article", "form",
	"label", "img", "h1", "h2", "h3", "p", "table", "td", "tr", "option",
	"menu", "dialog", "iframe",
}

// roleVocab 是高频 ARIA role 词表（索引 = 词表下标；仅在枚举内可还原为字符串）。
var roleVocab = []string{
	"button", "link", "navigation", "list", "listitem", "banner", "complementary",
	"main", "region", "form", "search", "tab", "tablist", "tabpanel", "checkbox",
	"radio", "menuitem", "combobox", "textbox", "heading", "status", "alert",
	"dialog", "grid", "cell", "row", "columnheader", "none", "generic",
}

// attrWhitelist 是稳定属性白名单（aria-*/data-*，与 StructFeature.Attrs 过滤同构）。
// 第 i 位对应 attrWhitelist[i]；40-bit 位图压缩到 5 字节。
var attrWhitelist = []string{
	"aria-label", "aria-labelledby", "aria-describedby", "aria-expanded",
	"aria-haspopup", "aria-controls", "aria-pressed", "aria-selected",
	"aria-checked", "aria-disabled", "aria-hidden", "aria-live",
	"data-testid", "data-role", "data-action", "data-id",
	"data-name", "data-value", "data-qa", "data-track",
	"placeholder", "title", "alt", "type", "name", "value",
	"for", "href", "src", "disabled", "readonly", "required",
}

// StructPayload 是解包后的结构载荷（供 L3/ONNX 决策链消费）。
type StructPayload struct {
	Hash          uint64   // 结构通道 SimHash（StructuralSimHash64）
	Tag           string   // 从词表还原的标签；未知为 ""
	Role          string   // 从词表还原的 role；未知为 ""
	AncestorRole  string   // 最近语义祖先 role；未知为 ""
	Attrs         []string // 白名单命中的稳定属性（还原自位图）
	RelativeDepth uint8    // 相对深度（阻尼流）
	HasIdentity   bool     // tag/role 是否可还原（用于身份兼容判断）
}

// PackStructFeature 把 StructFeature 编码为 ≤64 Bytes 的位图载荷。
// 载荷自身是纯二进制（无 JSON/明文字符串），跨 WASM/CGO 边界按本布局拷贝。
func PackStructFeature(f StructFeature) [StructPayloadSize]byte {
	var buf [StructPayloadSize]byte
	buf[0] = StructPayloadVersion
	flags := uint8(0)
	if f.Tag != "" {
		flags |= 0x01
	}
	if f.Role != "" {
		flags |= 0x02
	}
	if f.Ancestors != nil {
		flags |= 0x04
	}

	// 结构签名：用完整 StructFeature 计算阻尼哈希（保留文本无关的结构信号）。
	hash := StructuralSimHash64(f)
	binary.LittleEndian.PutUint64(buf[2:10], hash)

	tagCode, tagKnown := vocabCode(f.Tag, tagVocab)
	roleCode, roleKnown := vocabCode(f.Role, roleVocab)
	binary.LittleEndian.PutUint16(buf[10:12], tagCode)
	binary.LittleEndian.PutUint16(buf[12:14], roleCode)

	// 最近语义祖先 role（跳过 generic/none 幽灵包装）。
	anc := ""
	hasAnc := false
	for _, a := range f.Ancestors {
		if a.Role == "" || a.Role == "generic" || a.Role == "none" {
			continue
		}
		anc = a.Role
		hasAnc = true
		break
	}
	if hasAnc {
		flags |= 0x08
	}
	ancCode, _ := vocabCode(anc, roleVocab)
	binary.LittleEndian.PutUint16(buf[14:16], ancCode)

	// 稳定属性白名单位图（仅 aria-*/data-*；与 StructFeature 过滤一致）。
	attrBits := uint64(0)
	for _, a := range f.Attrs {
		if i := whitelistIndex(a); i >= 0 {
			attrBits |= 1 << uint(i)
		}
	}
	binary.LittleEndian.PutUint64(buf[16:24], attrBits)

	// 相对深度：取祖先链长度，封顶 255（阻尼流不依赖绝对深度）。
	depth := len(f.Ancestors)
	if depth > math.MaxUint8 {
		depth = math.MaxUint8
	}
	buf[24] = uint8(depth)

	if tagKnown && roleKnown && (f.Tag != "" || f.Role != "") {
		flags |= 0x10 // HasIdentity
	}
	buf[1] = flags
	return buf
}

// UnpackStructFeature 解码 PackStructFeature 的位图载荷。长度必须 == StructPayloadSize。
func UnpackStructFeature(buf []byte) (StructPayload, error) {
	if len(buf) < StructPayloadSize {
		return StructPayload{}, errors.New("structcodec: short payload")
	}
	flags := buf[1]
	hash := binary.LittleEndian.Uint64(buf[2:10])
	tagCode := binary.LittleEndian.Uint16(buf[10:12])
	roleCode := binary.LittleEndian.Uint16(buf[12:14])
	ancCode := binary.LittleEndian.Uint16(buf[14:16])
	attrBits := binary.LittleEndian.Uint64(buf[16:24])
	depth := buf[24]

	p := StructPayload{
		Hash:          hash,
		Attrs:         attrBitsOut(attrBits),
		RelativeDepth: depth,
	}
	_ = flags
	p.Tag = vocabString(tagCode, tagVocab)
	p.Role = vocabString(roleCode, roleVocab)
	p.AncestorRole = vocabString(ancCode, roleVocab)
	p.HasIdentity = flags&0x10 != 0 && (p.Tag != "" || p.Role != "")
	return p, nil
}

// StructPayloadFromFeature 是 PackStructFeature 的便捷别名（供探针/引擎调用）。
func StructPayloadFromFeature(f StructFeature) [StructPayloadSize]byte {
	return PackStructFeature(f)
}

func vocabCode(s string, vocab []string) (uint16, bool) {
	if s == "" {
		return structCodeNone, false
	}
	for i, v := range vocab {
		if v == s {
			return uint16(i), true
		}
	}
	return structCodeNone, false
}

func vocabString(code uint16, vocab []string) string {
	if code >= structCodeMax {
		return ""
	}
	if int(code) >= len(vocab) {
		return ""
	}
	return vocab[code]
}

func whitelistIndex(attr string) int {
	for i, w := range attrWhitelist {
		if attr == w {
			return i
		}
	}
	return -1
}

func attrBitsOut(bits uint64) []string {
	var out []string
	for i, w := range attrWhitelist {
		if bits&(1<<uint(i)) != 0 {
			out = append(out, w)
		}
	}
	return out
}
