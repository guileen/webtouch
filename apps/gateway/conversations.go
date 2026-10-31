package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// 站点侧栏对话列表（契约 §9「站点会话身份」S3，T-31）。
//
// 只读：在会话所在页签里收集全部锚点的 href / 文本，再用**站点登记模板**
// （SiteConversationFromURL）筛出对话条目。选择器不写死在页内脚本里，
// 站点差异集中在 config.go 的地址模板表；页面改版只影响锚点扫描本身。
// 纪律：读不到就回空数组 + error，**不虚构**条目。

// SiteConversation 是站点侧栏的一条对话（契约 §9 S3：id + 标题）。
type SiteConversation struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ConversationsResult 是 GET /v1/sessions/{id}/conversations 的响应体。
// Error 非空表示本次读不到站点侧栏（Conversations 必为空数组，不伪造）。
type ConversationsResult struct {
	SessionID     string             `json:"session_id"`
	Site          string             `json:"site,omitempty"`
	Count         int                `json:"count"`
	Conversations []SiteConversation `json:"conversations"`
	Error         string             `json:"error,omitempty"`
}

// sidebarAnchorsScript 收集页内全部锚点的 href 与可见文本（截断在页内完成，避免大页面回传过量）。
const sidebarAnchorsScript = `(() => {
  const out = [];
  for (const a of document.querySelectorAll("a[href]")) {
    const href = a.getAttribute("href") || "";
    const title = (a.textContent || "").replace(/\s+/g, " ").trim().slice(0, 200);
    out.push({ href: href, title: title });
  }
  return out;
})()`

// ListSiteConversations 枚举该会话页签里的站点侧栏对话（契约 §9 S3）。
func (g *Gateway) ListSiteConversations(ctx context.Context, sid string) (*ConversationsResult, error) {
	sess := g.getSession(sid)
	if sess == nil || sess.ContextID == "" {
		return nil, &NotFoundError{Reason: fmt.Sprintf("会话 %q 不存在或已关闭", sid)}
	}
	ch := g.cfg.Channel(sess.ChannelID)
	if ch == nil {
		return nil, &NotFoundError{Reason: fmt.Sprintf("会话 %q 的通道 %q 不存在", sid, sess.ChannelID)}
	}
	out := &ConversationsResult{SessionID: sid, Site: ch.Site, Conversations: []SiteConversation{}}
	eval := g.EvaluatorForSession(sid)
	if eval == nil {
		out.Error = "页内执行器不可用（页签已关或引擎不可达）"
		return out, nil
	}
	raw, err := eval.EvaluateScript(ctx, sidebarAnchorsScript)
	if err != nil {
		out.Error = "读取站点侧栏失败: " + err.Error()
		return out, nil
	}
	var anchors []struct {
		Href  string `json:"href"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &anchors); err != nil {
		out.Error = "站点侧栏返回值无法解析: " + err.Error()
		return out, nil
	}
	seen := make(map[string]bool)
	for _, a := range anchors {
		id, ok := SiteConversationFromURL(ch.Site, a.Href)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		out.Conversations = append(out.Conversations, SiteConversation{ID: id, Title: strings.TrimSpace(a.Title)})
	}
	out.Count = len(out.Conversations)
	return out, nil
}
