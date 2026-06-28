package auth

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey struct{}

// FromContext returns the authenticated identity attached by Middleware, if any.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(*Identity)
	return id, ok
}

// withIdentity stores the identity in the context.
func withIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// Middleware verifies the Bearer token on each request and attaches the
// resulting Identity to the request context. Verification is stateless
// (signature + expiry only); no database lookup is performed.
func (s *Signer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeUnauthorized(w)
			return
		}
		id, err := s.Verify(token)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}

// bearerToken extracts the token from an "Authorization: Bearer <jwt>" header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
}
