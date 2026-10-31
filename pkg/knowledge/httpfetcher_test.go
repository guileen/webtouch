// HTTP 对象存储 fetcher 单元测试（USER_ACTION #7 / tdd-checklist §3.4 契约层前置）。
// 用 httptest 服务与 pipeline/exporter 同构的产物目录，验证下载、SHA256 校验、
// SQLite 加载、规则状态归一与内存估算。
package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

type testRuleRow struct {
	ruleType      string
	textPattern   string
	semanticLabel string
	status        string
	action        string
	confidence    float64
	structHash    int64
}

type testSynonymRow struct {
	siteDomain string
	term       string
	status     string
}

type testVersion struct {
	version   string
	minEngine string
	maxEngine string
	rules     []testRuleRow
	synonyms  []testSynonymRow
	keywords  []string
	memMB     int64 // >0 时写入 deploy_metadata.memory_estimate_mb
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeTestL1DB 写入与 exporter SCHEMA 对齐的最小 uws_l1.db（fetcher 查询所需表）。
func writeTestL1DB(t *testing.T, path, version, schemaHash, modelHash, artifactURI string,
	rules []testRuleRow, synonyms []testSynonymRow, keywords []string) {
	t.Helper()
	con, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer con.Close()

	for _, st := range []string{
		`CREATE TABLE uws_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_type TEXT NOT NULL,
			text_pattern TEXT,
			selector TEXT,
			role TEXT,
			action TEXT NOT NULL,
			semantic_label TEXT,
			confidence REAL NOT NULL,
			source TEXT,
			status TEXT,
			struct_hash INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE site_synonyms (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			site_domain TEXT NOT NULL DEFAULT '*',
			synonym TEXT NOT NULL,
			base_weight REAL NOT NULL DEFAULT 0.2,
			status TEXT NOT NULL DEFAULT 'TENTATIVE',
			hit_count INTEGER NOT NULL DEFAULT 0,
			source_rule_id INTEGER,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE uws_negative_keywords (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			keyword TEXT NOT NULL UNIQUE,
			source TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE knowledge_versions (
			version TEXT NOT NULL,
			schema_hash TEXT NOT NULL,
			model_hash TEXT NOT NULL,
			artifact_uri TEXT NOT NULL
		)`,
	} {
		if _, err := con.Exec(st); err != nil {
			t.Fatalf("exec %q: %v", st, err)
		}
	}
	for _, r := range rules {
		if _, err := con.Exec(
			`INSERT INTO uws_rules
				(rule_type, text_pattern, action, semantic_label, confidence, source, status, struct_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ruleType, r.textPattern, r.action, r.semanticLabel, r.confidence, "test", r.status,
			r.structHash,
		); err != nil {
			t.Fatalf("insert rule: %v", err)
		}
	}
	for _, s := range synonyms {
		if _, err := con.Exec(
			`INSERT INTO site_synonyms (site_domain, synonym, status) VALUES (?, ?, ?)`,
			s.siteDomain, s.term, s.status,
		); err != nil {
			t.Fatalf("insert synonym: %v", err)
		}
	}
	for _, kw := range keywords {
		if _, err := con.Exec(
			`INSERT OR IGNORE INTO uws_negative_keywords (keyword, source) VALUES (?, ?)`,
			kw, "test",
		); err != nil {
			t.Fatalf("insert keyword: %v", err)
		}
	}
	if _, err := con.Exec(
		`INSERT INTO knowledge_versions (version, schema_hash, model_hash, artifact_uri) VALUES (?, ?, ?, ?)`,
		version, schemaHash, modelHash, artifactURI,
	); err != nil {
		t.Fatalf("insert version: %v", err)
	}
}

// writeVersionTree 生成一个版本目录（parent/version/...），返回 parent 根。
// postWrite/postRemove 在 manifest 落盘后应用，用于制造校验失败/缺文件场景。
func writeVersionTree(t *testing.T, parent string, v testVersion, onnx []byte,
	postWrite map[string][]byte, postRemove []string) string {
	t.Helper()
	dir := filepath.Join(parent, v.version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	l1Path := filepath.Join(dir, "uws_l1.db")
	writeTestL1DB(t, l1Path, v.version, "sha256:schema-"+v.version, "sha256:"+sha256Hex(onnx),
		"s3://uws-artifacts/"+v.version, v.rules, v.synonyms, v.keywords)
	if err := os.WriteFile(filepath.Join(dir, "classifier.onnx"), onnx, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uws_l2_default.db"), []byte("l2 placeholder\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := map[string]manifestFile{}
	for _, name := range []string{"uws_l1.db", "classifier.onnx", "uws_l2_default.db"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = manifestFile{Hash: "sha256:" + sha256Hex(data), SizeBytes: int64(len(data))}
	}
	deploy := map[string]any{}
	if v.memMB > 0 {
		deploy["memory_estimate_mb"] = v.memMB
	}
	m := exportManifest{
		Version:         v.version,
		ArtifactURIBase: "s3://uws-artifacts/" + v.version,
		Files:           files,
		DeployMetadata:  deploy,
	}
	m.Compatibility.MinEngineVersion = v.minEngine
	m.Compatibility.MaxEngineVersion = v.maxEngine
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	for _, name := range []string{"uws_l1.db", "classifier.onnx", "uws_l2_default.db"} {
		fmt.Fprintf(&sb, "%s  %s\n", strings.TrimPrefix(files[name].Hash, "sha256:"), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.sha256"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, data := range postWrite {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range postRemove {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return parent
}

func serveArtifacts(t *testing.T, root string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(srv.Close)
	return srv
}

func testRules() []testRuleRow {
	return []testRuleRow{
		{ruleType: "text_pattern", textPattern: "登录", semanticLabel: "登录", status: "active", action: "click", confidence: 0.95, structHash: 0x12345678},
		{ruleType: "text_pattern", textPattern: "submit", semanticLabel: "", status: "ACTIVE", action: "click", confidence: 0.9},
		{ruleType: "text_pattern", textPattern: "hidden", semanticLabel: "x", status: "staging", action: "click", confidence: 0.5},
	}
}

// ── 成功路径：下载 + SHA256 校验 + SQLite 加载 ─────────────────────────

func TestHTTPFetcherFetchSuccess(t *testing.T) {
	parent := t.TempDir()
	root := writeVersionTree(t, parent, testVersion{
		version: "v1.4.0", minEngine: "v1.4.0", maxEngine: "v1.4.x", rules: testRules(),
		synonyms: []testSynonymRow{
			{siteDomain: "*", term: "Sign in", status: "TENTATIVE"},
		},
		keywords: []string{"广告", "banner_zone"},
	}, []byte("onnx-model-bytes"), nil, nil)
	srv := serveArtifacts(t, root)

	f, err := NewHTTPFetcher(srv.URL, nil)
	if err != nil {
		t.Fatalf("NewHTTPFetcher: %v", err)
	}
	art, err := f.Fetch(context.Background(), "v1.4.0", "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if art.Version != "v1.4.0" || art.MinEngineVersion != "v1.4.0" || art.MaxEngineVersion != "v1.4.x" {
		t.Fatalf("版本/兼容字段错误: %+v", art)
	}
	if art.SchemaHash != "sha256:schema-v1.4.0" {
		t.Fatalf("SchemaHash = %q", art.SchemaHash)
	}
	if art.ModelHash != "sha256:"+sha256Hex([]byte("onnx-model-bytes")) {
		t.Fatalf("ModelHash = %q", art.ModelHash)
	}
	if art.KeyRuleIndexCount != 2 {
		t.Fatalf("KeyRuleIndexCount = %d, want 2（仅 ACTIVE）", art.KeyRuleIndexCount)
	}
	if !art.OnnxReady {
		t.Fatal("classifier.onnx 存在应 OnnxReady=true")
	}
	if art.Rules["text_pattern:登录"] != "ACTIVE" || art.Rules["text_pattern:submit"] != "ACTIVE" ||
		art.Rules["text_pattern:x"] != "STAGING" {
		t.Fatalf("规则状态归一错误: %+v", art.Rules)
	}
	if art.MemoryEstimateMB != 9 {
		t.Fatalf("MemoryEstimateMB = %d, want 9（8MB 基础 + 小产物向上取整 1MB）", art.MemoryEstimateMB)
	}
	if art.ArtifactURI != "s3://uws-artifacts/v1.4.0" {
		t.Fatalf("ArtifactURI = %q", art.ArtifactURI)
	}
	if len(art.RulesFull) != 3 {
		t.Fatalf("RulesFull 长度 = %d, want 3", len(art.RulesFull))
	}
	r0 := art.RulesFull[0]
	if r0.RuleType != "text_pattern" || r0.TextPattern != "登录" || r0.Action != "click" ||
		r0.Confidence != 0.95 || r0.Status != "active" || r0.Source != "test" ||
		r0.StructHash != 0x12345678 {
		t.Fatalf("RulesFull[0] 快照错误: %+v", r0)
	}
	if len(art.Synonyms) != 1 || art.Synonyms[0].Term != "Sign in" ||
		art.Synonyms[0].Status != "TENTATIVE" || art.Synonyms[0].SiteDomain != "*" {
		t.Fatalf("Synonyms 快照错误: %+v", art.Synonyms)
	}
	if len(art.NegativeKeywords) != 2 || art.NegativeKeywords[0] != "广告" ||
		art.NegativeKeywords[1] != "banner_zone" {
		t.Fatalf("NegativeKeywords 快照错误: %+v", art.NegativeKeywords)
	}
}

// ── artifactURI 直连（http(s) 时忽略 baseURL 的 version 拼接） ──────────

func TestHTTPFetcherDirectArtifactURI(t *testing.T) {
	parent := t.TempDir()
	root := writeVersionTree(t, parent, testVersion{
		version: "v1.4.0", minEngine: "v1.3.0", rules: testRules(),
	}, []byte("onnx"), nil, nil)
	srv := serveArtifacts(t, root)

	f, err := NewHTTPFetcher("http://example.invalid/ignored", nil)
	if err != nil {
		t.Fatalf("NewHTTPFetcher: %v", err)
	}
	art, err := f.Fetch(context.Background(), "v1.4.0", srv.URL+"/v1.4.0")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if art.Version != "v1.4.0" {
		t.Fatalf("version = %q", art.Version)
	}
}

// ── 错误路径 ───────────────────────────────────────────────────────────

func TestHTTPFetcherChecksumMismatch(t *testing.T) {
	parent := t.TempDir()
	root := writeVersionTree(t, parent, testVersion{
		version: "v1.4.0", minEngine: "v1.4.0", rules: testRules(),
	}, []byte("onnx-model-bytes"),
		map[string][]byte{"classifier.onnx": []byte("XXXXXXXXXXXXXXXX")}, nil)
	srv := serveArtifacts(t, root)

	f, err := NewHTTPFetcher(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Fetch(context.Background(), "v1.4.0", "")
	if err == nil || !strings.Contains(err.Error(), "校验和失败") {
		t.Fatalf("应报告校验和失败, got %v", err)
	}
}

func TestHTTPFetcherMissingArtifactFile(t *testing.T) {
	parent := t.TempDir()
	root := writeVersionTree(t, parent, testVersion{
		version: "v1.4.0", minEngine: "v1.4.0", rules: testRules(),
	}, []byte("onnx"), nil, []string{"uws_l2_default.db"})
	srv := serveArtifacts(t, root)

	f, err := NewHTTPFetcher(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Fetch(context.Background(), "v1.4.0", "")
	if err == nil || !strings.Contains(err.Error(), "uws_l2_default.db") {
		t.Fatalf("应报告缺失文件, got %v", err)
	}
}

func TestHTTPFetcherVersionNotFound(t *testing.T) {
	parent := t.TempDir()
	root := writeVersionTree(t, parent, testVersion{
		version: "v1.4.0", minEngine: "v1.4.0", rules: testRules(),
	}, []byte("onnx"), nil, nil)
	srv := serveArtifacts(t, root)

	f, err := NewHTTPFetcher(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Fetch(context.Background(), "v9.9.9", "")
	if err == nil || !strings.Contains(err.Error(), "artifact not found") {
		t.Fatalf("应报告 artifact not found, got %v", err)
	}
}

func TestHTTPFetcherManifestVersionMismatch(t *testing.T) {
	parent := t.TempDir()
	root := writeVersionTree(t, parent, testVersion{
		version: "v1.4.0", minEngine: "v1.4.0", rules: testRules(),
	}, []byte("onnx"),
		map[string][]byte{"manifest.json": []byte(`{"version":"v9.9.9"}`)}, nil)
	srv := serveArtifacts(t, root)

	f, err := NewHTTPFetcher(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Fetch(context.Background(), "v1.4.0", "")
	if err == nil || !strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("应报告 manifest 版本不匹配, got %v", err)
	}
}

func TestHTTPFetcherMemoryEstimateFromManifest(t *testing.T) {
	parent := t.TempDir()
	root := writeVersionTree(t, parent, testVersion{
		version: "v1.4.0", minEngine: "v1.4.0", rules: testRules(), memMB: 500,
	}, []byte("onnx"), nil, nil)
	srv := serveArtifacts(t, root)

	f, err := NewHTTPFetcher(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	art, err := f.Fetch(context.Background(), "v1.4.0", "")
	if err != nil {
		t.Fatal(err)
	}
	if art.MemoryEstimateMB != 500 {
		t.Fatalf("MemoryEstimateMB = %d, want 500（取自 manifest deploy_metadata）", art.MemoryEstimateMB)
	}
}

func TestHTTPFetcherRejectsNonHTTPScheme(t *testing.T) {
	if _, err := NewHTTPFetcher("s3://uws-artifacts", nil); err == nil ||
		!strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("非 http(s) base URL 应被拒绝, got %v", err)
	}
}

func TestHTTPFetcherArtifactURIDirectSchemeRequired(t *testing.T) {
	parent := t.TempDir()
	root := writeVersionTree(t, parent, testVersion{
		version: "v1.4.0", minEngine: "v1.4.0", rules: testRules(),
	}, []byte("onnx"), nil, nil)
	srv := serveArtifacts(t, root)

	f, err := NewHTTPFetcher(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	// s3:// URI 在配置了 baseURL 时映射为 baseURL/version（生产 CDN/预签名网关直连）。
	art, err := f.Fetch(context.Background(), "v1.4.0", "s3://uws-artifacts/v1.4.0")
	if err != nil {
		t.Fatalf("s3:// URI 应映射到 baseURL/version: %v", err)
	}
	if art.Version != "v1.4.0" {
		t.Fatalf("version = %q", art.Version)
	}
}
