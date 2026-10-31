package vlm

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LearnedEntry 是一条自进化记录：语义兜底成功后，把「页面 + 语义 → 确定性定位」
// 持久化。下次同页面同语义优先走 Selector（或内部 Semantic），不再调 VLM。
type LearnedEntry struct {
	Selector   string `json:"selector,omitempty"`
	Semantic   string `json:"semantic,omitempty"`
	NodeText   string `json:"node_text,omitempty"`
	VerifiedAt string `json:"verified_at,omitempty"`
	Hits       int    `json:"hits"`
}

// LearnedStore 是自进化学习存储（JSON 文件，原子写；同进程内存态并发安全）。
type LearnedStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]LearnedEntry
}

// NewLearnedStore 打开（文件不存在则空启动）学习存储；path 为空 = 纯内存态（测试用）。
func NewLearnedStore(path string) (*LearnedStore, error) {
	s := &LearnedStore{path: path, entries: map[string]LearnedEntry{}}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("vlm: load learned(%s): %w", path, err)
	}
	var data struct {
		Version int                     `json:"version"`
		Entries map[string]LearnedEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("vlm: parse learned(%s): %w", path, err)
	}
	if data.Entries != nil {
		s.entries = data.Entries
	}
	return s, nil
}

// NormalizeURL 把页面 URL 归一为学习键前缀（scheme://host/path，去掉 query/fragment）。
func NormalizeURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return u
	}
	p.RawQuery, p.Fragment = "", ""
	return p.String()
}

func keyFor(u, semantic string) string {
	return NormalizeURL(u) + "\x00" + strings.TrimSpace(semantic)
}

// Lookup 查询已学到的定位（同一页面 + 同一语义）。
func (s *LearnedStore) Lookup(u, semantic string) (LearnedEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[keyFor(u, semantic)]
	return e, ok
}

// Learn 记录一条成功定位；已存在则累加命中并覆盖定位。
func (s *LearnedStore) Learn(u, semantic string, e LearnedEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyFor(u, semantic)
	if prev, ok := s.entries[k]; ok {
		e.Hits = prev.Hits + 1
		if e.VerifiedAt == "" {
			e.VerifiedAt = prev.VerifiedAt
		}
	} else if e.Hits == 0 {
		e.Hits = 1
	}
	if e.VerifiedAt == "" {
		e.VerifiedAt = time.Now().Format(time.RFC3339)
	}
	s.entries[k] = e
	if s.path == "" {
		return nil
	}
	return s.flushLocked()
}

func (s *LearnedStore) flushLocked() error {
	data := map[string]any{"version": 1, "entries": s.entries}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Forget 删除一条已学定位（learned 复用命中的落点自证判 miss 时调用，§5.0.9 条款 6）：
// 失效条目必须清掉，否则会被反复复用、把整页点进防乱点熔断；删除后下次自动重新学习。
func (s *LearnedStore) Forget(u, semantic string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyFor(u, semantic)
	if _, ok := s.entries[k]; !ok {
		return nil
	}
	delete(s.entries, k)
	if s.path == "" {
		return nil
	}
	return s.flushLocked()
}

// Entries 返回条目副本（测试/导出用）。
func (s *LearnedStore) Entries() map[string]LearnedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]LearnedEntry, len(s.entries))
	for k, v := range s.entries {
		out[k] = v
	}
	return out
}

// Path 返回存储路径（空 = 内存态）。
func (s *LearnedStore) Path() string { return s.path }
