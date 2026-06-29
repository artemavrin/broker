// Command shardproto is a prototype that shards the broker's message queue
// across N independent PostgreSQL instances, keyed by recipient (to_addr).
//
// Rationale: in the star topology every message is between an initiator and one
// of its receivers, so a message's to_addr and from_addr always belong to the
// same tenant. Routing by the recipient therefore keeps each tenant's whole
// queue — send, fetch/ack and the LISTEN/NOTIFY doorbell — on one shard, so no
// statement ever spans shards. shard(to_addr) = fnv32a(to_addr) % N.
//
// This is a measurement prototype, not wired into the broker: it runs the real
// send (single-statement INSERT + pg_notify), fetch (FOR UPDATE SKIP LOCKED)
// and ack (DELETE) against each shard and reports aggregate throughput, so the
// scaling of independent WAL/commit pipelines can be measured directly.
//
// Usage:
//
//	shardproto -shards "postgres://...:5432/broker,postgres://...:5433/broker" \
//	  -senders 24 -recv 4 -duration 10s
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	shardsCSV := flag.String("shards", "", "comma-separated shard DSNs (required)")
	senders := flag.Int("senders", 24, "concurrent producer goroutines")
	recvPerShard := flag.Int("recv", 4, "receiver inboxes per shard")
	duration := flag.Duration("duration", 10*time.Second, "test duration")
	payload := flag.Int("payload", 256, "payload bytes (min 8)")
	batch := flag.Int("batch", 32, "consumer fetch batch")
	flag.Parse()
	if *shardsCSV == "" {
		log.Fatal("-shards is required")
	}
	if *payload < 8 {
		*payload = 8
	}
	if err := run(strings.Split(*shardsCSV, ","), *senders, *recvPerShard, *duration, *payload, *batch); err != nil {
		log.Fatal(err)
	}
}

func run(dsns []string, senders, recvPerShard int, duration time.Duration, payload, batch int) error {
	ctx := context.Background()
	n := len(dsns)

	// Connect a pool per shard and ensure the schema exists.
	shards := make([]*pgxpool.Pool, n)
	for i, dsn := range dsns {
		p, err := pgxpool.New(ctx, strings.TrimSpace(dsn))
		if err != nil {
			return fmt.Errorf("shard %d connect: %w", i, err)
		}
		defer p.Close()
		if err := ensureSchema(ctx, p); err != nil {
			return fmt.Errorf("shard %d schema: %w", i, err)
		}
		if _, err := p.Exec(ctx, "TRUNCATE messages RESTART IDENTITY"); err != nil {
			return fmt.Errorf("shard %d truncate: %w", i, err)
		}
		shards[i] = p
	}

	// Provision receivers, bucketed so each shard owns exactly recvPerShard of
	// them under the routing hash. This proves routing and balance line up.
	buckets := make([][]string, n)
	need := recvPerShard * n
	for got := 0; got < need; {
		id := newUUID()
		s := shardFor(id, n)
		if len(buckets[s]) < recvPerShard {
			buckets[s] = append(buckets[s], id)
			got++
		}
	}
	var allRecv []string
	for _, b := range buckets {
		allRecv = append(allRecv, b...)
	}
	from := newUUID() // a single synthetic initiator
	fmt.Printf("shards=%d receivers=%d senders=%d payload=%dB duration=%s\n",
		n, len(allRecv), senders, payload, duration)

	deadline, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	m := &metrics{}
	var wg sync.WaitGroup

	// Consumers: one per receiver, draining its own inbox on its own shard.
	for s := 0; s < n; s++ {
		for _, to := range buckets[s] {
			wg.Add(1)
			go func(pool *pgxpool.Pool, to string) {
				defer wg.Done()
				consume(deadline, pool, to, batch, m)
			}(shards[s], to)
		}
	}

	// Producers: round-robin receivers, route each to its shard.
	start := time.Now()
	for g := 0; g < senders; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			buf := make([]byte, payload)
			idx := seed
			for deadline.Err() == nil {
				to := allRecv[idx%len(allRecv)]
				idx++
				binary.BigEndian.PutUint64(buf, uint64(time.Now().UnixNano()))
				pool := shards[shardFor(to, n)]
				t0 := time.Now()
				if err := send(deadline, pool, from, to, buf); err != nil {
					m.sendErr.Add(1)
					continue
				}
				m.sent.Add(1)
				m.sendLat.add(time.Since(t0))
			}
		}(g)
	}

	wg.Wait()
	elapsed := time.Since(start)
	m.report(elapsed, n)
	// Per-shard distribution check: max(id) ≈ total rows ever inserted on the
	// shard (ids are gapless from RESTART IDENTITY; acked rows are deleted).
	fmt.Printf("per-shard inserted: ")
	for i, p := range shards {
		var ins int64
		_ = p.QueryRow(ctx, "SELECT coalesce(max(id),0) FROM messages").Scan(&ins)
		fmt.Printf("shard%d=%d ", i, ins)
	}
	fmt.Println()
	return nil
}

