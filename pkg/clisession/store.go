// Package clisession 是 WebTouch 对外 CLI 的本机 session lease。
//
// CLI 进程本身不持有浏览器。调用方带着 session_id 进来，状态落在本机
// lease 文件里（默认 ~/.uws/cli-sessions.json）。契约见
// docs/cli-session-contract-2026-10-31.md。
package clisession

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// ContractV1 是 stdout JSON 的契约名。
	ContractV1  = "webtouch.cli.session.v1"
	fileVersion = 1

	StateActive = "active"
	StateIdle   = "idle"
	StateClosed = "closed"

	RuntimeReady    = "ready"
	RuntimeSleeping = "sleeping"
	RuntimeReleased = "released"

	CodeInvalid         = "invalid_argument"
	CodeVaultRef        = "vault_ref_rejected"
	CodeProfileUnbound  = "profile_unbound"
	CodeProfileMismatch = "profile_mismatch"
	CodeExists          = "session_exists"
	CodeNotFound        = "session_not_found"
	CodeClosed          = "session_closed"
	CodeBusy            = "lease_busy"

	// DefaultIdleTTL 与网关 UWS_SESSION_IDLE_TTL 的 10 分钟对齐：
	// 超时后 runtime 记为 sleeping，lease 仍可恢复。
	DefaultIdleTTL = 10 * time.Minute
	// DefaultLeaseTTL 与引擎会话绑定的 24h 对齐。自上次活动起算。
	DefaultLeaseTTL = 24 * time.Hour
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Error 是可机器判别的 lease 错误。Message 不得包含凭据原文。
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// ProfileResolver 把账号解析到已经绑定的 Profile。实现不得创建新指纹。
type ProfileResolver interface {
	Resolve(account string) (string, error)
}

// CreateInput 是 session create 的参数。账号和档案都是引用，不是凭据。
type CreateInput struct {
	ID                 string
	Channel            string
	Account            string
	ProfileID          string
	SiteConversationID string
	Attach             string
	EngineAddr         string
	OwnerPID           int
}

// UseInput 复用一条 lease。OwnerPID 为 0 表示只刷新活动、不改持有者。
type UseInput struct {
	OwnerPID           int
	SiteConversationID *string
}

// Record 是落盘记录。不含 Cookie、密码或 API Key。
type Record struct {
	SessionID          string    `json:"session_id"`
	State              string    `json:"state"`
	Channel            string    `json:"channel,omitempty"`
	AccountRef         string    `json:"account_ref,omitempty"`
	ProfileID          string    `json:"profile_id,omitempty"`
	SiteConversationID string    `json:"site_conversation_id,omitempty"`
	Runtime            string    `json:"runtime"`
	LeaseEpoch         int       `json:"lease_epoch"`
	OwnerPID           int       `json:"owner_pid,omitempty"`
	Recoverable        bool      `json:"recoverable"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	LastActiveAt       time.Time `json:"last_active_at"`
	IdleAfter          time.Time `json:"idle_after"`
	ExpiresAt          time.Time `json:"expires_at"`
	Attach             string    `json:"attach,omitempty"`
	EngineAddr         string    `json:"engine_addr,omitempty"`
}

// HubRef 是 Hub 允许落账的全部字段。
type HubRef struct {
	Channel            string  `json:"channel"`
	SiteConversationID *string `json:"site_conversation_id"`
}

// View 是 CLI stdout 里单条 session 的稳定字段。
type View struct {
	SessionID          string    `json:"session_id"`
	State              string    `json:"state"`
	Channel            string    `json:"channel"`
	AccountRef         string    `json:"account_ref,omitempty"`
	ProfileID          string    `json:"profile_id,omitempty"`
	SiteConversationID *string   `json:"site_conversation_id"`
	Runtime            string    `json:"runtime"`
	LeaseEpoch         int       `json:"lease_epoch"`
	OwnerPID           int       `json:"owner_pid,omitempty"`
	Recoverable        bool      `json:"recoverable"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	IdleAfter          time.Time `json:"idle_after"`
	ExpiresAt          time.Time `json:"expires_at"`
	Attach             string    `json:"attach,omitempty"`
	EngineAddr         string    `json:"engine_addr,omitempty"`
	Hub                HubRef    `json:"hub"`
}

