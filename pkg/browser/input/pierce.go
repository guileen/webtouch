package input

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
)

// 穿透寻底（spec/algorithm-kernel.md §5.0.10 条款 1；WebTouch v2.0 RFC 推演 2 / 手术 3）。
//
// 复杂度治理·阶段 2·单元 2c（2026-09-29）：整组装（`pierce_hit` + `click_recovery`）自宿主
// `pkg/browser` 迁入本包；宿主能力经 `Session` 注入、派发标记面用 `DispatchTracker`
// （见 `session.go`），页内探针脚本与 AX 取值最小解码在 `pierce_probe.go`。
//
// 物理事实：现代页面普遍在真实受体之上铺「透明手势代理层」（`opacity:0` /
// `aria-hidden` / `pointer-events:none`），裸 `document.elementFromPoint(x,y)` 只回最外层
// 宿主或顶层透明层；而封闭 Shadow Root（`mode:"closed"`）在**任何** Execution World
// （含 Isolated World）下 `shadowRoot` 恒为 `null` —— 页内纯 JS 无法穿透。
//
// 因此本文件按双层分工实现：
//  1. 页内 JS（`hitPiercingFnJS` / `inputStemPiercingJS`）只做**几何层叠判定**：
//     `elementsFromPoint` 栈自顶向下剔除透明手势代理层；组合祖先回溯用
//     `getRootNode()` + `host`（`composedPath()` 是 Event 的方法，Element 上不存在）。
//  2. 宿主层（`PierceHitAt` / `InputAtPierced`）承担封闭 Shadow DOM 的穿透与物理击发：
//     AX 局部切片（可编辑角色 + 几何包含，天然穿 closed、天然剥离隐藏数据载体）优先，
//     `DOM.getNodeForLocation(ignorePointerEventsNone=true)` 兜底；聚焦/写值/回读走
//     `DOM.resolveNode` + `Runtime.callFunctionOn`（CDP 不受 closed 封装限制）。

// errPierceNoNode 表示宿主层也没找到可用受体（页内与宿主层双空）。
var errPierceNoNode = errors.New("cdp: pierced 未命中任何受体节点")

// ── 宿主层穿透（§5.0.10 条款 1 第二款）─────────────────────────────

// PiercedHit 是宿主层穿透得到的真实受体（页内 JS 不可见时的唯一权威画像）。
type PiercedHit struct {
	BackendNodeID int64
	ObjectID      string
	Role          string
	AXName        string
	AXEditable    bool
	NodeName      string
	Attrs         map[string]string
	X, Y, W, H    float64
}

// Tag 返回小写标签名（DOM.describeNode 的 nodeName 是大写）。
func (h *PiercedHit) Tag() string { return strings.ToLower(h.NodeName) }

var piercedNonTextInputTypes = map[string]bool{
	"hidden": true, "button": true, "submit": true, "reset": true, "image": true,
	"checkbox": true, "radio": true, "file": true, "range": true, "color": true,
}

// editable 判定受体是否可编辑（与 §5.0.3 条款 1 的页内判据同口径；AX 的 editable 属性优先）。
func (h *PiercedHit) editable() bool {
	if h.AXEditable {
		return true
	}
	if _, bad := h.Attrs["disabled"]; bad {
		return false
	}
	if _, bad := h.Attrs["readonly"]; bad {
		return false
	}
	switch h.Tag() {
	case "textarea", "select":
		return true
	case "input":
		t := strings.ToLower(h.Attrs["type"])
		if t == "" {
			t = "text"
		}
		return !piercedNonTextInputTypes[t]
	}
	if _, ce := h.Attrs["contenteditable"]; ce {
		return true
	}
	return h.Role == "textbox" || h.Role == "searchbox"
}

// receptorProfile 用宿主层画像构造受体画像（§5.0.9 条款 1 的守卫输入）。
// AX 可访问名在封闭 Shadow DOM 内是页内 JS 读不到的补充证据（aria-label/placeholder/label 派生）。
func (h *PiercedHit) receptorProfile() ReceptorProfile {
	aria := h.Attrs["aria-label"]
	if aria == "" {
		aria = h.AXName
	}
	return ReceptorProfile{
		Found:        true,
		Tag:          h.Tag(),
		Type:         strings.ToLower(h.Attrs["type"]),
		Name:         h.Attrs["name"],
		ID:           h.Attrs["id"],
		Placeholder:  h.Attrs["placeholder"],
		Aria:         aria,
		Autocomplete: strings.ToLower(h.Attrs["autocomplete"]),
	}
}

