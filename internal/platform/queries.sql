-- All queries here run as orion_platform.

-- name: InsertOperator :exec
INSERT INTO operator (id, email, password_hash, totp_secret_enc) VALUES (@id, @email, @password_hash, @totp_secret_enc);

-- name: GetOperatorByEmail :one
SELECT * FROM operator WHERE email = @email;

-- name: GetOperator :one
SELECT * FROM operator WHERE id = @id;

-- name: GetOperatorForUpdate :one
SELECT * FROM operator WHERE id = @id FOR UPDATE;

-- name: CountOperators :one
SELECT count(*) FROM operator;

-- name: AdvanceTOTP :exec
-- Records the accepted time step and confirms enrolment on the first success.
UPDATE operator SET totp_last_step = @step, totp_confirmed_at = coalesce(totp_confirmed_at, @now::timestamptz)
WHERE id = @id;

-- name: InsertRecoveryCode :exec
INSERT INTO operator_recovery_code (id, operator_id, code_hash) VALUES (@id, @operator_id, @code_hash);

-- name: UseRecoveryCode :execrows
UPDATE operator_recovery_code SET used_at = @now
WHERE operator_id = @operator_id AND code_hash = @code_hash AND used_at IS NULL;

-- name: InsertAudit :exec
INSERT INTO platform_audit_log (id, operator_id, action, target_type, target_id, tenant_id, before, after, reason, ip, user_agent)
VALUES (@id, sqlc.narg(operator_id), @action, @target_type, sqlc.narg(target_id), sqlc.narg(tenant_id),
        sqlc.narg(before), sqlc.narg(after), @reason, sqlc.narg(ip), sqlc.narg(user_agent));

-- name: ListAudit :many
-- Newest first; the cursor is the id of the last row of the previous page.
SELECT id, operator_id, action, target_type, target_id, tenant_id, before, after, reason, coalesce(host(ip), '')::text AS ip, user_agent, created_at
FROM platform_audit_log
WHERE id < @before AND (sqlc.narg(tenant_id)::uuid IS NULL OR tenant_id = sqlc.narg(tenant_id)::uuid)
ORDER BY id DESC
LIMIT @page_size;

-- Support tooling (support.go): read-only views of one business's sync health, and the one write an
-- operator may make there. They run as orion_platform, across every table they touch.

-- name: SupportDevices :many
SELECT d.id, d.name, d.device_code, o.code AS outlet_code, d.revoked_at, d.last_seen_at, d.last_sync_at, d.app_version,
       d.clock_skew_ms, d.unsynced_events, d.oldest_unsynced_at,
       coalesce((SELECT array_agg(a.kind ORDER BY a.kind) FROM device_alert a
                 WHERE a.tenant_id = d.tenant_id AND a.device_id = d.id AND a.resolved_at IS NULL), '{}')::text[] AS open_alerts
FROM device d JOIN outlet o ON o.tenant_id = d.tenant_id AND o.id = d.outlet_id
WHERE d.tenant_id = @tenant_id
ORDER BY o.code, d.device_code;

-- name: SupportParked :many
-- Events waiting for a record that has not arrived, oldest first.
SELECT i.id, i.event_type, i.depends_on, i.received_at, d.name AS device_name
FROM sync_inbox i JOIN device d ON d.tenant_id = i.tenant_id AND d.id = i.device_id
WHERE i.tenant_id = @tenant_id AND i.status = 'pending_dependency'
ORDER BY i.received_at, i.id
LIMIT 100;

-- name: SupportRejected :many
SELECT coalesce(code, '')::text AS code, count(*)::bigint AS events
FROM sync_inbox
WHERE tenant_id = @tenant_id AND status = 'rejected' AND received_at >= @since
GROUP BY code ORDER BY events DESC, code;

-- name: SupportFlagCounts :many
SELECT code, count(*)::bigint AS flags FROM flag
WHERE tenant_id = @tenant_id AND created_at >= @since
GROUP BY code ORDER BY flags DESC, code;

