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

## Поставка

Артефакт релиза — **один статический файл**: `CGO_ENABLED=0` даёт бинарь без
зависимостей, миграции вшиты через `//go:embed`. Ни Go, ни Docker, ни libc
нужной версии на целевой машине не требуются. Сборка под `linux/amd64` и
`linux/arm64` идёт в GitHub Actions, архивы и контрольные суммы прикладываются
к релизу.

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o broker ./cmd/broker
```

### Как выпустить релиз

Номер версии задаёт файл [`VERSION`](VERSION) — он подставляется в бинарь
(`broker version`) и в имена архивов. Релиз выпускается изменением этого файла
в `main`: `.github/workflows/release.yml` прогоняет полный набор проверок,
собирает обе архитектуры и создаёт тег с релизом через `gh` на том коммите,
который эти проверки прошёл. Тег вручную пушить не нужно — и по построению он
не может указывать на непроверенный код. Повторный запуск на неизменённой
версии останавливается, чтобы не перезаписать опубликованный релиз.

## Установка

На целевой машине нужны только Linux (x86-64 или arm64) и доступный
PostgreSQL 16+. Всё остальное — в бинаре.

```bash
# 1. Скачать архив и проверить целостность
V=v1.0.0
base=https://github.com/artemavrin/broker/releases/download/$V
curl -fsSL -O "$base/broker_${V}_linux_amd64.tar.gz"
curl -fsSL -O "$base/checksums.txt"
sha256sum -c --ignore-missing checksums.txt

# 2. Положить на место
sudo tar -xzf "broker_${V}_linux_amd64.tar.gz" -C /usr/local/bin
broker version

# 3. Базу и роль создаёт администратор СУБД (нужны права суперпользователя)
#    CREATE ROLE broker LOGIN PASSWORD '<пароль>';
#    CREATE DATABASE broker OWNER broker;
#    Расширение pgcrypto брокер создаёт сам: в PG16 оно trusted.

# 4. Проверить площадку до установки — команда ничего не меняет
export DATABASE_URL="postgres://broker:<пароль>@<хост>:5432/broker?sslmode=require"
broker check

# 5. Первичная настройка: секреты, миграции, первый инициатор
sudo install -d -m 0750 /etc/broker
sudo -E broker setup --env-file /etc/broker/broker.env

# 6. Запустить как службу (юнит — ниже) и проверить
sudo systemctl enable --now broker
curl -fsS http://127.0.0.1:8080/healthz
```

Юнит systemd (`/etc/systemd/system/broker.service`); сервис ничего не пишет на
диск, поэтому ограничения безопасные:

```ini
[Unit]
Description=Брокер сообщений
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=broker
EnvironmentFile=/etc/broker/broker.env
ExecStart=/usr/local/bin/broker
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
```

Наружу брокер публикуется обратным прокси: он слушает обычный HTTP, а прокси
добавляет TLS. Существенны две вещи — заголовки перехода на WebSocket и
таймаут простоя больше 20 секунд (брокер держит соединение проверочными
пакетами с этим интервалом):

```nginx
map $http_upgrade $connection_upgrade { default upgrade; '' close; }

location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_read_timeout 120s;
}
```

Обновление — замена файла и перезапуск: `broker check` на новой версии,
`systemctl stop broker`, распаковка архива поверх, `systemctl start broker`.
Миграции новая версия применяет сама при запуске.

## Быстрый старт

### Через docker-compose

```bash
docker compose up --build
```

Поднимется Postgres и брокер на `:8080`. Брокер сам применит миграции при
старте: они вшиты в бинарь через `//go:embed`, поэтому рядом с ним ничего
раскладывать не нужно — артефакт поставки состоит из одного файла. Каталог с
`*.sql` подхватывается только если явно задан `MIGRATIONS_DIR`.

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

## Команды

```bash
broker                    # запустить сервер
broker check              # проверить площадку перед развёртыванием
broker setup              # первичная настройка: проверка, секреты, миграции, инициатор
broker create-initiator   # создать инициатора и напечатать его секрет
broker version            # версия сборки
```