// describe 给出简短可归因证据串（页内 CSS 路径在封闭根内不可见，故走结构化描述）。
func (h *PiercedHit) describe() string {
	var b strings.Builder
	b.WriteString("pierced:" + h.Tag())
	if id := h.Attrs["id"]; id != "" {
		b.WriteString("#" + id)
	}
	if h.Role != "" {
		b.WriteString("(role=" + h.Role + ")")
	}
	return b.String()
}

// boxQuad 是节点的视口矩形（由 DOM.getBoxModel 的 content quad 归一）。
type boxQuad struct {
	X, Y, W, H float64
}

func (b boxQuad) contains(x, y float64) bool {
	return x >= b.X && x <= b.X+b.W && y >= b.Y && y <= b.Y+b.H
}

func (b boxQuad) area() float64 { return b.W * b.H }

// nodeBoxModel 取节点的内容盒矩形（不可渲染节点返回错误，由调用方跳过）。
func nodeBoxModel(ctx context.Context, s Session, backendNodeID int64) (*boxQuad, error) {
	var res struct {
		Model struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	if err := s.Call(ctx, "DOM.getBoxModel",
		map[string]any{"backendNodeId": backendNodeID}, &res); err != nil {
		return nil, err
	}
	q := res.Model.Content
	if len(q) < 8 {
		return nil, fmt.Errorf("cdp: box model 缺 content quad（%d 个分量）", len(q))
	}
	minX, minY := q[0], q[1]
	maxX, maxY := q[0], q[1]
	for i := 0; i+1 < 8; i += 2 {
		if q[i] < minX {
			minX = q[i]
		}
		if q[i] > maxX {
			maxX = q[i]
		}
		if q[i+1] < minY {
			minY = q[i+1]
		}
		if q[i+1] > maxY {
			maxY = q[i+1]
		}
	}
	return &boxQuad{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}, nil
}

// axEditableRole 判定 AX 角色是否属于「可接收文本输入」的受体角色。
func axEditableRole(role string) bool {
	return role == "textbox" || role == "searchbox"
}

// axEditableProp 判定 AX `editable` 属性值是否表示可编辑（§5.0.10 条款 1 的 AX 局部切片判据）。
// 真机实测（Chrome 153，2026-09-25 E2E 自证）：该属性取值形态是 `plaintext` / `richtext`
// （可编辑）或缺失 / `false`（不可编辑）——**不是**布尔字符串 `true`；按「非空且非 false」
// 判定，否则 contenteditable 一类 role 不落在 textbox/searchbox 的受体永远进不了切片。
func axEditableProp(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "false", "none":
		return false
	}
	return true
}

// axCandidate 是 AX 切片里的一个候选（纯选择逻辑的输入，便于离线回归）。
type axCandidate struct {
	hit    *PiercedHit
	isRole bool // role 落在可编辑角色（textbox/searchbox）
	area   float64
}

// pickAXPierceCandidate 按 §5.0.10 条款 1 第二款选受体：**可编辑角色优先**，无角色命中时
// 才退到 `editable` 属性；两趟各自取面积最小（＝最内层）。
//
// 真机实测（2026-09-25 E2E）：`input[type=search]` 的 **UA shadow 内部容器**是一个
// `role=generic` + `editable="plaintext"` 的 div，且内容盒比 input 本身更小——只按「面积最小」
// 会让它压过真实受体，focus/insertText 落到内建 div 上（回读为空 = 假失败）。角色优先把
// 「作者写的可编辑控件」与「浏览器内建渲染细节」区分开。
func pickAXPierceCandidate(cands []axCandidate) *PiercedHit {
	var roleBest, editableBest *axCandidate
	for i := range cands {
		c := &cands[i]
		if c.isRole && (roleBest == nil || c.area < roleBest.area) {
			roleBest = c
		}
		if !c.isRole && (editableBest == nil || c.area < editableBest.area) {
			editableBest = c
		}
	}
	if roleBest != nil {
		return roleBest.hit
	}
	if editableBest != nil {
		return editableBest.hit
	}
	return nil
}

// describePierced 补全宿主导出的标签/属性/对象句柄。
func describePierced(ctx context.Context, s Session, h *PiercedHit) error {
	var desc struct {
		Node struct {
			NodeName   string   `json:"nodeName"`
			Attributes []string `json:"attributes"`
		} `json:"node"`
	}
	if err := s.Call(ctx, "DOM.describeNode",
		map[string]any{"backendNodeId": h.BackendNodeID, "depth": 0}, &desc); err != nil {
		return fmt.Errorf("pierce_describe:%w", err)
	}
	h.NodeName = desc.Node.NodeName
	h.Attrs = make(map[string]string, len(desc.Node.Attributes)/2)
	for i := 0; i+1 < len(desc.Node.Attributes); i += 2 {
		h.Attrs[strings.ToLower(desc.Node.Attributes[i])] = desc.Node.Attributes[i+1]
	}
	var rn struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := s.Call(ctx, "DOM.resolveNode",
		map[string]any{"backendNodeId": h.BackendNodeID}, &rn); err == nil {
		h.ObjectID = rn.Object.ObjectID
	}
	return nil
}

