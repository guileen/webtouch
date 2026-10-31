package viewerhub

import (
	"bytes"
	"encoding/json"
	"strings"
)

// 观看壳 → 引擎的反向意图（2026-09-12）。
//
// 传输还是那一根 Unix socket，只是同一根线上现在有两个方向：
//
//	引擎 → 壳：帧 / 指针 / 脱敏动作（session.go 的 wireUpdate）
//	壳 → 引擎：用户意图（本文件）
//
// 意图不是自动化输入：它不带页面内容、不带页面坐标点击，只说「用户想把这个会话的
// 页面放大看」或「用户已经切回自己的工作了」。引擎收到后只搬动**这个会话自己的**
// 真窗口（见 window_intent.go），绝不注入页面、绝不代替用户点页面。
const (
	// IntentProtocolVersion 是反向行的协议版本；版本不认识的行一律丢弃。
	IntentProtocolVersion = 1
	// IntentKind 是反向行的 kind 标记（引擎 → 壳的帧行没有 kind）。
	IntentKind = "intent"
	// IntentResultKind 是引擎回执的 kind 标记。
	IntentResultKind = "intentAck"
	// IntentExpand：用户点了小窗，要真页面。
	IntentExpand = "expand"
	// IntentRaise：用户点小窗后的**动画末段**，壳说「现在把真窗口抬到前台」
	// （2026-09-24 口径，规范 §4.4 第 3 条）。它只做激活，不搬位置、不缩放、不回矩形。
	// expand 带 `deferRaise: true` 时引擎那一趟只摆窗口不激活，这条就是补上的那一步。
	IntentRaise = "raise"
	// IntentCollapse：壳要收回（用户切走了，或观看壳自己退出）。
	IntentCollapse = "collapse"
	// IntentClose：用户点了小窗上的 ✕ —— 壳已经真丢掉了这块面板（关掉不是收起，
	// 也不会被下一帧弹回来），引擎据此把这个会话静默回收。它**不**结束会话、**不**关
	// 那扇真窗口，也不搬动任何窗口：只是「这个人不想再看了」。
	IntentClose = "close"
	// IntentReasonViewerGone 是观看壳断开时引擎补发的收回原因。
	IntentReasonViewerGone = "viewerGone"
)

// WindowSize 是壳曾经要的展开尺寸。
//
// 2026-09-22 口径：展开 = 把这扇真窗口**抬到最前**，不搬位置、不缩放，所以这个尺寸
// 已经没有消费者了。留在协议上只为不跟老壳断连（老壳照旧带它，引擎解出来就丢掉）；
// 新壳不带它，也不该被当成畸形输入——见 DecodeIntent。
type WindowSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Intent 是一条 壳 → 引擎 的意图。
type Intent struct {
	Version int    `json:"version"`
	Kind    string `json:"kind,omitempty"`
	Intent  string `json:"intent"`
	Session string `json:"session"`
	// Reason 只有引擎自己补发的意图会带（viewerGone），观看壳发来的行不带。
	Reason string `json:"reason,omitempty"`
	// Size 只有 expand 需要：壳希望真窗口展开成多大。
	Size *WindowSize `json:"size,omitempty"`
	// DeferRaise 只有 expand 会带：壳要「先飞动画、末段再抬」，引擎这一趟就只摆窗口
	// 不激活（2026-09-24 口径）。老壳不带 → 引擎保持「收到 expand 就地抬」的老行为。
	DeferRaise bool `json:"deferRaise,omitempty"`
}

// DecodeIntent 解析一行观看壳意图。任何畸形/越界输入都返回 ok=false：观看壳是本地
// 客户端，但仍然是不可信输入源——引擎不能因为一行坏数据崩掉，更不能照着它乱动窗口。
func DecodeIntent(line []byte) (Intent, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return Intent{}, false
	}
	var in Intent
	if err := json.Unmarshal(trimmed, &in); err != nil {
		return Intent{}, false
	}
	if in.Version != IntentProtocolVersion {
		return Intent{}, false
	}
	if in.Kind != "" && in.Kind != IntentKind {
		return Intent{}, false
	}
	if strings.TrimSpace(in.Session) == "" {
		return Intent{}, false
	}
	switch in.Intent {
	case IntentExpand:
		// 尺寸不再参与判定：展开只把真窗口抬到最前（2026-09-22 口径）。老壳带的 size 解出来
		// 就丢掉，新壳不带也接受——2026-09-22 真机复验里「壳不发 size ⇒ 整条意图被当成畸形
		// 输入静默丢弃 ⇒ 点开只有回落面板、没有任何回执」就是这条校验造成的。
	case IntentCollapse, IntentClose, IntentRaise:
	default:
		return Intent{}, false
	}
	return in, true
}

// IntentResult 是引擎对一条意图的回执。观看壳据此决定「让位给真窗口」还是「回落到
// 自绘放大面板」，所以 ok=false 也必须回：沉默会让壳一直等在一个没有答案的状态里。
type IntentResult struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Intent  string `json:"intent"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	// WindowBounds 是引擎实际生效的窗口矩形（Chrome 坐标空间），壳用它做
	// 「用户点的是不是这扇真页面」的落点判定。
	WindowBounds *ViewerBoundsJSON `json:"windowBounds,omitempty"`
	// BrowserPID 是承载这个会话真页面的浏览器进程；壳用它区分「用户点的是我们
	// 那扇真页面」和「用户切回自己的应用了」（R4b 自动收回）。
	BrowserPID int `json:"browserPID,omitempty"`
}

// ViewerBoundsJSON 是引擎报给壳的生效矩形（Chrome 窗口坐标空间）。
type ViewerBoundsJSON struct {
	Left   int `json:"left"`
	Top    int `json:"top"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// EncodeIntentResult 把回执编成一行 JSON（含换行）。
func EncodeIntentResult(res IntentResult) ([]byte, error) {
	if res.Version == 0 {
		res.Version = IntentProtocolVersion
	}
	if res.Kind == "" {
		res.Kind = IntentResultKind
	}
	payload, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}
