package engine

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// indeterminateGrace 是「物理事件已派发但结果未知」的观察窗口。
//
// 它不是「等物理执行真的退出」的信号——CDP 命令一旦发出，我们无法从协议层确认
// Chrome 是否已经把它落成一次真实的鼠标事件。这是一个有界的保守近似：
// 窗口内同一 intent 的重试不再触发物理执行，只拿到明确的「结果未知」；
// 窗口过后，调用方（或用新 intent_id 的调用方）可以显式重试。
const indeterminateGrace = 15 * time.Second

// actionInterruptedErr 表示物理动作被调用方 ctx 中断（取消或超时）。
//
// dispatched=true 表示鼠标/键盘/导航事件在中断前已经派发出去：结果可能已经落地，
// 调用方不得盲目重放（同一 intent 的重放在引擎侧会被 indeterminateGrace 挡住）。
// dispatched=false 表示定位/等待阶段就中断了，没有任何副作用，可以安全重试。
type actionInterruptedErr struct {
	dispatched bool
	code       codes.Code
}

func (e *actionInterruptedErr) Error() string {
	if e.dispatched {
		return "physical_event_dispatched_outcome_unknown: 物理事件已派发，页面结果未知（取消不能撤销已发出的点击/导航）"
	}
	return "physical_action_not_dispatched: 物理动作在派发前中断，没有副作用，可安全重试"
}

// GRPCStatus 让 gRPC 直接采用这里的错误码与文案（status.FromError 会认这个方法）。
func (e *actionInterruptedErr) GRPCStatus() *status.Status {
	return status.New(e.code, e.Error())
}

// actionInterruptedError 依据 ctx 的中断原因构造错误：
// 调用方主动取消 → CANCELED；deadline 到点 → DEADLINE_EXCEEDED。
func actionInterruptedError(ctx context.Context, dispatched bool) error {
	code := codes.Canceled
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code = codes.DeadlineExceeded
	}
	return &actionInterruptedErr{dispatched: dispatched, code: code}
}
