package knowledge

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxMemoryMB   = 300
	handleTTL            = 60 * time.Second
	defaultEngineVersion = "v1.5.1"
)

// ErrResourceExhausted 表示加载新版本时内存不足（对应 gRPC RESOURCE_EXHAUSTED）。
var ErrResourceExhausted = errors.New("RESOURCE_EXHAUSTED")

// Artifact 是从对象存储下载并校验后的知识库产物。
type Artifact struct {
	Version           string
	SchemaHash        string
	ModelHash         string
	ArtifactURI       string
	MinEngineVersion  string
	MaxEngineVersion  string            // 兼容引擎上限（如 "v1.4.x" = v1.4 任意 patch；空 = 无上限）
	KeyRuleIndexCount int               // Sanity Check：关键规则索引是否可查
	OnnxReady         bool              // Sanity Check：ONNX 空特征推理是否正常
	OnnxModel         []byte            // classifier.onnx 模型字节（A 案 ort-go 加载；空 = 无真实模型）
	MemoryEstimateMB  int64             // 新版本驻留内存估算
	Rules             map[string]string // ruleID → status（ACTIVE/STAGING/…）
	RulesFull         []RuleSnapshot    // 完整规则行（热更后重建 matcher 的输入，R1）
	Synonyms          []SynonymSnapshot // L2 同义词快照（matcher 重建输入）
	NegativeKeywords  []string          // 黑名单快照（matcher 重建输入）
}

// RuleSnapshot 是 uws_rules 表的一行（spec/algorithm-kernel.md §4.2）。
// matcher 重建时由引擎映射为 matcher.Rule（两包解耦，避免循环依赖）。
type RuleSnapshot struct {
	RuleType      string
	TextPattern   string
	Selector      string
	Role          string
	Action        string
	SemanticLabel string
	Confidence    float64
	Source        string
	Status        string
	StructHash    uint64 // Tier 1.5：规则级结构通道参考哈希（matcher.StructuralSimHash64）
}

// SynonymSnapshot 是 site_synonyms 表的一行（spec/algorithm-kernel.md §4.3）。
type SynonymSnapshot struct {
	SiteDomain   string
	Term         string
	BaseWeight   float64
	Status       string
	HitCount     int
	SourceRuleID int64
}

// Fetcher 抽象下载 + SHA256 校验 + 加载（测试注入内存实现）。
type Fetcher interface {
	Fetch(ctx context.Context, version, artifactURI string) (*Artifact, error)
}

// Handle 是知识库读取句柄；旧句柄在 TTL（60s）内仍可服务进行中的异步验证。
type Handle struct {
	mu         sync.RWMutex
	Version    string
	SchemaHash string
	ModelHash  string
	Rules      map[string]string
	expiresAt  time.Time
	released   bool
}

func newHandle(art *Artifact, expiresAt time.Time) *Handle {
	rules := make(map[string]string, len(art.Rules))
	for k, v := range art.Rules {
		rules[k] = v
	}
	return &Handle{
		Version:    art.Version,
		SchemaHash: art.SchemaHash,
		ModelHash:  art.ModelHash,
		Rules:      rules,
		expiresAt:  expiresAt,
	}
}

// Lookup 从句柄查询规则状态；句柄已释放返回 ok=false。
func (h *Handle) Lookup(ruleID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.released {
		return "", false
	}
	st, ok := h.Rules[ruleID]
	return st, ok
}

// Release 释放句柄（TTL 到期后由 Reap 调用）。
func (h *Handle) Release() {
	h.mu.Lock()
	h.released = true
	h.mu.Unlock()
}

type Version struct {
	Artifact *Artifact
	handle   *Handle
}

func (v *Version) version() string {
	if v == nil || v.Artifact == nil {
		return ""
	}
	return v.Artifact.Version
}

type ReloadRequest struct {
	Version       string
	ArtifactURI   string
	DryRun        bool
	Force         bool
	EngineVersion string
}

type ReloadResponse struct {
	OK              bool
	ActiveVersion   string
	PreviousVersion string
	SwitchTimeMs    int64
	Reason          string
}

type RollbackLog struct {
	From string
	To   string
	At   time.Time
}

