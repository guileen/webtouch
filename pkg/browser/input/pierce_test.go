package input

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 宿主层受体画像的可编辑判定必须与页内判据同口径（§5.0.3 条款 1 / §5.0.10 条款 1）。
func TestPiercedHitEditable(t *testing.T) {
	cases := []struct {
		name string
		hit  PiercedHit
		want bool
	}{
		{"文本 input", PiercedHit{NodeName: "INPUT"}, true},
		{"email input（可编辑，角色守卫另判）", PiercedHit{NodeName: "INPUT", Attrs: map[string]string{"type": "email"}}, true},
		{"hidden input", PiercedHit{NodeName: "INPUT", Attrs: map[string]string{"type": "hidden"}}, false},
		{"checkbox", PiercedHit{NodeName: "INPUT", Attrs: map[string]string{"type": "checkbox"}}, false},
		{"disabled", PiercedHit{NodeName: "INPUT", Attrs: map[string]string{"disabled": ""}}, false},
		{"readonly", PiercedHit{NodeName: "INPUT", Attrs: map[string]string{"readonly": ""}}, false},
		{"textarea", PiercedHit{NodeName: "TEXTAREA"}, true},
		{"select", PiercedHit{NodeName: "SELECT"}, true},
		{"contenteditable div", PiercedHit{NodeName: "DIV", Attrs: map[string]string{"contenteditable": "true"}}, true},
		{"role=textbox 的 div", PiercedHit{NodeName: "DIV", Role: "textbox"}, true},
		{"纯 div（装饰/遮罩）", PiercedHit{NodeName: "DIV"}, false},
		{"AX 标了 editable 的非标准载体", PiercedHit{NodeName: "DIV", AXEditable: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.hit.editable(); got != tc.want {
				t.Fatalf("editable() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// 封闭 Shadow DOM 内的受体：页内 JS 读不到 aria-label，必须用 AX 可访问名补齐画像，
// 并让同一个 §5.0.9 条款 1 守卫照样熔断越权（拦截率 100% 不因出口新增而下降）。
func TestPiercedHitReceptorProfileUsesAXName(t *testing.T) {
	hit := PiercedHit{
		NodeName: "INPUT",
		Role:     "textbox",
		AXName:   "注册邮箱",
		Attrs:    map[string]string{"id": "hero_user_email", "type": "email"},
	}
	p := hit.receptorProfile()
	if p.Aria != "注册邮箱" {
		t.Fatalf("AX 可访问名必须补进画像 aria 位，got %q", p.Aria)
	}
	if !p.IsSensitiveReceptor() {
		t.Fatalf("邮箱受体必须判为敏感受体")
	}
	if blocked, detail := GuardReceptorProfile(p, "search_box", "webtouch"); !blocked {
		t.Fatalf("非凭据意图打到邮箱受体必须熔断，当前未熔断（detail=%q）", detail)
	}
	if blocked, _ := GuardReceptorProfile(p, "登录邮箱", "a@b.com"); blocked {
		t.Fatalf("凭据意图 + 凭据受体不得熔断")
	}
}

// boxQuad 的几何包含与面积（AX 切片「取面积最小＝最内层」的依据）。
func TestBoxQuadContainsAndArea(t *testing.T) {
	b := boxQuad{X: 100, Y: 200, W: 300, H: 40}
	if !b.contains(250, 220) {
		t.Fatalf("矩形内点必须命中")
	}
	if b.contains(99, 220) || b.contains(250, 241) {
		t.Fatalf("矩形外点不得命中")
	}
	if b.area() != 12000 {
		t.Fatalf("area = %v，期望 12000", b.area())
	}
	inner := boxQuad{X: 110, Y: 210, W: 10, H: 10}
	if inner.area() >= b.area() {
		t.Fatalf("内层面积必须更小（用来选最内层受体）")
	}
}

// 契约测试：寻底探针必须具备「剔除透明手势代理层 + 组合祖先回溯 + 封闭宿主上报」三要素，
// 且不得依赖 Element 上的 composedPath()。
func TestInputStemPiercingJSContract(t *testing.T) {
	for _, want := range []string{"elementsFromPoint", "uwsGestureProxy", "getRootNode", "closed_shadow_host", "aria-hidden"} {
		if !strings.Contains(inputStemPiercingJS, want) {
			t.Fatalf("寻底探针缺少 %q", want)
		}
	}
	if strings.Contains(inputStemPiercingJS, "composedPath") {
		t.Fatalf("composedPath() 是 Event 的方法，Element 上不存在，禁止使用")
	}
	if strings.Contains(inputStemPiercingJS, "document.elementFromPoint") {
		t.Fatalf("寻底必须走 elementsFromPoint 栈（单点 API 只回最外层宿主/透明层）")
	}
	// 真机实测（2026-09-25 E2E 场景 A/E）：封闭宿主的交接必须**先于**兄弟子树「附近输入框」
	// 启发式——否则页内会把值写进 DOM 里另一个无关 input（假成功 + 越权写入）。
	hostIdx := strings.Index(inputStemPiercingJS, "closed_shadow_host")
	neighborIdx := strings.Index(inputStemPiercingJS, "at.parentElement.querySelector")
	if hostIdx < 0 || neighborIdx < 0 || hostIdx > neighborIdx {
		t.Fatalf("封闭宿主交接（closed_shadow_host，位置 %d）必须先于兄弟子树搜索（at.parentElement.querySelector，位置 %d）",
			hostIdx, neighborIdx)
	}
	// 宿主层穿透必须显式让 pointer-events:none 遮罩穿透，且不注入 UA shadow 内部。
	src := strings.Join(strings.Fields(pierceHitSourceForTest(t)), " ")
	for _, want := range []string{
		`"DOM.getNodeForLocation"`, `"ignorePointerEventsNone": true`,
		`"includeUserAgentShadowDOM": false`,
		`"Accessibility.getFullAXTree"`, `"DOM.resolveNode"`, `"Runtime.callFunctionOn"`,
		`GuardReceptorProfile(`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("宿主层穿透实现缺少 %q", want)
		}
	}
	if strings.Contains(src, `"includeUserAgentShadowDOM": true`) {
		t.Fatalf("不得注入 UA shadow 内部（应取宿主元素本身）")
	}
}

// AX `editable` 属性的真机取值形态（Chrome 153 实测为 "plaintext"/"richtext"）必须被认成
// 可编辑——判据写成布尔字符串 "true" 会让 contenteditable 一类受体永远进不了 AX 切片。
func TestAxEditableProp(t *testing.T) {
	cases := map[string]bool{
		"": false, "false": false, "none": false,
		"true": true, "plaintext": true, "richtext": true, " PlainText ": true,
	}
	for in, want := range cases {
		if got := axEditableProp(in); got != want {
			t.Fatalf("axEditableProp(%q) = %v，期望 %v", in, got, want)
		}
	}
}

// AX 切片选择必须「可编辑角色优先、editable 属性兜底」：真机实测 input[type=search] 的
// UA shadow 内部容器（role=generic + editable="plaintext"、内容盒更小）不得压过真实受体。
func TestPickAXPierceCandidatePrefersRoleOverUAShadowInner(t *testing.T) {
	input := &PiercedHit{BackendNodeID: 15, Role: "searchbox", NodeName: "INPUT"}
	inner := &PiercedHit{BackendNodeID: 19, Role: "generic", AXEditable: true, NodeName: "DIV"}
	if got := pickAXPierceCandidate([]axCandidate{
		{hit: inner, isRole: false, area: 4185},
		{hit: input, isRole: true, area: 9928},
	}); got != input {
		t.Fatalf("可编辑角色必须优先于 editable 属性兜底，got backend=%d role=%q", got.BackendNodeID, got.Role)
	}
	// 无角色命中时（contenteditable 类载体）才退到 editable 属性，且取面积最小者。
	big := &PiercedHit{BackendNodeID: 31, Role: "generic", AXEditable: true, NodeName: "DIV"}
	small := &PiercedHit{BackendNodeID: 32, Role: "generic", AXEditable: true, NodeName: "DIV"}
	if got := pickAXPierceCandidate([]axCandidate{
		{hit: big, isRole: false, area: 8000},
		{hit: small, isRole: false, area: 4000},
	}); got != small {
		t.Fatalf("无角色命中时应选 editable 属性候选中面积最小者")
	}
	if got := pickAXPierceCandidate(nil); got != nil {
		t.Fatalf("无候选必须返回 nil")
	}
}

// pierceHitSourceForTest 读取本包 pierce.go 源码（宿主层穿透的真实调用面断言用）。
func pierceHitSourceForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("无法定位测试文件路径，跳过源码级契约断言")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "pierce.go"))
	if err != nil {
		t.Skipf("读取 pierce.go 失败（%v），跳过源码级契约断言", err)
	}
	return string(src)
}

// JS 语法门禁：本批新增/改写的页内脚本必须能被 JS 引擎解析（node --check）。
// 无 node 时跳过（只降低证据强度，不影响构建）。
func TestPiercingJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("未找到 node，跳过页内脚本语法门禁")
	}
	wantJSON := `{"tag":"input","id":"kw","name":"q","type":"text","path":"input#kw","x":1,"y":2}`
	scripts := map[string]string{
		"hitPiercingFnJS":        `(() => {` + hitPiercingFnJS + ` return null; })()`,
		"inputStemPiercingJS":    inputStemPiercingJS,
		"StructuralIdentityFnJS": `(() => {` + StructuralIdentityFnJS + ` return null; })()`,
		"IdentityProbeJS":        IdentityProbeJS,
		"ReanchorCandidateJS":    strings.Replace(ReanchorCandidateJS, "__UWS_WANT__", wantJSON, 1),
		// Runtime.callFunctionOn 的 functionDeclaration 是**匿名函数表达式**，
		// 作为脚本语句要靠括号进入表达式位（门禁按真实用法包一层）。
		"piercedFocusFnJS":     "(" + piercedFocusFnJS + ")",
		"piercedReadValueFnJS": "(" + piercedReadValueFnJS + ")",
		"piercedChangeFnJS":    "(" + piercedChangeFnJS + ")",
		"pierceCallShape":      "(function () { return this.value; })",
	}
	for name, src := range scripts {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name+".js")
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatalf("写入临时脚本失败：%v", err)
			}
			if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
				t.Fatalf("node --check 失败：%v\n%s", err, out)
			}
		})
	}
}
