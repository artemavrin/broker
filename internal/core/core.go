// Package core is the transport-agnostic heart of the broker. It enforces
// addressing policy, payload limits and idempotency on top of the db layer,
// so the HTTP and WebSocket front-ends share one implementation of the rules.
package core

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/artemavrin/broker/internal/auth"
	"github.com/artemavrin/broker/internal/db"
	"github.com/artemavrin/broker/internal/secret"
)

// Sentinel errors returned by the service. Transports map these to status
// codes (HTTP) or error frames (WS).
var (
	// ErrPolicy means the "to" target is not permitted for this sender.
	ErrPolicy = errors.New("policy violation")
	// ErrPayloadTooLarge means the payload exceeds MaxPayloadBytes.
	ErrPayloadTooLarge = errors.New("payload too large")
	// ErrForbidden means the caller is not the owner of the target resource.
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound mirrors db.ErrNotFound at the service boundary.
	ErrNotFound = errors.New("not found")
)

// Disconnected reports whether err only means the caller went away: a closed
// connection or a shutting-down server cancels the request context, so queries
// still in flight come back with context.Canceled. Transports use this to keep
// ordinary disconnects out of the error log — a receiver dropping off is
// routine, not an incident. A deadline that expired is deliberately not
// included: that is a real timeout worth reporting.
func Disconnected(err error) bool {
	return errors.Is(err, context.Canceled)
}

// Service bundles the database and the policy knobs.
type Service struct {
	db            *db.DB
	maxFetch      int
	maxPayload    int
	visibility    time.Duration
	listenChannel string

	// Live throughput counters since process start. Acked messages are deleted
	// from the queue, so these are the only source of historical throughput.
	sentTotal  atomic.Int64
	ackedTotal atomic.Int64
}

// Throughput is a snapshot of the live counters since process start.
type Throughput struct {
	Sent  int64
	Acked int64
}

// Throughput returns the cumulative sent/acked counts since start.
func (s *Service) Throughput() Throughput {
	return Throughput{Sent: s.sentTotal.Load(), Acked: s.ackedTotal.Load()}
}

// New builds a Service.
func New(database *db.DB, maxFetch, maxPayload int, visibility time.Duration, listenChannel string) *Service {
	return &Service{
		db:            database,
		maxFetch:      maxFetch,
		maxPayload:    maxPayload,
		visibility:    visibility,
		listenChannel: listenChannel,
	}
}

// DB exposes the underlying database (used by the auth-token endpoint and the
// listener wiring).
func (s *Service) DB() *db.DB { return s.db }

// MaxFetch is the hard cap on a single fetch request.
func (s *Service) MaxFetch() int { return s.maxFetch }

// MaxPayload is the per-message payload limit in bytes.
func (s *Service) MaxPayload() int { return s.maxPayload }

// NewReceiver provisions a receiver for the given initiator and returns its id
// plus a freshly generated secret. The raw secret is returned exactly once and
// never stored; only its hash is persisted.
func (s *Service) NewReceiver(ctx context.Context, initiatorID string) (id, rawSecret string, err error) {
	rawSecret, err = secret.Generate()
	if err != nil {
		return "", "", err
	}
	id, err = s.db.CreateReceiver(ctx, initiatorID, secret.Hash(rawSecret))
	if err != nil {
		return "", "", err
	}
	return id, rawSecret, nil
}

// ListReceivers returns the receivers owned by initiatorID.
func (s *Service) ListReceivers(ctx context.Context, initiatorID string) ([]db.Receiver, error) {
	return s.db.ListReceivers(ctx, initiatorID)
}

// RevokeReceiver revokes a receiver owned by initiatorID. It returns
// ErrForbidden when the receiver does not exist or is owned by someone else.
func (s *Service) RevokeReceiver(ctx context.Context, initiatorID, receiverID string) error {
	err := s.db.RevokeReceiver(ctx, receiverID, initiatorID)
	if errors.Is(err, db.ErrNotFound) {
		return ErrForbidden
	}
	return err
}

// Send validates addressing policy and payload size, then enqueues the
// message. from is always taken from the authenticated identity, never from
// the request body. It returns the new id and whether a row was actually
// inserted (false on an idempotent duplicate).
//
// Policy is enforced inside the insert (see db.Send): we identify the receiver
// row whose existence authorises this send and let the database gate on it in
// the same statement, avoiding a separate query and a check-then-insert race.
//   - initiator → its own receiver: the row is (id=to, initiator_id=sender).
//   - receiver  → its own initiator: the row is (id=sender, initiator_id=to).
func (s *Service) Send(ctx context.Context, id auth.Identity, to string, payload []byte, clientMsgID string) (msgID int64, inserted bool, err error) {
	if len(payload) > s.maxPayload {
		return 0, false, ErrPayloadTooLarge
	}

	var probeReceiver, probeInitiator string
	switch id.Role {
	case auth.RoleInitiator:
		probeReceiver, probeInitiator = to, id.Subject
	case auth.RoleReceiver:
		probeReceiver, probeInitiator = id.Subject, to
	default:
		return 0, false, ErrPolicy
	}

	msgID, inserted, policyOK, err := s.db.Send(
		ctx, id.Subject, to, payload, clientMsgID, probeReceiver, probeInitiator, s.listenChannel,
	)
	if err != nil {
		return 0, false, err
	}
	if !policyOK {
		return 0, false, ErrPolicy
	}
	if inserted {
		s.sentTotal.Add(1)
	}
	return msgID, inserted, nil
}

// Fetch claims and returns up to max messages for the identity's inbox,
// clamping max to [1, MaxFetch].
func (s *Service) Fetch(ctx context.Context, id auth.Identity, max int) ([]db.Message, error) {
	if max <= 0 || max > s.maxFetch {
		max = s.maxFetch
	}
	return s.db.Fetch(ctx, id.Subject, max, s.visibility)
}

// Ack deletes the given message ids from the identity's own inbox and returns
// the number removed.
func (s *Service) Ack(ctx context.Context, id auth.Identity, ids []int64) (int64, error) {
	n, err := s.db.Ack(ctx, id.Subject, ids)
	if err == nil && n > 0 {
		s.ackedTotal.Add(n)
	}
	return n, err
}

// NewInitiator provisions a new initiator and returns its id plus a freshly
// generated secret, returned exactly once (only its hash is stored). Used by
// the admin dashboard to onboard initiators without the CLI.
func (s *Service) NewInitiator(ctx context.Context) (id, rawSecret string, err error) {
	rawSecret, err = secret.Generate()
	if err != nil {
		return "", "", err
	}
	id, err = s.db.CreateInitiator(ctx, secret.Hash(rawSecret))
	if err != nil {
		return "", "", err
	}
	return id, rawSecret, nil
}
