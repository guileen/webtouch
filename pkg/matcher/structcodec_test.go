// TDD-PROBE-06（Tier 1.5 接线）：结构特征位图序列化契约。
//
// [State:D] RPC 通道硬约束：StructuralFeature 特征流严禁以 JSON 或明文字符串
// 跨 WASM/CGO 边界传递；序列化后的单节点特征载荷必须 ≤ 64 Bytes。
// 本测试验证 PackStructFeature 产出纯二进制、长度恒 ≤64、无 JSON/字符串编码，
// 且 UnpackStructFeature 还原结构签名与身份码用于 L3 双通道。
package matcher

import (
	"bytes"
	"strings"
	"testing"
)

func TestStructPayloadSize(t *testing.T) {
	if StructPayloadSize > 64 {
		t.Fatalf("StructPayloadSize 必须 ≤ 64B（State:D 硬约束），实际 %d", StructPayloadSize)
	}
	f := StructFeature{
		Tag:   "button",
		Role:  "button",
		Attrs: []string{"aria-label", "data-testid", "class", "id"},
		Ancestors: []StructAncestor{
			{Tag: "div", Role: "generic"},
			{Tag: "nav", Role: "navigation"},
		},
	}
	buf := PackStructFeature(f)
	if len(buf) != StructPayloadSize {
		t.Fatalf("载荷长度应为 %d，实际 %d", StructPayloadSize, len(buf))
	}
	// 纯二进制：载荷不得是 UTF-8 可打印字符串（无 JSON/明文字符串跨边界）。
	if s := string(buf[:]); strings.ContainsFunc(s, func(r rune) bool { return r == 0x7b || r == 0x22 }) {
		t.Fatalf("载荷含 JSON 字符（左花括号或双引号），违反「禁止 JSON/明文字符串」约束")
	}
}

func TestStructPayloadRoundTrip(t *testing.T) {
	f := StructFeature{
		Tag:   "button",
		Role:  "button",
		Attrs: []string{"aria-label", "data-testid"},
		Ancestors: []StructAncestor{
			{Tag: "div", Role: "generic"},
			{Tag: "nav", Role: "navigation"},
		},
	}
	buf := PackStructFeature(f)
	p, err := UnpackStructFeature(buf[:])
	if err != nil {
		t.Fatalf("UnpackStructFeature: %v", err)
	}
	if p.Hash != StructuralSimHash64(f) {
		t.Fatalf("结构签名不一致：%016x != %016x", p.Hash, StructuralSimHash64(f))
	}
	if p.Tag != "button" || p.Role != "button" {
		t.Fatalf("身份码还原失败：tag=%q role=%q", p.Tag, p.Role)
	}
	if p.AncestorRole != "navigation" {
		t.Fatalf("最近语义祖先 role 还原失败：%q", p.AncestorRole)
	}
	if !p.HasIdentity {
		t.Fatalf("身份标志应置位（tag+role 均在词表内）")
	}
	if len(p.Attrs) != 2 {
		t.Fatalf("白名单属性还原应命中 2 项，实际 %v", p.Attrs)
	}
	if p.RelativeDepth != 2 {
		t.Fatalf("相对深度应为 2，实际 %d", p.RelativeDepth)
	}
}

func TestStructPayloadUnknownIdentity(t *testing.T) {
	f := StructFeature{
		Tag:  "custom-x",
		Role: "mystery-role",
	}
	buf := PackStructFeature(f)
	p, err := UnpackStructFeature(buf[:])
	if err != nil {
		t.Fatalf("UnpackStructFeature: %v", err)
	}
	// 未知 tag/role 落保留槽 → 还原为空、HasIdentity=false（身份不约束）。
	if p.Tag != "" || p.Role != "" {
		t.Fatalf("未知词表项应还原为空 tag=%q role=%q", p.Tag, p.Role)
	}
	if p.HasIdentity {
		t.Fatalf("未知身份不应置位 HasIdentity")
	}
	if p.Hash != StructuralSimHash64(f) {
		t.Fatalf("结构签名应保留：%016x", p.Hash)
	}
}

func TestStructPayloadIsBinaryOnly(t *testing.T) {
	// 载荷只含固定宽度二进制字段，无长度前缀字符串（= 非 JSON/文本流）。
	var f StructFeature
	for i := 0; i < 50; i++ {
		f.Attrs = append(f.Attrs, "aria-label")
	}
	f.Tag = "button"
	buf := PackStructFeature(f)
	if len(buf) != StructPayloadSize {
		t.Fatalf("载荷长度必须恒定 %d，实际 %d", StructPayloadSize, len(buf))
	}
	if bytes.Count(buf[:], []byte("aria-label")) > 0 {
		t.Fatalf("位图载荷不得内嵌明文字符串（aria-label）")
	}
}

func TestStructPayloadDeterministic(t *testing.T) {
	f := StructFeature{Tag: "a", Role: "link", Attrs: []string{"href"}}
	if PackStructFeature(f) != PackStructFeature(f) {
		t.Fatalf("打包应确定性：同输入同输出")
	}
}
