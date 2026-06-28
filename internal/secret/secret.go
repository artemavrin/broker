// Package secret handles generation, hashing and constant-time comparison
// of participant secrets. Raw secrets are never persisted; only their
// SHA-256 hashes are stored.
package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// rawBytes is the entropy length of a generated secret.
const rawBytes = 32

// Generate returns a fresh, cryptographically random secret encoded with
// base64 raw-url (no padding). It uses crypto/rand exclusively.
func Generate() (string, error) {
	buf := make([]byte, rawBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Hash returns the SHA-256 digest (32 bytes) of the given secret. This is
// the only representation of a secret that is ever stored.
func Hash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// Equal reports whether two hashes match, using a constant-time comparison
// to avoid leaking information through timing.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
