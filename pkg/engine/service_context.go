package engine

import (
	"context"
	"errors"
	"log"
	"strings"
	"uws/pkg/browser"
	"uws/pkg/fingerprint"
	uwspb "uws/proto/gen/go/proto/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── InitializeContext（TDD-API-01 生命周期前置 / TDD-ENG-01/02/09 接线） ──

func (s *Server) InitializeContext(ctx context.Context, req *uwspb.InitContextRequest) (*uwspb.InitContextResponse, error) {
	if strings.TrimSpace(req.GetTaskId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id required")
	}
	if req.GetRequireExistingSession() && (strings.TrimSpace(req.GetContextName()) == "" ||
		req.GetInitialUrl() != "" || strings.TrimSpace(req.GetMutexKey()) != "") {
		return nil, status.Error(codes.InvalidArgument,
			"require_existing_session requires a named session and forbids navigation/mutex options")
	}
	cfg := browser.Config{TaskID: req.GetTaskId()}
	if p := req.GetProxy(); p != nil {
		cfg.Proxy = &browser.Proxy{
			Host: p.GetHost(), Port: int(p.GetPort()),
			Username: p.GetUsername(), Password: p.GetPassword(),
		}
	}
	if v := req.GetViewport(); v != nil && v.GetWidth() > 0 && v.GetHeight() > 0 {
		cfg.Viewport = &browser.Viewport{
			Width: int(v.GetWidth()), Height: int(v.GetHeight()),
			DeviceScaleFactor: float64(v.GetDeviceScaleFactor()),
		}
		cfg.ViewportExplicit = true
	}
	if cfg.Viewport == nil {
		cfg.Viewport = &browser.Viewport{Width: 1920, Height: 1080, DeviceScaleFactor: 1}
	}
	cfg.Timezone = req.GetTimezone()
	if cfg.Timezone == "" {
		cfg.Timezone = "UTC"
	}
	cfg.AttachCDPURL = req.GetAttachCdpUrl()
	cfg.InitialURL = req.GetInitialUrl()
	cfg.ProfileID = req.GetProfileId()
	if raw := strings.TrimSpace(req.GetFingerprintProfileJson()); raw != "" {
		profile, err := fingerprint.Parse([]byte(raw))
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "fingerprint_profile_json 非法: %v", err)
		}
		if cfg.ProfileID != "" && cfg.ProfileID != profile.ProfileID {
			return nil, status.Errorf(codes.InvalidArgument, "profile_id %q 与 fingerprint profile_id %q 不一致", cfg.ProfileID, profile.ProfileID)
		}
		cfg.ProfileID = profile.ProfileID
		cfg.Fingerprint = profile
	}
	if lo := req.GetLaunchOverrides(); lo != nil {
		cfg.UserAgent = lo.GetUserAgent()
	}
	for _, c := range req.GetCookies() {
		cfg.Cookies = append(cfg.Cookies, browser.Cookie{
			Name: c.GetName(), Value: c.GetValue(), Domain: c.GetDomain(),
			Path: c.GetPath(), HTTPOnly: c.GetHttpOnly(), Secure: c.GetSecure(),
			Expires: c.GetExpires(),
		})
	}

	ckey := s.ckeyFor(ctx, req.GetContextName())

	// v1.5.6 会话续页：同一客户端 + 同一 context_name（Session ID）已存在且仍
	// 存活时，**复用同一标签页**而不是“释放旧页开新页”——uws run / goal 由此可
	// 跨命令续作同一页面（单步 DSL 不再在新空白 Tab 上找不到目标）。
	// 语义：显式 initial_url 时原地导航；无 initial_url 时原样继续当前页。
	s.mu.Lock()
	old := s.contexts[ckey]
	s.mu.Unlock()
	if old != nil {
		if req.GetRequireExistingSession() {
			// Read-only observation must not clear verifier state, rewrite the binding,
			// or attach the viewer panel. Return the active Target as-is.
			return s.initResponse(old), nil
		}
		s.dropPage(old.ID) // 清页面时序，避免上一任务的验证器状态跨任务复用
		s.rememberSession(req.GetContextName(), old, cfg.AttachCDPURL, cfg.ProfileID)
		if cfg.InitialURL != "" {
			if err := old.Navigate(ctx, cfg.InitialURL); err == nil {
				// 续页复用的是老上下文：推流器可能已经断开（引擎重启过）或已经
				// 静默收过面板，这里幂等地重新挂载，保证「续页 = 面板还在」。
				s.attachViewer(old, req.GetTaskId(), req.GetContextName())
				return s.initResponse(old), nil
			}
			// 导航失败视为旧会话失效（标签被关/进程异常）：清出槽位走新建。
			s.mu.Lock()
			delete(s.contexts, ckey)
			s.mu.Unlock()
			s.pool.Release(old.ID)
		} else {
			s.attachViewer(old, req.GetTaskId(), req.GetContextName())
			return s.initResponse(old), nil
		}
	}
	// 只读观察不能把不存在/失效的会话降级成新建 about:blank Target。
	// 活跃上下文已在上方原地续接；此处只接受 attach/profile 匹配的持久绑定，
	// 后续由 browser.RequireReuseTarget 再验证真实 Target 仍可接管。
	if req.GetRequireExistingSession() {
		if _, ok := s.resumableSession(req.GetContextName(), cfg.AttachCDPURL, cfg.ProfileID); !ok {
			return nil, status.Errorf(codes.FailedPrecondition,
				"session_not_found: cannot observe missing or expired session %q", req.GetContextName())
		}
	}

	// v1.5.7 跨重启续页（GLOBAL #118）：内存里没有活跃上下文，但上次把
	// 互斥任务（P1，2026-09-23 定案）：同互斥键的任务共享同一扇窗——后来者直接
	// 复用拥有者的上下文，且**忽略 initial_url**（不得把前一个任务正在用的页面
	// 导航走）；动作靠 BrowserContext 的互斥量天然串行。调用方自己有活跃上下文时
	// 走上面的续页分支（会话隔离优先），互斥键只在「无自有上下文」时生效。
	// 已知边界：两个同键任务**并发首次创建**时存在竞态（后登记者覆盖槽位），
	// 顺序调用（CLI 常态）不受影响；见 USER_ACTION.md 第 8 条。
	mkey := strings.TrimSpace(req.GetMutexKey())
	if mkey != "" {
		if owner := s.mutexOwner(mkey); owner != nil {
			s.dropPage(owner.ID) // 与续页一致：旧页面时序不跨任务复用
			s.mu.Lock()
			s.contexts[ckey] = owner
			s.mu.Unlock()
			s.rememberSession(req.GetContextName(), owner, cfg.AttachCDPURL, cfg.ProfileID)
			s.attachViewer(owner, req.GetTaskId(), req.GetContextName())
			return s.initResponse(owner), nil
		}
	}

	// v1.5.7 跨重启续页（GLOBAL #118）：内存里没有活跃上下文，但上次把
	// 「会话名 → Target」落过盘、目标还在同一个 Chrome 里 —— 直接接管那扇窗口
	// 接着干，而不是再开一扇新的（「同一任务反复开新窗口」的根因就在这里）。
	// 持久绑定目标失效时必须失败关闭，不能把同一 session 落到其他标签。
	if b, ok := s.resumableSession(req.GetContextName(), cfg.AttachCDPURL, cfg.ProfileID); ok {
		cfg.ReuseTargetID = b.TargetID
		cfg.ReuseOwnWindow = b.OwnWindow
		cfg.RequireReuseTarget = true
	}

	bc, err := s.pool.Acquire(ctx, cfg)
	if err != nil {
		if errors.Is(err, browser.ErrResourceExhausted) {
			return nil, status.Error(codes.ResourceExhausted, "target pool exhausted")
		}
		return nil, status.Errorf(codes.Internal, "acquire target: %v", err)
	}
	if !req.GetRequireExistingSession() {
		s.dropPage(bc.ID) // 新 Target = 新页面时序（旧验证器状态不跨任务复用）
	}
	if cfg.InitialURL != "" {
		if err := bc.Navigate(ctx, cfg.InitialURL); err != nil {
			s.pool.Release(bc.ID)
			return nil, status.Errorf(codes.Internal, "initial navigation: %v", err)
		}
	}

	s.mu.Lock()
	s.contexts[ckey] = bc
	s.mu.Unlock()
	// 先到先得：只有本次真正新建/接管了上下文时才登记为拥有者。
	if !req.GetRequireExistingSession() {
		s.claimMutex(mkey, ckey, bc)
		s.rememberSession(req.GetContextName(), bc, cfg.AttachCDPURL, cfg.ProfileID)
		s.attachViewer(bc, req.GetTaskId(), req.GetContextName())
	}
	return s.initResponse(bc), nil
}