// Envelope 是单条命令的成功 JSON。
type Envelope struct {
	OK       bool   `json:"ok"`
	Contract string `json:"contract"`
	View
}

// ListEnvelope 是 list 的成功 JSON。
type ListEnvelope struct {
	OK       bool   `json:"ok"`
	Contract string `json:"contract"`
	Sessions []View `json:"sessions"`
}

// ErrorEnvelope 是失败 JSON。
type ErrorEnvelope struct {
	OK       bool   `json:"ok"`
	Contract string `json:"contract"`
	Error    struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type diskFile struct {
	Version int               `json:"version"`
	Entries map[string]Record `json:"entries"`
}

// Store 是本机 session lease。零值不可用，用 Open。
type Store struct {
	path     string
	mu       sync.Mutex
	now      func() time.Time
	alive    func(pid int) bool
	idleTTL  time.Duration
	leaseTTL time.Duration
	resolver ProfileResolver
	entries  map[string]Record
}

// DefaultPath 是 lease 文件路径。UWS_CLI_SESSION_FILE 优先，其次 UWS_ENGINE_DIR。
func DefaultPath() string {
	if v := strings.TrimSpace(os.Getenv("UWS_CLI_SESSION_FILE")); v != "" {
		return v
	}
	if dir := strings.TrimSpace(os.Getenv("UWS_ENGINE_DIR")); dir != "" {
		return filepath.Join(dir, "cli-sessions.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".uws", "cli-sessions.json")
	}
	return filepath.Join(home, ".uws", "cli-sessions.json")
}

// Open 打开或初始化 lease 文件。损坏的文件 fail-closed，不会被覆盖成空表。
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, &Error{Code: CodeInvalid, Message: "lease 路径为空"}
	}
	s := &Store{
		path:     path,
		now:      time.Now,
		alive:    pidAlive,
		idleTTL:  DefaultIdleTTL,
		leaseTTL: DefaultLeaseTTL,
		entries:  map[string]Record{},
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := s.withLock(func() error { return nil }); err != nil {
		return nil, err
	}
	return s, nil
}

// Path 返回落盘路径。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// SetResolver 注入账号→Profile 解析。nil 时，带账号的 create 一律 profile_unbound。
func (s *Store) SetResolver(r ProfileResolver) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.resolver = r
	s.mu.Unlock()
}

