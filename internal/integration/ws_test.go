package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/artemavrin/broker/internal/auth"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// wsFrame is a permissive view of any server frame.
type wsFrame struct {
	Type  string `json:"type"`
	Items []struct {
		ID      int64  `json:"id"`
		From    string `json:"from"`
		Payload string `json:"payload"`
	} `json:"items"`
	IDs []int64 `json:"ids"`
	ID  *int64  `json:"id"`
}

func dialWS(t *testing.T, env *testEnv, token string) (*websocket.Conn, context.Context) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(env.server.URL, "http") + "/v1/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	return c, ctx
}

func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) wsFrame {
	t.Helper()
	var f wsFrame
	if err := wsjson.Read(ctx, c, &f); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	return f
}

// TestWSDoorbellAndDrain verifies that a live WS session receives a "new"
// doorbell when a message is enqueued for it, can drain via fetch and ack,
// and that the backlog is delivered on demand.
func TestWSDoorbellAndDrain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iid, _ := e.newInitiator(t)
	rid, rtok := e.newReceiver(t, iid)
	initiator := auth.Identity{Subject: iid, Role: auth.RoleInitiator}

	c, wctx := dialWS(t, e, rtok)

	// Protocol: drain the backlog on connect. The fetch round-trip also proves
	// the session is registered in the hub, so a subsequent send is guaranteed
	// to ring this session's doorbell (no registration race).
	if err := wsjson.Write(wctx, c, map[string]any{"type": "fetch", "max": 10}); err != nil {
		t.Fatalf("write initial fetch: %v", err)
	}
	if f := readFrame(t, wctx, c); f.Type != "messages" || len(f.Items) != 0 {
		t.Fatalf("expected empty backlog, got %+v", f)
	}

	// Enqueue a message for the receiver; the listener should ring the doorbell.
	if _, ins, err := e.svc.Send(ctx, initiator, rid, []byte("ping"), ""); err != nil || !ins {
		t.Fatalf("send: ins=%v err=%v", ins, err)
	}

	if f := readFrame(t, wctx, c); f.Type != "new" {
		t.Fatalf("expected 'new' doorbell, got %q", f.Type)
	}

	// Client responds to the doorbell with a fetch.
	if err := wsjson.Write(wctx, c, map[string]any{"type": "fetch", "max": 10}); err != nil {
		t.Fatalf("write fetch: %v", err)
	}
	f := readFrame(t, wctx, c)
	if f.Type != "messages" || len(f.Items) != 1 || f.Items[0].Payload == "" {
		t.Fatalf("expected one message, got %+v", f)
	}
	gotID := f.Items[0].ID

	// Ack it.
	if err := wsjson.Write(wctx, c, map[string]any{"type": "ack", "ids": []int64{gotID}}); err != nil {
		t.Fatalf("write ack: %v", err)
	}
	if f := readFrame(t, wctx, c); f.Type != "ack_ok" {
		t.Fatalf("expected ack_ok, got %q", f.Type)
	}

	// Nothing left: a fetch returns an empty messages frame.
	if err := wsjson.Write(wctx, c, map[string]any{"type": "fetch", "max": 10}); err != nil {
		t.Fatalf("write fetch2: %v", err)
	}
	if f := readFrame(t, wctx, c); f.Type != "messages" || len(f.Items) != 0 {
		t.Fatalf("expected empty messages, got %+v", f)
	}
}

// TestWSMultiSession verifies every session of a participant gets the doorbell.
func TestWSMultiSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iid, _ := e.newInitiator(t)
	rid, rtok := e.newReceiver(t, iid)
	initiator := auth.Identity{Subject: iid, Role: auth.RoleInitiator}

	c1, ctx1 := dialWS(t, e, rtok)
	c2, ctx2 := dialWS(t, e, rtok)

	// Drain on both sessions; the completed round-trips guarantee both are
	// registered before we send.
	for _, p := range []struct {
		c   *websocket.Conn
		ctx context.Context
	}{{c1, ctx1}, {c2, ctx2}} {
		if err := wsjson.Write(p.ctx, p.c, map[string]any{"type": "fetch", "max": 10}); err != nil {
			t.Fatalf("initial fetch: %v", err)
		}
		if f := readFrame(t, p.ctx, p.c); f.Type != "messages" {
			t.Fatalf("expected messages, got %q", f.Type)
		}
	}

	if _, ins, err := e.svc.Send(ctx, initiator, rid, []byte("fan"), ""); err != nil || !ins {
		t.Fatalf("send: ins=%v err=%v", ins, err)
	}

	if f := readFrame(t, ctx1, c1); f.Type != "new" {
		t.Fatalf("session 1 expected 'new', got %q", f.Type)
	}
	if f := readFrame(t, ctx2, c2); f.Type != "new" {
		t.Fatalf("session 2 expected 'new', got %q", f.Type)
	}
}

// TestWSSendOverSocket verifies a participant can send via the socket and gets
// a "sent" acknowledgement.
func TestWSSendOverSocket(t *testing.T) {
	e := newEnv(t)
	iid, itok := e.newInitiator(t)
	rid, _ := e.newReceiver(t, iid)

	c, ctx := dialWS(t, e, itok)
	if err := wsjson.Write(ctx, c, map[string]any{
		"type": "send", "to": rid, "payload": "aGk=",
	}); err != nil {
		t.Fatalf("write send: %v", err)
	}
	f := readFrame(t, ctx, c)
	if f.Type != "sent" || f.ID == nil {
		t.Fatalf("expected 'sent' with id, got %+v", f)
	}
}
