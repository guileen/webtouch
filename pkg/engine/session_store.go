package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 会话续页持久化（v1.5.7；GLOBAL #118）
//
// 背景（2026-09-22 用户现场）：浏览器是持久的——调试 Chrome 常驻、标签页活得比引擎久——
// 但引擎的「会话名 → Target」映射只活在内存里。引擎一重启（每次发版的 daemon restart
// 都算），映射就没了：下一次同名会话只能再开一扇新窗口，同一任务反复开窗、占资源，
// 而用户「已经干到一半」的那个页面其实还在浏览器里躺着。
//
// 方案：把绑定落盘到 ~/.uws/sessions.json（0600；临时文件 + rename 原子替换）。
// 只记「哪个会话接哪扇窗」，不含 Cookie / 凭据 / 页面内容；绑定的目标已经不存在
// （用户关了窗、Chrome 重启过）时由调用方回退为新开窗口，绝不失败。
type SessionBinding struct {
	// Session 是逻辑会话名（CLI 的 --session，默认 default）。
	Session string `json:"session"`
	// TargetID 是真实 CDP targetId——续页的唯一凭据。
	TargetID string `json:"target_id"`
	// URL 是记下时的页面地址，只作诊断用，不参与判定。
	URL string `json:"url,omitempty"`
	// Attach 是当时附着的浏览器端点；换了浏览器（--attach default 等）
	// targetId 就不再指向同一扇窗，绑定必须视为失效。
	Attach string `json:"attach,omitempty"`
	// ProfileID 是当时的持久身份（空 = 临时身份）。
	ProfileID string `json:"profile_id,omitempty"`
	// OwnWindow 记录这扇窗是不是 uws 的专属小窗（接管时按原策略记回）。
	OwnWindow bool `json:"own_window,omitempty"`
	// UpdatedAt 是最后一次确认这条绑定的时间。
	UpdatedAt time.Time `json:"updated_at"`
}

// sessionStoreVersion 是落盘格式版本，便于以后无痛迁移。
const sessionStoreVersion = 1

type sessionStoreFile struct {
	Version int                       `json:"version"`
	Entries map[string]SessionBinding `json:"entries"`
}

// SessionStore 是「会话名 → Target 绑定」的落盘存储。零值不可用，用 OpenSessionStore。
// 所有方法对 nil 接收者安全（未启用持久化时调用方无需判空）。
type SessionStore struct {
	path    string
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	entries map[string]SessionBinding
}

// DefaultSessionTTL 是会话绑定不配置时的有效期。用户口径（2026-09-22）：
// **不做孤儿窗口自动回收**——窗口留在那里，用户可能还要看一眼；过期只表示
// 「引擎不再去接管它」，那扇窗就此变成用户的普通窗口。
const DefaultSessionTTL = 24 * time.Hour

// OpenSessionStore 打开（或首次创建）会话绑定存储。path 为空 = 不启用持久化，
// 返回 nil 且不报错（旧调用方行为零变化）。
func OpenSessionStore(path string) (*SessionStore, error) {
	if path == "" {
		return nil, nil
	}
	s := &SessionStore{
		path:    path,
		now:     time.Now,
		entries: make(map[string]SessionBinding),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path 返回落盘路径（诊断用）。
func (s *SessionStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// NewSessionStoreInMemory 构造一个不落盘的存储（测试 / 临时场景）。
func NewSessionStoreInMemory() *SessionStore {
	return &SessionStore{now: time.Now, entries: make(map[string]SessionBinding)}
}

// SetTTL 设定绑定有效期并顺手清掉已过期条目（启动期调用一次即可；0 = 永不过期）。
// 过期**不关窗**：只是不再拿它做续页接管，窗口留给用户。
func (s *SessionStore) SetTTL(d time.Duration) {
	if s == nil || d < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttl = d
	if removed := s.pruneLocked(); removed > 0 {
		_ = s.flushLocked()
	}
}

// TTL 返回当前有效期（0 = 永不过期）。
func (s *SessionStore) TTL() time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl
}

// expiredLocked 判断一条绑定是否已超过有效期。调用方持锁。
func (s *SessionStore) expiredLocked(b SessionBinding) bool {
	if s.ttl <= 0 || b.UpdatedAt.IsZero() {
		return false
	}
	return s.now().Sub(b.UpdatedAt) > s.ttl
}

// pruneLocked 删除所有过期绑定，返回删除条数。调用方持锁。
func (s *SessionStore) pruneLocked() int {
	if s.ttl <= 0 {
		return 0
	}
	removed := 0
	for name, b := range s.entries {
		if s.expiredLocked(b) {
			delete(s.entries, name)
			removed++
		}
	}
	return removed
}

// Lookup 查询会话绑定；未记录返回 false。
// 过期绑定（见 SetTTL）视为不存在——续页是「接着上次干」，太久以前的窗
// 直接接管反而会打断用户当前在做的事。
func (s *SessionStore) Lookup(session string) (SessionBinding, bool) {
	if s == nil || session == "" {
		return SessionBinding{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.entries[session]
	if ok && s.expiredLocked(b) {
		return SessionBinding{}, false
	}
	return b, ok
}

// Save 记下（或刷新）一条会话绑定。落盘失败返回错误，但内存已更新——
// 调用方最多丢一次续页机会，不影响本次任务。
func (s *SessionStore) Save(b SessionBinding) error {
	if s == nil {
		return nil
	}
	if b.Session == "" {
		return errors.New("session store: session 不能为空")
	}
	if b.TargetID == "" {
		return errors.New("session store: target_id 不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b.UpdatedAt = s.now()
	s.entries[b.Session] = b
	return s.flushLocked()
}

// Delete 删除一条会话绑定（显式 CloseContext：会话这条线结束了）。
func (s *SessionStore) Delete(session string) error {
	if s == nil || session == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[session]; !ok {
		return nil
	}
	delete(s.entries, session)
	return s.flushLocked()
}

// Sessions 返回全部绑定的快照（按会话名排序，诊断 / 测试用）。
func (s *SessionStore) Sessions() []SessionBinding {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionBinding, 0, len(s.entries))
	for _, b := range s.entries {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}

func (s *SessionStore) loadLocked() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("session store: 读取 %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var f sessionStoreFile
	if err := json.Unmarshal(data, &f); err != nil {
		// 坏文件不该拦住引擎启动：备份一份再当空的重来（下次 Save 覆盖）。
		backup := s.path + ".corrupt"
		_ = os.Rename(s.path, backup)
		return fmt.Errorf("session store: %s 不是合法 JSON（已备份为 %s）: %w", s.path, backup, err)
	}
	if f.Entries != nil {
		s.entries = f.Entries
	}
	return nil
}

// flushLocked 原子落盘。调用方持锁。path 为空 = 内存态，不落盘。
func (s *SessionStore) flushLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("session store: 建目录: %w", err)
	}
	data, err := json.MarshalIndent(sessionStoreFile{Version: sessionStoreVersion, Entries: s.entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("session store: 编码: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".sessions-*.json")
	if err != nil {
		return fmt.Errorf("session store: 建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session store: 权限: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session store: 写入: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session store: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("session store: 替换 %s: %w", s.path, err)
	}
	return nil
}
