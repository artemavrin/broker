# Брокер сообщений

Сервис-брокер маршрутизирует сообщения между **инициатором** (в закрытом
контуре) и его **приёмниками** (в открытом интернете). Обе стороны
дозваниваются до брокера наружу — брокер сам соединений не инициирует.
Топология строго «звезда»: инициатор ↔ его приёмники; mesh между приёмниками
запрещён политикой.

- Адресация `from → to`; `from` всегда берётся из JWT, никогда из тела запроса.
- Очередь на PostgreSQL с `ack` (физическое удаление) и visibility timeout.
- Транспорт: REST (HTTP-polling) и WebSocket. WS — лишь «дверной звонок»
  поверх той же очереди: данные всегда читаются из БД.
- `payload` для брокера непрозрачен (`bytea`), на границе API — base64.

## Стек

Go 1.25 · PostgreSQL 16 (`LISTEN/NOTIFY`) · `pgx/v5` · `coder/websocket` ·
`golang-jwt/v5` (HS256) · миграции через `goose` · логи `log/slog`.

## Архитектура

```
initiator ─┐                       ┌─ pgxpool ──► PostgreSQL
           ├──HTTP / WS──► broker ──┤
receivers ─┘                       └─ listener (LISTEN new_message) ──► wshub
```

- `internal/core` — транспортно-независимое ядро: политика адресации, лимиты,
  идемпотентность. И HTTP, и WS вызывают одни и те же методы.
- `internal/db` — единственный пул и весь SQL (initiators / receivers /
  messages). Забор очереди — `FOR UPDATE SKIP LOCKED`.
- `internal/notify` — отдельное долгоживущее соединение (не из пула) под
  `LISTEN`, реконнект с backoff; на уведомление звонит в `wshub`.
- `internal/wshub` — реестр WS-сессий по `participant id`, мультисессии.
- `internal/httpapi`, `internal/wsapi` — фронтенды.

## Быстрый старт

### Через docker-compose

```bash
docker compose up --build
```

Поднимется Postgres и брокер на `:8080`. Брокер сам применит миграции при
старте.

### Локально

```bash
# 1. Postgres (только база)
docker compose up -d postgres

export DATABASE_URL="postgres://broker:broker@127.0.0.1:5432/broker?sslmode=disable"
export JWT_SIGNING_KEY="$(head -c 48 /dev/urandom | base64)"   # >= 32 байт

# 2. Завести инициатора (печатает секрет ОДИН раз)
go run ./cmd/broker create-initiator
#   initiator_id:     <uuid>
#   initiator_secret: <secret>

# 3. Запустить сервер
go run ./cmd/broker
```

Миграции применяются автоматически и при `create-initiator`, и при запуске
сервера.

## Конфигурация (env)

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `HTTP_ADDR` | `:8080` | адрес HTTP-сервера |
| `DATABASE_URL` | — (обязателен) | строка подключения к Postgres |
| `JWT_SIGNING_KEY` | — (обязателен, ≥32 байт) | ключ подписи HS256 |
| `ACCESS_TTL` | `30m` | время жизни access-токена |
| `VISIBILITY_TIMEOUT` | `30s` | блокировка выданных сообщений |
| `MAX_FETCH` | `100` | потолок `max` на один забор |
| `MAX_PAYLOAD_BYTES` | `262144` | лимит payload (256 КБ) → `413` |
| `LISTEN_CHANNEL` | `new_message` | канал `LISTEN/NOTIFY` |
| `MIGRATIONS_DIR` | `migrations` | каталог с `*.sql` |
| `AUTH_RATE_PER_MIN` | `60` | лимит `/auth/token` на IP в минуту (`0` — выкл.) |

## REST API

Все ответы — JSON. `payload` — base64. Авторизация: `Authorization: Bearer
<jwt>` (кроме `/healthz` и `/v1/auth/token`).

| Метод | Путь | Роль | Тело | Ответ |
|---|---|---|---|---|
| `GET` | `/healthz` | — | — | `200 ok` |
| `POST` | `/v1/auth/token` | — | `{"secret"}` | `{"access_token","expires_in"}` |
| `POST` | `/v1/receivers` | initiator | — | `{"receiver_id","receiver_secret"}` (один раз) |
| `GET` | `/v1/receivers` | initiator | — | `[{"id","revoked","created_at"}]` |
| `DELETE` | `/v1/receivers/{id}` | initiator (владелец) | — | `204` |
| `POST` | `/v1/messages` | оба | `{"to","payload","client_msg_id?"}` | `{"id"}` / `{"id":null,"duplicate":true}` |
| `GET` | `/v1/messages?max=N` | оба | — | `[{"id","from","payload","created_at"}]` |
| `POST` | `/v1/messages/ack` | оба | `{"ids":[...]}` | `{"acked":n}` |

