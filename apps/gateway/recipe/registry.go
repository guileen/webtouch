package recipe

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"uws/apps/gateway/recipes"
)

// Registry 管理所有已知站点的 In-Page Direct Recipes。
type Registry struct {
	mu      sync.RWMutex
	recipes map[string]*Recipe
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{
		recipes: make(map[string]*Recipe),
	}
}

// Register 注册或覆盖一个 Recipe。
func (reg *Registry) Register(r *Recipe) error {
	if r == nil {
		return fmt.Errorf("registry: recipe 不能为空")
	}
	if err := r.Validate(); err != nil {
		return err
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.recipes[strings.ToLower(strings.TrimSpace(r.Site))] = r
	return nil
}

// Get 根据站点标识检索 Recipe。
func (reg *Registry) Get(site string) (*Recipe, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	r, ok := reg.recipes[strings.ToLower(strings.TrimSpace(site))]
	return r, ok
}

// FindByURL 根据当前页面 URL 匹配最适合的 Recipe。
func (reg *Registry) FindByURL(rawURL string) (*Recipe, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	for _, r := range reg.recipes {
		if r.Match.Matches(rawURL) {
			return r, true
		}
	}
	return nil, false
}

// List 列出当前已注册的所有 Recipe 副本。
func (reg *Registry) List() []*Recipe {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	out := make([]*Recipe, 0, len(reg.recipes))
	for _, r := range reg.recipes {
		out = append(out, r)
	}
	return out
}

// LoadFromFS 从文件系统抽象（如 embed.FS）批量加载 *.json Recipe。
func (reg *Registry) LoadFromFS(fsys fs.FS) (int, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, err
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			continue
		}
		r, err := FromJSON(data)
		if err != nil {
			continue
		}
		if err := reg.Register(r); err == nil {
			count++
		}
	}
	return count, nil
}

// LoadFromDir 从指定文件目录批量加载 *.json Recipe。
func (reg *Registry) LoadFromDir(dirPath string) (int, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		fullPath := filepath.Join(dirPath, entry.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		r, err := FromJSON(data)
		if err != nil {
			continue
		}
		if err := reg.Register(r); err == nil {
			count++
		}
	}
	return count, nil
}

var (
	defaultRegistryOnce sync.Once
	defaultRegistry     *Registry
)

// DefaultRegistry 返回预装了标准内嵌 recipes 的全局注册表单例。
func DefaultRegistry() *Registry {
	defaultRegistryOnce.Do(func() {
		defaultRegistry = NewRegistry()
		_, _ = defaultRegistry.LoadFromFS(recipes.FS)
		// 验证产物（~/.uws/recipes/active/）覆盖内嵌模板：内嵌一律 DRAFT，
		// 只有通过自证回放的站点才在这里带着 ACTIVE 与凭证出现（契约 §13）。
		_, _ = defaultRegistry.LoadActiveDir(ActiveRecipeDir())
	})
	return defaultRegistry
}
