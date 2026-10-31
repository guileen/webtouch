package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 站点会话清账（会话删除 + 「待删除列表」）——2026-09-26 用户定案：
// 讨论结束后把刚产生的会话删掉；服务器长期堆积临时会话「影响很不好」。
// 允许两种兑现方式：立即删除，或入「待删除列表」由后台异步删除。
//
// 事实来源（真实抓包，见 docs/evidence/pilots/session-purge-2026-09-26.md）：
// Gemini 删除会话 = batchexecute RPC `GzXR5e`，其第二层参数是会话内部 ID 的 JSON 文本
// （形如 `["c_<16hex>"]`，与页面 URL 的 /app/<16hex> 同源），槽位与补全请求同源：
// at=WIZ_global_data.SNlM0e、bl=cfb2h、f.sid=FdrFJe、_reqid=页内递增计数器。
//
// 纪律：只删调用方显式给出（或由该会话标签页实测读出）的会话 ID；
// 不提供「删除全部历史」能力，避免误伤用户真实历史。

const (
	PurgeStatePending = "pending" // 已入列、尚未尝试
	PurgeStateFailed  = "failed"  // 尝试过但未成功（仍在待删除列表里等重试）
	PurgeStateDeleted = "deleted" // 已确认删除（唯一销账状态）

	purgeTimeout = 40 * time.Second
)

var (
	purgeConvIDRe = regexp.MustCompile(`(?:^|/)(?:c_)?([0-9a-f]{16})(?:$|[?#/])`)
	// 千问会话 ID：32 hex；对话页 /chat/<32hex>（契约 §13 实例、SOP 第 7 条）。
	qianwenConvIDRe   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	qianwenConvPathRe = regexp.MustCompile(`(?:^|/)chat/([0-9a-f]{32})(?:$|[?#/])`)
)

// NormalizeConversationID 把站点会话 ID 归一成内部写法（大小写不敏感）：
// Gemini `/app/<16hex>`｜`<16hex>`｜`c_<16hex>` ⇒ `c_<16hex>`；千问 `/chat/<32hex>`｜`<32hex>` ⇒ 原样 32hex。
// 未登记站点形状一律 fail-closed（不猜 ID）。
func NormalizeConversationID(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", errors.New("purge: conversation_id 为空")
	}
	if m := purgeConvIDRe.FindStringSubmatch(s); m != nil {
		return "c_" + m[1], nil
	}
	if m := qianwenConvPathRe.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	if strings.HasPrefix(s, "c_") && len(s) > 2 {
		if m := purgeConvIDRe.FindStringSubmatch(s[2:]); m != nil {
			return "c_" + m[1], nil
		}
	}
	if qianwenConvIDRe.MatchString(s) {
		return s, nil
	}
	return "", fmt.Errorf("purge: 无法识别的会话 ID %q（接受 /app/<16hex>、<16hex>、c_<16hex>、/chat/<32hex>）", raw)
}

// SupportsPurge 站点是否已接会话删除（实测链路：Gemini `batchexecute GzXR5e`；千问 `session/delete/batch`）。
func SupportsPurge(site string) bool {
	switch strings.ToLower(strings.TrimSpace(site)) {
	case "gemini", "qianwen":
		return true
	}
	return false
}

