#!/usr/bin/env bash
# Установка брокера на Linux и macOS.
#
#   Linux:  curl -fsSL https://raw.githubusercontent.com/artemavrin/broker/main/deploy/install.sh | sudo bash
#   macOS:  curl -fsSL https://raw.githubusercontent.com/artemavrin/broker/main/deploy/install.sh | bash
#
# С параметрами — без вопросов:
#   curl -fsSL .../install.sh | sudo bash -s -- --mode docker --address 203.0.113.5
#
# Если на машине работает Docker, скрипт спросит, как ставить:
#   docker — PostgreSQL, брокер и Caddy в docker compose;
#   native — PostgreSQL 16 из пакетов (apt/dnf на Linux, Homebrew на macOS),
#            брокер и Caddy — службы systemd (Linux) или launchd (macOS).
# HTTPS даёт Caddy с сертификатом Let's Encrypt — прямо на IP или на домен;
# снаружи должны быть открыты порты 443 и 80.
#
# Скрипт идёт по шагам и на каждом проверяет, что уже сделано, поэтому
# повторный запуск безопасен: им же обновляется брокер до новой версии.
# Секреты (JWT_SIGNING_KEY, ADMIN_TOKEN) генерирует сам брокер командой setup
# и никогда не перегенерирует, иначе обнулились бы все выданные токены.
#
# Совместим с bash 3.2 из macOS: без ассоциативных массивов и ${x,,}.
set -euo pipefail

usage() {
	cat <<'EOF'
Параметры:
  --mode M          docker или native; по умолчанию спросит, если есть Docker
  --address A       IP или домен для HTTPS; '-' — без HTTPS; по умолчанию спросит
  --version V       версия брокера, например v1.1.0; по умолчанию последняя
  --port N          локальный порт брокера; по умолчанию 8080
  --database-url U  свой PostgreSQL вместо установки (только native)
EOF
}

REPO=artemavrin/broker
CADDY_VERSION=2.11.7
PG_MAJOR=16

MODE=
ADDRESS=
VERSION=latest
PORT=8080
DATABASE_URL_ARG=
while [ $# -gt 0 ]; do
	case "$1" in
	--mode) MODE=${2:?"$1: нужно значение"}; shift 2 ;;
	--address | --domain) ADDRESS=${2:?"$1: нужно значение"}; shift 2 ;;
	--version) VERSION=${2:?"$1: нужно значение"}; shift 2 ;;
	--port) PORT=${2:?"$1: нужно значение"}; shift 2 ;;
	--database-url) DATABASE_URL_ARG=${2:?"$1: нужно значение"}; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) echo "неизвестный параметр: $1" >&2; usage >&2; exit 2 ;;
	esac
done

if [ -t 1 ]; then
	C_STEP=$'\033[36m' C_OK=$'\033[32m' C_WARN=$'\033[33m' C_ERR=$'\033[31m' C_OFF=$'\033[0m'
else
	C_STEP='' C_OK='' C_WARN='' C_ERR='' C_OFF=''
fi

