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
