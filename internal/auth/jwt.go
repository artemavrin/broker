// Package auth issues and verifies the stateless HS256 JWTs used to
// authenticate participants, and provides middleware to extract identity
// from the Authorization header.
package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Role identifies the kind of participant a token belongs to.
type Role string

const (
	RoleInitiator Role = "initiator"
	RoleReceiver  Role = "receiver"
)

// Valid reports whether r is a recognised role.
func (r Role) Valid() bool {
	return r == RoleInitiator || r == RoleReceiver
}

// Identity is the authenticated subject extracted from a verified token.
type Identity struct {
	Subject string // participant id (initiator or receiver UUID)
	Role    Role
}

// claims is the JWT payload. The standard "sub", "iat" and "exp" claims are
// carried by jwt.RegisteredClaims; role is a custom claim.
type claims struct {
	Role Role `json:"role"`
	jwt.RegisteredClaims
}

// Signer issues and verifies tokens with a fixed HS256 key and TTL.
type Signer struct {
	key []byte
	ttl time.Duration
}

// NewSigner builds a Signer. key must be the HS256 signing secret.
func NewSigner(key []byte, ttl time.Duration) *Signer {
	return &Signer{key: key, ttl: ttl}
}

// Issue mints a signed token for the given subject and role. exp is set to
// now + ttl. Revocation is intentionally not encoded: it is checked only at
// issue time, so the TTL must be kept short.
func (s *Signer) Issue(subject string, role Role) (token string, expiresIn time.Duration, err error) {
	now := time.Now()
	c := claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", 0, err
	}
	return signed, s.ttl, nil
}

// Verify checks the token's signature and expiry (stateless: no DB lookup)
// and returns the embedded identity.
func (s *Signer) Verify(token string) (*Identity, error) {
	var c claims
	parsed, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.key, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	if c.Subject == "" {
		return nil, fmt.Errorf("token missing subject")
	}
	if !c.Role.Valid() {
		return nil, fmt.Errorf("token has invalid role")
	}
	return &Identity{Subject: c.Subject, Role: c.Role}, nil
}
