# Orion POS backend: implementation plan

This is the backend plan behind the product roadmap and the architecture records (ADRs). Both
currently live in the back-office repo:

- Roadmap: [`Orion-POS/Inventory-React/docs/ROADMAP.md`](https://github.com/Orion-POS/Inventory-React/blob/dev/docs/ROADMAP.md)
- ADRs: [`docs/adr`](https://github.com/Orion-POS/Inventory-React/tree/dev/docs/adr), referred to
  below as ADR 0002 to ADR 0008

The roadmap says *what* ships in each phase. This document says *how* the Go service delivers it:
the repository layout, the cross-cutting rules every module follows, the data model, the API, and
a task list for each phase with exit criteria.

Estimates are in full-time weeks for one developer, as in the roadmap. They cover backend work only
and run in parallel with front-end work in the same phase, so they do not add up to the roadmap
totals.

---

## 1. Principles

These come from the ADRs and are not re-argued here.

1. **One Go binary, one PostgreSQL database** (ADR 0002). Modules have clear boundaries inside one
   process. There are no microservices and no message broker, and no Redis until one of the
   triggers in section 4.13 applies.
2. **The OpenAPI document is the contract** (ADR 0003). Server stubs and the TypeScript client are
   generated from it, and CI fails when the generated code is stale.
3. **Every business table has `tenant_id` from the first migration**, and `outlet_id` where the
   row belongs to an outlet (ADR 0002).
4. **The POS is offline-first.** The server must accept the same event twice with no effect, and
   sales and payments are append-only (ADR 0004).
5. **Money is integer rupiah, and stock is an append-only ledger** (ADR 0006).
6. **Entitlements are checked in one place** (ADR 0007, ADR 0008). There is no billing code
   before Phase 5.
7. **Operators and tenants are separate**, with separate accounts, token audiences, route
   prefixes and an audit log (ADR 0008).

---

## 2. Tech stack

The ADRs suggest these as defaults. This plan adopts them so the choices are made once.

| Concern | Choice | Notes |
|---|---|---|
| Language | Go, latest stable, pinned in `go.mod` and CI | Upgrade on each minor release |
| HTTP router | `chi` | Plays well with `oapi-codegen` and standard `net/http` middleware |
| Contract | OpenAPI 3.1 + `oapi-codegen` (strict server) | Typed request/response objects per operation |
| DB driver | `pgx` v5 | Pool via `pgxpool` |
| Queries | `sqlc` | SQL in `.sql` files, generated Go in `internal/<module>/db` |
| Migrations | `goose` | SQL migrations, embedded in the binary, run by `orion migrate` |
| Background jobs | `river` | Postgres-backed; same transaction as the business write |
| IDs | UUIDv7 (`github.com/google/uuid`) | Time-ordered; client-generated for synced events |
| Password / PIN hashing | `argon2id` (`golang.org/x/crypto`) | |
| TOTP | `github.com/pquerna/otp` | Operator 2FA |
| Logging | `log/slog`, JSON | Request id, tenant id and actor on every line |
| Errors | Sentry SDK (or self-hosted GlitchTip, same SDK) | |
| Tests | standard `testing` + `testcontainers-go` for Postgres | Real Postgres, no mocks of the DB |
| Lint | `golangci-lint` | `govet`, `staticcheck`, `errcheck`, `gosec`, `errorlint`, `sqlclosecheck`; module boundaries by `internal/archtest` |
| Email | Transactional provider behind an interface (for example Postmark or Resend) | Needed in Phase 0 for owner verification |

---

## 3. Repository layout

```
orion-pos-backend/
├── api/
│   ├── openapi.yaml            # source of truth (split into files under api/paths, api/schemas if large)
│   └── oapi-codegen.yaml
├── cmd/
│   └── orion/                  # single binary: `orion serve | migrate | worker | admin ...`
├── internal/
│   ├── platform/               # operator accounts, admin endpoints, audit log (ADR 0008)
│   ├── tenancy/                # tenants, outlets, signup
│   ├── identity/               # users, staff, roles, permissions, sessions, devices, PINs
│   ├── entitlements/           # plans, flags, overrides, resolution (ADR 0007/0008)
│   ├── catalog/                # items, variants, modifiers, categories, prices, versioning
│   ├── sales/                  # sales, lines, discounts, voids, refunds, shifts, cash drawer
│   ├── payments/               # cash, manual QRIS, gateway QRIS/e-wallets, webhooks
│   ├── inventory/              # ingredients, recipes, UoM, ledger, opname, waste, purchasing
│   ├── sync/                   # push inbox, pull deltas, cursors
│   ├── reporting/              # end-of-shift, end-of-day, sales reports
│   ├── billing/                # Phase 5 only: subscriptions, invoices, promo codes
│   ├── notify/                 # email, in-app announcements
│   └── kernel/                 # shared, dependency-free: money, ids, clock, errors, tenant context, tx helper
├── migrations/                 # goose SQL files, one global sequence
├── gen/                        # oapi-codegen output (checked in, verified in CI)
├── testdata/
│   └── pricing-vectors/        # golden JSON vectors shared with the TS client (section 6.4)
├── deploy/                     # Dockerfile, compose for local dev, infra notes
├── docs/
│   ├── BACKEND_PLAN.md
│   └── runbooks/               # restore, rotate secrets, revoke device, incident
└── Makefile                    # gen, lint, test, migrate, run
```

### Module rules

- Each module exposes a small Go interface (`internal/<module>/service.go`). Other modules import
  only that interface, never another module's `db` package or tables.
- A test in `internal/archtest` enforces this from `go list`, since the compiler will not
  (ADR 0002). It is simpler and stricter than `depguard` rules for this shape of rule.
- Cross-module writes that must be atomic (for example a sale consuming stock) share one
  transaction through `kernel.Tx`, passed in by the caller. The consuming module still writes only
  its own tables.
- Asynchronous reactions go through `river` jobs inserted in the same transaction (a transactional
  outbox, for free).

### Where the OpenAPI document lives

The back office, the POS and the admin console all consume the contract, and the back-office repo
will become a monorepo in Phase 1. Two options:

- **A. The spec lives here** in `api/openapi.yaml`. CI publishes the generated TypeScript types as
  a versioned package (GitHub Packages) or the front-end CI pulls the spec at a pinned commit.
- **B. The backend moves into the front-end monorepo** at the Phase 1 restructure, and the spec
  sits at the root.

**Recommendation: start with A in Phase 0**, and decide on B at the Phase 1 monorepo restructure.
With one developer, B removes the version dance entirely and is probably the better end state.

---

## 4. Cross-cutting design

### 4.1 Multi-tenancy

- `tenant_id uuid not null` on every business table. `outlet_id` where the row is outlet-scoped.
- Composite foreign keys `(tenant_id, id)` so a row can never reference another tenant's row,
  even through a bug.
- **Row-level security as a second line of defence**, on from the first migration:
  - the app connects as a role `orion_app` that does not own the tables;
  - each request transaction runs `SET LOCAL app.tenant_id = '<uuid>'`;
  - every tenant table has a policy `USING (tenant_id = current_setting('app.tenant_id')::uuid)`;
  - the `platform` module and background jobs that span tenants use a separate role
    `orion_platform` with `BYPASSRLS`, only from code under `internal/platform` and named jobs.
- Handlers never take `tenant_id` from the request body. It comes from the authenticated principal.
- A test in CI creates two tenants and asserts that every list endpoint returns nothing from the
  other tenant.

### 4.2 Identifiers

- UUIDv7 primary keys everywhere. Synced records (sales, payments, shift events, stock events
  from the POS) use the **client-generated** id.
- Human-readable receipt numbers per ADR 0004: `{outlet_code}-{device_code}-{counter}`, for
  example `JKT1-03-000482`. The server stores the string and its parts, with a unique constraint
  on `(tenant_id, outlet_id, device_code, counter)`. Gaps are allowed and numbers are never reused.
- `device_code` is allocated by the server at pairing, unique per outlet, never reused (so a
  re-paired, wiped device gets a new one).

### 4.3 Authentication and principals

There are four kinds of principal. Each has its own token audience, and middleware rejects a
token on routes for another audience.

| Principal | How it signs in | Token | Routes |
|---|---|---|---|
| **Owner/manager user** | Email + password (argon2id), email verified | Access token (JWT, 15 min, `aud=tenant`) + rotating refresh token (opaque, hashed in DB, 30 days) | `/v1/...` |
| **Device** | Paired once by a signed-in owner/manager | Long-lived opaque device secret (hashed in DB, revocable), exchanged for short-lived access JWTs (`aud=device`) | `/v1/pos/...`, `/v1/sync/...` |
| **Cashier on a device** | PIN on the device, offline | No server token. Events carry `staff_id`; the server checks the staff member belongs to the device's outlet and was active at event time | — |
| **Operator** | Email + password + **required TOTP** | JWT (`aud=operator`, 15 min) + refresh; separate signing key | `/admin/...` |

Notes:

- **Device pairing** (ADR 0004): the owner signs in on the tablet, picks an outlet, and the server
  creates a `device` row with a `device_code` and returns the device secret once. Revocation sets
  `revoked_at`; the next token exchange fails, and the POS shows "device revoked" (it can still
  sell until it next goes online, which ADR 0004 accepts).
- **Cashier PINs**: the server stores an argon2id hash of each staff PIN and ships the hashes for
  the outlet's staff to paired devices in the pull payload. A 4–6 digit PIN hash can be brute-forced
  by anyone holding the device's storage, so **the PIN is a convenience switch, not a security
  boundary**: it controls who is recorded as acting, and permissions (void, refund, open drawer)
  are enforced by role. Mitigations: hashes go only to paired, non-revoked devices; the device
  locks out after repeated failures; a manager can rotate a PIN, which reaches devices at the next
  pull. This trade-off should be written up as an ADR.
- Signing keys: one per audience, from environment/secret store, with a `kid` header so they can
  be rotated.
- Rate limits (in-process token bucket keyed by IP and account, good enough for one instance;
  see section 4.13 for when they move to Redis):
  login, pairing, PIN-rotation, signup, promo-code redemption.

#### 4.3.1 Sessions as built (B0.6)

- **Tokens carry their tenant.** Refresh tokens, verification links and device secrets are
  `<prefix>.<tenant id>[.<id>].<256-bit secret>`, and only the SHA-256 of the secret is stored.
  A lookup therefore runs inside that tenant's row-level security like any other query, with no
  cross-tenant read to find the row first.
- **The one pre-tenant lookup is login.** Finding an account by email happens before a tenant is
  known, so it goes through `auth_find_user(email)`, a `SECURITY DEFINER` function that returns one
  row. Everything else stays under row-level security: `user_account` is visible to `orion_app`
  only while the user is a member of the current tenant, and `orion_app` has no `SELECT` on
  `password_hash` at all. Unknown email and wrong password cost the same (a dummy hash is checked)
  and give the same error.
- **Accounts may belong to several tenants.** Login takes an optional `tenant_id`; with several
  memberships and none given, it answers `409 tenant_required` listing the ids. Names are not
  listed yet because that would mean reading tenants before one is chosen.
- **Email must be verified to sign in.** Accounts made by an operator, a seed or a signup that
  already confirmed the address are created verified.
- **Rotation and reuse.** A refresh token works once. Presenting a used one revokes its whole
  family (`token_reused`) and commits that before reporting it. There is no grace window for a
  lost response, so a client that retries a refresh after a network failure must be prepared to
  sign in again; add a short grace period if the pilot shows this hurting.
- **Every request to a user route reloads membership** (one indexed query) and checks the tenant
  is not suspended (one primary-key read), so removing a member or suspending a business takes
  effect at once instead of when the access token expires. Both are cache candidates (section
  4.13) when measured, not before.
- **Jobs carry ids, never secrets.** The verification job holds `tenant_id` and `user_id`; the
  worker mints the token, stores its hash and sends the email. `river_job` is readable by
  `orion_app` (an insert returns the row), so nothing in it may be a credential.
- **The worker runs as `orion_platform`**, because jobs such as the token purge span tenants.
- Rate limits, per process: login 20 per address (refill 1 per 3 s) and 5 per account (refill 1
  per minute); token exchange 30 per address (1 per s); verification email 5 per address and 3
  per account.
- Differences from the table in 6.1: emails are stored lowercase with a `CHECK` instead of
  `citext`; `refresh_token` is tenant-scoped (operators get their own table in B0.10); the OpenAPI
  document is 3.0.3 because the code generator supports it fully; `POST
  /v1/auth/resend-verification` was added so an unverified owner is never stuck.

#### 4.3.2 Device pairing as built (B0.8)

- **Pair.** A user with `device.manage` at an outlet calls `POST /v1/devices/pair`. The response
  carries the secret once, as `dk1.<tenant id>.<device id>.<256-bit secret>`; only its SHA-256 is
  stored, so a lost secret means pairing again. Pairing, like every limit check, runs after
  `LockTenant` (the check itself arrives with B0.9).
- **Device codes** come from a per-outlet counter (`device_code_counter`, an upsert), not
  `max()+1`, so revoking the newest device never frees its code. Receipts show it with at least two
  digits, `{outlet}-{code:02d}-{counter}`.
- **Exchange.** `POST /v1/devices/token` trades the secret for a 30 minute access token
  (`aud=device`, claims `tid`, `did`, `oid`) signed with the device key. It also records the app
  version and the clock skew (server minus device) for the "stopped syncing" support view.
- **Revocation is immediate.** Every request to a device route reloads the device (one primary-key
  read, with `last_seen_at` refreshed at most once a minute), so a revoked device fails with
  `device_revoked` while its token is still valid, and cannot get a new one. This is stricter than
  ADR 0004 requires; the offline tablet itself can still sell until it reconnects.
- **Roster.** `GET /v1/pos/roster` returns the device, its outlet with settings, and the active
  staff assigned to that outlet plus owners, each with their PIN hash (null until set) and their
  permissions at that outlet. Two queries however many people there are. The sync pull (Phase 1)
  will deliver later changes to the same data; the roster is the first load.
- Device access tokens cannot call user routes and user tokens cannot call device routes: they use
  different keys and audiences.
- Revoking and pairing are written to `tenant_audit_log`; a token exchange is not (too frequent).

#### 4.3.3 Operators as built (B0.10)

- **Separate world.** `operator`, `operator_recovery_code` and `platform_audit_log` are readable only
  through `orion_platform`; `orion_app` has no privilege on them (a schema test checks every
  privilege). Operator tokens use `aud=operator` and their own signing keys
  (`ORION_JWT_OPERATOR_KEYS`), so no tenant or device token can be one.
- **Two steps, always.** `POST /admin/auth/login` checks the password and returns a 5 minute
  challenge, which is not a session. `POST /admin/auth/totp/verify` takes a TOTP code or a recovery
  code and returns a 1 hour session. Unknown operator, wrong password, wrong code and disabled
  account give the same `invalid_credentials`. There is no refresh token: an operator signs in
  again each hour, which is acceptable for a handful of people (add one if it hurts).
- **Codes work once.** The last accepted TOTP time step is stored, under a row lock, so a code
  cannot be replayed within its window and two simultaneous uses of one code produce one session
  (tested). Each challenge allows five guesses, and each address and account are limited too.
- **Enrolment** happens through the CLI: `orion admin create-operator` prints a generated password,
  the TOTP secret and URI, and ten recovery codes, once. Recovery codes are 80-bit random values
  stored as SHA-256. The first successful code confirms enrolment. TOTP seeds are encrypted with
  AES-256-GCM (`kernel.Box`, key `ORION_SECRETS_KEY`, bound to the operator's id) because they must
  be read back. **Losing that key locks every operator out**, so it is backed up separately from the
  database, in the secret store.
- **Audit.** Every operator action writes `platform_audit_log` in the same transaction as the
  change, with before and after state, the operator, the address, the user agent and a required
  reason; sign-ins, enrolment and recovery-code use are logged too. The first operator is created
  with no actor (there is nobody to attribute it to); every later one needs `--operator`.
  `GET /admin/audit-log` pages newest first.
- **Console off by default.** `orion serve` mounts `/admin` working only when
  `ORION_PLATFORM_DATABASE_URL` is set; otherwise those routes answer `503 admin_disabled`.

### 4.4 Roles and permissions

ADR 0002 requires roles from the start.

- Permissions are fixed strings in code: `catalog.manage`, `sale.void`, `sale.refund`,
  `drawer.open_no_sale`, `shift.close`, `discount.apply_manual`, `report.view`,
  `staff.manage`, `device.manage`, `settings.manage`, `inventory.manage`, and so on.
- Roles are per tenant: `role(id, tenant_id, name, is_system)` and `role_permission`. Seed system
  roles at signup: Owner, Manager, Cashier, Kitchen.
- Staff are assigned roles per outlet (`staff_outlet_role`), ready for multi-outlet.
- The device receives the roster with each person's permissions so checks work offline. The server
  **re-checks on push**: a void by someone without `sale.void` at event time is accepted (the money
  already moved) but flagged for review, not silently dropped.

#### 4.4.1 Roles, staff and PINs as built (B0.7)

- Fourteen permissions live in `internal/identity/permissions.go`; the HTTP layer reads each
  operation's `x-permission` from the OpenAPI document and refuses to start if one is not in that
  list. System roles are seeded with the tenant: Owner (all), Manager (all but `settings.manage`),
  Cashier (`sale.create`, `shift.open`, `shift.close`), Kitchen (`kitchen.view`).
- **Owners are members with `is_owner`, not a role.** They may do anything, so an owner never
  depends on role rows. Everyone else gets permissions from their staff record's role assignments
  per outlet (`staff_outlet_role`); an inactive staff record holds none.
- **No privilege escalation.** To assign a role the caller needs `staff.manage` at that outlet and
  must hold every permission of the role there; a manager cannot make an Owner. Only an owner
  can change an owner's record or PIN.
- **A staff record is for anyone who acts**, signed in by email or not. Members created with
  `CreateMember` get one automatically; cashiers are created with `POST /v1/staff` and have no
  email. Removing a membership keeps the staff row (sales will point at it) and clears `user_id`.
- **PINs.** 4 to 6 digits, no repeats or runs. Stored as argon2id (m=19 MiB, t=2, p=1), cheap on
  purpose because tablets check them offline, often in WebAssembly. The cashier picks their name
  and then types the PIN, so PINs need not be unique and the server never has to compare them. As
  section 4.3 says, a PIN is a convenience switch, not a security boundary: anyone who holds a
  device's storage can brute-force 10^4 to 10^6 values whatever the cost. What bounds the damage
  is that hashes go only to paired, non-revoked devices, that permissions are enforced by role on
  push, and that a manager can rotate a PIN. This is the text for the ADR the plan asks for.
- Staff changes go to `change_log` with `outlet_id` null (every outlet); the pull will filter by
  assignment. PIN changes, staff creation and updates, and new members are written to
  `tenant_audit_log` in the same transaction, with the acting user and address and never the PIN.
- Bulk inserts use `INSERT ... SELECT unnest(...)`, not `COPY`: Postgres refuses `COPY FROM` into
  tables with row-level security.
- Staff lists page by id and load their outlet roles with one extra query, whatever the page
  size; a test counts queries for 1 and for 50 rows and requires them to be equal.

### 4.5 Entitlements and feature flags

One module, one function, used by handlers, jobs and the POS payload (ADR 0007, ADR 0008).

```go
type Resolver interface {
    // Effective value for one key, resolved global default -> plan -> tenant override.
    Get(ctx context.Context, tenantID uuid.UUID, key Key) (Value, error)
    // Full snapshot for a tenant, for the POS cache and the back-office boot payload.
    Snapshot(ctx context.Context, tenantID uuid.UUID) (Snapshot, error)
}
```

Tables:

- `plan(id, code, name, is_public, created_at)`, with seeded `early_access`.
- `entitlement_key(key, kind ['bool'|'int'], description, owner, is_temporary, default_value)`.
  Both commercial modules (`module.inventory`, `limit.outlets`, `limit.devices`, `limit.staff`)
  and technical flags (`flag.sync_v2`) live here, separated by a `category` column.
- `plan_entitlement(plan_id, key, value)`.
- `tenant_entitlement_override(tenant_id, key, value, reason, expires_at, set_by_operator_id)`.

Rules:

- Turning a module off **hides** it: endpoints return `403 module_disabled`, data stays.
- Limits are checked at the point of creation (new outlet, pairing a device, adding staff), inside
  the same transaction, after `kernel.LockTenant` (`SELECT ... FOR NO KEY UPDATE` on the tenant
  row) to avoid races. See section 4.12 for the lock order.
- The snapshot sent to the POS carries `expires_at` (for example 7 days). The POS applies it at the
  next sync and never mid-shift.
- In-process cache with a 30-second TTL, invalidated on write. One instance means no distributed
  cache problem.

#### 4.5.1 Entitlements as built (B0.9)

- Values are integers: 0 or 1 for a bool, a count for a limit, `-1` for unlimited. Five keys are
  seeded (`module.inventory`, `module.restaurant`, `limit.outlets`, `limit.devices`,
  `limit.staff`); early access turns every module on and every limit off. Adding a key is a
  migration.
- Snapshots are cached in process for 30 s. Overrides are written by operator commands in another
  process, so the TTL, not invalidation, bounds staleness; that is acceptable for modules and flags.
- **Limits are never read from the cache.** `Resolver.CheckLimit` reads inside the creating
  transaction, after `LockTenant`, with the current count, so an override applies at once and two
  requests cannot both take the last slot (a test races ten tablets for three slots).
  `limit.devices` counts devices that are not revoked, `limit.staff` active staff records;
  `limit.outlets` is checked when outlet creation arrives with signup (Phase 2).
- `403 limit_reached` and `403 module_disabled` are the error codes. `RequireModule` is ready for
  the first module-gated endpoint (Phase 3).
- The resolver reads the tenant's `plan_id` straight from the `tenant` row, the one place a module
  reads another module's table: going through tenancy would make an import cycle, and the read is
  one column.

### 4.6 Tenant plan fields (Phase 0, first migration)

On `tenant`, per ADR 0007:

```
plan_id                 -> plan.id         (early_access for everyone)
subscription_status     enum: early_access | trialing | active | past_due | grace | read_only | canceled
trial_ends_at           timestamptz null
paid_until              timestamptz null
billing_starts_at       timestamptz null   -- per-tenant override of the global date (ADR 0008)
suspended_at            timestamptz null   -- operator suspension, separate from billing status
```

No billing code reads these until Phase 5, but the read-only middleware (section 9, Phase 5) has a
place to hook in.

### 4.7 Audit logs

Two separate append-only logs:

- `platform_audit_log`: every operator action (ADR 0008): `operator_id`, `action`,
  `target_type`, `target_id`, `tenant_id null`, `before jsonb`, `after jsonb`, `reason text not
  null`, `ip`, `user_agent`, `created_at`.
- `tenant_audit_log`: sensitive actions inside a tenant: role changes, PIN rotation, device
  pairing and revocation, price changes, settings changes, voids and refunds (as references).

Append-only is enforced in the database: `REVOKE UPDATE, DELETE` from the app roles, plus a
trigger that raises on update or delete as a backstop.

### 4.8 Money, tax and rounding

- `bigint` rupiah everywhere in the database; `int64` in Go. A `kernel/money` package with only
  integer operations and explicit rounding modes. No `float64` in any money path, enforced by a
  lint rule on the `money` and `sales` packages.
- Percentages are stored as **basis points** (`int`, 1000 = 10.00%).
- Outlet settings (`outlet_settings`):
  - `price_includes_tax bool`
  - `tax_rate_bp int` (PBJT, set per local government, up to 10%)
  - `service_charge_rate_bp int`
  - `service_charge_taxable bool`
  - `cash_rounding_unit int` (for example 100, 500, 1000; 0 = off)
  - `cash_rounding_mode` (`nearest` | `down` | `up`)
  - `timezone` (`Asia/Jakarta` | `Asia/Makassar` | `Asia/Jayapura`)
  - `business_day_cutoff` (`time`, default `00:00`)
- **The bill calculation is one written algorithm**, implemented in Go and TypeScript, and both
  must pass the same golden vectors in `testdata/pricing-vectors/*.json` (section 6.4). The order:
  1. line amount = unit price + modifier prices, times quantity
  2. line discounts
  3. bill discount, allocated across lines pro rata with largest-remainder allocation, so the parts
     add up exactly
  4. service charge on the discounted subtotal
  5. tax on the discounted subtotal (+ service charge if taxable); for tax-inclusive prices, tax is
     extracted rather than added
  6. cash rounding applies **only to the cash tender**, recorded as a separate `rounding_amount`
     so totals still reconcile
- Rounding of tax and service charge: round half up, per bill (not per line). **Confirm with an
  accountant before Phase 1 ships** (roadmap, Indonesia-specific requirements). The algorithm is
  versioned (`pricing_version` on each sale) so a later correction does not reinterpret old sales.

#### 4.8.1 Pricing as built (B1.2)

- `internal/pricing` is a pure package (no database, no clock) with one function, `Calculate`,
  used by the sale projector (B1.5) to recompute every sale a device sends, and already by the
  receipt test endpoint, so there is a single implementation. `pricing.Version` (1) is stored on
  each sale as `pricing_version`.
- **Discounts.** At most one discount per line and one bill discount, as percent (basis points)
  or amount (rupiah). A fixed discount may not exceed what it applies to, a percent is 0 to
  100%, and a bill discount works on the lines *after* their own discounts. More than one
  discount on a target is an invalid bill, which the projector accepts and flags like any other
  total it cannot reproduce.
- **Tender decides rounding.** Cash rounding applies only when every payment is cash (`cash`);
  `non_cash` and `mixed` bills are not rounded. `rounding_amount` is separate from `total`.
- **Inclusive prices.** The tax is extracted from the discounted subtotal plus the service charge
  when that is taxable, and the total is subtotal plus service charge; this matches the
  receipt-test preview that existed before.
- **Limits** keep the sums inside `int64`: 200 lines, 50 modifiers per line, quantity up to
  10,000, unit prices and modifier deltas up to 1,000,000,000. Beyond them the bill is invalid
  (`too_large`, `invalid_price`, ...).
- **Vectors.** `testdata/pricing-vectors` holds 51 files. The expected numbers came from an
  independent calculation in exact rational arithmetic, and the Go test fails if a vector has a
  field Go does not read. A second test prices 3,000 random bills and checks that lines, discounts,
  service charge, tax, total and rounding always reconcile.
- Still open from section 4.8: the **accountant's confirmation** of per-bill half-up rounding for
  tax and service charge must happen before the pilot sells anything.

### 4.9 Time and the reporting day

- All timestamps are `timestamptz` in UTC.
- Synced events store both `device_time` and `received_at` (server). Ordering and reporting use
  `received_at` for sequencing and `device_time` for "when it happened"; clock skew beyond a
  threshold (for example 10 minutes) is recorded on the device row for support.
- `business_date date` is computed **once**, when the sale is projected, from `device_time` in the
  outlet's timezone and cutoff, and stored on the sale and shift. Reports group by it. Changing
  an outlet's timezone does not rewrite history.

#### 4.9.1 Settings and the business date as built (B1.7)

- `PATCH /v1/outlets/{outletId}/settings` needs `settings.manage` *at that outlet* (an outlet of
  another business is `404`). Fields left out stay as they are. A real change is recorded in the
  change log for that outlet only (`outlet_settings`, so only its devices pull it) and in the audit
  log as `settings.updated` with the old and new value of each field; an update that changes
  nothing writes nothing. `business_day_cutoff` is `HH:MM`, 00:00 to 11:59 in the outlet's time
  zone, and is part of the outlet's settings in every response (so the roster carries it).
- `kernel.BusinessDate(instant, zone, cutoff)` is the calendar date of the instant in the outlet's
  zone after subtracting the cutoff: with a 04:00 cutoff, 02:30 belongs to the previous day and
  04:00 starts the new one. `tenancy.OutletSettings.BusinessDate` applies it with the outlet's own
  zone and cutoff. It is called once per sale, with the **device's** `device_time` (never
  `received_at`), when the sale is projected, and stored. Changing the zone or cutoff later never
  moves history.
- The three outlet time zones are fixed offsets (WIB +7, WITA +8, WIT +9). Indonesia has no daylight
  saving time, so this is exact and needs no tzdata in the binary.
- A device whose clock is wrong still gets a self-consistent date (its own clock, in the outlet's
  zone); the skew is recorded for support (5.1.1) but not corrected. If the pilot shows tablets
  with badly wrong clocks, correcting `device_time` by the skew measured on the same push is the
  change to make, as a new `pricing`-style versioned rule.

### 4.10 Errors and API conventions

- Errors use `application/problem+json` (RFC 9457) with a stable `code` field
  (`validation_failed`, `module_disabled`, `limit_reached`, `not_found`, `conflict`,
  `tenant_read_only`, `device_revoked`, ...). The front end switches on `code`, never on text.
- List endpoints use cursor pagination (`?cursor=&limit=`), with UUIDv7 ordering as the cursor.
- Mutating endpoints from the back office accept an `Idempotency-Key` header; the key and response
  are stored for 24 hours (`idempotency_key` table). Sync events have their own idempotency
  (section 5).
- API versioning by path prefix (`/v1`). Additive changes only within a version.
- `Accept-Language` (`id-ID` default, `en`) for any server-generated text (emails, error `detail`).
  Error `code`s are language-neutral.

### 4.11 Observability and operations

- Structured logs with `request_id`, `tenant_id`, `principal_type`, `principal_id`, `device_id`.
  Never log PINs, passwords, tokens, or customer personal data.
- Sentry for panics and 5xx.
- `/healthz` (process up) and `/readyz` (DB reachable, migrations at expected version).
- Minimal metrics that matter: sync push latency and error rate, events per push, jobs failed,
  webhook failures. A Prometheus `/metrics` endpoint behind auth, or log-based metrics if the host
  makes that simpler.
- Per-device `last_seen_at`, `last_sync_at`, `app_version`, `clock_skew_ms` for support and the
  admin "stopped syncing" view.

#### 4.10.1 CORS as built

`internal/httpserver/cors.go`: an exact-match allowlist from `ORION_CORS_ALLOWED_ORIGINS` (default
none; the origin of `ORION_PUBLIC_URL` when `ORION_ENV=local`). No wildcard (a bad value fails
startup), no credentials (the API authenticates with bearer tokens, never cookies). A preflight is
answered with 204 before routing and authentication; real responses, errors included, carry
`Access-Control-Allow-Origin` and expose `Retry-After`. `Vary: Origin` is set whenever a request has
an `Origin`, so a cache never serves one origin's answer to another. Tests cover exact matching
(scheme, port, prefix, subdomain, `null`), the preflight through the real API for the login, sync,
reports and admin routes, and the config parser.

#### 4.11.1 Device health as built (B1.10)

- **What the server knows.** Every push and pull stamps the device row: `last_sync_at` and
  `last_seen_at`, `app_version`, `clock_skew_ms` (server minus the device's `client_time`), and the
  outbox the device reports: `unsynced_events` and `oldest_unsynced_at` (device time of the oldest),
  with `health_reported_at`. `GET /v1/devices` returns all of it. A report that makes no sense (a
  negative count, a count without an oldest time) is ignored and never fails the sync it came with.
  The server cannot see a tablet's outbox, so this is how it learns that events are stuck; the POS
  must send `unsynced_events` (0 when the outbox is empty) on each call.
- **The monitor** is a river periodic job in `orion worker` (every 5 minutes, as `orion_platform`,
  `sync.HealthJobs`). It opens an incident (`device_alert`, at most one open per device and kind)
  for each device that is not revoked, whose business is not suspended, and that has been seen in
  the last 7 days, when:
  - `unsynced_events`: the device reported events whose oldest is older than
    `ORION_ALERT_UNSYNCED_AFTER` (default 30 minutes); or
  - `silent_open_shift`: it has a shift that arrived in the last 24 hours and is still open, and
    has not been heard from for `ORION_ALERT_SILENT_AFTER` (default 3 hours).
  A device that goes offline holding old events stays flagged, which is the case the alert exists for.
- **Telling people.** Each incident emails the business's owners with a verified address, once, in
  their language (Indonesian by default), saying what is wrong and what to do; a copy goes to
  `ORION_ALERT_OPERATOR_EMAIL` when set, naming the business. An email that fails is retried on the
  next run; an incident with nobody to tell is logged and marked handled. Incidents resolve
  themselves when the device delivers, speaks up, is revoked, or closes its shift, with no
  message. With `ORION_EMAIL_PROVIDER=log` (local) the worker writes these
  emails to its log; staging and production send through Resend.
- **Shape.** The monitor reads the `device` and `shift` tables read-only (with `device_alert`, its
  own table); migration 00013 adds the columns, the table, and a partial index of open shifts.

### 4.12 Database conventions: indexes, N+1 queries and deadlocks

**Indexes are added when a query needs one**, not for every column or foreign key. Each index is
justified by a real access path (a list endpoint, the sync pull, a report, a hot FK check), and
the reason is written next to it in the migration. In Postgres, primary keys and UNIQUE
constraints already create indexes, so most tenant-scoped lookups are covered by putting
`tenant_id` first in those constraints. Before an endpoint that lists or aggregates ships, run
`EXPLAIN (ANALYZE, BUFFERS)` on it against realistic data (for example the seeded demo tenant
scaled up) and add an index only if the plan shows a scan that grows with the table. Likely
candidates later: `sale (tenant_id, outlet_id, business_date)` for reports,
`stock_movement (tenant_id, outlet_id, ingredient_id, occurred_at)` for movement history,
partial indexes such as `payment_intent (status) WHERE status = 'pending'` for jobs.

**No N+1 queries.**

- A list endpoint makes a fixed number of queries whatever the page size: one query with JOINs
  for one-to-one data (outlets with their settings), and one batched query per child collection
  (`WHERE sale_id = ANY($1::uuid[])`) assembled in Go for one-to-many data (sales with lines).
- Loops that write many rows (sale lines, ledger rows, imported catalog rows) use
  `pgx.Batch` or a single `INSERT ... SELECT unnest($1::uuid[], ...)`, not one round trip per
  row. `COPY` is not an option on tenant tables: Postgres refuses `COPY FROM` into tables with
  row-level security.
- The pull endpoint loads each entity type for all changed ids in one query.
- The check: tests for list endpoints assert the query count stays the same with 1 row and
  50 rows, using the counting `pgx` tracer in `testdb` (`DB.Queries`). It arrived with the first
  list that has children, the staff list.

**Deadlocks.**

- **Lock order:** a transaction that records a change (`record_change`) or checks a per-tenant
  limit calls `kernel.LockTenant` first, before it locks or updates any other row. Every writer
  for a tenant then queues on the same first lock, so no two transactions can each hold a row the
  other needs. After that, rows of one table are locked in id order (`ORDER BY id FOR UPDATE`),
  and tables in a fixed order: tenant, outlet, then the module's own rows.
- `FOR NO KEY UPDATE` instead of `FOR UPDATE` on the tenant row, so inserts whose foreign keys
  reference the tenant are never blocked by it.
- Sync push holds no tenant-wide lock: each event is its own short transaction, and sales are
  inserts. The stock balance upsert (Phase 3) touches one row per ingredient, so the projector
  sorts a sale's ingredients by id before writing them.
- **Short transactions:** no network calls inside a transaction. Gateway calls, emails and
  other side effects go through jobs inserted in the same transaction.
- **Bounded waits:** `kernel.TenantTx` sets `lock_timeout = 5s` per transaction, so a stuck
  lock fails fast instead of exhausting the pool.
- **Retry:** `kernel.TenantTx` reruns the transaction up to three times on a deadlock (`40P01`) or
  serialization failure (`40001`), which is why the function it runs must have no side effects
  outside the database.
- A test runs 20 concurrent writers on one tenant and checks that change numbers stay gapless
  with no deadlock (`internal/tenancy`).

### 4.13 Queueing and caching

**Queueing: yes, in PostgreSQL, with `river`** (ADR 0002). Jobs are inserted in the same
transaction as the business write, so a job exists if and only if the write committed. A
separate broker (Redis, RabbitMQ, SQS) cannot give that without an outbox table anyway. `river`
arrives with the first real job, the verification email in B0.6, and `orion worker` starts then.
Section 8 lists the jobs by phase. The expected load (a few thousand jobs per day per hundred
tenants) is far inside what a Postgres queue handles.

**Caching: in-process first, Redis only when there is a concrete reason.** With one API
instance:

| What | Cache | Why it is enough |
|---|---|---|
| Entitlements snapshot (4.5) | In-process, 30 s TTL, invalidated on write | Read on most requests, changes rarely |
| JWT signing keys, plan list | In memory, loaded at start | Tiny and static |
| Rate limits (4.3) | In-process token buckets | One instance sees every request |
| Sync pull responses | None | The `change_log` cursor already makes a pull cheap |
| Reports | None at first; a rollup table (`end_of_day_rollup` job) if `EXPLAIN` shows they are slow | A summary table in Postgres beats a cache that can go stale |

Every cache sits behind a small interface (`Get`, `Set`, `Invalidate`), so switching one to
Redis is a local change.

**Add Redis when one of these is true:**

- the API runs as **more than one instance**, so rate limits and cache invalidation must be
  shared (rate limits go first, since per-instance limits multiply);
- the Phase 4 open-bill fan-out (SSE) spans several instances. Postgres `LISTEN/NOTIFY` should
  be tried first;
- profiling shows a hot read that a Postgres index or a summary table cannot fix.

Until then Redis would be one more thing to deploy, secure, back up and monitor for one
developer, with no measurable gain.

---

## 5. Sync protocol (the most important code)

ADR 0004 calls the sync protocol and its tests the most important code in the system. It gets its
own design and is built test-first.

### 5.1 Push: device to server

```
POST /v1/sync/push
Authorization: Bearer <device access token>

{
  "device_id": "...",
  "events": [
    {
      "id": "0192...",                    // UUIDv7, client-generated, is the record id
      "idempotency_key": "0192...",       // equal to id for create events
      "type": "sale.completed",
      "staff_id": "...",
      "device_time": "2026-10-02T09:14:03+07:00",
      "schema_version": 1,
      "payload": { ... }
    }
  ]
}
```

Event types for Phase 1: `shift.opened`, `shift.closed`, `cash.movement` (pay in, pay out,
no-sale drawer open), `sale.completed` (with lines, discounts and payments), `sale.voided`.
Phase 2 adds `refund.issued` and `payment.gateway_confirmed`. Phase 3 adds stock events from the
device if the POS ever records waste.

Server behaviour:

1. Authenticate the device. A revoked device gets `401 device_revoked`, and nothing is accepted.
2. For each event, in order, in **its own transaction** (one bad event must not block the rest):
   1. `INSERT INTO sync_inbox (tenant_id, device_id, idempotency_key, ...) ON CONFLICT DO NOTHING`.
   2. If the row already existed, return the **stored result** (`accepted` or `rejected`) without
      re-applying. This is what makes retries safe.
   3. Otherwise validate and **project** the event into the module's tables (`sales`, `shifts`,
      ...) and, from Phase 3, stock ledger rows. Store the outcome on the inbox row.
3. Respond with a result per event:

```
{ "results": [ { "id": "...", "status": "accepted" | "duplicate" | "rejected", "code": "...", "detail": "..." } ],
  "server_time": "..." }
```

Rules:

- **Reject only what is malformed**: unknown type, failed schema validation, wrong tenant/outlet,
  unknown staff. A sale whose totals look wrong, made by someone without the permission, or
  against a price that has since changed is **accepted and flagged** (`sale_flag` table), because
  the money has already changed hands. Rejected events stay on the device for support.
- **Ordering**: a `sale.voided` arriving before its `sale.completed` (possible after reinstalls or
  partial pushes) is parked as `pending_dependency` and retried when the sale arrives. The device
  pushes in outbox order, so this should be rare.
- Payload size limit per push (for example 500 events or 1 MB); the device pages.
- Events are immutable. The same `idempotency_key` with a **different payload hash** is rejected
  with `idempotency_conflict` and logged loudly, since it means a client bug.

#### 5.1.1 Push as built (B1.3 and B1.4)

- **Code.** `internal/sync` holds the push (`Service.Push`), the `Projector` interface the event
  types plug into, and `sync_inbox` (migration 00010). `POST /v1/sync/push` is the only endpoint;
  its `payload` is kept as the raw JSON the device sent. The server accepts every event type the
  registered projectors handle, and none until B1.5 registers the real ones (a push then answers
  `rejected unknown_type` for everything, which is the safe default).
- **Per event, one transaction** (`kernel.TenantTx`, so a deadlock reruns it): share-lock the
  device row (a revocation waits for events in flight, and nothing commits after it returns), claim
  the key with `INSERT ... ON CONFLICT DO NOTHING`, run the projector inside a savepoint (rolled
  back unless the event was applied, so a rejected or parked event leaves nothing), store the
  outcome, and release any parked event waiting for what this one created.
- **Answers.** `accepted`; `duplicate` (already accepted: a resend changes nothing, and resends
  after the first all answer alike); `rejected` with a `code` (kept in the inbox, so a resend gets
  the same answer and support can read it); `retry` (an internal failure: nothing was recorded, the
  device keeps the event). A parked event answers `accepted` with `code: pending_dependency`. The
  plan's list had no `retry`; without it a server fault would have had to fail the whole push, so
  one poison event could block every event behind it.
- **Revoked device.** At the start of a push: `403 device_revoked`, nothing accepted (the existing
  code is 403, not the 401 written in 5.1). Part way through: events after the revocation are
  answered `rejected device_revoked` and not stored.
- **What the core rejects:** `malformed` (not storable: missing ids, staff or time, a bad type or
  schema version, a payload that is not an object; not stored), `unknown_type`,
  `unsupported_schema_version`, `unknown_staff`, `wrong_outlet` (a payload `outlet_id` that is not the
  device's), `idempotency_conflict`, `id_conflict` (an event id another device used first; event ids
  are unique per business). A projector adds its own codes. A database error that says "this data
  cannot be stored" (a foreign key, a unique constraint, a check, a value out of range, a row of
  another business that row-level security hides) becomes a rejection too (`unknown_reference`,
  `duplicate`, `invalid_value`), which is how a device referring to another business's ids is
  turned away. Anything else is a `retry`.
- **Not rejected:** a deactivated cashier's event is accepted (`identity.Acting.Has` is false for
  them, so a projector flags it). There is no staff history, so a cashier deactivated *after* a
  sale still gets that late sale flagged `permission_missing`; the flag is for review, not an
  error.