// pierceViaAX 以 AX 局部切片求真实受体：可编辑角色 + 几何包含（取面积最小＝最内层）。
// AX 树在 Blink C++ 层天然穿透封闭 Shadow DOM，且天然排除隐藏/不可交互载体，
// 且不依赖页内 JS ⇒ 严格 CSP 站点同样可用。
func PierceViaAX(ctx context.Context, s Session, x, y float64) (*PiercedHit, error) {
	if err := s.Call(ctx, "Accessibility.enable", map[string]any{}, nil); err != nil {
		return nil, fmt.Errorf("pierce_ax_enable:%w", err)
	}
	var res struct {
		Nodes []axEditableNode `json:"nodes"`
	}
	if err := s.Call(ctx, "Accessibility.getFullAXTree", map[string]any{}, &res); err != nil {
		return nil, fmt.Errorf("pierce_ax_tree:%w", err)
	}
	cands := make([]axCandidate, 0, 8)
	for i := range res.Nodes {
		n := res.Nodes[i]
		if n.Ignored || n.BackendDOMNodeID == 0 {
			continue
		}
		role := n.Role.str()
		isRole := axEditableRole(role)
		editable := axEditableProp(n.prop("editable"))
		if !isRole && !editable {
			continue
		}
		box, err := nodeBoxModel(ctx, s, n.BackendDOMNodeID)
		if err != nil || box == nil || box.W <= 0 || box.H <= 0 {
			continue
		}
		if !box.contains(x, y) {
			continue
		}
		cands = append(cands, axCandidate{
			hit: &PiercedHit{
				BackendNodeID: n.BackendDOMNodeID,
				Role:          role,
				AXName:        n.Name.str(),
				AXEditable:    editable,
				X:             box.X, Y: box.Y, W: box.W, H: box.H,
			},
			isRole: isRole,
			area:   box.area(),
		})
	}
	best := pickAXPierceCandidate(cands)
	if best == nil {
		return nil, errPierceNoNode
	}
	if err := describePierced(ctx, s, best); err != nil {
		return nil, err
	}
	return best, nil
}

// pierceViaNodeForLocation 以 DOM 命中测试兜底（`ignorePointerEventsNone=true` 让
// `pointer-events:none` 遮罩直接穿透；closed shadow root 内的真实节点由 Blink 命中测试返回）。
func PierceViaNodeForLocation(ctx context.Context, s Session, x, y float64) (*PiercedHit, error) {
	var loc struct {
		BackendNodeID int64  `json:"backendNodeId"`
		FrameID       string `json:"frameId"`
	}
	params := map[string]any{
		"x":                         x,
		"y":                         y,
		"includeUserAgentShadowDOM": false,
		"ignorePointerEventsNone":   true,
	}
	if err := s.Call(ctx, "DOM.getNodeForLocation", params, &loc); err != nil {
		return nil, fmt.Errorf("pierce_get_node_for_location:%w", err)
	}
	if loc.BackendNodeID == 0 {
		return nil, errPierceNoNode
	}
	h := &PiercedHit{BackendNodeID: loc.BackendNodeID}
	if err := describePierced(ctx, s, h); err != nil {
		return nil, err
	}
	if box, err := nodeBoxModel(ctx, s, loc.BackendNodeID); err == nil && box != nil {
		h.X, h.Y, h.W, h.H = box.X, box.Y, box.W, box.H
	}
	return h, nil
}

// pierceHitAt 是宿主层穿透寻底的入口：AX 切片优先，DOM 命中测试兜底。
func PierceHitAt(ctx context.Context, s Session, x, y float64) (*PiercedHit, error) {
	hit, axErr := PierceViaAX(ctx, s, x, y)
	if axErr == nil && hit != nil {
		return hit, nil
	}
	fallback, domErr := PierceViaNodeForLocation(ctx, s, x, y)
	if domErr == nil && fallback != nil {
		if axErr != nil {
			log.Printf("uws pierce: AX 切片未命中（%v），改用 DOM 命中测试 backend=%d tag=%s",
				axErr, fallback.BackendNodeID, fallback.Tag())
		}
		return fallback, nil
	}
	if axErr == nil {
		axErr = errPierceNoNode
	}
	return nil, fmt.Errorf("%v; dom 兜底: %v", axErr, domErr)
}

