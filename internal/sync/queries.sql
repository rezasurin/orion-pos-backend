-- name: InsertInbox :execrows
-- Does nothing when the event is already there (same device and key) or its id was used by another
-- device; the caller reads the existing row to tell which. Zero rows means "already there".
INSERT INTO sync_inbox (tenant_id, device_id, idempotency_key, id, outlet_id, staff_id, event_type, schema_version,
                        device_time, payload_hash, payload, status, received_at)
VALUES (@tenant_id, @device_id, @idempotency_key, @id, @outlet_id, @staff_id, @event_type, @schema_version,
        @device_time, @payload_hash, @payload, 'received', @received_at)
ON CONFLICT DO NOTHING;

-- name: GetInboxByKey :one
SELECT * FROM sync_inbox WHERE tenant_id = @tenant_id AND device_id = @device_id AND idempotency_key = @idempotency_key;

-- name: GetInboxForUpdate :one
SELECT * FROM sync_inbox WHERE tenant_id = @tenant_id AND device_id = @device_id AND idempotency_key = @idempotency_key FOR UPDATE;

-- name: SetInboxOutcome :exec
UPDATE sync_inbox
SET status = @status, code = sqlc.narg(code), detail = sqlc.narg(detail), depends_on = sqlc.narg(depends_on),
    applied_at = sqlc.narg(applied_at)
WHERE tenant_id = @tenant_id AND device_id = @device_id AND idempotency_key = @idempotency_key;

-- name: ListParkedOn :many
-- The events waiting for any of these records, oldest first, locked so two pushes cannot run the
-- same one.
SELECT * FROM sync_inbox
WHERE tenant_id = @tenant_id AND status = 'pending_dependency' AND depends_on = ANY(@ids::uuid[])
ORDER BY received_at, id
FOR UPDATE;

-- Device health monitor (health.go). These run as orion_platform, across tenants, and read the device
-- and shift tables read-only.

-- name: FindUnsyncedDevices :many
-- Devices that said they hold events older than the threshold, among those seen recently.
SELECT d.id, d.tenant_id, d.name, d.unsynced_events, d.oldest_unsynced_at, d.last_sync_at, o.name AS outlet_name
FROM device d
JOIN tenant t ON t.id = d.tenant_id
JOIN outlet o ON o.tenant_id = d.tenant_id AND o.id = d.outlet_id
WHERE d.revoked_at IS NULL AND t.suspended_at IS NULL
  AND d.unsynced_events > 0 AND d.oldest_unsynced_at < @unsynced_before
  AND d.last_seen_at > @active_after;

-- name: FindSilentOpenShiftDevices :many
-- Devices with a shift still open that have not been heard from since the threshold.
SELECT d.id, d.tenant_id, d.name, o.name AS outlet_name, sh.id AS shift_id, sh.opened_at,
       coalesce(greatest(d.last_seen_at, d.last_sync_at), d.paired_at) AS last_contact
FROM device d
JOIN tenant t ON t.id = d.tenant_id
JOIN outlet o ON o.tenant_id = d.tenant_id AND o.id = d.outlet_id
JOIN shift sh ON sh.device_id = d.id AND sh.tenant_id = d.tenant_id AND sh.closed_at IS NULL AND sh.received_at > @opened_after
WHERE d.revoked_at IS NULL AND t.suspended_at IS NULL
  AND coalesce(greatest(d.last_seen_at, d.last_sync_at), d.paired_at) < @silent_before;

-- name: ListOpenAlerts :many
SELECT a.id, a.tenant_id, a.device_id, a.kind, a.opened_at, a.detail, a.notified_at, d.name AS device_name, o.name AS outlet_name
FROM device_alert a
JOIN device d ON d.tenant_id = a.tenant_id AND d.id = a.device_id
JOIN outlet o ON o.tenant_id = d.tenant_id AND o.id = d.outlet_id
WHERE a.resolved_at IS NULL
ORDER BY a.opened_at, a.id;

-- name: InsertAlert :execrows
-- Does nothing when an incident of this kind is already open for the device.
INSERT INTO device_alert (id, tenant_id, device_id, kind, opened_at, detail)
VALUES (@id, @tenant_id, @device_id, @kind, @opened_at, @detail)
ON CONFLICT DO NOTHING;

-- name: ResolveAlert :exec
UPDATE device_alert SET resolved_at = @resolved_at WHERE tenant_id = @tenant_id AND id = @id AND resolved_at IS NULL;

-- name: MarkAlertNotified :exec
UPDATE device_alert SET notified_at = @notified_at WHERE tenant_id = @tenant_id AND id = @id;

-- name: ListOwnerContacts :many
-- Who to tell: the business's owners with a verified email address.
SELECT u.email, u.locale
FROM tenant_member m JOIN user_account u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id AND m.is_owner AND u.email_verified_at IS NOT NULL
ORDER BY u.email;
