package secret

import (
	"encoding/base64"
	"testing"
)

func TestGenerateUniqueAndDecodable(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		s, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate secret generated: %s", s)
		}
		seen[s] = struct{}{}

		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("secret is not valid base64 raw-url: %v", err)
		}
		if len(raw) != rawBytes {
			t.Fatalf("expected %d random bytes, got %d", rawBytes, len(raw))
		}
	}
}

func TestHashIsDeterministicAndSized(t *testing.T) {
	h1 := Hash("hello")
	h2 := Hash("hello")
	if len(h1) != 32 {
		t.Fatalf("expected 32-byte hash, got %d", len(h1))
	}
	if !Equal(h1, h2) {
		t.Fatalf("hash of identical input must match")
	}
	if Equal(Hash("hello"), Hash("world")) {
		t.Fatalf("hash of different input must not match")
	}
}

func TestEqualConstantTime(t *testing.T) {
	a := Hash("secret-a")
	b := Hash("secret-a")
	c := Hash("secret-b")

	if !Equal(a, b) {
		t.Fatalf("equal hashes reported unequal")
	}
	if Equal(a, c) {
		t.Fatalf("unequal hashes reported equal")
	}
	// Differing lengths must be unequal (and must not panic).
	if Equal(a, a[:16]) {
		t.Fatalf("different-length inputs must be unequal")
	}
}
