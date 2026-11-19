package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

func TestRoundTripAndWrongKey(t *testing.T) {
	k1 := bytes.Repeat([]byte{1}, KeySize)
	k2 := bytes.Repeat([]byte{2}, KeySize)
	b1, err := New(k1)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := New(k2)
	plain := []byte("a totp secret")
	sealed, err := b1.Seal(plain, []byte("user-1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("plaintext visible in the sealed value")
	}
	again, _ := b1.Seal(plain, []byte("user-1"))
	if bytes.Equal(sealed, again) {
		t.Fatal("nonce reused")
	}
	got, err := b1.Open(sealed, []byte("user-1"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open: %v", err)
	}
	if _, err := b2.Open(sealed, []byte("user-1")); !errors.Is(err, ErrOpen) {
		t.Fatalf("wrong key opened: %v", err)
	}
	if _, err := b1.Open(sealed, []byte("user-2")); !errors.Is(err, ErrOpen) {
		t.Fatalf("wrong aad opened: %v", err)
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	if _, err := b1.Open(tampered, []byte("user-1")); !errors.Is(err, ErrOpen) {
		t.Fatal("tampered value opened")
	}
	if _, err := b1.Open([]byte{1, 2}, nil); !errors.Is(err, ErrOpen) {
		t.Fatal("short value opened")
	}
}

func TestParseKey(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, KeySize)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawURLEncoding} {
		if k, err := ParseKey(" " + enc.EncodeToString(raw) + "\n"); err != nil || !bytes.Equal(k, raw) {
			t.Fatalf("parse: %v", err)
		}
	}
	for _, bad := range []string{"", "short", base64.StdEncoding.EncodeToString(raw[:16])} {
		if _, err := ParseKey(bad); !errors.Is(err, ErrBadKey) {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, err := New(raw[:31]); !errors.Is(err, ErrBadKey) {
		t.Fatal("short key accepted")
	}
	if len(DevelopmentKey()) != KeySize {
		t.Fatal("development key size")
	}
}
