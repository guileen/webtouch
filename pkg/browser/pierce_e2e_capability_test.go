package browser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"uws/pkg/browser/input"
)

// TestPierceE2ECapabilitySelfProof 自证 Phase 1 依赖的宿主层 CDP 能力在真实 Chrome 上
// 成立（spec/algorithm-kernel.md §5.0.10 条款 1 第二款）。这些是落地前「未经实测的
// 设计推断」，必须由真机画面外的原始读数确认，不能靠单测假定：
//
//	① Accessibility.getFullAXTree 是否穿透 closed Shadow Root，且可编辑受体的
//	   role / name / editable 属性取值形态（"true" 还是 "plaintext"）；
//	② DOM.getBoxModel 在**未显式 DOM.enable** 时能否按 backendNodeId 返回 content quad；
//	③ DOM.getNodeForLocation(ignorePointerEventsNone=true) 在未 DOM.enable 时能否
//	   返回 backendNodeId；
//	④ DOM.describeNode / DOM.resolveNode / Runtime.callFunctionOn 能否拿到 closed 根内
//	   节点的对象句柄并读写其 value。
func TestPierceE2ECapabilitySelfProof(t *testing.T) {
	if os.Getenv("UWS_RUN_CHROME_E2E") != "1" {
		t.Skip("未设置 UWS_RUN_CHROME_E2E=1，跳过真机用例")
	}
	attachURL := os.Getenv("UWS_E2E_ATTACH_URL")
	if attachURL == "" {
		t.Skip("未设置 UWS_E2E_ATTACH_URL，跳过真机穿透能力自证用例")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pagePath := filepath.Join(t.TempDir(), "pierce-e2e.html")
	if err := os.WriteFile(pagePath, []byte(pierceE2EPage), 0o644); err != nil {
		t.Fatalf("写测试页: %v", err)
	}
	f := &CDPFactory{Headless: true}
	sess, err := f.Create(ctx, Config{
		AttachCDPURL:     attachURL,
		Viewport:         &Viewport{Width: 1280, Height: 900, DeviceScaleFactor: 1},
		ViewportExplicit: true,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sess.Close() }()
	s := sess.(*cdpSession)
	if err := s.Navigate(ctx, "file://"+pagePath); err != nil {
		t.Fatalf("导航测试页: %v", err)
	}

	var pt struct{ X, Y float64 }
	if err := s.evalValue(ctx, "window.__uwsCenterA()", &pt); err != nil {
		t.Fatalf("读场景 A 坐标: %v", err)
	}

	// ① AX 切片：closed 根内受体的 role / name / editable 原始取值
	var axRes struct {
		Nodes []axNodeRaw `json:"nodes"`
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Accessibility.enable", map[string]any{}, nil); err != nil {
		t.Fatalf("Accessibility.enable: %v", err)
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Accessibility.getFullAXTree", map[string]any{}, &axRes); err != nil {
		t.Fatalf("Accessibility.getFullAXTree: %v", err)
	}
	seen := 0
	for i := range axRes.Nodes {
		n := axRes.Nodes[i]
		role := axStr(n.Role)
		if role != "searchbox" && role != "textbox" {
			continue
		}
		seen++
		t.Logf("EVIDENCE AX role=%q name=%q ignored=%v backend=%d editable=%q",
			role, axStr(n.Name), n.Ignored, n.BackendDOMNodeID, axProp(n, "editable"))
	}
	if seen == 0 {
		t.Fatalf("① AX 切片未含任何 textbox/searchbox 节点：AX 未穿透 closed Shadow Root")
	}

	// ②③④ 宿主层命中与节点句柄（AX 优先、DOM 兜底）
	axHit, axErr := input.PierceViaAX(ctx, bridgeInputSession{s}, pt.X, pt.Y)
	t.Logf("EVIDENCE pierceViaAX hit=%v err=%v", axHit, axErr)
	domHit, domErr := input.PierceViaNodeForLocation(ctx, bridgeInputSession{s}, pt.X, pt.Y)
	if domHit != nil {
		t.Logf("EVIDENCE pierceViaNodeForLocation backend=%d tag=%s nodeName=%s box=%.0fx%.0f err=%v",
			domHit.BackendNodeID, domHit.Tag(), domHit.NodeName, domHit.W, domHit.H, domErr)
	} else {
		t.Logf("EVIDENCE pierceViaNodeForLocation hit=nil err=%v", domErr)
	}
	if axHit == nil && domHit == nil {
		t.Fatalf("②③ 宿主层双路皆未命中（AX err=%v / DOM err=%v）：DOM.getBoxModel 或 DOM.getNodeForLocation 在未 DOM.enable 时不可用", axErr, domErr)
	}
	hit := axHit
	if hit == nil {
		hit = domHit
	}
	if hit.ObjectID == "" {
		t.Fatalf("④ DOM.resolveNode 未给出闭包内节点对象句柄：backend=%d tag=%s", hit.BackendNodeID, hit.Tag())
	}
	// Runtime.callFunctionOn 读写 closed 根内节点：写探针值再回读，验证句柄真能触碰该节点。
	const writeFn = `function () { this.value = 'capability-probe'; return this.value; }`
	raw, err := input.CallOnPierced(ctx, bridgeInputSession{s}, hit.ObjectID, writeFn)
	if err != nil {
		t.Fatalf("④ Runtime.callFunctionOn 在 closed 根节点上失败: %v", err)
	}
	t.Logf("EVIDENCE callFunctionOn(closed) 回读=%s", string(raw))
	got, err := input.PiercedValue(ctx, bridgeInputSession{s}, hit.ObjectID)
	if err != nil || got != "capability-probe" {
		t.Fatalf("④ closed 根内节点写后回读不一致：got=%q err=%v", got, err)
	}
	t.Logf("EVIDENCE closed 根内写值成功：%q", got)
}
