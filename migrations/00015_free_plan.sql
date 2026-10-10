-- The free plan (BACKEND_PLAN.md task B2.2): what a self-serve signup gets. Values are written out
-- rather than left to the key defaults, so changing a default never changes what free tenants have.
-- Operators lift a limit for one tenant with an override, or move a design partner to early_access.

-- +goose Up
INSERT INTO plan (id, code, name, is_public)
VALUES ('01a1a1e0-6f2b-7c3d-9e4f-5a6b7c8d9e0f', 'free', 'Free', true);

INSERT INTO plan_entitlement (plan_id, key, value)
SELECT p.id, v.key, v.value
FROM plan p
CROSS JOIN (VALUES
    ('module.inventory', 0),
    ('module.restaurant', 0),
    ('limit.outlets', 1),
    ('limit.devices', 2),
    ('limit.staff', 5)
) AS v (key, value)
WHERE p.code = 'free';

-- +goose Down
DELETE FROM plan_entitlement WHERE plan_id = (SELECT id FROM plan WHERE code = 'free');
DELETE FROM plan WHERE code = 'free';