`setup` доводит пустую площадку до готовой: выполняет те же проверки, что
`check`, генерирует `JWT_SIGNING_KEY` и `ADMIN_TOKEN` в файл переменных с
правами `0600`, применяет миграции и создаёт первого инициатора. Команда
неинтерактивная и **идемпотентная** — существующие секреты не
перегенерируются (это обнулило бы все выданные токены), а инициатор создаётся
только если в базе нет ни одного, поэтому её можно вызывать из Ansible.
Базу и роль команда сознательно не создаёт: это требует прав суперпользователя
и остаётся за администратором базы данных.

```bash
broker setup --env-file /etc/broker/broker.env   # куда писать переменные
broker setup --no-initiator                      # не создавать инициатора
```

`check` отвечает на вопросы, которые иначе выясняются уже в работе: доступна ли
база, хватает ли прав на миграции, доступно ли `pgcrypto`, **проходит ли
`LISTEN/NOTIFY` через выбранное подключение** (через пул в режиме транзакций не
проходит, и доставка молча деградирует до опроса), какие стоят гарантии
сохранности и какой темп фиксации транзакций даёт накопитель — с переводом в
класс нагрузки. Ничего не настраивает; единственная запись — таблица замера,
которая удаляется. Требует только `DATABASE_URL`, поэтому запускается до того,
как заведены секреты. Возвращает `1`, если есть отказы.

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
| `MIGRATIONS_DIR` | — | каталог с `*.sql`; пусто — миграции из бинаря |
| `AUTH_RATE_PER_MIN` | `60` | лимит `/auth/token` на IP в минуту (`0` — выкл.) |
| `ADMIN_TOKEN` | — | включает админ-дашборд `/admin/` (пусто — выключен) |
| `PPROF_ADDR` | — | адрес приватного pprof-эндпоинта (пусто — выключен) |
| `LOG_LEVEL` | `info` | минимальный уровень логов: `debug`/`info`/`warn`/`error` |

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

### Размер кадра (важно для клиентов)

`payload` едет в base64 (+33 %), а `messages` отдаётся пачкой, поэтому кадр
бывает заметно больше самого сообщения. Брокер поднимает свой лимит чтения до
`MAX_PAYLOAD_BYTES * 2 + 4096`, так что `send` проходит вплоть до штатного
лимита payload.

**Клиент обязан поднять свой лимит чтения сам.** Большинство библиотек
(включая `coder/websocket`) по умолчанию рвут соединение на кадре больше
32 КиБ — это меньше 24 КБ payload, а с пачкой на `fetch` предел достигается и
на мелких сообщениях (100 × 512 Б ≈ 68 КБ). Симптом неприятный: брокер
сообщение принял и записал, кадр отправил, в его логах чисто — а клиент теряет
соединение и доставки не видит. Варианты: поднять лимит чтения до
`MAX_FETCH × MAX_PAYLOAD_BYTES × 4/3` с запасом на разметку, либо забирать
меньшими пачками (`max`).

## Админ-дашборд

Server-rendered дашборд (встроен в бинарь, без отдельного фронтенда) с
аналитикой и администрированием. Включается, когда задан `ADMIN_TOKEN`;
монтируется на `/admin/`.

```bash
export ADMIN_TOKEN="$(head -c 24 /dev/urandom | base64)"
go run ./cmd/broker
# открыть http://localhost:8080/admin/ и войти по ADMIN_TOKEN
```

Возможности:
- **Overview** — живая аналитика: глубина очереди, in-flight (залоченные),
  число инициаторов/приёмников, backlog по получателям (бар-чарт), throughput
  (sent/acked с момента старта), активные WS-сессии, возраст самого старого
  сообщения. Автообновление каждые 5 с.
- **Initiators** — список и **создание инициатора в один клик**: секрет
  показывается **один раз** (PRG-флоу, секрет нигде не хранится) с кнопкой
  копирования; revoke.
