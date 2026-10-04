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
WHERE id < @before
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
