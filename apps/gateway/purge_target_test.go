package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 2026-10-03 派单 d27954eeb：清账目标必须取「本会话自己绑定的站点对话 ID」，
// 并与页签当前对话核对；不一致一律拒绝删除（fail-closed），只解绑。
//
// 背景：旧实现拿 ConversationIDForSession（读 location.pathname）当目标，
// 站点 SPA 自动跳转后会把「此刻页签所在的那条」删掉——实测误删一条不可恢复的真实对话。

const (
	targetCid  = "c_25096c6ba2166662" // 会话绑定记录里的对话
	otherCid   = "c_44aa11bb22cc33dd" // 页签漂移到的另一条对话
	delVerified = `{"httpStatus":200,"domListed":false,"requestId":1}`
)

// purgeTargetEvaluator 同时喂 location.pathname / location.href / 删除脚本 / 侧栏锚点。
func purgeTargetEvaluator(t *testing.T, pagePath string, sidebar []map[string]string) func(ActionRequest) (ActionResult, error) {
	t.Helper()
	return func(req ActionRequest) (ActionResult, error) {
		switch {
		case req.Value == "location.pathname":
			b, _ := json.Marshal(pagePath)
			return ActionResult{Success: true, ExtractedData: string(b)}, nil
		case req.Value == "location.href":
			b, _ := json.Marshal("https://gemini.google.com" + pagePath)
			return ActionResult{Success: true, ExtractedData: string(b)}, nil
		case strings.Contains(req.Value, "GzXR5e"):
			return ActionResult{Success: true, ExtractedData: delVerified}, nil
		case req.Value == sidebarAnchorsScript:
			b, _ := json.Marshal(sidebar)
			return ActionResult{Success: true, ExtractedData: string(b)}, nil
		default:
			t.Errorf("未预期的页内脚本: %.60s", req.Value)
			return ActionResult{Success: true, ExtractedData: "null"}, nil
		}
	}
}

// 埋一个「已绑定对话」的活会话。
func addBoundSession(gw *Gateway, sid, cid string) {
	addLiveSession(gw, sid, "gemini-web")
	gw.bindSessionConversation(sid, cid)
}

// 错配：绑定 c_x、页签停在 c_y ⇒ 拒绝删除，只解绑。
func TestDeleteSessionRefusesPurgeOnTargetMismatch(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeTargetEvaluator(t, "/app/"+strings.TrimPrefix(otherCid, "c_"), []map[string]string{})
	gw := newPurgeGateway(t, engine)
	addBoundSession(gw, "s-mismatch", targetCid)
	h := NewHandler(gw, nil)

	rec := doJSON(t, h, http.MethodDelete, "/v1/sessions/s-mismatch?purge=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status string         `json:"status"`
		Purge  map[string]any `json:"purge"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Purge["status"] != "skipped" || out.Purge["reason"] != "mismatch" {
		t.Fatalf("错配必须拒绝删除: %s", rec.Body.String())
	}
	if out.Purge["conversation_id"] != targetCid || out.Purge["page_conversation_id"] != otherCid {
		t.Fatalf("拒绝原因要写清两侧 ID: %s", rec.Body.String())
	}
	for _, it := range gw.purge.List() {
		if it.ConversationID == otherCid || it.ConversationID == targetCid {
			t.Fatalf("拒绝删除后不得销账/入列: %+v", it)
		}
	}
	if len(gw.ListSessions().Sessions) != 0 {
		t.Fatalf("会话本身仍应解绑: %+v", gw.ListSessions().Sessions)
	}
}

// 无绑定记录：一律拒绝删除（不按页签地址猜目标）。
func TestDeleteSessionRefusesPurgeWithoutBinding(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeTargetEvaluator(t, "/app/25096c6ba2166662", []map[string]string{})
	gw := newPurgeGateway(t, engine)
	addLiveSession(gw, "s-nobind", "gemini-web") // 故意不 bind
	h := NewHandler(gw, nil)

	rec := doJSON(t, h, http.MethodDelete, "/v1/sessions/s-nobind?purge=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"no_binding"`) {
		t.Fatalf("无绑定必须拒绝删除: %s", rec.Body.String())
	}
	for _, it := range gw.purge.List() {
		if it.State == PurgeStateDeleted {
			t.Fatalf("无绑定不得删除任何东西: %+v", it)
		}
	}
}

// 匹配：绑定 == 页签当前对话 ⇒ 显式 purge=1 才删，且带目标 + 标题审计日志。
func TestDeleteSessionPurgesOnTargetMatch(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeTargetEvaluator(t, "/app/25096c6ba2166662", []map[string]string{
		{"href": "/app/25096c6ba2166662", "title": "简单的回应"},
	})
	gw := newPurgeGateway(t, engine)
	addBoundSession(gw, "s-match", targetCid)
	h := NewHandler(gw, nil)

	rec := doJSON(t, h, http.MethodDelete, "/v1/sessions/s-match?purge=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"deleted"`) {
		t.Fatalf("目标匹配应删除: %s", rec.Body.String())
	}
	items := gw.purge.List()
	if len(items) != 1 || items[0].ConversationID != targetCid || items[0].State != PurgeStateDeleted {
		t.Fatalf("应按绑定目标销账: %+v", items)
	}
}

// 默认与 purge=0 都只解绑，不清账（新增默认：删除必须显式 purge=1）。
func TestDeleteSessionDefaultOnlyUnbinds(t *testing.T) {
	for _, url := range []string{"/v1/sessions/s-plain", "/v1/sessions/s-plain?purge=0"} {
		engine := newFakeEngine()
		engine.evaluateFunc = purgeTargetEvaluator(t, "/app/25096c6ba2166662", []map[string]string{})
		gw := newPurgeGateway(t, engine)
		addBoundSession(gw, "s-plain", targetCid)
		h := NewHandler(gw, nil)

		rec := doJSON(t, h, http.MethodDelete, url, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", url, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), `"purge"`) {
			t.Fatalf("%s 不应清账（须显式 purge=1）: %s", url, rec.Body.String())
		}
		if len(gw.purge.List()) != 0 {
			t.Fatalf("%s 不得产生清账记录: %+v", url, gw.purge.List())
		}
	}
}

// purgeSessionOnClose 直接调用（空闲回收 / 一次性会话走这条）也受同一核对约束。
func TestPurgeSessionOnCloseRefusesMismatch(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluateFunc = purgeTargetEvaluator(t, "/app/"+strings.TrimPrefix(otherCid, "c_"), []map[string]string{})
	gw := newPurgeGateway(t, engine)
	addBoundSession(gw, "s-reap", targetCid)

	info := gw.purgeSessionOnClose(context.Background(), "s-reap", false)
	if info["status"] != "skipped" || info["reason"] != "mismatch" {
		t.Fatalf("空闲回收同样必须 fail-closed: %+v", info)
	}
}