// BuildGeminiDeleteScript 生成页内删除脚本：读页面态槽位 → 直接 POST batchexecute(GzXR5e)。
// 返回值结构见 ConversationDeleteResult。
func BuildGeminiDeleteScript(cid string) string {
	c, err := NormalizeConversationID(cid)
	if err != nil {
		// 归一化失败 ⇒ 不发请求，直接返回 fail-closed 脚本。
		return `(async () => ({ httpStatus: 0, bodyHead: "", domListed: false, requestId: 0, error: "invalid_conversation_id" }))()`
	}
	hex := strings.TrimPrefix(c, "c_")
	// 逐字对齐抓包：f.req = [[["GzXR5e","[\"c_<hex>\"]",null,"generic"]]]
	fReq := fmt.Sprintf(`[[["GzXR5e","[\"%s\"]",null,"generic"]]]`, c)
	return fmt.Sprintf(`(async () => {
  const out = { httpStatus: 0, bodyHead: "", domListed: false, requestId: 0, error: null };
  try {
    const g = window.WIZ_global_data || {};
    const at = g.SNlM0e, bl = g.cfb2h, fsid = g.FdrFJe;
    if (!at || !bl || !fsid) { out.error = "page_slots_missing"; return out; }
    let reqid = 0;
    try {
      const res = performance.getEntriesByType("resource") || [];
      for (const r of res) { const m = /_reqid=(\d+)/.exec(r.name || ""); if (m) reqid = Math.max(reqid, parseInt(m[1], 10)); }
    } catch (e) { /* 计数器读不到走兜底 */ }
    reqid = reqid ? reqid + 100000 : 3000000;
    out.requestId = reqid;
    const fReq = %s;
    const url = "https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=GzXR5e&source-path=" +
      encodeURIComponent(location.pathname) + "&bl=" + encodeURIComponent(bl) + "&f.sid=" + encodeURIComponent(fsid) +
      "&hl=en&_reqid=" + reqid + "&rt=c";
    const body = "f.req=" + encodeURIComponent(fReq) + "&at=" + encodeURIComponent(at);
    const resp = await fetch(url, { method: "POST", credentials: "include",
      headers: { "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8", "X-Same-Domain": "1" }, body: body });
    out.httpStatus = resp.status;
    const text = await resp.text().catch(() => "");
    out.bodyHead = String(text).slice(0, 400);
    await new Promise(r => setTimeout(r, 1500));
    out.domListed = [...document.querySelectorAll('a[href*="/app/"]')].some(a => (a.getAttribute("href") || "").indexOf("%s") >= 0);
  } catch (e) { out.error = String(e); }
  return out;
})()`, strconv.Quote(fReq), hex)
}

// BuildQianwenDeleteScript 生成千问页内删除脚本（契约 §14 / 案卷 §十五）：
// `qwenSign` 签名 → `POST https://chat2-api.qianwen.com/api/v1/session/delete/batch`（body `{session_ids:[<32hex>]}`）
// → 复核「详情 status != 1 且列表不再列出」。任一环失败即回 `error`/`domListed=true`，由 ParseConversationDeleteResult 判未成功（fail-closed）。
func BuildQianwenDeleteScript(cid string) string {
	c, err := NormalizeConversationID(cid)
	if err != nil || !qianwenConvIDRe.MatchString(c) {
		return `(async () => ({ httpStatus: 0, bodyHead: "", domListed: true, requestId: 0, error: "invalid_conversation_id" }))()`
	}
	return fmt.Sprintf(`(async () => {
  const out = { httpStatus: 0, bodyHead: "", domListed: true, requestId: 0, error: null };
  try {
    const sdk = window.__QIANWEN_CHAT_SDK__;
    if (!sdk || typeof sdk.qwenSign !== "function") { out.error = "signer_missing"; return out; }
    const post = async (plain, body) => {
      const sig = await sdk.qwenSign(plain);
      const r = await fetch((sig && sig.signedUrl) || plain, {
        method: "POST", credentials: "include",
        headers: Object.assign({ "Content-Type": "application/json" }, (sig && sig.signedHeader) || {}),
        body: JSON.stringify(body) });
      return { status: r.status, text: await r.text() };
    };
    const del = await post("https://chat2-api.qianwen.com/api/v1/session/delete/batch", { session_ids: [%s] });
    out.httpStatus = del.status;
    out.bodyHead = String(del.text).slice(0, 400);
    if (del.status < 200 || del.status >= 300) { return out; }
    await new Promise(r => setTimeout(r, 600));
    let listed = false, status = 0;
    try {
      const page = await post("https://chat2-api.qianwen.com/api/v2/session/page/list", {});
      const j = JSON.parse(page.text || "{}");
      const items = (j.data && j.data.list) || [];
      listed = items.some(it => it && it.session_id === %s);
    } catch (e) { /* 列表读不到交给 session/get 兜底 */ }
    try {
      const one = await post("https://chat2-api.qianwen.com/api/v1/session/get", { session_id: %s });
      const j = JSON.parse(one.text || "{}");
      if (one.status >= 200 && one.status < 300 && j.data) { status = j.data.status || 0; }
    } catch (e) { /* 详情读不到忽略 */ }
    out.domListed = listed || status === 1;
  } catch (e) { out.error = String(e); }
  return out;
})()`, strconv.Quote(c), strconv.Quote(c), strconv.Quote(c))
}

