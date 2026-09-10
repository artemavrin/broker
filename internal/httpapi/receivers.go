package httpapi

import (
	"errors"
	"net/http"

	"github.com/artemavrin/broker/internal/auth"
	"github.com/artemavrin/broker/internal/core"
)

type createReceiverResponse struct {
	ReceiverID     string `json:"receiver_id"`
	ReceiverSecret string `json:"receiver_secret"` // returned exactly once
}

// requireInitiator returns the identity only if it is an initiator, otherwise
// writes 403 and returns false.
func requireInitiator(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	id, ok := identity(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return auth.Identity{}, false
	}
	if id.Role != auth.RoleInitiator {
		writeError(w, http.StatusForbidden, "initiator role required")
		return auth.Identity{}, false
	}
	return id, true
}

// handleCreateReceiver provisions a receiver for the calling initiator and
// returns the id together with a one-time secret.
func (a *API) handleCreateReceiver(w http.ResponseWriter, r *http.Request) {
	id, ok := requireInitiator(w, r)
	if !ok {
		return
	}
	rid, rawSecret, err := a.svc.NewReceiver(r.Context(), id.Subject)
	if err != nil {
		a.logFailure("create receiver failed", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, createReceiverResponse{
		ReceiverID:     rid,
		ReceiverSecret: rawSecret,
	})
}

// handleListReceivers returns the calling initiator's receivers.
func (a *API) handleListReceivers(w http.ResponseWriter, r *http.Request) {
	id, ok := requireInitiator(w, r)
	if !ok {
		return
	}
	list, err := a.svc.ListReceivers(r.Context(), id.Subject)
	if err != nil {
		a.logFailure("list receivers failed", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleDeleteReceiver revokes a receiver owned by the calling initiator.
func (a *API) handleDeleteReceiver(w http.ResponseWriter, r *http.Request) {
	id, ok := requireInitiator(w, r)
	if !ok {
		return
	}
	rid := r.PathValue("id")
	err := a.svc.RevokeReceiver(r.Context(), id.Subject, rid)
	switch {
	case errors.Is(err, core.ErrForbidden):
		// A non-owned or missing receiver is reported as 403 so callers cannot
		// probe for the existence of receivers they do not own.
		writeError(w, http.StatusForbidden, "not the owner")
	case err != nil:
		a.logFailure("revoke receiver failed", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
