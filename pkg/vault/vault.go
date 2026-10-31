// Package vault 是 WebTouch 的本地凭据库（2026-09-23 用户定案：用 age；GLOBAL #119）。
//
// 定位：个人主体（gl 域）的账号密码 / API Key 统一收口在一处，供 CLI 与协调器消费。
// **不做对外服务、不走网络、不进 git**——它是本机的一只保险箱，不是凭证 SaaS。
//
// 设计：
//   - 密文 `~/.uws/vault.age`（0600，age X25519 + ASCII armor）：整库序列化后一次性加密，
//     「没有 identity 就读不出任何一条」，连条目名都不泄露；
//   - identity `~/.uws/vault.key`（0600，`AGE-SECRET-KEY-1…`）：只落本机，不进仓、不进日志；
//   - 写盘一律「临时文件 + rename」原子替换；除显式 `Get` 外任何路径都不打印明文。
//
// 边界（诚实声明）：identity 与密文同目录时，防的是「误传 / 误提交 / 备份泄露 / 明文 grep」，
// 防不了「本机已被读取」。要更进一步就把 identity 挪进系统钥匙串（生物解锁），
// 那是下一步的加固项，不是本版前提。
package vault

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// 默认文件名（同目录 = ~/.uws，可用环境变量或显式路径覆盖）。
const (
	DefaultFileName = "vault.age"
	DefaultKeyName  = "vault.key"
	fileVersion     = 1
)

// ReferencePrefix 是「凭据直填」引用前缀：调用方在需要填表的值里写 `@vault:<名>`，
// 由执行端在本机解引用后再填进页面。
//
// 为什么要这层引用（2026-09-23）：DSL 的值会进 `--plan` 的 argv（本机 `ps` 可见）、
// 也进 shell history；把明文写在计划文本里等于把密码摊在进程表上。写成引用后，
// argv/计划文本/日志里只有条目名，明文只在执行进程内存在一次。
const ReferencePrefix = "@vault:"

// ReferenceName 解析 `@vault:<名>`；不是引用、或名为空时 ok=false（调用方应把
// 原值当作普通文本继续处理）。
func ReferenceName(value string) (string, bool) {
	v := strings.TrimSpace(value)
	if !strings.HasPrefix(v, ReferencePrefix) {
		return "", false
	}
	name := strings.TrimSpace(v[len(ReferencePrefix):])
	if name == "" {
		return "", false
	}
	return name, true
}

// DefaultDir 是本机 webtouch 状态目录（默认 ~/.uws，可用 UWS_ENGINE_DIR 覆盖，
// 与 CLI/守护进程同口径）。
func DefaultDir() string {
	if v := os.Getenv("UWS_ENGINE_DIR"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".uws")
	}
	return ".uws"
}

// ResolvePaths 按统一口径解析 identity 与密文路径：优先环境变量
// UWS_VAULT_KEY / UWS_VAULT_FILE（多库隔离、测试用），否则落在 <dir> 下的
// vault.key / vault.age。CLI 与引擎共用这一处，避免两边各写一套默认值。
func ResolvePaths(dir string) (keyPath, filePath string) {
	if dir == "" {
		dir = DefaultDir()
	}
	keyPath = os.Getenv("UWS_VAULT_KEY")
	filePath = os.Getenv("UWS_VAULT_FILE")
	if keyPath == "" {
		keyPath = filepath.Join(dir, DefaultKeyName)
	}
	if filePath == "" {
		filePath = filepath.Join(dir, DefaultFileName)
	}
	return keyPath, filePath
}

var (
	// ErrNotInitialized 表示本机还没有 vault（先跑 `uws vault init`）。
	ErrNotInitialized = errors.New("vault: 尚未初始化（先执行 `uws vault init`）")
	// ErrExists 表示目标文件已存在（init 默认不覆盖已有 vault）。
	ErrExists = errors.New("vault: 已存在（要重建请显式 --force，旧文件会被覆盖）")
	// ErrNotFound 表示没有这一条。
	ErrNotFound = errors.New("vault: 没有这一条")
)

