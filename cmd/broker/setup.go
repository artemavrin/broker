package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/artemavrin/broker/internal/config"
	"github.com/artemavrin/broker/internal/db"
	"github.com/artemavrin/broker/internal/secret"
)

// Первичная настройка: проверяет площадку, заводит секреты, применяет миграции
// и создаёт первого инициатора. Команда неинтерактивная и идемпотентная, чтобы
// её можно было вызывать и руками, и из Ansible: существующие секреты не
// перегенерируются (это обнулило бы все выданные токены), а инициатор
// создаётся только когда в базе нет ни одного.
//
// Что команда сознательно НЕ делает: не создаёт базу и роль — для этого нужны
// права суперпользователя, отдавать которые установщику незачем. Три строки
// SQL выполняет администратор базы данных, а setup проверяет результат.

// secretVars — переменные, которые setup генерирует, если их ещё нет.
var secretVars = []string{"JWT_SIGNING_KEY", "ADMIN_TOKEN"}

func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	envFile := fs.String("env-file", "broker.env",
		"файл с переменными окружения; дополняется, существующие значения сохраняются")
	noInitiator := fs.Bool("no-initiator", false,
		"не создавать первого инициатора")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadPlatform()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	fmt.Println("Шаг 1 из 4. Проверка площадки")
	results := platformChecks(ctx, cfg)
	report(results)
	if anyFailed(results) {
		return errors.New("настройка прервана: площадка не готова, см. отказы выше")
	}

	fmt.Printf("\nШаг 2 из 4. Секреты\n")
	if err := writeEnvFile(*envFile, cfg.DatabaseURL); err != nil {
		return err
	}

	fmt.Printf("\nШаг 3 из 4. Схема базы данных\n")
	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("подключение к базе: %w", err)
	}
	defer database.Close()
	if err := database.Migrate(ctx, ""); err != nil {
		return fmt.Errorf("применение миграций: %w", err)
	}
	fmt.Println("  миграции применены (повторный запуск ничего не меняет)")

	fmt.Printf("\nШаг 4 из 4. Первый инициатор\n")
	if err := ensureInitiator(ctx, database, *noInitiator); err != nil {
		return err
	}

	printRemaining(*envFile)
	return nil
}

// writeEnvFile дополняет файл переменных: DATABASE_URL берётся из окружения,
// секреты генерируются только если их ещё нет ни в файле, ни в окружении.
func writeEnvFile(path, databaseURL string) error {
	existing, err := readEnvFile(path)
	if err != nil {
		return err
	}

	values := map[string]string{}
	for k, v := range existing {
		values[k] = v
	}
	values["DATABASE_URL"] = databaseURL

	for _, name := range secretVars {
		switch {
		case values[name] != "":
			fmt.Printf("  %s: уже задан в %s, оставлен без изменений\n", name, path)
		case os.Getenv(name) != "":
			values[name] = os.Getenv(name)
			fmt.Printf("  %s: взят из окружения\n", name)
		default:
			generated, err := secret.Generate()
			if err != nil {
				return fmt.Errorf("генерация %s: %w", name, err)
			}
			values[name] = generated
			fmt.Printf("  %s: сгенерирован\n", name)
		}
	}

	if err := writeAtomic(path, renderEnv(values)); err != nil {
		return err
	}
	fmt.Printf("  записано в %s с правами 0600\n", path)
	return nil
}

func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("чтение %s: %w", path, err)
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values, scanner.Err()
}

func renderEnv(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# Переменные окружения брокера. Файл содержит секреты:\n")
	b.WriteString("# храните права 0600 и не добавляйте его в систему контроля версий.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, values[k])
	}
	return b.String()
}

// writeAtomic пишет через временный файл в том же каталоге и переименование,
// чтобы прерванный запуск не оставил файл секретов обрезанным.
func writeAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".broker-env-*")
	if err != nil {
		return fmt.Errorf("создание временного файла в %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("права на временный файл: %w", err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("запись: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("закрытие: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}

func ensureInitiator(ctx context.Context, database *db.DB, skip bool) error {
	existing, err := database.ListInitiators(ctx)
	if err != nil {
		return fmt.Errorf("список инициаторов: %w", err)
	}
	if len(existing) > 0 {
		fmt.Printf("  в базе уже %d инициатор(ов), новый не создавался\n", len(existing))
		return nil
	}
	if skip {
		fmt.Println("  пропущено по --no-initiator")
		return nil
	}

	raw, err := secret.Generate()
	if err != nil {
		return fmt.Errorf("генерация секрета: %w", err)
	}
	id, err := database.CreateInitiator(ctx, secret.Hash(raw))
	if err != nil {
		return fmt.Errorf("создание инициатора: %w", err)
	}
	fmt.Println("  создан инициатор — секрет показывается один раз и не восстанавливается:")
	fmt.Printf("    initiator_id:     %s\n", id)
	fmt.Printf("    initiator_secret: %s\n", raw)
	return nil
}

func printRemaining(envFile string) {
	fmt.Printf(`
Готово. Осталось сделать вне брокера:

  1. Запустить сервис с этим файлом переменных, например в systemd:
     EnvironmentFile=%s
  2. Настроить обратный прокси: сертификат TLS, проксирование /v1/ и /admin/,
     переход на WebSocket и таймаут простоя больше 20 секунд.
  3. Проверить готовность: curl -fsS https://<домен>/healthz
`, envFile)
}