- **Receivers** — список приёмников всех инициаторов с владельцем и revoke.

Доступ: один `ADMIN_TOKEN` (сравнение константно-временное), сессия — cookie
с подписанным JWT (`HttpOnly`, `SameSite=Strict`, `Secure` под TLS). Роль
`admin` бесполезна на `/v1/*` (не проходит политику), так что сессия дашборда
не даёт прав участника. **Ставьте дашборд за TLS** — секреты и сессия ходят по
этому каналу.

### Как завести первого инициатора

Два способа:
1. **Через дашборд:** `/admin/` → *Initiators* → **+ New initiator** →
   скопировать выданные `initiator_id` и `initiator_secret` (показываются раз).
2. **Через CLI** (без дашборда): `go run ./cmd/broker create-initiator`.

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

## Нагрузочное тестирование

`cmd/loadtest` — генератор, который ходит через реальный API: аутентификация,
создание N приёмников, G параллельных отправителей (инициатор → приёмники по
кругу) и по одному потребителю на приёмник. В payload зашит таймстамп, поэтому
меряется и сквозная (e2e) латентность. Потребитель — HTTP-polling или
WS-doorbell.

```bash
# нужен секрет инициатора (из дашборда или create-initiator)
go run ./cmd/loadtest -base http://127.0.0.1:8080 -secret <initiator-secret> \
  -senders 16 -receivers 4 -payload 256 -duration 15s -consumer ws
# либо: make loadtest SECRET=<initiator-secret>
```

Флаги: `-senders`, `-receivers`, `-payload`, `-batch`, `-duration`,
`-consumer http|ws`, `-rate` (лимит sends/s, 0 — без лимита). Отчёт: throughput
(sent/delivered/acked в сек), latency p50/p95/p99 для send и e2e, ошибки и
остаточный backlog (рост = backpressure).

Что показывают замеры (одна локальная PG, durable-коммит): запись —
**commit-bound**, потолок ~650–820 msg/s, и он упирается в **fsync WAL на
каждый коммит**, а не в число соединений (пул больше ~8 только вредит).
`synchronous_commit=off` поднимает запись в ~10–13× (ценой durability
последних сотен мс при краше). WS-doorbell даёт меньшую e2e-латентность, чем
HTTP-polling (нет poll-gap). Забор/ack батчевый и дёшев. Тюнинг-рычаги:
`synchronous_commit`/`commit_delay` (группировка коммитов), `pool_max_conns` в
`DATABASE_URL`, горизонтальное масштабирование инстансов.

### Горизонтальное масштабирование

Брокер **stateless** (единственное состояние в памяти — реестр WS-сессий
`wshub`), поэтому за L4/L7-балансировщиком можно держать N инстансов на одном
Postgres без изменений кода. Что важно знать про мультиинстанс:

- **NOTIFY fan-out.** Каждый инстанс держит свой `LISTEN`, и каждое
  `pg_notify` доставляется **всем** инстансам, даже тем, у кого нет сессии
  адресата (в демо с 3 инстансами — ровно 3 LISTEN-коннекта, каждый звонок
  ×3). Цена на стороне PG; до десятков инстансов приемлемо, дальше fan-out
  становится расточительным.
- **Корректность.** `FOR UPDATE SKIP LOCKED` гарантирует отсутствие дублей,
  даже когда producers и consumers попадают на разные инстансы — очередь одна.
  Потерь нет (в замерах `delivered == sent`). WS-сессия «липнет» к своему
  инстансу (hub в памяти), но звонок всё равно приходит, т.к. NOTIFY
  широковещателен.
- **Throughput не суммируется.** Общий Postgres — потолок. Замеры (durable
  commit): 1 инстанс ≈ 1150 msg/s, 3 за балансировщиком ≈ 920 (не быстрее —
  лишний hop + больше соединений конкурируют за commit). С
  `synchronous_commit=off`: 1 инстанс ≈ 5000 msg/s, 2 параллельно ≈ 5300
  **суммарно** — инстансы делят один PG-потолок, а не складывают его.

