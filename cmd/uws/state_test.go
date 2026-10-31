package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"uws/pkg/fingerprint"
	uwspb "uws/proto/gen/go/proto/v1"
)

func TestNormalizeStateArgsAcceptsNaturalPositionalsFirst(t *testing.T) {
	got := normalizeStateArgs([]string{
		"fp_state_01", "https://example.test", "--target-id", "target-1",
		"--out=result.json", "--key-file", "key.txt",
	})
	want := []string{"--target-id", "target-1", "--out=result.json", "--key-file", "key.txt", "fp_state_01", "https://example.test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized args = %#v, want %#v", got, want)
	}
	opts, positional, err := parseStateRPCFlags("export", []string{
		"fp_state_01", "https://example.test", "--target-id", "target-1", "--out", "result.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.targetID != "target-1" || opts.output != "result.json" || !reflect.DeepEqual(positional, []string{"fp_state_01", "https://example.test"}) {
		t.Fatalf("parsed opts=%+v positional=%v", opts, positional)
	}
}

func TestStateBundleExportConversionAndEncryptedRoundTrip(t *testing.T) {
	response := &uwspb.ExportStateResponse{
		CookiesJson: `[{"name":"sid","value":"secret","domain":"example.test","path":"/"}]`,
		Origins: []*uwspb.StorageOriginContext{{
			Origin:         "https://example.test",
			LocalStorage:   []*uwspb.StorageOriginContext_KVPair{{Key: "theme", Value: "dark"}},
			SessionStorage: []*uwspb.StorageOriginContext_KVPair{{Key: "tab", Value: "active"}},
		}},
	}
	bundle, err := stateBundleFromExport("fp_state_01", response, time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.ProfileID != "fp_state_01" || len(bundle.Origins) != 1 || bundle.Origins[0].LocalStorage[0].Key != "theme" || bundle.Origins[0].SessionStorage[0].Value != "active" {
		t.Fatalf("unexpected bundle metadata: %+v", bundle)
	}
	key := bytes.Repeat([]byte{7}, 32)
	sealed, err := fingerprint.SealStateBundle(bundle, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.bundle")
	if err := writeStateBundle(path, sealed); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode = %o, want 600", info.Mode().Perm())
	}
	keyPath := filepath.Join(t.TempDir(), "state.key")
	if err := os.WriteFile(keyPath, []byte("0707070707070707070707070707070707070707070707070707070707070707\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, encrypted, err := readStateBundle(path, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !encrypted || opened.ProfileID != bundle.ProfileID || string(opened.Cookies) != string(bundle.Cookies) {
		t.Fatalf("encrypted round trip mismatch: encrypted=%v got=%+v", encrypted, opened)
	}
	proto := stateBundleProto(opened)
	if proto.GetProfileId() != bundle.ProfileID || proto.GetOrigins()[0].GetLocalStorage()[0].GetValue() != "dark" || proto.GetOrigins()[0].GetSessionStorage()[0].GetValue() != "active" {
		t.Fatalf("proto conversion mismatch: %+v", proto)
	}
}

func TestRequireFreshProfileRejectsExistingState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("UWS_PROFILE_ROOT", root)
	if err := requireFreshProfile("fresh-profile"); err != nil {
		t.Fatalf("missing profile should be fresh: %v", err)
	}
	dir := filepath.Join(root, "fresh-profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := requireFreshProfile("fresh-profile"); err != nil {
		t.Fatalf("empty profile directory should be fresh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Preferences"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireFreshProfile("fresh-profile"); err == nil {
		t.Fatal("existing profile data must require explicit confirmation")
	}
}
