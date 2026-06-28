package db

import (
	"context"
	"time"
)

// Receiver is a row from the receivers table, minus its secret hash.
type Receiver struct {
	ID        string    `json:"id"`
	Revoked   bool      `json:"revoked"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateReceiver inserts a receiver owned by initiatorID with the given secret
// hash and returns its generated id.
func (db *DB) CreateReceiver(ctx context.Context, initiatorID string, secretHash []byte) (string, error) {
	var id string
	err := db.pool.QueryRow(ctx,
		`INSERT INTO receivers (initiator_id, secret_hash) VALUES ($1, $2) RETURNING id`,
		initiatorID, secretHash,
	).Scan(&id)
	return id, err
}

// ListReceivers returns all receivers owned by initiatorID, newest first.
func (db *DB) ListReceivers(ctx context.Context, initiatorID string) ([]Receiver, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id::text, revoked, created_at FROM receivers WHERE initiator_id = $1 ORDER BY created_at DESC`,
		initiatorID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Receiver{}
	for rows.Next() {
		var r Receiver
		if err := rows.Scan(&r.ID, &r.Revoked, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeReceiver marks the receiver as revoked, but only if it is owned by
// initiatorID. It returns ErrNotFound when no such owned receiver exists
// (covering both "does not exist" and "not the owner").
func (db *DB) RevokeReceiver(ctx context.Context, receiverID, initiatorID string) error {
	tag, err := db.pool.Exec(ctx,
		`UPDATE receivers SET revoked = true WHERE id = $1 AND initiator_id = $2`,
		receiverID, initiatorID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
