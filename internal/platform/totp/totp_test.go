package totp

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 rows: the 20-byte ASCII secret and 8 digits.
func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		code string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		step := Step(time.Unix(v.unix, 0))
		if got := CodeDigits(secret, step, 8); got != v.code {
			t.Errorf("T=%d: got %s, want %s", v.unix, got, v.code)
		}
		// The 6-digit code is the same value modulo 10^6.
		if got := Code(secret, step); got != v.code[2:] {
			t.Errorf("T=%d: 6 digits %s, want %s", v.unix, got, v.code[2:])
		}
	}
}

func TestMatchToleranceIsOneStep(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111111, 0)
	cur := Step(now)
	for d, want := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		step, ok := Match(secret, Code(secret, cur+d), now)
		if ok != want || (ok && step != cur+d) {
			t.Errorf("offset %d: ok=%v step=%d", d, ok, step)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := Match(secret, bad, now); ok {
			t.Errorf("%q matched", bad)
		}
	}
}

func TestSecretEncodingAndURI(t *testing.T) {
	s, err := NewSecret()
	if err != nil || len(s) != SecretBytes {
		t.Fatal(err)
	}
	enc := EncodeSecret(s)
	if strings.Contains(enc, "=") {
		t.Fatal("padding in secret")
	}
	back, err := DecodeSecret(strings.ToLower(enc[:4]) + " " + enc[4:])
	if err != nil || string(back) != string(s) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := DecodeSecret("not base32!"); err == nil {
		t.Fatal("bad secret accepted")
	}
	uri := URI("RightProperty365", "admin@example.test", []byte("12345678901234567890"))
	want := "otpauth://totp/RightProperty365:admin@example.test?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=RightProperty365&algorithm=SHA1&digits=6&period=30"
	if uri != want {
		t.Fatalf("uri = %s", uri)
	}
}
