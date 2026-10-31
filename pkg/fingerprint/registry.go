package fingerprint

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// 身份注册表（Identity Registry）：把「有哪些身份」「哪个账号用哪个身份」「哪个身份走
// 哪条代理」收进一个目录，成为这三件事的唯一事实源。
//
// 目录布局（默认 ~/.uws/identities，`UWS_IDENTITY_ROOT` 可覆盖）：
//
//	<profile_id>.json   身份档案本体（事实源：UA/时区/语言/屏幕/代理）
//	bindings.json       账号 → 身份 的绑定（**只放关系**，不复制档案字段）
//
// 三条不变式（Bind 对顺序调用强制，Conflicts 可发现磁盘上的损坏）：
//  1. 一个账号只绑一个身份（改绑要先 Unbind）；
//  2. 一个身份只被一个账号绑——两个账号共用同一份指纹，等于直接告诉平台「这俩是同一个人」；
//  3. 身份名 = 文件名 = 档案里的 profile_id（三者不一致就是数据已经坏了）。
//
// 另外 Conflicts 会报出「多个身份共用同一条代理出口」：这不是硬错误（可能是有意为之），
// 但出口 IP 是平台侧最强的关联信号之一，必须让人看见而不是替他决定。
const IdentityRootEnv = "UWS_IDENTITY_ROOT"

const (
	bindingsFileName     = "bindings.json"
	registryLockFileName = ".registry.lock"
)

// Registry 是一个身份目录。
type Registry struct {
	Root string
}

// DefaultIdentityRoot 返回身份目录：~/.uws/identities。
func DefaultIdentityRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv(IdentityRootEnv)); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("fingerprint: 定位用户目录失败: %w", err)
	}
	return filepath.Join(home, ".uws", "identities"), nil
}

// OpenRegistry 打开（必要时创建）一个身份目录。
func OpenRegistry(root string) (*Registry, error) {
	if strings.TrimSpace(root) == "" {
		resolved, err := DefaultIdentityRoot()
		if err != nil {
			return nil, err
		}
		root = resolved
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("fingerprint: 建身份目录失败: %w", err)
	}
	// 目录里放的是身份，权限收紧（同 profile 目录的处理）。
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("fingerprint: 收紧身份目录权限失败: %w", err)
	}
	return &Registry{Root: root}, nil
}

func (r *Registry) profilePath(id string) (string, error) {
	if err := ValidIdentity(id); err != nil {
		return "", fmt.Errorf("fingerprint: %w", err)
	}
	return filepath.Join(r.Root, id+".json"), nil
}

// List 返回注册表里的全部身份（按 profile_id 排序）。
func (r *Registry) List() ([]*Profile, error) {
	entries, err := os.ReadDir(r.Root)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: 读身份目录失败: %w", err)
	}
	profiles := []*Profile{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || name == bindingsFileName {
			continue
		}
		profile, err := r.Load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ProfileID < profiles[j].ProfileID })
	return profiles, nil
}

// Load 读一份身份；文件名与档案里的 profile_id 必须一致。
func (r *Registry) Load(id string) (*Profile, error) {
	path, err := r.profilePath(id)
	if err != nil {
		return nil, err
	}
	profile, err := Load(path)
	if err != nil {
		return nil, err
	}
	if profile.ProfileID != id {
		return nil, fmt.Errorf("fingerprint: %s 里的 profile_id 是 %q，与文件名不一致（身份名必须唯一对得上）", path, profile.ProfileID)
	}
	return profile, nil
}

