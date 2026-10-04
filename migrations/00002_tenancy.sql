-- Plans, tenants, outlets, outlet settings and the change log (BACKEND_PLAN.md sections 4.1, 4.6,
-- 4.8, 5.2 and 6.1).

-- +goose Up
CREATE TYPE subscription_status AS ENUM (
    'early_access', 'trialing', 'active', 'past_due', 'grace', 'read_only', 'canceled'
);

CREATE TYPE cash_rounding_mode AS ENUM ('nearest', 'down', 'up');

-- Plans are global. Every tenant starts on early_access (ADR 0007).
CREATE TABLE plan (
    id         uuid PRIMARY KEY,
    code       text NOT NULL UNIQUE CHECK (code ~ '^[a-z][a-z0-9_]*$'),
    name       text NOT NULL,
    is_public  boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO plan (id, code, name, is_public)
VALUES ('01a0fd4a-4117-7ec2-a253-442cbce2f3d8', 'early_access', 'Early access', true);

CREATE TABLE tenant (
    id                  uuid PRIMARY KEY,
    name                text NOT NULL CHECK (length(btrim(name)) > 0),
    slug                text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    plan_id             uuid NOT NULL REFERENCES plan (id),
    subscription_status subscription_status NOT NULL DEFAULT 'early_access',
    trial_ends_at       timestamptz,
    paid_until          timestamptz,
    billing_starts_at   timestamptz, -- per-tenant override of the global date (ADR 0008)
    suspended_at        timestamptz, -- operator suspension, separate from billing status
    change_seq          bigint NOT NULL DEFAULT 0 CHECK (change_seq >= 0),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE outlet (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    name        text NOT NULL CHECK (length(btrim(name)) > 0),
    code        text NOT NULL CHECK (code ~ '^[A-Z0-9]{2,6}$'), -- prefix of receipt numbers (ADR 0004)
    address     text NOT NULL DEFAULT '',
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, code)
);

CREATE TABLE outlet_settings (
    outlet_id              uuid PRIMARY KEY,
    tenant_id              uuid NOT NULL,
    timezone               text NOT NULL DEFAULT 'Asia/Jakarta'
        CHECK (timezone IN ('Asia/Jakarta', 'Asia/Makassar', 'Asia/Jayapura')),
    business_day_cutoff    time NOT NULL DEFAULT '00:00',
    price_includes_tax     boolean NOT NULL DEFAULT false,
    tax_rate_bp            integer NOT NULL DEFAULT 0 CHECK (tax_rate_bp BETWEEN 0 AND 10000),
    service_charge_rate_bp integer NOT NULL DEFAULT 0 CHECK (service_charge_rate_bp BETWEEN 0 AND 10000),
    service_charge_taxable boolean NOT NULL DEFAULT true,
    cash_rounding_unit     integer NOT NULL DEFAULT 0 CHECK (cash_rounding_unit >= 0),
    cash_rounding_mode     cash_rounding_mode NOT NULL DEFAULT 'nearest',
    receipt_header         text NOT NULL DEFAULT '',
    receipt_footer         text NOT NULL DEFAULT '',
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id)
);

-- What changed for a tenant, in order, for the POS pull (BACKEND_PLAN.md section 5.2). seq is
-- gapless per tenant because it comes from tenant.change_seq under a row lock.
CREATE TABLE change_log (
    tenant_id   uuid NOT NULL REFERENCES tenant (id),
    seq         bigint NOT NULL CHECK (seq > 0),
    entity_type text NOT NULL,
    entity_id   uuid NOT NULL,
    op          text NOT NULL CHECK (op IN ('upsert', 'delete')),
    outlet_id   uuid, -- NULL when the change applies to every outlet
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, seq)
);

-- Indexes are added for a query that needs them, not for every column. Here the UNIQUE
-- constraints and primary keys already serve every access path: outlets by (tenant_id, code) and
-- (tenant_id, id), settings by outlet_id, and the pull's range scan on change_log
-- (tenant_id, seq > cursor). plan_id needs none: plan is tiny and its rows are never deleted.

CREATE TRIGGER tenant_set_updated_at BEFORE UPDATE ON tenant
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER outlet_set_updated_at BEFORE UPDATE ON outlet
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER outlet_settings_set_updated_at BEFORE UPDATE ON outlet_settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER change_log_append_only BEFORE UPDATE OR DELETE ON change_log
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Appends a change for the current tenant and returns its seq. Runs as the caller, so row-level
-- security still applies.
--
-- It takes a NO KEY UPDATE lock on the tenant row until the transaction ends, so changes to one
-- tenant are recorded one transaction at a time. To avoid deadlocks, a transaction that records a
-- change must take that lock first (callers run kernel.LockTenant before
-- touching other rows); see "Database conventions" in docs/BACKEND_PLAN.md.
-- +goose StatementBegin
CREATE FUNCTION record_change(p_entity_type text, p_entity_id uuid, p_op text, p_outlet_id uuid)
    RETURNS bigint
    LANGUAGE plpgsql
AS $$
DECLARE
    v_seq bigint;
BEGIN
    UPDATE tenant SET change_seq = change_seq + 1
    WHERE id = current_tenant_id()
    RETURNING change_seq INTO v_seq;

    IF v_seq IS NULL THEN
        RAISE EXCEPTION 'record_change: no tenant in context' USING ERRCODE = 'insufficient_privilege';
    END IF;

    INSERT INTO change_log (tenant_id, seq, entity_type, entity_id, op, outlet_id)
    VALUES (current_tenant_id(), v_seq, p_entity_type, p_entity_id, p_op, p_outlet_id);

    RETURN v_seq;
END
$$;
-- +goose StatementEnd

-- Row-level security. Every table with a tenant_id gets the same two policies; a schema test
-- (internal/tenancy) fails if a new table forgets them.
ALTER TABLE tenant ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant TO orion_app
    USING (id = current_tenant_id()) WITH CHECK (id = current_tenant_id());
CREATE POLICY platform_all ON tenant TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE outlet ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outlet TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON outlet TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE outlet_settings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outlet_settings TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON outlet_settings TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE change_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON change_log TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON change_log TO orion_platform USING (true) WITH CHECK (true);

-- Grants. No DELETE anywhere: business rows are archived, not deleted. The plan and billing
-- columns on tenant are written only by orion_platform.
GRANT SELECT ON plan TO orion_app;
GRANT SELECT, INSERT, UPDATE ON plan TO orion_platform;

GRANT SELECT, INSERT ON tenant TO orion_app;
GRANT UPDATE (name, slug, change_seq) ON tenant TO orion_app;
GRANT SELECT, INSERT, UPDATE ON tenant TO orion_platform;

GRANT SELECT, INSERT, UPDATE ON outlet, outlet_settings TO orion_app, orion_platform;

GRANT SELECT, INSERT ON change_log TO orion_app, orion_platform;

-- +goose Down
DROP FUNCTION record_change(text, uuid, text, uuid);
DROP TABLE change_log;
DROP TABLE outlet_settings;
DROP TABLE outlet;
DROP TABLE tenant;
DROP TABLE plan;
DROP TYPE cash_rounding_mode;
DROP TYPE subscription_status;
