-- Terms of service acceptance (BACKEND_PLAN.md task B2.10). Signup records the version the new
-- owner accepted; a later version is accepted again with a new row, so the history stays.

-- +goose Up
CREATE TABLE terms_acceptance (
    tenant_id   uuid NOT NULL,
    user_id     uuid NOT NULL,
    version     text NOT NULL CHECK (length(version) BETWEEN 1 AND 40),
    accepted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id, version),
    -- The business the user was acting in when they accepted.
    FOREIGN KEY (tenant_id, user_id) REFERENCES tenant_member (tenant_id, user_id)
);

ALTER TABLE terms_acceptance ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON terms_acceptance TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON terms_acceptance TO orion_platform USING (true) WITH CHECK (true);

-- Append-only: acceptance is evidence.
GRANT SELECT, INSERT ON terms_acceptance TO orion_app;
GRANT SELECT ON terms_acceptance TO orion_platform;

-- +goose Down
DROP TABLE terms_acceptance;
