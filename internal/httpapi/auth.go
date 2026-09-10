package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/artemavrin/broker/internal/auth"
	"github.com/artemavrin/broker/internal/db"
	"github.com/artemavrin/broker/internal/secret"
)

type tokenRequest struct {
	Secret string `json:"secret"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"` // seconds
}

// handleToken exchanges a participant secret for a short-lived JWT. The secret
// is hashed and looked up across both participant tables; revocation is checked
// here, at issue time.
func (a *API) handleToken(w http.ResponseWriter, r *http.Request) {
	if !a.authLimit.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	// Bound the body: the only field is a short secret.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var req tokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Secret == "" {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	p, err := a.svc.DB().LookupBySecretHash(r.Context(), secret.Hash(req.Secret))
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "invalid secret")
		return
	}
	if err != nil {
		a.logFailure("token lookup failed", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	token, ttl, err := a.signer.Issue(p.ID, auth.Role(p.Role))
	if err != nil {
		a.log.Error("token issue failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: token,
		ExpiresIn:   int(ttl.Seconds()),
	})
}