Вывод: горизонтально масштабировать брокер-тир стоит ради
**доступности/отказоустойчивости** и app-CPU (который и так дёшев — см.
профиль), но пропускную способность задаёт **общий Postgres**. Чтобы реально
поднять throughput — тюнить/масштабировать PG (батчинг коммитов,
партиционирование/шардирование по `to_addr`, отдельные БД на наборы
участников), а не число инстансов брокера. И берите нормальный L4/HAProxy/nginx
— наивный одно-процессный прокси сам станет узким местом.

### Шардирование очереди по получателю (прототип)

Единственный путь к **кратному** росту записи — раскидать очередь по N
независимых Postgres. Ключ шардирования — получатель (`to_addr`): в звезде
каждое сообщение между инициатором и его приёмником, оба в одном тенанте,
поэтому `to_addr` и `from_addr` всегда в одном шарде. Значит весь поток
тенанта — send, fetch/ack и звонок `LISTEN/NOTIFY` — живёт на одном шарде, и
**ни один запрос не пересекает шарды**. Маршрут: `shard = hash(to_addr) % N`.

Полная интеграция в брокер (эскиз): тенант (= id инициатора) кладётся в JWT →
роутинг stateless без обращения к БД; по одному `LISTEN` на шард; UUID
участников генерит брокер (чтобы знать шард до вставки); `/auth/token` —
scatter-lookup по шардам. Сам брокер уже stateless, так что это аддитивно.

`cmd/shardproto` — измерительный прототип (реальные send/fetch/ack по N шардам):

```bash
go run ./cmd/shardproto -shards "postgres://...:5432/broker,...:5433/broker,...:5434/broker" \
  -senders 24 -recv 4 -duration 10s
```

Что показал прогон (3 независимых кластера PG):
- **Роутинг корректен и равномерен** — распределение по шардам ровное
  (≈41.3k/41.3k/41.3k), пересечений шардов нет, ошибок нет.
- **Кратного роста на одной машине не видно** (durable: 616→745 msg/s ≈1.2×;
  `sync_commit=off`: 11k→12k msg/s ≈1.1×). Причина не в дизайне, а в общих
  ресурсах: на одном 4-vCPU боксе и нагрузчик, и все шарды делят те же ядра и
  **один диск** (общий fsync). Узкое место просто переезжает с WAL-lock на
  диск/CPU, оставаясь общим.
- Партиционирование **без межшардовой координации**, поэтому на независимых
  нодах (свои CPU/диск/сеть на шард) пропускная складывается — N× от потолка
  одного шарда. На одной машине это принципиально не воспроизвести.

Вывод: дизайн шардирования по `to_addr` чистый и масштабируемый; чтобы увидеть
кратный рост, шарды должны жить на разном железе.

### Профилирование (pprof)

Задайте `PPROF_ADDR` (например `127.0.0.1:6060`) — поднимется приватный
`net/http/pprof` на отдельном listener'е (и включится mutex/block-профиль).
Снимать под нагрузкой:

```bash
go tool pprof -top  http://127.0.0.1:6060/debug/pprof/profile?seconds=15  # CPU
go tool pprof -top  http://127.0.0.1:6060/debug/pprof/mutex               # контеншн
go tool pprof -top  http://127.0.0.1:6060/debug/pprof/block               # блокировки
```

Что показал профиль под нагрузкой: брокер **I/O-bound, а не CPU-bound** (~41%
утилизации CPU, в топе self-time — сетевые `syscall`, не прикладной код).
Прикладного хотспота нет: ни JSON, ни base64, ни crypto не доминируют. Контеншн
на mutex'ах ничтожный — глобальный мьютекс `wshub` на этих нагрузках не узкое
место. Время уходит на ожидание Postgres (commit). Вывод совпадает с
нагрузочным: ускорять надо Postgres (батчинг коммитов / масштабирование), а не
Go-код — горячий путь брокера и так тонкий.

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
