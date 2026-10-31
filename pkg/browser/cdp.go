// 最小 Chrome DevTools Protocol 客户端（纯标准库）。
//
// 覆盖 CDP 需要的子集：浏览器级 + target 级（flattened sessionId）命令，
// 命令 id 关联、错误响应、事件订阅；协议细节见
// https://chromedevtools.github.io/devtools-protocol/ 。
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"uws/pkg/browser/transport"
)

// cdpError 是 CDP 命令错误响应（{"id":N,"error":{"code":..,"message":..}}）。
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message) }

// cdpEnvelope 是 CDP 线协议消息（命令/响应/事件共用一个信封）。
type cdpEnvelope struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

// cdpEvent 是一次订阅到的 CDP 事件（含来源 sessionId）。
type cdpEvent struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

// cdpClient 是单条 CDP WebSocket 连接上的命令/事件客户端。
// 并发安全：命令发送串行化；事件由唯一 reader goroutine 分发。
type cdpClient struct {
	ws *transport.Conn

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *cdpEnvelope

	handlersMu sync.RWMutex
	handlerSeq int64
	handlers   map[string][]cdpHandler

	closeOnce sync.Once
	done      chan struct{}
}

type cdpHandler struct {
	id int64
	fn func(cdpEvent)
}

// newCDPClient 连接 CDP WebSocket 端点并启动 reader goroutine。
func newCDPClient(ctx context.Context, wsURL string) (*cdpClient, error) {
	ws, err := transport.DialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	c := &cdpClient{
		ws:       ws,
		pending:  make(map[int64]chan *cdpEnvelope),
		handlers: make(map[string][]cdpHandler),
		done:     make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// readLoop 读取消息：命令响应路由到 pending，事件分发到 handlers。
func (c *cdpClient) readLoop() {
	defer close(c.done)
	for {
		opcode, payload, err := c.ws.ReadMessage()
		if err != nil {
			if err != io.EOF {
				// 连接异常：唤醒所有 pending，避免调用方永久阻塞。
				c.failPending(fmt.Errorf("cdp: connection closed: %w", err))
			}
			return
		}
		if opcode != 1 { // 只处理文本帧（CDP 使用 JSON 文本）
			continue
		}
		var env cdpEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			continue
		}
		if env.ID > 0 {
			c.routeResponse(env)
			continue
		}
		if env.Method != "" {
			c.dispatchEvent(cdpEvent{
				SessionID: env.SessionID,
				Method:    env.Method,
				Params:    env.Params,
			})
		}
	}
}

func (c *cdpClient) routeResponse(env cdpEnvelope) {
	c.mu.Lock()
	ch, ok := c.pending[env.ID]
	if ok {
		delete(c.pending, env.ID)
	}
	c.mu.Unlock()
	if ok {
		ch <- &env
	}
}

func (c *cdpClient) failPending(err error) {
	c.mu.Lock()
	pend := c.pending
	c.pending = make(map[int64]chan *cdpEnvelope)
	c.mu.Unlock()
	for _, ch := range pend {
		ch <- &cdpEnvelope{Error: &cdpError{Code: -1, Message: err.Error()}}
	}
}

func (c *cdpClient) dispatchEvent(ev cdpEvent) {
	key := ev.SessionID + "\x00" + ev.Method
	c.handlersMu.RLock()
	hs := c.handlers[key]
	c.handlersMu.RUnlock()
	for _, h := range hs {
		h.fn(ev)
	}
}

// On 订阅事件：sessionID 为空订阅浏览器级事件；非空订阅指定 target 会话事件。
// 返回的函数可解除订阅，调用多次安全。长生命周期会话必须在 Close 时释放订阅，
// 否则重建会话后旧回调仍可能接到 Target 事件。
func (c *cdpClient) On(sessionID, method string, fn func(cdpEvent)) func() {
	key := sessionID + "\x00" + method
	c.handlersMu.Lock()
	c.handlerSeq++
	id := c.handlerSeq
	c.handlers[key] = append(c.handlers[key], cdpHandler{id: id, fn: fn})
	c.handlersMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.handlersMu.Lock()
			defer c.handlersMu.Unlock()
			hs := c.handlers[key]
			for i, h := range hs {
				if h.id == id {
					c.handlers[key] = append(hs[:i], hs[i+1:]...)
					return
				}
			}
		})
	}
}

