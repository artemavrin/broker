//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
)

// Под Windows брокер работает службой без обёрток вроде NSSM. Диспетчер служб
// (SCM) запускает процесс с аргументами из binPath, а остановку присылает
// командой, а не сигналом. Переменные служба берёт из файла, который пишет
// setup: передать их через SCM можно только через реестр, а ветку служб
// читает любой локальный пользователь.

// runIfService запускает брокер как службу, если процесс стартовал из SCM.
// handled=false значит обычный запуск из консоли.
func runIfService() (handled bool, err error) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, err
	}
	return true, runService(os.Args[1:])
}

func runService(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)

	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	envFile := fs.String("env-file", filepath.Join(dir, "broker.env"), "файл переменных окружения")
	logFile := fs.String("log-file", filepath.Join(dir, "broker.log"), "файл журнала")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// У службы нет консоли: всё, что пишется в stderr, уходит в файл журнала.
	f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("файл журнала: %w", err)
	}
	defer f.Close()
	os.Stderr = f
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := applyEnvFile(*envFile); err != nil {
		log.Error("service start failed", "err", err)
		return err
	}
	return svc.Run("broker", brokerService{log: log})
}

type brokerService struct {
	log *slog.Logger
}

// Execute — цикл обработки команд SCM. Ненулевой код выхода нужен, чтобы
// сработали действия восстановления службы (перезапуск при сбое).
func (s brokerService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, s.log) }()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			if err != nil {
				slog.Error("server exited with error", "err", err)
				return false, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// Корректная остановка в runServer занимает до 15 секунд.
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32((20 * time.Second).Milliseconds())}
				cancel()
				if err := <-done; err != nil {
					slog.Error("server exited with error", "err", err)
				}
				return false, 0
			}
		}
	}
}