// ConversationDeleteResult 是页内删除脚本的返回。
type ConversationDeleteResult struct {
	HTTPStatus int    `json:"httpStatus"`
	BodyHead   string `json:"bodyHead"`
	DOMListed  bool   `json:"domListed"`
	RequestID  int64  `json:"requestId"`
	Error      string `json:"error,omitempty"`
	// Verified 只在「HTTP 2xx 且侧栏已不再列出该会话」时为真（fail-closed：不谎报删除成功）。
	Verified bool `json:"verified"`
}

func purgeFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ParseConversationDeleteResult 解析脚本返回值；仅在证据充分时判成功。
func ParseConversationDeleteResult(raw []byte) (ConversationDeleteResult, error) {
	var r ConversationDeleteResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("purge: 删除脚本返回值无法解析: %w", err)
	}
	if r.Error != "" {
		return r, fmt.Errorf("purge: 页内删除失败: %s", r.Error)
	}
	if r.HTTPStatus < 200 || r.HTTPStatus >= 300 {
		return r, fmt.Errorf("purge: 站点返回 HTTP %d: %s", r.HTTPStatus, purgeFirstLine(r.BodyHead))
	}
	r.Verified = !r.DOMListed
	return r, nil
}

// ---------- 待删除列表 ----------

// PurgeItem 是「待删除列表」的一条记录。
type PurgeItem struct {
	ConversationID string    `json:"conversation_id"`
	ChannelID      string    `json:"channel_id"`
	SessionID      string    `json:"session_id,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	State          string    `json:"state"`
	Attempts       int       `json:"attempts"`
	LastError      string    `json:"last_error,omitempty"`
	EnqueuedAt     time.Time `json:"enqueued_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// PurgeQueue 是持久化的「待删除列表」（小体量 JSON，原子写：临时文件 + rename）。
type PurgeQueue struct {
	path  string
	mu    sync.Mutex
	items []PurgeItem
}

// DefaultPurgeQueuePath 返回默认落盘位置（可用 UWS_PURGE_QUEUE 覆盖）。
func DefaultPurgeQueuePath() string {
	if p := strings.TrimSpace(os.Getenv("UWS_PURGE_QUEUE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "uws-gateway-purge-queue.json")
	}
	return filepath.Join(home, ".uws", "gateway-purge-queue.json")
}

// NewPurgeQueue 打开（或创建）待删除列表；读取失败不致命，退化为空表。
func NewPurgeQueue(path string) *PurgeQueue {
	q := &PurgeQueue{path: path}
	if err := q.load(); err != nil {
		q.items = nil
	}
	return q
}

func (q *PurgeQueue) load() error {
	if q == nil || q.path == "" {
		return nil
	}
	raw, err := os.ReadFile(q.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var items []PurgeItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	q.items = items
	return nil
}

func (q *PurgeQueue) saveLocked() error {
	if q == nil || q.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(q.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(q.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, q.path)
}

// Enqueue 入列（同会话 ID 幂等；failed 态可重新入列重试）。
func (q *PurgeQueue) Enqueue(it PurgeItem) (PurgeItem, error) {
	if q == nil {
		return it, errors.New("purge: 待删除列表未启用")
	}
	now := time.Now()
	it.ConversationID = strings.TrimSpace(it.ConversationID)
	if it.ConversationID == "" {
		return it, errors.New("purge: conversation_id 为空")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].ConversationID != it.ConversationID {
			continue
		}
		if q.items[i].State != PurgeStateFailed {
			return q.items[i], nil // 已在列（pending/deleted）⇒ 幂等返回
		}
		q.items[i].State = PurgeStatePending
		q.items[i].LastError = ""
		q.items[i].UpdatedAt = now
		if it.SessionID != "" {
			q.items[i].SessionID = it.SessionID
		}
		if it.Reason != "" {
			q.items[i].Reason = it.Reason
		}
		return q.items[i], q.saveLocked()
	}
	if it.State == "" {
		it.State = PurgeStatePending
	}
	it.EnqueuedAt, it.UpdatedAt = now, now
	q.items = append(q.items, it)
	return it, q.saveLocked()
}

// List 返回全量快照（按入列时间排序）。
func (q *PurgeQueue) List() []PurgeItem {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]PurgeItem, 0, len(q.items))
	out = append(out, q.items...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].EnqueuedAt.Before(out[j].EnqueuedAt) })
	return out
}