- **Content hash.** `payload_hash` is SHA-256 over the type, staff, schema version, device time and
  the payload re-encoded with sorted keys and numbers as written, so spacing and key order are not
  content. The same key with a different hash is rejected and logged at error level.
- **Skew and bookkeeping.** A push may carry `client_time` and `app_version`; after the batch the
  device row gets `last_sync_at`, `last_seen_at`, `app_version` and `clock_skew_ms` (server minus
  device). `device_time` and `received_at` are stored as they are; nothing is corrected from the
  server clock. `business_date` comes from `device_time` when the sale is projected (4.9).
- **Dependencies.** A parked event is released by any applied event whose projector lists the
  awaited id in `Applied(ids...)`, inside that event's transaction, oldest first, up to 1,000 per
  event. Nothing expires a parked event yet; B1.10 alerts on events that stay unsynced or parked
  too long.
- **Tests** (all against a real database and the stub projector, also under `-race`): duplicate
  pushes 2 to 5 times, eight concurrent pushes of one batch, a server fault mid-batch and a full
  resend, the request dying mid-batch, 25 random orderings and batch cuts of a stream converging to
  one state, a void before its sale from another device, two devices and a retry storm on one
  outlet, skew, revocation before, during and in flight, another business's staff, ids and
  outlet, one event id on two devices, content changes under one key, malformed events, and the
  inbox being unrewritable.

