package gateway

import (
	"context"
	"net"
	"testing"

	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// fakeEngineServer 是 bufconn 上的最小 UwsEngine 实现（契约层测试）。
type fakeEngineServer struct {
	uwspb.UnimplementedUwsEngineServer
	initNames   []string
	initAttach  []string
	extractSSE  bool
	deadlineFor string
}

func (f *fakeEngineServer) InitializeContext(_ context.Context, req *uwspb.InitContextRequest) (*uwspb.InitContextResponse, error) {
	f.initNames = append(f.initNames, req.GetContextName())
	f.initAttach = append(f.initAttach, req.GetAttachCdpUrl())
	return &uwspb.InitContextResponse{Ok: true, TargetId: "tgt", ContextId: "ctx-" + req.GetContextName()}, nil
}

func (f *fakeEngineServer) ExecuteAction(_ context.Context, req *uwspb.ActionIntentRequest) (*uwspb.ActionResultResponse, error) {
	if req.GetTargetSemantic() == f.deadlineFor {
		return nil, status.Error(codes.DeadlineExceeded, "waiting for stable")
	}
	resp := &uwspb.ActionResultResponse{Success: true, IntentId: req.GetIntentId()}
	if req.GetTargetSemantic() == "chat_answer" && req.GetContextHint()["prefer_sse"] == "1" && f.extractSSE {
		resp.ExtractedData = proto.String("干净 SSE 文本")
		resp.ChatStreamDone = proto.Bool(true)
	}
	return resp, nil
}

func (f *fakeEngineServer) CloseContext(_ context.Context, _ *uwspb.CloseContextRequest) (*uwspb.CloseContextResponse, error) {
	return &uwspb.CloseContextResponse{Ok: true}, nil
}

func (f *fakeEngineServer) HealthCheck(_ context.Context, _ *uwspb.Empty) (*uwspb.HealthStatus, error) {
	return &uwspb.HealthStatus{Ok: true, Version: "v1.5.3", ActiveTargets: 1, MaxTargets: 8}, nil
}

func newBufconnClient(t *testing.T, server *fakeEngineServer) *GrpcEngineClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	uwspb.RegisterUwsEngineServer(srv, server)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}))
	if err != nil {
		t.Fatalf("拨号 bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return newGRPCEngineClient(conn, "uws_dev_key")
}

func TestGRPCEngineClientLifecycle(t *testing.T) {
	server := &fakeEngineServer{extractSSE: true}
	client := newBufconnClient(t, server)

	init, err := client.InitializeContext(context.Background(), "task-1", "https://chat.deepseek.com/", "s-1", "http://127.0.0.1:9222")
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	if !init.OK || init.ContextID != "ctx-s-1" {
		t.Fatalf("init=%+v", init)
	}
	if len(server.initAttach) != 1 || server.initAttach[0] != "http://127.0.0.1:9222" {
		t.Fatalf("attach_cdp_url 未透传: %v", server.initAttach)
	}
	resp, err := client.ExecuteAction(context.Background(), ActionRequest{
		IntentID: "intent-1", TargetSemantic: "chat_answer", ExpectedAction: "extract",
		ContextHint: map[string]string{"context": "s-1", "prefer_sse": "1"},
	})
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !resp.ChatStreamActive || !resp.ChatStreamDone || resp.ExtractedData != "干净 SSE 文本" {
		t.Fatalf("SSE extract 结果错误: %+v", resp)
	}
	if err := client.CloseContext(context.Background(), "s-1"); err != nil {
		t.Fatalf("CloseContext: %v", err)
	}
	hs, err := client.HealthCheck(context.Background())
	if err != nil || !hs.OK || hs.Version != "v1.5.3" {
		t.Fatalf("HealthCheck=%+v err=%v", hs, err)
	}
}

func TestGRPCEngineClientErrorMapping(t *testing.T) {
	server := &fakeEngineServer{deadlineFor: "send_button"}
	client := newBufconnClient(t, server)
	_, err := client.ExecuteAction(context.Background(), ActionRequest{
		IntentID: "i", TargetSemantic: "send_button", ExpectedAction: "click",
	})
	ee, ok := err.(*EngineError)
	if !ok || !ee.IsDeadlineExceeded() {
		t.Fatalf("期望 EngineError DEADLINE_EXCEEDED，实际 %v", err)
	}
}

func TestGRPCEngineClientDOMFallbackNoSSEField(t *testing.T) {
	server := &fakeEngineServer{extractSSE: false}
	client := newBufconnClient(t, server)
	resp, err := client.ExecuteAction(context.Background(), ActionRequest{
		IntentID: "i", TargetSemantic: "chat_answer", ExpectedAction: "extract",
		ContextHint: map[string]string{"prefer_sse": "1"},
	})
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if resp.ChatStreamActive || resp.ChatStreamDone {
		t.Fatalf("未接入 SSE 时 ChatStreamActive 应为 false: %+v", resp)
	}
}

func TestEngineErrorTargetDestroyed(t *testing.T) {
	ee := &EngineError{Code: codeAborted, Reason: "target_destroyed: Tab 被用户关闭"}
	if !ee.IsTargetDestroyed() {
		t.Fatal("target_destroyed 应被识别")
	}
	if !IsTargetDestroyed(ee) {
		t.Fatal("IsTargetDestroyed(EngineError) 应通过")
	}
	ce := &ChatError{Reason: "chat_input: target_destroyed"}
	if !IsTargetDestroyed(ce) {
		t.Fatal("IsTargetDestroyed(ChatError) 应通过")
	}
	if IsTargetDestroyed(&ChatError{Reason: "chat_input: 超时"}) {
		t.Fatal("无关错误不应识别为 target_destroyed")
	}
}