// Save 写入一份身份（原子替换：先写临时文件再 rename，避免半个 JSON 留在盘上）。
func (r *Registry) Save(profile *Profile) error {
	if profile == nil {
		return fmt.Errorf("fingerprint: 档案为空")
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	path, err := r.profilePath(profile.ProfileID)
	if err != nil {
		return err
	}
	return r.withWriteLock(func() error {
		return writeFileAtomic(path, mustJSON(profile))
	})
}

// PathOf 返回某个身份的档案路径（给「用它跑一次」这类调用方拼命令）。
func (r *Registry) PathOf(id string) (string, error) { return r.profilePath(id) }

// ── 账号 ↔ 身份 绑定 ────────────────────────────────────────────────

type bindingsFile struct {
	Version  int               `json:"version"`
	Accounts map[string]string `json:"accounts"`
}

func (r *Registry) readBindings() (*bindingsFile, error) {
	raw, err := os.ReadFile(filepath.Join(r.Root, bindingsFileName))
	if os.IsNotExist(err) {
		return &bindingsFile{Version: 1, Accounts: map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fingerprint: 读绑定表失败: %w", err)
	}
	b := &bindingsFile{}
	if err := json.Unmarshal(raw, b); err != nil {
		return nil, fmt.Errorf("fingerprint: 解析绑定表失败: %w", err)
	}
	if b.Accounts == nil {
		b.Accounts = map[string]string{}
	}
	return b, nil
}

func (r *Registry) writeBindings(b *bindingsFile) error {
	b.Version = 1
	return writeFileAtomic(filepath.Join(r.Root, bindingsFileName), mustJSON(b))
}

// withWriteLock serializes every registry mutation across CLI processes. The
// advisory flock is released by the OS if a writer exits unexpectedly, unlike
// a create-only lock file which can permanently strand an identity directory.
// All mutations in this package use it; reads remain lock-free because writes
// use atomic rename and readers can therefore observe only an old or new file.
func (r *Registry) withWriteLock(write func() error) (result error) {
	lock, err := os.OpenFile(filepath.Join(r.Root, registryLockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("fingerprint: 打开注册表写锁失败: %w", err)
	}
	defer func() {
		if err := lock.Close(); err != nil && result == nil {
			result = fmt.Errorf("fingerprint: 关闭注册表写锁失败: %w", err)
		}
	}()
	if err := lock.Chmod(0o600); err != nil {
		return fmt.Errorf("fingerprint: 收紧注册表写锁权限失败: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("fingerprint: 获取注册表写锁失败: %w", err)
	}
	defer func() {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil && result == nil {
			result = fmt.Errorf("fingerprint: 释放注册表写锁失败: %w", err)
		}
	}()
	return write()
}

// Bindings 返回 账号 → 身份 的绑定表副本。
func (r *Registry) Bindings() (map[string]string, error) {
	b, err := r.readBindings()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(b.Accounts))
	for account, id := range b.Accounts {
		out[account] = id
	}
	return out, nil
}

// Bind 把账号钉死在一个身份上。两条不变式都在这里挡：账号只能有一个身份，身份也只能
// 属于一个账号——否则同一份指纹服务两个账号，平台一眼就能把两个账号关联起来。
func (r *Registry) Bind(account, profileID string) error {
	account = strings.TrimSpace(account)
	if account == "" {
		return fmt.Errorf("fingerprint: 账号名不能为空")
	}
	return r.withWriteLock(func() error {
		if _, err := r.Load(profileID); err != nil {
			return fmt.Errorf("fingerprint: 先有身份再绑账号: %w", err)
		}
		b, err := r.readBindings()
		if err != nil {
			return err
		}
		if current, ok := b.Accounts[account]; ok && current != profileID {
			return fmt.Errorf("fingerprint: 账号 %q 已经绑在身份 %q 上；改绑请先 `uws identity unbind %s`", account, current, account)
		}
		for other, id := range b.Accounts {
			if id == profileID && other != account {
				return fmt.Errorf("fingerprint: 身份 %q 已被账号 %q 占用；两个账号共用一份指纹等于告诉平台这是同一个人", profileID, other)
			}
		}
		b.Accounts[account] = profileID
		return r.writeBindings(b)
	})
}

// Unbind 解除一个账号的绑定（没绑也算成功：调用方要的就是「现在没绑」）。
func (r *Registry) Unbind(account string) error {
	account = strings.TrimSpace(account)
	if account == "" {
		return fmt.Errorf("fingerprint: 账号名不能为空")
	}
	return r.withWriteLock(func() error {
		b, err := r.readBindings()
		if err != nil {
			return err
		}
		if _, ok := b.Accounts[account]; !ok {
			return nil
		}
		delete(b.Accounts, account)
		return r.writeBindings(b)
	})
}

// IdentityOf 查一个账号绑的身份 id（没绑返回空串，不是错误）。
func (r *Registry) IdentityOf(account string) (string, error) {
	b, err := r.readBindings()
	if err != nil {
		return "", err
	}
	return b.Accounts[strings.TrimSpace(account)], nil
}

// Resolve 是「这个账号该用哪份身份」的正解：没绑就明确报错，不许偷偷用默认身份。
func (r *Registry) Resolve(account string) (*Profile, error) {
	id, err := r.IdentityOf(account)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("fingerprint: 账号 %q 还没绑身份；用 `uws identity bind %s <profile-id>` 绑一个", strings.TrimSpace(account), strings.TrimSpace(account))
	}
	return r.Load(id)
}

// AccountsOf 返回绑在某个身份上的账号（0 或 1 个）。
func (r *Registry) AccountsOf(profileID string) ([]string, error) {
	b, err := r.readBindings()
	if err != nil {
		return nil, err
	}
	accounts := []string{}
	for account, id := range b.Accounts {
		if id == profileID {
			accounts = append(accounts, account)
		}
	}
	sort.Strings(accounts)
	return accounts, nil
}

// proxyKey 把代理地址归一成「同一条出口」的比较键。
//
// 为什么不能拿裸串比：`socks5h://Host.example.com:1080/` 与 `socks5://host.example.com:1080`
// 指的是同一个出口（Chromium 的 SOCKS5 本来就让代理解析域名，见 profile.go 的 chromeProxyArg），
// 裸串比较会把「两个身份共用同一条出口」这件最该报的事漏掉——共用出口恰恰是平台侧最强的
// 关联信号之一，漏报比误报贵得多。归一：scheme 小写并把 socks5h→socks5、socks4a→socks4，
// host 小写，去掉默认端口与尾部路径/查询。
func proxyKey(raw string) string {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return strings.ToLower(trimmed)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "socks5h":
		scheme = "socks5"
	case "socks4a":
		scheme = "socks4"
	}
	port := parsed.Port()
	if port == defaultProxyPort(scheme) {
		port = ""
	}
	key := scheme + "://" + strings.ToLower(parsed.Hostname())
	if port != "" {
		key += ":" + port
	}
	return key
}

// defaultProxyPort 是各 scheme 的默认端口：显式写默认端口与省略它是同一个出口。
func defaultProxyPort(scheme string) string {
	switch scheme {
	case "socks5", "socks4":
		return "1080"
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// Conflicts 返回注册表里「必须让人看见」的问题：身份缺失、档案/文件名不一致（Load 阶段
// 就会炸，这里覆盖的是绑定表指向的空气）、以及多个身份共用同一条代理出口。
func (r *Registry) Conflicts() ([]string, error) {
	profiles, err := r.List()
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	byProxy := map[string][]string{}
	display := map[string]string{}
	for _, profile := range profiles {
		known[profile.ProfileID] = true
		if proxy := strings.TrimSpace(profile.Network.ProxyURL); proxy != "" {
			// 按**归一后的出口键**分组，不按裸串：`socks5h://Host:1080/` 与
			// `socks5://host:1080` 是同一条出口，裸串比较会把最该报的事漏掉。
			key := proxyKey(proxy)
			byProxy[key] = append(byProxy[key], profile.ProfileID)
			if _, seen := display[key]; !seen {
				display[key] = proxy
			}
		}
	}
	problems := []string{}
	for key, ids := range byProxy {
		if len(ids) > 1 {
			sort.Strings(ids)
			problems = append(problems, fmt.Sprintf("身份 %v 共用同一条代理出口配置 %s：出口 IP 是最强的关联信号之一", ids, RedactProxyURL(display[key])))
		}
	}
	b, err := r.readBindings()
	if err != nil {
		return nil, err
	}
	byIdentity := map[string][]string{}
	for account, id := range b.Accounts {
		byIdentity[id] = append(byIdentity[id], account)
		if !known[id] {
			problems = append(problems, fmt.Sprintf("账号 %q 绑的身份 %q 不存在（档案文件没了？）", account, id))
		}
	}
	for id, accounts := range byIdentity {
		if len(accounts) > 1 {
			sort.Strings(accounts)
			problems = append(problems, fmt.Sprintf("身份 %q 被多个账号绑定 %v：同一份指纹会把账号关联成同一个人", id, accounts))
		}
	}
	sort.Strings(problems)
	return problems, nil
}

// MarshalIndent 以稳定的缩进形式序列化一份档案（CLI 展示与落盘共用同一套编码）。
func MarshalIndent(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("fingerprint: 序列化失败: %w", err)
	}
	return string(append(data, '\n')), nil
}

func mustJSON(v any) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// 我们的类型都是可序列化的；真出问题是编程错误，不该静默写半个文件。
		panic(fmt.Sprintf("fingerprint: 序列化失败: %v", err))
	}
	return append(data, '\n')
}

// writeFileAtomic 先写临时文件再 rename：身份档案是长期资产，不能出现半个 JSON。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("fingerprint: 建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fingerprint: 写临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fingerprint: 关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("fingerprint: 收紧档案权限失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("fingerprint: 落盘失败: %w", err)
	}
	return nil
}
