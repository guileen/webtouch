// 聊天 SSE 捕获管道（方案 1：CDP Network 层被动劫持，无页面 JS 注入）。
//
// 目标：DeepSeek 等网页聊天端把最终回答经 `POST /api/v0/chat/completion`
// 以 text/event-stream 下发。DOM 提取会把联网搜索的引用角标（如
// `<span class="ds-citation">`）摊平成 `-1-4-7` 类碎片；本管道直接在网络层
// 拿 SSE 的 JSON delta chunk，解析出干净 Markdown（content / reasoning_content），
// 以「快照 + 增量事件」两种语义暴露给上层（引擎 extract / 协调器流式）。
//
// 捕获机制（Chrome 新版实测路径）：
//   - Network.requestWillBeSent 命中聊天 URL → 武装 Network.streamResourceContent
//     （返回已缓冲字节，此后增量经 Network.dataReceived.data=base64 下发）；
//   - Network.loadingFinished / loadingFailed 收尾；
//   - 旧版 Chrome 不支持 streamResourceContent 时静默降级（快照为空，
//     API 层回退 DOM extract，不破坏既有链路）。
package modelbridge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
)

// IsChatCompletionURL 判定请求是否命中「聊天补全」SSE 端点。
// DeepSeek Web 端实测：POST /api/v0/chat/completion；保留 /chat/completions
// 等常见形态以兼容其它前端（path 匹配，忽略 query）。
func IsChatCompletionURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	idx := strings.Index(p, "chat/completion")
	if idx < 0 {
		return false
	}
	// 段边界校验：`chat/completion` 后只允许结束、路径延续（/）或复数 s
	// （OpenAI 风格 /v1/chat/completions）；`chat/completionx` 这类不匹配。
	rest := p[idx+len("chat/completion"):]
	return rest == "" || strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "s")
}

