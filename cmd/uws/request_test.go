package main

import "testing"

func TestParseRequestFlagsAcceptsNaturalArgumentOrder(t *testing.T) {
	options, positional, err := parseRequestFlags([]string{
		"https://api.example.test/v1/me", "--profile-file", "identity.json", "--state", "state.json",
		"--header", "Authorization: Bearer demo", "--method", "POST", "--body", `{"ok":true}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(positional) != 1 || positional[0] != "https://api.example.test/v1/me" || options.profileFile != "identity.json" || options.stateFile != "state.json" || options.method != "POST" || options.body != `{"ok":true}` || len(options.headers) != 1 {
		t.Fatalf("parsed request = %+v positionals=%v", options, positional)
	}
}

func TestParseRequestHeaderPreservesContextButProtectsIdentity(t *testing.T) {
	name, value, err := parseRequestHeader("Authorization: Bearer token")
	if err != nil || name != "Authorization" || value != "Bearer token" {
		t.Fatalf("authorization header = %q, %q, %v", name, value, err)
	}
	for _, raw := range []string{"Cookie: sid=x", "User-Agent: fake", "Sec-CH-UA: fake", "Bad Name: x", "X-Test: a\nb"} {
		if _, _, err := parseRequestHeader(raw); err == nil {
			t.Fatalf("%q should be rejected", raw)
		}
	}
	if _, _, err := parseRequestHeader("X-Request-ID: abc"); err != nil {
		t.Fatalf("context header should pass: %v", err)
	}
}
