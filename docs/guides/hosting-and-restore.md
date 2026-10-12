# Guide: hosting, deploying and restoring Orion

Status: **no environment exists yet.** This is the guide for standing up staging and production
yourself, and for the restore drill (plan task B0.13). The architecture is in
`docs/BACKEND_PLAN.md` section 11; database roles and configuration are in `deploy/README.md`.
Read both first.

## What you are deploying

One container image (`deploy/Dockerfile`, a static binary on distroless) run as **two processes**
from the same image, plus Postgres:

| Process | Command | Database role | Needs |
|---|---|---|---|
| API | `orion serve` | `ORION_DATABASE_URL` as the `orion_app` member (for example `orion_api`) | public HTTPS, `/healthz` and `/readyz` |
| Worker | `orion worker` | `ORION_PLATFORM_DATABASE_URL` as the `orion_platform` member (for example `orion_admin`) | no inbound traffic; sends email |
| Migration (step, not a service) | `orion migrate up` | `ORION_MIGRATE_DATABASE_URL`, the schema owner | runs before the new API starts |

The API refuses to start if its database role is a superuser, has `BYPASSRLS` or owns the tables.
That is deliberate: it is what keeps one business's data from another's. Do not work around it.

## 1. Choose where it runs

Recommendation from the plan: a **Jakarta region** (AWS `ap-southeast-3`, GCP
`asia-southeast2`) for latency and to keep personal data in Indonesia, a **single small container
service or VM**, and **managed PostgreSQL with point-in-time recovery**. One API instance and one
worker instance are enough for the pilot and well beyond. Decide, then write the decision down
(ADR). Pick a managed Postgres that gives you: PITR with 7 to 14 days retention, automated
snapshots, a role that can `CREATE ROLE`, and the ability to restore into a new instance.

## 2. Provision, once per environment

Create `staging` (deployed from `main`, demo data, gateway sandbox later) and `production`
(deployed from tags). For each:

1. **Postgres** (version 17 locally; use the same major version or newer). Create the database and
   the schema-owner login. Run `orion migrate up` once, then create the two service logins as shown
   in `deploy/README.md`:

   ```sql
   CREATE ROLE orion_api   LOGIN PASSWORD '<from the secret store>' IN ROLE orion_app;
   CREATE ROLE orion_admin LOGIN PASSWORD '<from the secret store>' IN ROLE orion_platform;
   ```

   Require TLS (`sslmode=require` or `verify-full` in the URLs). Put the database in a private
   network, reachable only by the API, worker and the deploy job.
2. **Secrets** in the platform's secret manager, never in the repo or the image:
   `ORION_DATABASE_URL`, `ORION_PLATFORM_DATABASE_URL`, `ORION_MIGRATE_DATABASE_URL`,
   `ORION_JWT_TENANT_KEYS`, `ORION_JWT_DEVICE_KEYS`, `ORION_JWT_OPERATOR_KEYS`,
   `ORION_SECRETS_KEY`, plus the email and gateway keys later. Generate each key with
   `openssl rand -base64 32 | tr '+/' '-_' | tr -d '='` and give it an id (`k1:<secret>`).
   **Back up `ORION_SECRETS_KEY` somewhere other than the database backups**: losing it locks every
   operator out of the console.
3. **Plain configuration**: `ORION_ENV=production`, `ORION_PUBLIC_URL` (the real front-end URL),
   `ORION_HTTP_ADDR`, `ORION_LOG_FORMAT=json`, `ORION_SENTRY_DSN`, and
   `ORION_TRUST_PROXY=true` **only** if exactly one proxy or load balancer sits in front and
   appends the caller's address to `X-Forwarded-For` (rate limits key on the client address; a
   wrong value either lets attackers dodge limits or blocks everyone behind one address).
4. **TLS and a hostname** for the API (for example `api.orion.example`), terminated at the load
   balancer or the platform. Health checks: `GET /healthz` (alive) and `GET /readyz` (database
   reachable).
5. **First operator**: `orion admin create-operator --email you@example.com --reason bootstrap`
   and store what it prints.
6. **Email**: Resend, set up by `docs/guides/email-provider.md` (domain DNS, then
   `ORION_EMAIL_PROVIDER=resend`, `ORION_EMAIL_API_KEY`, `ORION_EMAIL_FROM`). Production refuses
   to run the worker with `ORION_EMAIL_PROVIDER=log`.

## 3. Deploy pipeline (on tag)

Extend `.github/workflows/ci.yml` (it already lints, checks generated code, tests and builds the
image) with a release workflow. Shape:

1. Trigger on a tag (`v*`) for production, and on pushes to `main` for staging.
2. Build and push the image to your registry with `--build-arg VERSION=<tag>`. Tag it with the git
   tag and the commit sha. Use the cloud's workload identity or an OIDC role for credentials, not
   long-lived keys in GitHub secrets, where the platform allows it.
3. Run `orion migrate up` as a one-off task with the owner URL, **before** rolling the services.
4. Roll the API, then the worker, with health checks gating each step.
5. Smoke test: `curl -fsS https://api.../readyz`, then log in as the demo owner on staging.