-- name: SupportFlaggedSales :many
-- The sales most recently flagged, with their flag codes.
SELECT s.id, s.receipt_number, o.code AS outlet_code, s.business_date, s.total, s.status,
       array_agg(DISTINCT f.code ORDER BY f.code)::text[] AS codes, max(f.created_at)::timestamptz AS flagged_at
FROM flag f
JOIN sale s ON s.tenant_id = f.tenant_id AND s.id = f.target_id
JOIN outlet o ON o.tenant_id = s.tenant_id AND o.id = s.outlet_id
WHERE f.tenant_id = @tenant_id AND f.created_at >= @since
GROUP BY s.id, s.receipt_number, o.code, s.business_date, s.total, s.status
ORDER BY flagged_at DESC, s.id
LIMIT 20;

-- name: GetParkedEventForUpdate :one
SELECT tenant_id, device_id, idempotency_key, id, event_type, depends_on FROM sync_inbox
WHERE tenant_id = @tenant_id AND id = @id AND status = 'pending_dependency'
FOR UPDATE;

-- name: AbandonParkedEvent :exec
UPDATE sync_inbox SET status = 'rejected', code = 'abandoned', detail = @detail, depends_on = NULL
WHERE tenant_id = @tenant_id AND device_id = @device_id AND idempotency_key = @idempotency_key;

-- Usage metrics (metrics.go). Counts only (ADR 0008).

-- name: ComputeDailyMetrics :execrows
-- Recomputes the days from from_day to to_day for every business that existed then, replacing what
-- was there: a tablet that was offline for days changes the days its sales belong to. Sales count by
-- their business date; sync figures by the day they arrived, in Asia/Jakarta.
-- ponytail: scans the date range of sale, sync_inbox and flag across tenants once a night; add
-- indexes on those dates if the run gets slow.
WITH days AS (
    SELECT t.id AS tenant_id, d::date AS day
    FROM tenant t CROSS JOIN generate_series(@from_day::date, @to_day::date, interval '1 day') AS d
    WHERE d::date >= (t.created_at AT TIME ZONE 'Asia/Jakarta')::date
), sales_day AS (
    SELECT sa.tenant_id, sa.business_date AS day, count(*) AS sales, count(*) FILTER (WHERE sa.status = 'voided') AS voided
    FROM sale sa WHERE sa.business_date BETWEEN @from_day::date AND @to_day::date
    GROUP BY 1, 2
), events_day AS (
    SELECT i.tenant_id, (i.received_at AT TIME ZONE 'Asia/Jakarta')::date AS day, count(*) AS events,
           count(*) FILTER (WHERE i.status = 'rejected') AS rejected, count(DISTINCT i.device_id) AS devices
    FROM sync_inbox i
    WHERE i.received_at >= (sqlc.arg(from_day)::date::timestamp AT TIME ZONE 'Asia/Jakarta')
      AND i.received_at < ((sqlc.arg(to_day)::date + 1)::timestamp AT TIME ZONE 'Asia/Jakarta')
    GROUP BY 1, 2
), flags_day AS (
    SELECT fl.tenant_id, (fl.created_at AT TIME ZONE 'Asia/Jakarta')::date AS day, count(*) AS flags
    FROM flag fl
    WHERE fl.created_at >= (sqlc.arg(from_day)::date::timestamp AT TIME ZONE 'Asia/Jakarta')
      AND fl.created_at < ((sqlc.arg(to_day)::date + 1)::timestamp AT TIME ZONE 'Asia/Jakarta')
    GROUP BY 1, 2
)
INSERT INTO tenant_daily_metrics (tenant_id, day, sales, voided_sales, events, rejected_events, devices_synced, flags, computed_at)
SELECT days.tenant_id, days.day, coalesce(s.sales, 0), coalesce(s.voided, 0), coalesce(e.events, 0), coalesce(e.rejected, 0),
       coalesce(e.devices, 0), coalesce(f.flags, 0), @now::timestamptz
