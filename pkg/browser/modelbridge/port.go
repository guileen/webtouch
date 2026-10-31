// Package modelbridge 是「站点 LLM 模型桥」子包（复杂度治理阶段 2）：
// 收口网页端模型/模式切换的物理动作与聊天流的网络层解析。
//
// 边界：本包只做站点语义（模型菜单定位、档位匹配、SSE/NDJSON 解析与流状态机），
// 不持有 CDP 会话。宿主 `*cdpSession` 经 Session 窄接口注入（实测用到的全部能力
// 六项，不多不少）；鼠标事件「即将发出」的标记经 Dispatch 注入。
package modelbridge

import (
	"context"
	"encoding/json"
)

// Target 是页面元素几何描述（宿主 elementTarget 的同形子集）。
type Target struct {
	Found bool    `json:"found"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	Text  string  `json:"text"`
}

// Session 是模型桥对宿主的窄能力面。
type Session interface {
	EvalValue(ctx context.Context, expr string, dst any) error
	EvalBool(ctx context.Context, expr string) (bool, error)
	EvalResult(ctx context.Context, expr string) (string, error)
	ClickAt(ctx context.Context, t Target) error
	FindElementOpt(ctx context.Context, semantic, selector string, preferLast bool) (Target, error)
	Call(ctx context.Context, method string, params any, dst any) error
}

// Dispatch 是「鼠标事件即将发出」的标记面（宿主 dispatchTracker 的窄视图）。
// 宿主以可空指针实现，nil 时 Mark 为空操作。
type Dispatch interface{ Mark() }

// Chunk 是聊天流中的一个增量块（宿主 ChatStreamChunk 的同一实体）。
type Chunk struct {
	Content   string
	Reasoning string
	Done      bool
	Seq       uint64
}

// Snapshot 是最近一轮聊天流的累计快照（宿主 ChatStreamSnapshot 的同一实体）。
type Snapshot struct {
	Content   string
	Reasoning string
	Done      bool
	Failed    bool
	Epoch     uint64
	// ConversationID 是站点在响应体里自报的会话 ID（契约 §9 取值来源②）。
	// 原始读数（未归一化）——由调用方按站点模板校验后使用；读不到为空串。
	ConversationID string
}

// QuoteJS 生成 JS 字符串字面量（含引号/反斜杠转义）。宿主 quoteJS 委托到此。
func QuoteJS(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
