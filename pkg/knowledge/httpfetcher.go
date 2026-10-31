// HTTP 对象存储 fetcher（USER_ACTION #7 / spec/knowledge-versioning.md §2-§3）。
//
// 与 pipeline/exporter（M6B）产出的 artifact 布局对齐：
//
//	版本目录/manifest.json + checksums.sha256 + uws_l1.db + classifier.onnx + uws_l2_default.db
//
// 下载全部文件 → 校验大小与 SHA256 → 加载 uws_l1.db 元信息（schema/model 哈希、
// ACTIVE 规则索引、规则状态表、内存估算），产出 knowledge.Artifact 供双缓冲热更。
//
// 传输层：HTTP(S) 网关 / CDN / 预签名 URL 直连（零外部依赖）；
// s3:// 等对象键在配置了 baseURL 时映射为 baseURL/version。S3 SigV4 / OSS 原生
// 签名留待真实部署（见 GLOBAL.md §5 裁决 #10）。
package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// httpFetchTimeout 是单次 artifact 拉取的默认超时（spec §3.2：下载 < 10s）。
	httpFetchTimeout = 10 * time.Second
	// maxArtifactBytes 是单文件下载保护上限（512MB）。
	maxArtifactBytes = 512 << 20
	// baseMemoryOverheadMB 是双缓冲驻留的基础内存开销（SQLite 页缓存 + 句柄）。
	baseMemoryOverheadMB = 8
)

// errArtifactNotFound 对应 ReloadResponse 契约层断言的 "artifact not found"。
var errArtifactNotFound = errors.New("artifact not found")

// manifestFile 对应 exporter manifest.files.<name>。
type manifestFile struct {
	Hash      string `json:"hash"`
	SizeBytes int64  `json:"size_bytes"`
}

// exportManifest 对应 pipeline/exporter ExportManifest（子集）。
type exportManifest struct {
	Version         string                  `json:"version"`
	ReleaseDate     string                  `json:"release_date"`
	ArtifactURIBase string                  `json:"artifact_uri_base"`
	Files           map[string]manifestFile `json:"files"`
	Compatibility   struct {
		MinEngineVersion string `json:"min_engine_version"`
		MaxEngineVersion string `json:"max_engine_version"`
	} `json:"compatibility"`
	RollbackFrom   string         `json:"rollback_from"`
	DeployMetadata map[string]any `json:"deploy_metadata"`
}

// HTTPFetcher 实现 Fetcher：从 HTTP(S) 对象存储网关下载并校验知识库产物。
type HTTPFetcher struct {
	baseURL string // 例如 http://127.0.0.1:8000/artifacts；空时仅支持 http(s) artifactURI
	client  *http.Client
}

// NewHTTPFetcher 构造 HTTP fetcher；baseURL 必须为 http(s) 且非空主机。
func NewHTTPFetcher(baseURL string, client *http.Client) (*HTTPFetcher, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("artifact base URL 必须是 http(s): %q", baseURL)
		}
	}
	if client == nil {
		client = &http.Client{Timeout: httpFetchTimeout}
	}
	return &HTTPFetcher{baseURL: baseURL, client: client}, nil
}

// Fetch 下载并校验指定版本的 artifact。
// artifactURI 为 http(s) 时直连该目录；否则视为对象键，映射为 baseURL/version。
func (f *HTTPFetcher) Fetch(ctx context.Context, version, artifactURI string) (*Artifact, error) {
	base, err := f.resolveBase(version, artifactURI)
	if err != nil {
		return nil, err
	}
	m, err := f.fetchManifest(ctx, base)
	if err != nil {
		return nil, err
	}
	if m.Version != version {
		return nil, fmt.Errorf("manifest 版本不匹配: %s != %s", m.Version, version)
	}
	files, err := f.downloadFiles(ctx, base, m)
	if err != nil {
		return nil, err
	}
	return loadArtifact(version, m, files)
}

