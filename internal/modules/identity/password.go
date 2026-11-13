package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password length limits, counted in characters.
const (
	MinPasswordLength = 10
	MaxPasswordLength = 128
)

const (
	argonSaltLen = 16
	argonKeyLen  = 32
	// Upper bounds accepted when verifying a stored hash, so a corrupted or
	// hostile row cannot make the server allocate gigabytes.
	argonMaxMemoryKiB = 1 << 20 // 1 GiB
	argonMaxTime      = 16
)

// Hasher hashes passwords with Argon2id and encodes them in the PHC string
// format: $argon2id$v=19$m=<KiB>,t=<passes>,p=<threads>$<salt>$<hash>
// (unpadded standard base64). The parameters travel with every hash, so they
// can be raised later without invalidating existing passwords.
type Hasher struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// DefaultHasher uses the OWASP-recommended Argon2id minimum (19 MiB, 2
// passes, 1 thread).
func DefaultHasher() Hasher { return Hasher{MemoryKiB: 19 * 1024, Time: 2, Threads: 1} }

// Hash returns the encoded hash of password under a fresh random salt.
func (h Hasher) Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.Time, h.MemoryKiB, h.Threads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.MemoryKiB, h.Time, h.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

var errMalformedHash = errors.New("malformed password hash")

// VerifyPassword reports whether password matches the encoded Argon2id hash.
// The comparison is constant-time. A malformed hash is an error, not a mismatch.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errMalformedHash
	}
	var memory, passes uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &passes, &threads); err != nil ||
		memory == 0 || memory > argonMaxMemoryKiB || passes == 0 || passes > argonMaxTime || threads == 0 {
		return false, errMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, errMalformedHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 128 {
		return false, errMalformedHash
	}
	got := argon2.IDKey([]byte(password), salt, passes, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// tokenBytes is the entropy of session, verification and reset tokens.
const tokenBytes = 32

// newToken returns a random opaque token (base64url, no padding) and the
// SHA-256 digest that is stored in its place.
func newToken() (token string, hash []byte, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

// hashToken returns the SHA-256 digest under which a token is stored.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// plausibleToken cheaply rejects values that cannot be one of our tokens, so
// junk never reaches the store.
func plausibleToken(token string) bool {
	return len(token) == base64.RawURLEncoding.EncodedLen(tokenBytes)
}
