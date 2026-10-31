package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"uws/pkg/contract"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// gRPC 状态码常量（与 proto/uws_service.proto 末尾错误码映射表一致）。
const (
	codeInvalidArgument    = codes.InvalidArgument    // 3
	codeDeadlineExceeded   = codes.DeadlineExceeded   // 4
	codeNotFound           = codes.NotFound           // 5
	codeResourceExhausted  = codes.ResourceExhausted  // 8
	codeFailedPrecondition = codes.FailedPrecondition // 9
	codeAborted            = codes.Aborted            // 10
	codeUnimplemented      = codes.Unimplemented      // 12
	codeInternal           = codes.Internal           // 13
	codeUnavailable        = codes.Unavailable        // 14
	codeUnauthenticated    = codes.Unauthenticated    // 16
)

// EngineError 是引擎侧错误（携带 gRPC 状态码，协调层据此决策）。
type EngineError struct {
	Code   codes.Code
	Reason string
}

func (e *EngineError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("engine error code=%d: %s", e.Code, e.Reason)
	}
	return fmt.Sprintf("engine error code=%d", e.Code)
}

// IsDeadlineExceeded 判断是否为 DEADLINE_EXCEEDED（可 Fibonacci 退避重试）。
func (e *EngineError) IsDeadlineExceeded() bool { return e != nil && e.Code == codeDeadlineExceeded }

// IsTargetDestroyed 判断是否为标签页被关闭（用户手动关 Tab / Target.targetDestroyed）。
// 物理层显式返回 error_reason 含 "target_destroyed"（v1.5.3），API 层据此自动重建。
func (e *EngineError) IsTargetDestroyed() bool {
	return e != nil && strings.Contains(e.Reason, "target_destroyed")
}

// ActionRequest 是 ExecuteAction 入参（引擎 v1.5.3 语义）。
type ActionRequest struct {
	IntentID       string
	TargetSemantic string
	ExpectedAction string
	Value          string
	ContextHint    map[string]string
	TimeoutMS      int
}

// ActionResult 是 ExecuteAction 返回（引擎 v1.5.3 语义）。
type ActionResult struct {
	Success          bool
	ContextState     string
	Confidence       float32
	DecisionTraceID  string
	ErrorReason      string
	ExtractedData    string
	ChatStreamDone   bool
	ChatStreamActive bool // extract(prefer_sse) 时：SSE 通道已接入（HasField chat_stream_done）
	IntentID         string
}

// InitResult 是 InitializeContext 返回。
type InitResult struct {
	OK          bool
	TargetID    string
	ContextID   string
	ErrorReason string
}

// HealthStatus 是 HealthCheck 返回。
type HealthStatus struct {
	OK            bool
	Version       string
	ActiveTargets int32
	MaxTargets    int32
}

// EngineClient 是网关→引擎的客户端契约（移植 coordinator/engine_client.py）。
// 测试注入 FakeEngine；生产实现 GrpcEngineClient。
type EngineClient interface {
	InitializeContext(ctx context.Context, taskID, initialURL, contextName, attachURL string) (InitResult, error)
	InitializeContextWithProfile(ctx context.Context, taskID, initialURL, contextName, attachURL, profileID, fingerprintJSON string) (InitResult, error)
	ExecuteAction(ctx context.Context, req ActionRequest) (ActionResult, error)
	CloseContext(ctx context.Context, contextName string) error
	HealthCheck(ctx context.Context) (HealthStatus, error)
}

// GrpcEngineClient 是生产 gRPC 引擎客户端（对接 proto/gen/go 生成的桩）。
// 传输层默认明文（本地/内网，与引擎 --grpc-addr 默认一致）；
// 鉴权经 metadata x-api-key（security-dx.md §1.2）。
type GrpcEngineClient struct {
	stub   uwspb.UwsEngineClient
	apiKey string
}

