package recipe

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain 让单测不读用户机上的激活产物，保证结果只取决于仓库内嵌模板。
func TestMain(m *testing.M) {
	os.Setenv("UWS_RECIPE_ACTIVE_DIR", "")
	os.Exit(m.Run())
}

func overlayRecipe() *Recipe {
	return &Recipe{
		Site:   "gemini",
		Status: StatusDraft,
		Match:  MatchRule{Origin: "https://gemini.google.com", PathRe: "^/app"},
		Request: RequestSpec{
			Method:      "POST",
			URLTemplate: "/_/BardChatUi/data/StreamGenerate?bl={{bl}}",
		},
		Slots:    []Slot{{Name: "bl", Producer: ProducerPageGlobal, Path: "WIZ_global_data.cfb2h"}},
		Response: ResponseSpec{Kind: "ndjson_wrb_fr"},
	}
}

func TestOverlay_SaveThenLoadActivatesSite(t *testing.T) {
	dir := t.TempDir()
	path, err := SaveActive(dir, overlayRecipe(), ActiveEvidence{
		Prompt: "2+3=? 只回一个数字。", HTTPStatus: 200, Answer: "5", Bytes: 4096, ElapsedMs: 12000,
	})
	if err != nil {
		t.Fatalf("落盘激活产物失败: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("产物应落在目标目录: %s", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("产物不存在: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("产物权限应为 0600，got %o", perm)
	}

	reg := NewRegistry()
	n, err := reg.LoadActiveDir(dir)
	if err != nil || n != 1 {
		t.Fatalf("装载激活产物失败: n=%d err=%v", n, err)
	}
	got, ok := reg.Get("gemini")
	if !ok {
		t.Fatal("装载后应能按站点检索到 recipe")
	}
	if got.Status != StatusActive {
		t.Fatalf("产物装载后应为 ACTIVE（验证凭证即激活），got %s", got.Status)
	}
	if got.Request.URLTemplate != overlayRecipe().Request.URLTemplate {
		t.Fatalf("recipe 本体未随产物还原: %+v", got.Request)
	}
}

func TestOverlay_LoadBareRecipeAndMissingDir(t *testing.T) {
	dir := t.TempDir()
	// 裸 recipe（无外层凭证）也应能装载，并按 ACTIVE 归一。
	raw, err := overlayRecipe().ToJSON()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gemini.json"), raw, 0o600); err != nil {
		t.Fatalf("写裸 recipe 失败: %v", err)
	}
	reg := NewRegistry()
	if n, err := reg.LoadActiveDir(dir); err != nil || n != 1 {
		t.Fatalf("裸 recipe 装载失败: n=%d err=%v", n, err)
	}
	if got, ok := reg.Get("gemini"); !ok || got.Status != StatusActive {
		t.Fatalf("裸 recipe 应归一为 ACTIVE，got %+v", got)
	}

	empty := NewRegistry()
	if n, err := empty.LoadActiveDir(filepath.Join(dir, "does-not-exist")); err != nil || n != 0 {
		t.Fatalf("目录不存在应静默跳过: n=%d err=%v", n, err)
	}
	if n, err := empty.LoadActiveDir(""); err != nil || n != 0 {
		t.Fatalf("空目录配置应静默跳过: n=%d err=%v", n, err)
	}
}
