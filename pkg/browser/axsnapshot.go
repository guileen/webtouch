// 页面内容语义树（AX Content Snapshot，v1.5.6）。
//
// 定位：现有 ExportPageTree 的「可交互 ref 树」只覆盖探针 kept 的交互节点，
// 正文/标题/列表等承载语义的内容会被丢掉——Google 结果页只剩 7 行 offscreen
// 控制节点，VLM 因此“看不见结果”。本文件实现业界同款通道：
// CDP Accessibility.getFullAXTree → 层级文本（角色 + 可访问名 + 关键状态），
// 与 ref 树一并输出（ref 行保留 [node_id]，内容行不带 ref，仅供阅读理解）。
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// axContentKeepRoles 是内容树保留的角色（结构/语义/交互三类都保留，
// 通用容器只在有可访问名时保留，避免裸 div 噪音）。
var axContentKeepRoles = map[string]bool{
	"heading": true, "paragraph": true, "text": true, "staticText": true,
	"link": true, "button": true, "textbox": true, "searchbox": true,
	"combobox": true, "checkbox": true, "radio": true, "switch": true,
	"slider": true, "listbox": true, "option": true, "menu": true,
	"menuitem": true, "tab": true, "tablist": true, "tree": true,
	"treeitem": true, "list": true, "listitem": true, "table": true,
	"row": true, "rowheader": true, "columnheader": true, "cell": true,
	"navigation": true, "banner": true, "main": true, "complementary": true,
	"contentinfo": true, "form": true, "search": true, "region": true,
	"article": true, "dialog": true, "alert": true, "status": true,
	"tooltip": true, "img": true, "figure": true,
}

// axNoTextKidsRoles：这些角色的可访问名已含子文本，staticText 子节点再展开会重复。
var axNoTextKidsRoles = map[string]bool{
	"heading": true, "link": true, "button": true, "combobox": true,
	"searchbox": true, "option": true, "menuitem": true, "tab": true,
	"listitem": true, "treeitem": true, "cell": true, "columnheader": true,
	"rowheader": true,
}

const axContentMaxLines = 900

type axValueObj struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type axPropRaw struct {
	Name  string     `json:"name"`
	Value axValueObj `json:"value"`
}

type axNodeRaw struct {
	NodeID           string      `json:"nodeId"`
	Ignored          bool        `json:"ignored"`
	Role             axValueObj  `json:"role"`
	Name             axValueObj  `json:"name"`
	Value            axValueObj  `json:"value"`
	Description      axValueObj  `json:"description"`
	Properties       []axPropRaw `json:"properties"`
	ParentID         string      `json:"parentId"`
	ChildIDs         []string    `json:"childIds"`
	BackendDOMNodeID int64       `json:"backendDOMNodeId"`
}

func axStr(v axValueObj) string {
	if len(v.Value) == 0 || string(v.Value) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(v.Value, &s); err == nil {
		return s
	}
	var f float64
	if err := json.Unmarshal(v.Value, &f); err == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	var b bool
	if err := json.Unmarshal(v.Value, &b); err == nil {
		if b {
			return "true"
		}
		return "false"
	}
	return string(v.Value)
}

func axProp(n axNodeRaw, name string) string {
	for _, p := range n.Properties {
		if p.Name == name {
			return axStr(p.Value)
		}
	}
	return ""
}

// axKeep 判断节点是否需要输出一行（内容语义或显式命名的容器）。
func axKeep(n axNodeRaw) bool {
	if n.Ignored {
		return false
	}
	role := axStr(n.Role)
	if axContentKeepRoles[role] {
		return true
	}
	return (role == "generic" || role == "group" || role == "section") &&
		strings.TrimSpace(axStr(n.Name)) != ""
}