// Store 维护 active/staging 双缓冲句柄，提供原子切换、Sanity Check、回滚一致性修复。
type Store struct {
	mu            sync.RWMutex
	fetcher       Fetcher
	active        *Version
	previous      *Version
	now           func() time.Time
	ttl           time.Duration
	maxMemoryMB   int64
	engineVersion string
	rollbackLogs  []RollbackLog
	// lastAutoRollback 记录最近一次 P0 自动回滚（HealthCheck 回滚标记数据源，
	// knowledge-versioning.md §5.2；被后续人工 Reload/Rollback 覆盖后清除）。
	lastAutoRollback *RollbackLog
}

// NewStore 创建知识库 Store；initial 为初始 active 版本（可 nil）。
func NewStore(fetcher Fetcher, initial *Artifact) *Store {
	s := &Store{
		fetcher:       fetcher,
		now:           time.Now,
		ttl:           handleTTL,
		maxMemoryMB:   defaultMaxMemoryMB,
		engineVersion: defaultEngineVersion,
	}
	if initial != nil {
		s.active = &Version{Artifact: initial, handle: newHandle(initial, s.now().Add(s.ttl))}
	}
	return s
}

// Reload 执行热加载：下载校验 → 内存/Sanity/兼容性检查 → 原子切换。
// dry_run 仅下载并校验 SHA256，不切换。
func (s *Store) Reload(ctx context.Context, req ReloadRequest) ReloadResponse {
	uri := req.ArtifactURI
	if uri == "" {
		uri = "s3://uws-artifacts/" + req.Version
	}
	art, err := s.fetcher.Fetch(ctx, req.Version, uri)
	if err != nil {
		return ReloadResponse{OK: false, Reason: "fetch_failed: " + err.Error()}
	}

	s.mu.RLock()
	oldActive := ""
	if s.active != nil {
		oldActive = s.active.version()
	}
	s.mu.RUnlock()

	if !req.DryRun {
		if !s.memoryOK(art) {
			return ReloadResponse{OK: false, ActiveVersion: oldActive, Reason: ErrResourceExhausted.Error()}
		}
		if !sanityOK(art) {
			return ReloadResponse{OK: false, ActiveVersion: oldActive, Reason: "sanity_check_failed"}
		}
		engine := req.EngineVersion
		if engine == "" {
			engine = s.engineVersion
		}
		if !req.Force && !compatible(art.MinEngineVersion, art.MaxEngineVersion, engine) {
			return ReloadResponse{OK: false, ActiveVersion: oldActive, Reason: "version_incompatible"}
		}
	}

	s.mu.Lock()
	if req.DryRun {
		s.mu.Unlock()
		return ReloadResponse{OK: true, ActiveVersion: oldActive}
	}
	// 回滚/降级一致性（spec/knowledge-versioning.md §5.4）：
	// 目标版本未知的当前 ACTIVE 规则 → STAGING，防止污染旧知识（TDD-KB-07）。
	// 也覆盖经 ReloadKnowledge 回滚到旧版本的路径（不限于 Rollback）。
	if s.active != nil && s.active.Artifact != nil {
		demoteUnknownActive(s.active.Artifact.Rules, art)
	}
	next := &Version{Artifact: art, handle: newHandle(art, s.now().Add(s.ttl))}
	switchMS := s.swapLocked(next)
	s.lastAutoRollback = nil // 人工切换建立新基线，清除自动回滚标记
	s.mu.Unlock()
	return ReloadResponse{
		OK:              true,
		ActiveVersion:   next.version(),
		PreviousVersion: oldActive,
		SwitchTimeMs:    switchMS,
	}
}

// Rollback 回滚到指定版本，并修复回滚一致性：
// 新版本中 ACTIVE 但目标版本无对应记录的 L2 规则 → 重置 STAGING 重新评估。
func (s *Store) Rollback(ctx context.Context, version string) ReloadResponse {
	art, err := s.fetcher.Fetch(ctx, version, "s3://uws-artifacts/"+version)
	if err != nil {
		return ReloadResponse{OK: false, Reason: "fetch_failed: " + err.Error()}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return ReloadResponse{OK: false, Reason: "no_active_version"}
	}
	from := s.active.version()
	demoteUnknownActive(s.active.Artifact.Rules, art)
	next := &Version{Artifact: art, handle: newHandle(art, s.now().Add(s.ttl))}
	switchMS := s.swapLocked(next)
	s.rollbackLogs = append(s.rollbackLogs, RollbackLog{From: from, To: version, At: s.now()})
	s.lastAutoRollback = nil // 人工回滚建立新基线，清除自动回滚标记
	return ReloadResponse{OK: true, ActiveVersion: version, PreviousVersion: from, SwitchTimeMs: switchMS}
}

