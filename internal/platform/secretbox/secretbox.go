// Package secretbox encrypts small secrets at rest (the administrators' TOTP
// secrets) with AES-256-GCM under an application key from configuration.
//
// A sealed value is: version byte (1) || 12-byte random nonce || ciphertext
// and tag. The caller passes associated data (for example the owner's user
// ID), so a sealed value copied to another row does not open.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the length of a key: AES-256.
const KeySize = 32

const version byte = 1

var (
	// ErrBadKey: the key is not KeySize bytes of standard base64.
	ErrBadKey = errors.New("secretbox: the key must be 32 bytes, base64-encoded")
	// ErrOpen: the value is malformed, was sealed under another key or
	// with other associated data, or was altered.
	ErrOpen = errors.New("secretbox: cannot open sealed value")
)

// Box seals and opens values under one key.
type Box struct{ aead cipher.AEAD }

// New builds a Box from a raw KeySize-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, ErrBadKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return &Box{aead: aead}, nil
}

// ParseKey decodes a base64 key (standard or URL alphabet, padded or not)
// and checks its length. The error never echoes the value.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != KeySize {
				return nil, ErrBadKey
			}
			return b, nil
		}
	}
	return nil, ErrBadKey
}

// DevelopmentKey is a FIXED, publicly known key for development only, so a
// development database and the API agree across restarts. It protects
// nothing and must never be used anywhere else.
func DevelopmentKey() []byte {
	sum := sha256.Sum256([]byte("rightproperty365 DEVELOPMENT-ONLY mfa key; not a secret; never use outside development"))
	return sum[:]
}

// Seal encrypts plaintext bound to aad.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, version)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, aad), nil
}

// Open decrypts a value from Seal with the same aad.
func (b *Box) Open(sealed, aad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < 1+n+b.aead.Overhead() || sealed[0] != version {
		return nil, ErrOpen
	}
	plain, err := b.aead.Open(nil, sealed[1:1+n], sealed[1+n:], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}
