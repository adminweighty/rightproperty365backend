package identity

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

var testHasher = Hasher{MemoryKiB: 64, Time: 1, Threads: 1}

func TestHashAndVerify(t *testing.T) {
	const password = "correct horse battery staple"
	encoded, err := testHasher.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	// PHC string format with unpadded standard base64.
	if !regexp.MustCompile(`^\$argon2id\$v=19\$m=64,t=1,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`).MatchString(encoded) {
		t.Fatalf("unexpected encoding: %s", encoded)
	}
	if strings.Contains(encoded, password) {
		t.Fatal("hash contains the password")
	}

	again, err := testHasher.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	if again == encoded {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}

	tests := []struct {
		name     string
		password string
		want     bool
	}{
		{"correct", password, true},
		{"wrong", "correct horse battery stapl3", false},
		{"empty", "", false},
		{"prefix", password[:10], false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := VerifyPassword(encoded, tt.password)
			if err != nil || got != tt.want {
				t.Fatalf("VerifyPassword = %v, %v; want %v", got, err, tt.want)
			}
		})
	}

	// A hash made with other parameters still verifies: parameters are read
	// from the encoded string, so they can be raised later.
	other, err := Hasher{MemoryKiB: 128, Time: 2, Threads: 2}.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword(other, password); err != nil || !ok {
		t.Fatalf("cross-parameter verify = %v, %v", ok, err)
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	for _, encoded := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$***$aGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$",
	} {
		if ok, err := VerifyPassword(encoded, "anything"); ok || err == nil {
			t.Fatalf("VerifyPassword(%q) = %v, %v; want false and an error", encoded, ok, err)
		}
	}
}

func TestTokens(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		token, hash, err := newToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) != 43 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(token) {
			t.Fatalf("token %q is not 32 bytes of unpadded base64url", token)
		}
		if len(hash) != 32 || string(hash) != string(hashToken(token)) {
			t.Fatal("hash must be the SHA-256 of the token")
		}
		if seen[token] {
			t.Fatal("duplicate token")
		}
		seen[token] = true
		if !plausibleToken(token) || plausibleToken(token+"a") || plausibleToken("") {
			t.Fatal("plausibleToken misjudged a token")
		}
	}
}

func TestLinks(t *testing.T) {
	l := NewLinks("http://localhost:3000/")
	if got := l.VerifyEmail("abc_DEF-1"); got != "http://localhost:3000/verify-email?token=abc_DEF-1" {
		t.Fatalf("verify link = %s", got)
	}
	if got := l.ResetPassword("abc_DEF-1"); got != "http://localhost:3000/reset-password?token=abc_DEF-1" {
		t.Fatalf("reset link = %s", got)
	}
}

func TestMailersLogging(t *testing.T) {
	ctx := context.Background()
	const link = "http://localhost:3000/verify-email?token=SECRET-TOKEN"

	var dev strings.Builder
	_ = NewDevMailer(slog.New(slog.NewJSONHandler(&dev, nil))).SendVerification(ctx, "a@example.com", "A", link)
	if strings.Count(dev.String(), "\n") != 1 || !strings.Contains(dev.String(), link) {
		t.Fatalf("development mailer must log exactly one line containing the link, got %q", dev.String())
	}

	var prod strings.Builder
	m := NewUnconfiguredMailer(slog.New(slog.NewJSONHandler(&prod, nil)))
	_ = m.SendVerification(ctx, "a@example.com", "A", link)
	_ = m.SendPasswordReset(ctx, "a@example.com", "A", link)
	out := prod.String()
	if strings.Contains(out, "SECRET-TOKEN") || strings.Contains(out, "a@example.com") {
		t.Fatalf("placeholder mailer leaked the token or recipient: %s", out)
	}
	if strings.Count(out, `"level":"WARN"`) != 2 {
		t.Fatalf("placeholder mailer must warn for every unsent email: %s", out)
	}
}