// AutoRollback 执行 P0 自动回滚（knowledge-versioning.md §5.2）：
// 连续 50 个 NOT_FOUND（大面积失明）时，回滚到双缓冲保留的上一个稳定版本，
// 无需重新下载（previous 句柄仍在 TTL 60s 窗口内）；回滚后写入审计日志并
// 记录 HealthCheck 回滚标记。回滚后清空 previous，防止连续失败在坏版本间乒乓切换。
func (s *Store) AutoRollback() ReloadResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return ReloadResponse{OK: false, Reason: "no_active_version"}
	}
	from := s.active.version()
	if s.previous == nil || s.previous.Artifact == nil {
		return ReloadResponse{OK: false, ActiveVersion: from, Reason: "no_previous_version"}
	}
	prev := s.previous
	// 重建回滚目标句柄（不可复用旧句柄：句柄持有创建时的规则快照，
	// 而回滚一致性修复会修改规则集；旧句柄仍可由在途异步验证器持有至 TTL）。
	demoted := &Artifact{
		Version:           prev.Artifact.Version,
		SchemaHash:        prev.Artifact.SchemaHash,
		ModelHash:         prev.Artifact.ModelHash,
		ArtifactURI:       prev.Artifact.ArtifactURI,
		MinEngineVersion:  prev.Artifact.MinEngineVersion,
		MaxEngineVersion:  prev.Artifact.MaxEngineVersion,
		KeyRuleIndexCount: prev.Artifact.KeyRuleIndexCount,
		OnnxReady:         prev.Artifact.OnnxReady,
		MemoryEstimateMB:  prev.Artifact.MemoryEstimateMB,
		Rules:             make(map[string]string, len(prev.Artifact.Rules)),
		RulesFull:         append([]RuleSnapshot(nil), prev.Artifact.RulesFull...),
		Synonyms:          append([]SynonymSnapshot(nil), prev.Artifact.Synonyms...),
		NegativeKeywords:  append([]string(nil), prev.Artifact.NegativeKeywords...),
	}
	for k, v := range prev.Artifact.Rules {
		demoted.Rules[k] = v
	}
	// 回滚一致性（TDD-KB-07 同款）：坏版本独有 ACTIVE 规则 → 目标版本中置 STAGING。
	demoteUnknownActive(s.active.Artifact.Rules, demoted)
	next := &Version{Artifact: demoted, handle: newHandle(demoted, s.now().Add(s.ttl))}
	// 丢弃坏版本：若把坏版本留作 previous，二次连续失败会乒乓回滚到坏版本。
	s.active = next
	s.previous = nil
	s.rollbackLogs = append(s.rollbackLogs, RollbackLog{From: from, To: demoted.Version, At: s.now()})
	s.lastAutoRollback = &RollbackLog{From: from, To: demoted.Version, At: s.now()}
	return ReloadResponse{
		OK:              true,
		ActiveVersion:   demoted.Version,
		PreviousVersion: from,
		Reason:          "auto_rollback",
	}
}

// LastAutoRollback 返回最近一次自动回滚记录（nil = 从未自动回滚/已被人工切换覆盖）。
func (s *Store) LastAutoRollback() *RollbackLog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastAutoRollback == nil {
		return nil
	}
	cp := *s.lastAutoRollback
	return &cp
}

// demoteUnknownActive 一致性修复：目标版本无记录的当前 ACTIVE 规则 → STAGING
// （回滚后重新评估，避免旧版本被新规则污染）。
func demoteUnknownActive(activeRules map[string]string, target *Artifact) {
	if target.Rules == nil {
		target.Rules = map[string]string{}
	}
	for ruleID, status := range activeRules {
		if status == "ACTIVE" {
			if _, ok := target.Rules[ruleID]; !ok {
				target.Rules[ruleID] = "STAGING"
			}
		}
	}
}

