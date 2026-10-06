-- name: InsertTenant :one
-- Early access tenants have the early_access status (ADR 0007); any other plan starts active.
INSERT INTO tenant (id, name, slug, plan_id, subscription_status)
SELECT @id, @name, @slug, p.id,
       CASE WHEN p.code = 'early_access' THEN 'early_access' ELSE 'active' END::subscription_status
FROM plan p
WHERE p.code = @plan_code
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

-- Queries below run as orion_platform, from operator tooling.

-- name: GetTenantBySlug :one
SELECT * FROM tenant WHERE slug = @slug;

-- name: SetTenantSuspended :exec
UPDATE tenant SET suspended_at = sqlc.narg(suspended_at) WHERE id = @id;

-- name: GetOutletSettingsForUpdate :one
SELECT * FROM outlet_settings WHERE tenant_id = @tenant_id AND outlet_id = @outlet_id FOR UPDATE;

-- name: UpdateOutletSettings :one
UPDATE outlet_settings
SET timezone = @timezone, business_day_cutoff = @business_day_cutoff, price_includes_tax = @price_includes_tax,
    tax_rate_bp = @tax_rate_bp, service_charge_rate_bp = @service_charge_rate_bp,
    service_charge_taxable = @service_charge_taxable, cash_rounding_unit = @cash_rounding_unit,
    cash_rounding_mode = @cash_rounding_mode, receipt_header = @receipt_header, receipt_footer = @receipt_footer
WHERE tenant_id = @tenant_id AND outlet_id = @outlet_id
RETURNING *;