// Pending 返回「待删除项」：尚未删除的都算（pending 未试过、failed 试过但没成功，都要继续重试）；
// 只有 deleted 才销账。清账失败不会让记录消失，避免「静默漏删」。
func (q *PurgeQueue) Pending() []PurgeItem {
	out := []PurgeItem{}
	for _, it := range q.List() {
		if it.State != PurgeStateDeleted {
			out = append(out, it)
		}
	}
	return out
}

// Settle 更新一条记录的状态（删除成功 = deleted；失败 = failed + 原因）。
func (q *PurgeQueue) Settle(cid, state, lastErr string) error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].ConversationID != cid {
			continue
		}
		q.items[i].State = state
		q.items[i].Attempts++
		q.items[i].LastError = lastErr
		q.items[i].UpdatedAt = time.Now()
		return q.saveLocked()
	}
	return nil
}

// Forget 从列表移除（用于人工销账 / 不再重试）。
func (q *PurgeQueue) Forget(cid string) (bool, error) {
	if q == nil {
		return false, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].ConversationID != cid {
			continue
		}
		q.items = append(q.items[:i], q.items[i+1:]...)
		return true, q.saveLocked()
	}
	return false, nil
}

// ---------- 执行 ----------

// ConversationIDForSession 从会话标签页实测当前服务端会话 ID（新对话尚无 ID 时报错）。
func (g *Gateway) ConversationIDForSession(ctx context.Context, sid string) (string, error) {
	eval := g.EvaluatorForSession(sid)
	if eval == nil {
		return "", errors.New("purge: 会话无页内执行器（标签页已关或引擎不可用）")
	}
	raw, err := eval.EvaluateScript(ctx, "location.pathname")
	if err != nil {
		return "", fmt.Errorf("purge: 读取会话页面地址失败: %w", err)
	}
	var path string
	if err := json.Unmarshal(raw, &path); err != nil {
		return "", fmt.Errorf("purge: 页面地址返回值无法解析: %w", err)
	}
	cid, err := NormalizeConversationID(path)
	if err != nil {
		return "", fmt.Errorf("purge: 会话 %s 当前页面 %q 没有服务端会话 ID（仍是新对话）", sid, path)
	}
	return cid, nil
}

