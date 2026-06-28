package db

import (
	"context"
	"time"
)

// Message is a queue row as returned to its recipient. payload is raw bytes;
// the API layer base64-encodes it at the boundary.
type Message struct {
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	Payload   []byte    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

// sendQuery is a single statement (implicitly one transaction) that does the
// whole send: it validates addressing policy, conditionally inserts the
// message, and fires the doorbell — in one round-trip.
//
//   - The policy is "there exists a live receiver row (probeReceiver,
//     probeInitiator)". For an initiator that is its own target receiver; for a
//     receiver that is itself paired with the target initiator. The same EXISTS
//     gates the INSERT and is reported back, so a caller can tell a policy
//     failure (insert skipped, exists=false) apart from an idempotent duplicate
//     (insert skipped by ON CONFLICT, exists=true). Both yield zero inserted
//     rows, which is why the flag is needed.
//   - Evaluating the policy inside the INSERT removes the check-then-insert
//     race the previous two-query version had: both reads share one snapshot.
//   - The `notified` CTE references `ins` and is itself referenced by the final
//     SELECT, so pg_notify runs exactly once per inserted row and never on a
//     duplicate or a rejected send.
const sendQuery = `
WITH ins AS (
    INSERT INTO messages (from_addr, to_addr, payload, client_msg_id)
    SELECT $1::uuid, $2::uuid, $3::bytea, $4::text
    WHERE EXISTS (
        SELECT 1 FROM receivers
        WHERE id = $5::uuid AND initiator_id = $6::uuid AND NOT revoked
    )
    ON CONFLICT (from_addr, client_msg_id) WHERE client_msg_id IS NOT NULL DO NOTHING
    RETURNING id, to_addr
),
notified AS (
    SELECT pg_notify($7::text, json_build_object('to', to_addr)::text) FROM ins
)
SELECT
    EXISTS (
        SELECT 1 FROM receivers
        WHERE id = $5::uuid AND initiator_id = $6::uuid AND NOT revoked
    )                          AS policy_ok,
    (SELECT id FROM ins)       AS id,
    (SELECT count(*) FROM notified) AS notified`

// Send validates policy, inserts the message and rings the doorbell in a single
// round-trip. probeReceiver/probeInitiator identify the receiver row whose
// existence authorises the send (computed by the caller from the sender role).
//
// It returns policyOK=false when the addressing is not permitted (caller maps
// to 403), inserted=false with policyOK=true on an idempotent duplicate, and
// the new id otherwise. clientMsgID may be empty (no idempotency key).
func (db *DB) Send(ctx context.Context, from, to string, payload []byte, clientMsgID, probeReceiver, probeInitiator, channel string) (id int64, inserted, policyOK bool, err error) {
	var cmid *string
	if clientMsgID != "" {
		cmid = &clientMsgID
	}

	var (
		gotID    *int64
		notified int64
	)
	err = db.pool.QueryRow(ctx, sendQuery,
		from, to, payload, cmid, probeReceiver, probeInitiator, channel,
	).Scan(&policyOK, &gotID, &notified)
	if err != nil {
		return 0, false, false, err
	}
	if !policyOK {
		return 0, false, false, nil
	}
	if gotID == nil {
		return 0, false, true, nil // duplicate
	}
	return *gotID, true, true, nil
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