// OffSession 注销某个 target 级 session 上的全部事件处理器（返回注销条数）。
// 用途：会话切换当前 Target（新窗口接管，见 cdpSession.SwitchTarget）后，旧页
// 仍在后台运行，它的事件不得继续写入本会话的状态机（导航标记、challenge、聊天
// 流等共享状态会被旧页污染）。
func (c *cdpClient) OffSession(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	prefix := sessionID + "\x00"
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	n := 0
	for k := range c.handlers {
		if strings.HasPrefix(k, prefix) {
			delete(c.handlers, k)
			n++
		}
	}
	return n
}

// Send 发送浏览器级命令并等待响应。
func (c *cdpClient) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.send(ctx, "", method, params)
}

// SendSession 发送 target 级命令（flattened attach 后的 sessionId）。
func (c *cdpClient) SendSession(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	return c.send(ctx, sessionID, method, params)
}

// SendSessionNoWait writes a target-scoped CDP command without registering a
// pending response waiter. It is intentionally limited to protocol messages
// such as Page.screencastFrameAck that are emitted from the reader's event
// callback: waiting there would deadlock the reader that routes responses.
// Chrome may still send a response; routeResponse safely drops it because no
// pending entry exists.
func (c *cdpClient) SendSessionNoWait(sessionID, method string, params any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	msg := cdpEnvelope{ID: id, Method: method, SessionID: sessionID}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("cdp: marshal %s params: %w", method, err)
		}
		msg.Params = b
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("cdp: marshal envelope: %w", err)
	}
	if err := c.ws.WriteText(payload); err != nil {
		return fmt.Errorf("cdp: send %s: %w", method, err)
	}
	return nil
}

func (c *cdpClient) send(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan *cdpEnvelope, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := cdpEnvelope{ID: id, Method: method, SessionID: sessionID}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			c.dropPending(id)
			return nil, fmt.Errorf("cdp: marshal %s params: %w", method, err)
		}
		msg.Params = b
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		c.dropPending(id)
		return nil, fmt.Errorf("cdp: marshal envelope: %w", err)
	}
	if err := c.ws.WriteText(payload); err != nil {
		c.dropPending(id)
		return nil, fmt.Errorf("cdp: send %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.dropPending(id)
		return nil, fmt.Errorf("cdp: %s: %w", method, ctx.Err())
	case <-c.done:
		return nil, fmt.Errorf("cdp: %s: connection closed", method)
	}
}

func (c *cdpClient) dropPending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Call 发送命令并把 result 反序列化到 out（out 可为 nil）。
func (c *cdpClient) Call(ctx context.Context, method string, params, out any) error {
	raw, err := c.Send(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("cdp: decode %s result: %w", method, err)
	}
	return nil
}

// CallSession 发送 target 级命令并把 result 反序列化到 out。
func (c *cdpClient) CallSession(ctx context.Context, sessionID, method string, params, out any) error {
	raw, err := c.SendSession(ctx, sessionID, method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("cdp: decode %s result: %w", method, err)
	}
	return nil
}

// Close 关闭连接（reader goroutine 随底层 conn 关闭退出）。
func (c *cdpClient) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.ws.Close()
	})
	return err
}

// waitEvent 等待指定方法事件（测试与一次性初始化用），返回事件或超时。
func (c *cdpClient) waitEvent(ctx context.Context, sessionID, method string) (cdpEvent, error) {
	key := sessionID + "\x00" + method
	ch := make(chan cdpEvent, 1)
	fn := func(ev cdpEvent) {
		select {
		case ch <- ev:
		default:
		}
	}
	c.handlersMu.Lock()
	c.handlerSeq++
	hh := cdpHandler{id: c.handlerSeq, fn: fn}
	c.handlers[key] = append(c.handlers[key], hh)
	c.handlersMu.Unlock()
	defer func() {
		c.handlersMu.Lock()
		hs := c.handlers[key]
		for i, h := range hs {
			if h.id == hh.id {
				c.handlers[key] = append(hs[:i], hs[i+1:]...)
				break
			}
		}
		c.handlersMu.Unlock()
	}()
	select {
	case ev := <-ch:
		return ev, nil
	case <-ctx.Done():
		return cdpEvent{}, ctx.Err()
	case <-time.After(15 * time.Second):
		return cdpEvent{}, fmt.Errorf("cdp: timeout waiting for %s", method)
	}
}

// attachTarget 以 flatten 模式附着 target，返回 target 级 sessionId。
func (c *cdpClient) attachTarget(ctx context.Context, targetID string) (string, error) {
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "Target.attachToTarget",
		map[string]any{"targetId": targetID, "flatten": true}, &out); err != nil {
		return "", err
	}
	return out.SessionID, nil
}