func axSanitize(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > 260 {
		s = s[:260] + "…"
	}
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func axRoleLine(n axNodeRaw) string {
	role := axStr(n.Role)
	if role == "staticText" {
		role = "text"
	}
	name := axSanitize(axStr(n.Name))
	if name == "" && (role == "text" || role == "staticText") {
		return ""
	}
	attrs := make([]string, 0, 4)
	if role == "heading" {
		if lv := axProp(n, "level"); lv != "" {
			attrs = append(attrs, "level="+lv)
		}
	}
	// 链接必须带解析后 URL：同文「文章页链接 vs 页内 # 锚点」靠 href 区分
	// （2026-09-03 react.dev 复现：VLM 反复点错同名锚点导致 no-progress）。
	if role == "link" {
		if u := axSanitize(axProp(n, "url")); u != "" {
			attrs = append(attrs, "href="+u)
		}
	}
	if v := axSanitize(axStr(n.Value)); v != "" &&
		(role == "textbox" || role == "searchbox" || role == "combobox" ||
			role == "slider" || role == "progressbar" || role == "spinbutton") {
		attrs = append(attrs, "value="+v)
	}
	for _, st := range []string{"checked", "disabled", "expanded", "selected",
		"pressed", "required", "readonly", "autocomplete", "haspopup", "invalid"} {
		if v := axProp(n, st); v != "" && v != "false" {
			attrs = append(attrs, st+"="+v)
		}
	}
	if d := axSanitize(axStr(n.Description)); d != "" {
		attrs = append(attrs, "desc="+d)
	}
	if len(attrs) > 0 {
		return fmt.Sprintf("- %s %q [%s]", role, name, strings.Join(attrs, ", "))
	}
	return fmt.Sprintf("- %s %q", role, name)
}

// serializeAXContent 把 CDP AX 节点数组序列化为层级内容文本（仅阅读，无 ref）。
func serializeAXContent(nodes []axNodeRaw) string {
	byID := make(map[string]axNodeRaw, len(nodes))
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	var roots []axNodeRaw
	for _, n := range nodes {
		if n.ParentID == "" {
			roots = append(roots, n)
			continue
		}
		if _, ok := byID[n.ParentID]; !ok {
			roots = append(roots, n)
		}
	}
	rawOrder := make([]string, 0, len(nodes))
	for _, n := range nodes {
		rawOrder = append(rawOrder, n.NodeID)
	}
	// childIds 顺序优先；缺失时按输入序兜底。
	order := func(id string, seen map[string]bool) []string {
		n := byID[id]
		if len(n.ChildIDs) > 0 {
			out := make([]string, 0, len(n.ChildIDs))
			for _, c := range n.ChildIDs {
				if _, ok := byID[c]; ok && !seen[c] {
					out = append(out, c)
				}
			}
			return out
		}
		out := make([]string, 0, 4)
		for _, cand := range rawOrder {
			if byID[cand].ParentID == id && !seen[cand] {
				out = append(out, cand)
			}
		}
		return out
	}

	lines := make([]string, 0, 256)
	seen := make(map[string]bool)
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		if len(lines) >= axContentMaxLines {
			return
		}
		n, ok := byID[id]
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		role := axStr(n.Role)
		if axKeep(n) {
			// heading/link 等可访问名已含 staticText，避免重复输出正文行。
			if (role == "staticText" || role == "text") && axNoTextKidsRoles[axStr(byID[n.ParentID].Role)] {
				seen[id] = false
				return
			}
			line := axRoleLine(n)
			if line != "" {
				lines = append(lines, strings.Repeat("  ", depth)+line)
			}
			if len(lines) >= axContentMaxLines {
				return
			}
			for _, c := range order(id, seen) {
				walk(c, depth+1)
			}
			return
		}
		// 容器节点本身不输出，但保留其子孙语义行。
		for _, c := range order(id, seen) {
			walk(c, depth)
		}
	}
	for _, r := range roots {
		walk(r.NodeID, 0)
	}
	if len(lines) >= axContentMaxLines {
		lines = append(lines, fmt.Sprintf("…（内容树已到 %d 行上限，页面内容可能被截断）", axContentMaxLines))
	}
	return strings.Join(lines, "\n")
}

// exportAXContentTree 抓取完整 Accessibility 树并返回层级内容文本。
// 站点 AX 质量差/命令不可用时返回错误，由调用方决定降级（不阻塞 ref 树）。
func (s *cdpSession) exportAXContentTree(ctx context.Context) (string, error) {
	if err := s.client.CallSession(ctx, s.sessionID, "Accessibility.enable", map[string]any{}, nil); err != nil {
		return "", fmt.Errorf("ax enable: %w", err)
	}
	var res struct {
		Nodes []axNodeRaw `json:"nodes"`
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Accessibility.getFullAXTree", map[string]any{}, &res); err != nil {
		return "", fmt.Errorf("ax tree: %w", err)
	}
	body := serializeAXContent(res.Nodes)
	if body == "" {
		return "", fmt.Errorf("ax tree: 无可见语义节点（页面空白或 canvas/纯图形站点）")
	}
	return body, nil
}
