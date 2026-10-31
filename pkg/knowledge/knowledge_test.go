// 测试骨架：TDD-KB-01..07（tdd-checklist.md §3.4）。
// 与 tests/integration/test_knowledge.py（gRPC 契约层）双层验证。
package knowledge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── mock ───────────────────────────────────────────────────────────────

type fakeFetcher struct {
	mu        sync.Mutex
	artifacts map[string]*Artifact
	errs      map[string]error
	calls     []string
}

func (f *fakeFetcher) Fetch(_ context.Context, version, _ string) (*Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, version)
	if err := f.errs[version]; err != nil {
		return nil, err
	}
	art, ok := f.artifacts[version]
	if !ok {
		return nil, errors.New("artifact not found")
	}
	cp := *art
	cp.Rules = make(map[string]string, len(art.Rules))
	for k, v := range art.Rules {
		cp.Rules[k] = v
	}
	return &cp, nil
}

func initialArtifact() *Artifact {
	return &Artifact{
		Version:           "v1.3.9",
		SchemaHash:        "sha-old",
		ModelHash:         "model-old",
		ArtifactURI:       "s3://uws-artifacts/v1.3.9",
		MinEngineVersion:  "v1.3.0",
		KeyRuleIndexCount: 10,
		OnnxReady:         true,
		MemoryEstimateMB:  100,
		Rules:             map[string]string{"l1:login": "ACTIVE"},
	}
}

func newArtifact() *Artifact {
	return &Artifact{
		Version:           "v1.4.0",
		SchemaHash:        "sha-new",
		ModelHash:         "model-new",
		ArtifactURI:       "s3://uws-artifacts/v1.4.0",
		MinEngineVersion:  "v1.4.0",
		KeyRuleIndexCount: 12,
		OnnxReady:         true,
		MemoryEstimateMB:  100,
		Rules: map[string]string{
			"l1:login":      "ACTIVE",
			"l2:search_btn": "ACTIVE",
		},
	}
}

func newFetcher() *fakeFetcher {
	return &fakeFetcher{
		artifacts: map[string]*Artifact{
			"v1.3.9": initialArtifact(),
			"v1.4.0": newArtifact(),
		},
		errs: map[string]error{},
	}
}

func activeVersion(t *testing.T, s *Store) string {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return ""
	}
	return s.active.version()
}

// ── TDD-KB-06：P0 自动回滚（Store 层） ─────────────────────────────────

func TestAutoRollbackNoPrevious(t *testing.T) {
	s := NewStore(newFetcher(), initialArtifact())
	resp := s.AutoRollback()
	if resp.OK {
		t.Fatalf("无 previous 时不应回滚成功: %+v", resp)
	}
	if resp.Reason != "no_previous_version" {
		t.Fatalf("reason = %q, want no_previous_version", resp.Reason)
	}
	if got := activeVersion(t, s); got != "v1.3.9" {
		t.Fatalf("active 应保持 v1.3.9: %s", got)
	}
}

func TestAutoRollbackSwitchesToPrevious(t *testing.T) {
	f := newFetcher()
	s := NewStore(f, initialArtifact())
	if resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", EngineVersion: "v1.5.0"}); !resp.OK {
		t.Fatalf("reload v1.4.0 失败: %+v", resp)
	}

	resp := s.AutoRollback()
	if !resp.OK {
		t.Fatalf("自动回滚应成功: %+v", resp)
	}
	if resp.ActiveVersion != "v1.3.9" || resp.PreviousVersion != "v1.4.0" {
		t.Fatalf("回滚版本错误: %+v", resp)
	}
	if resp.Reason != "auto_rollback" {
		t.Fatalf("reason = %q, want auto_rollback", resp.Reason)
	}
	if got := activeVersion(t, s); got != "v1.3.9" {
		t.Fatalf("active 应为 v1.3.9: %s", got)
	}

	// 一致性修复：v1.4.0 独有 ACTIVE 规则在回滚目标中置 STAGING。
	if _, st, ok := s.ExecuteAction("l2:search_btn"); !ok || st != "STAGING" {
		t.Fatalf("v1.4.0 独有 ACTIVE 规则应降级 STAGING: st=%s ok=%v", st, ok)
	}
	if _, st, ok := s.ExecuteAction("l1:login"); !ok || st != "ACTIVE" {
		t.Fatalf("共有规则应保持 ACTIVE: st=%s ok=%v", st, ok)
	}

	rb := s.LastAutoRollback()
	if rb == nil {
		t.Fatal("LastAutoRollback 应为非 nil")
	}
	if rb.From != "v1.4.0" || rb.To != "v1.3.9" {
		t.Fatalf("回滚记录错误: %+v", rb)
	}
	s.mu.RLock()
	n := len(s.rollbackLogs)
	s.mu.RUnlock()
	if n != 1 {
		t.Fatalf("rollbackLogs 应为 1 条: %d", n)
	}
}

