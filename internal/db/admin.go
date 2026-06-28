package db

import (
	"context"
	"time"
)

// Stats is an aggregate snapshot of the broker's persistent state for the
// admin dashboard. All values reflect the live database, not history (acked
// messages are deleted, so throughput-over-time is tracked separately by
// in-memory counters in the core service).
type Stats struct {
	Initiators        int
	InitiatorsRevoked int
	Receivers         int
	ReceiversRevoked  int
	QueueDepth        int64   // total undelivered messages
	Recipients        int     // distinct inboxes with backlog
	InFlight          int64   // messages currently locked (being processed)
	OldestSeconds     float64 // age of the oldest queued message
}

// AdminStats gathers the dashboard aggregates in three small queries.
func (db *DB) AdminStats(ctx context.Context) (Stats, error) {
	var s Stats
	if err := db.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE revoked) FROM initiators`,
	).Scan(&s.Initiators, &s.InitiatorsRevoked); err != nil {
		return s, err
	}
	if err := db.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE revoked) FROM receivers`,
	).Scan(&s.Receivers, &s.ReceiversRevoked); err != nil {
		return s, err
	}
	if err := db.pool.QueryRow(ctx, `
		SELECT
			count(*),
			count(DISTINCT to_addr),
			count(*) FILTER (WHERE locked_until IS NOT NULL AND locked_until > now()),
			COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)
		FROM messages`,
	).Scan(&s.QueueDepth, &s.Recipients, &s.InFlight, &s.OldestSeconds); err != nil {
		return s, err
	}
	return s, nil
}

// RecipientBacklog is a single inbox's queue depth, for the top-N chart.
type RecipientBacklog struct {
	ToAddr string
	Count  int64
}

// TopBacklogs returns the inboxes with the largest backlog, busiest first.
func (db *DB) TopBacklogs(ctx context.Context, limit int) ([]RecipientBacklog, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT to_addr::text, count(*) AS n
		FROM messages
		GROUP BY to_addr
		ORDER BY n DESC, to_addr
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecipientBacklog{}
	for rows.Next() {
		var r RecipientBacklog
		if err := rows.Scan(&r.ToAddr, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InitiatorRow is an initiator with its receiver tally, for the admin list.
type InitiatorRow struct {
	ID             string
	Revoked        bool
	CreatedAt      time.Time
	ReceiverCount  int
	ActiveReceiver int
}

// ListInitiators returns all initiators, newest first, each with its receiver
// counts.
func (db *DB) ListInitiators(ctx context.Context) ([]InitiatorRow, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT i.id::text, i.revoked, i.created_at,
		       count(r.id) AS total,
		       count(r.id) FILTER (WHERE NOT r.revoked) AS active
		FROM initiators i
		LEFT JOIN receivers r ON r.initiator_id = i.id
		GROUP BY i.id
		ORDER BY i.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InitiatorRow{}
	for rows.Next() {
		var r InitiatorRow
		if err := rows.Scan(&r.ID, &r.Revoked, &r.CreatedAt, &r.ReceiverCount, &r.ActiveReceiver); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeInitiator marks an initiator revoked, blocking further token issuance
// for it. Returns ErrNotFound when no such initiator exists.
func (db *DB) RevokeInitiator(ctx context.Context, id string) error {
	tag, err := db.pool.Exec(ctx, `UPDATE initiators SET revoked = true WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeReceiverByID revokes a receiver by id without an ownership check
// (admin context). Returns ErrNotFound when no such receiver exists.
func (db *DB) RevokeReceiverByID(ctx context.Context, id string) error {
	tag, err := db.pool.Exec(ctx, `UPDATE receivers SET revoked = true WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReceiverRow is a receiver with its owning initiator, for the admin list.
type ReceiverRow struct {
	ID          string
	InitiatorID string
	Revoked     bool
	CreatedAt   time.Time
}

// ListAllReceivers returns receivers across every initiator, newest first.
func (db *DB) ListAllReceivers(ctx context.Context, limit int) ([]ReceiverRow, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id::text, initiator_id::text, revoked, created_at
		FROM receivers
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReceiverRow{}
	for rows.Next() {
		var r ReceiverRow
		if err := rows.Scan(&r.ID, &r.InitiatorID, &r.Revoked, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
