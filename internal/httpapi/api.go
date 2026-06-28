// Package httpapi implements the REST front-end. All payloads cross the API
// boundary as base64; "from" is always derived from the token, never the body.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/artemavrin/broker/internal/auth"
	"github.com/artemavrin/broker/internal/core"
	"github.com/artemavrin/broker/internal/wsapi"
)

// API holds the dependencies shared by every handler.
type API struct {
	svc       *core.Service
	signer    *auth.Signer
	ws        *wsapi.Handler
	log       *slog.Logger
	authLimit *rateLimiter
}

// New builds the API. ws may be nil if the WebSocket front-end is disabled.
// authRatePerMin throttles /auth/token per client IP (0 disables).
func New(svc *core.Service, signer *auth.Signer, ws *wsapi.Handler, authRatePerMin int, log *slog.Logger) *API {
	return &API{
		svc:       svc,
		signer:    signer,
		ws:        ws,
		log:       log,
		authLimit: newRateLimiter(authRatePerMin),
	}
}

// Routes wires the mux. Public endpoints (/healthz, /v1/auth/token) are
// registered directly; everything under /v1 otherwise sits behind the Bearer
// middleware.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealthz)
	mux.HandleFunc("POST /v1/auth/token", a.handleToken)

	authed := http.NewServeMux()
	authed.HandleFunc("POST /v1/receivers", a.handleCreateReceiver)
	authed.HandleFunc("GET /v1/receivers", a.handleListReceivers)
	authed.HandleFunc("DELETE /v1/receivers/{id}", a.handleDeleteReceiver)
	authed.HandleFunc("POST /v1/messages", a.handleSend)
	authed.HandleFunc("GET /v1/messages", a.handleFetch)
	authed.HandleFunc("POST /v1/messages/ack", a.handleAck)
	if a.ws != nil {
		authed.HandleFunc("GET /v1/ws", a.ws.ServeHTTP)
	}

	mux.Handle("/v1/", a.signer.Middleware(authed))
	return mux
}

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// identity pulls the authenticated identity placed by the middleware.
func identity(r *http.Request) (auth.Identity, bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok || id == nil {
		return auth.Identity{}, false
	}
	return *id, true
}

// writeJSON encodes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError emits a JSON error envelope.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