func TestAutoRollbackPingPongPrevented(t *testing.T) {
	s := NewStore(newFetcher(), initialArtifact())
	if resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", EngineVersion: "v1.5.0"}); !resp.OK {
		t.Fatalf("reload v1.4.0 失败: %+v", resp)
	}
	if resp := s.AutoRollback(); !resp.OK {
		t.Fatalf("首次自动回滚应成功: %+v", resp)
	}
	// 回滚后 previous 清空：二次自动回滚不得乒乓切回坏版本。
	resp := s.AutoRollback()
	if resp.OK {
		t.Fatalf("二次自动回滚应失败（坏版本已丢弃）: %+v", resp)
	}
	if got := activeVersion(t, s); got != "v1.3.9" {
		t.Fatalf("active 应保持 v1.3.9: %s", got)
	}
}

func TestAutoRollbackClearedOnManualReload(t *testing.T) {
	s := NewStore(newFetcher(), initialArtifact())
	if resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", EngineVersion: "v1.5.0"}); !resp.OK {
		t.Fatalf("reload v1.4.0 失败: %+v", resp)
	}
	if resp := s.AutoRollback(); !resp.OK {
		t.Fatalf("自动回滚应成功: %+v", resp)
	}
	if s.LastAutoRollback() == nil {
		t.Fatal("自动回滚后标记应存在")
	}
	// 人工切换建立新基线 → 标记清除。
	if resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", EngineVersion: "v1.5.0"}); !resp.OK {
		t.Fatalf("人工 reload 失败: %+v", resp)
	}
	if s.LastAutoRollback() != nil {
		t.Fatal("人工切换后自动回滚标记应清除")
	}
}

// ── TDD-KB-01：dry_run 仅下载校验，不切换 ───────────────────────────────

func TestKb01DryRunNoSwitch(t *testing.T) {
	f := newFetcher()
	s := NewStore(f, initialArtifact())

	resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", DryRun: true})
	if !resp.OK {
		t.Fatalf("dry_run 应成功: %+v", resp)
	}
	if resp.ActiveVersion != "v1.3.9" {
		t.Errorf("dry_run 不应切换: active=%s", resp.ActiveVersion)
	}
	if v := activeVersion(t, s); v != "v1.3.9" {
		t.Errorf("切换发生: active=%s", v)
	}
	s.mu.RLock()
	art := s.active.Artifact
	s.mu.RUnlock()
	if art.SchemaHash != "sha-old" || art.ModelHash != "model-old" {
		t.Errorf("schema/model hash 被修改: %s/%s", art.SchemaHash, art.ModelHash)
	}
	if len(f.calls) != 1 || f.calls[0] != "v1.4.0" {
		t.Errorf("dry_run 应仅下载 v1.4.0: %v", f.calls)
	}

	// SHA256 校验失败：不切换且返回失败原因
	f.mu.Lock()
	f.errs["v1.4.0"] = errors.New("sha256 mismatch")
	f.mu.Unlock()
	resp2 := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", DryRun: true})
	if resp2.OK {
		t.Error("SHA256 校验失败时不应 OK")
	}
	if !strings.Contains(resp2.Reason, "sha256") {
		t.Errorf("应返回 sha256 失败原因: %s", resp2.Reason)
	}
	if v := activeVersion(t, s); v != "v1.3.9" {
		t.Errorf("校验失败后不应切换: active=%s", v)
	}
}

// ── TDD-KB-02：原子切换 + 写锁 <1ms + 并发无半更新 + 旧句柄 TTL 60s ────

