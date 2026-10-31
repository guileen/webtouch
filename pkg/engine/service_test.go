// 引擎 gRPC 服务接线单测：TDD-API-01/02/03/04/06 + TDD-SEC-06 输入边界。
// 验收以本实现层为准；tests/integration/test_grpc_contract.py 为 gRPC 契约层双层验证。
package engine

import (
	"context"
	"net"
	"testing"
	"time"
	"uws/pkg/browser"
	"uws/pkg/fingerprint"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func testKeys() []APIKey {
	return []APIKey{
		{Key: "coordinator-key", Role: RoleCoordinator},
		{Key: "operator-key", Role: RoleOperator},
		{Key: "monitor-key", Role: RoleMonitor},
	}
}

type testEnv struct {
	client uwspb.UwsEngineClient
	conn   *grpc.ClientConn
	lis    *bufconn.Listener
	srv    *Server
	gs     *grpc.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWithOptions(t, Options{Keys: testKeys()})
}

func newTestEnvWithOptions(t *testing.T, opts Options) *testEnv {
	t.Helper()
	if len(opts.Keys) == 0 {
		opts.Keys = testKeys()
	}
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(
		grpc.UnaryInterceptor(srv.Auth().UnaryInterceptor),
		grpc.StreamInterceptor(srv.Auth().StreamInterceptor),
	)
	uwspb.RegisterUwsEngineServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(func() {
		gs.Stop()
		srv.Close()
	})

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &testEnv{client: uwspb.NewUwsEngineClient(conn), conn: conn, lis: lis, srv: srv, gs: gs}
}

func mdCtx(key string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "x-api-key", key)
}

func initCtx(t *testing.T, c uwspb.UwsEngineClient, key string) {
	t.Helper()
	_, err := c.InitializeContext(mdCtx(key), &uwspb.InitContextRequest{TaskId: "task-1"})
	if err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
}

// ── TDD-API-01：生命周期前置 ─────────────────────────────────────────────

func TestApi01LifecyclePrecondition(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "i1", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("未初始化应 FAILED_PRECONDITION，got %v", err)
	}

	initCtx(t, env.client, "coordinator-key")
	resp, err := env.client.ExecuteAction(mdCtx("coordinator-key"), &uwspb.ActionIntentRequest{
		IntentId: "i1", TargetSemantic: "登录", ExpectedAction: "click",
	})
	if err != nil {
		t.Fatalf("初始化后应成功: %v", err)
	}
	if !resp.Success {
		t.Errorf("动作应成功: %+v", resp)
	}
}

func TestImportStateValidatesBundleAndFailsClosedForUnsupportedSession(t *testing.T) {
	env := newTestEnv(t) // default simulated session deliberately has no secret-state capability.
	ctx := mdCtx("coordinator-key")
	initCtx(t, env.client, "coordinator-key")
	_, err := env.client.ImportState(ctx, &uwspb.ImportStateRequest{Bundle: &uwspb.StateBundle{
		ProfileId: "profile-a", CreatedAt: "not-a-time", CookiesJson: "[]",
	}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid bundle status = %v, want INVALID_ARGUMENT", err)
	}
	_, err = env.client.ImportState(ctx, &uwspb.ImportStateRequest{Bundle: &uwspb.StateBundle{
		ProfileId: "profile-a", CreatedAt: "2026-09-13T00:00:00Z", CookiesJson: "[]",
	}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("simulated unsupported import status = %v, want FAILED_PRECONDITION", err)
	}
}

func TestImportStateRequiresMatchingInitializedProfile(t *testing.T) {
	factory := &stateBundleFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), SessionFactory: factory})
	ctx := mdCtx("coordinator-key")
	init, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{TaskId: "state-import", ProfileId: proto.String("state-profile")})
	if err != nil {
		t.Fatal(err)
	}
	mismatch := &uwspb.StateBundle{ProfileId: "other-profile", CreatedAt: "2026-09-13T00:00:00Z", CookiesJson: "[]"}
	if _, err := env.client.ImportState(ctx, &uwspb.ImportStateRequest{TargetId: init.GetTargetId(), Bundle: mismatch}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("mismatched profile import status = %v, want FAILED_PRECONDITION", err)
	}
	matching := &uwspb.StateBundle{
		ProfileId: "state-profile", CreatedAt: "2026-09-13T00:00:00Z", CookiesJson: "[]",
		Origins: []*uwspb.StorageOriginContext{{
			Origin:         "https://example.test",
			SessionStorage: []*uwspb.StorageOriginContext_KVPair{{Key: "tab", Value: "active"}},
		}},
	}
	resp, err := env.client.ImportState(ctx, &uwspb.ImportStateRequest{TargetId: init.GetTargetId(), Bundle: matching})
	if err != nil || !resp.GetOk() {
		t.Fatalf("matching profile import = %+v, %v", resp, err)
	}
	if factory.session.imported == nil || factory.session.imported.ProfileID != "state-profile" || factory.session.imported.Origins[0].SessionStorage[0].Value != "active" {
		t.Fatalf("session import = %+v", factory.session.imported)
	}
}