// rememberSession 把「会话名 → Target」绑定落盘（v1.5.7 会话续页，GLOBAL #118）。
// 只对命名会话生效：匿名上下文按连接寻址，跨引擎进程没有意义。未启用存储时是
// 空操作；落盘失败只记日志——续页是优化，不是本次任务正确性的前提。
func (s *Server) rememberSession(name string, bc *browser.BrowserContext, attach, profileID string) {
	if s.sessions == nil || name == "" || bc == nil {
		return
	}
	targetID := contextTargetID(bc)
	if targetID == "" {
		return
	}
	own := false
	if wr, ok := bc.Session().(browser.WindowReporter); ok {
		own = wr.OwnWindow()
	}
	b := SessionBinding{
		Session:   name,
		TargetID:  targetID,
		URL:       sessionPageURL(bc.Session()),
		Attach:    attach,
		ProfileID: profileID,
		OwnWindow: own,
	}
	if err := s.sessions.Save(b); err != nil {
		log.Printf("engine: 会话 %q 的 Target 绑定落盘失败（续页能力降级，不影响本次任务）: %v", name, err)
	}
}

// resumableSession 返回可用于续页的落盘绑定。要求同一个浏览器端点与同一身份：
// 换了附着对象（--attach default 等）时 targetId 不再指向同一扇窗，必须视为失效。
// 目标本身是否还在由 browser 侧接管时校验。
func (s *Server) resumableSession(name, attach, profileID string) (SessionBinding, bool) {
	if s.sessions == nil || name == "" || attach == "" {
		return SessionBinding{}, false
	}
	b, ok := s.sessions.Lookup(name)
	if !ok || b.TargetID == "" {
		return SessionBinding{}, false
	}
	if b.Attach != attach || b.ProfileID != profileID {
		return SessionBinding{}, false
	}
	return b, true
}

