// Package notify bridges PostgreSQL LISTEN/NOTIFY to the WebSocket hub. It
// holds its own dedicated connection (never from the pool) and rings the
// doorbell for the recipient named in each notification, reconnecting with
// backoff on failure.
package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/artemavrin/broker/internal/wshub"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// payload is the JSON body the sender attaches to pg_notify.
type payload struct {
	To string `json:"to"`
}

// Listener consumes notifications on a single channel and forwards them to the
// hub.
type Listener struct {
	url     string
	channel string
	hub     *wshub.Hub
	log     *slog.Logger

	// OnListen, if set, is invoked after each successful LISTEN (including
	// after a reconnect). It lets callers observe readiness; it must not block.
	OnListen func()
}

// New constructs a Listener.
func New(url, channel string, hub *wshub.Hub, log *slog.Logger) *Listener {
	return &Listener{url: url, channel: channel, hub: hub, log: log}
}

// Run blocks until ctx is cancelled, maintaining a LISTEN on the channel and
// reconnecting with capped exponential backoff whenever the connection drops.
func (l *Listener) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := l.listen(ctx); err != nil && ctx.Err() == nil {
			l.log.Warn("listener connection lost, will reconnect",
				"err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		// Clean exit (ctx cancelled) or reset backoff after a healthy run.
		backoff = time.Second
		if ctx.Err() != nil {
			return
		}
	}
}

// listen establishes one connection, issues LISTEN, and pumps notifications
// until the context is cancelled or the connection errors.
func (l *Listener) listen(ctx context.Context) error {
	// Parse with pgxpool's parser so pool-only DSN parameters (pool_max_conns,
	// pool_min_conns, …) are stripped: a single pgx.Connect would otherwise
	// forward them to the server as runtime parameters and fail with
	// "unrecognized configuration parameter", silently killing the doorbell.
	cfg, err := pgxpool.ParseConfig(l.url)
	if err != nil {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{l.channel}.Sanitize()); err != nil {
		return err
	}
	l.log.Info("listening for notifications", "channel", l.channel)
	if l.OnListen != nil {
		l.OnListen()
	}

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var p payload
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			l.log.Warn("bad notification payload", "payload", n.Payload, "err", err)
			continue
		}
		if p.To != "" {
			l.hub.Notify(p.To)
		}
	}
}
