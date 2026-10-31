// cdp_session_factory 的启动前 profile 行为测试：持久/临时 user-data-dir 的准备、
// 以及落盘到 profile 的 Preferences 预置（关翻译、WebRTC 策略）。
//
// 2026-09-27 复杂度治理·阶段 1（单元 4）：纯目录逻辑（Dir/LockHolder/Reap/ProcessAlive）
// 已出包到 pkg/browser/profiledir 并在那里测试；本文件只留经 (*CDPFactory).openProfile
// 包装后的集成行为。
package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenProfileReusesTheSameDirectoryForSameID(t *testing.T) {
	root := t.TempDir()
	f := &CDPFactory{ProfileRoot: root}

	dir, persistent, err := f.openProfile(Config{ProfileID: "fp_a"})
	if err != nil {
		t.Fatalf("openProfile: %v", err)
	}
	if !persistent {
		t.Fatal("指定 ProfileID 应当是持久目录")
	}
	if want := filepath.Join(root, "fp_a"); dir != want {
		t.Fatalf("落盘位置不对: %s != %s", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("目录没建出来: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("持久 profile 权限必须是 0700，实际是 %04o", mode)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 「同机可复现」的第一层：同一身份，两次进来必须是同一个目录。
	again, _, err := f.openProfile(Config{ProfileID: "fp_a"})
	if err != nil || again != dir {
		t.Fatalf("同一身份两次指向了不同目录: %s vs %s (%v)", dir, again, err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("已存在的持久 profile 也必须收紧为 0700: %v", err)
	}
}

func TestOpenProfileWithoutIDStaysTemporary(t *testing.T) {
	f := &CDPFactory{ProfileRoot: t.TempDir()}
	dir, persistent, err := f.openProfile(Config{})
	if err != nil {
		t.Fatalf("openProfile: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if persistent {
		t.Fatal("不指定身份时必须还是临时目录（用完即焚，行为不许变）")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("临时目录没建出来: %v", err)
	}
}

func TestSeedChromePreferencesMergesInsteadOfOverwriting(t *testing.T) {
	profile := t.TempDir()
	dir := filepath.Join(profile, "Default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Preferences")
	existing := `{"profile":{"name":"worker-a","count":7},"translate":{"enabled":true}}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	seedChromePreferences(profile)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("合并结果不是合法 JSON: %v", err)
	}
	if got := string(top["translate"]); got != `{"enabled":false}` {
		t.Fatalf("translate 没被关掉: %s", got)
	}
	var prof struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(top["profile"], &prof); err != nil {
		t.Fatalf("profile 键被弄坏了: %v", err)
	}
	if prof.Name != "worker-a" || prof.Count != 7 {
		t.Fatalf("既有偏好被覆盖了: %+v", prof)
	}

	// 已经关过翻译：一次都不许多写（这是用户自己的偏好文件）。
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seedChromePreferences(profile)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("已关翻译时不该改写 Preferences")
	}
}

func TestDisableTranslatePrefLeavesOtherTopLevelKeysByteIdentical(t *testing.T) {
	data := []byte(`{"a":{"b":[1,2,{"c":3.5}]},"translate":{"enabled":true}}`)
	merged, changed, err := disableTranslatePref(data)
	if err != nil || !changed {
		t.Fatalf("应当发生一次合并: changed=%v err=%v", changed, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(merged, &top); err != nil {
		t.Fatal(err)
	}
	if string(top["a"]) != `{"b":[1,2,{"c":3.5}]}` {
		t.Fatalf("无关的顶层键被改写了: %s", top["a"])
	}
}