// NewGRPCEngineClient 拨号并构造客户端。
func NewGRPCEngineClient(addr, apiKey string) (*GrpcEngineClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("拨号引擎 %s: %w", addr, err)
	}
	return newGRPCEngineClient(conn, apiKey), nil
}

// newGRPCEngineClient 以既有连接构造客户端（测试注入 bufconn）。
func newGRPCEngineClient(conn *grpc.ClientConn, apiKey string) *GrpcEngineClient {
	return &GrpcEngineClient{
		stub:   uwspb.NewUwsEngineClient(conn),
		apiKey: apiKey,
	}
}

func (c *GrpcEngineClient) ctx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-api-key", c.apiKey)
}

// InitializeContext 初始化命名上下文（委派至 InitializeContextWithProfile 空 profile）。
func (c *GrpcEngineClient) InitializeContext(ctx context.Context, taskID, initialURL, contextName, attachURL string) (InitResult, error) {
	return c.InitializeContextWithProfile(ctx, taskID, initialURL, contextName, attachURL, "", "")
}

// InitializeContextWithProfile 初始化命名上下文并注入独立沙箱与指纹档案。
// attachURL 非空时引擎附着到已运行的 Chrome（共享登录态，如桌面端 9222 调试 Chrome）。
func (c *GrpcEngineClient) InitializeContextWithProfile(ctx context.Context, taskID, initialURL, contextName, attachURL, profileID, fingerprintJSON string) (InitResult, error) {
	req := &uwspb.InitContextRequest{TaskId: taskID}
	if initialURL != "" {
		req.InitialUrl = &initialURL
	}
	if contextName != "" {
		req.ContextName = &contextName
	}
	if attachURL != "" {
		req.AttachCdpUrl = &attachURL
	}
	if profileID != "" {
		req.ProfileId = &profileID
	}
	if fingerprintJSON != "" {
		req.FingerprintProfileJson = &fingerprintJSON
	}
	resp, err := c.stub.InitializeContext(c.ctx(ctx), req)
	if err != nil {
		return InitResult{}, mapEngineError(err)
	}
	return InitResult{
		OK:          resp.GetOk(),
		TargetID:    resp.GetTargetId(),
		ContextID:   resp.GetContextId(),
		ErrorReason: resp.GetErrorReason(),
	}, nil
}

// ExecuteAction 单步原子动作；幂等与在途去重由引擎保证。
func (c *GrpcEngineClient) ExecuteAction(ctx context.Context, req ActionRequest) (ActionResult, error) {
	protoReq := &uwspb.ActionIntentRequest{
		IntentId:       req.IntentID,
		TargetSemantic: req.TargetSemantic,
		ExpectedAction: req.ExpectedAction,
		TimeoutMs:      int32(req.TimeoutMS),
	}
	if req.Value != "" {
		protoReq.Value = &req.Value
	}
	if len(req.ContextHint) > 0 {
		protoReq.ContextHint = make(map[string]string, len(req.ContextHint))
		for k, v := range req.ContextHint {
			protoReq.ContextHint[k] = v
		}
	}
	resp, err := c.stub.ExecuteAction(c.ctx(ctx), protoReq)
	if err != nil {
		return ActionResult{}, mapEngineError(err)
	}
	out := ActionResult{
		Success:          resp.GetSuccess(),
		ContextState:     contextStateName(resp.GetContextState()),
		Confidence:       resp.GetConfidence(),
		DecisionTraceID:  resp.GetDecisionTraceId(),
		ErrorReason:      resp.GetErrorReason(),
		ExtractedData:    resp.GetExtractedData(),
		ChatStreamActive: resp.ChatStreamDone != nil,
		ChatStreamDone:   resp.GetChatStreamDone(),
		IntentID:         resp.GetIntentId(),
	}
	return out, nil
}

// CloseContext 关闭命名上下文（释放 Target，标签页随之关闭）；幂等。
func (c *GrpcEngineClient) CloseContext(ctx context.Context, contextName string) error {
	_, err := c.stub.CloseContext(c.ctx(ctx), &uwspb.CloseContextRequest{ContextName: contextName})
	if err != nil {
		return mapEngineError(err)
	}
	return nil
}