#### 5.1.2 Load check as built (B1.11)

`make load` (also part of `make test`; skipped by `-short`) pushes a week of a busy cafe through the
real push path: 3 tablets, 600 sales a day each day for 7 days (4,242 events with shift opens and
closes), cut into bursts of 1 to 25 events and sent by the three tablets at once. On a laptop-class
Postgres with no tuning (the same one CI uses) it measured about 620 events per second with push
latency p50 58 ms, p95 116 ms, p99 133 ms and max 150 ms against the 300 ms bar, with every sale
stored once, no flag, and every event accepted. Afterwards the end-of-day report takes about 9 ms,
the sales list 3 to 8 ms and the end-of-shift report 5 ms. The test fails if p95 passes 300 ms or a
report passes 500 ms.

The planner prefers a sequential scan on tables this small, so a plan on test data says little.
Instead `TestTheReportsHaveTheIndexesTheyReadThrough` checks that each way the reports, the list and
the monitor read has an index built for it (mutation-checked: removing one fails the test), and the
timings above are on a full week of data. Run `EXPLAIN (ANALYZE, BUFFERS)` on the pilot's database
after its first real week before adding anything.

### 5.2 Pull: server to device

```
GET /v1/sync/pull?cursor=<opaque>
```

Returns everything the device needs, as deltas since its cursor:

- catalog: items, variants, modifiers, categories, outlet prices and availability;
- outlet settings (tax, service charge, rounding, timezone, receipt header/footer);
- staff roster for the outlet: names, PIN hashes, permissions;
- entitlements snapshot with `expires_at`;
- announcements (Phase 2).

Implementation:

- A per-tenant `change_log(tenant_id, seq bigint, entity_type, entity_id, op, outlet_id null)`
  written in the same transaction as each catalog/settings/staff change. `seq` comes from a
  per-tenant counter row (`tenant.change_seq`, incremented with `UPDATE ... RETURNING`), which
  keeps it gapless and ordered per tenant.
- The cursor is the last `seq` seen. The response contains the **current state** of each changed
  entity (not the history), plus deletions as tombstones.
- `cursor=null` or a cursor older than retention returns a full snapshot.
- Each sale records the `catalog_seq` it was priced against, so a sale against a stale price is
  explainable.


**In CI** the latency bar is enforced by a separate job (`load`, running `make load` without
`-race`). The main `test` job runs under the race detector, which slows the code several times, so
there the same test still runs every correctness check (nothing lost, duplicated or flagged; the
report timings) but only logs the p95 instead of failing on it. The first CI run showed why: 430 ms
under `-race` on a shared runner against 121 ms without it on a laptop.

#### 5.2.1 Pull as built (B1.6)

- `GET /v1/sync/pull?cursor=&limit=` (device token). The response lists the **current state** of
  each entity that changed: `categories`, `items` (each with its variants and modifier group ids),
  `modifier_groups` (each with its modifiers), `outlet_variants` (this outlet's overrides and
  availability), `staff` roster entries (PIN hash, permissions at this outlet) and
  `removed_staff_ids`, the `outlet` with its settings when either changed, `deleted` tombstones
  (empty in Phase 1: the catalog is archived, and archived entities are sent marked
  `archived_at`), and the entitlements snapshot with `expires_at` (7 days) on every pull. Every
  list is present even when empty.
- **Entities, not rows.** The change log stores aggregates (6.3.1): a variant change is an `item`
  change. The pull collapses a run of entries to one reload per entity, so ten edits to one item
  arrive once, in their latest state, and a fixed number of queries (the log, one per kind of
  entity) serves any number of changes (tested at 1 and 41).
- **Cursor.** Opaque (`c1:<change number>`, base64url). The server first reads the tenant's head
  change number, which commits atomically with the changes it counts, so anything numbered up to it
  is visible to the reads that follow and anything newer arrives next time. A response never
  moves the cursor past a change it did not send: when a page is full, `has_more` is true and the
  cursor is the last change number sent; otherwise it is the head, so changes for other outlets are
  skipped once and not scanned again. The device stores the cursor after applying the response.
- **Snapshot.** No cursor, an unreadable one, or one ahead of the server (a restored database)
  returns the whole state with `snapshot: true`, which replaces the device's copy. There is no
  retention job for the change log yet, so "older than retention" cannot happen; when one exists,
  a cursor below the oldest retained change number must also give a snapshot. A snapshot is one
  response, not paged; the catalog of a cafe is small.
- **Per outlet.** Overrides, availability and settings of other outlets never reach a device.
  Staff changes are recorded for every outlet and filtered at read time: someone deactivated or no
  longer assigned to this outlet comes back in `removed_staff_ids`, and the snapshot roster is
  exactly active staff assigned here plus owners.
