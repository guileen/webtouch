package profiledir

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestDirRejectsUnsafeIDs(t *testing.T) {
	// 这些名字将来会从配置/网络下发：路径拼接前必须在这里被拒掉。
	bad := []string{"", ".", "..", ".hidden", "a/b", "../etc", `a\b`, strings.Repeat("x", 65), "身份"}
	for _, id := range bad {
		if _, err := Dir("/tmp/root", id); err == nil {
			t.Fatalf("Dir(%q) 应当被拒", id)
		}
	}
}

func TestDirAcceptsIdentityNames(t *testing.T) {
	dir, err := Dir("/tmp/root", "fp_dev_mac_01")
	if err != nil {
		t.Fatalf("合法身份名被拒: %v", err)
	}
	if want := filepath.Join("/tmp/root", "fp_dev_mac_01"); dir != want {
		t.Fatalf("目录拼接不对: %s != %s", dir, want)
	}
}

func TestPreparePersistentIsSameDirTwiceAndTempIsNotPersistent(t *testing.T) {
	root := t.TempDir()

	dir, persistent, err := Prepare(root, "fp_a")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !persistent {
		t.Fatal("指定 profileID 应当是持久目录")
	}
	if want := filepath.Join(root, "fp_a"); dir != want {
		t.Fatalf("落盘位置不对: %s != %s", dir, want)
	}
	again, _, err := Prepare(root, "fp_a")
	if err != nil || again != dir {
		t.Fatalf("同一身份两次指向了不同目录: %s vs %s (%v)", dir, again, err)
	}

	tmp, persistent, err := Prepare(root, "")
	if err != nil {
		t.Fatalf("Prepare(临时): %v", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if persistent {
		t.Fatal("不指定身份时必须还是临时目录（用完即焚，行为不许变）")
	}
	if !strings.HasPrefix(filepath.Base(tmp), TempPrefix) {
		t.Fatalf("临时目录应以 %q 开头，实际 %s", TempPrefix, tmp)
	}
}

func TestLockHolderBusyForLiveProcessAndCleanedWhenStale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SingletonLock 是 Unix 语义")
	}
	dir := t.TempDir()
	lock := filepath.Join(dir, "SingletonLock")

	// 活着的进程持锁（这里就是本测试进程）→ 判定为占用，启动必须被挡住。
	if err := os.Symlink("host-"+strconv.Itoa(os.Getpid()), lock); err != nil {
		t.Fatalf("建锁失败: %v", err)
	}
	if holder, busy := LockHolder(dir); !busy {
		t.Fatalf("活进程持锁应当判定为占用（holder=%q）", holder)
	}

	// 陈旧锁（pid 早没了）→ 清掉并放行，否则用户会被一个不存在的进程永久挡住。
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("host-999999", lock); err != nil {
		t.Fatal(err)
	}
	if holder, busy := LockHolder(dir); busy {
		t.Fatalf("陈旧锁不该挡住启动: %s", holder)
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatal("陈旧锁应当被清掉")
	}
}

// processAlive 必须保守：只有内核确认进程不存在（ESRCH）才判死。
// 旧实现把 signal-0 的任何错误都当成“已死”，会把 EPERM 误判为死亡。
func TestProcessAliveConfirmsLiveness(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatalf("对自身 pid 应判为存活")
	}

	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程失败: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("等待子进程失败: %v", err)
	}
	if processAlive(pid) {
		t.Fatalf("已退出并被回收的子进程 pid %d 应判为不存在（ESRCH）", pid)
	}
}

// 权限不足（EPERM）属于“无法确认”，必须判为还活着，否则会误删仍在使用的 profile。
func TestProcessAliveUnknownTreatedAsAlive(t *testing.T) {
	if err := syscall.Kill(1, syscall.Signal(0)); errors.Is(err, syscall.ESRCH) {
		t.Skip("pid 1 不存在，本机无法构造 EPERM 语义用例")
	}
	if !processAlive(1) {
		t.Fatalf("权限不足（EPERM）应判为「无法确认 → 还活着」；判死会误删活 profile")
	}
}
