# Deploying and running Orion

## Database roles

Row-level security depends on the service connecting with the right role, so each environment
needs five roles:

| Role | Kind | Used by | Notes |
|---|---|---|---|
| schema owner (for example `orion_owner`) | LOGIN | `orion migrate` only | Owns every table, so it bypasses row-level security. Never give it to the running service. Needs `CREATEROLE` for the first migration, or create the two group roles below by hand first. |
| `orion_app` | NOLOGIN group | — | Created by the first migration. Policies confine it to the tenant in `app.tenant_id`. |
| `orion_platform` | NOLOGIN group | — | Created by the first migration. Policies let it see every tenant. |
| service login (for example `orion_api`) | LOGIN, member of `orion_app` | `orion serve` (`ORION_DATABASE_URL`) | Must not be a superuser, have `BYPASSRLS`, or own the tables. `orion serve` refuses to start otherwise. |

| worker login (for example `orion_admin`) | LOGIN, member of `orion_platform` | `orion worker` (`ORION_PLATFORM_DATABASE_URL`) | Jobs span tenants, so this role's policies allow every row. The same restrictions apply as to the service login, and `orion worker` checks them. |

Setup on a fresh managed PostgreSQL, run as the owner after the first `orion migrate up`:

```sql
CREATE ROLE orion_api LOGIN PASSWORD '<from the secret store>' IN ROLE orion_app;
CREATE ROLE orion_admin LOGIN PASSWORD '<from the secret store>' IN ROLE orion_platform;
```

The schema owner must not be subject to row-level security on its own tables (do not use
`FORCE ROW LEVEL SECURITY`): the login lookup `auth_find_user()` is `SECURITY DEFINER` and relies
on that to find an account before a tenant is known.

## Local development

```sh
make db-up     # Postgres 17 in Docker, with the roles from deploy/initdb
make run       # migrate, then serve on :8080
make worker    # in a second terminal: background jobs; emails are written to the log
curl localhost:8080/readyz
```

`make test` needs Docker (tests start their own Postgres with testcontainers), or a superuser URL
in `ORION_TEST_DATABASE_URL`.

## Configuration

See `.env.example`. Every variable starts with `ORION_`.

Outside local development the service needs JWT signing keys (`ORION_JWT_TENANT_KEYS`,
`ORION_JWT_DEVICE_KEYS`). To rotate one, put the new key first and keep the old one after it
until every token it signed has expired (access tokens: 15 minutes for users, 30 minutes for
devices), then remove it.

The operator console needs `ORION_PLATFORM_DATABASE_URL` on `orion serve` too, `ORION_SECRETS_KEY`
(encrypts TOTP seeds; losing it locks every operator out, so keep it in the secret store and back
it up apart from the database) and `ORION_JWT_OPERATOR_KEYS`. Create the first operator with
`orion admin create-operator --email you@example.com --reason bootstrap` and store what it prints.

There is no email provider yet. `ORION_EMAIL_PROVIDER=log` writes each message, including
verification links, to the log, so `orion worker` refuses it when `ORION_ENV=production`.

## Not decided yet

Hosting, backups and the deploy pipeline are task B0.13 in `docs/BACKEND_PLAN.md` and wait on the
hosting decision.