// ── 宿主层物理击发与落地回读（§5.0.10 条款 1/3）────────────────────

// piercedFocusFnJS / piercedReadValueFnJS / piercedChangeFnJS 在**节点对象**上执行
// （Runtime.callFunctionOn 的 this 绑定），CDP 不受 closed shadow root 封装限制。
const piercedFocusFnJS = `function () {
  try { this.focus(); } catch (e) {}
  try { if (typeof this.select === 'function') this.select(); } catch (e) {}
  return true;
}`

const piercedReadValueFnJS = `function () {
  const tag = (this.tagName || '').toLowerCase();
  let v = '';
  try { v = (typeof this.value === 'string') ? this.value : ''; } catch (e) { v = ''; }
  if (!v) { try { v = this.innerText || this.textContent || ''; } catch (e) {} }
  return v || '';
}`

const piercedChangeFnJS = `function () {
  try { this.dispatchEvent(new Event('change', {bubbles: true})); } catch (e) {}
  return true;
}`

// callOnPierced 在穿透节点对象上执行函数（失败原样透传，不吞异常）。
func CallOnPierced(ctx context.Context, s Session, objectID, fn string) (json.RawMessage, error) {
	if objectID == "" {
		return nil, fmt.Errorf("cdp: pierced 节点缺对象句柄")
	}
	var raw struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := s.Call(ctx, "Runtime.callFunctionOn", map[string]any{
		"objectId":            objectID,
		"functionDeclaration": fn,
		"returnByValue":       true,
		"awaitPromise":        true,
	}, &raw); err != nil {
		return nil, err
	}
	if len(raw.ExceptionDetails) > 0 {
		return nil, fmt.Errorf("cdp: callFunctionOn 异常: %s", string(raw.ExceptionDetails))
	}
	return raw.Result.Value, nil
}

// piercedValue 回读穿透节点的当前值（§5.0.10 条款 3 的落地判据输入）。
func PiercedValue(ctx context.Context, s Session, objectID string) (string, error) {
	raw, err := CallOnPierced(ctx, s, objectID, piercedReadValueFnJS)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw), nil
	}
	return v, nil
}

// inputAtPierced 在宿主层完成一次「穿透寻底 → 守卫 → 聚焦写值 → 落地回读」。
// 该出口与页内出口共用同一个 §5.0.9 条款 1 守卫判定（GuardReceptorProfile）。
func InputAtPierced(ctx context.Context, s Session, disp *DispatchTracker, x, y int, value, semantic string) (string, bool, string) {
	fx, fy := float64(x), float64(y)
	hit, err := PierceHitAt(ctx, s, fx, fy)
	if err != nil || hit == nil {
		return "", false, "input_at:no_input"
	}
	if blocked, detail := GuardReceptorProfile(hit.receptorProfile(), semantic, value); blocked {
		log.Printf("uws input guard (pierced) semantic=%q blocked receptor=%q (TARGET_ROLE_MISMATCH)",
			semantic, detail)
		return "", false, fmt.Sprintf("%s:%s", FaultTargetRoleMismatch, detail)
	}
	if !hit.editable() {
		return "", false, fmt.Sprintf("input_not_editable:%s", hit.Tag())
	}
	if disp != nil {
		disp.Mark() // 物理事件从这里开始发出，之后中断只能报「结果未知」
	}
	if _, err := CallOnPierced(ctx, s, hit.ObjectID, piercedFocusFnJS); err != nil {
		return "", false, "input_failed:" + err.Error()
	}
	if err := s.InsertText(ctx, value); err != nil {
		return "", false, "input_failed:" + err.Error()
	}
	_, _ = CallOnPierced(ctx, s, hit.ObjectID, piercedChangeFnJS)
	got, rerr := PiercedValue(ctx, s, hit.ObjectID)
	if rerr != nil {
		log.Printf("uws input land read failed (pierced) semantic=%q tag=%s err=%v", semantic, hit.Tag(), rerr)
		return "", false, FaultVLMlandingUnverified + ":回读失败"
	}
	if !InputValueLanded(got, value) {
		log.Printf("uws input land check failed (pierced) semantic=%q tag=%s want=%q got=%q",
			semantic, hit.Tag(), value, got)
		return "", false, FaultVLMlandingUnverified + ":回读值未包含输入值"
	}
	sel := hit.describe()
	s.RecordHit(ElementTarget{Found: true, X: fx, Y: fy, Sel: sel, Tag: hit.Tag()})
	return sel, true, fmt.Sprintf("input_at:%d,%d|pierced:%s", x, y, hit.describe())
}