**Migrations must be backward compatible with the previous binary** (add the column first, deploy
code that uses it, drop the old one in a later release), so a rollback is just redeploying the
previous image. Never edit a migration that has run anywhere; add a new one. `orion migrate down`
exists for local use; do not use it in production.

## 4. Backups

Two layers, both needed:

1. **Managed point-in-time recovery**, 7 to 14 days. This is what you use for "we deleted
   something an hour ago".
2. **A nightly logical dump to a different account or project** (object storage with versioning
   and a retention rule of, say, 35 days). This is what you use if the whole cloud account or the
   database instance is lost. For example, from a scheduled job that has the owner URL:

   ```sh
   pg_dump --format=custom --no-owner --no-privileges "$ORION_MIGRATE_DATABASE_URL" \
     | <upload to the other account's bucket as orion-$(date -u +%F).dump>
   ```

   Encrypt at rest (bucket encryption) and restrict who can read it: it holds every business's
   sales and the staff PIN hashes.

Alert if the nightly job fails or its file is much smaller than yesterday's.

## 5. The restore drill

A backup nobody has restored is a hope, not a backup. Do the drill **once before the first pilot**
and then **monthly**, and after any change to how backups are taken.

`deploy/restore-drill.sh` does the mechanical part: it restores a dump into a scratch database,
then compares the migration version and the row count of every table with the source, and prints
PASS or FAIL. It exits non-zero on FAIL, so it can run from a scheduled job.

```sh
# against the live database (reads only; the owner URL so row-level security does not hide rows):
SOURCE_URL="$ORION_MIGRATE_DATABASE_URL" \
SCRATCH_ADMIN_URL="postgres://<admin>@<scratch server>/postgres?sslmode=require" \
  deploy/restore-drill.sh

# against last night's dump from the other account (download it first):
DUMP_FILE=./orion-2026-10-04.dump SOURCE_URL=... SCRATCH_ADMIN_URL=... deploy/restore-drill.sh
```

Use a scratch server that is **not** production (a throwaway instance, or a local Postgres), so a
mistake cannot touch live data. When the source is live and busy, counts can differ by the sales
that arrived during the dump; run it at a quiet hour, or restore a dump and compare it with a
snapshot taken at the same moment.

Then do the part a script cannot:

1. **Point-in-time restore**: in the managed service, restore to a new instance at a time ten
   minutes ago. Note how long it took. Run the drill against the new instance.
2. **Run the application on the restored database** (a scratch staging pointing at it) and check
   that the report for a recent day matches the production report for the same day:
   `GET /v1/reports/days/{date}?outlet_id=...`. Reports are computed live from the sales, so
   matching totals prove the sales rows are intact. Reconnect devices only to a copy: a device
   whose server was restored to an earlier point sees its cursor ahead of the server and is sent a
   full snapshot, which the apps handle; its outbox is unaffected and re-sends anything the
   restore lost (idempotently).
3. **Write it down** in `docs/runbooks/restore.md`: date, who, which backup, how long it took,
   PASS or FAIL, and anything surprising. The first version of that file should also list the
   exact commands for a real restore under pressure (find the latest backup, create the instance,
   recreate the two service roles with `deploy/README.md`, run the application, point DNS or the
   secret at it, check `/readyz`, and tell the pilot cafe).

### What a real restore needs besides the data

* The two service roles (`orion_api`, `orion_admin`) and their passwords. `--no-privileges` dumps
  carry no role setup; the first migration creates the group roles, the SQL in `deploy/README.md`
  creates the logins.
* `ORION_SECRETS_KEY` and the JWT keys. Without the first, operators cannot sign in; without the
  others every session and device token is invalid and users sign in again, which is survivable.
* River's job tables come back with the dump; jobs that were pending resume.

## 6. Secret rotation (write the runbook as you go)

| Secret | How |
|---|---|
| JWT keys | Put the new key **first**, keep the old one after it until every token it signed has expired (users 15 minutes, devices 30 minutes), then remove the old. No downtime. |
| Database passwords | Create the new password, update the secret, redeploy, then change the role. Do the API and worker logins separately. |
| `ORION_SECRETS_KEY` | Cannot be rotated in place: it encrypts operator TOTP seeds. Plan: re-enrol operators with the new key. Do not lose the old one before that. |
| Gateway and email keys | Per provider; keep two active during the change. |

## 7. Checklist

- [ ] Hosting and region chosen and written down (ADR)
- [ ] Staging up: migrated, roles created, deployed from `main`, demo data seeded
- [ ] Production up: deployed from a tag, first operator created
- [ ] `/readyz` monitored with an alert; Sentry DSN set; logs kept 30 days
- [ ] Managed PITR on; nightly logical dump to the other account; failure alert
- [ ] `docs/runbooks/restore.md` written and the first drill recorded **before the pilot**
- [ ] Secrets in the secret manager; `ORION_SECRETS_KEY` backed up separately
- [ ] Email provider live (`docs/guides/email-provider.md`)
- [ ] CORS or same-origin proxy settled with the front end
