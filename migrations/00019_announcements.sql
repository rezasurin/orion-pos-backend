-- Operator announcements for the back office (BACKEND_PLAN.md 6.2.1, task B2.8): a maintenance
-- window or a notice, to every business or to one. Text per locale; Indonesian is required.

-- +goose Up
CREATE TABLE announcement (
    id          uuid PRIMARY KEY,
    tenant_id   uuid REFERENCES tenant (id), -- null: every business
    severity    text NOT NULL CHECK (severity IN ('info', 'warning')),
    title       jsonb NOT NULL CHECK (jsonb_typeof(title) = 'object' AND coalesce(length(title->>'id'), 0) BETWEEN 1 AND 120),
    body        jsonb NOT NULL CHECK (jsonb_typeof(body) = 'object' AND coalesce(length(body->>'id'), 0) BETWEEN 1 AND 2000),
    starts_at   timestamptz NOT NULL,
    ends_at     timestamptz,
    created_by  uuid NOT NULL REFERENCES operator (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at IS NULL OR ends_at > starts_at)
);

-- What a business sees now: its own and the global ones that have started. A few rows at a time.
CREATE INDEX announcement_starts_idx ON announcement (starts_at);

-- A business reads the announcements to everyone and to itself, and nothing without a tenant in
-- context (TestEveryTenantTableIsolatesRows reads with none).
ALTER TABLE announcement ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON announcement TO orion_app
    USING ((tenant_id IS NULL AND current_tenant_id() IS NOT NULL) OR tenant_id = current_tenant_id());
CREATE POLICY platform_all ON announcement TO orion_platform USING (true) WITH CHECK (true);

GRANT SELECT ON announcement TO orion_app;
GRANT SELECT, INSERT, UPDATE ON announcement TO orion_platform;

-- +goose Down
DROP TABLE announcement;