# Число шагов зависит от режима и известно только после первого шага.
STEP_NO=0
TOTAL_STEPS=
step() {
	STEP_NO=$((STEP_NO + 1))
	printf '\n%sШаг %d%s. %s%s\n' "$C_STEP" "$STEP_NO" "${TOTAL_STEPS:+ из $TOTAL_STEPS}" "$1" "$C_OFF"
}
ok() { printf '%s  ок    %s%s\n' "$C_OK" "$1" "$C_OFF"; }
info() { printf '        %s\n' "$1"; }
warn() { printf '%s  внимание: %s%s\n' "$C_WARN" "$1" "$C_OFF" >&2; }
die() {
	printf '\n%sОшибка: %s%s\n' "$C_ERR" "$1" "$C_OFF" >&2
	exit 1
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Вопросы читаются с терминала: при запуске через curl | bash stdin занят
# самим скриптом. Без терминала берётся ответ по умолчанию.
ask() {
	REPLY=
	if { : >/dev/tty; } 2>/dev/null; then
		printf '%s' "$1" >/dev/tty
		read -r REPLY </dev/tty || REPLY=
	fi
	if [ -z "$REPLY" ]; then REPLY=$2; fi
}

# Шумные команды (apt, dnf, brew) пишут в журнал; он показывается при ошибке.
quiet() {
	local what=$1
	shift
	if ! "$@" >"$TMP/quiet.log" 2>&1; then
		tail -n 20 "$TMP/quiet.log" >&2
		die "$what завершилось с ошибкой"
	fi
}

# hex: без символов, которые пришлось бы экранировать в URL и в SQL.
new_secret() { od -An -N24 -tx1 /dev/urandom | tr -d ' \n'; }

download() {
	info "загрузка $1"
	curl -fsSL --retry 3 -o "$2" "$1"
}

hash_of() {
	if command -v "sha${1}sum" >/dev/null 2>&1; then
		"sha${1}sum" "$2"
	else
		shasum -a "$1" "$2"
	fi | awk '{print $1}'
}

verify_checksum() {
	local file=$1 sums=$2 bits=$3 name want
	name=$(basename "$file")
	want=$(awk -v n="$name" '$2 == n || $2 == "*" n {print $1; exit}' "$sums")
	[ -n "$want" ] || die "в $(basename "$sums") нет контрольной суммы для $name"
	[ "$(hash_of "$bits" "$file")" = "$want" ] ||
		die "контрольная сумма $name не совпала: файл повреждён или подменён"
	ok "контрольная сумма $name совпала"
}

env_get() {
	if [ -f "$1" ]; then sed -n "s/^[[:space:]]*$2[[:space:]]*=[[:space:]]*//p" "$1" | tail -n 1; fi
}

port_busy() { (: </dev/tcp/127.0.0.1/"$1") 2>/dev/null; }

wait_http() {
	local url=$1 seconds=$2 waited=0
	shift 2
	while [ "$waited" -lt "$seconds" ]; do
		if curl -fsS -m 5 -o /dev/null "$@" "$url" 2>/dev/null; then return 0; fi
		sleep 2
		waited=$((waited + 2))
	done
	return 1
}

go_arch() {
	case "$1" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "архитектура $1 не поддерживается: релизы собираются под amd64 и arm64" ;;
	esac
}

# Скачивает бинарь брокера из релиза, проверяет и кладёт в $3.
fetch_broker() {
	local os=$1 arch=$2 dest=$3
	local name="broker_${VERSION}_${os}_${arch}.tar.gz"
	local base="https://github.com/$REPO/releases/download/$VERSION"
	download "$base/$name" "$TMP/$name" ||
		die "в релизе $VERSION нет сборки $os/$arch ($name)"
	download "$base/checksums.txt" "$TMP/checksums.txt"
	verify_checksum "$TMP/$name" "$TMP/checksums.txt" 256
	mkdir -p "$TMP/broker-$os-$arch" "$(dirname "$dest")"
	tar -xzf "$TMP/$name" -C "$TMP/broker-$os-$arch"
	# Через переименование: файл работающей службы нельзя перезаписать на месте.
	install -m 0755 "$TMP/broker-$os-$arch/broker" "$dest.new"
	mv -f "$dest.new" "$dest"
}

# Скачивает Caddy (у него macOS называется mac, суммы — SHA-512) и кладёт в $3.
fetch_caddy() {
	local os=$1 arch=$2 dest=$3
	local name="caddy_${CADDY_VERSION}_${os}_${arch}.tar.gz"
	local base="https://github.com/caddyserver/caddy/releases/download/v$CADDY_VERSION"
	download "$base/$name" "$TMP/$name"
	download "$base/caddy_${CADDY_VERSION}_checksums.txt" "$TMP/caddy_checksums.txt"
	verify_checksum "$TMP/$name" "$TMP/caddy_checksums.txt" 512
	mkdir -p "$TMP/caddy" "$(dirname "$dest")"
	tar -xzf "$TMP/$name" -C "$TMP/caddy" caddy
	install -m 0755 "$TMP/caddy/caddy" "$dest.new"
	mv -f "$dest.new" "$dest"
}

is_ipv4() { echo "$1" | grep -Eq '^[0-9]{1,3}(\.[0-9]{1,3}){3}$'; }

# Caddyfile для адреса брокера. Дашборд /admin/ в интернет не публикуется: он
# доступен на сервере по http://127.0.0.1:<порт>/admin/.
# На IP Caddy по умолчанию выпускает самоподписанный сертификат, поэтому
# Let's Encrypt задаётся явно: на IP он выдаёт сертификаты только с профилем
# shortlived (срок около 6 дней, Caddy продлевает сам). Клиенты, которые
# подключаются по IP, не передают имя сервера (SNI) — отсюда default_sni.
# Хранилище и журнал необязательны: без них Caddy берёт свои значения.
write_caddyfile() {
	local file=$1 address=$2 upstream=$3 storage=${4:-} logfile=${5:-} ip=
	if is_ipv4 "$address"; then ip=1; fi
	{
		if [ -n "$storage$logfile$ip" ]; then
			echo '{'
			if [ -n "$storage" ]; then printf '\tstorage file_system "%s"\n' "$storage"; fi
			if [ -n "$ip" ]; then printf '\tdefault_sni %s\n' "$address"; fi
			if [ -n "$logfile" ]; then printf '\tlog {\n\t\toutput file "%s"\n\t}\n' "$logfile"; fi
			printf '}\n\n'
		fi
		printf '%s {\n' "$address"
		if [ -n "$ip" ]; then printf '\ttls {\n\t\tissuer acme {\n\t\t\tprofile shortlived\n\t\t}\n\t}\n'; fi
		printf '\t@admin path /admin /admin/*\n\trespond @admin 403\n\treverse_proxy %s\n}\n' "$upstream"
	} >"$file"
}

# Проверка HTTPS идёт на локальный порт 443, но с адресом и сертификатом
# публичного: многие роутеры не пускают сервер на собственный внешний IP.
wait_https() {
	info "получение сертификата Let's Encrypt (до 2 минут)…"
	if wait_http "https://$ADDRESS/healthz" 120 --connect-to "$ADDRESS:443:127.0.0.1:443"; then
		ok "https://$ADDRESS/healthz отвечает"
	else
		warn "https://$ADDRESS/healthz не ответил. Частые причины: порты 80/443 закрыты снаружи,"
		warn "домен не указывает на этот сервер. Журнал Caddy: $1"
	fi
}

open_firewall() {
	if command -v ufw >/dev/null 2>&1 && LC_ALL=C ufw status 2>/dev/null | grep -q '^Status: active'; then
		quiet 'ufw' ufw allow 80/tcp
		quiet 'ufw' ufw allow 443/tcp
		ok 'файрвол ufw: открыты входящие 80 и 443'
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		quiet 'firewall-cmd' firewall-cmd --permanent --add-service=http --add-service=https
		quiet 'firewall-cmd' firewall-cmd --reload
		ok 'файрвол firewalld: открыты входящие 80 и 443'
	else
		info 'локальный файрвол не включён — открывать нечего'
	fi
}

# --- PostgreSQL -------------------------------------------------------------

# Служба PostgreSQL в systemd: postgresql (Debian, Fedora) или postgresql-16
# (PGDG на RHEL). Шаблоны вида postgresql@ пропускаются.
pg_unit() {
	systemctl list-unit-files --type=service --no-legend 'postgresql*' 2>/dev/null |
		awk '{print $1}' | grep -v '@' | head -n 1 || true
}

install_postgres_linux() {
	# shellcheck disable=SC1091
	. /etc/os-release
	if command -v apt-get >/dev/null 2>&1; then
		info "установка PostgreSQL $PG_MAJOR из репозитория PGDG (несколько минут)…"
		export DEBIAN_FRONTEND=noninteractive
		quiet 'apt-get update' apt-get update
		quiet 'apt-get install' apt-get install -y postgresql-common ca-certificates
		quiet 'подключение PGDG' /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y
		quiet 'apt-get install' apt-get install -y "postgresql-$PG_MAJOR"
	elif command -v dnf >/dev/null 2>&1; then
		if [ "${ID:-}" = fedora ]; then
			# В Fedora своя свежая версия (не ниже 16), PGDG не нужен. В RHEL и
			# Fedora pgcrypto лежит в отдельном пакете contrib.
			info 'установка PostgreSQL из репозитория Fedora (несколько минут)…'
			quiet 'dnf install' dnf install -y postgresql-server postgresql-contrib
			quiet 'initdb' postgresql-setup --initdb
		else
			info "установка PostgreSQL $PG_MAJOR из репозитория PGDG (несколько минут)…"
			quiet 'подключение PGDG' dnf install -y \
				"https://download.postgresql.org/pub/repos/yum/reporpms/EL-${VERSION_ID%%.*}-$(uname -m)/pgdg-redhat-repo-latest.noarch.rpm"
			# В EL 8/9 встроенный модуль postgresql перекрывает PGDG; в EL 10 модулей нет.
			dnf -qy module disable postgresql >/dev/null 2>&1 || true
			quiet 'dnf install' dnf install -y "postgresql$PG_MAJOR-server" "postgresql$PG_MAJOR-contrib"
			quiet 'initdb' "/usr/pgsql-$PG_MAJOR/bin/postgresql-$PG_MAJOR-setup" initdb
		fi
	else
		die 'нет ни apt, ни dnf: поставьте PostgreSQL 16+ сами и передайте --database-url, либо ставьте в Docker'
	fi
}

# Команды от имени суперпользователя базы: на Linux — системный пользователь
# postgres через peer-аутентификацию, на macOS (Homebrew) — текущий.
pg_admin() {
	if [ "$OS" = linux ]; then
		(cd / && runuser -u postgres -- "$PSQL" -X -q -v ON_ERROR_STOP=1 -d postgres "$@")
	else
		"$PSQL" -X -q -v ON_ERROR_STOP=1 -d postgres "$@"
	fi
}

ensure_role_and_db() {
	local password=$1
	if [ "$(pg_admin -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'broker'")" = 1 ]; then
		pg_admin -c "ALTER ROLE broker LOGIN PASSWORD '$password'"
		ok 'роль broker уже была — пароль обновлён'
	else
		pg_admin -c "CREATE ROLE broker LOGIN PASSWORD '$password'"
		ok 'роль broker создана'
	fi
	if [ "$(pg_admin -tAc "SELECT 1 FROM pg_database WHERE datname = 'broker'")" = 1 ]; then
		ok 'база broker уже существует'
	else
		pg_admin -c 'CREATE DATABASE broker OWNER broker'
		ok 'база broker создана'
	fi
}

# В RHEL и Fedora подключения по TCP с localhost по умолчанию идут через ident,
# и вход по паролю не работает. Правило для роли broker ставится первым:
# PostgreSQL берёт первое подходящее, остальные настройки не трогаются.
allow_password_login() {
	local hba
	hba=$(pg_admin -tAc 'SHOW hba_file')
	if grep -Eq '^host[[:space:]]+broker[[:space:]]+broker[[:space:]]' "$hba"; then return; fi
	{
		echo '# broker: вход по паролю с localhost (добавлено установщиком брокера)'
		echo 'host broker broker 127.0.0.1/32 scram-sha-256'
		echo 'host broker broker ::1/128 scram-sha-256'
		cat "$hba"
	} >"$TMP/pg_hba.conf"
	# cat, а не mv: у файла остаются владелец и права.
	cat "$TMP/pg_hba.conf" >"$hba"
	pg_admin -c 'SELECT pg_reload_conf()' >/dev/null
	ok "в $hba разрешён вход роли broker по паролю"
}

# --- launchd (macOS) --------------------------------------------------------

# Служба пользователя: запускается при входе в систему и перезапускается при
# падении. Аргументы после журнала — команда запуска.
launch_agent() {
	local label=$1 log=$2 plist arg tries=0
	shift 2
	plist="$HOME/Library/LaunchAgents/$label.plist"
	mkdir -p "$HOME/Library/LaunchAgents"
	{
		cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$label</string>
	<key>ProgramArguments</key>
	<array>
EOF
		for arg in "$@"; do printf '\t\t<string>%s</string>\n' "$arg"; done
		cat <<EOF
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>$log</string>
	<key>StandardErrorPath</key>
	<string>$log</string>
</dict>
</plist>
EOF
	} >"$plist"
	launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
	# bootout завершается асинхронно: сразу после него bootstrap может не пройти.
	until launchctl bootstrap "gui/$(id -u)" "$plist" 2>/dev/null; do
		tries=$((tries + 1))
		[ "$tries" -lt 10 ] || die "launchctl не смог запустить $label"
		sleep 1
	done
}

# --- Режимы установки -------------------------------------------------------

install_native() {
	step 'Брокер: загрузка релиза'
	fetch_broker "$OS" "$(go_arch "$(uname -m)")" "$BROKER_BIN"
	ok "установлен $BROKER_BIN ($("$BROKER_BIN" version))"

	step 'PostgreSQL'
	local database_url
	database_url=$(env_get "$ENV_FILE" DATABASE_URL)
	if [ -n "$database_url" ]; then
		ok "DATABASE_URL уже задан в $ENV_FILE — база настроена ранее"
	elif [ -n "$DATABASE_URL_ARG" ]; then
		database_url=$DATABASE_URL_ARG
		ok 'используется база из --database-url'
	elif [ "$OS" = linux ]; then
		PG_UNIT=$(pg_unit)
		if [ -z "$PG_UNIT" ]; then
			install_postgres_linux
			PG_UNIT=$(pg_unit)
			[ -n "$PG_UNIT" ] || die 'после установки не найдена служба PostgreSQL'
			ok "PostgreSQL установлен ($PG_UNIT)"
		else
			ok "уже установлен: $PG_UNIT"
		fi
		quiet 'запуск PostgreSQL' systemctl enable --now "$PG_UNIT"
		PSQL=$(command -v psql 2>/dev/null || find /usr/pgsql-* -name psql -path "*/bin/*" 2>/dev/null | head -n 1 || true)
		[ -n "$PSQL" ] || die 'не найден psql'
		id -u postgres >/dev/null 2>&1 ||
			die 'нет системного пользователя postgres: передайте --database-url'
	else
		command -v brew >/dev/null 2>&1 ||
			die 'нужен Homebrew (https://brew.sh) — либо поставьте Docker Desktop и выберите Docker'
		if brew list --versions "postgresql@$PG_MAJOR" >/dev/null 2>&1; then
			ok "уже установлен: postgresql@$PG_MAJOR"
		else
			info "установка postgresql@$PG_MAJOR через Homebrew (несколько минут)…"
			quiet 'brew install' brew install "postgresql@$PG_MAJOR"
			ok "PostgreSQL $PG_MAJOR установлен"
		fi
		PSQL="$(brew --prefix "postgresql@$PG_MAJOR")/bin/psql"
		if port_busy 5432; then
			info 'порт 5432 уже занят — используется работающий там PostgreSQL'
		else
			quiet 'brew services start' brew services start "postgresql@$PG_MAJOR"
			local waited=0
			until "$(dirname "$PSQL")/pg_isready" -q -h 127.0.0.1 -p 5432; do
				waited=$((waited + 1))
				[ "$waited" -lt 30 ] || die 'PostgreSQL не запустился за 30 секунд'
				sleep 1
			done
			ok 'PostgreSQL запущен (brew services)'
		fi
	fi

	if [ -z "$database_url" ]; then
		step 'База данных и роль broker'
		pg_admin -tAc 'SELECT 1' >/dev/null 2>&1 ||
			die "нет прав суперпользователя в PostgreSQL на 5432: передайте --database-url или ставьте в Docker"
		local password
		password=$(new_secret)
		ensure_role_and_db "$password"
		if [ "$OS" = linux ]; then allow_password_login; fi
		# База на этой же машине и наружу не смотрит, поэтому без TLS.
		database_url="postgres://broker:$password@127.0.0.1:5432/broker?sslmode=disable"
	else
		step 'База данных и роль broker'
		info 'пропущено: база уже настроена'
	fi

	step 'Проверка площадки и первичная настройка брокера'
	mkdir -p "$(dirname "$ENV_FILE")"
	chmod 0750 "$(dirname "$ENV_FILE")"
	# Брокер слушает только loopback: наружу его публикует Caddy.
	if [ -z "$(env_get "$ENV_FILE" HTTP_ADDR)" ]; then
		(umask 077 && echo "HTTP_ADDR=127.0.0.1:$PORT" >>"$ENV_FILE")
	fi
	# setup сам проверяет площадку и останавливается на отказах; дописывает
	# DATABASE_URL и секреты в файл, существующие не трогает,
	# и создаёт первого инициатора, только если в базе нет ни одного.
	DATABASE_URL=$database_url "$BROKER_BIN" setup --env-file "$ENV_FILE" || die 'broker setup не прошёл'

	if [ "$OS" = linux ]; then
		install_native_linux_services
	else
		install_native_macos_services
	fi
}

install_native_linux_services() {
	step 'Служба systemd'
	if ! id -u broker >/dev/null 2>&1; then
		useradd --system --no-create-home --shell "$(command -v nologin || echo /usr/sbin/nologin)" broker
		ok 'создан системный пользователь broker'
	fi
	PG_UNIT=${PG_UNIT:-$(pg_unit)}
	# Брокер ничего не пишет на диск, поэтому ограничения строгие. Файл
	# переменных читает systemd до сброса прав, так что ему хватает 0600 root.
	cat >/etc/systemd/system/broker.service <<EOF
[Unit]
Description=Брокер сообщений
After=network-online.target $PG_UNIT
Wants=network-online.target $PG_UNIT

[Service]
Type=exec
User=broker
EnvironmentFile=$ENV_FILE
ExecStart=$BROKER_BIN
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
	quiet 'systemctl enable' systemctl enable broker
	systemctl restart broker
	if ! wait_http "http://127.0.0.1:$PORT/healthz" 30; then
		journalctl -u broker -n 20 --no-pager >&2 || true
		die 'служба брокера не поднялась'
	fi
	ok "служба broker запущена, http://127.0.0.1:$PORT/healthz отвечает"

	step "HTTPS: Caddy и Let's Encrypt"
	if [ "$ADDRESS" = - ]; then
		info 'пропущено'
		return
	fi
	local caddy_bin=/usr/local/lib/broker/caddy caddyfile=/etc/broker-caddy/Caddyfile
	fetch_caddy linux "$(go_arch "$(uname -m)")" "$caddy_bin"
	mkdir -p "$(dirname "$caddyfile")"
	write_caddyfile "$caddyfile" "$ADDRESS" "127.0.0.1:$PORT"
	quiet 'caddy validate' env XDG_DATA_HOME="$TMP" XDG_CONFIG_HOME="$TMP" \
		"$caddy_bin" validate --config "$caddyfile" --adapter caddyfile
	ok "Caddyfile: $caddyfile"
	# Отдельный безымянный пользователь: Caddy получает только право занять
	# порты 80/443, а сертификаты хранит в /var/lib/broker-caddy.
	cat >/etc/systemd/system/broker-caddy.service <<EOF
[Unit]
Description=HTTPS для брокера (Caddy)
After=network-online.target broker.service
Wants=network-online.target

[Service]
Type=notify
DynamicUser=true
StateDirectory=broker-caddy
Environment=XDG_DATA_HOME=/var/lib/broker-caddy XDG_CONFIG_HOME=/var/lib/broker-caddy
ExecStart=$caddy_bin run --config $caddyfile --adapter caddyfile
Restart=on-failure
RestartSec=5s
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
	quiet 'systemctl enable' systemctl enable broker-caddy
	systemctl restart broker-caddy
	open_firewall
	wait_https 'journalctl -u broker-caddy'
}

install_native_macos_services() {
	step 'Служба launchd'
	mkdir -p "$LOG_DIR"
	# launchd не читает файлы переменных: их подставляет sh перед запуском.
	launch_agent com.artemavrin.broker "$LOG_DIR/broker.log" \
		/bin/sh -c "set -a; . '$ENV_FILE'; exec '$BROKER_BIN'"
	if ! wait_http "http://127.0.0.1:$PORT/healthz" 30; then
		tail -n 20 "$LOG_DIR/broker.log" >&2 || true
		die 'служба брокера не поднялась'
	fi
	ok "служба запущена, http://127.0.0.1:$PORT/healthz отвечает"

	step "HTTPS: Caddy и Let's Encrypt"
	if [ "$ADDRESS" = - ]; then
		info 'пропущено'
		return
	fi
	local caddy_bin="$BASE_DIR/caddy/caddy" caddyfile="$BASE_DIR/Caddyfile"
	fetch_caddy mac "$(go_arch "$(uname -m)")" "$caddy_bin"
	write_caddyfile "$caddyfile" "$ADDRESS" "127.0.0.1:$PORT" "$BASE_DIR/caddy-data" "$LOG_DIR/caddy.log"
	quiet 'caddy validate' "$caddy_bin" validate --config "$caddyfile" --adapter caddyfile
	ok "Caddyfile: $caddyfile"
	# Начиная с macOS 10.14 порты 80 и 443 можно занять без root.
	launch_agent com.artemavrin.broker-caddy "$LOG_DIR/caddy-launchd.log" \
		"$caddy_bin" run --config "$caddyfile" --adapter caddyfile
	wait_https "$LOG_DIR/caddy.log"
}

dc() { docker compose -f "$BASE_DIR/compose.yml" "$@"; }

install_docker() {
	if [ "$OS" = linux ] && ! systemctl is-enabled --quiet docker 2>/dev/null; then
		warn 'служба docker не в автозапуске — после перезагрузки брокер не поднимется: systemctl enable docker'
	fi

	step 'Брокер: загрузка релиза'
	# В контейнере всегда Linux; архитектура — та, на которой работает Docker
	# (на Mac с Apple Silicon это arm64).
	if [ -f "$BASE_DIR/compose.yml" ]; then
		info 'остановка контейнера брокера для замены файла'
		dc rm -sf broker >/dev/null 2>&1 || true
	fi
	fetch_broker linux "$(go_arch "$(docker version -f '{{.Server.Arch}}')")" "$BASE_DIR/bin/broker"
	ok "установлен $BASE_DIR/bin/broker"

	step 'PostgreSQL в контейнере'
	mkdir -p "$BASE_DIR/config"
	chmod 0750 "$BASE_DIR" "$BASE_DIR/config"
	local password
	password=$(env_get "$BASE_DIR/postgres.env" POSTGRES_PASSWORD)
	if [ -z "$password" ]; then
		password=$(new_secret)
		(umask 077 && printf 'POSTGRES_USER=broker\nPOSTGRES_PASSWORD=%s\nPOSTGRES_DB=broker\n' "$password" \
			>"$BASE_DIR/postgres.env")
		ok "пароль базы сохранён в $BASE_DIR/postgres.env"
	fi
	# Файл переменных должен существовать до первого запуска compose.
	(umask 077 && touch "$BASE_DIR/config/broker.env")
	write_compose
	info 'запуск PostgreSQL…'
	quiet 'docker compose up postgres' dc up -d --wait postgres
	ok 'PostgreSQL запущен'

	step 'Проверка площадки и первичная настройка брокера'
	# Каталог монтируется целиком: setup пишет файл атомарно, через переименование.
	dc run --rm -T --no-deps --user 0:0 \
		-v "$BASE_DIR/config:/etc/broker" \
		-e "DATABASE_URL=postgres://broker:$password@postgres:5432/broker?sslmode=disable" \
		broker setup --env-file /etc/broker/broker.env || die 'broker setup не прошёл'

	step 'Запуск брокера'
	quiet 'docker compose up broker' dc up -d broker
	if ! wait_http "http://127.0.0.1:$PORT/healthz" 30; then
		dc logs --tail 20 broker >&2 || true
		die 'контейнер брокера не поднялся'
	fi
	ok "брокер запущен, http://127.0.0.1:$PORT/healthz отвечает"

	step "HTTPS: Caddy и Let's Encrypt"
	if [ "$ADDRESS" = - ]; then
		quiet 'docker compose up' dc up -d --remove-orphans
		info 'пропущено'
		return
	fi
	write_caddyfile "$BASE_DIR/Caddyfile" "$ADDRESS" broker:8080
	quiet 'caddy validate' dc run --rm -T --no-deps caddy \
		caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
	ok "Caddyfile: $BASE_DIR/Caddyfile"
	quiet 'docker compose up' dc up -d --remove-orphans
	# Caddy читает конфигурацию при старте: после правки Caddyfile — перезапуск.
	quiet 'docker compose restart caddy' dc restart caddy
	if [ "$OS" = linux ]; then open_firewall; fi
	wait_https "cd $BASE_DIR && docker compose logs caddy"
}

write_compose() {
	{
		cat <<EOF
# Создано установщиком брокера (deploy/install.sh); повторный запуск его перезапишет.
# Имя проекта не broker: иначе он делил бы тома с docker-compose.yml из репозитория.
name: broker-server
services:
  postgres:
    image: postgres:$PG_MAJOR
    restart: unless-stopped
    env_file: postgres.env
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U broker -d broker"]
      interval: 2s
      timeout: 3s
      retries: 30

  # Бинарь релиза статический, поэтому образ сборки не нужен: файл
  # монтируется в distroless. Обновление — замена bin/broker.
  broker:
    image: gcr.io/distroless/static-debian12:nonroot
    restart: unless-stopped
    entrypoint: ["/app/broker"]
    env_file: config/broker.env
    environment:
      HTTP_ADDR: ":8080"
    volumes:
      - ./bin/broker:/app/broker:ro
    ports:
      - "127.0.0.1:$PORT:8080"
    depends_on:
      postgres:
        condition: service_healthy
EOF
		if [ "$ADDRESS" != - ]; then
			cat <<EOF

  caddy:
    image: caddy:$CADDY_VERSION
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
      - caddy_config:/config
    depends_on:
      - broker
EOF
		fi
		cat <<EOF

volumes:
  pgdata:
  caddy_data:
  caddy_config:
EOF
	} >"$BASE_DIR/compose.yml"
	ok "compose: $BASE_DIR/compose.yml"
}

docker_ready() {
	command -v docker >/dev/null 2>&1 &&
		docker info >/dev/null 2>&1 &&
		docker compose version >/dev/null 2>&1
}

# --- Основной ход -----------------------------------------------------------

# Весь ход — в функции, вызываемой последней строкой: при запуске через
# curl | bash оборванная загрузка не выполнит половину скрипта.
main() {
	step 'Проверка системы и параметры'

	case "$(uname -s)" in
	Linux) OS=linux ;;
	Darwin) OS=darwin ;;
	*) die "система $(uname -s) не поддерживается; для Windows — deploy/install.ps1" ;;
	esac

	if [ "$OS" = linux ]; then
		[ "$(id -u)" -eq 0 ] || die 'нужны права root: запустите через sudo (curl … | sudo bash)'
		[ -d /run/systemd/system ] || die 'нужен systemd'
		DOCKER_DIR=/opt/broker
		NATIVE_MARK=/etc/systemd/system/broker.service
		# shellcheck disable=SC1091
		ok "$(. /etc/os-release && echo "${PRETTY_NAME:-Linux}"), $(uname -m)"
	else
		[ "$(id -u)" -ne 0 ] || die 'на macOS запускайте без sudo: Homebrew и службы пользователя не работают от root'
		DOCKER_DIR=$HOME/.broker-docker
		NATIVE_MARK=$HOME/Library/LaunchAgents/com.artemavrin.broker.plist
		ok "macOS $(sw_vers -productVersion), $(uname -m)"
	fi

	# Повторный запуск продолжает в том же режиме, в котором ставили раньше.
	EXISTING=
	if [ -z "$MODE" ]; then
		if [ -f "$DOCKER_DIR/compose.yml" ]; then
			MODE=docker EXISTING=1
			info "найдена установка в Docker ($DOCKER_DIR) — режим обновления"
		elif [ -f "$NATIVE_MARK" ]; then
			MODE=native EXISTING=1
			info 'найдена нативная установка — режим обновления'
		fi
	fi

	# Все вопросы задаются здесь, чтобы дальше скрипт шёл без участия человека.
	if [ -z "$MODE" ]; then
		if docker_ready; then
			ok "Docker $(docker version -f '{{.Server.Version}}' 2>/dev/null) и docker compose доступны"
			info 'docker — PostgreSQL, брокер и Caddy в контейнерах (docker compose);'
			info 'native — PostgreSQL из пакетов, брокер и Caddy — службы системы.'
			ask 'Как устанавливать: docker или native? [docker] ' docker
			case "$REPLY" in d* | D*) MODE=docker ;; n* | N*) MODE=native ;; *) MODE=$REPLY ;; esac
		elif command -v docker >/dev/null 2>&1; then
			warn 'Docker установлен, но не отвечает (не запущен или нет docker compose)'
			ask 'Продолжить нативную установку? Для установки в Docker запустите его и повторите. [y/N] ' n
			case "$REPLY" in y | Y | yes | д | Д | да) MODE=native ;; *) exit 1 ;; esac
		else
			info 'Docker не найден — нативная установка'
			MODE=native
		fi
	fi
	case "$MODE" in
	docker) docker_ready || die 'Docker недоступен: запустите его или выберите --mode native' ;;
	native) ;;
	*) die "неизвестный режим '$MODE': нужен docker или native" ;;
	esac
	ok "режим: $MODE"
	if [ "$MODE" = docker ]; then TOTAL_STEPS=6; else TOTAL_STEPS=7; fi
	if [ "$MODE" = docker ] && [ -n "$DATABASE_URL_ARG" ]; then
		die '--database-url применим только к нативной установке'
	fi

	if [ -z "$ADDRESS" ]; then
		ip=$(curl -fsS -m 10 https://api.ipify.org 2>/dev/null || true)
		if [ -n "$ip" ]; then info "внешний IP: $ip"; else warn 'не удалось определить внешний IP'; fi
		info "Сертификат Let's Encrypt выпускается прямо на IP; свой домен — по желанию."
		ask "Адрес для HTTPS (IP или домен)${ip:+ [$ip]}, '-' — без HTTPS: " "${ip:--}"
		ADDRESS=$REPLY
	fi
	if [ "$ADDRESS" = - ]; then
		warn 'HTTPS не настраивается: брокер будет доступен только с этой машины (127.0.0.1)'
	else
		ok "адрес: $ADDRESS"
	fi

	if [ -z "$EXISTING" ]; then
		if port_busy "$PORT"; then die "порт $PORT уже занят другой программой; задайте другой через --port"; fi
		if [ "$ADDRESS" != - ] && { port_busy 80 || port_busy 443; }; then
			warn 'порт 80 или 443 уже занят — Caddy не сможет его занять и не получит сертификат'
		fi
	fi

	if [ "$VERSION" = latest ]; then
		VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
		VERSION=${VERSION##*/}
		case "$VERSION" in v[0-9]*) ;; *) die 'не удалось определить последнюю версию брокера' ;; esac
	fi
	ok "версия $VERSION"

	if [ "$MODE" = docker ]; then
		BASE_DIR=$DOCKER_DIR
		install_docker
	elif [ "$OS" = linux ]; then
		BROKER_BIN=/usr/local/bin/broker
		ENV_FILE=/etc/broker/broker.env
		install_native
	else
		BASE_DIR=$HOME/.broker
		BROKER_BIN=$BASE_DIR/bin/broker
		ENV_FILE=$BASE_DIR/broker.env
		LOG_DIR=$BASE_DIR/logs
		install_native
	fi

	echo
	printf '%sГотово.%s\n' "$C_OK" "$C_OFF"
	if [ "$ADDRESS" != - ]; then info "Адрес брокера:      https://$ADDRESS"; fi
	if [ "$MODE" = docker ]; then
		info "Дашборд (на этой машине): http://127.0.0.1:$PORT/admin/  (токен — ADMIN_TOKEN в $BASE_DIR/config/broker.env)"
		info "Каталог установки:  $BASE_DIR"
		if [ "$OS" = darwin ]; then info 'Docker Desktop должен запускаться при входе в систему — иначе брокер не поднимется.'; fi
		info "Журнал:             cd $BASE_DIR && docker compose logs -f broker"
		info "Состояние:          cd $BASE_DIR && docker compose ps"
	elif [ "$OS" = linux ]; then
		info "Дашборд (на сервере): http://127.0.0.1:$PORT/admin/  (токен — ADMIN_TOKEN в $ENV_FILE)"
		info "  с другой машины:    ssh -L $PORT:127.0.0.1:$PORT <сервер>, затем тот же адрес"
		info "Журнал:             journalctl -u broker -f"
		info "Состояние:          systemctl status broker broker-caddy"
	else
		info "Дашборд:            http://127.0.0.1:$PORT/admin/  (токен — ADMIN_TOKEN в $ENV_FILE)"
		info "Журнал:             tail -f $LOG_DIR/broker.log"
		info 'Службы работают, пока пользователь вошёл в систему (LaunchAgent).'
	fi
	info 'Обновление:         запустить этот скрипт ещё раз (можно с --version vX.Y.Z)'
	info 'Секрет инициатора печатается выше один раз (шаг настройки) — сохраните его.'
}

main