func (f *HTTPFetcher) resolveBase(version, artifactURI string) (string, error) {
	if strings.HasPrefix(artifactURI, "http://") || strings.HasPrefix(artifactURI, "https://") {
		return strings.TrimRight(artifactURI, "/"), nil
	}
	if f.baseURL != "" {
		if version == "" {
			return "", errors.New("version 为空且 artifactURI 非 http(s)")
		}
		return strings.TrimRight(f.baseURL, "/") + "/" + version, nil
	}
	return "", fmt.Errorf("不支持的 artifact URI（需要 http(s) 或配置 base URL）: %q", artifactURI)
}

func (f *HTTPFetcher) fetchManifest(ctx context.Context, base string) (*exportManifest, error) {
	data, err := f.get(ctx, base+"/manifest.json")
	if err != nil {
		if errors.Is(err, errArtifactNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("下载 manifest 失败: %w", err)
	}
	var m exportManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析 manifest.json 失败: %w", err)
	}
	return &m, nil
}

func (f *HTTPFetcher) downloadFiles(ctx context.Context, base string, m *exportManifest) (map[string][]byte, error) {
	files := make(map[string][]byte, len(m.Files))
	for name, meta := range m.Files {
		if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
			return nil, fmt.Errorf("非法 artifact 文件名: %q", name)
		}
		data, err := f.get(ctx, base+"/"+name)
		if err != nil {
			if errors.Is(err, errArtifactNotFound) {
				return nil, fmt.Errorf("artifact 文件缺失 %s: %w", name, err)
			}
			return nil, fmt.Errorf("下载 %s 失败: %w", name, err)
		}
		if int64(len(data)) != meta.SizeBytes {
			return nil, fmt.Errorf("artifact 文件大小不符 %s: got %d want %d", name, len(data), meta.SizeBytes)
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		want := strings.TrimPrefix(meta.Hash, "sha256:")
		if !strings.EqualFold(got, want) {
			return nil, fmt.Errorf("artifact 校验和失败 %s: got %s want %s", name, got, want)
		}
		files[name] = data
	}
	return files, nil
}

func (f *HTTPFetcher) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", errArtifactNotFound, rawURL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxArtifactBytes))
}