- **The property tested:** a device applying a random run of edits (items, prices, archives,
  overrides at two outlets, modifiers, PINs, deactivations, role moves, settings) through pulls of
  random small page sizes ends with exactly what a fresh snapshot gives it.
- The roster endpoint (`GET /v1/pos/roster`) stays for the first load; the pull's snapshot covers
  the same ground and more.

### 5.3 Tests written first

Before any POS client code depends on it:

- **Duplicate push**: the same batch pushed 2–5 times gives identical state and responses.
- **Partial failure**: a crash after event N (simulated by killing the transaction) followed by a
  retry of the whole batch loses nothing and duplicates nothing.
- **Reordering**: random permutations of a valid event stream converge to the same final state
  (property-based, using `testing/quick` or `rapid`).
- **Concurrency**: two devices pushing at once to the same outlet, with overlapping shifts.
- **Clock skew**: device clocks hours off still produce correct `business_date` behaviour as
  specified.
- **Revocation**: a device revoked mid-batch.
- **Cross-tenant**: a device of tenant A sending an event that references tenant B's ids.
- **End-of-day reconciliation**: a simulated week of sales; the server's end-of-day totals match
  the totals the client computes from the same events. This is the Phase 1 exit test in miniature.

---

## 6. Data model by module

Columns listed are the essential ones. All tables have `id uuid pk`, `tenant_id` (except platform
tables), `created_at`, `updated_at` where mutable.

### 6.1 Tenancy and identity (Phase 0)

| Table | Key columns |
|---|---|
| `tenant` | `name`, `slug`, plan fields (4.6), `change_seq`, `suspended_at` |
| `outlet` | `tenant_id`, `name`, `code` (short, for receipts, unique per tenant), `address` (the timezone lives in `outlet_settings`) |
| `outlet_settings` | `outlet_id`, tax/service/rounding/time (4.8), `receipt_header`, `receipt_footer` |
| `user_account` | `email` (citext, unique), `password_hash`, `email_verified_at`, `locale` |
| `tenant_member` | `tenant_id`, `user_id`, `is_owner` |
| `staff` | `tenant_id`, `user_id null` (cashiers need no email), `display_name`, `pin_hash`, `pin_rotated_at`, `active` |
| `role`, `role_permission`, `staff_outlet_role` | section 4.4 |
| `device` | `tenant_id`, `outlet_id`, `device_code`, `name`, `secret_hash`, `paired_by`, `paired_at`, `revoked_at`, `last_seen_at`, `last_sync_at`, `app_version`, `clock_skew_ms` |
| `refresh_token` | `principal_type`, `principal_id`, `token_hash`, `family_id`, `expires_at`, `revoked_at` (rotation with reuse detection) |

#### 6.1.1 Self-serve signup as built (B2.1)

`POST /v1/signup` (public) and `internal/signup`, a small module that composes `tenancy` and
`identity` (`tenancy.CreateTenantIn` and `identity.CreateMemberIn` run inside one transaction opened
by signup; the password is hashed beforehand with `identity.PrepareMember`). One transaction creates
the tenant (early access), its four system roles, the first outlet with its settings, the owner (a
member and a staff record), the verification email job and a `tenant.signed_up` audit entry; any
failure leaves nothing behind (tested by forcing the owner step to fail after the tenant insert).

- **No account enumeration.** The endpoint always answers 202 with no body. An address that already
  has an account creates nothing: an unverified account is sent its verification link again, a
  verified one an "account exists" email pointing at sign-in (one per hour per account, deduplicated
  by river), so signing up repeatedly cannot flood an inbox. The password is hashed before the check,
  so timing does not give it away either. Two sign-ups with one address at the same moment (eight in
  the test) produce one business, and all answer 202.
- **Slug and outlet code** are generated: `slugify(business name)-xxxx` with four random characters,
  retried on collision up to five times; the outlet code is up to four letters of the outlet name
  plus 1 unless given.
- **Bot protection:** a honeypot field (`website`; filled means accepted and ignored) and rate limits
  per caller address (burst 5, then one per two minutes) and per email address (burst 3, then one per
  ten minutes). The caller address is only meaningful behind the right `ORION_TRUST_PROXY`
  (`deploy/README.md`).
- **Not built, on purpose:** Turnstile or another CAPTCHA (the honeypot and the limits come first;
  add a verifier behind the same handler if abuse appears), cleanup of businesses whose owner never
  verified (a job that removes tenants unverified after N days is B2.1b: deleting a tenant touches
  every table), accepting the terms (B2.10), password reset (no endpoint exists; the account-exists
  email has nothing better to point at than sign-in), and adding a second business to an existing
  account.

### 6.2 Platform (Phase 0)

| Table | Key columns |
|---|---|
| `operator` | `email`, `password_hash`, `totp_secret_enc`, `totp_confirmed_at`, `disabled_at` |
| `operator_recovery_code` | `operator_id`, `code_hash`, `used_at` |
| `platform_audit_log` | section 4.7 |
| `platform_setting` | `key`, `value jsonb` (for example the global `billing_starts_at` in Phase 5) |
| `announcement` (Phase 2) | `audience` (all / tenant), `tenant_id null`, `title`, `body` per locale, `starts_at`, `ends_at` |

TOTP secrets are encrypted at rest with a key from the secret store (AES-GCM via a small
`kernel/secrets` helper), not just hashed, because they must be read back.

### 6.3 Catalog (Phase 1)

| Table | Key columns |
|---|---|
| `category` | `name`, `sort_order` |
| `item` | `category_id`, `name`, `sku`, `barcode`, `image_url`, `track_stock`, `archived_at` |
| `variant` | `item_id`, `name`, `sku`, `barcode`, `base_price` (rupiah), `sort_order` |
| `modifier_group` | `name`, `min_select`, `max_select`, `required` |
| `modifier` | `group_id`, `name`, `price_delta` |
| `item_modifier_group` | `item_id`, `group_id`, `sort_order` |
| `outlet_variant` | `outlet_id`, `variant_id`, `price_override null`, `available` |
| `bundle`, `bundle_component` | Later in Phase 1 or Phase 2, only if the design partner needs it |

Archive, never hard-delete, anything a sale may reference. Sale lines also snapshot the name and
price (6.4), so a later rename never changes a receipt.

#### 6.3.1 Catalog as built (B1.1)

- **Permission.** Every catalog operation, reads included, needs `catalog.manage`. Cashiers read
  the catalog through the sync pull, not through `/v1`. The two outlet routes
  (`/v1/outlets/{outletId}/variants...`) also require the permission *at that outlet*, and an
  outlet of another business is `404`.
- **Archive, never delete.** `orion_app` has no `DELETE` on any catalog table except the
  `item_modifier_group` links, and column-level `UPDATE` grants keep ids and parents fixed. Lists
  hide archived rows unless `include_archived=true`; reading by id always works. Archiving a
  variant frees its SKU and barcode; restoring one that has been reused answers `409 conflict`.
  Live category and modifier group names are unique per business, ignoring case.
- **Prices live on variants.** An item is sold through its variants, so creating one needs at
  least one (a one-size item has one variant with an empty name). `outlet_variant` holds a price
  override and an `available` flag per outlet; no row means the base price and available.
  Rupiah are bounded to 0..1,000,000,000 (modifier deltas may be negative) by the API and by
  `CHECK`s.
- **The change log works in aggregates.** A change to a variant or to an item's modifier group
  links is recorded as an `item` change, a change to a modifier is a `modifier_group` change, and a
  category is its own entity. The pull (B1.6) therefore loads an item with its variants and links
  in two queries and a group with its modifiers in one. An outlet's price or availability is an
  `outlet_variant` change with `outlet_id` set and the variant id as the entity, so only that
  outlet's devices download it. Archiving is an upsert carrying `archived_at`, not a tombstone, so
  devices can still resolve old sales.
- **Audit.** A changed variant `base_price`, modifier `price_delta` or outlet `price_override`
  writes `catalog.price_changed` with the scope, the old and the new price. Renames and
  availability changes do not.
- **PATCH.** Fields left out stay as they are. A blank `sku`, `barcode` or `image_url` clears it,
  and `clear_category: true` removes the category (JSON `null` and "absent" are the same to the
  generated Go types, so an explicit flag keeps the two apart). `modifier_group_ids`, when present,
  replaces the item's groups in that order.
- **Lists** page by id (`cursor`, `limit`) and make a fixed number of queries whatever the page
  size (items: 3, groups: 2); a test compares 2 rows with 40. Item and group creation insert their
  children with one `INSERT ... SELECT unnest`.
- Every write takes `LockTenant` first (section 4.12); a test runs 16 mixed edits at once and
  requires gapless change numbers.
- Differences from the table above: `item_modifier_group`, `outlet_variant` and the others carry
  `tenant_id` in composite foreign keys like every tenant table; `modifier_group` also has
  `CHECK (min_select <= max_select)` and `CHECK (NOT required OR min_select >= 1)`.
- `orion admin seed-demo` creates a six-item cafe menu (with variants, a required and an optional
  modifier group) for front-end work.

### 6.4 Sales and shifts (Phase 1)

| Table | Key columns |
|---|---|
| `shift` | `outlet_id`, `device_id`, `opened_by`, `opened_at`, `opening_cash`, `closed_by`, `closed_at`, `counted_cash`, `expected_cash`, `business_date` |
| `cash_movement` | `shift_id`, `kind` (pay_in, pay_out, no_sale), `amount`, `reason`, `staff_id` |
| `sale` | `outlet_id`, `device_id`, `shift_id`, `staff_id`, `receipt_number` (+ parts), `device_time`, `received_at`, `business_date`, `pricing_version`, `catalog_seq`, `subtotal`, `discount_total`, `service_charge`, `tax`, `rounding_amount`, `total`, `status` (completed, voided) |
| `sale_line` | `sale_id`, `variant_id`, `name_snapshot`, `unit_price`, `quantity`, `line_discount`, `allocated_bill_discount`, `line_total` |
| `sale_line_modifier` | `sale_line_id`, `modifier_id`, `name_snapshot`, `price_delta` |
| `sale_discount` | `sale_id`, `sale_line_id null`, `kind` (percent, amount), `value`, `amount`, `reason`, `approved_by` |
| `payment` | `sale_id`, `method` (cash, qris_manual, qris_dynamic, ewallet, card_manual), `amount`, `tendered`, `change`, `reference`, `status` |
| `void` | `sale_id`, `staff_id`, `approved_by`, `reason`, `device_time` |
| `refund` (Phase 2) | `sale_id`, lines and amounts, `method`, `reason`, `staff_id`, `approved_by` |
| `sale_flag` | `sale_id`, `code` (total_mismatch, permission_missing, stale_price), `detail` |
| `sync_inbox` | `tenant_id`, `device_id`, `idempotency_key` (unique together), `event_type`, `payload_hash`, `payload jsonb`, `status`, `result jsonb`, `received_at` |

`sale` status moves `completed -> voided` only through a `void` row; the sale row's monetary
columns never change. Insert-only on `sale_line`, `payment`, `void`, `refund`. Revoke `UPDATE` on
those tables from `orion_app` except for the `status` column on `sale` (column-level grant).

The pricing vectors in `testdata/pricing-vectors/` are JSON files of the form
`{ settings, lines, discounts, tender } -> { expected totals }`. The Go and TS implementations
both run them in CI. Start with about 40 vectors covering inclusive and exclusive tax, service
charge on and off, line and bill discounts, rounding at each unit, and rupiah amounts that do not
divide evenly.

#### 6.4.1 Sales projectors as built (B1.5)

`internal/sales` implements `sync.Projector` for the five Phase 1 event types (schema version 1) and
is registered in `cmd/orion`. Migration 00011 holds the tables. Payloads are JSON objects; unknown
fields are ignored so a newer app may add fields within a version.