// ExecuteAction 读取 active 句柄执行一次查询（并发读与切换互斥，无半更新状态）。
func (s *Store) ExecuteAction(ruleID string) (version, status string, ok bool) {
	s.mu.RLock()
	v := s.active
	s.mu.RUnlock()
	if v == nil || v.handle == nil {
		return "", "", false
	}
	st, found := v.handle.Lookup(ruleID)
	return v.version(), st, found
}

// AcquireHandle 返回当前 active 句柄（异步验证器持有，切换后仍可用至 TTL）。
func (s *Store) AcquireHandle() (*Handle, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil || s.active.handle == nil {
		return nil, ""
	}
	return s.active.handle, s.active.version()
}

// ActiveArtifact 返回当前 active 产物的深拷贝。
// 引擎在热更/回滚成功后据此重建 matcher；副本防止调用方与并发切换互相污染。
func (s *Store) ActiveArtifact() *Artifact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil || s.active.Artifact == nil {
		return nil
	}
	return cloneArtifact(s.active.Artifact)
}

func cloneArtifact(a *Artifact) *Artifact {
	cp := *a
	cp.Rules = make(map[string]string, len(a.Rules))
	for k, v := range a.Rules {
		cp.Rules[k] = v
	}
	cp.RulesFull = append([]RuleSnapshot(nil), a.RulesFull...)
	cp.Synonyms = append([]SynonymSnapshot(nil), a.Synonyms...)
	cp.NegativeKeywords = append([]string(nil), a.NegativeKeywords...)
	return &cp
}

// Reap 释放已过期（TTL 60s）的 previous 句柄。
func (s *Store) Reap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.previous != nil && s.previous.handle != nil && !s.previous.handle.expiresAt.After(s.now()) {
		s.previous.handle.Release()
		s.previous = nil
	}
}

func (s *Store) swapLocked(next *Version) int64 {
	start := s.now()
	old := s.active
	if old != nil && old.handle != nil {
		old.handle.expiresAt = s.now().Add(s.ttl)
	}
	s.previous = old
	s.active = next
	return s.now().Sub(start).Milliseconds()
}

func (s *Store) memoryOK(art *Artifact) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	activeMem := int64(0)
	if s.active != nil && s.active.Artifact != nil {
		activeMem = s.active.Artifact.MemoryEstimateMB
	}
	return activeMem+art.MemoryEstimateMB <= s.maxMemoryMB
}

func sanityOK(art *Artifact) bool {
	return art.KeyRuleIndexCount > 0 && art.OnnxReady
}

// compatible 校验产物与引擎版本的双边界兼容性（spec/knowledge-versioning.md §1.2）：
//   - min_engine_version：引擎版本 ≥ 下限（既有语义）；
//   - max_engine_version：引擎版本 ≤ 上限，支持 "x" 通配
//     （"v1.4.x" = v1.4 任意 patch；"v1.x" = v1 任意 minor/patch）；空 = 无上限。
func compatible(minEngine, maxEngine, engine string) bool {
	if minEngine != "" && compareVersions(engine, minEngine) < 0 {
		return false
	}
	if maxEngine != "" && !withinMax(engine, maxEngine) {
		return false
	}
	return true
}

// withinMax 判断 engine 是否落在 max_engine_version 上限内（x 通配见 compatible）。
func withinMax(engine, max string) bool {
	pe := parseVersion(engine)
	parts := strings.Split(strings.TrimPrefix(max, "v"), ".")
	for i := 0; i < 3; i++ {
		if i >= len(parts) {
			return true
		}
		if parts[i] == "x" || parts[i] == "X" {
			return true
		}
		n, _ := strconv.Atoi(parts[i])
		if pe[i] > n {
			return false
		}
		if pe[i] < n {
			return true
		}
	}
	return true
}

func compareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	var out [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		n, _ := strconv.Atoi(strings.TrimFunc(parts[i], func(r rune) bool { return r < '0' || r > '9' }))
		out[i] = n
	}
	return out
}