// ---- SQL (faithful to the broker's hot path) ----

func ensureSchema(ctx context.Context, p *pgxpool.Pool) error {
	_, err := p.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS messages (
			id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			from_addr     UUID NOT NULL,
			to_addr       UUID NOT NULL,
			payload       BYTEA NOT NULL,
			client_msg_id TEXT,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
			locked_until  TIMESTAMPTZ,
			attempts      INT NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS messages_inbox_idx ON messages (to_addr, id);`)
	return err
}

func send(ctx context.Context, p *pgxpool.Pool, from, to string, payload []byte) error {
	// Single statement: insert + doorbell, one round-trip (no policy EXISTS here
	// — receivers live on the same shard in the real broker; omitted in the
	// prototype as it is a read and does not affect write scaling).
	_, err := p.Exec(ctx, `
		WITH ins AS (
			INSERT INTO messages (from_addr, to_addr, payload)
			VALUES ($1::uuid, $2::uuid, $3::bytea)
			RETURNING to_addr
		)
		SELECT pg_notify('new_message', json_build_object('to', to_addr)::text) FROM ins`,
		from, to, payload)
	return err
}

func consume(ctx context.Context, p *pgxpool.Pool, to string, batch int, m *metrics) {
	for ctx.Err() == nil {
		rows, err := p.Query(ctx, `
			UPDATE messages
			SET locked_until = now() + interval '30 seconds', attempts = attempts + 1
			WHERE id IN (
				SELECT id FROM messages
				WHERE to_addr = $1::uuid AND (locked_until IS NULL OR locked_until < now())
				ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED
			)
			RETURNING id, payload`, to, batch)
		if err != nil {
			m.fetchErr.Add(1)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var ids []int64
		for rows.Next() {
			var id int64
			var pl []byte
			if err := rows.Scan(&id, &pl); err != nil {
				break
			}
			ids = append(ids, id)
			if len(pl) >= 8 {
				ts := int64(binary.BigEndian.Uint64(pl[:8]))
				m.e2eLat.add(time.Since(time.Unix(0, ts)))
			}
		}
		rows.Close()
		if len(ids) == 0 {
			time.Sleep(3 * time.Millisecond)
			continue
		}
		m.delivered.Add(int64(len(ids)))
		if _, err := p.Exec(ctx, "DELETE FROM messages WHERE id = ANY($1::bigint[]) AND to_addr = $2::uuid", ids, to); err != nil {
			m.ackErr.Add(1)
			continue
		}
		m.acked.Add(int64(len(ids)))
	}
}

// ---- routing ----

func shardFor(id string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % uint32(n))
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ---- metrics ----

type metrics struct {
	sent, delivered, acked    atomic.Int64
	sendErr, fetchErr, ackErr atomic.Int64
	sendLat, e2eLat           latHist
}

func (m *metrics) report(elapsed time.Duration, shards int) {
	s := elapsed.Seconds()
	fmt.Printf("\n=========== shardproto report (%d shard%s) ===========\n", shards, plural(shards))
	fmt.Printf("duration:   %.1fs\n", s)
	fmt.Printf("sent:       %d  (%.0f/s)\n", m.sent.Load(), float64(m.sent.Load())/s)
	fmt.Printf("delivered:  %d  (%.0f/s)\n", m.delivered.Load(), float64(m.delivered.Load())/s)
	fmt.Printf("acked:      %d  (%.0f/s)\n", m.acked.Load(), float64(m.acked.Load())/s)
	fmt.Printf("errors:     send=%d fetch=%d ack=%d\n", m.sendErr.Load(), m.fetchErr.Load(), m.ackErr.Load())
	fmt.Printf("send lat:   %s\n", m.sendLat.summary())
	fmt.Printf("e2e lat:    %s\n", m.e2eLat.summary())
	fmt.Printf("=====================================================\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

type latHist struct {
	mu sync.Mutex
	s  []time.Duration
}

func (h *latHist) add(d time.Duration) {
	h.mu.Lock()
	h.s = append(h.s, d)
	h.mu.Unlock()
}

func (h *latHist) summary() string {
	h.mu.Lock()
	s := h.s
	h.mu.Unlock()
	if len(s) == 0 {
		return "no samples"
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	p := func(q float64) time.Duration {
		i := int(float64(len(s))*q) - 1
		if i < 0 {
			i = 0
		}
		return s[i].Round(time.Microsecond)
	}
	return fmt.Sprintf("n=%d p50=%s p95=%s p99=%s", len(s), p(0.50), p(0.95), p(0.99))
}