// ── 互斥键（P1 同站点跨 session 复用，2026-09-23 定案） ──

// mutexOwner 返回互斥键当前活跃的拥有者上下文（无则 nil）。
func (s *Server) mutexOwner(key string) *browser.BrowserContext {
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutexSlots[key].bc
}

// claimMutex 登记「互斥键 → 拥有者上下文」。先到先得；同上下文重复登记幂等
// （会话续页时每次都会走到这里）。
func (s *Server) claimMutex(key, ownerCKey string, bc *browser.BrowserContext) {
	if key == "" || bc == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutexSlots == nil {
		s.mutexSlots = make(map[string]mutexSlot)
	}
	s.mutexSlots[key] = mutexSlot{bc: bc, owner: ownerCKey}
}

// initResponse 构建 InitializeContext 响应；真实 CDP 会话暴露真实 targetId
// （否则客户端拿到的是 Pool 内部 ID ctx-N，误导调试与对拍；2026-08-27 前置）。
// 模拟会话未实现 TargetDebugInfo 时回退 bc.ID，行为零变化。
func (s *Server) initResponse(bc *browser.BrowserContext) *uwspb.InitContextResponse {
	targetID := bc.ID
	sessionURL := ""
	if dbg, ok := bc.Session().(browser.TargetDebugInfo); ok {
		if tid := dbg.TargetID(); tid != "" {
			targetID = tid
		}
		sessionURL = dbg.DebugPageURL()
	}
	// 记录客户端钉扎值：新窗口接管后会话作用面前移，但调用方手里仍是这个值
	// （续页/钉扎校验必须两者都接受，见 contextMatchesTarget）。
	bc.PinTarget(targetID)

	return &uwspb.InitContextResponse{
		TargetId:   targetID,
		ContextId:  bc.ID,
		SessionUrl: sessionURL,
		Ok:         true,
		// v1.5.6：续页语义下协调器需要知道“续到的是哪一页”（可能不是 about:blank），
		// 否则第一轮快照前 URL 恒为空，VLM 会把真实页面误读成空白/别的站点。
		CurrentUrl: sessionPageURL(bc.Session()),
	}
}

// ── ExportState ──