// Entry 是库里的一条凭据（Value 只在显式读取时离开存储）。
type Entry struct {
	Value     string    `json:"value"`
	Note      string    `json:"note,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Item 是列表视图：**不含明文值**，只给名字、备注、更新时间与指纹（供人工比对）。
type Item struct {
	Name        string
	Note        string
	UpdatedAt   time.Time
	Fingerprint string
}

type vaultFile struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Store 是一只已解锁的 vault。零值不可用；用 Open / Init 构造。
type Store struct {
	keyPath  string
	filePath string
	id       *age.X25519Identity
	entries  map[string]Entry
	now      func() time.Time
}

// Init 生成新的 identity 与空 vault（默认不覆盖已有文件）。返回可用的 Store 与公开收件人
// （`age1…`，可安全抄写/备份，用于加密给这只 vault）。
func Init(keyPath, filePath string, force bool) (*Store, string, error) {
	if keyPath == "" || filePath == "" {
		return nil, "", errors.New("vault: 需要 key 与 vault 两个路径")
	}
	if !force {
		for _, p := range []string{keyPath, filePath} {
			if _, err := os.Stat(p); err == nil {
				return nil, "", fmt.Errorf("%w: %s", ErrExists, p)
			}
		}
	} else {
		// 显式重建：旧 identity 一旦换掉，旧密文就再也解不开（这正是 force 的语义），
		// 所以旧文件必须清掉，否则 O_EXCL 建不出来、也容易留下「新旧混在一起」的假象。
		for _, p := range []string{keyPath, filePath} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, "", fmt.Errorf("vault: 清理旧文件 %s: %w", p, err)
			}
		}
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, "", fmt.Errorf("vault: 生成 identity: %w", err)
	}
	if err := writeFileExclusive(keyPath, []byte(id.String()+"\n")); err != nil {
		return nil, "", err
	}
	s := &Store{
		keyPath:  keyPath,
		filePath: filePath,
		id:       id,
		entries:  make(map[string]Entry),
		now:      time.Now,
	}
	if err := s.flush(); err != nil {
		return nil, "", err
	}
	return s, id.Recipient().String(), nil
}

// Open 用本机 identity 解锁 vault。文件缺失/损坏/identity 不匹配都返回错误——
// 凭据库**fail-closed**：读不出来就是没有，绝不退化到明文或空库继续跑。
func Open(keyPath, filePath string) (*Store, error) {
	if keyPath == "" || filePath == "" {
		return nil, errors.New("vault: 需要 key 与 vault 两个路径")
	}
	rawKey, err := os.ReadFile(keyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w（缺 %s）", ErrNotInitialized, keyPath)
		}
		return nil, fmt.Errorf("vault: 读取 identity: %w", err)
	}
	id, err := age.ParseX25519Identity(firstLine(string(rawKey)))
	if err != nil {
		return nil, fmt.Errorf("vault: 解析 identity（%s）: %w", keyPath, err)
	}
	sealed, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w（缺 %s）", ErrNotInitialized, filePath)
		}
		return nil, fmt.Errorf("vault: 读取 %s: %w", filePath, err)
	}
	plain, err := decrypt(sealed, id)
	if err != nil {
		return nil, fmt.Errorf("vault: 解密 %s（identity 不匹配或文件损坏）: %w", filePath, err)
	}
	var f vaultFile
	if err := json.Unmarshal(plain, &f); err != nil {
		return nil, fmt.Errorf("vault: 解密后不是合法 JSON: %w", err)
	}
	if f.Entries == nil {
		f.Entries = make(map[string]Entry)
	}
	return &Store{
		keyPath:  keyPath,
		filePath: filePath,
		id:       id,
		entries:  f.Entries,
		now:      time.Now,
	}, nil
}

// Recipient 返回公开收件人（`age1…`）；可安全展示、备份、抄写。
func (s *Store) Recipient() string {
	if s == nil || s.id == nil {
		return ""
	}
	return s.id.Recipient().String()
}

// Paths 返回（identity 路径, 密文路径）。
func (s *Store) Paths() (string, string) {
	if s == nil {
		return "", ""
	}
	return s.keyPath, s.filePath
}

// Len 返回条目数。
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	return len(s.entries)
}

// Set 写入（或覆盖）一条凭据并立即落盘。空值被拒绝——写空值通常是调用方 bug，
// 而「悄悄写进一条空密码」比报错危险得多。
func (s *Store) Set(name, value, note string) error {
	if s == nil {
		return errors.New("vault: 未打开")
	}
	if name == "" {
		return errors.New("vault: 条目名不能为空")
	}
	if value == "" {
		return errors.New("vault: 值不能为空（要删除请用 Delete）")
	}
	s.entries[name] = Entry{Value: value, Note: note, UpdatedAt: s.now()}
	return s.flush()
}

// Get 读取一条凭据（唯一会吐出明文的地方，由调用方负责不落日志）。
func (s *Store) Get(name string) (Entry, bool) {
	if s == nil {
		return Entry{}, false
	}
	e, ok := s.entries[name]
	return e, ok
}

// Delete 删除一条凭据；不存在返回 false。
func (s *Store) Delete(name string) (bool, error) {
	if s == nil {
		return false, errors.New("vault: 未打开")
	}
	if _, ok := s.entries[name]; !ok {
		return false, nil
	}
	delete(s.entries, name)
	return true, s.flush()
}

// List 返回全部条目的列表视图（**不含明文**，只给指纹；按名字排序）。
func (s *Store) List() []Item {
	if s == nil {
		return nil
	}
	out := make([]Item, 0, len(s.entries))
	for name, e := range s.entries {
		out = append(out, Item{
			Name:        name,
			Note:        e.Note,
			UpdatedAt:   e.UpdatedAt,
			Fingerprint: Fingerprint(e.Value),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Fingerprint 是值的短指纹（sha256 前 12 位十六进制）——用于双端比对「搬对了没有」，
// 与工作区 secret-handoff 口径一致：可以展示，不能反推。
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// flush 加密整库并原子落盘。
func (s *Store) flush() error {
	plain, err := json.MarshalIndent(vaultFile{Version: fileVersion, Entries: s.entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("vault: 编码: %w", err)
	}
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, s.id.Recipient())
	if err != nil {
		return fmt.Errorf("vault: 加密: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return fmt.Errorf("vault: 加密写入: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("vault: 加密收尾: %w", err)
	}
	if err := aw.Close(); err != nil {
		return fmt.Errorf("vault: armor 收尾: %w", err)
	}
	return writeFileAtomic(s.filePath, buf.Bytes())
}

func decrypt(sealed []byte, id *age.X25519Identity) ([]byte, error) {
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(sealed)), id)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return s[:i]
		}
	}
	return s
}

// writeFileExclusive 以 0600 新建文件（已存在即失败；不覆盖别人的密钥）。
func writeFileExclusive(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("vault: 建目录: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, path)
		}
		return fmt.Errorf("vault: 建 %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("vault: 写 %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("vault: 关闭 %s: %w", path, err)
	}
	return nil
}

// writeFileAtomic 临时文件 + rename 原子替换（0600；失败不留半截密文）。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("vault: 建目录: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".vault-*.tmp")
	if err != nil {
		return fmt.Errorf("vault: 建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("vault: 权限: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("vault: 写入: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("vault: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("vault: 替换 %s: %w", path, err)
	}
	return nil
}
