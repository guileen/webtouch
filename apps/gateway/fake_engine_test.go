package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// fakeCall 记录一次引擎动作调用（行为规格对照 tests/unit/test_api_server.py _RecordingEngine）。
type fakeCall struct {
	intent   string
	semantic string
	action   string
	value    string
	hints    map[string]string
}

// extractStep 是一次 extract 返回的计划步骤。
type extractStep struct {
	text   string
	done   bool
	active bool // chat_stream_done HasField（SSE 通道已接入）
}

// fakeEngine 是 EngineClient 测试夹具（模拟 Python _RetryEngine/_RecordingEngine/_DoneScriptedEngine）。
type fakeEngine struct {
	mu sync.Mutex

	initNames        []string
	initURLs         []string
	plainInitNames   []string
	profileInitNames []string
	closeNames       []string
	calls            []fakeCall

	// extractPlan 依次返回的 extract 结果；空 = 默认「确定性应答」（DOM 回退语义，
	// active=false 走文本稳定性完成判定，与 Python 默认 ActionResult 一致）。
	extractPlan []extractStep
	stepIndex   int
	// deadlineExtracts 是 extract 先失败 DEADLINE_EXCEEDED 的次数（重试语义）。
	deadlineExtracts int
	// failOnce 是「指定语义第一次调用返回错误」的注入（target_destroyed 恢复测试）。
	failOnce map[string]error
	// failAlways 是「指定语义每次调用都返回错误」的注入。
	failAlways map[string]error
	// initFailOnce 是「首次 InitializeContext 返回该错误」的注入（绑定失活自愈测试）。
	initFailOnce error
	// initFailTimes/initFailErr 是「接下来 initFailTimes 次 InitializeContext 返回该错误」的注入
	// （引擎短窗不可达 → 网关有界重拨测试）。
	initFailTimes int
	initFailErr   error
	// incrementAnswers 时每次 extract 返回 "确定性应答-N"（多轮基线不相等，模拟 Python _RecordingEngine）。
	incrementAnswers bool
	evaluateFunc     func(req ActionRequest) (ActionResult, error)
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		failOnce:   map[string]error{},
		failAlways: map[string]error{},
	}
}

func (f *fakeEngine) InitializeContext(_ context.Context, _, initialURL, contextName, _ string) (InitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.initNames = append(f.initNames, contextName)
	f.initURLs = append(f.initURLs, initialURL)
	f.plainInitNames = append(f.plainInitNames, contextName)
	if f.initFailOnce != nil {
		err := f.initFailOnce
		f.initFailOnce = nil
		return InitResult{}, err
	}
	if f.initFailTimes > 0 {
		f.initFailTimes--
		return InitResult{}, f.initFailErr
	}
	return InitResult{OK: true, TargetID: "tgt-" + contextName, ContextID: "ctx-" + contextName}, nil
}

func (f *fakeEngine) InitializeContextWithProfile(_ context.Context, _, initialURL, contextName, _, _, _ string) (InitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.initNames = append(f.initNames, contextName)
	f.initURLs = append(f.initURLs, initialURL)
	f.profileInitNames = append(f.profileInitNames, contextName)
	if f.initFailOnce != nil {
		err := f.initFailOnce
		f.initFailOnce = nil
		return InitResult{}, err
	}
	if f.initFailTimes > 0 {
		f.initFailTimes--
		return InitResult{}, f.initFailErr
	}
	return InitResult{OK: true, TargetID: "tgt-" + contextName, ContextID: "ctx-" + contextName}, nil
}

func (f *fakeEngine) ExecuteAction(_ context.Context, req ActionRequest) (ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.ExpectedAction == "evaluate" {
		if f.evaluateFunc != nil {
			f.calls = append(f.calls, fakeCall{
				intent: req.IntentID, semantic: req.TargetSemantic, action: req.ExpectedAction,
				value: req.Value, hints: req.ContextHint,
			})
			return f.evaluateFunc(req)
		}
		if strings.Contains(req.Value, "__uws_auth_probe") {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		return ActionResult{Success: true, ExtractedData: `{"status":"not_dispatched","error":"fake direct unavailable"}`}, nil
	}
	f.calls = append(f.calls, fakeCall{
		intent: req.IntentID, semantic: req.TargetSemantic, action: req.ExpectedAction,
		value: req.Value, hints: req.ContextHint,
	})
	if err := f.failAlways[req.TargetSemantic]; err != nil {
		return ActionResult{}, err
	}
	if err, ok := f.failOnce[req.TargetSemantic]; ok {
		delete(f.failOnce, req.TargetSemantic)
		return ActionResult{}, err
	}
	if strings.Contains(req.TargetSemantic, "answer") {
		if f.deadlineExtracts > 0 {
			f.deadlineExtracts--
			return ActionResult{}, &EngineError{Code: codeDeadlineExceeded, Reason: "waiting for stable"}
		}
		step := extractStep{text: "确定性应答"}
		if f.stepIndex < len(f.extractPlan) {
			step = f.extractPlan[f.stepIndex]
		}
		f.stepIndex++
		if f.incrementAnswers {
			step.text = fmt.Sprintf("确定性应答-%d", f.stepIndex)
		}
		return ActionResult{
			Success:          true,
			ExtractedData:    step.text,
			ChatStreamActive: step.active,
			ChatStreamDone:   step.done,
		}, nil
	}
	return ActionResult{Success: true}, nil
}

func (f *fakeEngine) CloseContext(_ context.Context, contextName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failAlways["close_context"]; err != nil {
		return err
	}
	f.closeNames = append(f.closeNames, contextName)
	return nil
}

func (f *fakeEngine) HealthCheck(_ context.Context) (HealthStatus, error) {
	return HealthStatus{OK: true, Version: "v1.5.3", ActiveTargets: 1, MaxTargets: 8}, nil
}

// initURLsSeen 返回 InitializeContext 收到的 initialURL 序列（T-31 S2 落点断言用）。
func (f *fakeEngine) initURLsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.initURLs))
	copy(out, f.initURLs)
	return out
}

// callKinds 返回已发生的 (semantic, action, value) 序列（测试断言用）。
func (f *fakeEngine) callKinds() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}
