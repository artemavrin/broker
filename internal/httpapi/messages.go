package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/artemavrin/broker/internal/core"
	"github.com/artemavrin/broker/internal/db"
)

type sendRequest struct {
	To          string `json:"to"`
	Payload     string `json:"payload"` // base64
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

type sendResponse struct {
	ID        *int64 `json:"id"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// apiMessage is the wire form of a message, with the payload base64-encoded.
type apiMessage struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
}

func toAPIMessage(m db.Message) apiMessage {
	return apiMessage{
		ID:        m.ID,
		From:      m.From,
		Payload:   base64.StdEncoding.EncodeToString(m.Payload),
		CreatedAt: m.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
}

// handleSend enqueues a message. "from" is taken from the token; "to" is
// validated by policy before insertion.
func (a *API) handleSend(w http.ResponseWriter, r *http.Request) {
	id, ok := identity(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// Guard against oversized bodies before reading them into memory. The cap
	// allows for base64 expansion (~4/3) plus JSON framing on top of the raw
	// payload limit; anything larger is rejected as 413.
	bodyCap := int64(a.svc.MaxPayload())*2 + 4096
	r.Body = http.MaxBytesReader(w, r.Body, bodyCap)

	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "missing 'to'")
		return
	}
	payload, err := base64.StdEncoding.DecodeString(req.Payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "payload must be base64")
		return
	}

	msgID, inserted, err := a.svc.Send(r.Context(), id, req.To, payload, req.ClientMsgID)
	switch {
	case errors.Is(err, core.ErrPayloadTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	case errors.Is(err, core.ErrPolicy):
		writeError(w, http.StatusForbidden, "destination not permitted")
		return
	case err != nil:
		a.log.Error("send failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if !inserted {
		writeJSON(w, http.StatusOK, sendResponse{ID: nil, Duplicate: true})
		return
	}
	writeJSON(w, http.StatusOK, sendResponse{ID: &msgID})
}

// handleFetch claims and returns up to max messages for the caller's inbox.
func (a *API) handleFetch(w http.ResponseWriter, r *http.Request) {
	id, ok := identity(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	max := 0
	if v := r.URL.Query().Get("max"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "max must be a positive integer")
			return
		}
		max = n
	}

	msgs, err := a.svc.Fetch(r.Context(), id, max)
	if err != nil {
		a.log.Error("fetch failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apiMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toAPIMessage(m))
	}
	writeJSON(w, http.StatusOK, out)
}

type ackRequest struct {
	IDs []int64 `json:"ids"`
}

type ackResponse struct {
	Acked int64 `json:"acked"`
}

// handleAck deletes acknowledged messages from the caller's inbox.
func (a *API) handleAck(w http.ResponseWriter, r *http.Request) {
	id, ok := identity(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req ackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	n, err := a.svc.Ack(r.Context(), id, req.IDs)
	if err != nil {
		a.log.Error("ack failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, ackResponse{Acked: n})
}
