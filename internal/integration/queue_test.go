package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/artemavrin/broker/internal/auth"
)

func TestSendFetchAckBothDirections(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iid, _ := e.newInitiator(t)
	rid, _ := e.newReceiver(t, iid)

	initiator := auth.Identity{Subject: iid, Role: auth.RoleInitiator}
	receiver := auth.Identity{Subject: rid, Role: auth.RoleReceiver}

	// initiator -> receiver
	if _, ins, err := e.svc.Send(ctx, initiator, rid, []byte("down"), ""); err != nil || !ins {
		t.Fatalf("send down: ins=%v err=%v", ins, err)
	}
	got, err := e.svc.Fetch(ctx, receiver, 10)
	if err != nil || len(got) != 1 || string(got[0].Payload) != "down" {
		t.Fatalf("receiver fetch: %+v err=%v", got, err)
	}
	if n, err := e.svc.Ack(ctx, receiver, []int64{got[0].ID}); err != nil || n != 1 {
		t.Fatalf("ack: n=%d err=%v", n, err)
	}

	// receiver -> initiator
	if _, ins, err := e.svc.Send(ctx, receiver, iid, []byte("up"), ""); err != nil || !ins {
		t.Fatalf("send up: ins=%v err=%v", ins, err)
	}
	got, err = e.svc.Fetch(ctx, initiator, 10)
	if err != nil || len(got) != 1 || string(got[0].Payload) != "up" {
		t.Fatalf("initiator fetch: %+v err=%v", got, err)
	}
}

func TestIdempotentSend(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iid, _ := e.newInitiator(t)
	rid, _ := e.newReceiver(t, iid)
	initiator := auth.Identity{Subject: iid, Role: auth.RoleInitiator}

	id1, ins1, err := e.svc.Send(ctx, initiator, rid, []byte("x"), "dup-key")
	if err != nil || !ins1 {
		t.Fatalf("first send: ins=%v err=%v", ins1, err)
	}
	id2, ins2, err := e.svc.Send(ctx, initiator, rid, []byte("x"), "dup-key")
	if err != nil {
		t.Fatalf("second send err: %v", err)
	}
	if ins2 {
		t.Fatalf("expected duplicate (inserted=false), got id=%d", id2)
	}
	// Only one row must exist in the receiver's inbox.
	got, _ := e.svc.Fetch(ctx, auth.Identity{Subject: rid, Role: auth.RoleReceiver}, 10)
	if len(got) != 1 || got[0].ID != id1 {
		t.Fatalf("expected exactly one message id=%d, got %+v", id1, got)
	}
}

func TestPolicyRejection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iid, _ := e.newInitiator(t)
	rid, _ := e.newReceiver(t, iid)

	// receiver may only address its own initiator, not itself.
	receiver := auth.Identity{Subject: rid, Role: auth.RoleReceiver}
	if _, _, err := e.svc.Send(ctx, receiver, rid, []byte("x"), ""); err == nil {
		t.Fatalf("expected policy rejection for receiver->self")
	}

	// initiator may only address its own receivers, not an arbitrary id.
	initiator := auth.Identity{Subject: iid, Role: auth.RoleInitiator}
	if _, _, err := e.svc.Send(ctx, initiator, iid, []byte("x"), ""); err == nil {
		t.Fatalf("expected policy rejection for initiator->self")
	}
}

func TestVisibilityReclaim(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iid, _ := e.newInitiator(t)
	rid, _ := e.newReceiver(t, iid)

	// Enqueue one message, then fetch with a short visibility window directly
	// against the db so we can observe reclaim without waiting 30s.
	if _, _, err := e.db.Send(ctx, iid, rid, []byte("reclaim"), "", "new_message"); err != nil {
		t.Fatalf("send: %v", err)
	}
	first, err := e.db.Fetch(ctx, rid, 10, 500*time.Millisecond)
	if err != nil || len(first) != 1 {
		t.Fatalf("first fetch: %+v err=%v", first, err)
	}
	// Immediately re-fetching must yield nothing (still locked).
	mid, _ := e.db.Fetch(ctx, rid, 10, 500*time.Millisecond)
	if len(mid) != 0 {
		t.Fatalf("expected empty while locked, got %+v", mid)
	}
	// After the window lapses the message becomes visible again.
	time.Sleep(700 * time.Millisecond)
	again, err := e.db.Fetch(ctx, rid, 10, 500*time.Millisecond)
	if err != nil || len(again) != 1 {
		t.Fatalf("expected reclaim after timeout, got %+v err=%v", again, err)
	}
	if again[0].ID != first[0].ID {
		t.Fatalf("reclaimed a different message")
	}
}

func TestConcurrentFetchNoDuplicates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iid, _ := e.newInitiator(t)
	rid, _ := e.newReceiver(t, iid)
	initiator := auth.Identity{Subject: iid, Role: auth.RoleInitiator}

	const total = 200
	for i := 0; i < total; i++ {
		if _, _, err := e.svc.Send(ctx, initiator, rid, []byte("m"), ""); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	var (
		mu   sync.Mutex
		seen = make(map[int64]int)
		wg   sync.WaitGroup
	)
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := e.svc.Fetch(ctx, auth.Identity{Subject: rid, Role: auth.RoleReceiver}, 17)
				if err != nil {
					t.Errorf("fetch: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, m := range got {
					seen[m.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("expected %d distinct messages, got %d", total, len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("message %d handed out %d times (SKIP LOCKED violated)", id, count)
		}
	}
}
