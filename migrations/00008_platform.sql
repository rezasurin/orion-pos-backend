-- Operators (Orion's own staff), their recovery codes and the platform audit log
-- (BACKEND_PLAN.md sections 4.3, 4.7 and 6.2; ADR 0008).
--
-- These tables belong to the platform module and are used only through orion_platform. orion_app
-- has no privileges on them at all, which a schema test checks, so a flaw in a tenant route cannot
-- reach operator credentials.

-- +goose Up
CREATE TABLE operator (
    id                uuid PRIMARY KEY,
    email             text NOT NULL UNIQUE
        CHECK (email = lower(btrim(email)) AND length(email) <= 254 AND email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),
    password_hash     text NOT NULL,
    -- AES-GCM ciphertext (internal/kernel secrets): the secret must be read back to check codes.
    totp_secret_enc   bytea NOT NULL,
    -- Set by the first successful code, which proves the authenticator app has the secret.
    totp_confirmed_at timestamptz,
    -- The time step of the last accepted code; a code is accepted once, so a stolen one is useless.
    totp_last_step    bigint NOT NULL DEFAULT 0,
    disabled_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE operator_recovery_code (
    id          uuid PRIMARY KEY,
    operator_id uuid NOT NULL REFERENCES operator (id),
    -- SHA-256 of an 80-bit random code. The unique constraint is also the lookup index.
    code_hash   bytea NOT NULL UNIQUE CHECK (octet_length(code_hash) = 32),
    used_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Every operator action (ADR 0008). operator_id is null only for the bootstrap of the first
-- operator, which has nobody to attribute it to; the reason says so.
CREATE TABLE platform_audit_log (
    id          uuid PRIMARY KEY,
    operator_id uuid REFERENCES operator (id),
    action      text NOT NULL CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'),
    target_type text NOT NULL,
    target_id   uuid,
    tenant_id   uuid REFERENCES tenant (id),
    before      jsonb,
    after       jsonb,
    reason      text NOT NULL CHECK (length(btrim(reason)) > 0),
    ip          inet,
    user_agent  text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER operator_set_updated_at BEFORE UPDATE ON operator
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER platform_audit_log_append_only BEFORE UPDATE OR DELETE ON platform_audit_log
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON operator, operator_recovery_code TO orion_platform;
GRANT SELECT, INSERT ON platform_audit_log TO orion_platform;

-- +goose Down
DROP TABLE platform_audit_log;
DROP TABLE operator_recovery_code;
DROP TABLE operator;
