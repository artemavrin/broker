package integration

import (
	"bytes"
	crand "crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// clientReadLimit is what a WS client must allow to receive a messages frame
// carrying a payload near the configured limit: base64 inflates by ~4/3 and
// JSON framing adds more on top.
const clientReadLimit = 8 << 20

// randomPayload returns n incompressible bytes, so nothing along the way can
// quietly shrink the frame being tested.
func randomPayload(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// TestWSSendAtPayloadLimit covers the broker's own read limit. The websocket
// library defaults to 32 KiB per message, far below MAX_PAYLOAD_BYTES, so
// without an explicit limit a send frame the REST path accepts would instead
// tear the connection down.
func TestWSSendAtPayloadLimit(t *testing.T) {
	env := newEnv(t)
	iid, itok := env.newInitiator(t)
	rid, _ := env.newReceiver(t, iid)

	payload := randomPayload(t, env.svc.MaxPayload())
	c, ctx := dialWS(t, env, itok)
	c.SetReadLimit(clientReadLimit)

	if err := wsjson.Write(ctx, c, map[string]any{
		"type":    "send",
		"to":      rid,
		"payload": base64.StdEncoding.EncodeToString(payload),
	}); err != nil {
		t.Fatalf("write send frame: %v", err)
	}
	if f := readFrame(t, ctx, c); f.Type != "sent" {
		t.Fatalf("frame type = %q, want \"sent\"", f.Type)
	}
}

// TestWSFetchAtPayloadLimit takes the same message back out over WS and checks
// it arrives byte for byte.
func TestWSFetchAtPayloadLimit(t *testing.T) {
	env := newEnv(t)
	iid, itok := env.newInitiator(t)
	rid, rtok := env.newReceiver(t, iid)

	payload := randomPayload(t, env.svc.MaxPayload())
	ic, ictx := dialWS(t, env, itok)
	ic.SetReadLimit(clientReadLimit)
	if err := wsjson.Write(ictx, ic, map[string]any{
		"type":    "send",
		"to":      rid,
		"payload": base64.StdEncoding.EncodeToString(payload),
	}); err != nil {
		t.Fatalf("write send frame: %v", err)
	}
	if f := readFrame(t, ictx, ic); f.Type != "sent" {
		t.Fatalf("send frame type = %q, want \"sent\"", f.Type)
	}

	rc, rctx := dialWS(t, env, rtok)
	rc.SetReadLimit(clientReadLimit)
	if err := wsjson.Write(rctx, rc, map[string]any{"type": "fetch", "max": 1}); err != nil {
		t.Fatalf("write fetch frame: %v", err)
	}
	f := readFrame(t, rctx, rc)
	if f.Type != "messages" {
		t.Fatalf("frame type = %q, want \"messages\"", f.Type)
	}
	if len(f.Items) != 1 {
		t.Fatalf("got %d messages, want 1", len(f.Items))
	}
	got, err := base64.StdEncoding.DecodeString(f.Items[0].Payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload round-trip mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestWSClientMustRaiseItsOwnReadLimit pins the half the broker cannot fix. The
// server writes the messages frame regardless of the client's limit, so a
// client left at the library default simply loses the connection on a large
// message. Client implementations must raise their limit (or fetch with a
// smaller max), and this test exists so that contract is not forgotten.
func TestWSClientMustRaiseItsOwnReadLimit(t *testing.T) {
	env := newEnv(t)
	iid, itok := env.newInitiator(t)
	rid, rtok := env.newReceiver(t, iid)

	payload := randomPayload(t, env.svc.MaxPayload())
	ic, ictx := dialWS(t, env, itok)
	ic.SetReadLimit(clientReadLimit)
	if err := wsjson.Write(ictx, ic, map[string]any{
		"type":    "send",
		"to":      rid,
		"payload": base64.StdEncoding.EncodeToString(payload),
	}); err != nil {
		t.Fatalf("write send frame: %v", err)
	}
	if f := readFrame(t, ictx, ic); f.Type != "sent" {
		t.Fatalf("send frame type = %q, want \"sent\"", f.Type)
	}

	// Deliberately left at the library default.
	rc, rctx := dialWS(t, env, rtok)
	if err := wsjson.Write(rctx, rc, map[string]any{"type": "fetch", "max": 1}); err != nil {
		t.Fatalf("write fetch frame: %v", err)
	}
	var f wsFrame
	err := wsjson.Read(rctx, rc, &f)
	if err == nil {
		t.Fatal("expected the read to fail at the default limit; if the library " +
			"default changed, update clientReadLimit guidance in the README")
	}
	if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
		t.Fatalf("expected a size-related failure, got a normal closure: %v", err)
	}
}
