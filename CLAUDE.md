# Orion POS backend: notes for Claude

Go modular monolith on PostgreSQL for a point-of-sale system (cafes and restaurants, Indonesia).
Read `README.md` for the layout and `docs/BACKEND_PLAN.md` for the plan, phases and "as built"
sections. Phase 0 and Phase 1 (B1.1 to B1.12) are done; in Phase 2, B2.1 to B2.3 and B2.6 to B2.10 are
done, B2.5 is done except the gateway refund, and B2.4 and B2.11 wait on `docs/OWNER_TODO.md`.
Phase 3 (inventory): B3.1 and B3.2 are done (ADR 0009, plan 6.6.1 and 6.6.2); next is B3.2b (setup per the back office's `docs/BUSINESS_RULES.md`), then B3.3. The back office it serves is
`../Inventory-React` (`src/pages/Setup`, `src/pages/stock-management`).

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

The owner's list, with what each item blocks, is `docs/OWNER_TODO.md` (parked on 2026-10-06 to
focus on features: B2.4 and B2.11 wait on it).

- Hosting is unchosen. It is written up as a guide for the owner to carry out:
  `docs/guides/hosting-and-restore.md` (B0.13 is marked done as a guide; the host, the deploy
  workflow and the first restore drill, with `docs/runbooks/restore.md`, are still to do before the
  pilot). Email uses Resend (`ORION_EMAIL_PROVIDER=resend`); the sending domain's DNS and a real
  delivery test (steps 1, 7 and 8 of `docs/guides/email-provider.md`) are still to do.
- Payment gateway: doit.id (BACKEND_PLAN.md 6.5.1); confirm the merchant can absorb the fee before B2.4.
- Publishing the TypeScript client package (B0.3), CORS headers (none are set), and the
  accountant's sign-off on per-bill half-up rounding before the pilot.
- The pilot runbook is `docs/runbooks/pilot.md`.
