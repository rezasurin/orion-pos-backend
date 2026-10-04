-- Paired POS devices and their per-outlet device codes (BACKEND_PLAN.md sections 4.2, 4.3 and 6.1;
-- ADR 0004).

-- +goose Up
-- device_code is the middle part of receipt numbers: {outlet_code}-{device_code}-{counter}. It is
-- unique per outlet and never reused, so a wiped and re-paired tablet gets a new one and receipt
-- numbers cannot collide. Handed out from a counter, not max()+1, so revoking the newest device
-- does not free its code.
CREATE TABLE device_code_counter (
    outlet_id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    next_code integer NOT NULL DEFAULT 1 CHECK (next_code >= 1),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id)
);

CREATE TABLE device (
    id             uuid PRIMARY KEY,
    tenant_id      uuid NOT NULL REFERENCES tenant (id),
    outlet_id      uuid NOT NULL,
    device_code    integer NOT NULL CHECK (device_code >= 1),
    name           text NOT NULL CHECK (length(btrim(name)) > 0 AND length(name) <= 60),
    -- SHA-256 of the device secret, which is shown once at pairing. The secret has 256 bits of
    -- entropy, so a fast hash is enough.
    secret_hash    bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
    paired_by      uuid NOT NULL REFERENCES user_account (id),
    paired_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at     timestamptz,
    revoked_by     uuid REFERENCES user_account (id),
    -- Support information (BACKEND_PLAN.md section 4.11).
    last_seen_at   timestamptz,
    last_sync_at   timestamptz,
    app_version    text,
    clock_skew_ms  integer,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, outlet_id, device_code),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlet (tenant_id, id),
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);

CREATE TRIGGER device_set_updated_at BEFORE UPDATE ON device
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE device_code_counter ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON device_code_counter TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON device_code_counter TO orion_platform USING (true) WITH CHECK (true);

ALTER TABLE device ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON device TO orion_app
    USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
CREATE POLICY platform_all ON device TO orion_platform USING (true) WITH CHECK (true);

GRANT SELECT, INSERT ON device_code_counter TO orion_app;
GRANT UPDATE (next_code) ON device_code_counter TO orion_app;
GRANT SELECT, INSERT, UPDATE ON device_code_counter TO orion_platform;

-- The service can pair devices, revoke them and record that they were seen; it cannot change
-- which outlet a device belongs to, its code, or its secret.
GRANT SELECT, INSERT ON device TO orion_app;
GRANT UPDATE (revoked_at, revoked_by, last_seen_at, last_sync_at, app_version, clock_skew_ms) ON device TO orion_app;
GRANT SELECT, INSERT, UPDATE ON device TO orion_platform;

-- +goose Down
DROP TABLE device;
DROP TABLE device_code_counter;