func TestImportStateRejectsRestoreAfterFirstNavigation(t *testing.T) {
	factory := &stateBundleFactory{}
	env := newTestEnvWithOptions(t, Options{Keys: testKeys(), SessionFactory: factory})
	ctx := mdCtx("coordinator-key")
	init, err := env.client.InitializeContext(ctx, &uwspb.InitContextRequest{TaskId: "state-too-late", ProfileId: proto.String("state-profile")})
	if err != nil {
		t.Fatal(err)
	}
	factory.session.importErr = browser.ErrStateBundleTooLate
	_, err = env.client.ImportState(ctx, &uwspb.ImportStateRequest{TargetId: init.GetTargetId(), Bundle: &uwspb.StateBundle{
		ProfileId: "state-profile", CreatedAt: "2026-09-13T00:00:00Z", CookiesJson: "[]",
	}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("post-navigation import status = %v, want FAILED_PRECONDITION", err)
	}
}

type stateBundleImportSession struct {
	id        string
	imported  *fingerprint.StateBundle
	importErr error
}

func (s *stateBundleImportSession) ID() string { return s.id }
func (s *stateBundleImportSession) Probe() browser.ProbeResult {
	return browser.ProbeResult{UserAgent: "state-test", Timezone: "UTC"}
}
func (s *stateBundleImportSession) Navigate(context.Context, string) error { return nil }
func (s *stateBundleImportSession) SetCookie(browser.Cookie)               {}
func (s *stateBundleImportSession) Cookies() []browser.Cookie              { return nil }
func (s *stateBundleImportSession) ExportState(context.Context, string) (*browser.SerializedSessionState, error) {
	return &browser.SerializedSessionState{}, nil
}
func (s *stateBundleImportSession) ExportStateBundle(_ context.Context, profileID, _ string) (*fingerprint.StateBundle, error) {
	return fingerprint.NewStateBundle(profileID, time.Now())
}
func (s *stateBundleImportSession) ImportStateBundle(_ context.Context, bundle *fingerprint.StateBundle) error {
	if s.importErr != nil {
		return s.importErr
	}
	s.imported = bundle
	return nil
}
func (s *stateBundleImportSession) Close() error { return nil }

type stateBundleFactory struct{ session *stateBundleImportSession }

func (f *stateBundleFactory) Create(_ context.Context, _ browser.Config) (browser.CDPSession, error) {
	f.session = &stateBundleImportSession{id: "state-import-session"}
	return f.session, nil
}

func TestInitializeContextLeaseLifecycleRejectsSecondHolder(t *testing.T) {
	store, err := fingerprint.OpenLeaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := newTestEnvWithOptions(t, Options{Keys: testKeys(), LeaseStore: store, LeaseHolderID: "node-a", LeaseTTL: time.Second})
	second := newTestEnvWithOptions(t, Options{Keys: testKeys(), LeaseStore: store, LeaseHolderID: "node-b", LeaseTTL: time.Second})
	request := &uwspb.InitContextRequest{TaskId: "lease-task", ProfileId: proto.String("lease-profile")}
	if _, err := first.client.InitializeContext(mdCtx("coordinator-key"), request); err != nil {
		t.Fatal(err)
	}
	if _, err := second.client.InitializeContext(mdCtx("coordinator-key"), request); status.Code(err) != codes.Internal {
		t.Fatalf("second holder status = %v, want INTERNAL acquisition failure", err)
	}
}
