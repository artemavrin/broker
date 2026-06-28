package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestIssueAndVerify(t *testing.T) {
	s := NewSigner([]byte("0123456789abcdef0123456789abcdef"), 30*time.Minute)
	tok, ttl, err := s.Issue("sub-123", RoleInitiator)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if ttl != 30*time.Minute {
		t.Fatalf("expected ttl 30m, got %v", ttl)
	}
	id, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Subject != "sub-123" || id.Role != RoleInitiator {
		t.Fatalf("unexpected identity: %+v", id)
	}
}

func TestVerifyExpired(t *testing.T) {
	s := NewSigner([]byte("0123456789abcdef0123456789abcdef"), -time.Minute)
	tok, _, err := s.Issue("sub", RoleReceiver)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := s.Verify(tok); err == nil {
		t.Fatalf("expected expired token to fail verification")
	}
}

func TestVerifyWrongKey(t *testing.T) {
	a := NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	b := NewSigner([]byte("ffffffffffffffffffffffffffffffff"), time.Minute)
	tok, _, _ := a.Issue("sub", RoleInitiator)
	if _, err := b.Verify(tok); err == nil {
		t.Fatalf("expected token signed with different key to fail")
	}
}

func TestVerifyRejectsNoneAlg(t *testing.T) {
	s := NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	// Forge a token with alg "none".
	c := claims{Role: RoleInitiator, RegisteredClaims: jwt.RegisteredClaims{Subject: "sub"}}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, c)
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := s.Verify(signed); err == nil {
		t.Fatalf("expected alg=none token to be rejected")
	}
}
