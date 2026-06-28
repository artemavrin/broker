package integration

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/artemavrin/broker/internal/auth"
	"github.com/artemavrin/broker/internal/core"
	"github.com/artemavrin/broker/internal/db"
	"github.com/artemavrin/broker/internal/httpapi"
	"github.com/artemavrin/broker/internal/notify"
	"github.com/artemavrin/broker/internal/secret"
	"github.com/artemavrin/broker/internal/wsapi"
	"github.com/artemavrin/broker/internal/wshub"
)

// testEnv bundles everything an integration test needs.
type testEnv struct {
	db     *db.DB
	svc    *core.Service
	signer *auth.Signer
	hub    *wshub.Hub
	server *httptest.Server
	url    string // database url
}

// dbURL returns the test database URL or skips the test if none is configured.
func dbURL(t *testing.T) string {
	t.Helper()
	for _, k := range []string{"BROKER_TEST_DATABASE_URL", "DATABASE_URL"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	t.Skip("set BROKER_TEST_DATABASE_URL (or DATABASE_URL) to run integration tests")
	return ""
}

// newEnv connects to the database, applies migrations, truncates state, and
// wires the full stack (core service, HTTP API, WS handler and a live LISTEN
// listener) behind an httptest server.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	url := dbURL(t)
	ctx := context.Background()

	database, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(database.Close)

	migDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("abs migrations: %v", err)
	}
	if err := database.Migrate(ctx, migDir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := database.Pool().Exec(ctx,
		`TRUNCATE messages, receivers, initiators RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	signer := auth.NewSigner([]byte("0123456789abcdef0123456789abcdef-itest"), 30*time.Minute)
	hub := wshub.New()
	svc := core.New(database, 100, 262144, 30*time.Second, "new_message")
	ws := wsapi.New(svc, signer, hub, log)
	api := httpapi.New(svc, signer, ws, 0, log)

	listenerCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan struct{})
	listener := notify.New(url, "new_message", hub, log)
	var once sync.Once
	listener.OnListen = func() { once.Do(func() { close(ready) }) }
	go listener.Run(listenerCtx)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not become ready")
	}

	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	return &testEnv{db: database, svc: svc, signer: signer, hub: hub, server: srv, url: url}
}

// newInitiator creates an initiator and returns its id plus an issued token.
func (e *testEnv) newInitiator(t *testing.T) (id, token string) {
	t.Helper()
	raw, err := secret.Generate()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	id, err = e.db.CreateInitiator(context.Background(), secret.Hash(raw))
	if err != nil {
		t.Fatalf("create initiator: %v", err)
	}
	token, _, err = e.signer.Issue(id, auth.RoleInitiator)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return id, token
}

// newReceiver creates a receiver under initiatorID and returns its id and token.
func (e *testEnv) newReceiver(t *testing.T, initiatorID string) (id, token string) {
	t.Helper()
	raw, err := secret.Generate()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	id, err = e.db.CreateReceiver(context.Background(), initiatorID, secret.Hash(raw))
	if err != nil {
		t.Fatalf("create receiver: %v", err)
	}
	token, _, err = e.signer.Issue(id, auth.RoleReceiver)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return id, token
}
