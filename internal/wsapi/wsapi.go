// Package wsapi implements the WebSocket transport. It is a thin layer over
// the same queue the REST API uses: every frame is translated into a core
// service call, and data always comes from the database. The socket only adds
// a doorbell ("new") so clients can fetch promptly instead of polling.
package wsapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/artemavrin/broker/internal/auth"
	"github.com/artemavrin/broker/internal/core"
	"github.com/artemavrin/broker/internal/wshub"
	"github.com/coder/websocket"
)

const (
	pingInterval = 20 * time.Second
	writeTimeout = 10 * time.Second
	outBuffer    = 32
)

// Handler upgrades and serves WebSocket sessions.
type Handler struct {
	svc    *core.Service
	signer *auth.Signer
	hub    *wshub.Hub
	log    *slog.Logger
}

// New builds a WebSocket Handler.
func New(svc *core.Service, signer *auth.Signer, hub *wshub.Hub, log *slog.Logger) *Handler {
	return &Handler{svc: svc, signer: signer, hub: hub, log: log}
}

// logFailure records a frame failure, demoting a session that merely went away
// to debug level: tearing down a connection cancels its context, so a routine
// disconnect would otherwise surface as an error on every in-flight fetch or
// ack.
func (h *Handler) logFailure(msg string, err error) {
	if core.Disconnected(err) {
		h.log.Debug(msg, "err", err)
		return
	}
	h.log.Error(msg, "err", err)
}

// inbound is the union of all client→server frames.
type inbound struct {
	Type        string  `json:"type"`
	Max         int     `json:"max"`
	IDs         []int64 `json:"ids"`
	To          string  `json:"to"`
	Payload     string  `json:"payload"`
	ClientMsgID string  `json:"client_msg_id"`
}

// ServeHTTP authenticates the handshake, upgrades the connection, registers a
// hub session, and runs the read/write loops until either side closes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := h.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.log.Warn("ws accept failed", "err", err)
		return
	}

	sess := h.hub.Add(id.Subject)
	defer h.hub.Remove(sess)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	out := make(chan any, outBuffer)
	go h.writeLoop(ctx, cancel, c, sess, out)
	h.readLoop(ctx, cancel, c, id, out)

	c.Close(websocket.StatusNormalClosure, "bye")
}

// authenticate extracts and verifies the Bearer token from the handshake.
func (h *Handler) authenticate(r *http.Request) (auth.Identity, error) {
	const prefix = "Bearer "
	hdr := r.Header.Get("Authorization")
	if len(hdr) <= len(prefix) || hdr[:len(prefix)] != prefix {
		return auth.Identity{}, errors.New("missing bearer token")
	}
	idp, err := h.signer.Verify(hdr[len(prefix):])
	if err != nil {
		return auth.Identity{}, err
	}
	return *idp, nil
}

// writeLoop owns the connection's write side: it is the only goroutine that
// writes frames, serialising doorbells, responses and pings.
func (h *Handler) writeLoop(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, sess *wshub.Session, out <-chan any) {
	defer cancel()
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.Signal():
			if err := h.write(ctx, c, frameNew{Type: "new"}); err != nil {
				return
			}
		case msg := <-out:
			if err := h.write(ctx, c, msg); err != nil {
				return
			}
		case <-ticker.C:
			pingCtx, pcancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Ping(pingCtx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}

// write marshals and sends one frame under a write timeout.
func (h *Handler) write(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, data)
}

// readLoop reads and dispatches client frames until the connection closes.
func (h *Handler) readLoop(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, id auth.Identity, out chan<- any) {
	defer cancel()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var in inbound
		if err := json.Unmarshal(data, &in); err != nil {
			h.send(ctx, out, errorFrame("bad_request", "invalid frame"))
			continue
		}
		h.dispatch(ctx, id, in, out)
	}
}

// dispatch routes a single decoded frame to the matching service call.
func (h *Handler) dispatch(ctx context.Context, id auth.Identity, in inbound, out chan<- any) {
	switch in.Type {
	case "fetch":
		h.doFetch(ctx, id, in, out)
	case "ack":
		h.doAck(ctx, id, in, out)
	case "send":
		h.doSend(ctx, id, in, out)
	default:
		h.send(ctx, out, errorFrame("bad_request", "unknown frame type"))
	}
}

func (h *Handler) doFetch(ctx context.Context, id auth.Identity, in inbound, out chan<- any) {
	msgs, err := h.svc.Fetch(ctx, id, in.Max)
	if err != nil {
		h.logFailure("ws fetch failed", err)
		h.send(ctx, out, errorFrame("internal", "fetch failed"))
		return
	}
	items := make([]wsMessage, 0, len(msgs))
	for _, m := range msgs {
		items = append(items, wsMessage{
			ID:        m.ID,
			From:      m.From,
			Payload:   base64.StdEncoding.EncodeToString(m.Payload),
			CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	h.send(ctx, out, frameMessages{Type: "messages", Items: items})
}

func (h *Handler) doAck(ctx context.Context, id auth.Identity, in inbound, out chan<- any) {
	if _, err := h.svc.Ack(ctx, id, in.IDs); err != nil {
		h.logFailure("ws ack failed", err)
		h.send(ctx, out, errorFrame("internal", "ack failed"))
		return
	}
	h.send(ctx, out, frameAckOK{Type: "ack_ok", IDs: in.IDs})
}

func (h *Handler) doSend(ctx context.Context, id auth.Identity, in inbound, out chan<- any) {
	if in.To == "" {
		h.send(ctx, out, errorFrame("bad_request", "missing 'to'"))
		return
	}
	payload, err := base64.StdEncoding.DecodeString(in.Payload)
	if err != nil {
		h.send(ctx, out, errorFrame("bad_request", "payload must be base64"))
		return
	}
	msgID, inserted, err := h.svc.Send(ctx, id, in.To, payload, in.ClientMsgID)
	switch {
	case errors.Is(err, core.ErrPayloadTooLarge):
		h.send(ctx, out, errorFrame("payload_too_large", "payload too large"))
	case errors.Is(err, core.ErrPolicy):
		h.send(ctx, out, errorFrame("policy", "destination not permitted"))
	case err != nil:
		h.logFailure("ws send failed", err)
		h.send(ctx, out, errorFrame("internal", "send failed"))
	case !inserted:
		h.send(ctx, out, frameSent{Type: "sent", Duplicate: true})
	default:
		h.send(ctx, out, frameSent{Type: "sent", ID: &msgID})
	}
}

// send enqueues a frame for the write loop, abandoning it if the connection is
// being torn down (so a dead writer can never deadlock the reader).
func (h *Handler) send(ctx context.Context, out chan<- any, v any) {
	select {
	case out <- v:
	case <-ctx.Done():
	}
}
