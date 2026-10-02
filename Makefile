SQLC := go tool -modfile=tools/go.mod sqlc
OAPI := go tool -modfile=tools/go.mod oapi-codegen
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMPOSE := docker compose -f deploy/compose.yaml

# Local database from deploy/compose.yaml.
export ORION_MIGRATE_DATABASE_URL ?= postgres://orion_owner:orion@localhost:5432/orion?sslmode=disable
export ORION_DATABASE_URL ?= postgres://orion_api:orion@localhost:5432/orion?sslmode=disable
export ORION_PLATFORM_DATABASE_URL ?= postgres://orion_admin:orion@localhost:5432/orion?sslmode=disable

.PHONY: help gen lint test build run worker migrate db-up db-down db-reset

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

gen: ## Regenerate code (sqlc, oapi-codegen)
	$(SQLC) generate
	$(OAPI) -config api/oapi-codegen.yaml api/openapi.yaml

lint: ## Run golangci-lint
	golangci-lint run ./...

test: ## Run all tests (needs Docker, or ORION_TEST_DATABASE_URL)
	go test -race -count=1 ./...

build: ## Build bin/orion
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/orion ./cmd/orion

run: build ## Migrate the local database and start the API
	ORION_LOG_FORMAT=text ./bin/orion migrate up
	ORION_LOG_FORMAT=text ./bin/orion serve

worker: build ## Run background jobs (emails are written to the log locally)
	ORION_LOG_FORMAT=text ./bin/orion worker

migrate: build ## Apply migrations to the local database
	./bin/orion migrate up

db-up: ## Start local Postgres
	$(COMPOSE) up -d --wait

db-down: ## Stop local Postgres
	$(COMPOSE) down

db-reset: ## Delete local Postgres data and start fresh
	$(COMPOSE) down -v
	$(COMPOSE) up -d --wait
