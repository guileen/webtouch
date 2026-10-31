// Package profiledir 管「持久身份 profile 目录」与「自起浏览器临时 profile」的解析、校验、
// 加锁判定与陈旧残留回收。
//
// 2026-09-27 从 pkg/browser/profile_dir.go 出包（复杂度治理·阶段 1，单元 4）：本包是**纯逻辑**，
// 不依赖 CDP/会话；pkg/browser 保留同名薄包装（DefaultProfileRoot / ProfileDir /
// ReapStaleTempProfiles 与 (*CDPFactory).openProfile）供既有消费方零改动使用。
//
// 用户口径：浏览器指纹要「多个身份、同机可复现、将来跨端可同步」。指纹的第一层不是渲染噪声，
// 而是**身份本身**——Chrome 每次都用临时 user-data-dir 启动，就等于每次都是一台新设备
// （Cookie、LocalStorage、扩展、缓存全从零开始），此时再多的注入也只是给一个空身份化妆。
//
// 所以这一层只做一件小事：会话可以指名一个 profile id，落盘到 `~/.uws/profiles/<id>/`，
// 同一个 id 每次进来都是同一台设备；**不指名时行为与以前完全一致**（临时目录、用完即焚）。
//
// 边界（诚实记账）：
//   - 只解决「同机可复现」。跨端一致性是 Phase 3/4 的事（State Bundle + 租约）。
//   - 单写者是操作系统给的：Chrome 自己用 SingletonLock 保证一个 user-data-dir 只有
//     一个实例。我们只把这个锁翻译成一句人话，不另造一套锁。
//   - 完整方案：docs/fingerprint-and-viewer-final-spec-2026-09-12.md §2。
package profiledir

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"uws/pkg/fingerprint"
)

// RootEnv 覆盖持久 profile 的根目录（测试、多环境部署用）。
const RootEnv = "UWS_PROFILE_ROOT"

// DefaultRoot 返回持久 profile 根目录：~/.uws/profiles。
func DefaultRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv(RootEnv)); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("browser: 定位用户目录失败: %w", err)
	}
	return filepath.Join(home, ".uws", "profiles"), nil
}

// ResolveRoot 决定用哪个根目录：显式参数 > 环境变量 > 默认。
func ResolveRoot(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	return DefaultRoot()
}

// Dir 把身份名落成一个目录，并挡住会逃出根目录的名字。
//
// id 只允许 [A-Za-z0-9._-]，长度 1..64，不能是 "."/".."/以 "." 开头：这个值将来会从
// 配置或网络下发，路径拼接必须先校验再拼（`../../etc` 这类名字要在这里就被拒掉）。
func Dir(root, id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(root, id), nil
}

// ValidateID 复用 pkg/fingerprint 里那唯一一份身份名判定：档案解析与目录拼接
// 必须用同一把尺子，否则「档案能读进来、目录却建不出来」这种不一致会四处漏。
func ValidateID(id string) error {
	if err := fingerprint.ValidIdentity(id); err != nil {
		return fmt.Errorf("browser: profile id: %w", err)
	}
	return nil
}

// Prepare 为一次启动准备 user-data-dir，返回（目录，是否持久）。
//
// 不指名 profileID（含只有空白）时行为与以前完全一致：建一个临时目录，用完即焚。
//
// 持久身份（profileID 非空）：
//   - 目录不存在就建（0700：里面装的是登录态）；
//   - 已被别的实例占用 → 明确报错，而不是让调用方干等 15 秒启动超时；
//   - 上次异常退出留下的陈旧锁 → 就地清掉（否则用户会被一个早不存在的进程永久挡住）。
func Prepare(explicitRoot, profileID string) (string, bool, error) {
	if strings.TrimSpace(profileID) == "" {
		dir, err := os.MkdirTemp("", TempPrefix+"*")
		return dir, false, err
	}
	root, err := ResolveRoot(explicitRoot)
	if err != nil {
		return "", false, err
	}
	dir, err := Dir(root, profileID)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, fmt.Errorf("browser: 建持久 profile 失败: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", false, fmt.Errorf("browser: 收紧持久 profile 权限失败: %w", err)
	}
	if holder, busy := LockHolder(dir); busy {
		return "", false, fmt.Errorf(
			"browser: profile %q 已被占用（%s）：同一身份同一时刻只能有一个浏览器在跑", profileID, holder)
	}
	log.Printf("browser: 持久身份 profile %q → %s", profileID, dir)
	return dir, true, nil
}