// targetSpec 是 createTarget 的窗口语义（由 window.go 的 windowPolicy 映射而来）。
type targetSpec struct {
	NewWindow  bool  // true = 新窗口（专属小窗），false = 现有窗口的新标签（旧行为）
	Background bool  // true = 后台创建，不把窗口拉到前台
	Focus      *bool // false = 不聚焦（nil = Chrome 默认）
	Width      int   // 仅 NewWindow 生效（窗口外框 DIP）
	Height     int
}

// createTarget 创建新标签页/新窗口（attach 模式不破坏现有标签页）。
func (c *cdpClient) createTarget(ctx context.Context, url string, spec targetSpec) (string, error) {
	var out struct {
		TargetID string `json:"targetId"`
	}
	params := map[string]any{"url": url, "newWindow": spec.NewWindow, "background": spec.Background}
	if spec.Focus != nil {
		params["focus"] = *spec.Focus
	}
	if spec.NewWindow && spec.Width > 0 {
		params["width"] = spec.Width
	}
	if spec.NewWindow && spec.Height > 0 {
		params["height"] = spec.Height
	}
	if err := c.Call(ctx, "Target.createTarget", params, &out); err != nil {
		return "", err
	}
	return out.TargetID, nil
}

// windowIDForTarget 读取 target 所在浏览器窗口的 windowId（浏览器级命令）。
func (c *cdpClient) windowIDForTarget(ctx context.Context, targetID string) (int, error) {
	var out struct {
		WindowID int `json:"windowId"`
	}
	if err := c.Call(ctx, "Browser.getWindowForTarget",
		map[string]any{"targetId": targetID}, &out); err != nil {
		return 0, err
	}
	return out.WindowID, nil
}

// moveWindow 设置窗口位置（只带 left/top/state，尺寸沿用 createTarget 的结果；
// 未指定字段按 CDP 语义保持不变）。
func (c *cdpClient) moveWindow(ctx context.Context, windowID, left, top int) error {
	return c.Call(ctx, "Browser.setWindowBounds", map[string]any{
		"windowId": windowID,
		"bounds":   map[string]any{"left": left, "top": top, "windowState": "normal"},
	}, nil)
}

// browserHeadlessState 读取 CDP /json/version 的 UA，判断附着对象的真实形态：
// 无头浏览器没有可见窗口，专属小窗策略对它是空操作。
//
// known=false 表示这次读不到（连不上 / 不是 HTTP 端点 / UA 为空）。调用方必须保留
// 自己的默认值，不能凭「读不到」下结论（2026-09-22 现场批次 #2：附着到真 Chrome 的
// 会话曾沿用引擎的 headless=true 默认，于是窗口策略退化成 tab、真窗口搬不动）。
func browserHeadlessState(ctx context.Context, base string) (headless bool, known bool) {
	body, err := httpGet(ctx, strings.TrimRight(base, "/")+"/json/version")
	if err != nil {
		return false, false
	}
	var ver struct {
		UserAgent string `json:"User-Agent"`
	}
	if err := json.Unmarshal(body, &ver); err != nil || strings.TrimSpace(ver.UserAgent) == "" {
		return false, false
	}
	return strings.Contains(ver.UserAgent, "HeadlessChrome"), true
}

// browserIsHeadless 是 browserHeadlessState 的两态包装：读不到时按「有头」处理
// （与历史行为一致，见调用点注释）。
func browserIsHeadless(ctx context.Context, base string) bool {
	headless, _ := browserHeadlessState(ctx, base)
	return headless
}

// resolveBrowserEndpoint 把 http(s)://host:port 解析为浏览器 WebSocket 调试端点。
func resolveBrowserEndpoint(ctx context.Context, base string) (string, error) {
	if len(base) >= 3 && base[:3] == "ws:" || len(base) >= 4 && base[:4] == "wss:" {
		return base, nil
	}
	verURL := base + "/json/version"
	body, err := httpGet(ctx, verURL)
	if err != nil {
		return "", err
	}
	var ver struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &ver); err != nil {
		return "", fmt.Errorf("cdp: decode %s: %w", verURL, err)
	}
	if ver.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("cdp: no webSocketDebuggerUrl in %s", verURL)
	}
	return ver.WebSocketDebuggerURL, nil
}

// httpGet 执行一次短超时 GET 并返回响应体。
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp: new request %s: %w", url, err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdp: get %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cdp: read %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cdp: %s status %d", url, resp.StatusCode)
	}
	return body, nil
}