func TestKb02AtomicSwitch(t *testing.T) {
	f := newFetcher()
	s := NewStore(f, initialArtifact())

	const workers = 16
	const rounds = 200
	results := make(chan string, workers*rounds)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				v, _, ok := s.ExecuteAction("l1:login")
				if !ok {
					results <- "ERR"
					return
				}
				results <- v
			}
		}()
	}
	time.Sleep(5 * time.Millisecond) // 让部分读先发生，再执行切换
	resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0"})
	wg.Wait()
	close(results)

	if !resp.OK || resp.ActiveVersion != "v1.4.0" || resp.PreviousVersion != "v1.3.9" {
		t.Fatalf("切换响应异常: %+v", resp)
	}
	if resp.SwitchTimeMs >= 1 {
		t.Errorf("写锁窗口 >=1ms: %dms（规格要求 <1ms）", resp.SwitchTimeMs)
	}
	for v := range results {
		if v == "ERR" {
			t.Fatal("并发 ExecuteAction 出现失败")
		}
		if v != "v1.3.9" && v != "v1.4.0" {
			t.Fatalf("检测到半更新状态: %s", v)
		}
	}

	// 旧句柄 TTL 60s：切换前持有的句柄在 TTL 内可用，过期后释放
	s2 := NewStore(f, initialArtifact())
	oldH, oldV := s2.AcquireHandle()
	if oldV != "v1.3.9" {
		t.Fatalf("初始句柄版本异常: %s", oldV)
	}
	if resp2 := s2.Reload(context.Background(), ReloadRequest{Version: "v1.4.0"}); !resp2.OK {
		t.Fatalf("切换失败: %+v", resp2)
	}
	if _, ok := oldH.Lookup("l1:login"); !ok {
		t.Error("旧句柄在 TTL 内不可用")
	}
	base := s2.now()
	s2.now = func() time.Time { return base.Add(61 * time.Second) }
	s2.Reap()
	if _, ok := oldH.Lookup("l1:login"); ok {
		t.Error("旧句柄超过 TTL 后未被释放")
	}
}

// ── TDD-KB-03：Sanity Check 失败时旧版本保持有效 ───────────────────────

func TestKb03SanityCheckFailKeepsOld(t *testing.T) {
	f := newFetcher()
	s := NewStore(f, initialArtifact())

	// 关键规则索引缺失
	bad := newArtifact()
	bad.KeyRuleIndexCount = 0
	f.mu.Lock()
	f.artifacts["v1.4.0"] = bad
	f.mu.Unlock()

	resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0"})
	if resp.OK || resp.Reason != "sanity_check_failed" {
		t.Fatalf("应返回 sanity_check_failed: %+v", resp)
	}
	if v := activeVersion(t, s); v != "v1.3.9" {
		t.Errorf("Sanity 失败后切换发生: active=%s", v)
	}
	if _, _, ok := s.ExecuteAction("l1:login"); !ok {
		t.Error("Sanity 失败后旧知识库不可用")
	}

	// ONNX 空特征推理异常
	bad2 := newArtifact()
	bad2.OnnxReady = false
	f.mu.Lock()
	f.artifacts["v1.4.0"] = bad2
	f.mu.Unlock()
	resp2 := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0"})
	if resp2.OK || resp2.Reason != "sanity_check_failed" {
		t.Fatalf("ONNX 异常时应返回 sanity_check_failed: %+v", resp2)
	}
	if v := activeVersion(t, s); v != "v1.3.9" {
		t.Errorf("ONNX Sanity 失败后切换发生: active=%s", v)
	}
}

// ── TDD-KB-04：内存不足拒绝切换 → RESOURCE_EXHAUSTED ───────────────────

func TestKb04MemoryLimitReject(t *testing.T) {
	f := newFetcher()
	heavy := initialArtifact()
	heavy.MemoryEstimateMB = 200
	s := NewStore(f, heavy)

	tooBig := newArtifact()
	tooBig.MemoryEstimateMB = 200 // 200+200=400 > 300MB 上限
	f.mu.Lock()
	f.artifacts["v1.4.0"] = tooBig
	f.mu.Unlock()

	resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0"})
	if resp.OK || resp.Reason != ErrResourceExhausted.Error() {
		t.Fatalf("应返回 RESOURCE_EXHAUSTED: %+v", resp)
	}
	if v := activeVersion(t, s); v != "v1.3.9" {
		t.Errorf("内存不足时不应切换: active=%s", v)
	}

	// 新版本内存合适 → 切换成功
	ok := newArtifact()
	ok.MemoryEstimateMB = 80 // 200+80=280 ≤ 300
	f.mu.Lock()
	f.artifacts["v1.4.0"] = ok
	f.mu.Unlock()
	resp2 := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0"})
	if !resp2.OK {
		t.Fatalf("内存合适时应切换成功: %+v", resp2)
	}
}