// DeleteConversationInSession 在指定会话标签页内执行站点删除。
func (g *Gateway) DeleteConversationInSession(ctx context.Context, sid, cid string) (ConversationDeleteResult, error) {
	normalized, err := NormalizeConversationID(cid)
	if err != nil {
		return ConversationDeleteResult{}, err
	}
	eval := g.EvaluatorForSession(sid)
	if eval == nil {
		return ConversationDeleteResult{}, errors.New("purge: 会话无页内执行器（标签页已关或引擎不可用）")
	}
	site := ""
	if ch := g.cfg.Channel(g.sessionChannelID(sid)); ch != nil {
		site = strings.ToLower(strings.TrimSpace(ch.Site))
	}
	var script string
	switch site {
	case "qianwen":
		script = BuildQianwenDeleteScript(normalized)
	default:
		script = BuildGeminiDeleteScript(normalized)
	}
	raw, err := eval.EvaluateScript(ctx, script)
	if err != nil {
		return ConversationDeleteResult{}, fmt.Errorf("purge: 页内执行失败: %w", err)
	}
	return ParseConversationDeleteResult(raw)
}

// purgeSessionFor 为一条待删除项挑一个可用的页内执行会话：
// 优先后该会话自己的标签页 → 同通道任一存活会话 → 新建一个临时会话（调用方在 drain 收尾时关闭）。
func (g *Gateway) purgeSessionFor(ctx context.Context, it PurgeItem, created *[]string) (string, bool) {
	if it.SessionID != "" {
		if s := g.getSession(it.SessionID); s != nil {
			return it.SessionID, true
		}
	}
	g.mu.Lock()
	reuse := ""
	for sid, s := range g.sessions {
		if s.ChannelID == it.ChannelID {
			reuse = sid
			break
		}
	}
	g.mu.Unlock()
	if reuse != "" {
		return reuse, true
	}
	ch := g.cfg.Channel(it.ChannelID)
	if ch == nil || !SupportsPurge(ch.Site) {
		return "", false
	}
	sid := "purge-" + strings.TrimPrefix(it.ConversationID, "c_")
	g.mu.Lock()
	prevCurrent := g.current
	g.mu.Unlock()
	if _, err := g.ensureSession(ctx, ch, sid, ""); err != nil {
		g.mu.Lock()
		g.current = prevCurrent
		g.mu.Unlock()
		return "", false
	}
	// ensureSession 会把该会话置为 current：清账属旁路动作，恢复原状。
	g.mu.Lock()
	g.current = prevCurrent
	g.mu.Unlock()
	*created = append(*created, sid)
	return sid, true
}

