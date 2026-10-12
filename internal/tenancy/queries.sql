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

-- Operator tooling (admin.go), as orion_platform.

-- name: GetTenantPlanCode :one
SELECT p.code FROM tenant t JOIN plan p ON p.id = t.plan_id WHERE t.id = @id;

-- name: SetTenantPlan :execrows
-- Moving to early_access also restores its status; any other plan is an active subscription until
-- billing (Phase 5) says otherwise.
UPDATE tenant
SET plan_id = p.id,
    subscription_status = CASE WHEN p.code = 'early_access' THEN 'early_access' ELSE 'active' END::subscription_status
FROM plan p
WHERE tenant.id = @id AND p.code = @plan_code;

-- name: ActiveAnnouncements :many
-- Row-level security limits this to the announcements to everyone and to the current tenant.
SELECT id, severity, title, body, starts_at, ends_at FROM announcement
WHERE starts_at <= @now AND (ends_at IS NULL OR ends_at > @now)
ORDER BY starts_at DESC, id
LIMIT 20;