// SetClock 注入时钟（测试用）。
func (s *Store) SetClock(now func() time.Time) {
	if s == nil || now == nil {
		return
	}
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// SetAlive 注入进程存活判断（测试用）。
func (s *Store) SetAlive(fn func(int) bool) {
	if s == nil || fn == nil {
		return
	}
	s.mu.Lock()
	s.alive = fn
	s.mu.Unlock()
}

// SetTTL 覆盖空闲与租约时限。lease 必须不短于 idle。
func (s *Store) SetTTL(idle, lease time.Duration) error {
	if s == nil {
		return &Error{Code: CodeInvalid, Message: "store 为空"}
	}
	if idle <= 0 || lease < idle {
		return &Error{Code: CodeInvalid, Message: "lease TTL 必须不短于 idle TTL，且两者为正"}
	}
	s.mu.Lock()
	s.idleTTL = idle
	s.leaseTTL = lease
	s.mu.Unlock()
	return nil
}

// Create 分配一条 active lease。不会创建指纹。
func (s *Store) Create(in CreateInput) (View, error) {
	var view View
	err := s.withLock(func() error {
		rec, err := s.prepareCreate(in)
		if err != nil {
			return err
		}
		s.entries[rec.SessionID] = rec
		view = rec.View()
		return nil
	})
	return view, err
}

// Use 复用一条未关闭的 lease。空闲或崩溃回收后的 idle 会被唤醒。
func (s *Store) Use(id string, in UseInput) (View, error) {
	var view View
	err := s.withLock(func() error {
		rec, ok := s.entries[id]
		if !ok {
			return &Error{Code: CodeNotFound, Message: "session 不存在"}
		}
		if rec.State == StateClosed {
			return &Error{Code: CodeClosed, Message: "session 已关闭"}
		}
		if rec.OwnerPID > 0 && in.OwnerPID > 0 && rec.OwnerPID != in.OwnerPID && s.alive(rec.OwnerPID) {
			return &Error{Code: CodeBusy, Message: "session 租约仍由其他进程持有"}
		}
		now := s.now()
		woke := rec.State != StateActive
		took := false
		if in.OwnerPID > 0 && rec.OwnerPID != in.OwnerPID {
			rec.OwnerPID = in.OwnerPID
			took = true
		}
		if woke || took {
			rec.LeaseEpoch++
		}
		if in.SiteConversationID != nil {
			site := strings.TrimSpace(*in.SiteConversationID)
			if err := rejectSensitive(site); err != nil {
				return err
			}
			rec.SiteConversationID = site
		}
		rec.State = StateActive
		rec.Runtime = RuntimeReady
		rec.Recoverable = false
		rec.UpdatedAt = now
		rec.LastActiveAt = now
		rec.IdleAfter = now.Add(s.idleTTL)
		rec.ExpiresAt = now.Add(s.leaseTTL)
		s.entries[id] = rec
		view = rec.View()
		return nil
	})
	return view, err
}

// Close 释放租约。重复关闭仍成功；未知 id 报 session_not_found。
func (s *Store) Close(id string) (View, error) {
	var view View
	err := s.withLock(func() error {
		rec, ok := s.entries[id]
		if !ok {
			return &Error{Code: CodeNotFound, Message: "session 不存在"}
		}
		if rec.State != StateClosed {
			now := s.now()
			rec.State = StateClosed
			rec.Runtime = RuntimeReleased
			rec.Recoverable = false
			rec.OwnerPID = 0
			rec.UpdatedAt = now
			s.entries[id] = rec
		}
		view = rec.View()
		return nil
	})
	return view, err
}

// Lookup 读取一条 lease（含已关闭）。不存在时 ok 为 false。
func (s *Store) Lookup(id string) (View, bool, error) {
	var view View
	var found bool
	err := s.withLock(func() error {
		rec, ok := s.entries[id]
		if !ok {
			return nil
		}
		found = true
		view = rec.View()
		return nil
	})
	return view, found, err
}

// SetSiteConversation 把页面学到的站点对话 id 写回 lease。关闭的 lease 拒绝写入。
func (s *Store) SetSiteConversation(id, siteID string) error {
	return s.withLock(func() error {
		rec, ok := s.entries[id]
		if !ok {
			return &Error{Code: CodeNotFound, Message: "session 不存在"}
		}
		if rec.State == StateClosed {
			return &Error{Code: CodeClosed, Message: "session 已关闭"}
		}
		siteID = strings.TrimSpace(siteID)
		if err := rejectSensitive(siteID); err != nil {
			return err
		}
		rec.SiteConversationID = siteID
		rec.UpdatedAt = s.now()
		s.entries[id] = rec
		return nil
	})
}

// List 返回 lease。all 为 false 时隐藏已关闭记录。
func (s *Store) List(all bool) ([]View, error) {
	out := []View{}
	err := s.withLock(func() error {
		for _, rec := range s.entries {
			if !all && rec.State == StateClosed {
				continue
			}
			out = append(out, rec.View())
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].SessionID < out[j].SessionID
			}
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		})
		return nil
	})
	return out, err
}

// Reap 按 TTL 与死亡 owner 推进状态机。
func (s *Store) Reap() error {
	return s.withLock(func() error { return nil })
}

