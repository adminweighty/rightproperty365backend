// Package totp implements time-based one-time passwords (RFC 6238 over the
// HOTP of RFC 4226) with the parameters every authenticator app supports:
// HMAC-SHA1, 6 digits and a 30-second step.
//
// Nothing here logs: a secret or a code must never reach a log line.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 authenticator apps use HMAC-SHA1; HMAC-SHA1 is not broken.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Digits is the length of a code.
	Digits = 6
	// Period is the length of one time step.
	Period = 30 * time.Second
	// Skew is how many steps before and after the current one are accepted.
	Skew = 1
	// SecretBytes is the size of a new secret (160 bits, RFC 4226's
	// recommendation for HMAC-SHA1).
	SecretBytes = 20
	// Algorithm is the name of the HMAC used, as written in an otpauth URI.
	Algorithm = "SHA1"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrBadSecret: the base32 text is not a usable secret.
var ErrBadSecret = errors.New("totp: malformed secret")

// NewSecret returns SecretBytes random bytes.
func NewSecret() ([]byte, error) {
	b := make([]byte, SecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("totp: generate secret: %w", err)
	}
	return b, nil
}

// EncodeSecret is the unpadded upper-case base32 form apps accept.
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// DecodeSecret reads a base32 secret, ignoring case, spaces and padding.
func DecodeSecret(s string) ([]byte, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(s))
	b, err := b32.DecodeString(s)
	if err != nil || len(b) < 10 {
		return nil, ErrBadSecret
	}
	return b, nil
}

// Step is the time step t falls in.
func Step(t time.Time) int64 { return t.Unix() / int64(Period/time.Second) }

// hotp is RFC 4226 section 5.3 with the given number of digits.
func hotp(secret []byte, counter int64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// CodeDigits is the code of a step with the given number of digits (the RFC
// 6238 test vectors use 8).
func CodeDigits(secret []byte, step int64, digits int) string { return hotp(secret, step, digits) }

// Code is the 6-digit code of a step.
func Code(secret []byte, step int64) string { return hotp(secret, step, Digits) }

// Match reports whether code is the code of a step within Skew of now, and
// which step. Every candidate is compared in constant time. The caller must
// still refuse a step already used (replay protection).
func Match(secret []byte, code string, now time.Time) (int64, bool) {
	if len(code) != Digits {
		return 0, false
	}
	current := Step(now)
	var matched int64
	found := 0
	for d := int64(-Skew); d <= Skew; d++ {
		ok := subtle.ConstantTimeCompare([]byte(Code(secret, current+d)), []byte(code))
		if ok == 1 && found == 0 {
			matched = current + d
		}
		found |= ok
	}
	return matched, found == 1
}

// URI is the otpauth:// key URI an authenticator app scans: the label is
// "issuer:account" and the issuer is repeated as a parameter.
func URI(issuer, account string, secret []byte) string {
	// Parameter order is fixed for readability: secret first.
	query := fmt.Sprintf("secret=%s&issuer=%s&algorithm=%s&digits=%d&period=%d",
		EncodeSecret(secret), url.QueryEscape(issuer), Algorithm, Digits, int(Period/time.Second))
	return "otpauth://totp/" + url.PathEscape(issuer) + ":" + url.PathEscape(account) + "?" + query
}
