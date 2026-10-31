package fingerprint

import (
	"bytes"
	"testing"
	"time"
)

func testStateBundle(t *testing.T) *StateBundle {
	t.Helper()
	b, err := NewStateBundle("state-test", time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	b.Cookies = []byte(`[{"name":"sid","value":"test","domain":"example.test","path":"/","httpOnly":true}]`)
	b.Origins = []StateBundleOrigin{{
		Origin: "https://example.test", LocalStorage: []StateBundleKVPair{{Key: "theme", Value: "dark"}},
		SessionStorage: []StateBundleKVPair{{Key: "tab_state", Value: "open"}},
	}}
	return b
}

func TestStateBundlePlainRoundTrip(t *testing.T) {
	want := testStateBundle(t)
	data, err := want.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseStateBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileID != want.ProfileID || string(got.Cookies) != string(want.Cookies) || len(got.Origins) != 1 || got.Origins[0].SessionStorage[0].Value != "open" {
		t.Fatalf("plain round trip changed bundle: %#v", got)
	}
}

func TestStateBundleAESGCMRoundTripAndTamperFailure(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	sealed, err := SealStateBundle(testStateBundle(t), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenStateBundle(sealed, key)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileID != "state-test" {
		t.Fatalf("profile id = %q", got.ProfileID)
	}
	sealed[len(sealed)-2] ^= 1
	if _, err := OpenStateBundle(sealed, key); err == nil {
		t.Fatal("tampered encrypted bundle must fail closed")
	}
	if _, err := SealStateBundle(testStateBundle(t), []byte("too short")); err == nil {
		t.Fatal("short key must fail")
	}
}

func TestStateBundleRejectsUnknownAndMalformedCookie(t *testing.T) {
	if _, err := ParseStateBundle([]byte(`{"$schema":"https://webtouch.dev/schemas/state-bundle.v1.json","profile_id":"x","created_at":"2026-09-13T00:00:00Z","cookies":{},"origins":[]}`)); err == nil {
		t.Fatal("non-array cookies must fail")
	}
	if _, err := ParseStateBundle([]byte(`{"$schema":"https://webtouch.dev/schemas/state-bundle.v1.json","profile_id":"x","created_at":"2026-09-13T00:00:00Z","cookies":[],"origins":[],"extra":1}`)); err == nil {
		t.Fatal("unknown fields must fail")
	}
}