// PurgeConversation 立即尝试删除一条会话；失败即入「待删除列表」异步重试。
// 第二个返回值是本次删除的实测结果（未执行时为零值 + err）。
func (g *Gateway) PurgeConversation(ctx context.Context, it PurgeItem) (PurgeItem, ConversationDeleteResult, error) {
	if g.purge == nil {
		return it, ConversationDeleteResult{}, errors.New("purge: 待删除列表未启用")
	}
	cid, err := NormalizeConversationID(it.ConversationID)
	if err != nil {
		return it, ConversationDeleteResult{}, err
	}
	it.ConversationID = cid
	if it.ChannelID == "" && it.SessionID != "" {
		if sess := g.getSession(it.SessionID); sess != nil {
			it.ChannelID = sess.ChannelID
		}
	}
	if it.ChannelID == "" {
		if ch := g.purgeChannelFor(cid); ch != nil {
			it.ChannelID = ch.ID
		}
	}
	if it.ChannelID == "" {
		return it, ConversationDeleteResult{}, errors.New("purge: 无法确定通道（请带 channel_id 或 session_id）")
	}
	if ch := g.cfg.Channel(it.ChannelID); ch == nil || !SupportsPurge(ch.Site) {
		queued, qerr := g.purge.Enqueue(it)
		if qerr != nil {
			return it, ConversationDeleteResult{}, qerr
		}
		return queued, ConversationDeleteResult{}, fmt.Errorf("purge: 通道 %s 未接会话删除（已入待删除列表）", it.ChannelID)
	}
	ctx, cancel := context.WithTimeout(ctx, purgeTimeout)
	defer cancel()
	// 清账是旁路动作：若为此新建了临时标签页，收尾时关掉，不留给运行中的讨论。
	var created []string
	defer func() {
		for _, sid := range created {
			g.closeSession(context.Background(), sid)
		}
	}()
	sid, ok := g.purgeSessionFor(ctx, it, &created)
	if !ok {
		queued, qerr := g.purge.Enqueue(it)
		if qerr != nil {
			return it, ConversationDeleteResult{}, qerr
		}
		return queued, ConversationDeleteResult{}, errors.New("purge: 无可用标签页，已入待删除列表")
	}
	res, derr := g.DeleteConversationInSession(ctx, sid, cid)
	if derr != nil {
		if _, qerr := g.purge.Enqueue(it); qerr != nil {
			return it, res, qerr
		}
		_ = g.purge.Settle(cid, PurgeStateFailed, derr.Error())
		return it, res, derr
	}
	if !res.Verified {
		queued, qerr := g.purge.Enqueue(it)
		if qerr != nil {
			return it, res, qerr
		}
		_ = g.purge.Settle(cid, PurgeStateFailed, "站点返回 2xx 但侧栏仍列出该会话")
		return queued, res, errors.New("purge: 站点返回 2xx 但侧栏仍列出该会话（已入待删除列表重试）")
	}
	if _, qerr := g.purge.Enqueue(it); qerr == nil {
		_ = g.purge.Settle(cid, PurgeStateDeleted, "")
	}
	return it, res, nil
}

// purgeChannelFor 未指定通道时推断清账通道：唯一可清账通道直接用；多个时按会话 ID 形状唯一匹配
// （Gemini `c_<16hex>` / 千问 `<32hex>`，两种形状不重叠）；形状也消歧不了 ⇒ nil（拒绝猜通道）。
func (g *Gateway) purgeChannelFor(cid string) *ChannelConfig {
	if ch := g.onlyPurgeChannel(); ch != nil {
		return ch
	}
	var found *ChannelConfig
	for i := range g.cfg.Channels {
		ch := &g.cfg.Channels[i]
		if ch.Type != ChannelTypeWeb || !SupportsPurge(ch.Site) || !siteAcceptsConversationID(ch.Site, cid) {
			continue
		}
		if found != nil {
			return nil
		}
		found = ch
	}
	return found
}

// siteAcceptsConversationID 站点是否认这个（已归一化的）会话 ID 形状——只认已实测的站点模板。
func siteAcceptsConversationID(site, cid string) bool {
	switch strings.ToLower(strings.TrimSpace(site)) {
	case "gemini":
		return strings.HasPrefix(cid, "c_") && purgeConvIDRe.MatchString(cid)
	case "qianwen":
		return qianwenConvIDRe.MatchString(cid)
	}
	return false
}

// onlyPurgeChannel 在未指定通道时，若恰好只有一个已接删除的网站通道，用它。
func (g *Gateway) onlyPurgeChannel() *ChannelConfig {
	var found *ChannelConfig
	for i := range g.cfg.Channels {
		ch := &g.cfg.Channels[i]
		if ch.Type == ChannelTypeWeb && SupportsPurge(ch.Site) {
			if found != nil {
				return nil
			}
			found = ch
		}
	}
	return found
}

// PurgeDrainReport 是一次清账的结果。
type PurgeDrainReport struct {
	Attempted int         `json:"attempted"`
	Deleted   int         `json:"deleted"`
	Failed    int         `json:"failed"`
	Skipped   int         `json:"skipped"`
	Items     []PurgeItem `json:"items"`
}