// LockHolder 判断这个 profile 目录是否已被一个**活着的** Chrome 占用。
//
// Chrome 在 user-data-dir 根下放 SingletonLock（指向 `<host>-<pid>` 的符号链接）。进程被
// SIGKILL 掉时锁会留下来，所以「有锁文件」不等于「有人在用」——还得看那个 pid 还在不在。
// 真有人用就报给调用方；只是陈旧锁就清掉（连同 SingletonSocket/SingletonCookie），否则
// 用户会被一个早就不存在的进程永久挡在门外。
func LockHolder(dir string) (holder string, busy bool) {
	lock := filepath.Join(dir, "SingletonLock")
	target, err := os.Readlink(lock)
	if err != nil {
		return "", false
	}
	if pid := lockPID(target); pid > 0 && !processAlive(pid) {
		log.Printf("browser: 清理陈旧 profile 锁 %s（pid %d 已不在）", lock, pid)
		for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
			_ = os.Remove(filepath.Join(dir, name))
		}
		return "", false
	}
	return fmt.Sprintf("锁 %s → %s", lock, target), true
}

// lockPID 从 `<host>-<pid>` 里取出 pid；取不到返回 0（保守当作「有人在用」）。
func lockPID(target string) int {
	i := strings.LastIndex(target, "-")
	if i < 0 || i == len(target)-1 {
		return 0
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// processAlive 判断 pid 是否还活着（signal 0 只做存在性与权限检查，不真发信号）。
//
// 保守口径（2026-09-23 审查结论）：**只有 ESRCH（内核确认无此进程）才判为已死**；
// EPERM 等「权限不足/无法确认」一律当作「还活着」。把权限错误当成死亡会让本文件的
// 陈旧锁清理与孤儿目录回收误删仍在使用的 profile，方向不安全。
func processAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, syscall.Signal(0)), syscall.ESRCH)
}

// TempPrefix 是自起浏览器临时 user-data-dir 的命名前缀（Prepare 用 MkdirTemp 建）。
const TempPrefix = "uws-cdp-"

// Reap 清理自起浏览器的**临时 Profile 残留**（2026-09-23 审查结论）：
// 无人消费方（Agent Hub）的超时链路里，最坏情况是 CLI 被 SIGKILL 掉，QA Runner 的
// defer session.Close() 不执行，于是留下一个孤儿 headless Chrome 和它的临时目录。
// 这里按「**能证明**占用者已死」才删的保守口径回收：
//   - 只动 $TMPDIR/uws-cdp-* 目录（持久身份目录绝不碰）；
//   - 没有 SingletonLock、锁读不出来、pid 取不到、pid 还活着 → 一律不删；
//   - olderThan 是时间保险：刚创建的目录不碰（避免和正在启动的浏览器抢目录）。
//
// 返回清理掉的目录数。
func Reap(olderThan time.Duration) int {
	return ReapIn(os.TempDir(), olderThan)
}

// ReapIn 是 Reap 的可测内核（root 可注入）。
func ReapIn(root string, olderThan time.Duration) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), TempPrefix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		if olderThan > 0 && time.Since(info.ModTime()) < olderThan {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, "SingletonLock"))
		if err != nil {
			// 没有锁 = 无法证明占用者已死（可能刚建好、还没起锁）；保守跳过。
			continue
		}
		pid := lockPID(target)
		if pid <= 0 || processAlive(pid) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("browser: 清理临时 profile %s 失败: %v", dir, err)
			continue
		}
		log.Printf("browser: 清理孤儿临时 profile %s（pid %d 已不在）", dir, pid)
		removed++
	}
	return removed
}
