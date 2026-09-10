// Command broker is the message broker server. Subcommands cover the operator
// side: check inspects the platform, setup performs first-time configuration,
// create-initiator provisions a participant, version prints the build.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
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

// version подставляется при сборке релиза: -ldflags "-X main.version=v1.2.3".
// Без неё по работающему сервису не понять, какая сборка развёрнута.
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// Подкоманды печатают результат человеку, поэтому об ошибке сообщают
	// строкой в stderr, а не структурным логом, предназначенным для сбора.
	if len(os.Args) > 1 {
		switch cmd := os.Args[1]; cmd {
		case "create-initiator":
			exitOnError(runCreateInitiator())
		case "check":
			exitOnError(runCheck())
		case "setup":
			exitOnError(runSetup(os.Args[2:]))
		case "version":
			fmt.Println(version)
		default:
			fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n%s", cmd, usage)
			os.Exit(2)
		}
		return
	}

	if err := runServer(log); err != nil {
		log.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

const usage = `Использование:
  broker                    запустить сервер
  broker check              проверить площадку: база, права, LISTEN/NOTIFY, темп фиксации
  broker setup [флаги]      первичная настройка: проверка, секреты, миграции, инициатор
  broker create-initiator   создать инициатора и напечатать его секрет
  broker version            напечатать версию сборки

Флаги setup:
  --env-file PATH           файл переменных окружения (по умолчанию broker.env)
  --no-initiator            не создавать первого инициатора
`

func exitOnError(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runServer(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// The configured level is only known once the config is loaded, so the
	// bootstrap logger above is replaced here.
	log = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	// Первой строкой — версия: по логам развёрнутого сервиса должно быть видно,
	// какая сборка работает.
	log.Info("broker starting", "version", version)

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

	// Optional pprof endpoint on a separate, private listener. Enabling it also
	// turns on mutex/block profiling so contention shows up in the profiles.
	if cfg.PprofAddr != "" {
		runtime.SetMutexProfileFraction(5)
		runtime.SetBlockProfileRate(1_000_000) // sample blocking events ~every 1ms
		pmux := http.NewServeMux()
		pmux.HandleFunc("/debug/pprof/", pprof.Index)
		pmux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pmux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pmux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pmux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		psrv := &http.Server{Addr: cfg.PprofAddr, Handler: pmux}
		go func() {
			log.Info("pprof listening", "addr", cfg.PprofAddr)
			if err := psrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("pprof server stopped", "err", err)
			}
		}()
		defer psrv.Close()
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