Коды: `401` нет/протух токен · `403` нарушение политики `to` либо не владелец ·
`413` payload больше лимита · `429` превышен rate-limit на `/auth/token`.

### Пример: инициатор → приёмник

```bash
B=http://127.0.0.1:8080

# токен инициатора
ITOK=$(curl -s $B/v1/auth/token -d '{"secret":"<initiator_secret>"}' | jq -r .access_token)

# создать приёмник (секрет выдаётся один раз)
curl -s -X POST $B/v1/receivers -H "Authorization: Bearer $ITOK"
# {"receiver_id":"<rid>","receiver_secret":"<rsec>"}

# отправить (payload "hello" в base64)
curl -s -X POST $B/v1/messages -H "Authorization: Bearer $ITOK" \
  -d '{"to":"<rid>","payload":"aGVsbG8=","client_msg_id":"m1"}'
# {"id":1}

# приёмник: токен → забор → подтверждение
RTOK=$(curl -s $B/v1/auth/token -d '{"secret":"<rsec>"}' | jq -r .access_token)
curl -s "$B/v1/messages?max=10" -H "Authorization: Bearer $RTOK"
# [{"id":1,"from":"<iid>","payload":"aGVsbG8=","created_at":"..."}]
curl -s -X POST $B/v1/messages/ack -H "Authorization: Bearer $RTOK" -d '{"ids":[1]}'
# {"acked":1}
```

## WebSocket

Апгрейд: `GET /v1/ws` с `Authorization: Bearer <jwt>` на handshake
(не-браузерные клиенты). Сессия регистрируется в hub по `participant id = sub`;
у одного participant может быть несколько сессий.

**Клиент → сервер:**
```json
{"type":"fetch","max":50}
{"type":"ack","ids":[12,13]}
{"type":"send","to":"<uuid>","payload":"<base64>","client_msg_id":"abc"}
```
**Сервер → клиент:**
```json
{"type":"new"}                 // дверной звонок: есть новое
{"type":"messages","items":[...]}
{"type":"ack_ok","ids":[12,13]}
{"type":"sent","id":99}        // или {"type":"sent","id":null,"duplicate":true}
{"type":"error","code":"...","message":"..."}
```

Поток: при коннекте клиент шлёт `fetch` (дренаж бэклога), дальше работает на
звонках. На `{"type":"new"}` клиент отвечает `fetch` → сервер `messages` →
клиент `ack`. Keepalive — ping/pong ~20 с; на разрыв сессия снимается из hub.

## Тесты

Юнит-тесты (без БД):

```bash
go test ./internal/secret/... ./internal/auth/... ./internal/httpapi/...
```

Интеграционные тесты поднимают полный стек (HTTP + WS + listener) поверх
реального Postgres. Укажите БД через `BROKER_TEST_DATABASE_URL` (или
`DATABASE_URL`); без неё интеграционные тесты пропускаются.

```bash
export BROKER_TEST_DATABASE_URL="postgres://broker:broker@127.0.0.1:5432/broker?sslmode=disable"
go test -race ./...
```

Покрытие: send→fetch→ack в обе стороны, идемпотентность по `client_msg_id`,
реклейм по visibility timeout, конкурентный забор без дублей (SKIP LOCKED),
`revoke` блокирует повторную авторизацию, WS-звонок, мультисессия, отправка по
WS.

## Безопасность (несущие правила)

- Секреты — `crypto/rand`, 32 байта, `base64.RawURLEncoding`.
- В БД хранится только `sha256(secret)`; сырой секрет показывается один раз.
- Сравнение хешей — `subtle.ConstantTimeCompare`.
- JWT проверяется stateless (подпись + exp); `revoked` проверяется только при
  выпуске токена → держите `ACCESS_TTL` коротким.
- `from` — всегда из `sub`; `to` валидируется политикой до вставки.

## Вне скоупа

Refresh-токены, парсинг/хранение содержимого payload, провижининг ключей в
клиента, mesh приёмник↔приёмник, аудит/soft-delete сообщений.