// DrainPurge 依次处理待删除项（max>0 时限量）。删除成功即销账；失败留在表里等下次。
func (g *Gateway) DrainPurge(ctx context.Context, max int) PurgeDrainReport {
	rep := PurgeDrainReport{Items: []PurgeItem{}}
	if g.purge == nil {
		return rep
	}
	pending := g.purge.Pending()
	if max > 0 && len(pending) > max {
		pending = pending[:max]
	}
	var created []string
	defer func() {
		for _, sid := range created {
			g.closeSession(context.Background(), sid)
		}
	}()
	for _, it := range pending {
		rep.Attempted++
		sid, ok := g.purgeSessionFor(ctx, it, &created)
		if !ok {
			rep.Skipped++
			_ = g.purge.Settle(it.ConversationID, PurgeStateFailed, "no_live_session")
			continue
		}
		res, err := g.DeleteConversationInSession(ctx, sid, it.ConversationID)
		if err != nil || !res.Verified {
			rep.Failed++
			msg := ""
			if err != nil {
				msg = err.Error()
			} else {
				msg = "站点返回 2xx 但侧栏仍列出该会话"
			}
			_ = g.purge.Settle(it.ConversationID, PurgeStateFailed, msg)
			continue
		}
		rep.Deleted++
		_ = g.purge.Settle(it.ConversationID, PurgeStateDeleted, "")
	}
	rep.Items = g.purge.List()
	return rep
}

func (g *Gateway) sessionChannelID(sid string) string {
	sess := g.getSession(sid)
	if sess == nil {
		return ""
	}
	return sess.ChannelID
}

// bindSessionConversation 记录「本会话自己绑定过的站点对话 ID」——清账目标的**唯一**事实源。
// 只接受站点回传过的真实 ID（响应体自报 / echo 槽位回显 / S2 恢复入参），调用方负责已归一化。
func (g *Gateway) bindSessionConversation(sid, cid string) {
	sid = strings.TrimSpace(sid)
	cid = strings.TrimSpace(cid)
	if sid == "" || cid == "" {
		return
	}
	g.mu.Lock()
	if s := g.sessions[sid]; s != nil {
		s.OwnConversationID = cid
	}
	g.mu.Unlock()
}

// boundConversationID 读本会话自己绑定的站点对话 ID（无绑定返回空串）。
func (g *Gateway) boundConversationID(sid string) string {
	sess := g.getSession(sid)
	if sess == nil {
		return ""
	}
	return strings.TrimSpace(sess.OwnConversationID)
}

