package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String(), runErr
}

func TestSessionCLICreateUseClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli-sessions.json")
	t.Setenv("UWS_CLI_SESSION_FILE", path)
	t.Setenv("WEBTOUCH_SESSION", "")
	t.Setenv("UWS_SESSION", "")

	out, err := captureStdout(t, func() error {
		return runSession([]string{"create", "--channel", "gemini-web", "--attach", "http://127.0.0.1:9222"})
	})
	if err != nil {
		t.Fatal(err, out)
	}
	created := decodeSessionLine(t, out)
	if created["ok"] != true || created["contract"] != "webtouch.cli.session.v1" {
		t.Fatalf("create=%s", out)
	}
	id, _ := created["session_id"].(string)
	if !strings.HasPrefix(id, "wts_") || created["state"] != "active" || created["channel"] != "gemini-web" {
		t.Fatalf("create=%s", out)
	}
	if created["site_conversation_id"] != nil {
		t.Fatalf("site id should be null: %s", out)
	}
	hub, _ := created["hub"].(map[string]any)
	if hub["channel"] != "gemini-web" || hub["site_conversation_id"] != nil {
		t.Fatalf("hub=%v", hub)
	}
	if _, ok := hub["profile_id"]; ok {
		t.Fatal("hub ledger must not carry profile_id")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "@vault:") {
		t.Fatalf("lease leaked a vault ref: %s", raw)
	}

	t.Setenv("WEBTOUCH_SESSION", id)
	out, err = captureStdout(t, func() error {
		return runSession([]string{"use", "--site-conversation", "c_site1"})
	})
	if err != nil {
		t.Fatal(err, out)
	}
	used := decodeSessionLine(t, out)
	if used["session_id"] != id || used["state"] != "active" {
		t.Fatalf("use=%s", out)
	}
	if used["site_conversation_id"] != "c_site1" {
		t.Fatalf("use site=%v", used["site_conversation_id"])
	}

	out, err = captureStdout(t, func() error {
		return runSession([]string{"close", id})
	})
	if err != nil {
		t.Fatal(err, out)
	}
	closed := decodeSessionLine(t, out)
	if closed["state"] != "closed" || closed["runtime"] != "released" {
		t.Fatalf("close=%s", out)
	}

	out, err = captureStdout(t, func() error {
		return runSession([]string{"use", "--session", id})
	})
	if err == nil {
		t.Fatal("closed session should fail")
	}
	failed := decodeSessionLine(t, out)
	errObj, _ := failed["error"].(map[string]any)
	if failed["ok"] != false || errObj["code"] != "session_closed" {
		t.Fatalf("use after close=%s err=%v", out, err)
	}

	out, listErr := captureStdout(t, func() error {
		return runSession([]string{"list"})
	})
	if listErr != nil {
		t.Fatal(listErr, out)
	}
	var listed struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatal(err, out)
	}
	for _, item := range listed.Sessions {
		if item["session_id"] == id {
			t.Fatal("closed session listed by default")
		}
	}
}

func TestSessionCLIRejectsVaultRef(t *testing.T) {
	t.Setenv("UWS_CLI_SESSION_FILE", filepath.Join(t.TempDir(), "cli-sessions.json"))
	out, err := captureStdout(t, func() error {
		return runSession([]string{"create", "--account", "@vault:gmail"})
	})
	if err == nil {
		t.Fatal("vault ref should be rejected", out)
	}
	body := decodeSessionLine(t, out)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "vault_ref_rejected" {
		t.Fatalf("body=%s", out)
	}
	if strings.Contains(out, "gmail") {
		t.Fatalf("stdout echoed the vault name: %s", out)
	}
}

func decodeSessionLine(t *testing.T, out string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("json: %v body=%s", err, out)
	}
	return body
}
