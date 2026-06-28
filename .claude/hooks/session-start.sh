#!/bin/bash
# SessionStart hook for Claude Code on the web.
#
# Prepares the container so `go build`, `go vet` and the full `go test ./...`
# (including the Postgres-backed integration tests and benchmarks) work out of
# the box. It downloads Go modules, brings up the local PostgreSQL cluster,
# ensures the broker role/database exist, and exports the connection string.
#
# The harness auto-skips integration tests when DATABASE_URL is unset, so if
# Postgres tooling is missing the hook degrades gracefully: unit tests still run.
#
# Idempotent and non-interactive; safe to run on startup, resume and compact.
set -euo pipefail

# Only run in the remote (web) environment.
if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

log() { echo "[session-start] $*" >&2; }

PROJECT_DIR="${CLAUDE_PROJECT_DIR:-$(pwd)}"
cd "$PROJECT_DIR"

# --- Go modules ---------------------------------------------------------------
# `download` (not `mod tidy`) so the result is cached with the container image.
log "downloading Go modules"
go mod download

# --- PostgreSQL ---------------------------------------------------------------
PG_USER="broker"
PG_PASS="broker"
PG_DB="broker"
PG_DSN="postgres://${PG_USER}:${PG_PASS}@127.0.0.1:5432/${PG_DB}?sslmode=disable"

if command -v pg_ctlcluster >/dev/null 2>&1 && pg_lsclusters >/dev/null 2>&1; then
  log "starting PostgreSQL cluster (if not already up)"
  pg_ctlcluster 16 main start >/dev/null 2>&1 || true

  # Wait for the server to accept connections.
  for _ in $(seq 1 30); do
    if pg_isready -h 127.0.0.1 -p 5432 -q; then break; fi
    sleep 1
  done

  if pg_isready -h 127.0.0.1 -p 5432 -q; then
    log "ensuring role and database exist"
    su postgres -c "psql -tAc \"SELECT 1 FROM pg_roles WHERE rolname='${PG_USER}'\"" \
      | grep -q 1 \
      || su postgres -c "psql -c \"CREATE ROLE ${PG_USER} LOGIN PASSWORD '${PG_PASS}'\""
    su postgres -c "psql -tAc \"SELECT 1 FROM pg_database WHERE datname='${PG_DB}'\"" \
      | grep -q 1 \
      || su postgres -c "psql -c \"CREATE DATABASE ${PG_DB} OWNER ${PG_USER}\""

    # --- Export env for the session -----------------------------------------
    if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
      add_env() { grep -qxF "$1" "$CLAUDE_ENV_FILE" 2>/dev/null || echo "$1" >> "$CLAUDE_ENV_FILE"; }
      add_env "export DATABASE_URL='${PG_DSN}'"
      add_env "export BROKER_TEST_DATABASE_URL='${PG_DSN}'"
      # A dev-only signing key so \`go run ./cmd/broker\` works without extra setup.
      add_env "export JWT_SIGNING_KEY='dev-session-signing-key-please-change-32b'"
      log "exported DATABASE_URL / BROKER_TEST_DATABASE_URL / JWT_SIGNING_KEY"
    fi
    log "PostgreSQL ready at ${PG_DSN}"
  else
    log "PostgreSQL did not come up; integration tests will be skipped"
  fi
else
  log "no PostgreSQL tooling found; integration tests will be skipped"
fi

log "done"