// ── TDD-KB-05：版本兼容性；force=true 跳过（紧急回滚路径） ──────────────

func TestKb05VersionCompatibilityForce(t *testing.T) {
	f := newFetcher()
	s := NewStore(f, initialArtifact())

	incompat := newArtifact()
	incompat.MinEngineVersion = "v2.0.0"
	f.mu.Lock()
	f.artifacts["v1.4.0"] = incompat
	f.mu.Unlock()

	resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", EngineVersion: "v1.4.0"})
	if resp.OK || resp.Reason != "version_incompatible" {
		t.Fatalf("应拒绝不兼容版本: %+v", resp)
	}
	if v := activeVersion(t, s); v != "v1.3.9" {
		t.Errorf("不兼容时不应切换: active=%s", v)
	}

	resp2 := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", EngineVersion: "v1.4.0", Force: true})
	if !resp2.OK || resp2.ActiveVersion != "v1.4.0" {
		t.Fatalf("force=true 应跳过兼容性检查: %+v", resp2)
	}

	// 兼容版本无需 force
	s3 := NewStore(f, initialArtifact())
	compatArt := newArtifact()
	compatArt.MinEngineVersion = "v1.3.0"
	f.mu.Lock()
	f.artifacts["v1.4.0"] = compatArt
	f.mu.Unlock()
	resp3 := s3.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", EngineVersion: "v1.4.0"})
	if !resp3.OK {
		t.Fatalf("兼容版本应切换成功: %+v", resp3)
	}
}

// ── TDD-KB-05 补充：max_engine_version 上限校验（x 通配语义） ─────────────

func TestKb05MaxEngineVersionEnforced(t *testing.T) {
	cases := []struct {
		name      string
		minEngine string
		maxEngine string
		engine    string
		wantOK    bool
	}{
		{"上限低于引擎（patch 级 x 通配）", "v1.3.0", "v1.4.x", "v1.5.0", false},
		{"上限低于引擎（精确版本）", "v1.3.0", "v1.4.9", "v1.5.0", false},
		{"上限等于引擎 minor（x 通配）", "v1.3.0", "v1.5.x", "v1.5.0", true},
		{"上限等于引擎精确版本", "v1.3.0", "v1.5.0", "v1.5.0", true},
		{"上限高于引擎", "v1.3.0", "v1.6.0", "v1.5.0", true},
		{"minor 级 x 通配（major 精确、minor/patch 任意）", "v1.3.0", "v1.x", "v1.99.99", true},
		{"minor 级 x 通配超出 major → 拒绝", "v1.3.0", "v1.x", "v2.5.3", false},
		{"上限短格式（缺 patch 段 = 任意 patch）", "v1.3.0", "v1.5", "v1.5.9", true},
		{"max 为空 = 无上限", "v1.3.0", "", "v9.9.9", true},
		{"min 不满足且 max 满足 → 仍拒绝", "v2.0.0", "v9.x", "v1.5.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFetcher()
			s := NewStore(f, initialArtifact())
			art := newArtifact()
			art.MinEngineVersion = tc.minEngine
			art.MaxEngineVersion = tc.maxEngine
			f.mu.Lock()
			f.artifacts["v1.4.0"] = art
			f.mu.Unlock()

			resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0", EngineVersion: tc.engine})
			if resp.OK != tc.wantOK {
				t.Fatalf("wantOK=%v, got %+v", tc.wantOK, resp)
			}
			if tc.wantOK && resp.Reason != "" {
				t.Fatalf("成功切换不应带 reason: %+v", resp)
			}
			if !tc.wantOK && resp.Reason != "version_incompatible" {
				t.Fatalf("拒绝原因应为 version_incompatible: %+v", resp)
			}
		})
	}
}

