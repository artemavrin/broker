package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/artemavrin/broker/internal/config"
	"github.com/jackc/pgx/v5"
)

// Проверка площадки перед развёртыванием. Отвечает на вопросы, которые иначе
// выясняются уже в работе: доступна ли база, хватает ли прав на миграции,
// проходит ли LISTEN/NOTIFY через выбранное подключение и какой темп фиксации
// транзакций даёт накопитель. Ничего не настраивает и данных не меняет:
// единственная запись — временная таблица замера, которая удаляется.

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
)

// label возвращает метку одинаковой ширины, чтобы столбец не рвался.
func (s checkStatus) label() string {
	switch s {
	case statusOK:
		return "  ок  "
	case statusWarn:
		return "замеч."
	}
	return " ОТКАЗ"
}

type checkResult struct {
	name   string
	detail string
	status checkStatus
}

// probeRows — сколько однострочных транзакций выполняет замер темпа фиксации.
// Хватает, чтобы отделить локальный накопитель от сетевого тома, и достаточно
// мало, чтобы проверка оставалась быстрой.
const probeRows = 200

func runCheck() error {
	cfg, err := config.LoadPlatform()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	results := platformChecks(ctx, cfg)
	report(results)
	if anyFailed(results) {
		return errors.New("площадка не готова: см. отказы выше")
	}
	return nil
}

func anyFailed(results []checkResult) bool {
	for _, r := range results {
		if r.status == statusFail {
			return true
		}
	}
	return false
}

// platformChecks открывает своё соединение и выполняет весь набор проверок.
// Вынесено отдельно, чтобы setup выполнял ровно те же проверки, а не свой
// похожий набор, который со временем разойдётся.
func platformChecks(ctx context.Context, cfg *config.Platform) []checkResult {
	conn, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return []checkResult{{
			name:   "Подключение к базе",
			detail: err.Error(),
			status: statusFail,
		}}
	}
	defer conn.Close(ctx)

	privileges := checkPrivileges(ctx, conn)
	results := []checkResult{
		checkVersion(ctx, conn),
		privileges,
		checkPgcrypto(ctx, conn),
		checkNotify(ctx, conn, cfg),
		checkDurability(ctx, conn),
	}
	// Замер создаёт таблицу, поэтому без прав он упал бы второй раз по той же
	// причине — дублировать отказ незачем.
	if privileges.status == statusFail {
		results = append(results, checkResult{
			name:   "Темп фиксации транзакций",
			detail: "пропущено: нет прав на создание таблицы замера",
			status: statusWarn,
		})
	} else {
		results = append(results, checkCommitRate(ctx, conn))
	}
	return results
}

func checkVersion(ctx context.Context, conn *pgx.Conn) checkResult {
	res := checkResult{name: "Версия PostgreSQL"}
	var full, num string
	err := conn.QueryRow(ctx,
		"SELECT current_setting('server_version'), current_setting('server_version_num')").
		Scan(&full, &num)
	if err != nil {
		return fail(res, err.Error())
	}
	res.detail = full
	n, _ := strconv.Atoi(num)
	if n < 160000 {
		res.status = statusWarn
		res.detail = full + " — брокер проверялся на 16, работа на более раннем не гарантируется"
	}
	return res
}

func checkPrivileges(ctx context.Context, conn *pgx.Conn) checkResult {
	res := checkResult{name: "Права на миграции"}
	var onDatabase, onSchema bool
	err := conn.QueryRow(ctx, `
		SELECT has_database_privilege(current_user, current_database(), 'CREATE'),
		       has_schema_privilege(current_user, 'public', 'CREATE')`).
		Scan(&onDatabase, &onSchema)
	if err != nil {
		return fail(res, err.Error())
	}
	switch {
	case onDatabase && onSchema:
		res.detail = "CREATE на базу и на схему public — достаточно"
	case !onSchema:
		return fail(res, "нет CREATE на схему public: миграции не создадут таблицы")
	default:
		return fail(res, "нет CREATE на базу: брокер не сможет создать расширение pgcrypto")
	}
	return res
}

