// Package core is the transport-agnostic heart of the broker. It enforces
// addressing policy, payload limits and idempotency on top of the db layer,
// so the HTTP and WebSocket front-ends share one implementation of the rules.
package core

import (
	"context"
	"errors"
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

// Service bundles the database and the policy knobs.
type Service struct {
	db            *db.DB
	maxFetch      int
	maxPayload    int
	visibility    time.Duration
	listenChannel string
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
func (s *Service) Send(ctx context.Context, id auth.Identity, to string, payload []byte, clientMsgID string) (msgID int64, inserted bool, err error) {
	if len(payload) > s.maxPayload {
		return 0, false, ErrPayloadTooLarge
	}
	if err := s.checkPolicy(ctx, id, to); err != nil {
		return 0, false, err
	}
	return s.db.Send(ctx, id.Subject, to, payload, clientMsgID, s.listenChannel)
}

// checkPolicy enforces the star topology:
//   - an initiator may only address its own non-revoked receivers;
//   - a receiver may only address its own initiator.
func (s *Service) checkPolicy(ctx context.Context, id auth.Identity, to string) error {
	switch id.Role {
	case auth.RoleInitiator:
		ok, err := s.db.OwnsLiveReceiver(ctx, id.Subject, to)
		if err != nil {
			return err
		}
		if !ok {
			return ErrPolicy
		}
		return nil
	case auth.RoleReceiver:
		initiatorID, err := s.db.ReceiverInitiator(ctx, id.Subject)
		if errors.Is(err, db.ErrNotFound) {
			// The receiver was revoked after its token was issued.
			return ErrPolicy
		}
		if err != nil {
			return err
		}
		if initiatorID != to {
			return ErrPolicy
		}
		return nil
	default:
		return ErrPolicy
	}
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
	return s.db.Ack(ctx, id.Subject, ids)
}