| Event | Payload | Notes |
|---|---|---|
| `shift.opened` | `{opening_cash}` | The event id is the shift id. `opened_at` is `device_time`. |
| `shift.closed` | `{shift_id, counted_cash}` | Own event id. Waits for the shift. A shift closes once (`already_closed`). |
| `cash.movement` | `{shift_id, kind: pay_in \| pay_out \| no_sale, amount, reason}` | `no_sale` has no amount; `pay_out` needs a reason. |
| `sale.completed` | `{shift_id, receipt_number, catalog_seq, pricing{version, price_includes_tax, tax_rate_bp, service_charge_rate_bp, service_charge_taxable, cash_rounding_unit, cash_rounding_mode}, lines[{variant_id, name, unit_price, quantity, modifiers[{modifier_id, name, price_delta}], discount, allocated_bill_discount, total}], discounts[{line, kind, value, amount, reason, approved_by}], totals{subtotal, discount_total, service_charge, tax, rounding_amount, total}, payments[{method, amount, tendered, change, reference}]}` | The event id is the sale id. |
| `sale.voided` | `{sale_id, reason, approved_by?, shift_id?}` | Own event id. Waits for the sale. `shift_id` is the shift the void happened in (default: the sale's). |

- **Recorded as rung up.** A sale stores the amounts the device sent, never the server's own
  numbers. The server recomputes it with `internal/pricing` using the settings the device says it
  used (so a later settings change cannot make old sales look wrong) and compares every line,
  discount and total; a difference is a `total_mismatch` flag listing the fields, an unknown
  `pricing.version` a `pricing_version` flag, a bill the algorithm cannot price (two discounts on a
  line, a discount larger than its line) a `pricing_invalid` flag. Payments are checked against what
  was due (`payment_mismatch`). Tender (all cash, none, mixed) is derived from the payments.
- **Flags** are a table, `flag`, for every event type, a generalisation of the plan's `sale_flag`
  (`target_type`: sale, void, shift, cash_movement). Codes: `total_mismatch`, `pricing_invalid`,
  `pricing_version`, `payment_mismatch`, `permission_missing`, `stale_price`, `sale_outside_shift`,
  `shift_other_device`, `device_time_ahead` (more than 10 minutes ahead of the server; behind is
  just a late sync), `after_shift_close`. None of them blocks the event.
- **Permissions** are checked against the person's role *now* (4.4): `sale.create` for a sale,
  `discount.apply_manual` for each discount (from `approved_by` if named, else the cashier),
  `sale.void` from the voider or the approver, `shift.open`, `shift.close`, and
  `drawer.open_no_sale` for every cash movement (there is no separate cash permission). An
  `approved_by` who is not part of the business is rejected (`unknown_staff`).
- **Stale price** means the line's unit price differs from the variant's current price at the outlet
  *and* the variant (or its outlet price) changed after the device's `catalog_seq` but before the
  sale's `device_time`: an update the device could have had and did not. A sale made before a price
  change, or by a device that had pulled it, is not flagged.
- **Order.** A shift close or cash movement waits for its shift; a sale waits for its shift; a void
  waits for its sale (and for the shift it names); a chain released by one arrival runs in that
  arrival's transaction. `sale_outside_shift` flags a sale whose `device_time` is before its shift
  opened or after it closed, so the flag is the same whichever order the events arrived in.
- **Rejections** specific to sales: `invalid_payload` (field named in `detail`),
  `invalid_receipt_number` (not `{outlet code}-{device code}-{counter}`, or not this outlet and
  device), `duplicate_receipt_number`, `unknown_reference` (a variant or modifier that is not in the
  business's catalog), `wrong_outlet`, `already_voided`, `already_closed`, `unknown_staff`.
- **Insert-only.** The app role may insert into every sales table and update only `sale.status` and
  the four shift closing columns; a `CHECK` ties the closing columns together. There is no delete.
- **Expected cash is not stored** on the shift (the plan listed `expected_cash`). It is derived in
  the end-of-shift report (B1.8) from the opening cash, cash payments net of change, pay ins and
  outs, and refunds of voids made in that shift, so a late event can never leave a stale number.
- **Tests** run the real push path with the real projectors: all 45 priceable golden vectors
  project with no flag at all; tampered totals, permissions, prices, the chain of a void before its
  sale before its shift, 12 random orderings of a real day converging to identical sales, shifts,
  cash and flags, receipt numbers, concurrent voids and concurrent devices, business dates under a
  04:00 cutoff, and the app role being unable to rewrite anything.

#### 6.4.2 Reports as built (B1.8)

- `GET /v1/reports/shifts/{shiftId}` and `GET /v1/reports/days/{date}?outlet_id=`, both needing
  `report.view` **at the outlet** (an id of another business is `404`). `internal/reporting` reads
  the sales tables in its own read-only queries, checked against the migrations by `sqlc`, and
  computes live in one transaction, so a report can be rebuilt at any time and nothing stored goes
  stale. This is the one place a module reads another module's tables.
- **Sales** are the completed ones by the business date they were rung up on, with the amounts the
  device charged: subtotal, discounts, net, service charge, tax, total (before cash rounding) and
  rounding. A void recorded later removes the sale from its day, as on any POS, so a past day can
  change after the fact; voids are reported next to it (`voided_sales`).
- **Expected cash** = opening cash + cash applied to the bills of the shift's sales (net of change,
  whatever became of the sale) - cash refunded for voids made *in this shift* (of any shift's
  sales) + pay ins - pay outs. A refund therefore leaves the drawer of the shift that paid it, and a
  closed shift's expected cash never changes when a later shift voids one of its sales.
  `difference` is counted minus expected (negative: short); both are absent while the shift is open.
- **The day** lists the shifts opened on that business date, in order, with their reconciliation, the
  cash of the closed ones added up, payment methods, manual discounts, pay ins and outs, drawer
  openings, and the day's review flags by code. A shift that spans midnight belongs to the day it
  opened on, and a sale rung up after midnight but before the cutoff belongs to the previous day.
- **The reconciliation test** (`internal/sales/reports_test.go`) pushes a simulated week (about 300
  sales over seven days on two devices, discounts, cash and QRIS and split payments, voids of the
  same day's and of earlier days' sales, pay ins and outs, drawer openings, a 03:00 cutoff with
  sales after midnight, shifts closed with the drawer short or over) and requires every figure in
  every day and shift report to equal the test's own bookkeeping, which never reads the database.
  This is the Phase 1 exit test in miniature, and it fails when the refund rule above is changed.
- Migration 00012 adds the indexes the reports need: PostgreSQL does not index the referencing side
  of a foreign key, so each is justified in the file. B1.11 runs `EXPLAIN` on each report against a
  busy week.

#### 6.4.3 Sales list and detail as built (B1.9)

- `GET /v1/sales` and `GET /v1/sales/{saleId}`, read-only, needing `report.view` at the outlet(s).
  Without `outlet_id` the list covers the outlets where the caller holds `report.view` (all of them
  for an owner); with it, an outlet of another business is `404` and one the caller may not see is
  `403`. A sale id of another business is `404`.
- **Order and paging.** Newest business day first, then newest by the device's clock, then id,
  keyset-paged by `(business_date, device_time, id)` with an opaque `next_cursor`. Ordering by the
  device clock (not the id) keeps the list chronological even for a client that does not send
  UUIDv7 ids. Filters: `from` and `to` (business dates, inclusive), `status`, exact
  `receipt_number`, `staff_id`, and `flagged` (a flag on the sale or on its void).
- **List items** carry the amounts as recorded, the payments and the codes of the review flags,
  from two extra queries however many sales are on the page (tested at 2 and 60). **Detail** adds the
  lines with their modifiers (names and prices as on the receipt), the discounts (null `line_no`
  for a bill discount), the void with its reason and approver, the flags with their detail, the
  calculation settings the device used and the catalog change number it priced against.
- The sales list and the reports use indexes from migration 00012; B1.11 checks the plans with
  `EXPLAIN` on a busy week.

### 6.5 Payments (Phase 1 manual, Phase 2 gateway)

- Phase 1: payments arrive inside `sale.completed`. Manual QRIS stores the static QR reference and
  the cashier's confirmation. Card is `card_manual` with an optional EDC approval code.
- Phase 2, gateway:
  - `payment_intent(id, tenant_id, outlet_id, sale_id null, method, amount, gateway, gateway_ref,
    qr_string, status, expires_at)`.
  - `POST /v1/pos/payment-intents` creates a dynamic QRIS or e-wallet charge (needs the device
    online). The POS polls `GET /v1/pos/payment-intents/{id}` or waits for a push.
  - `POST /webhooks/{gateway}`: verify the signature, store the raw body in `gateway_webhook`
    (unique on the gateway's event id for idempotency), update the intent in the same transaction.
  - A `river` job reconciles intents stuck in `pending` by asking the gateway's API, and a daily
    job compares the gateway's settlement report with recorded payments and flags differences.
  - A `Gateway` interface (`CreateQRIS`, `CreateEwalletCharge`, `GetStatus`, `VerifyWebhook`,
    `Refund`) with one implementation for whichever gateway is chosen and a fake for tests.
  - Sub-merchant / platform onboarding depends on the gateway's model (roadmap open question). Keep
    per-tenant gateway credentials or sub-account ids in `tenant_payment_account`, encrypted.

#### 6.5.1 Gateway choice: doit.id (decided 2026-10-06)

The owner chose [doit.id](https://doit.id/docs/). It is a merchant aggregator. The Bank
Indonesia licence is held by its partner Manjo (licence no. 25/594/DKSP/Srt/B.). Facts from its
docs that change the plan above:

- **Platform model fits tenants.** We use one parent API key. Each tenant is a sub-merchant
  (`POST /v1/submerchants`, with KYC through doit.id's `onboarding_url` or our own form), and we
  route a payment to it with the `for-sub-merchant: <id>` header. Doit.id pays the net amount
  straight to the tenant's bank account, so we never hold funds. `tenant_payment_account` stores
  only the sub-merchant id and its onboarding status. Nothing secret is stored per tenant.
- **No separate e-wallet charge.** Customers pay with any e-wallet by scanning a dynamic QRIS
  (`POST /v1/payments` with `rail: "qris"`, which returns `qr_content`). Drop `CreateEwalletCharge`
  from the interface.
- **The QR goes stale after about 10 minutes.** After that `qr_content` is null even though the
  payment is still pending. The POS creates a fresh intent instead of showing a stale QR.
- **The fee may be added on top** (`total_amount = amount + fee_amount`). Before building B2.4,
  confirm that the merchant can absorb the fee. The customer must pay exactly the bill total
  that `internal/pricing` calculated.
- **Webhooks:** signature header `PayBridge-Signature: t=<unix>,v1=<hex>`, computed as
  `HMAC-SHA256(secret, t + "." + rawBody)`. Reject a stale `t`. Deduplicate on the event `id`.
  Event types: `payment.paid`, `payment.expired`, `refund.succeeded` and `refund.failed`. Retries
  run for 24 hours. Two flags need handling as review flags: `bayar_telat` (paid after expiry)
  and `bayar_ganda` (paid twice).
- **One rate limit for every tenant:** 120 requests per minute on the parent key. Devices never
  poll doit.id. They poll our intent, which webhooks keep up to date. The reconcile job is
  throttled to stay under the limit.
- Every POST needs an `Idempotency-Key`. Use the `payment_intent` id.
- **Refunds:** `POST /v1/payments/{id}/refunds`, partial or full, only for paid payments (B2.5).
- **Daily reconciliation:** compare `GET /v1/submerchants/{id}/settlements` and
  `GET /v1/payments?status=paid` against our recorded payments.
- **Sandbox:** `pb_test_` keys. The fake `Gateway` is still what the tests use.

### 6.6 Inventory (Phase 3)

The existing back-office pages (item library, UoM categories, transaction types, stock opname,
adjustments, waste, used stock) define the shape. Mapping them onto the ledger:

| Table | Key columns |
|---|---|
| `uom_category` | `name` |
| `uom` | `category_id`, `name`, `kind` (reference, bigger, smaller), `ratio_num`, `ratio_den` (integer ratio to the reference unit, instead of float), `rounding_scaled`, `active` |
| `ingredient` | `name`, `category_id`, `base_uom_id`, `track`, `archived_at` |
| `ingredient_category` | `name`, `default_transaction_type_id` |
| `recipe` | `variant_id` or `modifier_id`, `version`, `active_from` |
| `recipe_line` | `recipe_id`, `ingredient_id`, `quantity_scaled` |
| `stock_movement` | `outlet_id`, `ingredient_id`, `quantity_scaled` (signed), `kind` (receive, sale_consumption, waste, opname_adjustment, transfer_out, transfer_in, manual_adjustment), `source_type`, `source_id`, `reason`, `staff_id`, `occurred_at`, `business_date`, `unit_cost null` |
| `stock_balance` | `outlet_id`, `ingredient_id`, `quantity_scaled`, `last_movement_id` (materialized) |
| `purchase`, `purchase_line` | `supplier`, `transaction_type_id`, lines with quantity, UoM and cost |
| `transaction_type` | `name`, `category` (matches the existing "Belanja Bahan Pasar" style setup) |
| `opname`, `opname_line` | `status` (draft, counting, posted), counted vs. expected per ingredient |
| `waste`, `transfer` | header + lines, each posting ledger rows |

Rules:

- **Scale**: quantities are integers in the base unit times 1000 (so 1 = 0.001 g or 0.001 ml). This
  covers grams and millilitres with three decimals and stays far inside `bigint`.
- Ledger rows are insert-only. `stock_balance` is updated in the **same transaction** as the
  movement (`INSERT ... ON CONFLICT DO UPDATE SET quantity = quantity + excluded.quantity`).
- **Sale consumption**: when a `sale.completed` event is projected, the inventory module (through
  its interface) inserts one `sale_consumption` movement per recipe ingredient, using the recipe
  version active at `device_time`. A void inserts the reversing movements. Negative balances are
  allowed (ADR 0004).
- **Opname**: counted quantity minus the balance at the count time gives one `opname_adjustment`
  per ingredient. Posting is idempotent.
- **Rebuild job** (ADR 0006): `orion admin rebuild-stock --tenant --outlet` recomputes balances
  from the ledger and reports differences. A nightly job checks a sample and alerts on drift.

### 6.7 Restaurant flow (Phase 4)

This breaks the "a sale is complete when it reaches the server" assumption, so it needs an ADR of
its own before the code.

- `floor`, `table(outlet_id, floor_id, name, seats, position)`.
- An **open bill** is an `order` with `order_line`s that change while it is open. Recommended
  approach: keep it append-only as `order.line_added`, `order.line_removed`,
  `order.sent_to_kitchen`, `order.moved_table`, `order.split`, `order.merged`, `order.closed`
  events, with the `order` row as a projection. Closing an order produces a normal
  `sale.completed`.
- **Several devices on one open bill** is the first true multi-writer case. Since the events are
  append-only, they merge without CRDTs, but "who sees the latest bill" needs the devices online
  or on the same LAN. The ADR should decide: require connectivity for shared open bills, and allow
  single-device open bills offline.
- `kitchen_station`, `item_station` routing, `kitchen_ticket` (printed by the POS in Phase 4,
  then a KDS view reading the same tickets).

### 6.8 Billing (Phase 5 only)

| Table | Key columns |
|---|---|
| `plan_price` | `plan_id`, `interval` (month, year), `amount`, `unit` (per outlet / per device / flat; decided by then) |
| `subscription` | `tenant_id`, `plan_id`, `status`, `current_period_start`, `current_period_end`, `trial_ends_at`, `founding_price null` |
| `invoice` | `tenant_id`, `number`, `period`, `subtotal`, `discount`, `tax`, `total`, `status` (draft, open, paid, void, expired), `due_at` |
| `invoice_line` | `invoice_id`, `description`, `quantity`, `unit_amount`, `amount` |
| `invoice_payment` | `invoice_id`, `gateway`, `method` (va, qris, transfer, card), `gateway_ref`, `amount`, `paid_at` |
| `subscription_promo` | `code` (normalized, unique), `reward_kind` (trial_extension, percent_off, amount_off, plan_upgrade), `reward_value`, `periods`, `valid_from`, `valid_until`, `max_redemptions`, `applies_to_plan_ids` |
| `subscription_promo_redemption` | `promo_id`, `tenant_id` (unique together), `redeemed_at`, `applied_at null`, `granted jsonb` |

Note the names: `subscription_promo` here, `voucher` for cafe-to-customer promos in Phase 6
(ADR 0008).

---

## 7. API surface by phase

Paths under `/v1` are tenant-side (user or device tokens). Paths under `/admin` are operator-only.

| Phase | Endpoints |
|---|---|
| 0 | `POST /v1/auth/login`, `/refresh`, `/logout`; `POST /v1/auth/verify-email`; `GET /v1/me`; `GET/PATCH /v1/outlets/{id}`; `GET/PUT /v1/outlets/{id}/settings`; `GET/POST/PATCH /v1/staff`; `PUT /v1/staff/{id}/pin`; `GET /v1/roles`; `POST /v1/devices/pair`; `POST /v1/devices/token`; `DELETE /v1/devices/{id}`; `GET /v1/entitlements`; `GET /v1/pos/receipt-test` (for the Phase 0 "print a test receipt from API data" exit); `POST /admin/auth/login`, `/totp/verify`; `GET /admin/audit-log` |
| 1 | Catalog CRUD (`/v1/categories`, `/v1/items`, `/v1/items/{id}/variants`, `/v1/modifier-groups`, `/v1/outlets/{id}/prices`); `POST /v1/sync/push`; `GET /v1/sync/pull`; `GET /v1/reports/shift/{id}`; `GET /v1/reports/end-of-day?outlet_id&date`; `GET /v1/sales`, `GET /v1/sales/{id}` |
| 2 | `POST /v1/signup`; onboarding (`POST /v1/outlets`, device pairing reused); `POST /v1/catalog/import` (CSV, async job, `GET /v1/jobs/{id}`); `POST /v1/pos/payment-intents`, `GET .../{id}`; `POST /webhooks/{gateway}`; refunds via sync; `GET /v1/reports/sales?group_by=day|item|payment_method`; `/admin/tenants`, `/admin/tenants/{id}`, `POST /admin/tenants/{id}/suspend|reinstate`, `DELETE /admin/devices/{id}`, `/admin/entitlements`, `/admin/flags`, `/admin/announcements`, `/admin/metrics` |
| 3 | `/v1/uom-categories`, `/v1/ingredients`, `/v1/recipes`, `/v1/purchases`, `/v1/opnames` (+ `/post`), `/v1/waste`, `/v1/transfers`, `GET /v1/stock/balances`, `GET /v1/stock/movements` |
| 4 | `/v1/floors`, `/v1/tables`, `/v1/kitchen-stations`; order events via sync; `GET /v1/kitchen/tickets` (KDS) |
| 5 | `GET /v1/billing/subscription`, `GET /v1/billing/invoices`, `POST /v1/billing/invoices/{id}/pay`, `POST /v1/billing/promo-codes/redeem`, `GET /v1/export` (async); `/admin/billing/schedule`, `/admin/promos`, `/admin/plans`, `/admin/tenants/{id}/billing` |

---

## 8. Background jobs (river)

| Job | Phase | Trigger |
|---|---|---|
| `send_email` | 0 | verification, password reset, device paired |
| `purge_expired_tokens`, `purge_idempotency_keys` | 0 | daily |
| `retry_pending_dependency_events` | 1 | on sale insert + every 5 min |
| `end_of_day_rollup` | 1 | per outlet, after local cutoff + grace; also computed on read so it is never a blocker |
| `catalog_import` | 2 | on CSV upload |
| `reconcile_payment_intents` | 2 | every minute for pending intents |
| `reconcile_gateway_settlement` | 2 | daily |
| `tenant_daily_metrics` | 2 | nightly aggregates for the admin console (sales counts only, no business data, ADR 0008) |
| `stock_balance_check` | 3 | nightly sample; full rebuild on demand |
| `billing_start_notices` | 5 | notice schedule before the global/tenant billing date |
| `billing_start_transition` | 5 | on the date: early_access -> trial/paid, applying stored promo redemptions |
| `trial_reminders`, `issue_invoices`, `dunning`, `grace_to_read_only` | 5 | daily |
| `tenant_export` | 5 | on request; zipped CSV/JSON to object storage, signed URL by email |

---

## 9. Phase-by-phase work

Task ids (`B0.1` ...) are meant to become GitHub issues.

### Phase 0: Foundation (backend share: about 3 weeks)

| Id | Task | Size |
|---|---|---|
| B0.1 | ✅ Repo bootstrap: `go.mod`, Makefile, `golangci-lint` (+ `internal/archtest` for module boundaries), GitHub Actions (lint, tests on a testcontainers Postgres, `make gen` + `git diff --exit-code`), Dockerfile, `docker compose` for local Postgres | 2d |
| B0.2 | ✅ `cmd/orion` with `serve` and `migrate` subcommands (`worker` arrived with `river` in B0.6); config from env; graceful shutdown; `/healthz`, `/readyz`; slog; Sentry | 1d |
| B0.3 | ◐ OpenAPI pipeline: `api/openapi.yaml`, `oapi-codegen` strict server, problem+json errors, CI staleness check are done; access rules (`security`, `x-permission`) are read from the spec. **Still open:** publishing the TS types (option A in section 3), which waits on the registry decision | 2d |
| B0.4 | ✅ First migrations: roles `orion_app` / `orion_platform`, `tenant` with plan fields, `outlet`, `outlet_settings`, `plan` seeded with `early_access`, RLS policies, `change_log` | 2d |
| B0.5 | ✅ `kernel`: UUIDv7, `money` (integer, basis points, allocation), clock interface, `TenantTx` helper that sets `app.tenant_id`, lock timeout and deadlock retry, tenant context | 1d |
| B0.6 | ✅ Identity: `user_account`, email+password login, email verification, refresh-token rotation with reuse detection, JWT per audience with `kid`; `river` and `orion worker` with the verification email and the token purge (see 4.3.1) | 3d |
| B0.7 | ✅ Roles and permissions: tables, seeded system roles, permission middleware, `staff`, PIN set/rotate (argon2id), and the tenant audit log it needs (see 4.4.1) | 2d |
| B0.8 | ✅ Device pairing, device token exchange, revocation, `device_code` allocation, and `GET /v1/pos/roster` (the roster download in the Phase 0 exit; see 4.3.2) | 2d |
| B0.9 | ✅ Entitlements: tables, resolver, cache, `GET /v1/entitlements`, limit checks helper (always-allow on `early_access` but exercised in tests); enforced for devices and staff. Operator commands to set overrides arrive with B0.10 | 2d |
| B0.10 | ✅ Platform: `operator`, TOTP sign-in, recovery codes, `platform_audit_log` (append-only enforced), `orion admin` CLI (create operator, set and clear entitlement overrides, suspend and reinstate tenants), each writing to the audit log with a required reason (see 4.3.3). Not built: operator refresh tokens, `set-flag` (flags are overrides) | 3d |
| B0.11 | ✅ Tenant isolation suite (`internal/api/isolation_test.go`): two tenants, every tenant-side operation, a coverage test that fails when an operation has no case, and a sweep of every table with a `tenant_id` | 1d |
| B0.12 | ✅ Receipt test endpoint (`GET /v1/pos/receipt-test`, device token; a made-up sale priced with the outlet's tax, service charge and cash rounding, numbered with the device code; a preview of 4.8 without discounts): returns outlet header/footer and a sample sale from real data, for the PWA hardware spike | 0.5d |
| B0.13 | ✅ (as a guide, for now) Ops: the hosting, deploy-pipeline, backup and restore-drill steps are written down in `docs/guides/hosting-and-restore.md`, and the drill is scripted and tested (`deploy/restore-drill.sh`: dump, restore into a scratch database, compare migration version and every table's row count; fails on a stale or incomplete backup). **Not done, by decision: choosing a host, the deploy workflow, and running the first drill. Do these, and write `docs/runbooks/restore.md`, before the pilot** | 3d |
| B0.14 | ✅ Seed command for a demo tenant (`orion admin seed-demo`) for front-end and MSW work. `orion admin create-tenant` also exists (the rest of the `orion admin` CLI is B0.10) | 0.5d |

**Done when** (backend side of the roadmap exit): a tablet can pair with an outlet, fetch a device
token, download the staff roster with PIN hashes, and fetch receipt data from the deployed API;
an operator can sign in with TOTP from the CLI flow; every action shows in the audit log; and a
backup has been restored into a scratch database.

### Phase 1: Pilot-ready counter cafe (backend share: about 6–7 weeks)

| Id | Task | Size |
|---|---|---|
| B1.1 | ✅ Catalog schema and CRUD endpoints, archive semantics, per-outlet prices and availability, `change_log` writes (see 6.3.1) | 5d |
| B1.2 | ✅ Pricing algorithm in Go + golden vectors (51, six of them invalid bills); the format and rules for the TS implementation are in `testdata/pricing-vectors/README.md` (see 4.8.1) | 3d |
| B1.3 | ✅ **Sync tests first** (section 5.3) against a stub projector (`internal/sync/sync_test.go`, `internal/sync/syncstub`); the end-of-day reconciliation test waits for the real projectors (B1.5) | 3d |
| B1.4 | ✅ `sync_inbox`, push endpoint, per-event transactions, idempotency, payload-hash conflict detection, `pending_dependency` (see 5.1.1) | 4d |
| B1.5 | ✅ Projectors: `shift.opened/closed`, `cash.movement`, `sale.completed` (sale, lines, modifiers, discounts, payments, flags), `sale.voided` (permission re-check); event payloads and rules in 6.4.1 | 5d |
| B1.6 | ✅ Pull endpoint: deltas from `change_log`, full snapshot fallback, roster and settings and entitlements, cursor handling (see 5.2.1) | 3d |
| B1.7 | ✅ Outlet settings for tax, service charge, rounding, timezone, cutoff (`PATCH /v1/outlets/{outletId}/settings`); `business_date` derivation (`kernel.BusinessDate`, see 4.9.1). Done ahead of B1.5, which needs it | 1d |
| B1.8 | ✅ Reports: end of shift (expected vs counted cash, by payment method, voids, discounts) and end of day per outlet; numbers match the POS's own totals (see 6.4.2) | 4d |
| B1.9 | ✅ Sales list and detail for the back office (read-only; see 6.4.3) | 2d |
| B1.10 | ✅ Device health: `last_sync_at`, skew, app version; alert (email to owner/operator) when a device has unsynced events for too long (see 4.11.1) | 1d |
| B1.11 | ✅ Load sanity check: one week of a busy cafe (600 sales/day, 3 devices) pushed in bursts, p95 push latency under 300 ms (see 5.1.2) | 1d |
| B1.12 | ✅ Pilot runbook (`docs/runbooks/pilot.md`): how to read flags, fix a stuck device, rebuild a report, run the load check. Support tooling: `orion admin support-report` and `abandon-event` (audited) | 1d |

**Done when** the design partner's week passes with matching end-of-day totals (roadmap), **and**
the server has zero duplicated or lost sales against the device outboxes (checked by comparing
device receipt counters with `sale` rows, gaps explained by voids or unsent drafts).

### Phase 2: Early access (backend share: about 5 weeks)

| Id | Task | Size |
|---|---|---|
| B2.1 | ✅ (see 6.1.1) Self-serve signup: tenant + owner + first outlet + system roles in one transaction; email verification; bot protection (rate limit + honeypot or Turnstile) | 3d |
| B2.2 | Per-tenant limits enforced through entitlements (outlets, devices, staff) for the free tier | 1d |
| B2.3 | CSV catalog import: template compatible with a spreadsheet and Moka's export, dry-run with row errors, then commit as a job | 4d |
| B2.4 | Gateway integration behind the `Gateway` interface: doit.id (6.5.1): tenant sub-merchant onboarding, dynamic QRIS (e-wallets pay by scanning it), webhooks, reconciliation jobs | 6d |
| B2.5 | Refunds: `refund.issued` event, permission, partial refunds by line, gateway refund for gateway payments, negative report entries | 3d |
| B2.6 | Sales reports by day, item and payment method, per outlet, in outlet local time; CSV download | 3d |
| B2.7 | Kitchen/bar tickets: station routing on items, included in pull; printing is client-side | 1d |
| B2.8 | Admin endpoints: tenant list with metrics, suspend/reinstate (suspended tenants: back office read-only, POS warned at next sync, never cut mid-shift), device revocation, entitlement and flag editing, announcements, audit log viewer | 5d |
| B2.9 | `tenant_daily_metrics` aggregates, "stopped syncing" query | 1d |
| B2.10 | Legal plumbing: `terms_acceptance(user_id, version, accepted_at)`; signup requires the current version | 0.5d |
| B2.11 | Security pass: rate limits, headers, dependency audit (`govulncheck` in CI), secret rotation runbook, operator account review | 2d |

**Done when** a stranger can sign up, import a menu, pair a tablet and sell with dynamic QRIS, and
the operator can see them in the console, all without the developer touching the database.

### Phase 3: Inventory (backend share: about 5–6 weeks)

| Id | Task | Size |
|---|---|---|
| B3.1 | Write an ADR for the scaled-integer quantity unit (x1000 base unit) and integer UoM ratios | 0.5d |
| B3.2 | UoM categories and units, ingredients and categories, transaction types (port of the existing Setup pages' data) | 4d |
| B3.3 | Ledger + materialized balance + rebuild command and nightly check | 4d |
| B3.4 | Recipes (versioned) on variants and modifiers; sale consumption and void reversal in the sale projector | 4d |
| B3.5 | Purchasing / receiving with cost | 3d |
| B3.6 | Stock opname: draft, count, post; adjustments | 3d |
| B3.7 | Waste and transfers between outlets (transfer is two ledger rows in one transaction) | 2d |
| B3.8 | Stock endpoints matching the existing Stock Management pages, so the back office can drop its mocks | 3d |
| B3.9 | Inventory reports: movement history, waste report, opname variance, stock value at cost | 3d |

**Done when** selling an item deducts its recipe's ingredients, a void restores them, an opname
posts the difference, and a full rebuild reproduces every balance exactly.

### Phase 4: Restaurant flow (backend share: about 4–5 weeks)

| Id | Task | Size |
|---|---|---|
| B4.1 | ADR: open bills, multi-device editing, connectivity requirement (6.7) | 1d |
| B4.2 | Floors and tables | 2d |
| B4.3 | Order events and projection, including move, split and merge; closing produces `sale.completed` | 8d |
| B4.4 | Near-real-time fan-out of open-bill changes to other devices in the outlet: Server-Sent Events from the API, with pull as the fallback | 3d |
| B4.5 | Kitchen tickets per station, ticket status for a later KDS, `GET /v1/kitchen/tickets` | 3d |
| B4.6 | Reports: covers, table turnover, average bill | 2d |

**Done when** two tablets and a kitchen printer run a full service on shared open bills with
splits and merges, and the end-of-day matches.

### Phase 5: Paid launch (backend share: about 4–5 weeks)

| Id | Task | Size |
|---|---|---|
| B5.1 | Plan prices, subscriptions, invoices (Orion computes the discount and tax; the gateway only collects) | 4d |
| B5.2 | Gateway invoice payments by VA, QRIS, bank transfer and card; webhooks; reconciliation | 4d |
| B5.3 | Billing start schedule: global date + tenant override, refusal of dates inside the notice period, notice emails and in-app notices | 2d |
| B5.4 | Subscription promo codes: generation (random, no look-alike characters, case-insensitive), limits, redemption with rate limit, deferred application for early-access redemptions | 3d |
| B5.5 | Trial (30 days) for new sign-ups, reminders; migration job for early-access tenants with founding price | 2d |
| B5.6 | Grace period and read-only mode: middleware that blocks back-office writes for `read_only` tenants with `tenant_read_only`, while **sync push keeps working** (a cashier is never stopped mid-shift; sales are never dropped) | 2d |
| B5.7 | Data export for every tenant (paid or not) as a job | 2d |
| B5.8 | Admin: plan editing, billing status per tenant, promo usage | 3d |
| B5.9 | End-to-end billing tests with a fake clock: trial -> invoice -> unpaid -> grace -> read-only -> paid -> active | 2d |

**Done when** a new tenant signs up, trials, gets reminded, pays by VA or QRIS and keeps working,
with nobody touching the database; and an unpaid tenant drops to read-only without losing a sale.

### Phase 6: Growth (open-ended)

- Loyalty, vouchers and rule-based promos (`voucher`, `promo_rule`; customers table with consent
  and deletion under UU PDP **designed before** the feature).
- Multi-outlet UI needs little backend work, because outlet ids and per-outlet roles exist from
  Phase 0. Expect cross-outlet reports and transfers to need tuning.
- Online-order integrations: inbound orders become `order` events (Phase 4 model).
- Accounting export (CSV/journal formats).
- PBJT reporting integrations where a local government requires them (tapping box / online
  reporting), behind a per-outlet adapter.

---

## 10. Testing strategy

| Layer | What | How |
|---|---|---|
| Unit | money, pricing, permission resolution, entitlement resolution, receipt number parsing, business date | table tests; golden vectors |
| Repository | sqlc queries, RLS policies, constraints, append-only enforcement | `testcontainers-go` Postgres, one DB per package, migrations applied |
| Sync | section 5.3 | property tests + scenario tests; run on every PR |
| API | each operation through the generated server, including auth audiences and tenant isolation | `httptest` against the real router and DB |
| Contract | generated code is current; spec lints (`redocly lint` or `vacuum`) | CI |
| Billing / jobs | time-dependent flows | injected `Clock`, river test helpers |
| Smoke | after each deploy: `/readyz`, login, pull for the demo tenant | CI job against the deployed URL |

Coverage is not a target. The sync, pricing, ledger and billing packages must have their
scenario suites before their endpoints are used by a client.

---

## 11. Deployment and operations

- **Hosting** (roadmap open question). Recommendation: a region in Jakarta (AWS `ap-southeast-3`
  or GCP `asia-southeast2`) for latency and to keep personal data in Indonesia, which simplifies
  UU PDP questions. A single small VM or container service plus **managed PostgreSQL with
  point-in-time recovery** is enough until well after paid launch. Revisit only if cost forces it.
- **Deploy**: CI builds one container image per tag, runs migrations (`orion migrate up`), then
  rolls the service. Migrations must be backward compatible with the previous binary (expand, then
  contract) so a rollback is just redeploying the old image.
- **Backups**: managed PITR (7–14 days) plus a nightly logical dump to separate object storage in
  another account. A **restore drill every month** into a scratch database, with a script that
  checks row counts and runs the stock rebuild and report totals. Recorded in the runbook.
- **Secrets**: from the platform's secret manager; never in the repo. Rotation runbook for JWT
  keys, the TOTP encryption key, gateway keys and the database password.
- **Environments**: `local` (compose), `staging` (deployed from `main`, seeded demo tenant, gateway
  sandbox), `production` (deployed from tags).
- **Runbooks** (`docs/runbooks/`): restore, revoke a lost device, a stuck sync, rebuild stock,
  rotate secrets, operator account compromise, gateway webhook outage.

---

## 12. Decisions still open

These are the backend side of the roadmap's open questions. Each one should become an ADR when it
is decided.

| Decision | Needed by | Recommendation |
|---|---|---|
| Where the OpenAPI spec lives (option A or B, section 3) | Phase 0 / Phase 1 restructure | A now, revisit B in Phase 1 |
| RLS on from day one | Phase 0 | Yes (section 4.1) |
| PIN hashes on devices: accepted trade-off | Phase 0 | Write it up as an ADR (section 4.3) |
| Pricing/tax rounding rules | Before Phase 1 pilot | Draft in 4.8; confirm with an accountant |
| Hosting provider and region | Phase 0 | Jakarta region, managed Postgres |
| Payment gateway | Phase 2 (apply in Phase 0) | **Decided: doit.id** (platform sub-merchants, QRIS; see 6.5.1). Open: who pays the fee |
| Open-bill concurrency model | Phase 4 | Event-sourced orders, connectivity required for shared bills |
| Pricing unit (per outlet / device / tier) | Phase 5 | Schema supports all; decide commercially |

## 13. Inconsistencies spotted in the source documents

- ADR 0002 says the multi-outlet UI waits until **Phase 5**; the roadmap puts it in **Phase 6**.
  The roadmap is newer, so this plan follows it. ADR 0002 should get a superseding note.
- ADR 0008's timing table matches the roadmap, but ADR 0002's suggested libraries are "defaults,
  not commitments"; section 2 of this plan commits to them. Record that as an ADR or a status
  update on ADR 0002.
- The ADRs and roadmap live in the back-office repo while this repo holds the backend. Once the
  monorepo exists (Phase 1), move the ADRs to the root so the backend's decisions are recorded
  next to the backend code.
