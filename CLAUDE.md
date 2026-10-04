# Orion POS backend: notes for Claude

Go modular monolith on PostgreSQL for a point-of-sale system (cafes and restaurants, Indonesia).
Read `README.md` for the layout and `docs/BACKEND_PLAN.md` for the plan, phases and "as built"
sections. Phase 0 and Phase 1 (B1.1 to B1.12) are done.

## Standing rules

1. **Keep `docs/API_CONTRACT.md` current.** It is the guide the frontend developers and their AI
   agents follow. Whenever you change `api/openapi.yaml`, a sync event payload, a flag or error
   code, the pricing rules, a permission, or anything else a client can see, update
   `docs/API_CONTRACT.md` in the same commit. A task that changes client-visible behaviour is not
   done until the contract says so. `api/openapi.yaml` stays the source of truth for shapes; the
   contract carries the behaviour the spec cannot express.
2. **Work one plan task at a time**: implement, test, add an "as built" note to
   `docs/BACKEND_PLAN.md` and mark the task done, commit, push. Do not open a pull request unless
   asked.
3. Develop on the branch the session names (currently `claude/exciting-newton-od1g6c`); `main`
   receives merges from pull requests.

## Commands

```sh
export ORION_TEST_DATABASE_URL='postgres://postgres:pg@localhost:5432/postgres?sslmode=disable'  # when Docker is not available
go vet ./... && go test -count=1 -p 1 ./...   # -p 1: packages share one template database
make gen        # after editing migrations, queries.sql or api/openapi.yaml (sqlc + oapi-codegen); CI fails on stale output
make load       # the load check
make lint
```

Tests use a real Postgres (testcontainers, or `ORION_TEST_DATABASE_URL`). Never mock the database.

## Architecture rules

- `internal/archtest` enforces module boundaries: a module imports another module's root package
  only (tests may also import `<module>/<module>stub`). Read-only cross-module table reads exist
  only in `reporting`, the `sync` health monitor and `platform` support tooling.
- Every tenant table has row-level security. Tenant data goes through `kernel.TenantTx`; the app
  role `orion_app` cannot bypass it. New tables need RLS, grants and a tenant isolation test
  (`internal/api/isolation_test.go` covers every operation).
- Money is integer rupiah; rates are basis points. Ids are UUIDs (devices generate UUIDv7 events).
- Sales are inserted exactly as the device rang them up; the server recomputes and raises review
  flags, it never rejects a well-formed sale and never edits one. `internal/pricing` is the single
  bill calculation; changing a result is a new `pricing.Version` plus new golden vectors in
  `testdata/pricing-vectors/`.
- Sync: per-event transactions, idempotent by event id and payload hash, out-of-order events park
  as `pending_dependency`. A change to the protocol needs a test first (`internal/sync/sync_test.go`).
- Change log entries are aggregates (a variant change is an item change); pulls return current
  state, filtered per outlet.
- Errors: `application/problem+json` with a stable `code`; clients switch on `code`.
- Every operation needs an access policy (`x-permission` in the spec) or the server panics at start.
- Tests are written to fail when the code is wrong: use mutation checks, N+1 query-count tests and
  tenant isolation checks for new behaviour.

## Open items (not done)

- B0.13 ops and restore drill, publishing the TypeScript client package (B0.3), a real email
  provider (email goes to the log), CORS headers (none are set), and the accountant's sign-off on
  per-bill half-up rounding before the pilot.
- The pilot runbook is `docs/runbooks/pilot.md`.
