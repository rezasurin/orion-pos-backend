# Deploying and running Orion

## Database roles

Row-level security depends on the service connecting with the right role, so each environment
needs four roles:

| Role | Kind | Used by | Notes |
|---|---|---|---|
| schema owner (for example `orion_owner`) | LOGIN | `orion migrate` only | Owns every table, so it bypasses row-level security. Never give it to the running service. Needs `CREATEROLE` for the first migration, or create the two group roles below by hand first. |
| `orion_app` | NOLOGIN group | — | Created by the first migration. Policies confine it to the tenant in `app.tenant_id`. |
| `orion_platform` | NOLOGIN group | — | Created by the first migration. Policies let it see every tenant. |
| service login (for example `orion_api`) | LOGIN, member of `orion_app` | `orion serve` (`ORION_DATABASE_URL`) | Must not be a superuser, have `BYPASSRLS`, or own the tables. `orion serve` refuses to start otherwise. |

The platform module and cross-tenant jobs will get their own login (for example
`orion_admin`, a member of `orion_platform`) when they arrive.

Setup on a fresh managed PostgreSQL, run as the owner after the first `orion migrate up`:

```sql
CREATE ROLE orion_api LOGIN PASSWORD '<from the secret store>' IN ROLE orion_app;
```

## Local development

```sh
make db-up     # Postgres 17 in Docker, with the roles from deploy/initdb
make run       # migrate, then serve on :8080
curl localhost:8080/readyz
```

`make test` needs Docker (tests start their own Postgres with testcontainers), or a superuser URL
in `ORION_TEST_DATABASE_URL`.

## Configuration

See `.env.example`. Every variable starts with `ORION_`.

## Not decided yet

Hosting, backups and the deploy pipeline are task B0.13 in `docs/BACKEND_PLAN.md` and wait on the
hosting decision.
