package main

import (
	"strings"
	"testing"
	"time"
)

func TestSessionSignAndVerify(t *testing.T) {
	a, err := newAuth("admin", "secret", false)
	if err != nil {
		t.Fatal(err)
	}
	good := a.sign(time.Now().Add(time.Hour).Unix())
	if !a.valid(good) {
		t.Fatal("a freshly signed, unexpired session must verify")
	}
	expired := a.sign(time.Now().Add(-time.Hour).Unix())
	if a.valid(expired) {
		t.Fatal("an expired session must not verify")
	}
	if a.valid("garbage") || a.valid("") || a.valid("123.not-hex") {
		t.Fatal("malformed values must not verify")
	}
	// A session signed with different credentials (a different key) must not verify:
	// this is what makes changing the password invalidate every existing session.
	other, _ := newAuth("admin", "different", false)
	if a.valid(other.sign(time.Now().Add(time.Hour).Unix())) {
		t.Fatal("a session signed under different credentials must not verify")
	}
	// Tampering with the expiry (without resigning) must not verify.
	exp := time.Now().Add(time.Hour).Unix()
	sig := a.sign(exp)
	last := "0"
	if strings.HasSuffix(sig, "0") {
		last = "1" // replacing a "0" with "0" would tamper with nothing
	}
	tampered := sig[:len(sig)-1] + last
	if a.valid(tampered) {
		t.Fatal("a tampered signature must not verify")
	}
}

func TestValidCredsConstantTime(t *testing.T) {
	a, err := newAuth("admin", "secret", false)
	if err != nil {
		t.Fatal(err)
	}
	if !a.validCreds("admin", "secret") {
		t.Fatal("correct credentials must pass")
	}
	for _, c := range [][2]string{{"admin", "wrong"}, {"wrong", "secret"}, {"", ""}} {
		if a.validCreds(c[0], c[1]) {
			t.Fatalf("%v must be rejected", c)
		}
	}
}

func TestSafeNextRejectsOpenRedirects(t *testing.T) {
	cases := map[string]string{
		"":                            "/",
		"/feed/UCone":                 "/feed/UCone",
		"//evil.example":              "/",
		"https://evil.example/path":   "/",
		"javascript:alert(1)":         "/",
		"/ok?with=query&and=fragment": "/ok?with=query&and=fragment",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
