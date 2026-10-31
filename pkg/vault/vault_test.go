// 本地凭据库（2026-09-23 用户定案用 age；GLOBAL #119）。
// 关注点：密文里读不出任何东西、错 identity 一律失败（fail-closed）、不打印明文、
// 权限 0600、写盘原子（不留半截库）。
package vault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, DefaultKeyName)
	filePath := filepath.Join(dir, DefaultFileName)
	s, recipient, err := Init(keyPath, filePath, false)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !strings.HasPrefix(recipient, "age1") {
		t.Fatalf("公开收件人应形如 age1…，实际 %q", recipient)
	}
	return s, keyPath, filePath
}

func TestVaultSetGetRoundTripAcrossReopen(t *testing.T) {
	s, keyPath, filePath := newStore(t)
	if err := s.Set("autodl", "hunter2-not-a-real-password", "AutoDL 控制台"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("openai", "sk-test-xxx", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// 「换一个进程」= 重新 Open：identity 能解开，条目与备注都在。
	again, err := Open(keyPath, filePath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e, ok := again.Get("autodl")
	if !ok || e.Value != "hunter2-not-a-real-password" || e.Note != "AutoDL 控制台" {
		t.Fatalf("回读不一致: %+v ok=%v", e, ok)
	}
	if again.Len() != 2 {
		t.Fatalf("条目数应为 2，实际 %d", again.Len())
	}
}

func TestVaultCiphertextLeaksNothing(t *testing.T) {
	s, _, filePath := newStore(t)
	secret := "S3cr3t-V4lue-绝不落明文"
	if err := s.Set("portal", secret, "备注也加密"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, needle := range []string{secret, "portal", "备注也加密"} {
		if strings.Contains(text, needle) {
			t.Fatalf("密文里不该出现 %q（条目名也不该泄露）:\n%s", needle, text)
		}
	}
	if !strings.Contains(text, "-----BEGIN AGE ENCRYPTED FILE-----") {
		t.Fatalf("应是 ASCII armor 密文，实际:\n%s", text)
	}
	if mode := fileMode(t, filePath); mode != 0o600 {
		t.Fatalf("密文权限应为 0600，实际 %o", mode)
	}
	if mode := fileMode(t, filepath.Join(filepath.Dir(filePath), DefaultKeyName)); mode != 0o600 {
		t.Fatalf("identity 权限应为 0600，实际 %o", mode)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestVaultWrongIdentityFailsClosed(t *testing.T) {
	_, keyPath, filePath := newStore(t)
	// 换一把 identity（另一台机器/另一只 vault）：必须失败，不得退化到空库或明文。
	otherDir := t.TempDir()
	otherKey := filepath.Join(otherDir, DefaultKeyName)
	otherFile := filepath.Join(otherDir, DefaultFileName)
	if _, _, err := Init(otherKey, otherFile, false); err != nil {
		t.Fatalf("Init(other): %v", err)
	}
	if _, err := Open(otherKey, filePath); err == nil {
		t.Fatal("拿别的 identity 解别人的库必须报错")
	}
	if _, err := Open(keyPath, otherFile); err != nil {
		// 空库 + 自己的 identity 是合法的（只是没有条目），这里不断言失败；
		// 真正要保证的是「identity 不匹配一定失败」——上一句已覆盖。
		_ = err
	}
}

func TestVaultNotInitializedAndNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, DefaultKeyName)
	filePath := filepath.Join(dir, DefaultFileName)
	if _, err := Open(keyPath, filePath); err == nil {
		t.Fatal("没有 identity 时应报 ErrNotInitialized")
	}
	if _, _, err := Init(keyPath, filePath, false); err != nil {
		t.Fatalf("首次 Init 应成功: %v", err)
	}
	if _, _, err := Init(keyPath, filePath, false); err == nil {
		t.Fatal("重复 Init 必须拒绝（不覆盖已有密钥），除非显式 force")
	}
	if _, _, err := Init(keyPath, filePath, true); err != nil {
		t.Fatalf("显式 force 应允许重建: %v", err)
	}
}

func TestVaultListHidesValuesAndDeleteWorks(t *testing.T) {
	s, keyPath, filePath := newStore(t)
	if err := s.Set("a", "value-a", "note-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("b", "value-b", ""); err != nil {
		t.Fatal(err)
	}
	items := s.List()
	if len(items) != 2 || items[0].Name != "a" || items[1].Name != "b" {
		t.Fatalf("List 应按名字排序: %+v", items)
	}
	if items[0].Fingerprint != Fingerprint("value-a") || len(items[0].Fingerprint) != 12 {
		t.Fatalf("指纹应为 sha256 前 12 位: %+v", items[0])
	}
	// 列表视图序列化后也不得出现明文（下游把它写日志/贴群时不泄露）。
	blob, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "value-a") || strings.Contains(string(blob), "value-b") {
		t.Fatalf("List 结果含明文: %s", blob)
	}
	ok, err := s.Delete("a")
	if err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.Delete("不存在"); ok {
		t.Fatal("删除不存在的条目应返回 false")
	}
	again, err := Open(keyPath, filePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Get("a"); ok {
		t.Fatal("删除应已落盘")
	}
	if _, ok := again.Get("b"); !ok {
		t.Fatal("不该误删别的条目")
	}
}

func TestVaultRejectsEmptyValueAndName(t *testing.T) {
	s, _, _ := newStore(t)
	if err := s.Set("", "v", ""); err == nil {
		t.Fatal("空条目名必须拒绝")
	}
	if err := s.Set("x", "", ""); err == nil {
		t.Fatal("空值必须拒绝（避免悄悄写进一条空密码）")
	}
}

func TestVaultCorruptCiphertextFailsClosed(t *testing.T) {
	s, keyPath, filePath := newStore(t)
	if err := s.Set("k", "v", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("-----BEGIN AGE ENCRYPTED FILE-----\n不是密文\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(keyPath, filePath); err == nil {
		t.Fatal("损坏的密文必须报错，绝不能当成空库")
	}
}

func TestVaultUpdatedAtUsesClock(t *testing.T) {
	s, _, _ := newStore(t)
	fixed := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	if err := s.Set("t", "v", ""); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get("t")
	if !e.UpdatedAt.Equal(fixed) {
		t.Fatalf("UpdatedAt 应取注入时钟，实际 %s", e.UpdatedAt)
	}
}

// ── 凭据直填引用（2026-09-23）：`@vault:<名>` 解析与统一路径口径 ──────────────

func TestVaultReferenceName(t *testing.T) {
	cases := []struct {
		in    string
		name  string
		isRef bool
	}{
		{"@vault:autodl", "autodl", true},
		{"  @vault:autodl  ", "autodl", true},
		{"@vault:  autodl", "autodl", true},
		{"@vault:", "", false}, // 空名不是引用（否则会静默解成空值）
		{"@vault:   ", "", false},
		{"autodl", "", false},           // 普通值照旧按文本填
		{"@Vault:autodl", "", false},    // 前缀大小写敏感，不做模糊匹配
		{"前缀 @vault:autodl", "", false}, // 只有整串就是引用才解引用
		{"", "", false},
	}
	for _, c := range cases {
		name, ok := ReferenceName(c.in)
		if ok != c.isRef || name != c.name {
			t.Errorf("ReferenceName(%q) = (%q,%t)，期望 (%q,%t)", c.in, name, ok, c.name, c.isRef)
		}
	}
}

func TestVaultResolvePathsDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UWS_VAULT_KEY", "")
	t.Setenv("UWS_VAULT_FILE", "")
	k, f := ResolvePaths(dir)
	if k != filepath.Join(dir, DefaultKeyName) || f != filepath.Join(dir, DefaultFileName) {
		t.Fatalf("默认路径应为 <dir>/vault.{key,age}，实际 (%q,%q)", k, f)
	}
	// 空 dir 退回状态目录（UWS_ENGINE_DIR 优先）。
	t.Setenv("UWS_ENGINE_DIR", dir)
	if k2, _ := ResolvePaths(""); k2 != filepath.Join(dir, DefaultKeyName) {
		t.Fatalf("空 dir 应退回 UWS_ENGINE_DIR，实际 %q", k2)
	}
	// 显式环境变量覆盖（多库隔离）。
	t.Setenv("UWS_VAULT_KEY", "/tmp/other.key")
	t.Setenv("UWS_VAULT_FILE", "/tmp/other.age")
	if k3, f3 := ResolvePaths(dir); k3 != "/tmp/other.key" || f3 != "/tmp/other.age" {
		t.Fatalf("环境变量应覆盖默认路径，实际 (%q,%q)", k3, f3)
	}
}
