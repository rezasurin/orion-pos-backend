-- Daily usage per business for the operator console (BACKEND_PLAN.md task B2.9, ADR 0008): counts
-- only, never amounts, items or people. Written by a nightly job in `orion worker` as orion_platform;
-- the app role has no privilege on it, so a business never sees another's numbers, or its own.

-- +goose Up
CREATE TABLE tenant_daily_metrics (
    tenant_id       uuid NOT NULL REFERENCES tenant (id),
    day             date NOT NULL,
    sales           integer NOT NULL, -- sales rung up with this business date, voided ones included
    voided_sales    integer NOT NULL, -- of those, voided since
    events          integer NOT NULL, -- events received this day (Asia/Jakarta)
    rejected_events integer NOT NULL, -- of those, rejected
    devices_synced  integer NOT NULL, -- distinct devices that pushed this day
    flags           integer NOT NULL, -- review flags raised this day
    computed_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, day)
);

-- Every tenant table has the two policies (TestEveryTenantTableHasRowLevelSecurity); with no grant,
-- orion_app's one is never used.
ALTER TABLE tenant_daily_metrics ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_daily_metrics TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON tenant_daily_metrics TO orion_platform USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON tenant_daily_metrics TO orion_platform;

-- +goose Down
DROP TABLE tenant_daily_metrics;
