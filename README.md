# Orion POS backend

Go API for Orion, a point-of-sale system for cafes and restaurants in Indonesia.

Start with the [backend implementation plan](./docs/BACKEND_PLAN.md), which follows the product
[roadmap](https://github.com/Orion-POS/Inventory-React/blob/dev/docs/ROADMAP.md) and the
[architecture decision records](https://github.com/Orion-POS/Inventory-React/tree/dev/docs/adr).

In short: one Go binary on PostgreSQL (modular monolith), an OpenAPI-first contract, an
offline-first sync protocol for the cashier app, integer-rupiah money and an append-only stock
ledger.

## Getting started

Requirements: Go (version in `go.mod`), Docker, and `golangci-lint` for `make lint`.

```sh
make db-up   # local Postgres
make run     # migrate and serve on :8080
make test    # all tests, against a real Postgres started by testcontainers
make lint
make gen     # regenerate sqlc code after editing migrations or queries
```

See [`deploy/README.md`](./deploy/README.md) for database roles and configuration.

## Layout

| Path | What |
|---|---|
| `cmd/orion` | The single binary: `serve`, `migrate up\|down\|status`, `version` |
| `migrations` | goose SQL migrations, embedded in the binary |
| `internal/kernel` | Shared basics: UUIDv7 ids, integer money, clock, tenant transactions |
| `internal/tenancy` | Tenants, outlets and outlet settings (queries in `queries.sql`, generated into `db/`) |
| `internal/database` | Pools, migrations, the app-role safety check, schema tests |
| `internal/httpserver` | Router, middleware, `/healthz` and `/readyz` |
| `internal/testdb` | Real Postgres for tests: one container, one fresh database per test |
| `internal/archtest` | Enforces module boundaries |
| `tools` | Pinned code generators (`go tool -modfile=tools/go.mod ...`) |