func (s *Store) prepareCreate(in CreateInput) (Record, error) {
	in.ID = strings.TrimSpace(in.ID)
	in.Channel = strings.TrimSpace(in.Channel)
	in.Account = strings.TrimSpace(in.Account)
	in.ProfileID = strings.TrimSpace(in.ProfileID)
	in.SiteConversationID = strings.TrimSpace(in.SiteConversationID)
	in.Attach = strings.TrimSpace(in.Attach)
	in.EngineAddr = strings.TrimSpace(in.EngineAddr)
	if err := rejectSensitive(in.ID, in.Channel, in.Account, in.ProfileID, in.SiteConversationID, in.Attach, in.EngineAddr); err != nil {
		return Record{}, err
	}
	if in.ID == "" {
		id, err := newSessionID()
		if err != nil {
			return Record{}, err
		}
		in.ID = id
	}
	if err := validateSessionID(in.ID); err != nil {
		return Record{}, err
	}
	if _, exists := s.entries[in.ID]; exists {
		return Record{}, &Error{Code: CodeExists, Message: "session_id 已存在"}
	}
	for _, name := range []struct{ k, v string }{
		{"channel", in.Channel},
		{"account", in.Account},
		{"profile", in.ProfileID},
		{"site_conversation_id", in.SiteConversationID},
		{"attach", in.Attach},
		{"engine_addr", in.EngineAddr},
	} {
		if err := validateText(name.k, name.v); err != nil {
			return Record{}, err
		}
	}
	profileID, err := s.resolveProfile(in.Account, in.ProfileID)
	if err != nil {
		return Record{}, err
	}
	if in.Account != "" {
		for _, ex := range s.entries {
			if ex.State == StateClosed || ex.AccountRef != in.Account {
				continue
			}
			if ex.ProfileID != profileID {
				return Record{}, &Error{Code: CodeProfileMismatch, Message: "同一账号已有未关闭会话绑定了另一份 Profile，拒绝静默更换指纹"}
			}
		}
	}
	now := s.now()
	return Record{
		SessionID:          in.ID,
		State:              StateActive,
		Channel:            in.Channel,
		AccountRef:         in.Account,
		ProfileID:          profileID,
		SiteConversationID: in.SiteConversationID,
		Runtime:            RuntimeReady,
		LeaseEpoch:         1,
		OwnerPID:           in.OwnerPID,
		CreatedAt:          now,
		UpdatedAt:          now,
		LastActiveAt:       now,
		IdleAfter:          now.Add(s.idleTTL),
		ExpiresAt:          now.Add(s.leaseTTL),
		Attach:             in.Attach,
		EngineAddr:         in.EngineAddr,
	}, nil
}

func (s *Store) resolveProfile(account, profileID string) (string, error) {
	if account == "" {
		return profileID, nil
	}
	if s.resolver == nil {
		return "", &Error{Code: CodeProfileUnbound, Message: "未配置身份解析，拒绝在未知绑定下创建会话"}
	}
	resolved, err := s.resolver.Resolve(account)
	if err != nil || strings.TrimSpace(resolved) == "" {
		if err == nil {
			err = errors.New("身份解析返回空 Profile")
		}
		return "", &Error{Code: CodeProfileUnbound, Message: err.Error()}
	}
	resolved = strings.TrimSpace(resolved)
	if profileID != "" && profileID != resolved {
		return "", &Error{Code: CodeProfileMismatch, Message: "传入的 Profile 与该账号已绑定的档案不一致"}
	}
	return resolved, nil
}

func (r Record) View() View {
	return View{
		SessionID:          r.SessionID,
		State:              r.State,
		Channel:            r.Channel,
		AccountRef:         r.AccountRef,
		ProfileID:          r.ProfileID,
		SiteConversationID: strPtr(r.SiteConversationID),
		Runtime:            r.Runtime,
		LeaseEpoch:         r.LeaseEpoch,
		OwnerPID:           r.OwnerPID,
		Recoverable:        r.Recoverable,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
		IdleAfter:          r.IdleAfter,
		ExpiresAt:          r.ExpiresAt,
		Attach:             r.Attach,
		EngineAddr:         r.EngineAddr,
		Hub: HubRef{
			Channel:            r.Channel,
			SiteConversationID: strPtr(r.SiteConversationID),
		},
	}
}

