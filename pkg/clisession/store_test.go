package clisession

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeResolver struct {
	id    string
	err   error
	calls int
}

func (f *fakeResolver) Resolve(account string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if f.id == "" {
		return "", errors.New("账号 " + account + " 还没绑身份")
	}
	return f.id, nil
}

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "cli-sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetTTL(time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCreateUseCloseReusesSameLease(t *testing.T) {
	st := testStore(t)
	res := &fakeResolver{id: "prof-1"}
	st.SetResolver(res)
	created, err := st.Create(CreateInput{
		Channel: "gemini-web", Account: "acct-gemini", SiteConversationID: "c_abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.SessionID, "wts_") || created.State != StateActive || created.ProfileID != "prof-1" {
		t.Fatalf("created=%+v", created)
	}
	if created.Hub.Channel != "gemini-web" || created.Hub.SiteConversationID == nil || *created.Hub.SiteConversationID != "c_abc" {
		t.Fatalf("hub=%+v", created.Hub)
	}
	if res.calls != 1 {
		t.Fatalf("resolver calls=%d", res.calls)
	}
	again, err := st.Create(CreateInput{Channel: "gemini-web", Account: "acct-gemini"})
	if err != nil {
		t.Fatal(err)
	}
	if again.SessionID == created.SessionID || again.ProfileID != "prof-1" {
		t.Fatalf("second=%+v first=%s", again, created.SessionID)
	}
	used, err := st.Use(created.SessionID, UseInput{})
	if err != nil {
		t.Fatal(err)
	}
	if used.SessionID != created.SessionID || used.ProfileID != "prof-1" || used.LeaseEpoch != 1 {
		t.Fatalf("use=%+v", used)
	}
	closed, err := st.Close(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != StateClosed || closed.Runtime != RuntimeReleased {
		t.Fatalf("close=%+v", closed)
	}
	if _, err := st.Close(created.SessionID); err != nil {
		t.Fatalf("second close: %v", err)
	}
	_, useErr := st.Use(created.SessionID, UseInput{})
	var ce *Error
	if !errors.As(useErr, &ce) || ce.Code != CodeClosed {
		t.Fatalf("err=%v", useErr)
	}
	listed, err := st.List(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range listed {
		if item.SessionID == created.SessionID {
			t.Fatal("closed lease still in default list")
		}
	}
	all, err := st.List(true)
	if err != nil || len(all) < 2 {
		t.Fatalf("all=%v err=%v", all, err)
	}
}

func TestUnboundAccountDoesNotMintProfile(t *testing.T) {
	st := testStore(t)
	res := &fakeResolver{err: errors.New("账号未绑定")}
	st.SetResolver(res)
	_, err := st.Create(CreateInput{Account: "acct-new", Channel: "gemini-web"})
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != CodeProfileUnbound {
		t.Fatal(err)
	}
	if res.calls != 1 {
		t.Fatalf("calls=%d", res.calls)
	}
	listed, err := st.List(true)
	if err != nil || len(listed) != 0 {
		t.Fatalf("listed=%v err=%v", listed, err)
	}
	raw, err := os.ReadFile(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "acct-new") {
		t.Fatalf("unbound account was persisted: %s", raw)
	}
}

func TestProfileMismatchAndVaultRefRejected(t *testing.T) {
	st := testStore(t)
	res := &fakeResolver{id: "prof-bound"}
	st.SetResolver(res)
	if _, err := st.Create(CreateInput{Account: "acct", ProfileID: "prof-other"}); err == nil {
		t.Fatal("mismatch should fail")
	} else {
		var ce *Error
		if !errors.As(err, &ce) || ce.Code != CodeProfileMismatch {
			t.Fatal(err)
		}
	}
	secret := "@vault:gmail-password"
	_, err := st.Create(CreateInput{Account: secret})
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != CodeVaultRef {
		t.Fatal(err)
	}
	if strings.Contains(err.Error(), "gmail-password") {
		t.Fatalf("error echoed secret: %v", err)
	}
	raw, _ := os.ReadFile(st.Path())
	if strings.Contains(string(raw), "@vault:") || strings.Contains(string(raw), "gmail-password") {
		t.Fatalf("secret landed in lease file: %s", raw)
	}
	res.id = "prof-other"
	if _, err := st.Create(CreateInput{Account: "acct", Channel: "gemini-web"}); err != nil {
		t.Fatal(err)
	}
	res.id = "prof-bound"
	if _, err := st.Create(CreateInput{Account: "acct"}); err == nil {
		t.Fatal("changing fingerprint for the same account must fail")
	}
}

func TestIdleWakeAndDeadOwnerReclaim(t *testing.T) {
	st := testStore(t)
	start := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	now := start
	st.SetClock(func() time.Time { return now })
	st.SetAlive(func(pid int) bool { return pid == 10 })
	created, err := st.Create(CreateInput{Channel: "deepseek-web", OwnerPID: 10})
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Minute + time.Second)
	if err := st.Reap(); err != nil {
		t.Fatal(err)
	}
	view, ok, err := st.Lookup(created.SessionID)
	if err != nil || !ok || view.State != StateIdle || view.Runtime != RuntimeSleeping || !view.Recoverable {
		t.Fatalf("after idle: ok=%v view=%+v err=%v", ok, view, err)
	}
	woke, err := st.Use(created.SessionID, UseInput{})
	if err != nil {
		t.Fatal(err)
	}
	if woke.State != StateActive || woke.Runtime != RuntimeReady || woke.LeaseEpoch != 2 {
		t.Fatalf("wake=%+v", woke)
	}
	now = woke.UpdatedAt
	st.SetClock(func() time.Time { return now })
	if _, err := st.Use(created.SessionID, UseInput{OwnerPID: 11}); err == nil {
		t.Fatal("live owner must reject a different holder")
	} else if !errors.As(err, new(*Error)) {
		t.Fatal(err)
	} else {
		var ce *Error
		errors.As(err, &ce)
		if ce.Code != CodeBusy {
			t.Fatal(ce)
		}
	}
	st.SetAlive(func(int) bool { return false })
	if err := st.Reap(); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := st.Use(created.SessionID, UseInput{OwnerPID: 11})
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.State != StateActive || reclaimed.OwnerPID != 11 {
		t.Fatalf("reclaim=%+v", reclaimed)
	}
}

func TestHardExpireClosesLease(t *testing.T) {
	st := testStore(t)
	start := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	now := start
	st.SetClock(func() time.Time { return now })
	created, err := st.Create(CreateInput{Channel: "gemini-web"})
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Hour + time.Second)
	if err := st.Reap(); err != nil {
		t.Fatal(err)
	}
	view, ok, err := st.Lookup(created.SessionID)
	if err != nil || !ok || view.State != StateClosed || view.Runtime != RuntimeReleased {
		t.Fatalf("expired=%+v ok=%v err=%v", view, ok, err)
	}
	if _, err := st.Use(created.SessionID, UseInput{}); !errors.As(err, new(*Error)) {
		t.Fatal(err)
	}
	info, err := os.Stat(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestHubLedgerFieldsAreOnlyChannelAndSiteConversation(t *testing.T) {
	raw, err := json.Marshal(HubRef{Channel: "gemini-web"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 {
		t.Fatalf("hub fields=%v", fields)
	}
	if _, ok := fields["channel"]; !ok {
		t.Fatal(fields)
	}
	if _, ok := fields["site_conversation_id"]; !ok {
		t.Fatal(fields)
	}
	if _, ok := fields["account_ref"]; ok {
		t.Fatal("account_ref must not be a hub ledger field")
	}
	envRaw, err := json.Marshal(Envelope{OK: true, Contract: ContractV1, View: Record{SessionID: "wts_abc", State: StateActive, Channel: "gemini-web"}.View()})
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(envRaw, &env); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ok", "contract", "session_id", "state", "channel", "site_conversation_id", "runtime", "lease_epoch", "hub"} {
		if _, ok := env[key]; !ok {
			t.Fatalf("missing %s in %s", key, envRaw)
		}
	}
	if env["site_conversation_id"] != nil {
		t.Fatalf("empty site id must be null: %s", envRaw)
	}
}

func TestCorruptLeaseIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cli-sessions.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt lease should fail closed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{" {
		t.Fatalf("corrupt file was overwritten: %q", raw)
	}
}