FROM days
LEFT JOIN sales_day s ON s.tenant_id = days.tenant_id AND s.day = days.day
LEFT JOIN events_day e ON e.tenant_id = days.tenant_id AND e.day = days.day
LEFT JOIN flags_day f ON f.tenant_id = days.tenant_id AND f.day = days.day
ON CONFLICT (tenant_id, day) DO UPDATE SET
    sales = excluded.sales, voided_sales = excluded.voided_sales, events = excluded.events,
    rejected_events = excluded.rejected_events, devices_synced = excluded.devices_synced, flags = excluded.flags,
    computed_at = excluded.computed_at;

-- name: ListDailyMetrics :many
SELECT * FROM tenant_daily_metrics
WHERE tenant_id = @tenant_id AND day BETWEEN @from_day::date AND @to_day::date
ORDER BY day;

-- name: StoppedSyncing :many
-- Tablets in use that have gone quiet: not revoked, in a business that is not suspended, that synced
-- (or were paired) within the window but not since `quiet_since`. Oldest silence first.
SELECT t.slug AS tenant_slug, t.name AS tenant_name, o.code AS outlet_code, d.id AS device_id, d.name AS device_name,
       d.device_code, d.paired_at, d.last_sync_at, d.last_seen_at, d.app_version, d.unsynced_events, d.oldest_unsynced_at
FROM device d
JOIN tenant t ON t.id = d.tenant_id
JOIN outlet o ON o.tenant_id = d.tenant_id AND o.id = d.outlet_id
WHERE d.revoked_at IS NULL AND t.suspended_at IS NULL
  AND coalesce(d.last_sync_at, d.paired_at) < @quiet_since::timestamptz
  AND coalesce(d.last_sync_at, d.paired_at) >= @window_start::timestamptz
ORDER BY coalesce(d.last_sync_at, d.paired_at), d.id
LIMIT 500;

-- The operator console (console.go).

-- name: AdminTenants :many
-- A page of businesses with what an operator scans for: plan, state, size and the last week's use.
-- With id set, that one business.
SELECT t.id, t.slug, t.name, p.code AS plan_code, t.subscription_status::text AS subscription_status,
       t.suspended_at, t.created_at,
       (SELECT count(*) FROM outlet o WHERE o.tenant_id = t.id)::integer AS outlets,
       (SELECT count(*) FROM device d WHERE d.tenant_id = t.id AND d.revoked_at IS NULL)::integer AS devices,
       (SELECT d.last_sync_at FROM device d WHERE d.tenant_id = t.id AND d.last_sync_at IS NOT NULL
        ORDER BY d.last_sync_at DESC LIMIT 1) AS last_sync_at,
       coalesce((SELECT sum(m.sales) FROM tenant_daily_metrics m WHERE m.tenant_id = t.id AND m.day >= @since_day::date), 0)::integer AS sales_7d,
       coalesce((SELECT sum(m.events) FROM tenant_daily_metrics m WHERE m.tenant_id = t.id AND m.day >= @since_day::date), 0)::integer AS events_7d
FROM tenant t JOIN plan p ON p.id = t.plan_id
WHERE t.id > @after
  AND (sqlc.narg(id)::uuid IS NULL OR t.id = sqlc.narg(id)::uuid)
  AND (@q::text = '' OR t.name ILIKE '%' || @q::text || '%' OR t.slug ILIKE '%' || @q::text || '%')
ORDER BY t.id
LIMIT @page_size;

-- name: GetDeviceForOperator :one
SELECT id, tenant_id, outlet_id, device_code, name, revoked_at FROM device WHERE id = @id FOR UPDATE;

-- name: RevokeDeviceByOperator :exec
UPDATE device SET revoked_at = @now, revoked_by_operator_id = @operator_id WHERE id = @id AND revoked_at IS NULL;

-- name: InsertTenantAuditAsSystem :exec
-- The business's own audit log, for something Orion staff did to it.
INSERT INTO tenant_audit_log (id, tenant_id, actor_type, action, target_type, target_id, detail)
VALUES (@id, @tenant_id, 'system', @action, @target_type, @target_id, @detail);
