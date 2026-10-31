package profiledir

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestReapInStaleTempProfiles 守住保守口径：只有「锁指向的 pid 确实已死 + 目录够旧」
// 才回收，其余一律放过（宁可留下垃圾，也不删掉别人的浏览器状态）。
func TestReapInStaleTempProfiles(t *testing.T) {
	root := t.TempDir()

	mk := func(name string, lock string, age time.Duration) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if lock != "" {
			if err := os.Symlink(lock, filepath.Join(dir, "SingletonLock")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		}
		old := time.Now().Add(-age)
		_ = os.Chtimes(dir, old, old)
		return dir
	}

	deadPID := 4194303 // 远超本机 pid 上限，Signal(0) 必然 ESRCH
	dead := mk(TempPrefix+"dead", "host-"+strconv.Itoa(deadPID), 2*time.Hour)
	alive := mk(TempPrefix+"alive", "host-"+strconv.Itoa(os.Getpid()), 2*time.Hour)
	noLock := mk(TempPrefix+"nolock", "", 2*time.Hour)
	fresh := mk(TempPrefix+"fresh", "host-"+strconv.Itoa(deadPID), 0)
	other := mk("somewhere-else-dead", "host-"+strconv.Itoa(deadPID), 2*time.Hour)

	removed := ReapIn(root, time.Hour)
	if removed != 1 {
		t.Fatalf("expected exactly 1 reaped dir, got %d", removed)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("死进程锁的旧目录应被清理: %v", err)
	}
	for _, dir := range []string{alive, noLock, fresh, other} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("不该被清理的目录被删了: %s", dir)
		}
	}
}