func (s *Server) ExportState(ctx context.Context, req *uwspb.ExportStateRequest) (*uwspb.ExportStateResponse, error) {
	key := s.auth.ClientKey(ctx)
	s.mu.Lock()
	bc := s.findContextLocked(key, req.GetTargetId())
	hasAny := s.hasAnyContextLocked(key)
	s.mu.Unlock()
	if bc == nil {
		if req.GetTargetId() != "" && hasAny {
			return nil, status.Errorf(codes.NotFound, "Target %s 未初始化或已销毁", req.TargetId)
		}
		return nil, status.Error(codes.FailedPrecondition, "未初始化浏览器上下文")
	}

	cdpSess := bc.Session()
	if cdpSess == nil {
		return nil, status.Error(codes.FailedPrecondition, "CDP 尚未初始化")
	}

	state, err := cdpSess.ExportState(ctx, req.TargetOrigin)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "状态导出失败: %v", err)
	}

	resp := &uwspb.ExportStateResponse{
		CookiesJson: state.CookiesJSON,
	}
	for _, o := range state.Origins {
		originPb := &uwspb.StorageOriginContext{
			Origin: o.Origin,
		}
		for _, kv := range o.LocalStorage {
			if k, ok := kv["key"].(string); ok {
				if v, ok := kv["value"].(string); ok {
					originPb.LocalStorage = append(originPb.LocalStorage, &uwspb.StorageOriginContext_KVPair{
						Key:   k,
						Value: v,
					})
				}
			}
		}
		for _, kv := range o.SessionStorage {
			if k, ok := kv["key"].(string); ok {
				if v, ok := kv["value"].(string); ok {
					originPb.SessionStorage = append(originPb.SessionStorage, &uwspb.StorageOriginContext_KVPair{
						Key: k, Value: v,
					})
				}
			}
		}
		resp.Origins = append(resp.Origins, originPb)
	}

	return resp, nil
}

// ImportState restores a logically exported StateBundle into an already
// initialized clean browser context. The caller must invoke this before the
// first Navigate; the CDP implementation writes cookies now and registers a
// new-document script for origin-scoped LocalStorage and same-tab SessionStorage.
func (s *Server) ImportState(ctx context.Context, req *uwspb.ImportStateRequest) (*uwspb.ImportStateResponse, error) {
	if req.GetBundle() == nil {
		return nil, status.Error(codes.InvalidArgument, "state bundle required")
	}
	key := s.auth.ClientKey(ctx)
	s.mu.Lock()
	bc := s.findContextLocked(key, req.GetTargetId())
	hasAny := s.hasAnyContextLocked(key)
	s.mu.Unlock()
	if bc == nil {
		if req.GetTargetId() != "" && hasAny {
			return nil, status.Errorf(codes.NotFound, "Target %s 未初始化或已销毁", req.TargetId)
		}
		return nil, status.Error(codes.FailedPrecondition, "未初始化浏览器上下文")
	}
	pb := req.GetBundle()
	bundle := &fingerprint.StateBundle{
		Schema:    fingerprint.StateBundleSchemaV1,
		ProfileID: pb.GetProfileId(),
		CreatedAt: pb.GetCreatedAt(),
		Cookies:   []byte(pb.GetCookiesJson()),
	}
	for _, origin := range pb.GetOrigins() {
		out := fingerprint.StateBundleOrigin{Origin: origin.GetOrigin()}
		for _, kv := range origin.GetLocalStorage() {
			out.LocalStorage = append(out.LocalStorage, fingerprint.StateBundleKVPair{Key: kv.GetKey(), Value: kv.GetValue()})
		}
		for _, kv := range origin.GetSessionStorage() {
			out.SessionStorage = append(out.SessionStorage, fingerprint.StateBundleKVPair{Key: kv.GetKey(), Value: kv.GetValue()})
		}
		bundle.Origins = append(bundle.Origins, out)
	}
	if err := bundle.Validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "state bundle 非法: %v", err)
	}
	if profileID := bc.ProfileID(); profileID == "" {
		return nil, status.Error(codes.FailedPrecondition, "State Bundle 恢复必须在带 profile_id 的干净上下文中执行")
	} else if profileID != bundle.ProfileID {
		return nil, status.Errorf(codes.FailedPrecondition, "state bundle profile_id %q 与当前上下文 profile_id %q 不一致", bundle.ProfileID, profileID)
	}
	session, ok := bc.Session().(browser.StateBundleSession)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "当前浏览器会话不支持 State Bundle 恢复")
	}
	if err := session.ImportStateBundle(ctx, bundle); err != nil {
		if errors.Is(err, browser.ErrStateBundleTooLate) {
			return nil, status.Error(codes.FailedPrecondition, "State Bundle 必须在首次导航前导入")
		}
		return nil, status.Errorf(codes.Internal, "状态恢复失败: %v", err)
	}
	return &uwspb.ImportStateResponse{Ok: true}, nil
}
