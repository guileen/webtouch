package fingerprint

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func profileWithID(t *testing.T, id string) *Profile {
	t.Helper()
	p := mustParse(t, validProfileJSON)
	p.ProfileID = id
	return p
}

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	return reg
}

func TestRegistryRoundTripsProfiles(t *testing.T) {
	reg := newRegistry(t)
	for _, id := range []string{"fp_a", "fp_b"} {
		if err := reg.Save(profileWithID(t, id)); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}
	ids := []string{}
	profiles, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range profiles {
		ids = append(ids, p.ProfileID)
	}
	if strings.Join(ids, ",") != "fp_a,fp_b" {
		t.Fatalf("List 结果不对: %v", ids)
	}
	loaded, err := reg.Load("fp_a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Emulation.Locale.TimezoneID != "Asia/Shanghai" {
		t.Fatalf("读回来的档案不对: %+v", loaded.Emulation.Locale)
	}
	// 身份目录与档案都不该给别人看：里面是身份本身。
	info, err := os.Stat(filepath.Join(reg.Root, "fp_a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("档案权限应当是 0600，实际 %v", info.Mode().Perm())
	}
}

func TestRegistryRefusesNameFileMismatch(t *testing.T) {
	reg := newRegistry(t)
	// 文件名 fp_a.json 里写着 fp_b —— 身份名必须唯一对得上，否则「按账号找身份」会串。
	if err := writeFileAtomic(filepath.Join(reg.Root, "fp_a.json"), mustJSON(profileWithID(t, "fp_b"))); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Load("fp_a"); err == nil {
		t.Fatal("文件名与 profile_id 不一致应当被拒绝")
	}
}

func TestRegistrySaveRejectsAPathEscapingIdentity(t *testing.T) {
	reg := newRegistry(t)
	if err := reg.Save(profileWithID(t, "../evil")); err == nil {
		t.Fatal("会逃出身份目录的身份名应当被拒绝")
	}
}

func TestBindIsOneToOne(t *testing.T) {
	reg := newRegistry(t)
	for _, id := range []string{"fp_a", "fp_b"} {
		if err := reg.Save(profileWithID(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.Bind("alice", "fp_a"); err != nil {
		t.Fatalf("首次绑定失败: %v", err)
	}
	// 同一账号改绑另一个身份：必须显式解绑，不能悄悄漂移（否则哪天登错号都说不清）。
	if err := reg.Bind("alice", "fp_b"); err == nil {
		t.Fatal("账号改绑应当先 unbind")
	}
	// 同一身份被第二个账号抢：这是最危险的关联信号，必须挡住。
	if err := reg.Bind("bob", "fp_a"); err == nil {
		t.Fatal("一个身份不该被两个账号共用")
	}
	if err := reg.Unbind("alice"); err != nil {
		t.Fatalf("解绑失败: %v", err)
	}
	if err := reg.Bind("bob", "fp_a"); err != nil {
		t.Fatalf("解绑后应当可以重新绑定: %v", err)
	}
	accounts, err := reg.AccountsOf("fp_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0] != "bob" {
		t.Fatalf("反向查询不对: %v", accounts)
	}
}

const registryLockHelperEnv = "UWS_TEST_REGISTRY_LOCK_HELPER"

// TestRegistryWriteLockHelper is executed only in a separately spawned test
// process. It deliberately holds the same advisory lock until its parent
// releases stdin, proving that the mutation lock is process-wide rather than
// merely a goroutine mutex.
func TestRegistryWriteLockHelper(t *testing.T) {
	if os.Getenv(registryLockHelperEnv) != "1" {
		return
	}
	reg, err := OpenRegistry(os.Getenv(IdentityRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.withWriteLock(func() error {
		if _, err := fmt.Fprintln(os.Stdout, "registry-lock-held"); err != nil {
			return err
		}
		_, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryWriteLockSerializesSeparateProcess(t *testing.T) {
	reg := newRegistry(t)
	if err := reg.Save(profileWithID(t, "fp_a")); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestRegistryWriteLockHelper$", "-test.v")
	child.Env = append(os.Environ(), registryLockHelperEnv+"=1", IdentityRootEnv+"="+reg.Root)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = stdin.Write([]byte("\n"))
		_ = stdin.Close()
		_ = child.Wait()
	}()

	locked := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "registry-lock-held" {
				locked <- true
				return
			}
		}
		locked <- false
	}()
	select {
	case ok := <-locked:
		if !ok {
			t.Fatal("子进程没有取得注册表写锁")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待子进程取得注册表写锁超时")
	}

	bound := make(chan error, 1)
	go func() { bound <- reg.Bind("alice", "fp_a") }()
	select {
	case err := <-bound:
		t.Fatalf("另一进程持锁时 Bind 不应提前返回，实际错误=%v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := stdin.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-bound:
		if err != nil {
			t.Fatalf("锁释放后的 Bind 失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("锁释放后 Bind 仍未完成")
	}
}

func TestBindRequiresAnExistingIdentity(t *testing.T) {
	reg := newRegistry(t)
	if err := reg.Bind("alice", "ghost"); err == nil {
		t.Fatal("绑到一个不存在的身份应当被拒绝")
	}
}

func TestResolveNeverFallsBackToADefaultIdentity(t *testing.T) {
	reg := newRegistry(t)
	if err := reg.Save(profileWithID(t, "fp_a")); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve("alice"); err == nil {
		t.Fatal("没绑定的账号必须明确报错，不许偷偷用默认身份")
	}
	if err := reg.Bind("alice", "fp_a"); err != nil {
		t.Fatal(err)
	}
	profile, err := reg.Resolve("alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if profile.ProfileID != "fp_a" {
		t.Fatalf("Resolve 给错了身份: %s", profile.ProfileID)
	}
}

func TestConflictsReportSharedProxyAndDanglingBinding(t *testing.T) {
	reg := newRegistry(t)
	// 两份档案指着同一条代理出口：平台侧 IP 是最强的关联信号，必须被报出来。
	for _, id := range []string{"fp_a", "fp_b"} {
		if err := reg.Save(profileWithID(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	// 第三条身份换成别的出口，不该被误报。
	other := profileWithID(t, "fp_c")
	other.Network.ProxyURL = "socks5://127.0.0.1:10809"
	if err := reg.Save(other); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(reg.Root, bindingsFileName),
		[]byte("{\"version\":1,\"accounts\":{\"alice\":\"ghost\"}}")); err != nil {
		t.Fatal(err)
	}

	problems, err := reg.Conflicts()
	if err != nil {
		t.Fatalf("Conflicts: %v", err)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "fp_a") || !strings.Contains(joined, "fp_b") || !strings.Contains(joined, "代理出口") {
		t.Fatalf("没报出共用代理: %v", problems)
	}
	if strings.Contains(joined, "fp_c") {
		t.Fatalf("独占出口的身份被误报: %v", problems)
	}
	if !strings.Contains(joined, "alice") || !strings.Contains(joined, "不存在") {
		t.Fatalf("没报出悬空绑定: %v", problems)
	}
}

func TestConflictsReportDuplicateIdentityBinding(t *testing.T) {
	reg := newRegistry(t)
	if err := reg.Save(profileWithID(t, "fp_a")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(reg.Root, bindingsFileName),
		[]byte(`{"version":1,"accounts":{"alice":"fp_a","bob":"fp_a"}}`)); err != nil {
		t.Fatal(err)
	}
	problems, err := reg.Conflicts()
	if err != nil {
		t.Fatalf("Conflicts: %v", err)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "多个账号") || !strings.Contains(joined, "alice") || !strings.Contains(joined, "bob") {
		t.Fatalf("没报出重复身份绑定: %v", problems)
	}
}

func TestConflictsTreatSocks5hAndSocks5AsOneExit(t *testing.T) {
	reg := newRegistry(t)
	// 同一条出口的两种写法：socks5h 是「让代理解析域名」的写法，Chrome 只认 socks5，
	// 落到 --proxy-server 上是同一个值（见 chromeProxyArg）。共用出口这件事必须报出来。
	variants := map[string]string{
		"fp_a": "socks5h://Proxy.Example.com:1080/",
		"fp_b": "socks5://proxy.example.com:1080",
	}
	for id, proxy := range variants {
		profile := profileWithID(t, id)
		profile.Network.ProxyURL = proxy
		if err := reg.Save(profile); err != nil {
			t.Fatal(err)
		}
	}
	// 换条出口（端口不同）不该被误报。
	other := profileWithID(t, "fp_c")
	other.Network.ProxyURL = "socks5://proxy.example.com:1081"
	if err := reg.Save(other); err != nil {
		t.Fatal(err)
	}

	problems, err := reg.Conflicts()
	if err != nil {
		t.Fatalf("Conflicts: %v", err)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "fp_a") || !strings.Contains(joined, "fp_b") {
		t.Fatalf("同一条出口的两种写法没被认成共用: %v", problems)
	}
	if strings.Contains(joined, "fp_c") {
		t.Fatalf("不同端口被误报成共用出口: %v", problems)
	}
}