// loadArtifact 把已校验的产物加载为 Artifact 元信息（规则状态表 + 哈希 + 内存估算）。
// uws_l1.db 写入临时文件后以 SQLite 只读加载，加载完即清理。
func loadArtifact(version string, m *exportManifest, files map[string][]byte) (*Artifact, error) {
	dbBytes, ok := files["uws_l1.db"]
	if !ok {
		return nil, errors.New("artifact 缺少 uws_l1.db")
	}
	tmp, err := os.CreateTemp("", "uws-artifact-*.db")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(dbBytes); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		return nil, fmt.Errorf("打开知识库失败: %w", err)
	}
	defer db.Close()
	// 旧 version artifact（v1.5.2 前）无 struct_hash 列：幂等补列，保证
	// 后续规则快照装载不因 no such column 失败（Tier 1.5 结构通道接线）。
	if _, err := db.Exec(
		`ALTER TABLE uws_rules ADD COLUMN struct_hash INTEGER NOT NULL DEFAULT 0`,
	); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("迁移 struct_hash 列失败: %w", err)
	}

	var schemaHash, modelHash, artifactURI string
	err = db.QueryRow(
		`SELECT schema_hash, model_hash, artifact_uri FROM knowledge_versions WHERE version = ?`,
		version,
	).Scan(&schemaHash, &modelHash, &artifactURI)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("knowledge_versions 缺少版本 %s", version)
	}
	if err != nil {
		return nil, fmt.Errorf("查询 knowledge_versions 失败: %w", err)
	}

	var keyCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM uws_rules WHERE LOWER(status) = 'active'`,
	).Scan(&keyCount); err != nil {
		return nil, fmt.Errorf("统计 ACTIVE 规则失败: %w", err)
	}

	rows, err := db.Query(
		`SELECT rule_type,
		        COALESCE(text_pattern, ''), COALESCE(selector, ''), COALESCE(role, ''),
		        COALESCE(action, ''), COALESCE(semantic_label, ''),
		        COALESCE(confidence, 0), COALESCE(source, ''), COALESCE(status, ''),
		        COALESCE(struct_hash, 0)
		   FROM uws_rules`,
	)
	if err != nil {
		return nil, fmt.Errorf("加载规则状态失败: %w", err)
	}
	defer rows.Close()
	rules := make(map[string]string)
	var rulesFull []RuleSnapshot
	for rows.Next() {
		var ruleType, textPattern, selector, role, action, semanticLabel, source, status string
		var confidence float64
		var structHash int64
		if err := rows.Scan(&ruleType, &textPattern, &selector, &role, &action,
			&semanticLabel, &confidence, &source, &status, &structHash); err != nil {
			return nil, fmt.Errorf("扫描规则失败: %w", err)
		}
		key := ruleType + ":" + semanticLabel
		if semanticLabel == "" {
			key = ruleType + ":" + textPattern
		}
		rules[key] = strings.ToUpper(status)
		rulesFull = append(rulesFull, RuleSnapshot{
			RuleType: ruleType, TextPattern: textPattern, Selector: selector,
			Role: role, Action: action, SemanticLabel: semanticLabel,
			Confidence: confidence, Source: source, Status: status,
			StructHash: uint64(structHash),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// L2 同义词快照（site_synonyms；exporter 与 matcher schema 对齐）。
	synRows, err := db.Query(
		`SELECT site_domain, synonym, base_weight, status, hit_count, COALESCE(source_rule_id, 0)
		   FROM site_synonyms`,
	)
	if err != nil {
		return nil, fmt.Errorf("加载同义词失败: %w", err)
	}
	defer synRows.Close()
	var syns []SynonymSnapshot
	for synRows.Next() {
		var siteDomain, term, status string
		var baseWeight float64
		var hitCount, sourceRuleID int64
		if err := synRows.Scan(&siteDomain, &term, &baseWeight, &status, &hitCount, &sourceRuleID); err != nil {
			return nil, fmt.Errorf("扫描同义词失败: %w", err)
		}
		syns = append(syns, SynonymSnapshot{
			SiteDomain: siteDomain, Term: term, BaseWeight: baseWeight,
			Status: status, HitCount: int(hitCount), SourceRuleID: sourceRuleID,
		})
	}
	if err := synRows.Err(); err != nil {
		return nil, err
	}

	// 黑名单快照（uws_negative_keywords）。
	kwRows, err := db.Query(`SELECT keyword FROM uws_negative_keywords ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("加载黑名单失败: %w", err)
	}
	defer kwRows.Close()
	var kws []string
	for kwRows.Next() {
		var kw string
		if err := kwRows.Scan(&kw); err != nil {
			return nil, fmt.Errorf("扫描黑名单失败: %w", err)
		}
		kws = append(kws, kw)
	}
	if err := kwRows.Err(); err != nil {
		return nil, err
	}

	if m.ArtifactURIBase != "" {
		artifactURI = m.ArtifactURIBase
	}
	return &Artifact{
		Version:           version,
		SchemaHash:        schemaHash,
		ModelHash:         modelHash,
		ArtifactURI:       artifactURI,
		MinEngineVersion:  m.Compatibility.MinEngineVersion,
		MaxEngineVersion:  m.Compatibility.MaxEngineVersion,
		KeyRuleIndexCount: keyCount,
		OnnxReady:         len(files["classifier.onnx"]) > 0,
		OnnxModel:         files["classifier.onnx"],
		MemoryEstimateMB:  memoryEstimateMB(m, files),
		Rules:             rules,
		RulesFull:         rulesFull,
		Synonyms:          syns,
		NegativeKeywords:  kws,
	}, nil
}

// memoryEstimateMB 优先取 manifest deploy_metadata.memory_estimate_mb，
// 缺省按产物总字节估算（基础 8MB + 向上取整 MB）。
func memoryEstimateMB(m *exportManifest, files map[string][]byte) int64 {
	if v, ok := m.DeployMetadata["memory_estimate_mb"]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int:
			return int64(n)
		case int64:
			return n
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i
			}
		}
	}
	var total int64
	for _, data := range files {
		total += int64(len(data))
	}
	return baseMemoryOverheadMB + (total+(1<<20)-1)/(1<<20)
}

var _ Fetcher = (*HTTPFetcher)(nil)
