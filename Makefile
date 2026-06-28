# Developer shortcuts. Integration tests and benchmarks need a Postgres in
# DATABASE_URL (the Claude Code web SessionStart hook sets this up
# automatically; locally use `make db` or `docker compose up -d postgres`).

DATABASE_URL ?= postgres://broker:broker@127.0.0.1:5432/broker?sslmode=disable
export DATABASE_URL

.PHONY: help build run test test-unit bench lint fmt vet tidy db create-initiator

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Build the broker binary
	go build -o bin/broker ./cmd/broker

run: ## Run the server (needs DATABASE_URL + JWT_SIGNING_KEY)
	go run ./cmd/broker

create-initiator: ## Provision an initiator and print its secret
	go run ./cmd/broker create-initiator

test: ## Run all tests with the race detector
	go test -race ./...

test-unit: ## Run only the no-DB unit tests
	go test ./internal/secret/... ./internal/auth/... ./internal/httpapi/...

bench: ## Run the queue throughput benchmarks
	go test -run xxx -bench . -benchmem ./internal/integration/

lint: fmt vet ## Check formatting and run go vet

fmt: ## Report any unformatted files
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi
	@echo "gofmt: ok"

vet: ## Run go vet
	go vet ./...

tidy: ## Tidy go.mod/go.sum
	go mod tidy

db: ## Start a local Postgres via docker-compose
	docker compose up -d postgres
