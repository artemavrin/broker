package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a lookup matches no live row.
var ErrNotFound = errors.New("not found")

// Participant is the result of a secret lookup: which table the secret
// belonged to and the matching id.
type Participant struct {
	Role string // "initiator" or "receiver"
	ID   string // UUID
}

// CreateInitiator inserts a new initiator with the given secret hash and
// returns its generated id.
func (db *DB) CreateInitiator(ctx context.Context, secretHash []byte) (string, error) {
	var id string
	err := db.pool.QueryRow(ctx,
		`INSERT INTO initiators (secret_hash) VALUES ($1) RETURNING id`,
		secretHash,
	).Scan(&id)
	return id, err
}

// LookupBySecretHash searches both the initiators and receivers tables for a
// live (non-revoked) participant whose secret hash matches. It returns
// ErrNotFound when there is no match. This backs the /auth/token endpoint;
// revocation is checked here, at issue time only.
func (db *DB) LookupBySecretHash(ctx context.Context, secretHash []byte) (Participant, error) {
	var p Participant
	err := db.pool.QueryRow(ctx, `
		SELECT 'initiator' AS role, id::text FROM initiators WHERE secret_hash = $1 AND NOT revoked
		UNION ALL
		SELECT 'receiver'  AS role, id::text FROM receivers  WHERE secret_hash = $1 AND NOT revoked
		LIMIT 1`,
		secretHash,
	).Scan(&p.Role, &p.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Participant{}, ErrNotFound
	}
	return p, err
}
