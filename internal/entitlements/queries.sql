-- name: ListEntitlements :many
-- Every key with its resolved inputs for the current tenant, in one query. The tenant's plan comes
-- from its own row, which row-level security lets the service read.
SELECT k.key, k.kind, k.category, k.default_value,
       pe.value AS plan_value,
       o.value  AS override_value
FROM entitlement_key k
JOIN tenant t ON t.id = @tenant_id
LEFT JOIN plan_entitlement pe ON pe.plan_id = t.plan_id AND pe.key = k.key
LEFT JOIN tenant_entitlement_override o
       ON o.tenant_id = t.id AND o.key = k.key AND (o.expires_at IS NULL OR o.expires_at > @now)
ORDER BY k.key;

-- name: GetEntitlement :one
SELECT k.key, k.kind, k.category, k.default_value,
       pe.value AS plan_value,
       o.value  AS override_value
FROM entitlement_key k
JOIN tenant t ON t.id = @tenant_id
LEFT JOIN plan_entitlement pe ON pe.plan_id = t.plan_id AND pe.key = k.key
LEFT JOIN tenant_entitlement_override o
       ON o.tenant_id = t.id AND o.key = k.key AND (o.expires_at IS NULL OR o.expires_at > @now)
WHERE k.key = @key;

-- Queries below run as orion_platform.

-- name: GetKeyKind :one
SELECT kind FROM entitlement_key WHERE key = @key;

-- name: UpsertOverride :exec
INSERT INTO tenant_entitlement_override (tenant_id, key, value, reason, expires_at, set_by_operator_id)
VALUES (@tenant_id, @key, @value, @reason, sqlc.narg(expires_at), sqlc.narg(set_by_operator_id))
ON CONFLICT (tenant_id, key) DO UPDATE
SET value = EXCLUDED.value, reason = EXCLUDED.reason, expires_at = EXCLUDED.expires_at,
    set_by_operator_id = EXCLUDED.set_by_operator_id;

-- name: DeleteOverride :execrows
DELETE FROM tenant_entitlement_override WHERE tenant_id = @tenant_id AND key = @key;

-- name: GetOverride :one
SELECT value, reason, expires_at FROM tenant_entitlement_override WHERE tenant_id = @tenant_id AND key = @key;

-- name: ListTenantEntitlementsAdmin :many
-- Every key for one tenant with all its inputs, including an override that has expired, for the
-- operator console.
SELECT k.key, k.kind, k.category, k.default_value,
       pe.value AS plan_value,
       o.value AS override_value, o.reason AS override_reason, o.expires_at AS override_expires_at
FROM entitlement_key k
JOIN tenant t ON t.id = @tenant_id
LEFT JOIN plan_entitlement pe ON pe.plan_id = t.plan_id AND pe.key = k.key
LEFT JOIN tenant_entitlement_override o ON o.tenant_id = t.id AND o.key = k.key
ORDER BY k.key;

-- name: ListKeys :many
SELECT key, kind, category, description, owner, is_temporary, default_value FROM entitlement_key ORDER BY key;

-- name: ListPlanValues :many
SELECT p.code AS plan_code, pe.key, pe.value
FROM plan_entitlement pe JOIN plan p ON p.id = pe.plan_id
ORDER BY p.code, pe.key;

-- name: GetKeyForUpdate :one
SELECT key, kind, category, default_value FROM entitlement_key WHERE key = @key FOR UPDATE;

-- name: SetKeyDefault :exec
UPDATE entitlement_key SET default_value = @default_value WHERE key = @key;
