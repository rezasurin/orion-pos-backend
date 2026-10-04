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
make worker  # background jobs (in a second terminal); emails are written to the log
./bin/orion admin seed-demo   # demo business: owner@demo.orion.test / demo-password-1, cashier PINs, a paired device
make test    # all tests, against a real Postgres started by testcontainers
make lint
make gen     # regenerate sqlc and OpenAPI code after editing migrations, queries or api/openapi.yaml
```

See [`deploy/README.md`](./deploy/README.md) for database roles and configuration.

## Layout

| Path | What |
|---|---|
| `api` | The OpenAPI contract (`openapi.yaml`), the source of truth for the HTTP API |
| `gen/openapi` | Go types and server stubs generated from it (do not edit) |
| `cmd/orion` | The single binary: `serve`, `worker`, `migrate up\|down\|status`, `version` |
| `migrations` | goose SQL migrations, embedded in the binary |
| `internal/kernel` | Shared basics: UUIDv7 ids, integer money, clock, tenant transactions |
| `internal/tenancy` | Tenants, outlets and outlet settings (queries in `queries.sql`, generated into `db/`) |
| `internal/identity` | Users, sessions (JWT access tokens, rotating refresh tokens), email verification, roles, staff and PINs, device pairing and the roster; its background jobs |
| `internal/catalog` | Categories, items with variants, modifier groups, per-outlet prices and availability, with their change log entries |
| `internal/sales` | Shifts, cash, sales, payments, voids and review flags, written only by projecting pushed events |
| `internal/reporting` | End-of-shift and end-of-day reports, computed live from the sales tables |
| `internal/sync` | The device push: inbox, idempotency, projector registry (`syncstub` is a stand-in projector for tests) |
| `internal/pricing` | The one bill calculation, shared by the sale projector and the receipt preview |
| `internal/notify` | Sending email behind an interface |
| `internal/api` | The HTTP layer: implements the OpenAPI operations by calling the modules, and enforces each operation's access rule |
| `internal/database` | Pools, migrations, the app-role safety check, schema tests |
| `internal/httpserver` | Router, middleware, `/healthz` and `/readyz` |
| `internal/testdb` | Real Postgres for tests: one container, one fresh database per test |
| `internal/archtest` | Enforces module boundaries |
| `tools` | Pinned code generators (`go tool -modfile=tools/go.mod ...`) |
