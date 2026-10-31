package recipe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ActiveRecipeDir 返回「验证产物」目录：内嵌模板是 DRAFT，只有通过自证回放的站点
// 才会把 ACTIVE 产物写到这里，启动时覆盖内嵌同名模板（契约 §13 激活门禁）。
// UWS_RECIPE_ACTIVE_DIR 可覆盖目录；容器/测试里设空串表示不加载。
func ActiveRecipeDir() string {
	if v, ok := os.LookupEnv("UWS_RECIPE_ACTIVE_DIR"); ok {
		return strings.TrimSpace(v)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".uws", "recipes", "active")
}

// ActiveEvidence 是激活时的实测凭证（何时、用哪段 prompt、拿到什么回答）。
type ActiveEvidence struct {
	Prompt     string `json:"prompt"`
	HTTPStatus int    `json:"http_status"`
	Answer     string `json:"answer"`
	Bytes      int    `json:"bytes"`
	ElapsedMs  int64  `json:"elapsed_ms"`
	SessionID  string `json:"session_id,omitempty"`
}

// ActiveArtifact 是写入 overlay 的验证产物：不是裸 recipe，而是「recipe + 验证凭证」，
// 便于日后回答「什么时候验过、验出来什么」。
type ActiveArtifact struct {
	Recipe     *Recipe        `json:"recipe"`
	VerifiedAt string         `json:"verified_at"`
	Evidence   ActiveEvidence `json:"evidence"`
}

// SaveActive 原子写入某站点已激活的 recipe（先写临时文件再 rename；目录 0700、文件 0600）。
func SaveActive(dir string, r *Recipe, ev ActiveEvidence) (string, error) {
	if r == nil {
		return "", fmt.Errorf("recipe: 激活产物缺少 recipe")
	}
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("recipe: 激活产物目录为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("recipe: 无法创建激活产物目录 %s: %w", dir, err)
	}
	art := ActiveArtifact{Recipe: r, VerifiedAt: time.Now().Format(time.RFC3339), Evidence: ev}
	data, err := json.MarshalIndent(art, "", "  ")
	if err != nil {
		return "", fmt.Errorf("recipe: 无法序列化激活产物: %w", err)
	}
	target := filepath.Join(dir, strings.ToLower(strings.TrimSpace(r.Site))+".json")
	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return "", fmt.Errorf("recipe: 无法创建临时产物: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return "", fmt.Errorf("recipe: 无法写入临时产物: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("recipe: 无法关闭临时产物: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", fmt.Errorf("recipe: 无法收紧产物权限: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return "", fmt.Errorf("recipe: 无法落盘激活产物: %w", err)
	}
	return target, nil
}

// LoadActiveDir 把验证产物合并进注册表，覆盖内嵌同名站点；产物一律按 ACTIVE 装载
// （能落到这里就意味着当初通过了自证回放）。
func (reg *Registry) LoadActiveDir(dir string) (int, error) {
	if strings.TrimSpace(dir) == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		r := parseActiveArtifact(data)
		if r == nil {
			continue
		}
		r.Status = StatusActive
		if err := reg.Register(r); err == nil {
			count++
		}
	}
	return count, nil
}

// parseActiveArtifact 解析激活产物；兼容「裸 recipe」形态（无外层凭证）。
func parseActiveArtifact(data []byte) *Recipe {
	var art ActiveArtifact
	if err := json.Unmarshal(data, &art); err == nil && art.Recipe != nil {
		return art.Recipe
	}
	r, err := FromJSON(data)
	if err != nil {
		return nil
	}
	return r
}