// purgeSessionOnClose 关会话前把「**本会话自己的**站点对话」删掉：先趁标签页还活着就地删除，
// 失败即入列等后台清账。
//
// 目标只取会话绑定记录（OwnConversationID），并**与页签当前对话做一致性核对**；
// 核对不通过一律拒绝删除、回落为只解绑。理由（2026-10-03 派单 d27954eeb）：
// 旧实现用 ConversationIDForSession 直接读 location.pathname 当目标，而站点 SPA 会在
// 删除/跳转后自己导航到**别的**对话 ⇒ 关闭会话会删掉「此刻页签所在的那条」，
// 实测误删一条不可恢复的真实对话（标题「简单的回应」）。宁可漏删（有 `POST /v1/purge`
// 显式 ID 的补删通道），绝不再删错。
func (g *Gateway) purgeSessionOnClose(ctx context.Context, sid string, keep bool) map[string]any {
	if g.purge == nil {
		return map[string]any{"status": "disabled"}
	}
	bound := g.boundConversationID(sid)
	// 调试保留（契约 §14「调试保留 · 不清站点对话」）：开则不删任何站点对话，历史留在站点侧供 review。
	// 放在绑定核对**之前**——调试态连「绑定对不上」都不该删，而且要把 conv 记进日志便于 review 定位。
	if keep {
		g.logger.Printf("调试保留 · 未清账 sid=%s conv=%s（%s 开启，跳过站点对话删除，历史留站点侧供 review）",
			sid, orPlaceholder(bound, "(未绑定)"), KeepSessionsEnv)
		return map[string]any{"status": "skipped", "reason": "debug_keep",
			"conversation_id": bound,
			"error":           "调试保留开启（" + KeepSessionsEnv + "／keep=1），保留站点对话不清账"}
	}
	if bound == "" {
		g.logger.Printf("purge: 关会话清账拒绝 sid=%s reason=no_binding —— 本会话没有绑定过的站点对话 ID，拒绝按页签地址推断目标", sid)
		return map[string]any{"status": "skipped", "reason": "no_binding",
			"error": "本会话没有绑定过的站点对话 ID，拒绝按页签当前地址推断清账目标（fail-closed）"}
	}
	cur, err := g.ConversationIDForSession(ctx, sid)
	if err != nil {
		g.logger.Printf("purge: 关会话清账拒绝 sid=%s reason=page_unreadable target=%s err=%v", sid, bound, err)
		return map[string]any{"status": "skipped", "reason": "page_unreadable",
			"conversation_id": bound, "error": err.Error()}
	}
	if cur != bound {
		g.logger.Printf("purge: 关会话清账拒绝 sid=%s reason=mismatch target=%s page_conversation_id=%s —— 页签已不在本会话对话页，拒绝删除", sid, bound, cur)
		return map[string]any{"status": "skipped", "reason": "mismatch",
			"conversation_id": bound, "page_conversation_id": cur,
			"error": "清账目标（会话绑定对话）与页签当前对话不一致，拒绝删除（fail-closed），仅解绑"}
	}
	// 删除前把目标 id ＋ 标题写进日志（可审计）：出事故时能一眼看出删的是哪条。
	title := g.conversationTitle(ctx, sid, bound)
	g.logger.Printf("purge: 关会话清账 target=%s title=%q channel=%s reason=session_closed", bound, title, g.sessionChannelID(sid))
	item, res, derr := g.PurgeConversation(ctx, PurgeItem{
		ConversationID: bound,
		SessionID:      sid,
		ChannelID:      g.sessionChannelID(sid),
		Reason:         "session_closed",
	})
	if derr != nil {
		return map[string]any{"status": "queued", "conversation_id": item.ConversationID, "error": derr.Error(), "result": res}
	}
	return map[string]any{"status": "deleted", "conversation_id": item.ConversationID, "verified": res.Verified, "result": res}
}

// orPlaceholder 给日志用：空值显示占位符（不猜、不省略）。
func orPlaceholder(v, ph string) string {
	if strings.TrimSpace(v) == "" {
		return ph
	}
	return v
}

// conversationTitle 尽力读站点侧栏里该对话的标题（只为审计日志；读不到返回空串，不猜）。
func (g *Gateway) conversationTitle(ctx context.Context, sid, cid string) string {
	out, err := g.ListSiteConversations(ctx, sid)
	if err != nil || out == nil {
		return ""
	}
	for _, c := range out.Conversations {
		if c.ID == cid {
			return c.Title
		}
	}
	return ""
}

// StartPurgeWorker 起后台清账协程（interval<=0 不启动）；每轮只处理有待删除项的情况。
func (g *Gateway) StartPurgeWorker(interval time.Duration) {
	if g == nil || g.purge == nil || interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if len(g.purge.Pending()) == 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*purgeTimeout)
			rep := g.DrainPurge(ctx, 0)
			cancel()
			g.logger.Printf("purge: 后台清账 attempted=%d deleted=%d failed=%d skipped=%d", rep.Attempted, rep.Deleted, rep.Failed, rep.Skipped)
		}
	}()
}

// defaultPurgeInterval 后台清账间隔（UWS_PURGE_INTERVAL 覆盖，单位秒；<=0 关闭）。
func defaultPurgeInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("UWS_PURGE_INTERVAL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				return 0
			}
			return time.Duration(n) * time.Second
		}
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 10 * time.Minute
}
