-- name: InsertTenant :one
-- Every new tenant starts on the early_access plan (ADR 0007).
INSERT INTO tenant (id, name, slug, plan_id)
SELECT @id, @name, @slug, p.id
FROM plan p
WHERE p.code = 'early_access'
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenant WHERE id = @id;

-- name: InsertOutlet :one
INSERT INTO outlet (id, tenant_id, name, code, address)
VALUES (@id, @tenant_id, @name, @code, @address)
RETURNING *;

-- name: InsertOutletSettings :one
INSERT INTO outlet_settings (outlet_id, tenant_id, timezone)
VALUES (@outlet_id, @tenant_id, @timezone)
RETURNING *;

-- name: RecordChange :one
SELECT record_change(@entity_type::text, @entity_id::uuid, @op::text, sqlc.narg(outlet_id)::uuid)::bigint AS seq;

-- Outlets come back joined with their settings in one query, never one query per outlet.

-- name: ListOutletsWithSettings :many
SELECT sqlc.embed(o), sqlc.embed(s)
FROM outlet o
JOIN outlet_settings s ON s.tenant_id = o.tenant_id AND s.outlet_id = o.id
WHERE o.tenant_id = @tenant_id AND o.archived_at IS NULL
ORDER BY o.code;

-- name: GetOutletWithSettings :one
SELECT sqlc.embed(o), sqlc.embed(s)
FROM outlet o
JOIN outlet_settings s ON s.tenant_id = o.tenant_id AND s.outlet_id = o.id
WHERE o.tenant_id = @tenant_id AND o.id = @id;