func (s *Store) withLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.reload(); err != nil {
		return err
	}
	s.reapInMemory()
	if err := fn(); err != nil {
		_ = s.flush()
		return err
	}
	return s.flush()
}

func (s *Store) reapInMemory() {
	now := s.now()
	for id, rec := range s.entries {
		if rec.State == StateClosed {
			if s.leaseTTL > 0 && !rec.UpdatedAt.IsZero() && now.Sub(rec.UpdatedAt) > s.leaseTTL {
				delete(s.entries, id)
			}
			continue
		}
		if rec.OwnerPID > 0 && !s.alive(rec.OwnerPID) {
			rec.OwnerPID = 0
			rec.State = StateIdle
			rec.Runtime = RuntimeSleeping
			rec.Recoverable = true
			rec.UpdatedAt = now
			s.entries[id] = rec
		}
		rec = s.entries[id]
		if !rec.ExpiresAt.IsZero() && now.After(rec.ExpiresAt) {
			rec.State = StateClosed
			rec.Runtime = RuntimeReleased
			rec.Recoverable = false
			rec.OwnerPID = 0
			rec.UpdatedAt = now
			s.entries[id] = rec
			continue
		}
		if rec.State == StateActive && !rec.IdleAfter.IsZero() && now.After(rec.IdleAfter) {
			rec.State = StateIdle
			rec.Runtime = RuntimeSleeping
			rec.Recoverable = true
			rec.UpdatedAt = now
			s.entries[id] = rec
		}
	}
}

func (s *Store) reload() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.entries = map[string]Record{}
		return nil
	}
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		s.entries = map[string]Record{}
		return nil
	}
	var doc diskFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("cli session lease 损坏（fail-closed，不覆盖）: %w", err)
	}
	if doc.Version != 0 && doc.Version != fileVersion {
		return fmt.Errorf("cli session lease 版本 %d 不受支持", doc.Version)
	}
	if doc.Entries == nil {
		doc.Entries = map[string]Record{}
	}
	s.entries = doc.Entries
	return nil
}

func (s *Store) flush() error {
	doc := diskFile{Version: fileVersion, Entries: s.entries}
	if doc.Entries == nil {
		doc.Entries = map[string]Record{}
	}
	buf, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	// WriteFile 受 umask 影响；强制 0600，避免 lease 被同机其他用户读到。
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) lockFile() (func(), error) {
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func newSessionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "wts_" + hex.EncodeToString(b[:]), nil
}

func validateSessionID(id string) error {
	if id == "default" {
		return &Error{Code: CodeInvalid, Message: "session_id default 是网关隐式槽，CLI 句柄请使用分配的 wts_ id"}
	}
	if !idPattern.MatchString(id) {
		return &Error{Code: CodeInvalid, Message: "session_id 只允许字母、数字、下划线与连字符"}
	}
	return nil
}

func validateText(name, v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 256 {
		return &Error{Code: CodeInvalid, Message: name + " 过长"}
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return &Error{Code: CodeInvalid, Message: name + " 含有控制字符"}
		}
	}
	if strings.ContainsAny(v, "/\\") && name != "attach" {
		return &Error{Code: CodeInvalid, Message: name + " 不允许路径分隔符"}
	}
	return nil
}

func rejectSensitive(values ...string) error {
	for _, v := range values {
		lower := strings.ToLower(v)
		if strings.Contains(v, "@vault:") || strings.Contains(lower, "begin private key") || strings.Contains(lower, "api_key=") {
			return &Error{Code: CodeVaultRef, Message: "session 参数拒绝凭据或 @vault: 引用；明文与引用都不得进入 argv 或 lease 文件"}
		}
	}
	return nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
