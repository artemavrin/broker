// Command broker is the message broker server. With the "create-initiator"
// subcommand it instead provisions a new initiator and prints its secret.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/artemavrin/broker/internal/admin"
	"github.com/artemavrin/broker/internal/auth"
	"github.com/artemavrin/broker/internal/config"
	"github.com/artemavrin/broker/internal/core"
	"github.com/artemavrin/broker/internal/db"
	"github.com/artemavrin/broker/internal/httpapi"
	"github.com/artemavrin/broker/internal/notify"
	"github.com/artemavrin/broker/internal/wsapi"
	"github.com/artemavrin/broker/internal/wshub"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if len(os.Args) > 1 && os.Args[1] == "create-initiator" {
		if err := runCreateInitiator(); err != nil {
			log.Error("create-initiator failed", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := runServer(log); err != nil {
		log.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

func runServer(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The signal context governs the whole process lifecycle.
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	connectCtx, cancel := context.WithTimeout(rootCtx, 10*time.Second)
	defer cancel()
	database, err := db.Connect(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()

	if err := database.Migrate(rootCtx, cfg.MigrationsDir); err != nil {
		return err
	}
	log.Info("migrations applied")

	signer := auth.NewSigner(cfg.JWTSigningKey, cfg.AccessTTL)
	hub := wshub.New()
	svc := core.New(database, cfg.MaxFetch, cfg.MaxPayloadBytes, cfg.VisibilityTimeout, cfg.ListenChannel)
	ws := wsapi.New(svc, signer, hub, log)
	api := httpapi.New(svc, signer, ws, cfg.AuthRatePerMin, log)

	// The admin dashboard is mounted only when an ADMIN_TOKEN is configured.
	handler := http.Handler(api.Routes())
	if cfg.AdminToken != "" {
		const adminSessionTTL = 12 * time.Hour
		dash := admin.New(svc, signer, hub, cfg.AdminToken, adminSessionTTL, log)
		root := http.NewServeMux()
		root.Handle("/admin/", dash.Routes())
		root.Handle("/", api.Routes())
		handler = root
		log.Info("admin dashboard enabled", "path", "/admin/")
	}

	// Dedicated listener connection (never from the pool) drives the doorbell.
	listener := notify.New(database.URL(), cfg.ListenChannel, hub, log)
	listenerCtx, listenerCancel := context.WithCancel(context.Background())
	defer listenerCancel()
	go listener.Run(listenerCtx)

	// requestCtx is handed to every HTTP/WS request; cancelling it on shutdown
	// unblocks long-lived WS handlers so Shutdown can drain them.
	requestCtx, requestCancel := context.WithCancel(context.Background())
	defer requestCancel()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-rootCtx.Done():
		log.Info("shutdown signal received")
	case err := <-serveErr:
		return err
	}

	// Graceful shutdown: cancel WS handlers, stop the listener, drain HTTP,
	// then the deferred db.Close drains the pool.
	requestCancel()
	listenerCancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out", "err", err)
	}
	log.Info("shutdown complete")
	return nil
}
