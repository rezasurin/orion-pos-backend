-- Entitlements: what a tenant may use (BACKEND_PLAN.md section 4.5; ADR 0007, ADR 0008).
-- Resolution is: tenant override (unless expired), else plan value, else the key's default.
-- Values are integers: 0 or 1 for a bool, a count for a limit, and -1 for "unlimited".

-- +goose Up
CREATE TABLE entitlement_key (
    key           text PRIMARY KEY CHECK (key ~ '^(module|limit|flag)\.[a-z0-9_]+$'),
    kind          text NOT NULL CHECK (kind IN ('bool', 'int')),
    category      text NOT NULL CHECK (category IN ('module', 'limit', 'flag')),
    description   text NOT NULL,
    owner         text NOT NULL,          -- who answers for it; flags especially
    is_temporary  boolean NOT NULL DEFAULT false, -- a flag that should be removed once rolled out
    default_value bigint NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (key LIKE category || '.%'),
    CHECK (kind = 'int' OR default_value IN (0, 1)),
    CHECK (kind = 'int' OR category <> 'limit'),
    CHECK (default_value >= -1)
);

CREATE TABLE plan_entitlement (
    plan_id uuid NOT NULL REFERENCES plan (id),
    key     text NOT NULL REFERENCES entitlement_key (key),
    value   bigint NOT NULL CHECK (value >= -1),
    PRIMARY KEY (plan_id, key)
);

-- Set by operators. set_by_operator_id points at the operator table that arrives with B0.10.
CREATE TABLE tenant_entitlement_override (
    tenant_id          uuid NOT NULL REFERENCES tenant (id),
    key                text NOT NULL REFERENCES entitlement_key (key),
    value              bigint NOT NULL CHECK (value >= -1),
    reason             text NOT NULL CHECK (length(btrim(reason)) > 0),
    expires_at         timestamptz,
    set_by_operator_id uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);

CREATE TRIGGER tenant_entitlement_override_set_updated_at BEFORE UPDATE ON tenant_entitlement_override
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO entitlement_key (key, kind, category, description, owner, default_value) VALUES
    ('module.inventory',  'bool', 'module', 'Ingredients, recipes and the stock ledger', 'product', 0),
    ('module.restaurant', 'bool', 'module', 'Tables, open bills and the kitchen display',  'product', 0),
    ('limit.outlets',     'int',  'limit',  'Active outlets',                               'product', 1),
    ('limit.devices',     'int',  'limit',  'Paired, not revoked devices',                  'product', 2),
    ('limit.staff',       'int',  'limit',  'Active staff records',                         'product', 5);

-- Early access is unrestricted (ADR 0007): every module on, every limit unlimited.
INSERT INTO plan_entitlement (plan_id, key, value)
SELECT p.id, k.key, CASE WHEN k.kind = 'bool' THEN 1 ELSE -1 END
FROM plan p CROSS JOIN entitlement_key k
WHERE p.code = 'early_access';

ALTER TABLE tenant_entitlement_override ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_entitlement_override TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON tenant_entitlement_override TO orion_platform USING (true) WITH CHECK (true);

-- Keys and plan values are global configuration, like plan: readable by the service, written by
-- operators. Overrides are per tenant: the service reads its own, operators write them.
GRANT SELECT ON entitlement_key, plan_entitlement, tenant_entitlement_override TO orion_app;
GRANT SELECT, INSERT, UPDATE ON entitlement_key, plan_entitlement TO orion_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_entitlement_override TO orion_platform;

-- +goose Down
DROP TABLE tenant_entitlement_override;
DROP TABLE plan_entitlement;
DROP TABLE entitlement_key;