func TestCompatibleMaxWildcardUnit(t *testing.T) {
	// withinMax 单测：边界与通配语义直接核验。
	if withinMax("v1.4.9", "v1.4.x") != true {
		t.Fatal("v1.4.9 应在 v1.4.x 内")
	}
	if withinMax("v1.5.0", "v1.4.x") != false {
		t.Fatal("v1.5.0 不应在 v1.4.x 内")
	}
	if withinMax("v2.0.0", "v1.x") != false {
		t.Fatal("v2.0.0 不应在 v1.x 内")
	}
	if withinMax("v1.99.99", "v1.x") != true {
		t.Fatal("v1.99.99 应在 v1.x 内")
	}
	if withinMax("v1.5.0", "v1.5") != true {
		t.Fatal("v1.5 上限应允许任意 patch")
	}
}

// ── TDD-KB-06：P0 自动回滚（E2E，留待 M7） ──────────────────────────────

func TestKb06AutoRollbackP0(t *testing.T) {
	t.Skip("skeleton: TDD-KB-06 待实现（连续 50 次 NOT_FOUND → <1 分钟自动回滚 + rollback_logs）")
}

// ── TDD-KB-07：回滚一致性——新 ACTIVE 规则在旧版本中重置 STAGING ────────

func TestKb07RollbackConsistencyL2Status(t *testing.T) {
	f := newFetcher()
	s := NewStore(f, initialArtifact())

	resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0"})
	if !resp.OK {
		t.Fatalf("切换到 v1.4.0 失败: %+v", resp)
	}
	if v, st, ok := s.ExecuteAction("l2:search_btn"); !ok || v != "v1.4.0" || st != "ACTIVE" {
		t.Fatalf("v1.4.0 中 l2:search_btn 应为 ACTIVE: v=%s st=%s ok=%v", v, st, ok)
	}

	rb := s.Rollback(context.Background(), "v1.3.9")
	if !rb.OK || rb.ActiveVersion != "v1.3.9" || rb.PreviousVersion != "v1.4.0" {
		t.Fatalf("回滚响应异常: %+v", rb)
	}
	if v, st, ok := s.ExecuteAction("l2:search_btn"); !ok || v != "v1.3.9" {
		t.Fatalf("回滚后规则应存在且被重置: v=%s st=%s ok=%v", v, st, ok)
	} else if st != "STAGING" {
		t.Errorf("新 ACTIVE 规则回滚后应为 STAGING: %s", st)
	}
	if v, st, ok := s.ExecuteAction("l1:login"); !ok || v != "v1.3.9" || st != "ACTIVE" {
		t.Errorf("旧版本原有 ACTIVE 规则不应被污染: v=%s st=%s ok=%v", v, st, ok)
	}
	s.mu.RLock()
	logs := len(s.rollbackLogs)
	s.mu.RUnlock()
	if logs != 1 {
		t.Errorf("rollback_logs 应有 1 条: %d", logs)
	}
}

// ── TDD-KB-07 补充：经 Reload 回滚旧版本同样触发一致性修复（spec §5.4） ──

func TestKb07ReloadPathRollbackConsistency(t *testing.T) {
	f := newFetcher()
	s := NewStore(f, initialArtifact())

	resp := s.Reload(context.Background(), ReloadRequest{Version: "v1.4.0"})
	if !resp.OK {
		t.Fatalf("切换到 v1.4.0 失败: %+v", resp)
	}

	// 运维经 ReloadKnowledge 回滚到旧版本（不是 Rollback 专用路径）。
	rb := s.Reload(context.Background(), ReloadRequest{Version: "v1.3.9"})
	if !rb.OK || rb.ActiveVersion != "v1.3.9" || rb.PreviousVersion != "v1.4.0" {
		t.Fatalf("经 Reload 回滚异常: %+v", rb)
	}
	if v, st, ok := s.ExecuteAction("l2:search_btn"); !ok || v != "v1.3.9" {
		t.Fatalf("回滚后规则应存在: v=%s ok=%v", v, ok)
	} else if st != "STAGING" {
		t.Errorf("新 ACTIVE 规则经 Reload 回滚后应为 STAGING: %s", st)
	}
	if v, st, ok := s.ExecuteAction("l1:login"); !ok || v != "v1.3.9" || st != "ACTIVE" {
		t.Errorf("旧版本原有 ACTIVE 规则不应被污染: v=%s st=%s ok=%v", v, st, ok)
	}
}