func checkPgcrypto(ctx context.Context, conn *pgx.Conn) checkResult {
	res := checkResult{name: "Расширение pgcrypto"}
	var installed, available, trusted bool
	err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pgcrypto'),
		       EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'pgcrypto'),
		       COALESCE((SELECT bool_or(trusted) FROM pg_available_extension_versions
		                 WHERE name = 'pgcrypto'), false)`).
		Scan(&installed, &available, &trusted)
	if err != nil {
		return fail(res, err.Error())
	}
	switch {
	case installed:
		res.detail = "установлено"
	case !available:
		return fail(res, "не установлено и недоступно: нужен пакет contrib для PostgreSQL")
	case trusted:
		res.detail = "доступно и помечено trusted — брокер создаст его сам при первом запуске"
	default:
		res.status = statusWarn
		res.detail = "доступно, но не trusted: расширение должен создать суперпользователь " +
			"командой CREATE EXTENSION pgcrypto"
	}
	return res
}

// checkNotify — главная проверка: через пул соединений в режиме транзакций
// LISTEN/NOTIFY не работает, и доставка молча деградирует до периодического
// опроса. В работе это выглядит как «всё поднялось, но сообщения идут с
// задержкой», поэтому выясняться должно здесь.
func checkNotify(ctx context.Context, listener *pgx.Conn, cfg *config.Platform) checkResult {
	res := checkResult{name: "Механизм LISTEN/NOTIFY"}
	channel := pgx.Identifier{cfg.ListenChannel}.Sanitize()

	if _, err := listener.Exec(ctx, "LISTEN "+channel); err != nil {
		return fail(res, "LISTEN отклонён: "+err.Error())
	}
	defer listener.Exec(ctx, "UNLISTEN "+channel) //nolint:errcheck // соединение всё равно закрывается

	// Уведомление посылаем из отдельного соединения: так же, как это делает
	// брокер, где отправитель и слушатель — разные соединения.
	sender, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(res, "второе подключение не открылось: "+err.Error())
	}
	defer sender.Close(ctx)
	if _, err := sender.Exec(ctx,
		"SELECT pg_notify($1, $2)", cfg.ListenChannel, `{"to":"broker-check"}`); err != nil {
		return fail(res, "pg_notify отклонён: "+err.Error())
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := listener.WaitForNotification(waitCtx)
	if err != nil {
		return fail(res, "уведомление не дошло за 5 с — вероятно, подключение идёт через "+
			"пул соединений в режиме транзакций; подключайте брокер к базе напрямую "+
			"либо переведите пул в режим сессий")
	}
	res.detail = fmt.Sprintf("уведомление по каналу %q доставлено", n.Channel)
	return res
}

func checkDurability(ctx context.Context, conn *pgx.Conn) checkResult {
	res := checkResult{name: "Гарантии сохранности"}
	var fsync, syncCommit string
	err := conn.QueryRow(ctx,
		"SELECT current_setting('fsync'), current_setting('synchronous_commit')").
		Scan(&fsync, &syncCommit)
	if err != nil {
		return fail(res, err.Error())
	}
	res.detail = fmt.Sprintf("fsync=%s, synchronous_commit=%s", fsync, syncCommit)
	if fsync != "on" || (syncCommit != "on" && syncCommit != "remote_apply" && syncCommit != "remote_write") {
		res.status = statusWarn
		res.detail += " — подтверждённые сообщения могут потеряться при отказе питания"
	}
	return res
}

// checkCommitRate измеряет то, во что упирается предел операций в секунду:
// частоту фиксации транзакций. Замер идёт через то же подключение, которым
// будет пользоваться брокер, поэтому в него входит и задержка сети — именно с
// этим брокер и будет работать.
func checkCommitRate(ctx context.Context, conn *pgx.Conn) checkResult {
	res := checkResult{name: "Темп фиксации транзакций"}
	const table = "broker_check_probe"

	if _, err := conn.Exec(ctx,
		"CREATE TABLE IF NOT EXISTS "+table+" (id bigserial PRIMARY KEY, v bytea)"); err != nil {
		return fail(res, "не удалось создать таблицу замера: "+err.Error())
	}
	defer func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn.Exec(dropCtx, "DROP TABLE IF EXISTS "+table) //nolint:errcheck // остаётся пустая таблица, не критично
	}()

	payload := make([]byte, 512) // порядок размера служебного сообщения
	start := time.Now()
	for i := 0; i < probeRows; i++ {
		// Каждый Exec — своя неявная транзакция, то есть одна фиксация.
		if _, err := conn.Exec(ctx, "INSERT INTO "+table+" (v) VALUES ($1)", payload); err != nil {
			return fail(res, "замер не прошёл: "+err.Error())
		}
	}
	rate := float64(probeRows) / time.Since(start).Seconds()

	res.detail = fmt.Sprintf("%.0f в секунду — %s", rate, sizingClass(rate))
	if rate < 200 {
		res.status = statusWarn
		res.detail = fmt.Sprintf("%.0f в секунду — ниже класса S; вероятно, база на сетевом томе",
			rate)
	}
	return res
}

// sizingClass переводит измеренный темп фиксации в класс нагрузки из
// требований к размещению (около семи фиксаций на операцию).
func sizingClass(rate float64) string {
	switch {
	case rate >= 4000:
		return "хватает до класса L (600 операций/с)"
	case rate >= 1000:
		return "хватает до класса M (150 операций/с)"
	case rate >= 200:
		return "хватает до класса S (25 операций/с)"
	}
	return "недостаточно даже для класса S"
}

func fail(res checkResult, detail string) checkResult {
	res.status = statusFail
	res.detail = detail
	return res
}

func report(results []checkResult) {
	// Ширина считается в символах, а не в байтах: в кириллице их по два, и
	// выравнивание через %-*s разъезжается.
	width := 0
	for _, r := range results {
		if n := utf8.RuneCountInString(r.name); n > width {
			width = n
		}
	}
	fmt.Println("Проверка площадки")
	fmt.Println(strings.Repeat("─", 72))
	worst := statusOK
	for _, r := range results {
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(r.name))
		fmt.Printf("[%s] %s%s  %s\n", r.status.label(), r.name, pad, r.detail)
		if r.status > worst {
			worst = r.status
		}
	}
	fmt.Println(strings.Repeat("─", 72))
	switch worst {
	case statusOK:
		fmt.Println("Итог: площадка готова")
	case statusWarn:
		fmt.Println("Итог: площадка пригодна, но есть замечания выше")
	default:
		fmt.Fprintln(os.Stderr, "Итог: площадка не готова")
	}
}