// HealthCheck 引擎健康检查。
func (c *GrpcEngineClient) HealthCheck(ctx context.Context) (HealthStatus, error) {
	resp, err := c.stub.HealthCheck(c.ctx(ctx), &uwspb.Empty{})
	if err != nil {
		return HealthStatus{}, mapEngineError(err)
	}
	return HealthStatus{
		OK:            resp.GetOk(),
		Version:       resp.GetVersion(),
		ActiveTargets: resp.GetActiveTargets(),
		MaxTargets:    resp.GetMaxTargets(),
	}, nil
}

// mapEngineError 把 gRPC 错误映射为 EngineError（与 Python _map_error 一致）。
func mapEngineError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return &EngineError{Code: codeInternal, Reason: err.Error()}
	}
	switch st.Code() {
	case codeInvalidArgument, codeDeadlineExceeded, codeNotFound, codeResourceExhausted,
		codeFailedPrecondition, codeAborted, codeUnauthenticated, codeUnavailable, codeUnimplemented:
		return &EngineError{Code: st.Code(), Reason: st.Message()}
	default:
		return &EngineError{Code: codeInternal, Reason: st.Message()}
	}
}

// contextStateName 把 ContextState 枚举转为字符串（与 Python _CONTEXT_STATES 一致）。
func contextStateName(v uwspb.ContextState) string {
	switch v {
	case uwspb.ContextState_STABLE:
		return "STABLE"
	case uwspb.ContextState_NAVIGATING:
		return "NAVIGATING"
	case uwspb.ContextState_NAVIGATED:
		return "NAVIGATED"
	case uwspb.ContextState_NEW_WINDOW_OPENED:
		return "NEW_WINDOW_OPENED"
	case uwspb.ContextState_CHALLENGE_INTERCEPTED:
		return "CHALLENGE_INTERCEPTED"
	default:
		return "STABLE"
	}
}

// defaultEngineFactory 返回生产引擎客户端工厂；apiKey 为空时使用开发内置 Key
// （引擎侧 NewAuthenticator 空 keys 回退 uws_dev_key，与 Python 零配置行为一致）。
func defaultEngineFactory(apiKey string) func(addr string) (EngineClient, error) {
	return func(addr string) (EngineClient, error) {
		return NewGRPCEngineClient(addr, apiKey)
	}
}

// EngineScriptEvaluator 实现 contract.DirectScriptEvaluator 接口，
// 包装 EngineClient.ExecuteAction(ExpectedAction: "evaluate")，
// 使网关能通过 gRPC 统一对远程/本地引擎无缝执行页内脚本。
type EngineScriptEvaluator struct {
	client      EngineClient
	contextName string
}

var _ contract.DirectScriptEvaluator = (*EngineScriptEvaluator)(nil)

// NewEngineScriptEvaluator 构造 EngineScriptEvaluator。
func NewEngineScriptEvaluator(client EngineClient, contextName string) *EngineScriptEvaluator {
	return &EngineScriptEvaluator{
		client:      client,
		contextName: contextName,
	}
}

// EvaluateScript 在页面 Runtime 执行原生 JS 脚本并返回提取结果。
func (e *EngineScriptEvaluator) EvaluateScript(ctx context.Context, expr string) ([]byte, error) {
	req := ActionRequest{
		IntentID:       fmt.Sprintf("eval-%d", time.Now().UnixNano()),
		TargetSemantic: "direct_evaluate",
		ExpectedAction: "evaluate",
		Value:          expr,
		ContextHint:    map[string]string{"context": e.contextName},
		TimeoutMS:      30000,
	}
	res, err := e.client.ExecuteAction(ctx, req)
	if err != nil {
		return nil, err
	}
	if !res.Success {
		return nil, fmt.Errorf("evaluate 失败: %s", res.ErrorReason)
	}
	return []byte(res.ExtractedData), nil
}
