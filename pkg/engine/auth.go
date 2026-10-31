package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Role 对应 spec/security-dx.md §1.2 的调用方角色。
type Role string

const (
	// RoleCoordinator 是协调器角色：可初始化上下文与执行动作。
	RoleCoordinator Role = "coordinator"
	// RoleOperator 是运维角色：可热更知识库与健康查询。
	RoleOperator Role = "operator"
	// RoleMonitor 是监控角色：仅健康与版本查询。
	RoleMonitor Role = "monitor"
)

// APIKey 是一条 API Key 配置；Permissions 为空时按角色默认权限。
type APIKey struct {
	Key         string   `json:"key"`
	Role        Role     `json:"role"`
	Permissions []string `json:"permissions,omitempty"`
}

// KeyFile 是 --api-key-file 的 JSON 格式。
type KeyFile struct {
	Keys []APIKey `json:"keys"`
}

// 角色默认权限（security-dx.md §1.2 示例 + InitializeContext 必要性扩展）。
var roleDefaults = map[Role][]string{
	RoleCoordinator: {
		"InitializeContext", "ExecuteAction", "ExecuteActionStream",
		"CaptureScreenshot", "ExportPageTree", "ExportState", "ImportState", "CloseContext", "HealthCheck", "GetKnowledgeVersion",
	},
	RoleOperator: {"ReloadKnowledge", "HealthCheck", "GetKnowledgeVersion"},
	RoleMonitor:  {"HealthCheck", "GetKnowledgeVersion"},
}

// Authenticator 校验 X-API-Key 元数据并做方法级权限裁决。
type Authenticator struct {
	byKey map[string]APIKey
}

// NewAuthenticator 构造鉴权器；keys 为空时使用开发内置 Key（零配置启动）。
func NewAuthenticator(keys []APIKey) (*Authenticator, error) {
	if len(keys) == 0 {
		keys = []APIKey{{Key: DevAPIKey, Role: RoleCoordinator}}
	}
	a := &Authenticator{byKey: make(map[string]APIKey, len(keys))}
	for _, k := range keys {
		if strings.TrimSpace(k.Key) == "" {
			return nil, fmt.Errorf("engine: empty api key")
		}
		if k.Role == "" {
			return nil, fmt.Errorf("engine: key %q missing role", k.Key)
		}
		if _, dup := a.byKey[k.Key]; dup {
			return nil, fmt.Errorf("engine: duplicate api key %q", k.Key)
		}
		if len(k.Permissions) == 0 {
			k.Permissions = roleDefaults[k.Role]
			if k.Permissions == nil {
				return nil, fmt.Errorf("engine: unknown role %q", k.Role)
			}
		}
		a.byKey[k.Key] = k
	}
	return a, nil
}

// LoadKeyFile 从 JSON 文件加载 API Key 列表。
func LoadKeyFile(path string) ([]APIKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read api key file: %w", err)
	}
	var kf KeyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, fmt.Errorf("parse api key file: %w", err)
	}
	return kf.Keys, nil
}

// ClientKey 返回当前 RPC 的客户端标识（API Key + 对端地址），
// 用于「每客户端一个当前上下文」的生命周期语义（TDD-API-01）。
func (a *Authenticator) ClientKey(ctx context.Context) string {
	key := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-api-key"); len(vals) > 0 {
			key = vals[0]
		}
	}
	addr := ""
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr = p.Addr.String()
	}
	return key + "|" + addr
}

// APIKey 返回请求携带的 API Key（不含连接地址）。
// v1.5.6 会话续页：具名上下文（Session ID）跨进程/跨连接按 API Key 全局寻址，
// 同一 Key 的 uws run/goal 可续作同一标签页；默认（空名）上下文仍按
// ClientKey（含连接地址）保持旧「每连接单上下文」隔离语义。
func (a *Authenticator) APIKey(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-api-key"); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

func (a *Authenticator) authenticate(ctx context.Context) (APIKey, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return APIKey{}, status.Error(codes.Unauthenticated, "missing X-API-Key")
	}
	vals := md.Get("x-api-key")
	if len(vals) == 0 {
		return APIKey{}, status.Error(codes.Unauthenticated, "missing X-API-Key")
	}
	k, ok := a.byKey[vals[0]]
	if !ok {
		return APIKey{}, status.Error(codes.Unauthenticated, "invalid X-API-Key")
	}
	return k, nil
}

// methodPermission 把 gRPC 方法名映射到权限名。
func methodPermission(method string) string {
	name := method
	if i := strings.LastIndexByte(method, '/'); i >= 0 {
		name = method[i+1:]
	}
	switch name {
	case "InitializeContext", "ExecuteAction", "ExecuteActionStream",
		"CaptureScreenshot", "ExportPageTree", "ExportState", "ImportState", "ReloadKnowledge", "HealthCheck", "GetKnowledgeVersion":
		return name
	case "CloseContext":
		return name
	}
	return ""
}

func (a *Authenticator) authorize(k APIKey, method string) error {
	perm := methodPermission(method)
	if perm == "" {
		return status.Error(codes.PermissionDenied, "unknown method")
	}
	for _, p := range k.Permissions {
		if p == perm {
			return nil
		}
	}
	return status.Errorf(codes.PermissionDenied, "role %s cannot call %s", k.Role, perm)
}

// UnaryInterceptor 是服务端一元拦截器（零信任：所有 RPC 必须先认证）。
func (a *Authenticator) UnaryInterceptor(ctx context.Context, req any,
	info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	k, err := a.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.authorize(k, info.FullMethod); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// StreamInterceptor 是服务端流拦截器。
func (a *Authenticator) StreamInterceptor(srv any, ss grpc.ServerStream,
	info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	k, err := a.authenticate(ss.Context())
	if err != nil {
		return err
	}
	if err := a.authorize(k, info.FullMethod); err != nil {
		return err
	}
	return handler(srv, ss)
}
