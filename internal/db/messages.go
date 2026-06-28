package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Message is a queue row as returned to its recipient. payload is raw bytes;
// the API layer base64-encodes it at the boundary.
type Message struct {
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	Payload   []byte    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

// Send inserts a message and, when it is genuinely new, fires a
// pg_notify("<channel>", {"to":...}) in the same transaction so that a live
// recipient session gets a doorbell. Idempotency is enforced by the partial
// unique index on (from_addr, client_msg_id): a duplicate send returns
// inserted=false with no notification.
//
// clientMsgID may be empty, in which case no idempotency key applies.
func (db *DB) Send(ctx context.Context, from, to string, payload []byte, clientMsgID string, channel string) (id int64, inserted bool, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)

	var cmid *string
	if clientMsgID != "" {
		cmid = &clientMsgID
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO messages (from_addr, to_addr, payload, client_msg_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (from_addr, client_msg_id) WHERE client_msg_id IS NOT NULL DO NOTHING
		RETURNING id`,
		from, to, payload, cmid,
	)
	if err := row.Scan(&id); err != nil {
		if err == pgx.ErrNoRows {
			// Conflict: a duplicate. No row inserted, no doorbell.
			if cerr := tx.Commit(ctx); cerr != nil {
				return 0, false, cerr
			}
			return 0, false, nil
		}
		return 0, false, err
	}

	notice, _ := json.Marshal(struct {
		To string `json:"to"`
	}{To: to})
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, string(notice)); err != nil {
		return 0, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// Fetch atomically claims up to max messages addressed to "to" whose lock has
// expired, extends their visibility lock by vis, and returns them. The
// FOR UPDATE SKIP LOCKED clause guarantees concurrent fetchers (WS and HTTP)
// never receive the same row.
func (db *DB) Fetch(ctx context.Context, to string, max int, vis time.Duration) ([]Message, error) {
	rows, err := db.pool.Query(ctx, `
		UPDATE messages
		SET locked_until = now() + $3::interval, attempts = attempts + 1
		WHERE id IN (
			SELECT id FROM messages
			WHERE to_addr = $1 AND (locked_until IS NULL OR locked_until < now())
			ORDER BY id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, from_addr::text, payload, created_at`,
		to, max, vis.String(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.Payload, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Ack permanently deletes the given message ids, scoped to the recipient's
// own inbox so a participant can only ack messages addressed to it. It returns
// the number of rows actually removed.
func (db *DB) Ack(ctx context.Context, to string, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM messages WHERE id = ANY($1::bigint[]) AND to_addr = $2`,
		ids, to,
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
