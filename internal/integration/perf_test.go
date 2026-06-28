package integration

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/artemavrin/broker/internal/auth"
)

// BenchmarkSend measures the cost of one policy-checked, idempotent enqueue
// (including the pg_notify doorbell) on the hot path.
func BenchmarkSend(b *testing.B) {
	e := newEnv(b)
	ctx := context.Background()
	iid, _ := e.newInitiator(b)
	rid, _ := e.newReceiver(b, iid)
	initiator := auth.Identity{Subject: iid, Role: auth.RoleInitiator}
	payload := []byte("benchmark-payload")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := e.svc.Send(ctx, initiator, rid, payload, ""); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSendParallel measures enqueue throughput under concurrency, which
// exercises the connection pool and contention.
func BenchmarkSendParallel(b *testing.B) {
	e := newEnv(b)
	ctx := context.Background()
	iid, _ := e.newInitiator(b)
	rid, _ := e.newReceiver(b, iid)
	initiator := auth.Identity{Subject: iid, Role: auth.RoleInitiator}
	payload := []byte("benchmark-payload")

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, _, err := e.svc.Send(ctx, initiator, rid, payload, ""); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkFetchAck measures the drain cycle: claim a batch (SKIP LOCKED +
// visibility lock) then ack-delete it. The inbox is pre-seeded so the loop is
// never starved.
func BenchmarkFetchAck(b *testing.B) {
	e := newEnv(b)
	ctx := context.Background()
	iid, _ := e.newInitiator(b)
	rid, _ := e.newReceiver(b, iid)
	initiator := auth.Identity{Subject: iid, Role: auth.RoleInitiator}
	receiver := auth.Identity{Subject: rid, Role: auth.RoleReceiver}

	const batch = 50
	seed := b.N*batch + batch
	for i := 0; i < seed; i++ {
		if _, _, err := e.svc.Send(ctx, initiator, rid, []byte("m"), ""); err != nil {
			b.Fatal(err)
		}
	}

	var drained int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msgs, err := e.svc.Fetch(ctx, receiver, batch)
		if err != nil {
			b.Fatal(err)
		}
		if len(msgs) == 0 {
			continue
		}
		ids := make([]int64, len(msgs))
		for j, m := range msgs {
			ids[j] = m.ID
		}
		n, err := e.svc.Ack(ctx, receiver, ids)
		if err != nil {
			b.Fatal(err)
		}
		atomic.AddInt64(&drained, n)
	}
	b.StopTimer()
	b.ReportMetric(float64(drained)/float64(b.N), "msgs/op")
}