// sseJSON 是聊天 SSE data 行的宽容 JSON 形状（覆盖 DeepSeek Web 与
// OpenAI 兼容两种常见结构；字段缺失时保持零值）。
type sseJSON struct {
	Type  string `json:"type"`
	Delta struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		Content          string `json:"content"`
		ReasoningContent string `json:"reasoning_content"`
	} `json:"delta"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	FinishReason string `json:"finish_reason"`
	Content      string `json:"content"`
	// 通义千问（Qwen）Web 补全形态（2026-10-02 实测帧）：正文在
	// data.messages[] 里，mime_type="multi_load/iframe" 的那条按「前缀快照」
	// 递增（"4" → "42"，status processing→complete）；会话 ID 在顶层
	// communication.sessionid（契约 §9 取值来源②）。
	Data struct {
		Messages []struct {
			MimeType string `json:"mime_type"`
			Content  string `json:"content"`
			Status   string `json:"status"`
		} `json:"messages"`
	} `json:"data"`
	Communication struct {
		SessionID string `json:"sessionid"`
	} `json:"communication"`
}

// Sink 是单轮聊天流的解析器：按 SSE 事件边界（data: 行 + 空行）喂入，
// 输出增量块与累计快照。chunk 边界可能在任意字节处截断行，内部保留
// 未成行尾部（tail），跨 Feed 调用安全拼接。
type Sink struct {
	epoch       uint64
	content     strings.Builder
	reasoning   strings.Builder
	done        bool
	chunks      []Chunk
	pending     []string          // 当前事件的 data 行
	tail        []byte            // 未成行的尾部字节
	frags       []sseFragment     // 新版补丁协议片段（保序）
	fragIdx     map[int]int       // fragment id → frags 下标
	lastFrag    int               // 最近一次显式 APPEND 的 fragment id（-1 = 最后一个）
	lastFragSet bool              // 是否已确立补丁路径（隐式增量沿它继续）
	ndjson      bool              // Gemini batchexecute NDJSON 模式（长度前缀帧）
	qwenText    string            // 千问正文帧的最新前缀快照（该站按「累计快照」而非增量下发）
	qwenFrames  bool              // 是否已见过千问正文帧（event:complete 只在千问帧后当完成信号）
	geminiText  map[string]string // rc_<candidate> → 累计文本（增量去重）
	convID      string            // 站点在响应体里自报的会话 ID（首个非空；原始读数）
}

func NewSink(epoch uint64) *Sink {
	return &Sink{epoch: epoch}
}

// Feed 喂入一段原始字节（支持 SSE 与 Gemini NDJSON 增量帧），处理完整行并保留尾部。
func (s *Sink) Feed(b []byte) {
	s.feed(b)
}

// feed 喂入一段原始字节（base64 解码后的 SSE 流），处理完整行并保留尾部。
func (s *Sink) feed(b []byte) {
	if s.done {
		return
	}
	s.tail = append(s.tail, b...)
	for {
		idx := bytes.IndexByte(s.tail, '\n')
		if idx < 0 {
			break
		}
		line := s.tail[:idx]
		s.tail = s.tail[idx+1:]
		s.feedLine(line)
	}
}

func (s *Sink) feedLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if !s.ndjson {
		// 自动识别 Gemini NDJSON：anti-XSSI 头或 batchexecute 帧签名。
		t := bytes.TrimSpace(line)
		if bytes.HasPrefix(t, []byte(")]}'")) ||
			bytes.HasPrefix(t, []byte(`[["wrb.fr"`)) ||
			bytes.HasPrefix(t, []byte(`[["di"`)) ||
			bytes.HasPrefix(t, []byte(`[["e"`)) {
			s.ndjson = true
		}
	}
	if s.ndjson {
		s.feedGeminiLine(line)
		return
	}
	if len(line) == 0 {
		s.flushEvent()
		return
	}
	if bytes.HasPrefix(line, []byte("event:")) {
		// 千问在收尾发 `event:complete`；只在已见正文帧时才当完成信号，
		// 避免其它站点的同名事件误触（DeepSeek 用 event:ready/answer_finished）。
		if s.qwenFrames && strings.TrimSpace(string(line[len("event:"):])) == "complete" {
			s.markDone()
		}
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		v := strings.TrimSpace(string(line[len("data:"):]))
		s.pending = append(s.pending, v)
	}
	// 忽略 event:/id:/retry: 与注释行——聊天补全只消费 data。
}

// flushEvent 完成一个 SSE 事件：拼接 data 行（多行 JSON）→ 解析增量。
func (s *Sink) flushEvent() {
	if len(s.pending) == 0 {
		return
	}
	data := strings.Join(s.pending, "\n")
	s.pending = nil
	if data == "[DONE]" {
		s.markDone()
		return
	}
	if s.ingestPatch(data) {
		return
	}
	var ev sseJSON
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return // 非 JSON 事件（心跳/注释）跳过
	}
	content, reasoning := extractDeltas(&ev)
	if content != "" {
		s.content.WriteString(content)
		s.append(Chunk{Content: content})
	}
	if reasoning != "" {
		s.reasoning.WriteString(reasoning)
		s.append(Chunk{Reasoning: reasoning})
	}
	if ev.Type == "answer_finished" || ev.FinishReason != "" ||
		(len(ev.Choices) > 0 && ev.Choices[0].FinishReason != "") {
		s.markDone()
	}
	if ev.Communication.SessionID != "" {
		s.setConversationID(ev.Communication.SessionID)
	}
	if snap, ok := qianwenAnswer(&ev); ok {
		s.qwenFrames = true
		// 站点按「累计快照」下发（"4" → "42"）：只追加新增前缀，重复帧不重播。
		if delta := strings.TrimPrefix(snap, s.qwenText); delta != "" {
			s.content.WriteString(delta)
			s.append(Chunk{Content: delta})
		}
		s.qwenText = snap
	}
}

// qianwenAnswer 从千问帧里取正文快照。判据＝data.messages 里
// mime_type="multi_load/iframe" 的那条（正文载体；signal/bar/paa/survey 非正文）。
// 返回值 ok=false 表示这不是千问正文帧。
func qianwenAnswer(ev *sseJSON) (string, bool) {
	if ev == nil || len(ev.Data.Messages) == 0 {
		return "", false
	}
	for _, m := range ev.Data.Messages {
		if m.MimeType == "multi_load/iframe" {
			return m.Content, true
		}
	}
	return "", false
}

// setConversationID 记录站点自报的会话 ID（首个非空生效，重复帧不覆盖）。
func (s *Sink) setConversationID(raw string) {
	v := strings.TrimSpace(raw)
	if v == "" || s.convID != "" {
		return
	}
	s.convID = v
}

func (s *Sink) append(c Chunk) {
	c.Seq = uint64(len(s.chunks))
	s.chunks = append(s.chunks, c)
}

func (s *Sink) markDone() {
	if s.done {
		return
	}
	s.done = true
	s.append(Chunk{Done: true})
}

// finish 在 Network.loadingFinished 时调用：无显式结束标记也视为流结束
// （快照 Done 供协调器判定完成；后续同轮增量不再接受）。
func (s *Sink) finish() {
	s.feedLine(nil) // 若尾部残留空行，先冲洗挂起事件
	s.markDone()
}

// snapshot 返回累计快照（调用方持锁或单 goroutine 场景）。
func (s *Sink) Snapshot() Snapshot {
	return Snapshot{
		Content:        s.content.String(),
		Reasoning:      s.reasoning.String(),
		Done:           s.done,
		Epoch:          s.epoch,
		ConversationID: s.convID,
	}
}

// drain 返回并清空增量块（读后清零语义，与 NavigationState 一致）。
func (s *Sink) Drain() []Chunk {
	if len(s.chunks) == 0 {
		return nil
	}
	out := append([]Chunk(nil), s.chunks...)
	s.chunks = nil
	return out
}

// extractDeltas 从 SSE JSON 中提取 content / reasoning 增量（宽容多形态）：
//   - delta.content / delta.text（DeepSeek Web：delta.type=text_delta 时 text 为正文）
//   - delta.reasoning_content / delta.type=reasoning_delta 时 text 为思考链
//   - choices[].delta.content / reasoning_content（OpenAI 兼容）
//   - 顶层 content（个别实现的兜底）
func extractDeltas(ev *sseJSON) (content, reasoning string) {
	if ev == nil {
		return "", ""
	}
	switch ev.Delta.Type {
	case "reasoning_delta":
		if ev.Delta.Text != "" {
			reasoning = ev.Delta.Text
		}
	default:
		if ev.Delta.Content != "" {
			content = ev.Delta.Content
		} else if ev.Delta.Text != "" {
			content = ev.Delta.Text
		}
	}
	if ev.Delta.ReasoningContent != "" {
		reasoning = ev.Delta.ReasoningContent
	}
	if content == "" && len(ev.Choices) > 0 {
		content = ev.Choices[0].Delta.Content
		if reasoning == "" {
			reasoning = ev.Choices[0].Delta.ReasoningContent
		}
	}
	if content == "" {
		content = ev.Content
	}
	return content, reasoning
}

// Stream 是 cdpSession 内的聊天 SSE 状态机：
//   - 每个新的 chat completion 请求（requestWillBeSent 命中）开启新轮
//     （Epoch+1、重置 sink、记录 requestId）；
//   - streamResourceContent 武装与 dataReceived 增量可能并发到达：武装完成前
//     的增量先进 preFeed 队列，武装完成后按「bufferedData 先、增量后」顺序
//     喂入，保证字节序不翻转；
//   - loadingFinished / loadingFailed 收尾。
type Stream struct {
	mu      sync.Mutex
	epoch   uint64
	active  string // 当前捕获中的 requestId；空 = 无在途捕获
	armed   bool
	failed  bool // 最近一轮以失败收尾（loadingFailed/中止/拉取失败）：SSE 快照不可信
	preFeed [][]byte
	sink    *Sink
}

func NewStream() *Stream {
	return &Stream{}
}

// onRequestStart 开启新一轮捕获（同一时刻只追踪最新请求）。
func (c *Stream) OnRequestStart(reqID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.active = reqID
	c.armed = false
	c.failed = false
	c.preFeed = nil
	c.sink = NewSink(c.epoch)
}

// onBuffered 接收 streamResourceContent 的已缓冲字节（武装完成）。
func (c *Stream) OnBuffered(reqID string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != reqID || c.armed {
		return
	}
	c.armed = true
	if len(data) > 0 {
		c.sink.feed(data)
	}
	c.flushPreFeedLocked()
}

// onArmFailed 处理 streamResourceContent 不可用（旧版 Chrome）：放行 preFeed。
func (c *Stream) OnArmFailed(reqID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != reqID || c.armed {
		return
	}
	c.armed = true
	c.flushPreFeedLocked()
}

func (c *Stream) flushPreFeedLocked() {
	for _, b := range c.preFeed {
		c.sink.feed(b)
	}
	c.preFeed = nil
}

// onData 接收 Network.dataReceived 的 base64 增量字节。
func (c *Stream) OnData(reqID, b64 string) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(data) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != reqID || c.sink == nil {
		return
	}
	if !c.armed {
		c.preFeed = append(c.preFeed, data)
		return
	}
	c.sink.feed(data)
}

// onFinish 在 Network.loadingFinished 时标记本轮完成并停止捕获。
func (c *Stream) OnFinish(reqID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != reqID || c.sink == nil {
		return
	}
	// 武装可能尚未完成（streamResourceContent 响应慢于流结束）：先放行
	// preFeed 中的增量再收尾，避免数据滞留队列。
	c.flushPreFeedLocked()
	c.sink.finish()
	c.active = ""
	c.preFeed = nil
}

// needsBody 判断本轮是否「零增量收尾」——Gemini batchexecute NDJSON 的
// dataReceived/streamResourceContent 均不携带数据（响应整体缓冲），
// 收尾后只能靠 Network.getResponseBody 取全文。返回 true 时调用方应
// 异步拉取响应体并喂给 onBody，而不是直接 onFinish（否则快照永远为空）。
func (c *Stream) NeedsBody(reqID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != reqID || c.sink == nil {
		return false
	}
	snap := c.sink.Snapshot()
	return snap.Content == "" && snap.Reasoning == ""
}

// onBody 在收尾后喂入 Network.getResponseBody 的完整响应体（Gemini 路径或零增量补救）；
// 已有增量（DeepSeek 路径）时忽略，避免重复消费。
func (c *Stream) OnBody(reqID string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != reqID || c.sink == nil {
		return
	}
	if c.sink.Snapshot().Content != "" || c.sink.Snapshot().Reasoning != "" {
		return
	}
	c.flushPreFeedLocked()
	c.sink.done = false
	c.sink.feed(body)
	c.sink.finish()
	c.active = ""
	c.preFeed = nil
}

// onFail 在 Network.loadingFailed 时放弃本轮（不标记完成，避免脏快照）。
func (c *Stream) OnFail(reqID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == reqID {
		c.active = ""
		c.preFeed = nil
		c.failed = true
	}
}

func (c *Stream) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sink == nil {
		return Snapshot{}
	}
	snap := c.sink.Snapshot()
	snap.Failed = c.failed
	return snap
}

func (c *Stream) Drain() []Chunk {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sink == nil {
		return nil
	}
	return c.sink.Drain()
}
